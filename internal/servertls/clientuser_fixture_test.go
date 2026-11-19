package servertls

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// The two-language fixture of a certificate's user: Go's ClientCertUser and
// shed-core's certuser.rs (crates/shed-core/src/certuser.rs) read the same
// certificates, and this test holds the claim between them.
//
// It lives under crates/ because the Linux leg of the Rust core runs in a
// container that mounts only crates/. This test writes it under
// UPDATE_CLIENTUSER=1 and only checks it otherwise, so a fixture that no
// longer matches what this file describes fails `make test`:
//
//	UPDATE_CLIENTUSER=1 go test ./internal/servertls/ -run ClientUserFixture
//
// For each fixture expected.json records Go's answer, computed here from the
// committed file (named:<user>, implicit, unbound, malformed, unparseable),
// and Rust's answer, declared in the table below (named:<user>, implicit,
// unbound, error); the Rust test requires its reader to give exactly that.
// The rule this test checks: where Go parses the certificate, Rust's answer
// equals Go's or is an error. Where Go cannot parse it, Rust's answer is
// recorded as it is, and README.md lists the fixture as Go-unparseable.

const clientUserFixtureDir = "../../crates/fixtures/clientuser"

func updatingClientUserFixture() bool { return os.Getenv("UPDATE_CLIENTUSER") == "1" }

// clientUserFixture is one certificate of the set. A built fixture is
// deterministic (fixed keys, serial, times; Ed25519 signatures), so the
// committed file must equal what build returns byte for byte. An issued
// fixture comes from SignClientCSR, whose key, serial and validity differ on
// each issue, so its committed file is checked by what it names, and only
// that: its subject and issuer bytes (RawSubject, RawIssuer) must equal those
// of a certificate issued now for the same user. Nothing else in an issued
// fixture is tracked. A change to what SignClientCSR writes outside those two
// names (validity, public key, signature algorithm, key usage, extended key
// usage, key identifiers, any other extension) leaves the committed file
// passing as long as it still parses, and neither reader verifies its
// signature, so a flipped signature bit passes too. The issued fixtures hold
// the two readers to the names the CA writes; they are not a copy of a
// current certificate.
type clientUserFixture struct {
	name   string
	what   string
	rust   string
	build  func() []byte
	issued bool
	user   string // the user an issued fixture is issued to ("" implicit)
}

func (f clientUserFixture) file() string { return f.name + ".pem" }

// --- a small DER writer -----------------------------------------------------

func fxLen(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var b []byte
	for ; n > 0; n >>= 8 {
		b = append([]byte{byte(n)}, b...)
	}
	return append([]byte{0x80 | byte(len(b))}, b...)
}

func fxTLV(tag byte, parts ...[]byte) []byte {
	content := bytes.Join(parts, nil)
	out := append([]byte{tag}, fxLen(len(content))...)
	return append(out, content...)
}

// fxContent is the content of the TLV der, which must be one TLV.
func fxContent(der []byte) []byte {
	var raw asn1.RawValue
	rest, err := asn1.Unmarshal(der, &raw)
	if err != nil || len(rest) != 0 {
		panic(fmt.Sprintf("fixture set-up: not one TLV: %v", err))
	}
	return raw.Bytes
}

var (
	fxOIDCN       = []byte{0x06, 0x03, 0x55, 0x04, 0x03}
	fxOIDOU       = []byte{0x06, 0x03, 0x55, 0x04, 0x0B}
	fxOIDUser     = []byte{0x06, 0x0A, 0x09, 0x92, 0x26, 0x89, 0x93, 0xF2, 0x2C, 0x64, 0x01, 0x01}
	fxOIDNearUser = []byte{0x06, 0x0B, 0x09, 0x92, 0x26, 0x89, 0x93, 0xF2, 0x2C, 0x64, 0x01, 0x01, 0x01}
	fxOIDEd25519  = []byte{0x06, 0x03, 0x2B, 0x65, 0x70}
	fxOIDSAN      = []byte{0x06, 0x03, 0x55, 0x1D, 0x11}
)

