package tunnels

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// ---------------------------------------------------------------------------
// The tunnel's own capture-vs-transmit race, and the probe's missing reactive
// recovery. Both are the tunnel-side twins of behaviors the control plane
// already had; a tunnel is the one long-lived thing in the CLI, so it is the
// path most likely to actually meet a mid-flight rotation.
// ---------------------------------------------------------------------------

// TestDialTransmitsTheGenerationItRecorded forces a refresh to land in the exact
// window the pin closes: after Dial captured its credential and generation, and
// before the TLS stack asked which certificate to present.
//
// Without the pin the handshake re-reads the live source and presents the NEW
// certificate while the dial has recorded the OLD generation. A rejection would
// then be blamed on a generation that is already superseded, Refresh would mint
// nothing, and the single re-dial would replay the rejected credential.
func TestDialTransmitsTheGenerationItRecorded(t *testing.T) {
	var src *clienttoken.Source
	var once sync.Once
	// Fires on the server's goroutine mid-handshake — see mtlsConnectServerHook.
	race := func() {
		once.Do(func() {
			_, gen := src.Current()
			if _, err := src.Refresh(gen); err != nil {
				t.Errorf("forced concurrent refresh: %v", err)
			}
		})
	}

	srv, ca, pin, lastSerial := mtlsConnectServerHook(t, upgradeEchoHandler(t, ""), race)

	captured, capturedSerial := issueTunnelCert(t, ca, "tunnel-key", 24*time.Hour)
	racing, racingSerial := issueTunnelCert(t, ca, "tunnel-key", 24*time.Hour)
	if capturedSerial == racingSerial {
		t.Fatal("test setup: the two certificates must be distinguishable by serial")
	}

	var mints int32
	src = clienttoken.New(
		clienttoken.MTLSCredential(captured, time.Now().Add(24*time.Hour)),
		func() (clienttoken.Credential, error) {
			atomic.AddInt32(&mints, 1)
			return clienttoken.MTLSCredential(racing, time.Now().Add(24*time.Hour)), nil
		})

	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)
	conn, err := client.Dial(context.Background(), "myshed", 8080)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)

	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Fatalf("mints = %d, want 1 (only the forced race — a second means the dial retried)", got)
	}
	if got := lastSerial.Load().(string); got != capturedSerial {
		t.Errorf("the handshake presented serial %s, want the CAPTURED %s (the racing mint was %s) — "+
			"the dial transmitted a generation it had not recorded", got, capturedSerial, racingSerial)
	}
}

