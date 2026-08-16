package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
)

// hubMuxOpts configures the in-test fake rc hub served over a real TCP listener
// (so the proxy transport's DialService reaches it like a real guest hub).
type hubMuxOpts struct {
	app          string // /v1/health "app" identity; "" → the real rc.HubAppID
	sessionsBody string // GET /v1/sessions body; "" → an empty {"sessions":[]}
	// onRequest, when set, observes every request the fake hub receives (all
	// endpoints, health probe included) — used to assert which headers the proxy
	// forwards into the guest.
	onRequest func(*http.Request)
}

// startFakeHub serves a fake rc hub on a real loopback listener and returns its
// address. The health/sessions/messages/input/events endpoints answer with
// minimal bodies sufficient for the proxy + enrichment tests.
func startFakeHub(t *testing.T, opts hubMuxOpts) string {
	t.Helper()
	app := opts.app
	if app == "" {
		app = rc.HubAppID
	}
	sessions := opts.sessionsBody
	if sessions == "" {
		sessions = `{"sessions":[]}`
	}
	inner := http.NewServeMux()
	var mux http.Handler = inner
	if opts.onRequest != nil {
		observe := opts.onRequest
		mux = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observe(r)
			inner.ServeHTTP(w, r)
		})
	}
	inner.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"app":"` + app + `","version":"test","pid":1}`))
	})
	inner.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sessions))
	})
	inner.HandleFunc("GET /v1/sessions/{slug}/messages", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[],"truncated":false}`))
	})
	inner.HandleFunc("POST /v1/sessions/{slug}/input", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"delivered":true}`))
	})
	// The contract-v2 verbs (internal/ext/rc/hub_verbs.go): the fake hub answers
	// with the pinned success shape so the proxy tests can assert routing/pass-through
	// independent of the real hub's current not_supported rejection (R0 rejects
	// every kind; the proxy allowlist doesn't know or care what the hub decides).
	inner.HandleFunc("POST /v1/sessions/{slug}/turn", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"turn_id":"t1"}`))
	})
	inner.HandleFunc("POST /v1/sessions/{slug}/interrupt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"interrupting":true}`))
	})
	inner.HandleFunc("POST /v1/sessions/{slug}/approvals/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resolved":true,"decision":"allow"}`))
	})
	inner.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(": ok\n\n"))
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// dialTo returns a dialFn that dials addr regardless of the requested shed/port
// (the proxy transport ignores the address and delegates to DialService).
func dialTo(addr string) func(context.Context, string, uint16) (net.Conn, error) {
	return func(ctx context.Context, _ string, _ uint16) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

// TestClassifyRCProxyPath pins the allowlist + traversal defense at the unit
// level: only the seven exact method/path shapes pass (the four pre-existing GET
// /POST-input routes plus the contract-v2 turn/interrupt/approvals verbs added in
// C3); a wrong method is 405; an unknown path or an unsafe {slug}/{id} (dots,
// traversal, empty, slash-bearing) is 404.
func TestClassifyRCProxyPath(t *testing.T) {
	cases := []struct {
		method, rest string
		wantOK       bool
		wantStatus   int
	}{
		{http.MethodGet, "v1/sessions", true, 0},
		{http.MethodGet, "v1/events", true, 0},
		{http.MethodGet, "v1/sessions/abc234/messages", true, 0},
		{http.MethodPost, "v1/sessions/abc234/input", true, 0},
		// The three contract-v2 verbs (C3).
		{http.MethodPost, "v1/sessions/abc234/turn", true, 0},
		{http.MethodPost, "v1/sessions/abc234/interrupt", true, 0},
		{http.MethodPost, "v1/sessions/abc234/approvals/call_01HQ8Z3K", true, 0},
		// The approval id grammar (rc.ApprovalIDRe) allows the dot/colon/underscore/
		// dash a native lane id carries — prove one of each passes the classifier.
		{http.MethodPost, "v1/sessions/abc234/approvals/req-42_a.b:c", true, 0},
		// Wrong method on a known path → 405.
		{http.MethodPost, "v1/sessions", false, http.StatusMethodNotAllowed},
		{http.MethodGet, "v1/sessions/abc234/input", false, http.StatusMethodNotAllowed},
		{http.MethodDelete, "v1/events", false, http.StatusMethodNotAllowed},
		{http.MethodGet, "v1/sessions/abc234/turn", false, http.StatusMethodNotAllowed},
		{http.MethodGet, "v1/sessions/abc234/interrupt", false, http.StatusMethodNotAllowed},
		{http.MethodGet, "v1/sessions/abc234/approvals/call_01HQ8Z3K", false, http.StatusMethodNotAllowed},
		// Unknown paths → 404 (steer is not a contract verb).
		{http.MethodGet, "v1/bogus", false, http.StatusNotFound},
		{http.MethodGet, "v1/sessions/abc234/history", false, http.StatusNotFound},
		{http.MethodGet, "v2/sessions", false, http.StatusNotFound},
		{http.MethodPost, "v1/sessions/abc234/steer", false, http.StatusNotFound},
		// Unsafe slug (dot / traversal / empty) → 404, never proxied.
		{http.MethodGet, "v1/sessions/a.b/messages", false, http.StatusNotFound},
		{http.MethodGet, "v1/sessions/../messages", false, http.StatusNotFound},
		{http.MethodPost, "v1/sessions//input", false, http.StatusNotFound},
		// Extra depth → 404.
		{http.MethodGet, "v1/sessions/abc/x/messages", false, http.StatusNotFound},
		// Traversal guard on the {slug} wildcard, turn/interrupt/approvals routes:
		// a literal ".." and a percent-encoded "%2E%2E" (chi's wildcard capture
		// preserves the RAW, still-encoded segment when the request's raw path
		// differs from its decoded form — verified in TestRCProxy_TraversalGuard_
		// PercentEncoded — so a "%2E%2E" segment reaches the classifier literally
		// and fails rcSlugRe on the '%' character, not because it was decoded to
		// "..") must both 404 before any dial.
		{http.MethodPost, "v1/sessions/../turn", false, http.StatusNotFound},
		{http.MethodPost, "v1/sessions/%2E%2E/turn", false, http.StatusNotFound},
		{http.MethodPost, "v1/sessions/../interrupt", false, http.StatusNotFound},
		{http.MethodPost, "v1/sessions/%2E%2E/interrupt", false, http.StatusNotFound},
		{http.MethodPost, "v1/sessions/../approvals/call_01", false, http.StatusNotFound},
		{http.MethodPost, "v1/sessions/%2E%2E/approvals/call_01", false, http.StatusNotFound},
		// Traversal guard on the {id} wildcard (approvals only): same two shapes,
		// rejected by rc.ApprovalIDRe's must-start-alphanumeric rule (".." can never
		// match) or, for the percent-encoded form, by the disallowed '%' character.
		{http.MethodPost, "v1/sessions/abc234/approvals/..", false, http.StatusNotFound},
		{http.MethodPost, "v1/sessions/abc234/approvals/%2E%2E", false, http.StatusNotFound},
		// Slash-bearing ids → 404: a literal "/" simply adds a path segment (wrong
		// depth for the 5-segment approvals shape); a percent-encoded "%2F" stays a
		// single segment (chi preserves the raw encoding here too) and fails
		// rc.ApprovalIDRe on '%'. Either way the id can never smuggle a path
		// separator into the proxied request.
		{http.MethodPost, "v1/sessions/abc234/approvals/a/b", false, http.StatusNotFound},
		{http.MethodPost, "v1/sessions/abc234/approvals/a%2Fb", false, http.StatusNotFound},
		// An approval id that is syntactically invalid (fails rc.ApprovalIDRe) but
		// contains none of the above traversal/slash characters still 404s here —
		// this is the proxy-classifier half of the documented asymmetry at
		// classifyRCProxyPath: the hub's own 400 invalid_approval_id branch only
		// ever sees ids that already passed this same grammar, so with the shared
		// rc.ApprovalIDRe symbol that branch is unreachable through the proxy.
		{http.MethodPost, "v1/sessions/abc234/approvals/.leading-dot", false, http.StatusNotFound},
	}
	for _, tc := range cases {
		gotOK, gotStatus := classifyRCProxyPath(tc.method, tc.rest)
		if gotOK != tc.wantOK || (!gotOK && gotStatus != tc.wantStatus) {
			t.Errorf("classify(%s %q) = (%v,%d), want (%v,%d)",
				tc.method, tc.rest, gotOK, gotStatus, tc.wantOK, tc.wantStatus)
		}
	}
}

// TestRCProxy_AllowlistRouting drives the four allowed shapes end-to-end through
// the router into a live fake hub, and the reject shapes (405/404) which are
// resolved before any dial.
func TestRCProxy_AllowlistRouting(t *testing.T) {
	addr := startFakeHub(t, hubMuxOpts{})
	be := &rcFakeBackend{dialFn: dialTo(addr)}
	srv := newRCServer(be)

	do := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}

	if w := do(http.MethodGet, "/api/sheds/proj/rc/v1/sessions"); w.Code != http.StatusOK {
		t.Errorf("GET v1/sessions = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if w := do(http.MethodGet, "/api/sheds/proj/rc/v1/sessions/abc234/messages"); w.Code != http.StatusOK {
		t.Errorf("GET messages = %d, want 200", w.Code)
	}
	if w := do(http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/input"); w.Code != http.StatusOK {
		t.Errorf("POST input = %d, want 200", w.Code)
	}
	// The events stream is long-lived; bound the request context so the proxied
	// SSE (and the fake hub's blocking handler) terminates and the test doesn't
	// hang. The 200 + content-type are committed on the first flush.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		r := httptest.NewRequest(http.MethodGet, "/api/sheds/proj/rc/v1/events", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
			t.Errorf("GET events = %d ct=%q, want 200 text/event-stream", w.Code, w.Header().Get("Content-Type"))
		}
	}
	// Reject shapes short-circuit before any dial.
	if w := do(http.MethodPost, "/api/sheds/proj/rc/v1/sessions"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST v1/sessions = %d, want 405", w.Code)
	}
	if w := do(http.MethodGet, "/api/sheds/proj/rc/v1/bogus"); w.Code != http.StatusNotFound {
		t.Errorf("GET v1/bogus = %d, want 404", w.Code)
	}
}

// TestRCProxy_VerbRoutesAllowlistRouting is TestRCProxy_AllowlistRouting's
// companion for the three contract-v2 verbs added in C3: each is proxied through
// to the live fake hub (which answers with the pinned success shape) end-to-end,
// alongside the reject shapes those routes introduce — wrong method (405), an
// unrecognized verb on an otherwise well-formed session path (404), and an
// approval id that fails rc.ApprovalIDRe (404, never dialed — see
// TestRCProxy_ApprovalRoutes_RejectedBeforeDial for the no-dial proof).
func TestRCProxy_VerbRoutesAllowlistRouting(t *testing.T) {
	addr := startFakeHub(t, hubMuxOpts{})
	be := &rcFakeBackend{dialFn: dialTo(addr)}
	srv := newRCServer(be)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		r := httptest.NewRequest(method, path, rdr)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}

	if w := do(http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/turn", `{"text":"hi"}`); w.Code != http.StatusOK {
		t.Errorf("POST turn = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if w := do(http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/interrupt", ""); w.Code != http.StatusOK {
		t.Errorf("POST interrupt = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if w := do(http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/approvals/call_01HQ8Z3K", `{"decision":"allow"}`); w.Code != http.StatusOK {
		t.Errorf("POST approvals = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	// Wrong method on a known verb path → 405, before any dial.
	if w := do(http.MethodGet, "/api/sheds/proj/rc/v1/sessions/abc234/turn", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET turn = %d, want 405", w.Code)
	}
	// An unrecognized verb → 404 (the classifier's example case: steer is not a
	// contract verb).
	if w := do(http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/steer", ""); w.Code != http.StatusNotFound {
		t.Errorf("POST steer = %d, want 404", w.Code)
	}
	// An approval id that fails rc.ApprovalIDRe → 404 before any dial (the path
	// simply isn't recognized; see classifyRCProxyPath's doc comment for the
	// asymmetry with the hub's own 400 invalid_approval_id branch).
	if w := do(http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/approvals/..", ""); w.Code != http.StatusNotFound {
		t.Errorf("POST approvals/.. = %d, want 404", w.Code)
	}
}

// TestRCProxy_TraversalGuard_PercentEncoded proves the traversal guard holds for
// a REAL HTTP request carrying a percent-encoded ".." (not just the classifier
// unit test's literal string input). chi's wildcard capture returns the raw,
// still-encoded path segment whenever the request's raw path differs from its
// decoded form — so "%2E%2E" reaches classifyRCProxyPath as the literal four
// characters '%','2','E','.'… (never silently decoded to "..") and is rejected by
// rcSlugRe/rc.ApprovalIDRe on the disallowed '%' character. Both outcomes (a
// decoded ".." and a still-encoded "%2E%2E") land on the same 404 verdict, so a
// server that DID decode first would not be distinguishable from this test's pass
// — the point is that neither encoding ever reaches a dial.
func TestRCProxy_TraversalGuard_PercentEncoded(t *testing.T) {
	be := &rcFakeBackend{
		dialFn: func(context.Context, string, uint16) (net.Conn, error) {
			panic("must not dial: traversal path should 404 before any dial")
		},
	}
	srv := newRCServer(be)

	for _, path := range []string{
		"/api/sheds/proj/rc/v1/sessions/%2E%2E/turn",
		"/api/sheds/proj/rc/v1/sessions/abc234/approvals/%2E%2E",
		"/api/sheds/proj/rc/v1/sessions/abc234/approvals/a%2Fb",
	} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, w.Code)
		}
	}
}

// TestRCProxy_ApprovalRoutes_RejectedBeforeDial proves "rejected before dial" is
// literal, not just a status-code coincidence: dialFn panics if ever invoked, so a
// classifier bug that let a traversal/invalid-id request through to
// ensureHubReachable/the proxy transport would fail this test even if the final
// status still happened to read 404.
func TestRCProxy_ApprovalRoutes_RejectedBeforeDial(t *testing.T) {
	be := &rcFakeBackend{
		dialFn: func(context.Context, string, uint16) (net.Conn, error) {
			panic("must not dial: rejected shape should 404/405 before any dial")
		},
	}
	srv := newRCServer(be)

	cases := []struct {
		method, path string
		wantStatus   int
	}{
		{http.MethodGet, "/api/sheds/proj/rc/v1/sessions/abc234/turn", http.StatusMethodNotAllowed},
		// Every non-POST method on each new route must 405 WITHOUT dialing — the
		// routing test's fake hub is method-aware, so only this panicking-dial table
		// proves a wrong method never reaches the guest.
		{http.MethodHead, "/api/sheds/proj/rc/v1/sessions/abc234/turn", http.StatusMethodNotAllowed},
		{http.MethodPut, "/api/sheds/proj/rc/v1/sessions/abc234/interrupt", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/sheds/proj/rc/v1/sessions/abc234/interrupt", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/sheds/proj/rc/v1/sessions/abc234/approvals/call_01", http.StatusMethodNotAllowed},
		{http.MethodOptions, "/api/sheds/proj/rc/v1/sessions/abc234/approvals/call_01", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/steer", http.StatusNotFound},
		{http.MethodPost, "/api/sheds/proj/rc/v1/sessions/../turn", http.StatusNotFound},
		{http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/approvals/..", http.StatusNotFound},
		{http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/approvals/a/b", http.StatusNotFound},
		// A doubled slash after /rc is an EMPTY segment: TrimPrefix would otherwise
		// normalize it into an allowed path, so the handler rejects it explicitly
		// (exact-path-or-404 — the classification must describe the path as sent).
		{http.MethodPost, "/api/sheds/proj/rc//v1/sessions/abc234/turn", http.StatusNotFound},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		if w.Code != tc.wantStatus {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, w.Code, tc.wantStatus)
		}
	}
}

// TestRCProxy_OversizedPOSTBody pins the documented 413 contract for the POST
// verbs when the blanket rcHubInputBodyLimit cap is exceeded THROUGH the proxy
// (turn included).
//
// The stdlib's own auto-413 never fires on this path: http.MaxBytesReader's
// "requestTooLarge" hook only triggers when the ResponseWriter is the real
// *http.response the stdlib server constructs per connection (an unexported
// interface check) — httptest.ResponseRecorder is not that type, and more
// fundamentally the body here is STREAMED to the upstream guest hub by
// httputil.ReverseProxy/http.Transport, not read directly by our handler. So when
// the wrapped reader trips the cap mid-stream the Transport sees a body-read
// error, RoundTrip fails, and ReverseProxy's ErrorHandler runs — which is why the
// ErrorHandler classifies *http.MaxBytesError explicitly and answers 413
// REQUEST_TOO_LARGE, keeping 502 RC_PROXY_FAILED for genuine upstream failures.
// The hub enforces the same 16 KiB independently (internal/ext/rc/hub_verbs.go,
// decodeHubBody), so a direct-to-hub caller sees a matching rejection.
func TestRCProxy_OversizedPOSTBody(t *testing.T) {
	addr := startFakeHub(t, hubMuxOpts{})
	be := &rcFakeBackend{dialFn: dialTo(addr)}
	srv := newRCServer(be)

	oversized := strings.Repeat("a", rcHubInputBodyLimit+1)
	r := httptest.NewRequest(http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/turn",
		strings.NewReader(`{"text":"`+oversized+`"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "REQUEST_TOO_LARGE") {
		t.Fatalf("oversized proxied body: got %d %s, want 413 REQUEST_TOO_LARGE",
			w.Code, w.Body.String())
	}
}