const (
	tagUTF8      = 0x0C
	tagNumeric   = 0x12
	tagPrintable = 0x13
	tagT61       = 0x14
	tagIA5       = 0x16
	tagUTCTime   = 0x17
	tagBMP       = 0x1E
)

func fxStr(tag byte, s string) []byte {
	b := []byte(s)
	if tag == tagBMP {
		b = nil
		for _, u := range utf16.Encode([]rune(s)) {
			b = append(b, byte(u>>8), byte(u))
		}
	}
	return fxTLV(tag, b)
}

func fxATV(oid []byte, values ...[]byte) []byte {
	return fxTLV(0x30, append([][]byte{oid}, values...)...)
}

func fxUser(tag byte, v string) []byte { return fxATV(fxOIDUser, fxStr(tag, v)) }

func fxCN() []byte { return fxATV(fxOIDCN, fxStr(tagPrintable, testFingerprint)) }

func fxRDN(atvs ...[]byte) []byte { return fxTLV(0x31, atvs...) }

func fxName(rdns ...[]byte) []byte { return fxTLV(0x30, rdns...) }

// fxKey is a fixed Ed25519 key, so a built certificate is the same bytes on
// every run.
func fxKey(label string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("shed clientuser fixture: " + label))
	return ed25519.NewKeyFromSeed(seed[:])
}

func fxAlg() []byte { return fxTLV(0x30, fxOIDEd25519) }

// fxTBS is a TBSCertificate; a nil member takes its usual value. extensions,
// when set, are the Extension SEQUENCEs written in the [3] extensions field
// (the certificate is v3).
type fxTBS struct {
	noVersion  bool
	serial     []byte
	issuer     []byte
	validity   []byte
	subject    []byte
	extensions [][]byte
}

func (t fxTBS) der() []byte {
	var parts [][]byte
	if !t.noVersion {
		parts = append(parts, fxTLV(0xA0, fxTLV(0x02, []byte{2})))
	}
	serial := t.serial
	if serial == nil {
		serial = fxTLV(0x02, []byte{0x2C, 0x0F, 0x1A, 0x55})
	}
	issuer := t.issuer
	if issuer == nil {
		issuer = fxName(fxRDN(fxATV(fxOIDCN, fxStr(tagPrintable, "shed fixture CA"))))
	}
	validity := t.validity
	if validity == nil {
		validity = fxTLV(0x30, fxStr(tagUTCTime, "260101000000Z"), fxStr(tagUTCTime, "360101000000Z"))
	}
	pub := fxKey("subject").Public().(ed25519.PublicKey)
	spki := fxTLV(0x30, fxAlg(), fxTLV(0x03, append([]byte{0}, pub...)))
	parts = append(parts, serial, fxAlg(), issuer, validity, t.subject, spki)
	if t.extensions != nil {
		parts = append(parts, fxTLV(0xA3, fxTLV(0x30, t.extensions...)))
	}
	return fxTLV(0x30, parts...)
}

// fxExtension is a non-critical Extension: SEQUENCE { extnID, extnValue }.
func fxExtension(oid []byte, value []byte) []byte {
	return fxTLV(0x30, oid, fxTLV(0x04, value))
}

// fxSANDirectoryName is a subjectAltName extension holding one directoryName
// ([4] EXPLICIT Name) made of the RDNs given.
func fxSANDirectoryName(rdns ...[]byte) []byte {
	return fxExtension(fxOIDSAN, fxTLV(0x30, fxTLV(0xA4, fxName(rdns...))))
}

func fxSignature(tbs []byte) []byte { return ed25519.Sign(fxKey("issuer"), tbs) }

func fxBitString(sig []byte) []byte { return fxTLV(0x03, append([]byte{0}, sig...)) }

// fxSigned is a certificate over tbs, signed by the fixture issuer.
func fxSigned(tbs []byte) []byte {
	return fxTLV(0x30, tbs, fxAlg(), fxBitString(fxSignature(tbs)))
}

