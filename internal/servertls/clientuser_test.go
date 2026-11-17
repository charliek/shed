package servertls

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"testing"
	"time"
)

// TestClientCertUser: an issued certificate names the user it was issued to
// in one userId attribute (the implicit user as ImplicitUserValue), which
// ClientCertUser reads back; a certificate without one (issued before) is
// unbound; a duplicated or malformed attribute is refused; and issuance
// refuses a user name that is not one.
func TestClientCertUser(t *testing.T) {
	ca := newServiceTestCA(t)
	issue := func(user string) *x509.Certificate {
		t.Helper()
		csr, _ := p256CSR(t)
		der, err := ca.SignClientCSR(csr, testFingerprint, user, "control", "cli", time.Hour)
		if err != nil {
			t.Fatalf("issue for %q: %v", user, err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return leaf
	}
	for _, user := range []string{"alice", ""} {
		leaf := issue(user)
		got, bound, err := ClientCertUser(leaf)
		if err != nil || !bound || got != user {
			t.Errorf("issued to %q: read back %q bound %v err %v", user, got, bound, err)
		}
		if leaf.Subject.CommonName != testFingerprint || len(leaf.Subject.OrganizationalUnit) != 1 {
			t.Errorf("issued to %q: subject %v", user, leaf.Subject)
		}
	}
	if _, bound, err := ClientCertUser(&x509.Certificate{Subject: pkix.Name{CommonName: testFingerprint}}); bound || err != nil {
		t.Errorf("a certificate with no user attribute: bound %v err %v, want unbound", bound, err)
	}
	two := &x509.Certificate{Subject: pkix.Name{Names: []pkix.AttributeTypeAndValue{
		{Type: ClientUserOID, Value: "alice"}, {Type: ClientUserOID, Value: "bob"},
	}}}
	if _, _, err := ClientCertUser(two); !errors.Is(err, ErrClientUserMalformed) {
		t.Errorf("two user attributes: %v, want malformed", err)
	}
	for _, v := range []any{"Alice", "", "!unresolved", 7} {
		odd := &x509.Certificate{Subject: pkix.Name{Names: []pkix.AttributeTypeAndValue{{Type: ClientUserOID, Value: v}}}}
		if _, _, err := ClientCertUser(odd); !errors.Is(err, ErrClientUserMalformed) {
			t.Errorf("user attribute %v: %v, want malformed", v, err)
		}
	}
	csr, _ := p256CSR(t)
	for _, bad := range []string{"Alice", ImplicitUserValue, "a b"} {
		if _, err := ca.SignClientCSR(csr, testFingerprint, bad, "control", "cli", time.Hour); !errors.Is(err, ErrCAInvalidUser) {
			t.Errorf("issuing to %q: %v, want ErrCAInvalidUser", bad, err)
		}
	}
}