// TestRCProxy_StripsCredentialHeaders pins the proxy's inbound-header allowlist:
// the guest hub must NEVER receive the client's Authorization (the control-scope
// bearer token in token mode) or Cookie headers — a guest that saw them could
// replay the token against the server API — while allowlisted headers
// (Content-Type) still pass. Asserted on both the GET and POST proxied paths, in
// token mode so a real bearer token is on the inbound request.
func TestRCProxy_StripsCredentialHeaders(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]http.Header{} // method+path → headers the fake hub received
	addr := startFakeHub(t, hubMuxOpts{
		onRequest: func(r *http.Request) {
			mu.Lock()
			seen[r.Method+" "+r.URL.Path] = r.Header.Clone()
			mu.Unlock()
		},
	})
	be := &rcFakeBackend{dialFn: dialTo(addr)}
	srv, control, _ := newTokenModeRCServer(t, be)

	do := func(method, path, body string) {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		r := httptest.NewRequest(method, path, rdr)
		r.Header.Set("Authorization", "Bearer "+control)
		r.Header.Set("Cookie", "session=secret")
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200 (body %s)", method, path, w.Code, w.Body.String())
		}
	}

	do(http.MethodGet, "/api/sheds/proj/rc/v1/sessions", "")
	do(http.MethodPost, "/api/sheds/proj/rc/v1/sessions/abc234/input", `{"text":"hi"}`)

	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{"GET /v1/sessions", "POST /v1/sessions/abc234/input"} {
		h, ok := seen[key]
		if !ok {
			t.Fatalf("fake hub never saw %s (saw: %v)", key, seen)
		}
		if got := h.Get("Authorization"); got != "" {
			t.Errorf("%s: Authorization leaked to the guest hub: %q", key, got)
		}
		if got := h.Get("Cookie"); got != "" {
			t.Errorf("%s: Cookie leaked to the guest hub: %q", key, got)
		}
		if got := h.Get("X-Forwarded-For"); got != "" {
			t.Errorf("%s: non-allowlisted header forwarded: X-Forwarded-For=%q", key, got)
		}
	}
	if got := seen["POST /v1/sessions/abc234/input"].Get("Content-Type"); got != "application/json" {
		t.Errorf("allowlisted Content-Type not forwarded on POST: %q", got)
	}
}

