package clienttoken

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"
	"time"
)

// testRefusal is a Refusal, as config.EntryRefusedError is.
type testRefusal struct{ msg string }

func (r *testRefusal) Error() string { return r.msg }
func (r *testRefusal) EntryRefusal() {}

func TestIsRefusal(t *testing.T) {
	r := &testRefusal{msg: "refused"}
	if !IsRefusal(r) || !IsRefusal(fmt.Errorf("wrapped: %w", r)) {
		t.Error("a refusal, bare or wrapped, was not recognized")
	}
	if IsRefusal(errors.New("an ordinary mint failure")) || IsRefusal(nil) {
		t.Error("an ordinary error was taken for a refusal")
	}
}

// TestRefusedSourceIsTerminal: a Source built refused holds nothing, never
// mints, presents no certificate, and reports its refusal from every entry
// point.
func TestRefusedSourceIsTerminal(t *testing.T) {
	r := &testRefusal{msg: "entry refused"}
	s := Refused(r)
	if s.Err() != r {
		t.Fatalf("Err = %v, want the refusal", s.Err())
	}
	if s.Refreshable() {
		t.Error("a refused source claims it can mint")
	}
	if err := s.EnsureFreshErr(); err != r {
		t.Errorf("EnsureFreshErr = %v, want the refusal", err)
	}
	if _, err := s.Refresh(0); err != r {
		t.Errorf("Refresh = %v, want the refusal", err)
	}
	if s.CertificateFor(context.Background()) != nil {
		t.Error("a refused source presented a certificate")
	}
}

// TestRefusalFromAMintEndsTheSource: a refresh whose mint fails with a Refusal
// ends the Source for good; later calls return it without minting again. An
// ordinary mint failure does not.
func TestRefusalFromAMintEndsTheSource(t *testing.T) {
	r := &testRefusal{msg: "another user"}
	var mints atomic.Int32
	var next error
	s := New(Credential{Mode: ModeMTLS}, func() (Credential, error) {
		mints.Add(1)
		return Credential{}, next
	})

	next = errors.New("ssh exited 255")
	if _, err := s.Refresh(0); err == nil || s.Err() != nil {
		t.Fatalf("an ordinary failure: err=%v Err=%v, want a failure that leaves the source usable", err, s.Err())
	}

	next = r
	if _, err := s.Refresh(0); err != r {
		t.Fatalf("Refresh = %v, want the refusal", err)
	}
	if s.Err() != r {
		t.Fatalf("Err = %v, want the refusal", s.Err())
	}
	before := mints.Load()
	if _, err := s.Refresh(0); err != r {
		t.Errorf("a later Refresh = %v, want the refusal", err)
	}
	if err := s.EnsureFreshErr(); err != r {
		t.Errorf("a later EnsureFreshErr = %v, want the refusal", err)
	}
	if got := mints.Load(); got != before {
		t.Errorf("a refused source minted %d more time(s)", got-before)
	}
}

// testInvalidation is a Refusal that also withdraws the held credential, as
// config.EntryRefusedError does when the entry's recorded user changed.
type testInvalidation struct{ testRefusal }

func (*testInvalidation) InvalidatesHeld() bool { return true }

// leafCert is a self-signed client certificate whose leaf expires at notAfter,
// returned with Leaf unset (the parse path) and with Leaf set.
func leafCert(t *testing.T, notAfter time.Time) (bare, withLeaf *tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "held"},
		NotBefore:    notAfter.Add(-48 * time.Hour),
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	bare = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	withLeaf = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	return bare, withLeaf
}

// setClock moves the refusal decisions' clock for the rest of the test.
func setClock(t *testing.T, at time.Time) {
	t.Helper()
	orig := timeNow
	t.Cleanup(func() { timeNow = orig })
	timeNow = func() time.Time { return at }
}

