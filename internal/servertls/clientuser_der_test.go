package servertls

import (
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// The userId attribute as a real certificate carries it, built at the DER
// level: in any RDN of the subject, in each string encoding X.509 allows,
// duplicated, and with names at the edges of the user-name grammar. And a CSR
// that asks for its own userId (or SANs and extensions) has no say in the
// certificate's user.

// derString is an ASN.1 string of the given universal tag.
func derString(tag int, s string) asn1.RawValue {
	b := []byte(s)
	if tag == asn1.TagBMPString {
		b = nil
		for _, u := range utf16.Encode([]rune(s)) {
			b = append(b, byte(u>>8), byte(u))
		}
	}
	return asn1.RawValue{Class: asn1.ClassUniversal, Tag: tag, Bytes: b}
}

func cnATV() pkix.AttributeTypeAndValue {
	return pkix.AttributeTypeAndValue{Type: asn1.ObjectIdentifier{2, 5, 4, 3}, Value: testFingerprint}
}

func uidATV(v any) pkix.AttributeTypeAndValue {
	return pkix.AttributeTypeAndValue{Type: ClientUserOID, Value: v}
}

// certWithSubject issues a certificate whose subject is exactly rdns (DER),
// and parses it back. A subject the parser refuses returns the parse error.
func certWithSubject(t *testing.T, rdns pkix.RDNSequence) (*x509.Certificate, error) {
	t.Helper()
	raw, err := asn1.Marshal(rdns)
	if err != nil {
		t.Fatalf("marshal subject: %v", err)
	}
	key := mustECDSAKey(t, elliptic.P256())
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		RawSubject:   raw,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return x509.ParseCertificate(der)
}

func TestClientCertUserDER(t *testing.T) {
	maxName := "a" + strings.Repeat("z9-", 20) + "zz"
	if len(maxName) != 63 {
		t.Fatalf("test set-up: maxName is %d bytes", len(maxName))
	}
	utf8 := func(s string) asn1.RawValue { return derString(asn1.TagUTF8String, s) }

	for _, tc := range []struct {
		name     string
		rdns     pkix.RDNSequence
		want     string
		wantBad  bool // ErrClientUserMalformed
		mayFail  bool // the parser may refuse the certificate outright
		wantNone bool // no attribute: unbound
	}{
		{name: "own RDN after the CN", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("alice"))}}, want: "alice"},
		{name: "own RDN before the CN", rdns: pkix.RDNSequence{{uidATV(utf8("alice"))}, {cnATV()}}, want: "alice"},
		{name: "multi-valued RDN with the CN", rdns: pkix.RDNSequence{{cnATV(), uidATV(utf8("alice"))}}, want: "alice"},
		{name: "third RDN", rdns: pkix.RDNSequence{{cnATV()}, {{Type: asn1.ObjectIdentifier{2, 5, 4, 11}, Value: "control"}}, {uidATV(utf8("alice"))}}, want: "alice"},
		{name: "PrintableString", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(derString(asn1.TagPrintableString, "alice"))}}, want: "alice"},
		{name: "IA5String", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(derString(asn1.TagIA5String, "alice"))}}, want: "alice"},
		{name: "T61String", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(derString(asn1.TagT61String, "alice"))}}, want: "alice"},
		{name: "BMPString", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(derString(asn1.TagBMPString, "alice"))}}, want: "alice"},
		{name: "implicit, IA5String", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(derString(asn1.TagIA5String, ImplicitUserValue))}}, want: ""},
		{name: "implicit, BMPString", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(derString(asn1.TagBMPString, ImplicitUserValue))}}, want: ""},
		{name: "longest name", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8(maxName))}}, want: maxName},
		{name: "one letter", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("a"))}}, want: "a"},
		{name: "trailing hyphen", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("a-"))}}, want: "a-"},
		{name: "consecutive hyphens", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("a--b"))}}, want: "a--b"},
		{name: "the name implicit", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("implicit"))}}, want: "implicit"},
		{name: "no attribute", rdns: pkix.RDNSequence{{cnATV()}}, wantNone: true},
		{name: "identical duplicate, two RDNs", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("alice"))}, {uidATV(utf8("alice"))}}, wantBad: true},
		{name: "identical duplicate, one RDN", rdns: pkix.RDNSequence{{cnATV(), uidATV(utf8("alice")), uidATV(utf8("alice"))}}, wantBad: true},
		{name: "identical duplicate, two encodings", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("alice"))}, {uidATV(derString(asn1.TagPrintableString, "alice"))}}, wantBad: true},
		{name: "different duplicate, two RDNs", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("alice"))}, {uidATV(utf8("bob"))}}, wantBad: true},
		{name: "different duplicate, one RDN", rdns: pkix.RDNSequence{{cnATV(), uidATV(utf8("bob")), uidATV(utf8("alice"))}}, wantBad: true},
		{name: "named and implicit", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8(ImplicitUserValue))}, {uidATV(utf8("alice"))}}, wantBad: true},
		{name: "too long", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8(maxName + "a"))}}, wantBad: true},
		{name: "leading hyphen", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("-a"))}}, wantBad: true},
		{name: "leading digit", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("9a"))}}, wantBad: true},
		{name: "upper case", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("Alice"))}}, wantBad: true},
		{name: "trailing space", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("alice "))}}, wantBad: true},
		{name: "trailing newline", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("alice\n"))}}, wantBad: true},
		{name: "non-ASCII letter", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("аlice"))}}, wantBad: true},
		{name: "empty", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8(""))}}, wantBad: true},
		{name: "another marker", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(utf8("!unresolved"))}}, wantBad: true},
		{name: "NUL inside, BMPString", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(derString(asn1.TagBMPString, "ali\x00ce"))}}, wantBad: true, mayFail: true},
		{name: "INTEGER value", rdns: pkix.RDNSequence{{cnATV()}, {uidATV(7)}}, wantBad: true, mayFail: true},
		{name: "OCTET STRING value", rdns: pkix.RDNSequence{{cnATV()}, {uidATV([]byte("alice"))}}, wantBad: true, mayFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf, err := certWithSubject(t, tc.rdns)
			if err != nil {
				if tc.mayFail {
					return // refused before anyone reads it: never a user
				}
				t.Fatalf("parse: %v", err)
			}
			user, bound, err := ClientCertUser(leaf)
			switch {
			case tc.wantBad:
				if !errors.Is(err, ErrClientUserMalformed) {
					t.Errorf("read %q bound %v err %v, want malformed", user, bound, err)
				}
			case tc.wantNone:
				if err != nil || bound {
					t.Errorf("read %q bound %v err %v, want unbound", user, bound, err)
				}
			default:
				if err != nil || !bound || user != tc.want {
					t.Errorf("read %q bound %v err %v, want %q", user, bound, err, tc.want)
				}
			}
		})
	}
}