// TestRCProxy_Scope: control scope required (default auth branch). A credentials
// token is 403, no token 401, a control token 200.
func TestRCProxy_Scope(t *testing.T) {
	addr := startFakeHub(t, hubMuxOpts{})
	be := &rcFakeBackend{dialFn: dialTo(addr)}
	srv, control, credentials := newTokenModeRCServer(t, be)

	call := func(token string) int {
		r := httptest.NewRequest(http.MethodGet, "/api/sheds/proj/rc/v1/sessions", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w.Code
	}
	if got := call(""); got != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", got)
	}
	if got := call(credentials); got != http.StatusForbidden {
		t.Errorf("credentials token: got %d, want 403", got)
	}
	if got := call(control); got != http.StatusOK {
		t.Errorf("control token: got %d, want 200", got)
	}
}

// TestRCProxy_ForeignListener: a listener that answers /v1/health but is NOT a
// hub (wrong app identity) is never started over and yields 503 RC_HUB_UNAVAILABLE.
func TestRCProxy_ForeignListener(t *testing.T) {
	addr := startFakeHub(t, hubMuxOpts{app: "not-a-hub"})
	be := &rcFakeBackend{dialFn: dialTo(addr)}
	srv := newRCServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/sheds/proj/rc/v1/sessions", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), rcHubUnavailableCode) {
		t.Fatalf("foreign listener: got %d %s, want 503 %s", w.Code, w.Body.String(), rcHubUnavailableCode)
	}
	// A foreign listener must NOT trigger an ensure-start exec.
	if got := be.execCalls.Load(); got != 0 {
		t.Fatalf("foreign listener triggered %d execs, want 0", got)
	}
}

