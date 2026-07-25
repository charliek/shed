package servertls

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

type ctxKey struct{}

// TestPinnedTransportPassesTheRequestContextToTheCertSource pins the mechanism
// the per-request credential pin rides on: for an http.Transport dial, the TLS
// handshake runs under the context of the REQUEST that triggered it, so a value
// attached to that request reaches GetClientCertificate.
//
// This is a real behavior of net/http rather than something this package
// controls, which is exactly why it is asserted: if it ever stopped holding,
// the credential pin would degrade silently to "read the live source" — the
// race it exists to close — with no other test failing.
func TestPinnedTransportPassesTheRequestContextToTheCertSource(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	defer srv.Close()

	seen := make(chan string, 1)
	tr := PinnedTransport(Fingerprint(srv.Certificate().Raw), func(ctx context.Context) *tls.Certificate {
		v, _ := ctx.Value(ctxKey{}).(string)
		select {
		case seen <- v:
		default:
		}
		return nil
	})

	req, err := http.NewRequestWithContext(
		context.WithValue(context.Background(), ctxKey{}, "pinned"), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	select {
	case got := <-seen:
		if got != "pinned" {
			t.Errorf("the cert source saw context value %q, want pinned — "+
				"the request context no longer reaches the TLS handshake, so per-request credential pinning is inert", got)
		}
	default:
		t.Fatal("GetClientCertificate was never called")
	}
}

// TestPinnedClientConfigToleratesAContextlessHandshake: a hand-built
// CertificateRequestInfo (a test, or a TLS stack that does not populate one)
// carries no context. The source must still be called, with a usable context,
// rather than the callback panicking mid-handshake.
func TestPinnedClientConfigToleratesAContextlessHandshake(t *testing.T) {
	called := false
	cfg := PinnedClientConfig("sha256:whatever", func(ctx context.Context) *tls.Certificate {
		called = true
		if ctx == nil {
			t.Error("the cert source was handed a nil context")
		}
		return nil
	})
	cert, err := cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate: %v", err)
	}
	if !called {
		t.Error("the cert source was not consulted")
	}
	// An EMPTY certificate, never an error: the handshake must reach the server
	// so it can answer with its own alert (see the PinnedClientConfig doc).
	if cert == nil || len(cert.Certificate) != 0 {
		t.Errorf("cert = %v, want an empty tls.Certificate", cert)
	}
}
