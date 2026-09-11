// Package roostprovider implements the `shed roost-provider` menu/state
// machine (plan 019 §3.1-§3.2): the logic behind a roost provider script that
// lets roost's own palette start an agent on a shed or a `machines:` entry.
package roostprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// maxShedsResponseBody bounds how many bytes fetchSheds will read from one
// GET /api/sheds response. DefaultPerServerTimeout bounds TIME, not bytes —
// a server that streams a large body FAST could still exhaust memory well
// inside that deadline. A real `shed list` payload is small (each shed row
// is a couple hundred bytes of JSON at most, and a fleet running hundreds of
// sheds is still well under a megabyte); 8 MiB is generous headroom for even
// an unusually large fleet while still catching a runaway or malicious body.
//
// A body that hits this limit MUST fail the request rather than silently
// decode a truncated prefix — see fetchSheds, which reads up to
// maxShedsResponseBody+1 bytes and treats "got more than the limit" as an
// explicit error, rather than handing json.Decoder an io.LimitReader and
// hoping the truncation happens to land on invalid JSON.
const maxShedsResponseBody = 8 << 20 // 8 MiB

// DefaultPerServerTimeout bounds ONE server's whole list round trip —
// connect, TLS handshake, response headers, and body decode, not just the
// dial — inside Inventory's fan-out. roost budgets a provider's `list` phase at
// 5s by default (plan 019 §"Provider contract"); this keeps a single
// unreachable or slow server from eating that budget on its own, however
// many servers are configured.
const DefaultPerServerTimeout = 2 * time.Second

// RunningShed is one row for the provider's list menu (§3.2 step 1): a shed
// that answered "running" from one of the configured servers.
type RunningShed struct {
	// Name is the shed's own name.
	Name string
	// Server is the `servers:` entry name that hosts it (the config.yaml map
	// key, not Host — the two commonly differ, e.g. an entry named "mini3"
	// with host "mini3.local").
	Server string
	// ServerHost / ServerSSHPort are copied out of the config.ServerEntry
	// this shed was found on, rather than holding a pointer to the entry
	// itself — Inventory fans out over a map the caller owns, and copying is
	// what lets that map be read (or reloaded) concurrently with an in-flight
	// Inventory call without a data race.
	//
	// A shed's ssh identity is always `<shed>@<ServerHost> -p
	// <ServerSSHPort>` against `~/.shed/known_hosts` (never a per-server
	// host-key pin — see shed-core/src/terminal.rs's identical rule), so
	// that's the whole reach this struct needs to carry.
	ServerHost    string
	ServerSSHPort int
	// LandingDir is config.Shed's landing_dir field verbatim. Empty means the
	// server didn't report one — an older shed-server predating the field, or
	// a shed with no project mount — and the caller falls back to "~" per
	// §3.2 step 3.
	LandingDir string
}

// errNoStoredCredential marks a server entry this package must not attempt
// to reach: presenting nothing would either dial an endpoint that demands a
// credential (a wasted, doomed round trip) or, worse, invite a caller to add
// a fallback path that mints one — which is exactly the SSH-borne enrollment
// this package exists to never trigger. See stashedCredential.
//
// Every caller only ever checks this for non-nilness (listServer treats it
// like any other per-server failure), so a plain sentinel is enough — no
// custom type needed.
var errNoStoredCredential = errors.New("no usable stored credential")

// ShedInventory is one Inventory call's whole answer: the running sheds, and
// which servers were reachable at all.
//
// The menu needs that second part and it is the reason this is a struct rather
// than a bare slice: plan 019 §3.2 pins two DIFFERENT empty-menu rows, and
// telling them apart is exactly the question "did any server answer at all" —
// a fleet that answered and is simply idle gets "No running sheds or
// machines", while a fleet nothing could be reached on gets "no shed-server
// answered" and the list of names it tried. Collapsing those two would tell a
// user with a dead VPN that they have no sheds.
type ShedInventory struct {
	// Sheds is every RUNNING shed found, sorted by (server, shed name).
	Sheds []RunningShed
	// Tried names every server in the input map, sorted — including the ones
	// skipped for holding no usable stored credential, which were still
	// "tried" from the user's point of view.
	Tried []string
	// Answered names the servers that returned a decodable /api/sheds
	// response, sorted. A server here with no rows in Sheds is running no
	// sheds; a server in Tried but not here could not be reached.
	//
	// A server whose worker had not finished when ctx expired appears in
	// neither — unreachable is the honest reading of "we never heard back".
	Answered []string
}

