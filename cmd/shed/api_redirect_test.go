package main

// No API client follows a redirect (servertls.RefuseRedirects): shed-server's
// API answers no request with one, so a 3xx comes from something else at the
// endpoint, and following it would carry the request's credential to the
// Location it names, a plain-http URL included (Go keeps an Authorization
// header across a redirect to the same host on another port).

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/sdk"
)

// plainListener is a plain-http server a pinned endpoint redirects to. It
// records each request it sees with the Authorization header it carried, and
// answers as a token-mode server's API would, so a client that follows the
// redirect succeeds (the negative control).
type plainListener struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string // "<method> <path> auth=<Authorization>"
}

func newPlainListener(t *testing.T) *plainListener {
	t.Helper()
	p := &plainListener{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen = append(p.seen, fmt.Sprintf("%s %s auth=%s", r.Method, r.URL.Path, r.Header.Get("Authorization")))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/info" {
			_, _ = io.WriteString(w, `{"name":"tok","ssh_port":2222,"auth_mode":"token"}`)
			return
		}
		_, _ = io.WriteString(w, `{"name":"box","status":"running"}`)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// requests is every request the plain listener saw.
func (p *plainListener) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// redirectingTo answers every request with a 302 to the same path on target.
func redirectingTo(target *plainListener) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+r.URL.Path, http.StatusFound)
	})
}

// pinnedEndpoint starts a TLS server answering with h. Its certificate is
// httptest's own, so every endpoint it starts holds the same pin (that of
// any httptest TLS server). tlsCfg, when set, is the server's TLS config (a
// client-certificate requirement). conns counts the connections it accepted.
func pinnedEndpoint(t *testing.T, h http.Handler, tlsCfg *tls.Config) (srv *httptest.Server, conns *atomic.Int32) {
	t.Helper()
	conns = &atomic.Int32{}
	srv = httptest.NewUnstartedServer(h)
	srv.TLS = tlsCfg
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, conns
}

// otherCertEndpoint starts a TLS server with a certificate of its own, so a
// client pinned to httptest's certificate fails the pin check there.
func otherCertEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	cert, _, err := servertls.LoadOrGenerate(filepath.Join(dir, "tls.pem"), filepath.Join(dir, "tls.key"), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request reached the endpoint whose certificate is not pinned: %s %s", r.Method, r.URL.Path)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestAPIClientFollowsNoRedirect: an ordinary command's request paths (a JSON
// request, a long-timeout request, the create event stream, the ping) against
// a pinned endpoint that answers 302 to a plain-http URL. The answer is the
// 302, named in the error, and the plain listener sees no request and no
// Authorization header. Negative control: with the client following
// redirects (CheckRedirect nil in newHTTPClient), each path's request
// reaches the plain listener with the token.
func TestAPIClientFollowsNoRedirect(t *testing.T) {
	testClientConfig(t)
	plain := newPlainListener(t)
	redir, _ := pinnedEndpoint(t, redirectingTo(plain), nil)
	host, _ := hostPort(t, redir.URL)
	entry := config.ServerEntry{
		Host: host, SSHPort: 2222,
		APIURL:                redir.URL,
		TLSCertFingerprint:    servertls.Fingerprint(redir.Certificate().Raw),
		AuthMode:              config.AuthModeToken,
		ControlToken:          "shed_ctl_live",
		ControlTokenExpiresAt: time.Now().Add(24 * time.Hour), // outside the proactive refresh window
	}
	putServerEntry(t, "tok", entry)
	stubBootstrap(func() sdk.Credential {
		t.Error("a 302 is no authentication failure: nothing may be minted")
		return sdk.Credential{}
	})
	client := NewAPIClientFromNamedEntry("tok", &entry, DefaultTimeout)

	paths := []struct {
		name string
		call func() error
	}{
		{"a JSON request (GetInfo)", func() error { _, err := client.GetInfo(); return err }},
		{"a long-timeout request (StartShed)", func() error { _, err := client.StartShed("box"); return err }},
		{"the create event stream", func() error {
			_, err := client.CreateShedWithProgress(&config.CreateShedRequest{Name: "box"}, false, func(backend.ProgressEvent) {})
			return err
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			err := p.call()
			if err == nil || !strings.Contains(err.Error(), "302") {
				t.Errorf("err = %v, want the 302 named", err)
			}
		})
	}
	t.Run("the ping", func(t *testing.T) {
		if client.Ping() {
			t.Error("Ping answered true for a 302")
		}
	})
	if seen := plain.requests(); len(seen) != 0 {
		t.Errorf("the plain-http listener saw %d request(s), want none: %q", len(seen), seen)
	}
}

// TestRefetchFollowsNoRedirect: a token-mode refetch whose entry's own
// endpoint holds the pinned certificate and answers 302 to a plain-http URL.
// The new token reaches neither the plain listener nor (an answer being no
// sign of a moved port) the endpoint the server reports; the refetch is
// refused, naming the endpoint and the 302, and nothing is written. Negative
// control: with the client following redirects, the plain listener answers,
// the verification passes, and it saw the new token.
func TestRefetchFollowsNoRedirect(t *testing.T) {
	plain := newPlainListener(t)
	redir, _ := pinnedEndpoint(t, redirectingTo(plain), nil)
	w := newTokenRefetchWorld(t, func(string) string { return redir.URL })

	out, err := w.refetch(t)
	if seen := plain.requests(); len(seen) != 0 {
		t.Errorf("the plain-http listener saw %d request(s), want none: %q", len(seen), seen)
	}
	if err == nil {
		t.Fatalf("refetch succeeded, want a refusal; output:\n%s", out)
	}
	for _, want := range []string{"the server's API at " + redir.URL + " answered HTTP 302", "nothing was saved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err, want)
		}
	}
	w.assertNothingElseHappened(t)
}

