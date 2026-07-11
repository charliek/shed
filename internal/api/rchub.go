package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
	"github.com/charliek/shed/internal/vmutil"
	"github.com/go-chi/chi/v5"
)

// This file implements the server-side rc hub reverse proxy
// (/api/sheds/{name}/rc/*) plus the ensure-start machinery (singleflight +
// circuit breaker) it and the aggregator share. The proxy forwards a strict
// allowlist of hub endpoints into a shed's guest-local rc hub over
// backend.DialService(shed, rc.HubPort); everything outside the allowlist is
// rejected BEFORE any dial. The hub is loopback-only inside the guest, so on
// Firecracker (where DialService reaches the VM's bridge IP, not loopback) the
// hub is unreachable and the proxy degrades to 503 RC_HUB_UNAVAILABLE — the
// documented FC-degrade.

const (
	// rcHubUnavailableCode is the error code returned when a shed's rc hub can't
	// be reached (dial refused after an ensure-start attempt, a foreign listener
	// on the port, the circuit breaker is open, or the FC loopback-unreachable
	// degrade). Clients key feature-degrade off this code.
	rcHubUnavailableCode = "RC_HUB_UNAVAILABLE"

	// rcHubProbeTimeout bounds a single /v1/health identity probe through the hub
	// transport. Loopback-local, so a live hub answers in well under this; a
	// refused dial fails fast (this is only the ceiling for a hung dial).
	rcHubProbeTimeout = 750 * time.Millisecond

	// rcHubStartTimeout bounds the guest `shed-ext-rc serve --detach` exec. The
	// detach parent waits for its own port probe (≈3s) then exits, so the exec
	// returns promptly; this is a generous ceiling for a slow guest.
	rcHubStartTimeout = 8 * time.Second

	// rcHubInputBodyLimit caps a POST /v1/sessions/{slug}/input body at the server
	// (the hub enforces the same 16 KiB independently). Bounds an oversized upload
	// before it is streamed into the guest.
	rcHubInputBodyLimit = 16 << 10

	// rcHubRespBodyLimit caps a non-streaming proxied response body (sessions /
	// messages). The streaming events path is exempt (it is line-bounded instead).
	rcHubRespBodyLimit = 2 << 20

	// rcHubEventsLineLimit caps a single SSE line on the events path so a broken or
	// hostile hub can't stream an unbounded line. The hub emits small frames; this
	// is a safety ceiling.
	rcHubEventsLineLimit = 256 << 10

	// Circuit-breaker tuning: 3 failed starts within the window trips the breaker,
	// which then returns 503 immediately (no exec) for the remainder of the window.
	rcHubBreakerThreshold = 3
	rcHubBreakerWindow    = 5 * time.Minute
)

// rcSlugRe validates a proxied {slug} path segment: alphanumeric plus - and _
// only. It disallows dots and slashes, so a ".." or a slash-bearing segment can
// never reach the proxied path (traversal defense) — such a segment fails the
// pattern and the request 404s before any dial.
var rcSlugRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// errRCHubUnavailable is the internal sentinel for "the hub could not be reached"
// (dial refused post-start, foreign listener, or breaker open). Mapped to 503
// rcHubUnavailableCode by the proxy handler.
var errRCHubUnavailable = errors.New("rc hub unavailable")

// hubDialContext returns a DialContext that ignores the requested address and
// dials shedName's rc hub port through the backend. The transport built on it
// therefore always targets rc.HubAddr inside the shed regardless of the URL host.
func (s *Server) hubDialContext(shedName string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return s.backend.DialService(ctx, shedName, rc.HubPort)
	}
}