// TestProbeReEnrollsOnCertificateRejection: the probe is the tunnel's startup
// gate, and it used to have only a PROACTIVE refresh. A certificate the server
// had already stopped accepting therefore failed startup with a message telling
// the operator to re-run the command — describing, as manual work, exactly the
// re-enrollment the client was able to perform itself. Dial and the control
// plane both recover from this; the probe now does too.
func TestProbeReEnrollsOnCertificateRejection(t *testing.T) {
	// The TLS-level rejection: an expired certificate never produces an HTTP
	// status at all, so the recovery cannot be keyed on a 401.
	t.Run("handshake rejection", func(t *testing.T) {
		srv, ca, pin, lastSerial := mtlsConnectServer(t, probeStatusHandler(t, "", http.StatusBadRequest))

		expired, _ := issueTunnelCert(t, ca, "tunnel-key", time.Nanosecond)
		fresh, freshSerial := issueTunnelCert(t, ca, "tunnel-key", 24*time.Hour)

		var mints int32
		src := clienttoken.New(
			// A far-future recorded expiry: nothing proactive fires, so the
			// server's refusal is the only thing that can drive the re-mint.
			clienttoken.MTLSCredential(expired, time.Now().Add(24*time.Hour)),
			func() (clienttoken.Credential, error) {
				atomic.AddInt32(&mints, 1)
				return clienttoken.MTLSCredential(fresh, time.Now().Add(24*time.Hour)), nil
			})
		client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Probe(ctx, "myshed"); err != nil {
			t.Fatalf("Probe should have re-enrolled and recovered: %v", err)
		}
		if got := atomic.LoadInt32(&mints); got != 1 {
			t.Errorf("mints = %d, want exactly 1", got)
		}
		if got := lastSerial.Load().(string); got != freshSerial {
			t.Errorf("the retried probe presented %s, want the re-minted %s", got, freshSerial)
		}
	})

	// The per-request rejection: the handshake succeeds and the server refuses
	// the identity with a 401, which is the shed server's actual mtls posture
	// (internal/api re-validates the peer certificate on every request).
	t.Run("401 on a completed handshake", func(t *testing.T) {
		var denied atomic.Value
		denied.Store("")
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.TLS.PeerCertificates) > 0 &&
				r.TLS.PeerCertificates[0].SerialNumber.Text(16) == denied.Load().(string) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(config.APIError{
				Error: config.APIErrorDetail{Code: "INVALID_PORT", Message: "port must be 1-65535"}})
		})
		srv, ca, pin, lastSerial := mtlsConnectServer(t, handler)

		stale, staleSerial := issueTunnelCert(t, ca, "tunnel-key", 24*time.Hour)
		fresh, freshSerial := issueTunnelCert(t, ca, "tunnel-key", 24*time.Hour)
		denied.Store(staleSerial)

		var mints int32
		src := clienttoken.New(
			clienttoken.MTLSCredential(stale, time.Now().Add(24*time.Hour)),
			func() (clienttoken.Credential, error) {
				atomic.AddInt32(&mints, 1)
				return clienttoken.MTLSCredential(fresh, time.Now().Add(24*time.Hour)), nil
			})
		client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Probe(ctx, "myshed"); err != nil {
			t.Fatalf("Probe should have re-enrolled after the 401: %v", err)
		}
		if got := atomic.LoadInt32(&mints); got != 1 {
			t.Errorf("mints = %d, want exactly 1", got)
		}
		if got := lastSerial.Load().(string); got != freshSerial {
			t.Errorf("the retried probe presented %s, want the re-minted %s", got, freshSerial)
		}
	})

	// A token-mode tunnel gets the same recovery: the trigger is the observed
	// failure, not the mode the client believes it is in.
	t.Run("401 in token mode", func(t *testing.T) {
		var attempts atomic.Int32
		srv, pin := tokenOnlyProbeServer(t, &attempts)

		var mints int32
		src := clienttoken.New(
			clienttoken.TokenCredential("stale", time.Now().Add(24*time.Hour)),
			func() (clienttoken.Credential, error) {
				atomic.AddInt32(&mints, 1)
				return clienttoken.TokenCredential("fresh", time.Now().Add(24*time.Hour)), nil
			})
		client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Probe(ctx, "myshed"); err != nil {
			t.Fatalf("Probe should have re-minted the token and recovered: %v", err)
		}
		if got := atomic.LoadInt32(&mints); got != 1 {
			t.Errorf("mints = %d, want exactly 1", got)
		}
	})

	// A STATIC source cannot re-mint, so the behavior must be unchanged: one
	// attempt, and an error that names the credential rather than a network
	// fault. Retrying a credential that cannot be replaced would only double the
	// startup latency of a tunnel that is going to fail anyway.
	t.Run("a static credential does not retry", func(t *testing.T) {
		var attempts atomic.Int32
		srv, pin := tokenOnlyProbeServer(t, &attempts)

		src := clienttoken.Static("stale")
		client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := client.Probe(ctx, "myshed")
		if err == nil {
			t.Fatal("Probe should fail: the token is refused and cannot be re-minted")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error should name the rejection, got: %v", err)
		}
		if got := attempts.Load(); got != 1 {
			t.Errorf("server saw %d probes, want 1 (a static credential must not retry)", got)
		}
	})
}

// tokenOnlyProbeServer is a pinned-TLS Connect listener that never asks for a
// client certificate and accepts only the bearer token "fresh" — the token-mode
// server a tunnel meets after a flip away from mtls. attempts counts probes, so
// a test can assert whether a retry happened at all.
func tokenOnlyProbeServer(t *testing.T, attempts *atomic.Int32) (srv *httptest.Server, pin string) {
	t.Helper()
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(config.APIError{
			Error: config.APIErrorDetail{Code: "INVALID_PORT", Message: "port must be 1-65535"}})
	}))
	t.Cleanup(srv.Close)
	return srv, servertls.Fingerprint(srv.Certificate().Raw)
}