// TestProactiveRefusalKeepsAStillValidCredential: EnsureFresh's refusal ends
// the Source only when it holds nothing it may still present. Whether the held
// certificate may be kept is read from the certificate itself (its leaf's
// NotAfter), not from the cached expiry recorded beside it: a stale cached
// expiry in the past does not end a Source whose certificate is valid, and one
// in the future does not keep a certificate that has expired.
func TestProactiveRefusalKeepsAStillValidCredential(t *testing.T) {
	now := time.Now()
	validBare, validLeaf := leafCert(t, now.Add(time.Hour))
	expiredBare, _ := leafCert(t, now.Add(-time.Minute))
	cases := []struct {
		name    string
		initial Credential
		ends    bool
	}{
		{"near expiry, still valid (leaf parsed)", MTLSCredential(validBare, now.Add(time.Hour)), false},
		{"near expiry, still valid (leaf set)", MTLSCredential(validLeaf, now.Add(time.Hour)), false},
		{"cached expiry stale in the past, certificate still valid", MTLSCredential(validBare, now.Add(-time.Hour)), false},
		{"cached expiry in the window, certificate expired", MTLSCredential(expiredBare, now.Add(time.Hour)), true},
		{"expired", MTLSCredential(expiredBare, now.Add(-time.Minute)), true},
		{"a certificate with no leaf to read", MTLSCredential(&tls.Certificate{}, now.Add(time.Hour)), true},
		{"nothing held", Credential{Mode: ModeMTLS}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &testRefusal{msg: "another user"}
			s := New(tc.initial, func() (Credential, error) { return Credential{}, r })
			if err := s.EnsureFreshErr(); err != r {
				t.Fatalf("EnsureFreshErr = %v, want the refusal", err)
			}
			if ended := s.Err() != nil; ended != tc.ends {
				t.Errorf("source ended = %v, want %v", ended, tc.ends)
			}
		})
	}
}

// TestKeptCredentialIsNeverMintedPast: after a refused proactive refresh kept
// the held certificate, later EnsureFresh calls do not run ssh again (each
// would be refused the same way); the held certificate is used until it is no
// longer valid, and then the Source ends with the remembered refusal — still
// without minting. A reactive Refresh (the server rejected what was kept) ends
// it at once, without minting either.
func TestKeptCredentialIsNeverMintedPast(t *testing.T) {
	setup := func(t *testing.T) (*Source, *atomic.Int32, *testRefusal, time.Time) {
		t.Helper()
		notAfter := time.Now().Add(time.Hour)
		cert, _ := leafCert(t, notAfter)
		r := &testRefusal{msg: "another user"}
		var mints atomic.Int32
		s := New(MTLSCredential(cert, notAfter), func() (Credential, error) {
			mints.Add(1)
			return Credential{}, r
		})
		if err := s.EnsureFreshErr(); err != r || s.Err() != nil {
			t.Fatalf("setup: EnsureFreshErr = %v, Err = %v; want the refusal with the credential kept", err, s.Err())
		}
		return s, &mints, r, notAfter
	}

	t.Run("later proactive calls do not mint", func(t *testing.T) {
		s, mints, r, notAfter := setup(t)
		for i := 0; i < 3; i++ {
			if err := s.EnsureFreshErr(); err != r {
				t.Fatalf("EnsureFreshErr #%d = %v, want the remembered refusal", i+2, err)
			}
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want 1 (a kept credential is never minted past)", got)
		}
		if s.Err() != nil || s.ClientCertificate() == nil {
			t.Fatal("the still-valid certificate was not kept in use")
		}

		setClock(t, notAfter.Add(time.Second))
		if err := s.EnsureFreshErr(); err != r {
			t.Fatalf("EnsureFreshErr past NotAfter = %v, want the refusal", err)
		}
		if s.Err() != r {
			t.Errorf("Err = %v once the kept certificate expired, want the refusal (terminal)", s.Err())
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want 1", got)
		}
	})

	t.Run("the pre-send check alone ends it once the kept certificate expires", func(t *testing.T) {
		// Err is what every request path checks before it sends or dials. It
		// must notice the expiry itself: a client that makes no further
		// EnsureFreshErr call (an existing API client's next request, Ping,
		// Status) must never send the expired kept certificate.
		s, mints, r, notAfter := setup(t)
		if s.Err() != nil {
			t.Fatal("setup: the still-valid certificate was not kept")
		}
		setClock(t, notAfter.Add(time.Second))
		if err := s.Err(); err != r {
			t.Fatalf("Err past NotAfter = %v, want the remembered refusal", err)
		}
		if s.CertificateFor(context.Background()) != nil {
			t.Error("the expired kept certificate is still presented")
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want 1", got)
		}
	})

	t.Run("a reactive refresh ends it without minting", func(t *testing.T) {
		s, mints, r, _ := setup(t)
		_, gen := s.Current()
		if _, err := s.Refresh(gen); err != r {
			t.Fatalf("Refresh = %v, want the refusal", err)
		}
		if s.Err() != r {
			t.Errorf("Err = %v, want the refusal", s.Err())
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want 1", got)
		}
	})
}