func fxCert(t fxTBS) []byte { return fxSigned(t.der()) }

// fxWithSubject is a certificate whose subject is the RDNs given.
func fxWithSubject(rdns ...[]byte) func() []byte {
	return func() []byte { return fxCert(fxTBS{subject: fxName(rdns...)}) }
}

// fxPatCert is the usual well-formed certificate for pat that the
// byte-level fixtures damage.
func fxPatCert() []byte { return fxCert(fxTBS{subject: fxPatSubject()}) }

func fxPatSubject() []byte { return fxName(fxRDN(fxCN()), fxRDN(fxUser(tagPrintable, "pat"))) }

const fxLongestName = "a" + "z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-" + "zz"

// clientUserFixtures is the set, in the order expected.json lists it.
func clientUserFixtures() []clientUserFixture {
	return []clientUserFixture{
		// What the CA issues.
		{name: "issued-pat", what: "issued by SignClientCSR for pat (PrintableString)", rust: "named:pat", issued: true, user: "pat"},
		{name: "issued-implicit", what: "issued by SignClientCSR for the implicit user (UTF8String !implicit)", rust: "implicit", issued: true, user: ""},

		// Readings.
		{name: "no-user", what: "a subject with a CN and no userId (a shed-server older than 0.10.0)", rust: "unbound",
			build: fxWithSubject(fxRDN(fxCN()))},
		{name: "empty-subject", what: "an empty subject", rust: "unbound",
			build: fxWithSubject()},
		{name: "utf8-pat", what: "pat as a UTF8String in its own RDN", rust: "named:pat",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagUTF8, "pat")))},
		{name: "user-before-cn", what: "the userId RDN before the CN", rust: "named:pat",
			build: fxWithSubject(fxRDN(fxUser(tagPrintable, "pat")), fxRDN(fxCN()))},
		{name: "multi-valued-rdn", what: "pat in one multi-valued RDN beside the CN", rust: "named:pat",
			build: fxWithSubject(fxRDN(fxCN(), fxUser(tagPrintable, "pat")))},
		{name: "issuer-only", what: "a userId (admin) in the issuer only, none in the subject", rust: "unbound",
			build: func() []byte {
				return fxCert(fxTBS{
					issuer:  fxName(fxRDN(fxATV(fxOIDCN, fxStr(tagPrintable, "shed fixture CA"))), fxRDN(fxUser(tagPrintable, "admin"))),
					subject: fxName(fxRDN(fxCN())),
				})
			}},
		{name: "issuer-and-subject", what: "admin in the issuer and pat in the subject", rust: "named:pat",
			build: func() []byte {
				return fxCert(fxTBS{
					issuer:  fxName(fxRDN(fxUser(tagPrintable, "admin"))),
					subject: fxPatSubject(),
				})
			}},
		{name: "extension-only", what: "a userId (admin) only in a subjectAltName directoryName extension, none in the subject", rust: "unbound",
			build: func() []byte {
				return fxCert(fxTBS{
					subject:    fxName(fxRDN(fxCN())),
					extensions: [][]byte{fxSANDirectoryName(fxRDN(fxUser(tagPrintable, "admin")))},
				})
			}},
		{name: "extension-oid-only", what: "an extension whose extnID is the userId OID (value admin), none in the subject", rust: "unbound",
			build: func() []byte {
				return fxCert(fxTBS{
					subject:    fxName(fxRDN(fxCN())),
					extensions: [][]byte{fxExtension(fxOIDUser, fxStr(tagPrintable, "admin"))},
				})
			}},
		{name: "subject-and-extension", what: "pat in the subject and admin in a subjectAltName directoryName", rust: "named:pat",
			build: func() []byte {
				return fxCert(fxTBS{
					subject:    fxPatSubject(),
					extensions: [][]byte{fxSANDirectoryName(fxRDN(fxUser(tagPrintable, "admin")))},
				})
			}},
		{name: "v1-no-version", what: "a v1 certificate: no [0] version", rust: "named:pat",
			build: func() []byte { return fxCert(fxTBS{noVersion: true, subject: fxPatSubject()}) }},
		{name: "near-oid", what: "an attribute whose OID extends the userId OID by one arc", rust: "unbound",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxATV(fxOIDNearUser, fxStr(tagPrintable, "pat"))))},
		{name: "longest-name", what: "a 63-character name, the longest", rust: "named:" + fxLongestName,
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagPrintable, fxLongestName)))},
		{name: "third-rdn", what: "pat in the third RDN, after an OU", rust: "named:pat",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxATV(fxOIDOU, fxStr(tagPrintable, "control"))), fxRDN(fxUser(tagPrintable, "pat")))},

		// Values that are not a user.
		{name: "too-long", what: "a 64-character name", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagPrintable, fxLongestName+"a")))},
		{name: "upper-case", what: "the value Pat", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagPrintable, "Pat")))},
		{name: "trailing-newline", what: "pat followed by a newline, as a UTF8String", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagUTF8, "pat\n")))},
		{name: "empty-value", what: "an empty UTF8String", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagUTF8, "")))},
		{name: "other-marker", what: "the UTF8String !unresolved", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagUTF8, "!unresolved")))},
		{name: "implicit-printable", what: "!implicit as a PrintableString (! is not printable)", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagPrintable, ImplicitUserValue)))},

		// String types the CA never writes.
		{name: "ia5-pat", what: "pat as an IA5String", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagIA5, "pat")))},
		{name: "t61-pat", what: "pat as a T61String", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagT61, "pat")))},
		{name: "bmp-pat", what: "pat as a BMPString", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagBMP, "pat")))},
		{name: "numeric-value", what: "123 as a NumericString", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagNumeric, "123")))},
		{name: "implicit-ia5", what: "!implicit as an IA5String", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagIA5, ImplicitUserValue)))},
		{name: "integer-value", what: "an INTEGER value", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxATV(fxOIDUser, fxTLV(0x02, []byte{7}))))},
		{name: "octet-string-value", what: "an OCTET STRING value", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxATV(fxOIDUser, fxTLV(0x04, []byte("pat")))))},
		{name: "high-tag-value", what: "a value with a multi-byte tag", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxATV(fxOIDUser, []byte{0x1F, 0x22, 0x03, 'p', 'a', 't'})))},

		// More than one user.
		{name: "two-rdns", what: "pat and admin, each in its own RDN", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagPrintable, "pat")), fxRDN(fxUser(tagPrintable, "admin")))},
		{name: "two-in-one-rdn", what: "pat and admin in one multi-valued RDN with the CN", rust: "error",
			build: fxWithSubject(fxRDN(fxCN(), fxUser(tagPrintable, "pat"), fxUser(tagPrintable, "admin")))},
		{name: "same-user-twice", what: "pat twice, in two RDNs and two encodings", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagPrintable, "pat")), fxRDN(fxUser(tagUTF8, "pat")))},
		{name: "named-and-implicit", what: "!implicit and pat", rust: "error",
			build: fxWithSubject(fxRDN(fxUser(tagUTF8, ImplicitUserValue)), fxRDN(fxUser(tagPrintable, "pat")))},

		// Malformed subject attributes.
		{name: "attribute-two-values", what: "a userId attribute holding two values", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxATV(fxOIDUser, fxStr(tagPrintable, "pat"), fxStr(tagPrintable, "pat"))))},
		{name: "attribute-no-value", what: "a CN attribute with no value, beside pat", rust: "error",
			build: fxWithSubject(fxRDN(fxATV(fxOIDCN)), fxRDN(fxUser(tagPrintable, "pat")))},
		{name: "cn-not-printable", what: "a CN PrintableString holding @, beside pat", rust: "named:pat",
			build: fxWithSubject(fxRDN(fxATV(fxOIDCN, fxStr(tagPrintable, "pat@laptop"))), fxRDN(fxUser(tagPrintable, "pat")))},
		{name: "rdn-stray-element", what: "an RDN holding pat and then a NULL", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxUser(tagPrintable, "pat"), fxTLV(0x05)))},
		{name: "empty-rdn", what: "an empty RDN before pat", rust: "error",
			build: fxWithSubject(fxRDN(), fxRDN(fxUser(tagPrintable, "pat")))},
		{name: "nonminimal-value-length", what: "pat's length in long form (81 03)", rust: "error",
			build: fxWithSubject(fxRDN(fxCN()), fxRDN(fxATV(fxOIDUser, []byte{tagPrintable, 0x81, 0x03, 'p', 'a', 't'})))},

		// Damage to the certificate's frame.
		{name: "nonminimal-length", what: "the certificate's length with a leading zero octet", rust: "error",
			build: func() []byte {
				c := fxContent(fxPatCert())
				return append([]byte{0x30, 0x83, 0x00, byte(len(c) >> 8), byte(len(c))}, c...)
			}},
		{name: "indefinite-length", what: "the certificate with an indefinite length", rust: "error",
			build: func() []byte {
				return append(append([]byte{0x30, 0x80}, fxContent(fxPatCert())...), 0x00, 0x00)
			}},
		{name: "trailing-bytes", what: "two bytes after the certificate", rust: "error",
			build: func() []byte { return append(fxPatCert(), 0x00, 0x00) }},
		{name: "fourth-element", what: "a NULL after the signature, inside the certificate", rust: "error",
			build: func() []byte {
				tbs := fxTBS{subject: fxPatSubject()}.der()
				return fxTLV(0x30, tbs, fxAlg(), fxBitString(fxSignature(tbs)), fxTLV(0x05))
			}},

		// Damage to what the reader skips.
		{name: "damaged-validity", what: "a notBefore that is not a time", rust: "named:pat",
			build: func() []byte {
				return fxCert(fxTBS{
					validity: fxTLV(0x30, fxStr(tagUTCTime, "26XX01000000Z"), fxStr(tagUTCTime, "360101000000Z")),
					subject:  fxPatSubject(),
				})
			}},
		{name: "damaged-serial", what: "an empty serial INTEGER", rust: "named:pat",
			build: func() []byte { return fxCert(fxTBS{serial: fxTLV(0x02), subject: fxPatSubject()}) }},
		{name: "damaged-signature", what: "a signature BIT STRING with 8 unused bits", rust: "named:pat",
			build: func() []byte {
				tbs := fxTBS{subject: fxPatSubject()}.der()
				return fxTLV(0x30, tbs, fxAlg(), fxTLV(0x03, append([]byte{8}, fxSignature(tbs)...)))
			}},
		{name: "wrong-signature", what: "a well-formed signature that does not verify", rust: "named:pat",
			build: func() []byte {
				tbs := fxTBS{subject: fxPatSubject()}.der()
				sig := fxSignature(tbs)
				sig[0] ^= 0xFF
				return fxTLV(0x30, tbs, fxAlg(), fxBitString(sig))
			}},
	}
}

