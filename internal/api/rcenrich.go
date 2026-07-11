package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
	"github.com/charliek/shed/internal/vmutil"
	"golang.org/x/sync/errgroup"
)

const (
	// rcEnrichConcurrency caps concurrent `shed-ext-rc list` execs during a single
	// session-listing request so a fleet host with many rc-bearing sheds never fans
	// out unbounded. Mirrors the CLI's retired rcEnrichConcurrency, now server-side.
	rcEnrichConcurrency = 4
	// rcEnrichTimeout bounds one `shed-ext-rc list` exec so a slow or hung shed can't
	// stall the whole listing (the shed simply degrades to un-enriched rows).
	rcEnrichTimeout = 2 * time.Second
	// rcExecOutputLimit caps captured guest stdout. Guest output is untrusted input;
	// a session envelope for tens of sessions is a few KiB, so 1 MiB is generous.
	rcExecOutputLimit = 1 << 20
	// rcCapsTTL is how long a cached per-shed capabilities block is served before a
	// re-probe. It covers in-place agent installs in a running shed (the capabilities
	// change without a lifecycle event); shorter than a human notices, long enough to
	// keep a hot poll path from re-probing every call.
	rcCapsTTL = 5 * time.Minute
)

// rcEnrichEnabled reports whether a request wants rc enrichment. Enrichment is
// on by default; `?rc=0` opts out entirely (zero execs) for hot poll paths that
// only need the plain tmux listing.
func rcEnrichEnabled(r *http.Request) bool {
	return r.URL.Query().Get("rc") != "0"
}

// rcCapsCache caches per-shed rc capabilities keyed by shed name. Capabilities
// change rarely (only an in-place agent install or a shed recreate), so they are
// served from cache within rcCapsTTL and invalidated on lifecycle transitions.
//
// A per-shed generation counter closes the invalidate-vs-in-flight-put race: a
// probe snapshots the shed's generation before its exec starts and passes it to
// put; invalidate bumps the generation, so a probe that raced a lifecycle
// transition (started before, finished after) has a stale generation and its put
// is dropped instead of resurrecting pre-transition capabilities.
//
// now is injectable so TTL expiry is testable.
type rcCapsCache struct {
	mu      sync.Mutex
	entries map[string]rcCapsEntry
	gens    map[string]uint64 // per-shed generation; bumped by invalidate, never reset
	now     func() time.Time
}

type rcCapsEntry struct {
	caps    *rc.Capabilities
	fetched time.Time
}

func newRCCapsCache() *rcCapsCache {
	return &rcCapsCache{entries: map[string]rcCapsEntry{}, gens: map[string]uint64{}, now: time.Now}
}

// get returns a cached capabilities block for shed, or (nil, false) when absent
// or older than rcCapsTTL.
func (c *rcCapsCache) get(shed string) (*rc.Capabilities, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[shed]
	if !ok || c.now().Sub(e.fetched) > rcCapsTTL {
		return nil, false
	}
	return e.caps, true
}

// generation returns the shed's current invalidation generation. Snapshot it
// BEFORE starting the probe exec whose result will be put.
func (c *rcCapsCache) generation(shed string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gens[shed]
}

// put records caps for shed, stamping the fetch time for TTL. gen must be the
// generation observed (via generation) before the probe exec started; if a
// lifecycle invalidation landed in between, the result is stale and is dropped.
func (c *rcCapsCache) put(shed string, gen uint64, caps *rc.Capabilities) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gens[shed] != gen {
		return // invalidated while the probe was in flight — drop the stale result
	}
	c.entries[shed] = rcCapsEntry{caps: caps, fetched: c.now()}
}

// invalidate drops shed's cached capabilities and bumps its generation so any
// in-flight probe's put is dropped. Called on stop/start/delete/reset so a
// recreated or in-place-changed shed never serves a stale capability set. The
// generation entry is intentionally never deleted: a shed recreated under the
// same name keeps counting up, so an old generation can never match again.
func (c *rcCapsCache) invalidate(shed string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, shed)
	c.gens[shed]++
}

// limitedWriter buffers up to limit bytes and silently discards the rest, always
// reporting a full write so the guest side never sees a short-write error. It
// bounds untrusted guest stdout.
type limitedWriter struct {
	buf   []byte
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if remaining := w.limit - len(w.buf); remaining > 0 {
		if len(p) > remaining {
			w.buf = append(w.buf, p[:remaining]...)
		} else {
			w.buf = append(w.buf, p...)
		}
	}
	return len(p), nil
}

func (w *limitedWriter) Bytes() []byte { return w.buf }