// tokenRefetchWorld is a token-mode entry "tok", added against a token server
// at the port its bootstrap reports (the reported endpoint), whose api_url is
// then set to stored(the reported endpoint's URL): the state a refetch starts
// from. The refetch's bootstrap reports the same endpoint and pin.
type tokenRefetchWorld struct {
	host          string
	reportedURL   string
	reportedConns *atomic.Int32
	connsBefore   int32
	stored        string
	before        map[string]fileBytes
}

func newTokenRefetchWorld(t *testing.T, storedFor func(reportedURL string) string) *tokenRefetchWorld {
	t.Helper()
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	reported, conns := pinnedEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer shed_ctl_fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"tok","ssh_port":2222,"auth_mode":"token"}`)
	}), nil)
	host, port := hostPort(t, reported.URL)
	pin := servertls.Fingerprint(reported.Certificate().Raw)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapResult(tokenBundle("shed_ctl_fresh", pin, port, time.Now().Add(time.Hour)), nil)
	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("add: %v", err)
	}
	e, ok := clientConfig.Servers["tok"]
	if !ok {
		t.Fatalf("no entry tok: %+v", clientConfig.Servers)
	}
	stored := storedFor(reported.URL)
	e.APIURL = stored
	putServerEntry(t, "tok", e)

	serverUpdateRefetch, serverUpdateTrustTOFU = true, true
	return &tokenRefetchWorld{host: host, reportedURL: reported.URL, reportedConns: conns, stored: stored}
}

// refetch runs `shed server update tok --refetch`, recording the config
// directory and the reported endpoint's connections first.
func (w *tokenRefetchWorld) refetch(t *testing.T) (string, error) {
	t.Helper()
	w.before = snapshotConfigDir(t)
	w.connsBefore = w.reportedConns.Load()
	var err error
	out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"tok"}) })
	return out, err
}

// assertNothingElseHappened: after a refused refetch, every file under the
// config directory is as it was, the in-memory api_url is the stored one, and
// the reported endpoint accepted no connection.
func (w *tokenRefetchWorld) assertNothingElseHappened(t *testing.T) {
	t.Helper()
	assertConfigDirUnchanged(t, w.before)
	if got := clientConfig.Servers["tok"].APIURL; got != w.stored {
		t.Errorf("in-memory api_url = %q, want the stored %q", got, w.stored)
	}
	if n := w.reportedConns.Load() - w.connsBefore; n != 0 {
		t.Errorf("the endpoint the server reports accepted %d connection(s), want none", n)
	}
}
