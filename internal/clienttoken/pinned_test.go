package clienttoken

import (
	"context"
	"testing"
	"time"
)

// TestPinnedCredentialRoundTrip: the pin is the per-request identity the whole
// capture-vs-transmit fix rests on, so its basic contract gets its own test.
func TestPinnedCredentialRoundTrip(t *testing.T) {
	cred := TokenCredential("pinned", time.Now().Add(time.Hour))

	got, ok := PinnedCredential(WithPinned(context.Background(), cred))
	if !ok {
		t.Fatal("a pinned context reported no credential")
	}
	if got.Token != "pinned" {
		t.Errorf("pinned token = %q, want pinned", got.Token)
	}

	// An unpinned context must report false rather than a zero credential that a
	// caller could mistake for "no credential at all".
	if _, ok := PinnedCredential(context.Background()); ok {
		t.Error("an unpinned context reported a credential")
	}
	//nolint:staticcheck // a nil context is exactly what this guards against
	if _, ok := PinnedCredential(nil); ok {
		t.Error("a nil context reported a credential")
	}
	//nolint:staticcheck // same
	if WithPinned(nil, cred) == nil {
		t.Error("WithPinned(nil, ...) returned a nil context")
	}
}

// TestCertificateForPrefersThePinOverTheLiveSource is the property the transport
// depends on: once a request has captured its credential, a later re-mint must
// not change what that request's handshake presents.
func TestCertificateForPrefersThePinOverTheLiveSource(t *testing.T) {
	captured := certFor("captured")
	racing := certFor("racing")

	s := New(MTLSCredential(captured, time.Now().Add(time.Hour)), func() (Credential, error) {
		return MTLSCredential(racing, time.Now().Add(time.Hour)), nil
	})
	ctx := WithPinned(context.Background(), mustCurrent(s))

	// The refresh lands after the capture — exactly the window that used to let
	// a request transmit a generation it had not recorded.
	if _, err := s.Refresh(0); err != nil {
		t.Fatal(err)
	}
	if got := certMarker(s.CertificateFor(ctx)); got != "captured" {
		t.Errorf("CertificateFor(pinned) = %q, want captured", got)
	}
	// An unpinned handshake still gets the current credential, which is what
	// keeps a pooled connection re-handshaking outside any request correct.
	if got := certMarker(s.CertificateFor(context.Background())); got != "racing" {
		t.Errorf("CertificateFor(unpinned) = %q, want the current racing credential", got)
	}
}

// TestCertificateForPinnedTokenCredentialPresentsNothing: a pinned credential in
// TOKEN state must present no certificate, even while the live source has since
// flipped to mtls. The mode gate travels with the pin.
func TestCertificateForPinnedTokenCredentialPresentsNothing(t *testing.T) {
	s := New(TokenCredential("t", time.Now().Add(time.Hour)), func() (Credential, error) {
		return MTLSCredential(certFor("flipped"), time.Now().Add(time.Hour)), nil
	})
	ctx := WithPinned(context.Background(), mustCurrent(s))
	if _, err := s.Refresh(0); err != nil {
		t.Fatal(err)
	}
	if got := s.CertificateFor(ctx); got != nil {
		t.Errorf("a pinned token credential presented a certificate: %v", got)
	}
}

func mustCurrent(s *Source) Credential {
	cred, _ := s.Current()
	return cred
}