// TestRCProxy_ColdStart: the hub is absent on first probe; the proxy execs
// `shed-ext-rc serve --detach` once, the hub comes up, and the request is
// proxied (200).
func TestRCProxy_ColdStart(t *testing.T) {
	addr := startFakeHub(t, hubMuxOpts{})
	var up atomic.Bool
	be := &rcFakeBackend{
		dialFn: func(ctx context.Context, _ string, _ uint16) (net.Conn, error) {
			if !up.Load() {
				return nil, errors.New("connect: connection refused")
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
	}
	be.execFn = func(_ context.Context, _ string, opts backend.ExecOptions) error {
		if got := strings.Join(opts.Cmd, " "); got != "shed-ext-rc serve --detach" {
			t.Errorf("ensure-start cmd = %q, want shed-ext-rc serve --detach", got)
		}
		up.Store(true) // the daemon is now listening
		return nil
	}
	srv := newRCServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/sheds/proj/rc/v1/sessions", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("cold start: got %d %s, want 200", w.Code, w.Body.String())
	}
	if got := be.execCalls.Load(); got != 1 {
		t.Fatalf("cold start execs = %d, want exactly 1", got)
	}
}

// TestEnsureHub_ConcurrentSingleflight: N racing ensure calls for an absent hub
// share ONE ensure-start exec (singleflight) and one breaker outcome.
func TestEnsureHub_ConcurrentSingleflight(t *testing.T) {
	addr := startFakeHub(t, hubMuxOpts{})
	var up atomic.Bool
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	be := &rcFakeBackend{
		dialFn: func(ctx context.Context, _ string, _ uint16) (net.Conn, error) {
			if !up.Load() {
				return nil, errors.New("connect: connection refused")
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
	}
	be.execFn = func(context.Context, string, backend.ExecOptions) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // hold the flight open so the others join
		up.Store(true)
		return nil
	}
	srv := newRCServer(be)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	entered := make(chan struct{}, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entered <- struct{}{}
			errs[i] = srv.ensureHubReachable(context.Background(), "proj")
		}()
	}
	for range n {
		<-entered
	}
	<-started
	close(release)
	wg.Wait()

	if got := be.execCalls.Load(); got != 1 {
		t.Fatalf("%d concurrent ensures made %d execs, want exactly 1", n, got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
}

// TestEnsureHub_BreakerTrips: a hub that never comes up trips the breaker after
// rcHubBreakerThreshold start attempts, after which ensure returns immediately
// with no further exec.
func TestEnsureHub_BreakerTrips(t *testing.T) {
	be := &rcFakeBackend{
		dialFn: func(context.Context, string, uint16) (net.Conn, error) {
			return nil, errors.New("connect: connection refused")
		},
		execFn: func(context.Context, string, backend.ExecOptions) error { return nil }, // start "succeeds" but hub never listens
	}
	srv := newRCServer(be)

	for i := 0; i < rcHubBreakerThreshold; i++ {
		if err := srv.ensureHubReachable(context.Background(), "proj"); !errors.Is(err, errRCHubUnavailable) {
			t.Fatalf("attempt %d: err=%v, want errRCHubUnavailable", i, err)
		}
	}
	if got := be.execCalls.Load(); got != int32(rcHubBreakerThreshold) {
		t.Fatalf("execs before trip = %d, want %d", got, rcHubBreakerThreshold)
	}
	// Breaker now open: the next ensure returns immediately with NO exec.
	if err := srv.ensureHubReachable(context.Background(), "proj"); !errors.Is(err, errRCHubUnavailable) {
		t.Fatalf("post-trip err=%v, want errRCHubUnavailable", err)
	}
	if got := be.execCalls.Load(); got != int32(rcHubBreakerThreshold) {
		t.Fatalf("breaker-open ensure exec'd (%d), want it suppressed at %d", got, rcHubBreakerThreshold)
	}
}

// TestBreaker_LifecycleReset: the four shed lifecycle transitions
// (stop/start/reset/delete) clear the shed's hub-start breaker entry — a tripped
// breaker must not outlive the shed state that tripped it, and the map must not
// accumulate entries for deleted sheds (the rcCaps.invalidate seam, extended).
func TestBreaker_LifecycleReset(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"stop", http.MethodPost, "/api/sheds/proj/stop"},
		{"start", http.MethodPost, "/api/sheds/proj/start"},
		{"reset", http.MethodPost, "/api/sheds/proj/reset"},
		{"delete", http.MethodDelete, "/api/sheds/proj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &rcFakeBackend{}
			srv := newRCServer(be)
			// Trip the breaker for proj.
			for i := 0; i < rcHubBreakerThreshold; i++ {
				srv.rcHubBreaker.failure("proj")
			}
			if srv.rcHubBreaker.allow("proj") {
				t.Fatal("precondition: breaker should be open")
			}
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
				t.Fatalf("%s %s = %d", tc.method, tc.path, w.Code)
			}
			if !srv.rcHubBreaker.allow("proj") {
				t.Fatalf("%s must reset the breaker entry", tc.name)
			}
			// The entry is gone, not merely closed (leak guard).
			srv.rcHubBreaker.mu.Lock()
			_, present := srv.rcHubBreaker.m["proj"]
			srv.rcHubBreaker.mu.Unlock()
			if present {
				t.Fatalf("%s must delete the breaker entry, not just close it", tc.name)
			}
		})
	}
}