// newHubTransport builds an HTTP transport whose every dial reaches shedName's
// guest-local rc hub over backend.DialService. Keep-alives are disabled: each
// proxied request / probe is short-lived and a pooled connection to a
// per-request DialService stream has no reuse value.
func (s *Server) newHubTransport(shedName string) *http.Transport {
	return &http.Transport{
		DialContext:           s.hubDialContext(shedName),
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// hubGet issues a GET to path on shedName's guest-local rc hub through the hub
// transport and returns the raw response. The caller owns ctx (and thus the
// timeout) and must close the body. It is the shared request-issuing core for the
// non-streaming hub reads (the /v1/health probe and the /v1/sessions consult); the
// SSE upstream (openHubEvents) builds its own request because it also sets an
// Accept header and runs untimed. A dial/build failure surfaces as a returned
// error, which each caller classifies.
func (s *Server) hubGet(ctx context.Context, shedName, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+rc.HubAddr+path, nil)
	if err != nil {
		return nil, err
	}
	return (&http.Client{Transport: s.newHubTransport(shedName)}).Do(req)
}

// hubReachClass classifies the outcome of a hub reachability probe.
type hubReachClass int

const (
	hubReachOK         hubReachClass = iota // a verified hub answered /v1/health
	hubReachNotRunning                      // the shed itself is stopped
	hubReachNotFound                        // the shed does not exist
	hubReachAbsent                          // dial reached the VM but nothing (or nothing yet) listens on the hub port
	hubReachForeign                         // something answered but it is not a shed rc hub (identity mismatch)
)

// probeHubHealth performs one GET /v1/health through the hub transport and
// classifies the result. It never starts a hub — it only observes.
func (s *Server) probeHubHealth(ctx context.Context, shedName string) hubReachClass {
	ctx, cancel := context.WithTimeout(ctx, rcHubProbeTimeout)
	defer cancel()

	resp, err := s.hubGet(ctx, shedName, "/v1/health")
	if err != nil {
		switch {
		case errors.Is(err, config.ErrShedNotRunningSentinel):
			return hubReachNotRunning
		case errors.Is(err, config.ErrShedNotFoundSentinel):
			return hubReachNotFound
		default:
			// Dial reached (or tried to reach) the VM but the hub port isn't
			// answering — hub absent (or, on FC, loopback-unreachable).
			return hubReachAbsent
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return hubReachForeign
	}
	var hh struct {
		App string `json:"app"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&hh) != nil || hh.App != rc.HubAppID {
		return hubReachForeign
	}
	return hubReachOK
}

// ensureHubReachable guarantees a verified rc hub is reachable for shedName,
// starting one at most once (guest `shed-ext-rc serve --detach`, deduplicated
// per shed by singleflight and gated by the circuit breaker) and re-probing.
// Returns nil when a hub answers the identity handshake; otherwise a classified
// error the caller maps to a status: config.ErrShedNotRunningSentinel /
// ErrShedNotFoundSentinel pass through; a foreign listener, an open breaker, or a
// still-absent hub after a start attempt map to errRCHubUnavailable.
func (s *Server) ensureHubReachable(ctx context.Context, shedName string) error {
	switch s.probeHubHealth(ctx, shedName) {
	case hubReachOK:
		s.rcHubBreaker.success(shedName)
		return nil
	case hubReachNotRunning:
		return config.ErrShedNotRunningSentinel
	case hubReachNotFound:
		return config.ErrShedNotFoundSentinel
	case hubReachForeign:
		// Something is squatting the port that is not a hub; never try to start
		// over it, and never retry-storm — surface unavailable immediately.
		return errRCHubUnavailable
	case hubReachAbsent:
		// fall through to the start path
	}

	// Breaker: after rcHubBreakerThreshold failed starts in the window, refuse
	// immediately (no exec storm) until the window elapses.
	if !s.rcHubBreaker.allow(shedName) {
		return errRCHubUnavailable
	}

	// One start attempt per shed, shared across concurrent callers (singleflight),
	// with the start+reprobe+breaker accounting done exactly once inside the
	// flight so N racing callers make ONE exec and record ONE breaker outcome.
	v, _, _ := s.rcHubFlight.Do(shedName, func() (any, error) {
		_ = s.startHub(ctx, shedName) // a start error still falls through to the reprobe (the hub may have raced up)
		ok := s.probeHubHealth(ctx, shedName) == hubReachOK
		if ok {
			s.rcHubBreaker.success(shedName)
		} else {
			s.rcHubBreaker.failure(shedName)
		}
		return ok, nil
	})
	if ok, _ := v.(bool); ok {
		return nil
	}
	return errRCHubUnavailable
}

// startHub execs `shed-ext-rc serve --detach` in the shed. The guest detach path
// double-forks the daemon (setsid) so it survives the exec channel's SIGHUP, and
// the parent waits for its own port probe before exiting — so this blocking exec
// returns promptly with the hub up (or an error if it could not confirm).
func (s *Server) startHub(ctx context.Context, shedName string) error {
	ctx, cancel := context.WithTimeout(ctx, rcHubStartTimeout)
	defer cancel()
	return s.backend.Exec(ctx, shedName, backend.ExecOptions{
		Cmd:    []string{"shed-ext-rc", "serve", "--detach"},
		Stdout: vmutil.NopWriteCloser(io.Discard),
		Stderr: vmutil.NopWriteCloser(io.Discard),
	})
}

// classifyRCProxyPath validates the proxied sub-path (everything after
// /api/sheds/{name}/rc/) against the strict allowlist and returns the HTTP status
// to reject with when it isn't allowed:
//
//	GET  v1/sessions
//	GET  v1/events
//	GET  v1/sessions/{slug}/messages
//	POST v1/sessions/{slug}/input
//
// A known path with the wrong method yields 405; anything else (including an
// unsafe {slug}) yields 404 — so a traversal attempt never reaches a dial.
func classifyRCProxyPath(method, rest string) (allowed bool, rejectStatus int) {
	segs := strings.Split(rest, "/")
	switch {
	case rest == "v1/sessions":
		return methodGate(method, http.MethodGet)
	case rest == "v1/events":
		return methodGate(method, http.MethodGet)
	case len(segs) == 4 && segs[0] == "v1" && segs[1] == "sessions" && segs[3] == "messages" && rcSlugRe.MatchString(segs[2]):
		return methodGate(method, http.MethodGet)
	case len(segs) == 4 && segs[0] == "v1" && segs[1] == "sessions" && segs[3] == "input" && rcSlugRe.MatchString(segs[2]):
		return methodGate(method, http.MethodPost)
	default:
		return false, http.StatusNotFound
	}
}

func methodGate(got, want string) (bool, int) {
	if got == want {
		return true, 0
	}
	return false, http.StatusMethodNotAllowed
}

// handleRCProxy reverse-proxies an allowlisted request into a shed's rc hub.
//
//	GET  /api/sheds/{name}/rc/v1/sessions
//	GET  /api/sheds/{name}/rc/v1/events                    (SSE)
//	GET  /api/sheds/{name}/rc/v1/sessions/{slug}/messages
//	POST /api/sheds/{name}/rc/v1/sessions/{slug}/input
//
// Control scope, enforced by the auth middleware's default branch (this route is
// neither the credential bus, the Connect tunnel, nor the egress stream). The
// allowlist is checked BEFORE any dial; the hub is ensured reachable (started at
// most once) before the request is forwarded.
func (s *Server) handleRCProxy(w http.ResponseWriter, r *http.Request) {
	shedName := chi.URLParam(r, "name")
	rest := strings.TrimPrefix(chi.URLParam(r, "*"), "/")

	allowed, rejectStatus := classifyRCProxyPath(r.Method, rest)
	if !allowed {
		if rejectStatus == http.StatusMethodNotAllowed {
			writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed on this rc endpoint")
			return
		}
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown rc endpoint")
		return
	}

	isEvents := rest == "v1/events"

	// Bound a POST input body at the server before it is streamed to the guest.
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, rcHubInputBodyLimit)
	}

	// Ensure a verified hub is reachable (start it at most once). Classify the
	// failure into the caller-facing status.
	if err := s.ensureHubReachable(r.Context(), shedName); err != nil {
		switch {
		case errors.Is(err, config.ErrShedNotFoundSentinel):
			writeError(w, http.StatusNotFound, config.ErrShedNotFound, "shed not found")
		case errors.Is(err, config.ErrShedNotRunningSentinel):
			writeError(w, http.StatusServiceUnavailable, "SHED_NOT_RUNNING", "shed is not running")
		default:
			writeError(w, http.StatusServiceUnavailable, rcHubUnavailableCode, "rc hub is not available for this shed")
		}
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = rc.HubAddr
			pr.Out.Host = rc.HubAddr
			pr.Out.URL.Path = "/" + rest
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			// SECURITY: forward only an explicit allowlist of inbound headers.
			// pr.Out starts as a clone of pr.In, so without this the client's
			// Authorization (the control-scope bearer token in secure mode) and
			// Cookie headers would be handed verbatim to the guest-local hub —
			// untrusted-adjacent code that could replay the token against the
			// server API. Everything not on the allowlist is dropped.
			pr.Out.Header = allowlistedHubHeaders(pr.In.Header)
		},
		Transport: s.newHubTransport(shedName),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			// A dial/read failure after ensure-start (e.g. the hub raced an
			// idle-exit between the probe and the forward) is a 502.
			writeError(w, http.StatusBadGateway, "RC_PROXY_FAILED", "rc hub request failed")
		},
		ModifyResponse: func(resp *http.Response) error {
			if isEvents {
				resp.Body = newLineCapReadCloser(resp.Body, rcHubEventsLineLimit)
			} else {
				resp.Body = boundedReadCloser(resp.Body, rcHubRespBodyLimit)
			}
			return nil
		},
	}
	if isEvents {
		// Flush each SSE frame immediately rather than buffering the stream.
		proxy.FlushInterval = -1
	}

	// The ContentTypeJSON middleware pre-set Content-Type: application/json on the
	// writer; drop it so the hub's own header (text/event-stream for the events
	// path, application/json otherwise) is authoritative — ReverseProxy APPENDS the
	// upstream header rather than replacing it, so a lingering pre-set value would
	// win and mislabel an SSE stream as JSON.
	w.Header().Del("Content-Type")

	proxy.ServeHTTP(w, r)
}

// hubForwardedHeaders is the explicit allowlist of inbound request headers the
// rc proxy forwards into the guest hub. Notably ABSENT: Authorization and Cookie
// — the guest must never see the client's server-API credentials (see the
// Rewrite comment). Content-Length is not listed because Go's transport derives
// it from the outbound request's ContentLength field, not the header map.
var hubForwardedHeaders = []string{"Accept", "Content-Type", "Cache-Control"}

// allowlistedHubHeaders returns a fresh header map holding only the
// hubForwardedHeaders entries of in.
func allowlistedHubHeaders(in http.Header) http.Header {
	out := http.Header{}
	for _, k := range hubForwardedHeaders {
		if vs := in.Values(k); len(vs) > 0 {
			out[http.CanonicalHeaderKey(k)] = vs
		}
	}
	return out
}

// boundedReadCloser wraps rc so at most limit bytes are read from the underlying
// stream (a defensive cap on non-streaming proxied bodies), preserving Close.
func boundedReadCloser(rc io.ReadCloser, limit int64) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{Reader: io.LimitReader(rc, limit), Closer: rc}
}
