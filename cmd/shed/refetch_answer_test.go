package main

// A refetch moves an entry to the port the server reports only when the
// entry's own endpoint could not be reached or did not prove it is the server
// (endpointUnreached). An answer from it (an HTTP status, a body that does not
// decode, a TLS refusal of the certificate) is terminal.

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// answering is a pinned endpoint that answers every request with status and
// body.
func answering(t *testing.T, status int, body string) string {
	t.Helper()
	srv, _ := pinnedEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}), nil)
	return srv.URL
}

// refusingCertificates is a pinned endpoint whose TLS layer requires a client
// certificate, which a token-mode client does not present: it refuses with a
// TLS alert once the handshake has proved it holds the pinned certificate.
func refusingCertificates(t *testing.T) string {
	t.Helper()
	srv, _ := pinnedEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a request got past the certificate requirement: %s %s", r.Method, r.URL.Path)
	}), &tls.Config{ClientAuth: tls.RequireAnyClientCert})
	return srv.URL
}

// hangingUp is a TCP listener that accepts each connection and closes it.
func hangingUp(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return "https://" + ln.Addr().String()
}

// blackHole is a TCP listener that accepts each connection and never writes,
// holding it until the test ends: a request to it times out.
func blackHole(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	return "https://" + ln.Addr().String()
}

func closedEndpoint(t *testing.T) string {
	return "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(freeClosedPort(t)))
}

// TestRefetchAnAnswerIsNotAMovedPort: a token-mode refetch whose entry's own
// endpoint (not the port the server reports) fails in each way. An answer is
// refused there, naming the endpoint and what it answered; the endpoint the
// server reports is never tried, and nothing is written. Only an endpoint
// that could not be reached or did not prove it is the server moves the
// entry. Negative control: with the old rule (any error falls back to the
// reported port), each answer case moves the entry instead.
func TestRefetchAnAnswerIsNotAMovedPort(t *testing.T) {
	cases := []struct {
		name   string
		stored func(t *testing.T) string
		// wantErr: the refusal's parts; nil when the entry moves.
		wantErr []string
	}{
		{"a 401", func(t *testing.T) string { return answering(t, http.StatusUnauthorized, "") },
			[]string{"answered HTTP 401"}},
		{"a 403 with the API's error body", func(t *testing.T) string {
			return answering(t, http.StatusForbidden, `{"error":{"code":"FORBIDDEN","message":"not from here"}}`)
		}, []string{"answered HTTP 403", "FORBIDDEN: not from here"}},
		{"a 500", func(t *testing.T) string { return answering(t, http.StatusInternalServerError, "boom") },
			[]string{"answered HTTP 500"}},
		{"a 200 whose body does not decode", func(t *testing.T) string { return answering(t, http.StatusOK, "not json") },
			[]string{"could not be used", "failed to parse response"}},
		{"a TLS refusal of the credential", refusingCertificates,
			[]string{"refused it", "certificate required"}},
		{"a closed port moves the entry", closedEndpoint, nil},
		{"another certificate (the pin does not match) moves the entry",
			func(t *testing.T) string { return otherCertEndpoint(t).URL }, nil},
		{"a listener that hangs up moves the entry", hangingUp, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := tc.stored(t)
			w := newTokenRefetchWorld(t, func(string) string { return stored })
			out, err := w.refetch(t)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("refetch: %v\n%s", err, out)
				}
				for _, cfg := range loadedAndInMemory(t, config.GetClientConfigPath()) {
					if got := cfg.Servers["tok"].APIURL; got != w.reportedURL {
						t.Errorf("api_url = %q, want the reported %q", got, w.reportedURL)
					}
				}
				if !strings.Contains(out, "HTTPS port changed") {
					t.Errorf("the move was not printed:\n%s", out)
				}
				return
			}
			if err == nil {
				t.Fatalf("refetch succeeded, want a refusal; output:\n%s", out)
			}
			want := append([]string{
				"the server's API at " + stored,
				"nothing was saved",
				"the endpoint the server reports, " + w.reportedURL + ", was not tried",
			}, tc.wantErr...)
			for _, part := range want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q must contain %q", err, part)
				}
			}
			w.assertNothingElseHappened(t)
		})
	}
}