// execRCList runs `shed-ext-rc list` in shedName over the guest agent channel and
// decodes the envelope. Runtime is bounded (rcEnrichTimeout) and captured stdout
// is size-bounded (rcExecOutputLimit); guest stdout is untrusted input. An old
// baked-in binary that prints the bare `{"rc_sessions":[…]}` envelope (no
// capabilities) still decodes — Capabilities is an omitempty pointer.
func (s *Server) execRCList(ctx context.Context, shedName string) (*rc.ListResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, rcEnrichTimeout)
	defer cancel()

	lw := &limitedWriter{limit: rcExecOutputLimit}
	opts := backend.ExecOptions{
		// shed-ext-rc installs at /usr/local/bin, which is on the guest exec PATH.
		Cmd:    []string{"shed-ext-rc", "list"},
		Stdout: vmutil.NopWriteCloser(lw),
		Stderr: vmutil.NopWriteCloser(io.Discard),
	}
	if err := s.backend.Exec(ctx, shedName, opts); err != nil {
		return nil, err
	}
	var resp rc.ListResponse
	if err := json.Unmarshal(lw.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("decoding shed-ext-rc list on %s: %w", shedName, err)
	}
	return &resp, nil
}

// RCCapabilities returns shedName's rc capabilities, served from the per-shed
// cache within rcCapsTTL unless fresh forces a re-probe. A re-probe execs
// `shed-ext-rc list` and caches the returned block. Returns (nil, err) when the
// shed can't be probed (no rc binary, exec failure), or (nil, nil) when a binary
// too old to advertise capabilities responds. Exported so the overview endpoint
// can reach the cached block without duplicating the exec/cache plumbing.
//
// The probe itself is deduplicated cross-request through a per-shed singleflight:
// M concurrent callers that miss the cache (or force ?fresh=1) share ONE guest
// exec per shed rather than fanning out M — refresh-heavy overview polling can't
// stampede a shed. The generation is snapshotted INSIDE the flight, so the shared
// result still respects a lifecycle invalidation landing mid-exec (the stale put
// is dropped; every caller still receives the freshly probed block). The flight
// runs under the leader's ctx; execRCList self-bounds it to rcEnrichTimeout, so a
// joined caller waits at most that long even if the leader's request is slow.
func (s *Server) RCCapabilities(ctx context.Context, shedName string, fresh bool) (*rc.Capabilities, error) {
	if !fresh {
		if caps, ok := s.rcCaps.get(shedName); ok {
			return caps, nil
		}
	}
	v, err, _ := s.rcCapsFlight.Do(shedName, func() (any, error) {
		gen := s.rcCaps.generation(shedName) // snapshot before the probe starts
		resp, err := s.execRCList(ctx, shedName)
		if err != nil {
			return nil, err
		}
		if resp.Capabilities != nil {
			s.rcCaps.put(shedName, gen, resp.Capabilities)
		}
		return resp.Capabilities, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*rc.Capabilities), nil
}

// toSessionRC projects the canonical rc.Session onto the display subset carried on
// config.Session.RC. The decode type stays the canonical internal/ext/rc type; this
// is presentation-only, not a third copy of the wire shape.
func toSessionRC(s rc.Session) *config.SessionRC {
	return &config.SessionRC{
		Kind:        string(s.Kind),
		State:       string(s.State),
		Managed:     s.Managed,
		DisplayName: s.DisplayName,
		URL:         s.URL,
		CreatedBy:   s.CreatedBy,
	}
}

// enrichSessionsRC fills the RC field of every rc-* session in sessions by exec'ing
// `shed-ext-rc list` once per shed that actually has rc-* rows, keyed by
// Session.ShedName. Concurrency is capped (rcEnrichConcurrency), each exec is
// time-bounded, and captured stdout is size-bounded. A shed that fails to enrich
// keeps its plain rows and contributes a warning (degrade, not silent). Zero execs
// when no rc-* rows are present. Each successful exec refreshes the per-shed
// capabilities cache from the same envelope. Mutates sessions in place; returns the
// warnings to append to the response.
func (s *Server) enrichSessionsRC(ctx context.Context, sessions []config.Session) []string {
	idxByShed := map[string][]int{}
	for i := range sessions {
		if strings.HasPrefix(sessions[i].Name, rc.TmuxPrefix) {
			idxByShed[sessions[i].ShedName] = append(idxByShed[sessions[i].ShedName], i)
		}
	}
	if len(idxByShed) == 0 {
		return nil
	}

	var (
		mu       sync.Mutex
		warnings []string
		g        errgroup.Group
	)
	g.SetLimit(rcEnrichConcurrency)
	for shedName, idxs := range idxByShed {
		g.Go(func() error {
			gen := s.rcCaps.generation(shedName) // snapshot before the probe starts
			resp, err := s.execRCList(ctx, shedName)
			if err != nil {
				mu.Lock()
				warnings = append(warnings, "shed "+shedName+": RC metadata unavailable: "+err.Error())
				mu.Unlock()
				return nil
			}
			if resp.Capabilities != nil {
				s.rcCaps.put(shedName, gen, resp.Capabilities)
			}
			byTmux := rc.IndexByTmux(resp.RCSessions)
			// Indices are partitioned per shed, so these writes never overlap
			// across goroutines.
			for _, i := range idxs {
				if rs, ok := byTmux[sessions[i].Name]; ok {
					sessions[i].RC = toSessionRC(rs)
				}
			}
			return nil
		})
	}
	// Per-shed failures are captured as warnings, never returned, so Wait's
	// error is always nil (degrade, not fail-fast).
	_ = g.Wait()
	return warnings
}