// TestRefusalIsRecordedBeforeTheMintReturns: the refusal is recorded inside the
// shared mint, so in the instant after it returns to its caller — before that
// caller does anything — the Source is already ended and a second caller
// cannot start another mint (another ssh, whose result could be installed
// after the first caller ended the Source).
func TestRefusalIsRecordedBeforeTheMintReturns(t *testing.T) {
	r := &testRefusal{msg: "another user"}
	var mints atomic.Int32
	s := New(Credential{Mode: ModeMTLS}, func() (Credential, error) {
		mints.Add(1)
		return Credential{}, r
	})

	var arrived atomic.Bool
	var second error
	orig := afterSharedMint
	t.Cleanup(func() { afterSharedMint = orig })
	afterSharedMint = func() {
		if arrived.CompareAndSwap(false, true) {
			// Another caller arrives in the window, still on the same generation.
			_, second = s.Refresh(0)
		}
	}

	if _, err := s.Refresh(0); err != r {
		t.Fatalf("Refresh = %v, want the refusal", err)
	}
	if second != r {
		t.Errorf("the caller in the window got %v, want the refusal", second)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1 (a caller in the window minted again)", got)
	}
}

// TestHeldInvalidationEndsTheSourceAtOnce: a refusal that withdraws the held
// credential (the entry's recorded user changed under this process) ends the
// Source even when its certificate is still valid and the refresh was
// proactive: the old user's certificate is never sent again.
func TestHeldInvalidationEndsTheSourceAtOnce(t *testing.T) {
	notAfter := time.Now().Add(time.Hour)
	cert, _ := leafCert(t, notAfter)
	r := &testInvalidation{testRefusal{msg: "the entry now records another user"}}
	s := New(MTLSCredential(cert, notAfter), func() (Credential, error) { return Credential{}, r })
	if err := s.EnsureFreshErr(); err != r {
		t.Fatalf("EnsureFreshErr = %v, want the refusal", err)
	}
	if s.Err() != r {
		t.Fatalf("Err = %v, want the refusal: the held certificate is withdrawn", s.Err())
	}
	if s.CertificateFor(context.Background()) != nil {
		t.Error("the withdrawn certificate is still presented")
	}
}

// TestZeroCachedExpiryNeverRefreshesProactively: a zero cached expiry means
// "never proactively re-mint" (a static or legacy credential), so EnsureFresh
// does not mint and so cannot be refused; a refusal can then come only from
// the reactive path, which ends the Source.
func TestZeroCachedExpiryNeverRefreshesProactively(t *testing.T) {
	cert, _ := leafCert(t, time.Now().Add(time.Hour))
	r := &testRefusal{msg: "another user"}
	var mints atomic.Int32
	s := New(MTLSCredential(cert, time.Time{}), func() (Credential, error) {
		mints.Add(1)
		return Credential{}, r
	})
	if err := s.EnsureFreshErr(); err != nil {
		t.Fatalf("EnsureFreshErr = %v, want nil (no proactive mint)", err)
	}
	if got := mints.Load(); got != 0 {
		t.Fatalf("mints = %d, want 0", got)
	}
	_, gen := s.Current()
	if _, err := s.Refresh(gen); err != r {
		t.Fatalf("Refresh = %v, want the refusal", err)
	}
	if s.Err() != r {
		t.Errorf("Err = %v, want the refusal (a reactive refusal ends the Source)", s.Err())
	}
}