// TestRCProxy_HubDown models a hub-down degrade: DialService succeeds in
// reaching the guest agent's TCP proxy, but the rc hub itself is not listening
// on 127.0.0.1:1029 (old image, or the hub failed to start), so every dial to
// the hub port is refused. The proxy attempts one start then returns 503
// RC_HUB_UNAVAILABLE. This is backend-agnostic: both VZ and Firecracker route
// through the tcpproxy now, so a refused dial means the hub is down, not that
// the backend is structurally unable to reach loopback.
func TestRCProxy_HubDown(t *testing.T) {
	be := &rcFakeBackend{
		dialFn: func(context.Context, string, uint16) (net.Conn, error) {
			return nil, errors.New("connect: connection refused") // hub not listening on 127.0.0.1:1029
		},
		execFn: func(context.Context, string, backend.ExecOptions) error { return nil },
	}
	srv := newRCServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/sheds/proj/rc/v1/sessions", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), rcHubUnavailableCode) {
		t.Fatalf("hub down: got %d %s, want 503 %s", w.Code, w.Body.String(), rcHubUnavailableCode)
	}
}

// TestRCProxy_ShedNotRunning: DialService reporting the shed-not-running sentinel
// surfaces as 503 SHED_NOT_RUNNING (not the hub-unavailable code) and no start.
func TestRCProxy_ShedNotRunning(t *testing.T) {
	be := &rcFakeBackend{
		dialFn: func(context.Context, string, uint16) (net.Conn, error) {
			return nil, config.ErrShedNotRunningSentinel
		},
	}
	srv := newRCServer(be)

	r := httptest.NewRequest(http.MethodGet, "/api/sheds/proj/rc/v1/sessions", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "SHED_NOT_RUNNING") {
		t.Fatalf("shed not running: got %d %s, want 503 SHED_NOT_RUNNING", w.Code, w.Body.String())
	}
	if got := be.execCalls.Load(); got != 0 {
		t.Fatalf("shed-not-running triggered %d execs, want 0", got)
	}
}

