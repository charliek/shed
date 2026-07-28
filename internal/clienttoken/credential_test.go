package clienttoken

import (
	"crypto/tls"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// certFor returns a distinguishable stand-in certificate. The Source never
// inspects a certificate's contents — it hands the pointer to the TLS stack —
// so a marker in Certificate[0] is enough to prove WHICH one is current.
func certFor(marker string) *tls.Certificate {
	return &tls.Certificate{Certificate: [][]byte{[]byte(marker)}}
}

func certMarker(c *tls.Certificate) string {
	if c == nil || len(c.Certificate) == 0 {
		return ""
	}
	return string(c.Certificate[0])
}

// TestCredentialAccessorsAreModeGated is the invariant the whole two-state
// design rests on: a credential exposes ONLY the material its mode names.
// A leaked bearer token in mtls state would be a live secret sent to an
// endpoint that ignores it; a certificate offered in token state would be
// presented to a listener that never asked.
func TestCredentialAccessorsAreModeGated(t *testing.T) {
	cert := certFor("c1")
	tests := []struct {
		name      string
		cred      Credential
		wantToken string
		wantCert  string
	}{
		{"token credential exposes its token", TokenCredential("tok", time.Now()), "tok", ""},
		{"mtls credential exposes its certificate", MTLSCredential(cert, time.Now()), "", "c1"},
		{"zero credential (open server) exposes nothing", Credential{}, "", ""},
		{
			"a token smuggled into an mtls credential is not exposed",
			Credential{Mode: ModeMTLS, Token: "leak", Cert: cert},
			"", "c1",
		},
		{
			"a certificate smuggled into a token credential is not exposed",
			Credential{Mode: ModeToken, Token: "tok", Cert: cert},
			"tok", "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cred.BearerToken(); got != tt.wantToken {
				t.Errorf("BearerToken = %q, want %q", got, tt.wantToken)
			}
			if got := certMarker(tt.cred.ClientCertificate()); got != tt.wantCert {
				t.Errorf("ClientCertificate = %q, want %q", got, tt.wantCert)
			}
		})
	}
}

// TestSourceMTLSStateRotation: an mtls Source re-mints like a token one, and
// ClientCertificate reflects the new certificate immediately afterwards.
func TestSourceMTLSStateRotation(t *testing.T) {
	var mints int32
	s := New(MTLSCredential(certFor("old"), time.Now().Add(time.Hour)),
		func() (Credential, error) {
			atomic.AddInt32(&mints, 1)
			return MTLSCredential(certFor("new"), time.Now().Add(24*time.Hour)), nil
		})

	if got := certMarker(s.ClientCertificate()); got != "old" {
		t.Fatalf("initial certificate = %q, want old", got)
	}
	if s.Token() != "" {
		t.Error("an mtls Source must never yield a bearer token")
	}

	s.EnsureFresh() // within the 2h window ⇒ proactive re-mint
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Fatalf("mints = %d, want 1", got)
	}
	if got := certMarker(s.ClientCertificate()); got != "new" {
		t.Errorf("certificate after rotation = %q, want new", got)
	}
	if s.Mode() != ModeMTLS {
		t.Errorf("mode = %q, want %q", s.Mode(), ModeMTLS)
	}
}

// TestSourceModeFlip drives the migration in BOTH directions through the only
// mechanism that performs it: a refresh whose returned Credential names a
// different mode. Nothing about the Source is reconfigured — the server's answer
// alone moves it.
func TestSourceModeFlip(t *testing.T) {
	t.Run("token to mtls", func(t *testing.T) {
		s := New(TokenCredential("tok", time.Now().Add(time.Hour)), func() (Credential, error) {
			return MTLSCredential(certFor("issued"), time.Now().Add(24*time.Hour)), nil
		})
		if _, err := s.Refresh(gen(s)); err != nil {
			t.Fatal(err)
		}
		if s.Mode() != ModeMTLS {
			t.Errorf("mode = %q, want mtls", s.Mode())
		}
		if s.Token() != "" {
			t.Errorf("the stale bearer token survived the flip to mtls: %q", s.Token())
		}
		if got := certMarker(s.ClientCertificate()); got != "issued" {
			t.Errorf("certificate = %q, want issued", got)
		}
	})

	t.Run("mtls to token", func(t *testing.T) {
		s := New(MTLSCredential(certFor("old"), time.Now().Add(time.Hour)),
			func() (Credential, error) {
				return TokenCredential("tok", time.Now().Add(24*time.Hour)), nil
			})
		if _, err := s.Refresh(gen(s)); err != nil {
			t.Fatal(err)
		}
		if s.Mode() != ModeToken {
			t.Errorf("mode = %q, want token", s.Mode())
		}
		if s.ClientCertificate() != nil {
			t.Error("the stale certificate survived the flip back to token mode")
		}
		if s.Token() != "tok" {
			t.Errorf("token = %q, want tok", s.Token())
		}
	})
}