// --- the answers --------------------------------------------------------------

// goClientUserAnswer is what Go reads from a fixture file.
func goClientUserAnswer(pemBytes []byte) (string, error) {
	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return "", errors.New("not one PEM CERTIFICATE block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "unparseable", nil
	}
	user, bound, err := ClientCertUser(leaf)
	switch {
	case errors.Is(err, ErrClientUserMalformed):
		return "malformed", nil
	case err != nil:
		return "", err
	case !bound:
		return "unbound", nil
	case user == "":
		return "implicit", nil
	default:
		return "named:" + user, nil
	}
}

// clientUserRule is the claim between the two readers: where Go parses a
// certificate, Rust's answer equals Go's or is an error. Rust never answers
// "malformed": a certificate Go reads as malformed is an error in Rust.
func clientUserRule(goAnswer, rustAnswer string) error {
	if rustAnswer != "error" && rustAnswer != "implicit" && rustAnswer != "unbound" && !strings.HasPrefix(rustAnswer, "named:") {
		return fmt.Errorf("%q is not a Rust answer", rustAnswer)
	}
	if goAnswer == "unparseable" || rustAnswer == "error" || rustAnswer == goAnswer {
		return nil
	}
	return fmt.Errorf("Rust answers %s where Go answers %s", rustAnswer, goAnswer)
}