// TestEnrich_HubActivityOverlay: after the exec-based enrichment, a reachable hub's
// /v1/sessions activity is overlaid onto the rc row (activity/activity_at/last_message).
func TestEnrich_HubActivityOverlay(t *testing.T) {
	const sessions = `{"sessions":[{"slug":"abc234","tmux_session":"rc-abc234",` +
		`"kind":"claude-rc","state":"ready","managed":true,"activity":"working",` +
		`"activity_at":"2026-07-11T10:00:00Z","last_message":"running tests"}]}`
	addr := startFakeHub(t, hubMuxOpts{sessionsBody: sessions})
	be := &rcFakeBackend{
		sessions: map[string][]config.Session{
			"proj": {{Name: "rc-abc234", ShedName: "proj"}},
		},
		execFn: execServes(newEnvelope),
		dialFn: dialTo(addr),
	}
	srv := newRCServer(be)

	resp := getSessions(t, srv, "/api/sheds/proj/sessions")
	row := findSession(resp.Sessions, "rc-abc234")
	if row == nil || row.RC == nil {
		t.Fatalf("row not enriched: %+v", row)
	}
	if row.RC.Activity != "working" || row.RC.ActivityAt != "2026-07-11T10:00:00Z" || row.RC.LastMessage != "running tests" {
		t.Fatalf("hub activity not overlaid: %+v", row.RC)
	}
}