// Inventory enumerates every RUNNING shed across servers, using ONLY each
// server's already-stored credential (never minting or persisting one —
// see stashedCredential) and never writing to config.yaml, and records which
// servers answered at all.
//
// Every server is queried concurrently, each bounded independently by
// timeout (DefaultPerServerTimeout when timeout <= 0); ctx bounds the whole
// call. A server that errors, times out, or holds no usable stored
// credential is skipped SILENTLY — this runs inside roost's `list` phase,
// which has no stderr a human is watching, and a provider that spammed one
// line per unreachable server on every keystroke of roost's palette would be
// worse than saying nothing. (Skipped is not the same as invisible: the
// server still appears in Tried and not in Answered, which is what the menu
// reads.)
//
// Sheds is sorted by (server, shed name) for a stable menu across calls with
// the same input.
//
// Inventory returns the moment ctx is done, with whatever results have arrived
// by then — it does NOT wait for every worker to finish. That matters because a
// worker can block before its own per-server timeout even exists:
// stashedCredential's config.LoadClientCredentials reads two files
// synchronously, with no context at all, so a ClientCertFile/ClientKeyFile
// pointing at a FIFO with no writer, or a stalled network mount, blocks that
// worker forever — no per-server timeout ever gets a chance to fire. Fanning
// out over a wg.Wait() (the earlier shape) would let one such worker hang
// this whole call past roost's 5s `list`-phase budget with no way out.
//
// Each worker's result channel is buffered to exactly len(servers), so a
// worker that finishes (or unblocks) after Inventory has already returned on
// ctx.Done() can still send without blocking — it simply leaks until it
// completes, at which point the goroutine exits and the channel becomes
// unreferenced. That buffering is also what removes the need for the mutex
// the old shape used to guard the shared output slice: out is local to this
// call and is only ever touched by the goroutine that owns it, never by a
// worker after Inventory has returned.
func Inventory(ctx context.Context, servers map[string]config.ServerEntry, timeout time.Duration) ShedInventory {
	if timeout <= 0 {
		timeout = DefaultPerServerTimeout
	}

	type serverResult struct {
		name     string
		rows     []RunningShed
		answered bool
	}
	results := make(chan serverResult, len(servers))
	for name, entry := range servers {
		go func(name string, entry config.ServerEntry) {
			rows, answered := listServer(ctx, name, entry, timeout)
			results <- serverResult{name: name, rows: rows, answered: answered}
		}(name, entry)
	}

	out := ShedInventory{Tried: slices.Sorted(maps.Keys(servers))}
collect:
	for range servers {
		select {
		case res := <-results:
			out.Sheds = append(out.Sheds, res.rows...)
			if res.answered {
				out.Answered = append(out.Answered, res.name)
			}
		case <-ctx.Done():
			break collect
		}
	}

	sort.Slice(out.Sheds, func(i, j int) bool {
		if out.Sheds[i].Server != out.Sheds[j].Server {
			return out.Sheds[i].Server < out.Sheds[j].Server
		}
		return out.Sheds[i].Name < out.Sheds[j].Name
	})
	sort.Strings(out.Answered)
	return out
}

// listServer queries one server for its running sheds, and reports whether the
// server answered at all. Any failure — no usable credential, connection
// refused, timeout, non-200, a body that doesn't decode — returns
// (nil, false): in the shed list an erroring server and a server with nothing
// running look identical, and the second return value is the only thing that
// tells them apart.
func listServer(ctx context.Context, name string, entry config.ServerEntry, timeout time.Duration) ([]RunningShed, bool) {
	resp, err := fetchSheds(ctx, entry, timeout)
	if err != nil {
		return nil, false
	}

	var rows []RunningShed
	for _, shed := range resp.Sheds {
		if shed.Status != config.StatusRunning {
			continue
		}
		rows = append(rows, RunningShed{
			Name:          shed.Name,
			Server:        name,
			ServerHost:    entry.Host,
			ServerSSHPort: entry.SSHPort,
			LandingDir:    shed.LandingDir,
		})
	}
	return rows, true
}