// TestEndpointUnreachedByType: the classification itself, on the errors a
// real request produces. A dial failure, a timeout and a handshake that did
// not authenticate the pinned server are "not reached"; every answer is not.
func TestEndpointUnreachedByType(t *testing.T) {
	cases := []struct {
		name      string
		url       func(t *testing.T) string
		unreached bool
	}{
		{"a closed port", closedEndpoint, true},
		{"a timeout", blackHole, true},
		{"another certificate", func(t *testing.T) string { return otherCertEndpoint(t).URL }, true},
		{"a listener that hangs up", hangingUp, true},
		{"a 401", func(t *testing.T) string { return answering(t, http.StatusUnauthorized, "") }, false},
		{"a 403", func(t *testing.T) string { return answering(t, http.StatusForbidden, "{}") }, false},
		{"a 302", func(t *testing.T) string {
			srv, _ := pinnedEndpoint(t, redirectingTo(newPlainListener(t)), nil)
			return srv.URL
		}, false},
		{"a 503", func(t *testing.T) string { return answering(t, http.StatusServiceUnavailable, "") }, false},
		{"a body that does not decode", func(t *testing.T) string { return answering(t, http.StatusOK, "[") }, false},
		{"a TLS refusal of the credential", refusingCertificates, false},
		{"a URL that does not parse (never sent)", func(t *testing.T) string { return closedEndpoint(t) + "\n" }, false},
	}
	pin := servertls.Fingerprint(mustHTTPTestCert(t))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newAPIClientWithSource(tc.url(t), pin, clienttoken.Static("shed_ctl_x"), DefaultTimeout)
			client.httpClient = client.newHTTPClient(300 * time.Millisecond)
			_, err := client.GetInfo()
			if err == nil {
				t.Fatal("GetInfo succeeded, want an error")
			}
			if got := endpointUnreached(err); got != tc.unreached {
				t.Errorf("endpointUnreached(%v) = %v, want %v", err, got, tc.unreached)
			}
			if tc.name == "a TLS refusal of the credential" && !hasPeerAlert(err) {
				t.Errorf("the refusal %v carries no *net.OpError \"remote error\": it is classified by its text only", err)
			}
		})
	}
}

// hasPeerAlert reports whether err's chain holds the error crypto/tls makes
// of an alert the peer sent (a *net.OpError whose Op is "remote error").
func hasPeerAlert(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if op, ok := e.(*net.OpError); ok && op.Op == "remote error" {
			return true
		}
	}
	return false
}

// mustHTTPTestCert is the DER of httptest's TLS certificate, which every
// pinnedEndpoint presents.
func mustHTTPTestCert(t *testing.T) []byte {
	t.Helper()
	srv, _ := pinnedEndpoint(t, http.NotFoundHandler(), nil)
	return srv.Certificate().Raw
}

// TestVerifyErrorEscapesTheEntrysURL: verifyServerEntry's own error shows the
// api_url, which comes from config.yaml on a refetch, escaped. The review's
// case: a hand-edited api_url with a trailing newline that names the endpoint
// the server reports (so it is the only endpoint tried) cannot be requested,
// and the refusal shows the newline escaped, never raw; nothing is written.
// Negative control: without the escaping, the raw newline is in the error.
func TestVerifyErrorEscapesTheEntrysURL(t *testing.T) {
	t.Run("a refetch with a trailing newline in api_url", func(t *testing.T) {
		w := newTokenRefetchWorld(t, func(reported string) string { return reported + "\n" })
		if got := clientConfig.Servers["tok"].APIURL; !strings.HasSuffix(got, "\n") {
			t.Fatalf("api_url = %q: the trailing newline did not survive config.yaml; the test proves nothing", got)
		}
		_, err := w.refetch(t)
		if err == nil {
			t.Fatal("refetch succeeded, want a refusal")
		}
		if strings.ContainsAny(err.Error(), "\n\r") {
			t.Errorf("a raw control character reached the error: %q", err)
		}
		if want := "the server's API at " + w.reportedURL + `\n`; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must show the api_url escaped, %q", err, want)
		}
		w.assertNothingElseHappened(t)
	})

	t.Run("an escape sequence in api_url", func(t *testing.T) {
		entry := &config.ServerEntry{APIURL: "https://h\x1b[2J:1", TLSCertFingerprint: "sha256:00"}
		_, err := verifyServerEntry(entry, clienttoken.Credential{})
		if err == nil {
			t.Fatal("verifyServerEntry succeeded")
		}
		if strings.ContainsRune(err.Error(), 0x1b) {
			t.Errorf("a raw escape character reached the error: %q", err)
		}
		if !strings.Contains(err.Error(), `https://h\x1b[2J:1`) {
			t.Errorf("error %q must show the api_url escaped", err)
		}
	})
}