// TestMTLSRefreshConcurrentCoalesces is the singleflight guarantee in mtls
// state. It matters more here than for tokens: every mint is an SSH exchange
// that generates a keypair and signs a CSR, and N port-tunnels rejecting at
// once must not become N enrollments (each of which would supersede the last,
// leaving most of them wasted).
func TestMTLSRefreshConcurrentCoalesces(t *testing.T) {
	var mints int32
	release := make(chan struct{})
	s := New(MTLSCredential(certFor("old"), time.Now().Add(time.Hour)),
		func() (Credential, error) {
			n := atomic.AddInt32(&mints, 1)
			<-release // hold the mint open so concurrent callers pile onto singleflight
			return MTLSCredential(certFor("new"+strconv.Itoa(int(n))), time.Now().Add(24*time.Hour)), nil
		})
	gen0 := gen(s)

	const n = 16
	var wg sync.WaitGroup
	got := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cred, err := s.Refresh(gen0)
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			got[i] = certMarker(cred.ClientCertificate())
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if m := atomic.LoadInt32(&mints); m != 1 {
		t.Errorf("mints = %d across %d concurrent auth failures, want 1 (coalesced)", m, n)
	}
	for i, marker := range got {
		if marker != "new1" {
			t.Errorf("goroutine %d got certificate %q, want new1", i, marker)
		}
	}
}

// TestGenerationSurvivesModeFlip: the generation counter is what makes the
// stale-caller check work across a mode change, where the old
// compare-the-token-string identity could not (there is no token to compare
// once the credential is a certificate, and vice versa).
func TestGenerationSurvivesModeFlip(t *testing.T) {
	var mints int32
	s := New(TokenCredential("tok", time.Now().Add(time.Hour)), func() (Credential, error) {
		atomic.AddInt32(&mints, 1)
		return MTLSCredential(certFor("issued"), time.Now().Add(24*time.Hour)), nil
	})
	gen0 := gen(s)

	if _, err := s.Refresh(gen0); err != nil {
		t.Fatal(err)
	}
	// A second request that also failed at generation 0 (it was in flight during
	// the flip) must observe the new credential, not mint a second one.
	cred, err := s.Refresh(gen0)
	if err != nil {
		t.Fatal(err)
	}
	if certMarker(cred.ClientCertificate()) != "issued" {
		t.Errorf("stale-generation caller got %v, want the issued certificate", cred)
	}
	if m := atomic.LoadInt32(&mints); m != 1 {
		t.Errorf("mints = %d, want 1 (the stale caller must not re-mint across a flip)", m)
	}
}

// TestClientCertificateIsSafeUnderConcurrentRotation exercises the exact access
// pattern tls.Config.GetClientCertificate creates: many goroutines reading the
// current certificate while a rotation replaces it. Assertion is -race plus
// "never observes a torn/absent credential".
func TestClientCertificateIsSafeUnderConcurrentRotation(t *testing.T) {
	s := New(MTLSCredential(certFor("gen0"), time.Now().Add(time.Hour)),
		func() (Credential, error) {
			return MTLSCredential(certFor("gen1"), time.Now().Add(24*time.Hour)), nil
		})

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%4 == 0 {
				s.EnsureFresh()
				return
			}
			if got := certMarker(s.ClientCertificate()); got != "gen0" && got != "gen1" {
				t.Errorf("observed unexpected certificate %q mid-rotation", got)
			}
		}(i)
	}
	wg.Wait()
}

// TestRefreshErrorLeavesMTLSCredentialIntact: a failed enrollment must not
// disarm the client. The existing certificate may still have life left, and
// dropping it would turn a transient SSH failure into a hard outage.
func TestRefreshErrorLeavesMTLSCredentialIntact(t *testing.T) {
	boom := errors.New("ssh unreachable")
	s := New(MTLSCredential(certFor("current"), time.Now().Add(time.Hour)),
		func() (Credential, error) { return Credential{}, boom })

	if _, err := s.Refresh(gen(s)); !errors.Is(err, boom) {
		t.Errorf("Refresh err = %v, want the mint error", err)
	}
	if got := certMarker(s.ClientCertificate()); got != "current" {
		t.Errorf("certificate = %q after a failed mint, want current (unchanged)", got)
	}
	// EnsureFresh swallows the error entirely — it is best-effort by contract.
	s.EnsureFresh()
	if got := certMarker(s.ClientCertificate()); got != "current" {
		t.Errorf("certificate = %q after a failed EnsureFresh, want current", got)
	}
}