type clientUserExpected struct {
	Comment  string                    `json:"_comment"`
	Fixtures []clientUserExpectedEntry `json:"fixtures"`
}

type clientUserExpectedEntry struct {
	Name string `json:"name"`
	File string `json:"file"`
	What string `json:"what"`
	Go   string `json:"go"`
	Rust string `json:"rust"`
}

func renderClientUserExpected(fixtures []clientUserFixture, goAnswers map[string]string) []byte {
	doc := clientUserExpected{
		Comment: "Written by internal/servertls/clientuser_fixture_test.go under UPDATE_CLIENTUSER=1 and checked by it otherwise; " +
			"read by crates/shed-core/tests/certuser_fixtures.rs. See README.md.",
	}
	for _, f := range fixtures {
		doc.Fixtures = append(doc.Fixtures, clientUserExpectedEntry{
			Name: f.name, File: f.file(), What: f.what, Go: goAnswers[f.name], Rust: f.rust,
		})
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(out, '\n')
}

func renderClientUserReadme(fixtures []clientUserFixture, goAnswers map[string]string) []byte {
	var b strings.Builder
	b.WriteString(`# Certificate user fixtures (two-language, written by Go)

Client certificates and the user each one names, read by BOTH:

- Go: ` + "`servertls.ClientCertUser`" + ` (` + "`internal/servertls/clientuser.go`" + `), and
- Rust: ` + "`shed_core::certuser::cert_user`" + ` (` + "`crates/shed-core/src/certuser.rs`" + `).

This folder is **written by a Go test**, never by hand:

` + "```bash" + `
UPDATE_CLIENTUSER=1 go test ./internal/servertls/ -run ClientUserFixture
` + "```" + `

Without ` + "`UPDATE_CLIENTUSER=1`" + ` the same test only checks the folder, so a fixture that no
longer matches its description fails ` + "`make test`" + `. A built fixture is deterministic (fixed
Ed25519 keys, serial and times) and must equal what the test builds, byte for byte. The two
` + "`issued-*`" + ` fixtures come from ` + "`SignClientCSR`" + `, whose key, serial and validity differ on each
issue, and an update keeps them while they pass the check.

**What the check of an ` + "`issued-*`" + ` fixture tracks, and what it does not.** It tracks the
subject and issuer bytes only: the committed certificate's subject and issuer must equal,
byte for byte, those of a certificate issued now for the same user. It does **not** track
the rest of the certificate. A change to what ` + "`SignClientCSR`" + ` writes outside those two
names (validity, public key, signature algorithm, key usage, extended key usage, key
identifiers, any other extension) leaves the committed file passing as long as it still
parses, and neither reader verifies the signature, so a changed signature bit passes too.
These fixtures hold the two readers to the names the CA writes; they are not a copy of a
current certificate.

The folder lives under ` + "`crates/`" + ` because the Linux leg of the Rust core
(` + "`make desktop-core-linux`" + `) mounts only ` + "`crates/`" + `.

` + "`expected.json`" + ` records, for each fixture, Go's answer (computed by the test from the file:
` + "`named:<user>`, `implicit`, `unbound`, `malformed`, `unparseable`" + `) and Rust's answer (declared by
the test: ` + "`named:<user>`, `implicit`, `unbound`, `error`" + `). The Rust test
(` + "`crates/shed-core/tests/certuser_fixtures.rs`" + `) requires its reader to give exactly Rust's answer.

**The rule** the Go test checks: where Go parses the certificate, Rust's answer equals Go's or
is ` + "`error`" + `. Rust refuses some certificates Go reads (IA5String, T61String and BMPString values,
which the CA never writes). Where Go cannot parse the certificate, Rust's answer is recorded as
it is: Rust's strict reader refuses the damage it sees (a non-minimal or indefinite length,
trailing bytes, a broken structure on its path), but it does not validate what it skips
(validity, serial, signature, other subject attributes' contents), so it may still read a
user. No server accepts such a certificate, since every server parses it in Go. The fixtures
prove the rule on this set, in both languages; they prove nothing more.

## Go-unparseable fixtures

Go's ` + "`x509.ParseCertificate`" + ` refuses these; Rust's answer is recorded as it is.

| Fixture | What | Rust |
|---|---|---|
`)
	for _, f := range fixtures {
		if goAnswers[f.name] == "unparseable" {
			fmt.Fprintf(&b, "| `%s` | %s | `%s` |\n", f.name, strings.ReplaceAll(f.what, "|", "\\|"), f.rust)
		}
	}
	b.WriteString(`
## Every fixture

| Fixture | What | Go | Rust |
|---|---|---|---|
`)
	for _, f := range fixtures {
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | `%s` |\n", f.name, strings.ReplaceAll(f.what, "|", "\\|"), goAnswers[f.name], f.rust)
	}
	return []byte(b.String())
}

// --- issued fixtures ----------------------------------------------------------

// issueClientUserFixture issues a certificate for user from a fresh test CA.
func issueClientUserFixture(t *testing.T, user string) []byte {
	t.Helper()
	ca := newServiceTestCA(t)
	csr, _ := p256CSR(t)
	der, err := ca.SignClientCSR(csr, testFingerprint, user, "control", "cli", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("issue for %q: %v", user, err)
	}
	return der
}

// checkIssuedClientUserFixture: the committed certificate names what one
// issued now for the same user names. It compares RawSubject and RawIssuer
// and nothing else (see clientUserFixture for what that leaves untracked).
func checkIssuedClientUserFixture(t *testing.T, f clientUserFixture, pemBytes []byte) error {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return errors.New("not PEM")
	}
	got, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("does not parse: %v", err)
	}
	now, err := x509.ParseCertificate(issueClientUserFixture(t, f.user))
	if err != nil {
		return err
	}
	if !bytes.Equal(got.RawSubject, now.RawSubject) {
		return fmt.Errorf("its subject %q is not what SignClientCSR writes now (%q)", got.Subject, now.Subject)
	}
	if !bytes.Equal(got.RawIssuer, now.RawIssuer) {
		return fmt.Errorf("its issuer %q is not the CA's now (%q)", got.Issuer, now.Issuer)
	}
	return nil
}

func pemCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// --- the test -----------------------------------------------------------------

func TestClientUserFixture(t *testing.T) {
	if !bytes.Equal(mustMarshalOID(t), fxOIDUser) {
		t.Fatalf("fixture set-up: the userId OID encoding is not ClientUserOID's")
	}
	if len(fxLongestName) != 63 {
		t.Fatalf("fixture set-up: the longest name is %d characters", len(fxLongestName))
	}
	fixtures := clientUserFixtures()
	if updatingClientUserFixture() {
		writeClientUserFixtures(t, fixtures)
	}
	checkClientUserFixtures(t, fixtures)
}

func mustMarshalOID(t *testing.T) []byte {
	t.Helper()
	b, err := asn1.Marshal(ClientUserOID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeClientUserFixtures(t *testing.T, fixtures []clientUserFixture) {
	t.Helper()
	if err := os.MkdirAll(clientUserFixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{"expected.json": true, "README.md": true}
	goAnswers := map[string]string{}
	for _, f := range fixtures {
		keep[f.file()] = true
		path := filepath.Join(clientUserFixtureDir, f.file())
		var data []byte
		if f.issued {
			if old, err := os.ReadFile(path); err == nil && checkIssuedClientUserFixture(t, f, old) == nil {
				data = old // still what the CA issues: keep it, so an update is idempotent
			} else {
				data = pemCertificate(issueClientUserFixture(t, f.user))
			}
		} else {
			data = pemCertificate(f.build())
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		answer, err := goClientUserAnswer(data)
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		goAnswers[f.name] = answer
	}
	entries, err := os.ReadDir(clientUserFixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !keep[e.Name()] && strings.HasSuffix(e.Name(), ".pem") {
			if err := os.Remove(filepath.Join(clientUserFixtureDir, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(clientUserFixtureDir, "expected.json"), renderClientUserExpected(fixtures, goAnswers), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientUserFixtureDir, "README.md"), renderClientUserReadme(fixtures, goAnswers), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("UPDATE_CLIENTUSER: wrote %d fixtures to %s", len(fixtures), clientUserFixtureDir)
}

func checkClientUserFixtures(t *testing.T, fixtures []clientUserFixture) {
	t.Helper()
	const regen = "; regenerate with UPDATE_CLIENTUSER=1 go test ./internal/servertls/ -run ClientUserFixture"

	want := []string{"README.md", "expected.json"}
	seen := map[string]bool{}
	for _, f := range fixtures {
		if seen[f.name] {
			t.Fatalf("fixture set-up: %s listed twice", f.name)
		}
		seen[f.name] = true
		want = append(want, f.file())
	}
	entries, err := os.ReadDir(clientUserFixtureDir)
	if err != nil {
		t.Fatalf("read %s: %v%s", clientUserFixtureDir, err, regen)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s holds %v, want %v%s", clientUserFixtureDir, got, want, regen)
	}

	goAnswers := map[string]string{}
	for _, f := range fixtures {
		data, err := os.ReadFile(filepath.Join(clientUserFixtureDir, f.file()))
		if err != nil {
			t.Errorf("%s: %v%s", f.name, err, regen)
			continue
		}
		if f.issued {
			if err := checkIssuedClientUserFixture(t, f, data); err != nil {
				t.Errorf("%s is stale: %v%s", f.name, err, regen)
			}
		} else if !bytes.Equal(data, pemCertificate(f.build())) {
			t.Errorf("%s is stale: it is not the certificate this test builds%s", f.name, regen)
		}
		answer, err := goClientUserAnswer(data)
		if err != nil {
			t.Errorf("%s: %v", f.name, err)
			continue
		}
		goAnswers[f.name] = answer
		if err := clientUserRule(answer, f.rust); err != nil {
			t.Errorf("%s breaks the rule: %v", f.name, err)
		}
	}

	if data, err := os.ReadFile(filepath.Join(clientUserFixtureDir, "expected.json")); err != nil {
		t.Errorf("expected.json: %v%s", err, regen)
	} else if !bytes.Equal(data, renderClientUserExpected(fixtures, goAnswers)) {
		t.Errorf("expected.json is stale%s", regen)
	}
	if data, err := os.ReadFile(filepath.Join(clientUserFixtureDir, "README.md")); err != nil {
		t.Errorf("README.md: %v%s", err, regen)
	} else if !bytes.Equal(data, renderClientUserReadme(fixtures, goAnswers)) {
		t.Errorf("README.md is stale%s", regen)
	}
}

// TestClientUserRule: the rule refuses a Rust answer that differs from a
// Go reading, and lets an error or a Go-unparseable certificate through.
func TestClientUserRule(t *testing.T) {
	for _, tc := range []struct {
		goAnswer, rust string
		ok             bool
	}{
		{"named:pat", "named:pat", true},
		{"named:pat", "error", true},
		{"implicit", "implicit", true},
		{"unbound", "unbound", true},
		{"malformed", "error", true},
		{"unparseable", "named:pat", true},
		{"unparseable", "unbound", true},
		{"named:pat", "named:admin", false},
		{"named:pat", "unbound", false},
		{"unbound", "named:pat", false},
		{"implicit", "named:pat", false},
		{"malformed", "unbound", false},
		{"malformed", "named:pat", false},
		{"unbound", "malformed", false},
		{"unparseable", "unparseable", false},
	} {
		if err := clientUserRule(tc.goAnswer, tc.rust); (err == nil) != tc.ok {
			t.Errorf("rule(go %s, rust %s) = %v, want ok %v", tc.goAnswer, tc.rust, err, tc.ok)
		}
	}
}