// TestSignClientCSRIgnoresTheRequestedUser: a CSR whose subject names a user
// of its own (in its own RDN, next to the CN, twice), that requests SANs, and
// that asks for extensions (one of them under the userId OID) is issued a
// certificate naming exactly the user the caller passed, and nothing the CSR
// asked for.
func TestSignClientCSRIgnoresTheRequestedUser(t *testing.T) {
	ca := newServiceTestCA(t)
	mallory := func(v string) pkix.AttributeTypeAndValue { return uidATV(v) }
	evil, _ := url.Parse("spiffe://shed/user/mallory")
	for _, tc := range []struct {
		name string
		tmpl *x509.CertificateRequest
	}{
		{"subject userId", &x509.CertificateRequest{Subject: pkix.Name{CommonName: testFingerprint, ExtraNames: []pkix.AttributeTypeAndValue{mallory("mallory")}}}},
		{"raw subject, userId twice", &x509.CertificateRequest{RawSubject: func() []byte {
			raw, err := asn1.Marshal(pkix.RDNSequence{{cnATV(), mallory("mallory")}, {mallory(ImplicitUserValue)}})
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}()}},
		{"SANs and extensions", &x509.CertificateRequest{
			Subject:        pkix.Name{ExtraNames: []pkix.AttributeTypeAndValue{mallory("mallory")}},
			DNSNames:       []string{"mallory.example"},
			EmailAddresses: []string{"mallory@example.invalid"},
			URIs:           []*url.URL{evil},
			ExtraExtensions: []pkix.Extension{
				{Id: ClientUserOID, Value: []byte{0x0c, 0x07, 'm', 'a', 'l', 'l', 'o', 'r', 'y'}},
				{Id: asn1.ObjectIdentifier{2, 5, 29, 19}, Critical: true, Value: []byte{0x30, 0x03, 0x01, 0x01, 0xff}}, // basicConstraints CA:TRUE
			},
		}},
	} {
		for _, user := range []string{"alice", ""} {
			t.Run(tc.name+"/"+map[string]string{"alice": "alice", "": "implicit"}[user], func(t *testing.T) {
				tc.tmpl.SignatureAlgorithm = x509.ECDSAWithSHA256
				csr := makeCSR(t, tc.tmpl, mustECDSAKey(t, elliptic.P256()))
				der, err := ca.SignClientCSR(csr, testFingerprint, user, "control", "cli", time.Hour)
				if err != nil {
					t.Fatalf("sign: %v", err)
				}
				leaf, err := x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
				got, bound, err := ClientCertUser(leaf)
				if err != nil || !bound || got != user {
					t.Errorf("issued certificate names %q bound %v err %v, want %q", got, bound, err, user)
				}
				if strings.Contains(leaf.Subject.String(), "mallory") || leaf.Subject.CommonName != testFingerprint {
					t.Errorf("issued subject %q carries what the CSR asked for", leaf.Subject)
				}
				if len(leaf.DNSNames)+len(leaf.EmailAddresses)+len(leaf.URIs)+len(leaf.IPAddresses) != 0 || leaf.IsCA {
					t.Errorf("issued certificate took SANs or CA:TRUE from the CSR: %v %v %v CA %v", leaf.DNSNames, leaf.EmailAddresses, leaf.URIs, leaf.IsCA)
				}
				for _, ext := range leaf.Extensions {
					if ext.Id.Equal(ClientUserOID) {
						t.Error("issued certificate carries the userId extension the CSR asked for")
					}
				}
			})
		}
	}
}