// fetchSheds performs the one bounded GET /api/sheds call for entry.
func fetchSheds(ctx context.Context, entry config.ServerEntry, timeout time.Duration) (*config.ShedsResponse, error) {
	cred, ok := stashedCredential(&entry)
	if !ok {
		return nil, errNoStoredCredential
	}

	// Refuse to send a credential over plaintext. entry.BaseURL() falls
	// back to plain http:// whenever api_url is empty, but
	// servertls.PinnedTransport's certificate pinning (and everything that
	// depends on it — the Authorization header below, the client cert for
	// mtls) is only ever consulted for an https:// URL; an http:// dial
	// never even looks at it. An entry that has a stored fingerprint (and
	// therefore a stored, real credential) but a missing or hand-edited
	// plaintext api_url would otherwise ship that credential in the clear.
	//
	// This is THIS package's own guard, not a repo-wide fix:
	// cmd/shed/client.go's newAPIClientWithSource has the identical gap and
	// is out of scope here.
	baseURL := entry.BaseURL()
	if entry.TLSCertFingerprint != "" && !strings.HasPrefix(strings.ToLower(baseURL), "https://") {
		return nil, fmt.Errorf("entry has a stored TLS fingerprint but base URL %q is not https; refusing to send its credential over plaintext", baseURL)
	}

	// A fresh, refresh-less Source: CertificateFor reads cred back for the
	// TLS handshake, but nothing here ever calls EnsureFresh, Refresh, or
	// mints over SSH. See stashedCredential's doc for why "no usable
	// credential" is a skip rather than a trigger to go get one.
	src := clienttoken.New(cred, nil)
	transport := servertls.PinnedTransport(entry.TLSCertFingerprint, src.CertificateFor)
	client := &http.Client{Transport: transport}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+"/api/sheds", nil)
	if err != nil {
		return nil, err
	}
	// Pin the credential onto the request the same way APIClient.setAuth
	// does: the Authorization header for token mode, the context value
	// CertificateFor reads for mtls mode. Both channels read the SAME
	// captured cred, so a request never presents a header and a certificate
	// from two different generations (moot here since this Source never
	// advances a generation, but the shape is copied deliberately rather
	// than re-derived).
	req = req.WithContext(clienttoken.WithPinned(req.Context(), cred))
	if tok := cred.BearerToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d (%s)", res.StatusCode, http.StatusText(res.StatusCode))
	}

	// reqCtx bounds this read too (res.Body is tied to the request's
	// context) — the per-server timeout covers the body, not just the
	// connect+handshake, per plan 019 §3.2. That bounds TIME; it says
	// nothing about SIZE, which is what maxShedsResponseBody is for (see
	// its doc comment). Read up to one byte past the limit and treat
	// exceeding it as an explicit error rather than handing json.Decoder an
	// io.LimitReader directly — a naive LimitReader can, depending on where
	// the cut lands, still decode a truncated-but-coincidentally-valid
	// prefix instead of failing, which would silently hand the caller a
	// partial shed list.
	body, err := io.ReadAll(io.LimitReader(res.Body, maxShedsResponseBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxShedsResponseBody {
		return nil, fmt.Errorf("sheds response exceeds %d byte limit", maxShedsResponseBody)
	}

	var out config.ShedsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// stashedCredential mirrors cmd/shed/client.go's entryCredential — read the
// entry's ALREADY-STORED credential (a bearer token or, for an mtls entry,
// the client certificate on disk) into a clienttoken.Credential — but never
// mints one and never touches disk beyond the read.
//
// It returns ok=false in exactly the case where entryCredential would need
// an SSH-borne mint to get anything usable: an mtls entry with no loadable
// certificate, or a secure (HTTPS) entry with no token recorded at all
// (config.ServerEntry.NeedsEnrollment). Those are the shapes a real client
// would enroll for — this package must never do that (a roost `list` call
// has a 5s budget and no interactive prompt to ask permission with), so it
// treats them as "unreachable" instead, exactly like a server that didn't
// answer.
//
// A legacy static token, a not-yet-expired bootstrap-minted token, and an
// OPEN server (which needs no credential at all — the zero Credential is
// perfectly usable there) all return ok=true. An expired stored token is
// NOT specially detected here; the resulting request simply 401s and
// listServer treats that like any other per-server failure — this package
// has no reactive re-mint to fall back on by design.
func stashedCredential(entry *config.ServerEntry) (clienttoken.Credential, bool) {
	if entry.IsMTLS() {
		if entry.ClientCertFile == "" || entry.ClientKeyFile == "" {
			return clienttoken.Credential{}, false
		}
		// name="" — this call never persists, so there's nothing to
		// serialize a write against; loadClientCert's own reasoning
		// (cmd/shed/client.go) for locking under a name doesn't apply here.
		cert, err := config.LoadClientCredentials("", entry.ClientCertFile, entry.ClientKeyFile)
		if err != nil || cert == nil {
			return clienttoken.Credential{}, false
		}
		return clienttoken.MTLSCredential(cert, entry.ClientCertExpiresAt), true
	}
	if entry.ControlTokenExpiresAt.IsZero() {
		// Either a legacy static token (has something to present, never
		// re-minted) or an open server (needs nothing — entry.ControlToken
		// is "" and TokenCredential("", zero).BearerToken() is correctly
		// ""). NeedsEnrollment is what tells those apart from a secure entry
		// stripped of its credential, which DOES need a mint and is
		// therefore refused here.
		if entry.NeedsEnrollment() {
			return clienttoken.Credential{}, false
		}
		return clienttoken.TokenCredential(entry.ControlToken, time.Time{}), true
	}
	return clienttoken.TokenCredential(entry.ControlToken, entry.ControlTokenExpiresAt), true
}