// TestEnrich_HubConsultMiss_SilentFallback: with no reachable hub (dial refused),
// the rc row still enriches from the exec but carries no activity, and no warning
// is added for the consult miss.
func TestEnrich_HubConsultMiss_SilentFallback(t *testing.T) {
	be := &rcFakeBackend{
		sessions: map[string][]config.Session{
			"proj": {{Name: "rc-abc234", ShedName: "proj"}},
		},
		execFn: execServes(newEnvelope),
		// dialFn nil → the default connection-refused stub → consult misses silently.
	}
	srv := newRCServer(be)

	resp := getSessions(t, srv, "/api/sheds/proj/sessions")
	row := findSession(resp.Sessions, "rc-abc234")
	if row == nil || row.RC == nil {
		t.Fatalf("row not enriched: %+v", row)
	}
	if row.RC.Activity != "" || row.RC.ActivityAt != "" || row.RC.LastMessage != "" {
		t.Fatalf("consult miss must leave activity empty: %+v", row.RC)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("consult miss must be silent (no warnings), got %v", resp.Warnings)
	}
}

// TestServerFeatures_RCProxyAndEvents: the rc-proxy and rc-events tokens are
// advertised on /api/info (and via serverFeatures alongside the existing tokens).
func TestServerFeatures_RCProxyAndEvents(t *testing.T) {
	feats := serverFeatures()
	for _, want := range []string{FeatureOverview, FeatureRCEnrich, FeatureRCEvents, FeatureRCProxy} {
		if !sliceHas(feats, want) {
			t.Fatalf("serverFeatures() missing %q: %v", want, feats)
		}
	}
	be := &rcFakeBackend{}
	srv := newRCServer(be)
	r := httptest.NewRequest(http.MethodGet, "/api/info", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), FeatureRCEvents) || !strings.Contains(w.Body.String(), FeatureRCProxy) {
		t.Fatalf("/api/info missing rc-events/rc-proxy tokens: %s", w.Body.String())
	}
}
