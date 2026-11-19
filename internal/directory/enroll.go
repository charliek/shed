package directory

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charliek/shed/internal/config"
)

// Enrolment limits (section 1).
const (
	// MaxCodeBytes is the longest enrolment code.
	MaxCodeBytes = 64
	// MaxServerNameBytes is the longest enrolment name.
	MaxServerNameBytes = 255
)

// clientIDPattern is a client id's form.
var clientIDPattern = regexp.MustCompile(`^srv_[0-9a-f]{32}$`)

// ValidClientID reports whether s has a client id's form.
func ValidClientID(s string) bool { return clientIDPattern.MatchString(s) }

// ValidCode reports whether s can be an enrolment code: 1 to 64 bytes of
// UTF-8.
func ValidCode(s string) bool {
	return len(s) >= 1 && len(s) <= MaxCodeBytes && utf8.ValidString(s)
}

// ValidServerName reports whether s can be the enrolment's name: 1 to 255
// printable ASCII characters.
func ValidServerName(s string) bool {
	if len(s) < 1 || len(s) > MaxServerNameBytes {
		return false
	}
	for _, c := range []byte(s) {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// Enrolment is an enrolled server's identity with the service: what
// enrolment.json holds.
type Enrolment struct {
	ClientID      string
	Issuer        string
	TokenEndpoint string
	Organization  string
	Name          string
	EnrolledAt    time.Time
	PrivateKey    ed25519.PrivateKey
}

// EnrolRequest is one enrolment.
type EnrolRequest struct {
	// Issuer is the configured issuer (auth.directory's URL, normalized):
	// the 201 must name it byte for byte.
	Issuer string
	// Organization is the configured organization, or "" for none (then
	// the 201's organization is accepted when it has rule 5's form).
	Organization string
	// Code is the one-time enrolment code.
	Code string
	// Name is the display name sent for the operator's audit trail.
	Name string
	// AllowPlainHTTP allows an http issuer (auth.directory.allow_plain_http):
	// the request then goes only to a loopback address, checked on the
	// address dialled, through no proxy.
	AllowPlainHTTP bool
	// RootCAs, when set, are the root certificates an https issuer is
	// checked against in place of the system's: for tests against a TLS
	// test server. Nothing else of the transport can be configured.
	RootCAs *x509.CertPool
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Rand is the source of the key pair; nil means crypto/rand.
	Rand io.Reader

	// baseTransport replaces the HTTP transport under the issuer and
	// redirect rules: tests in this package only.
	baseTransport http.RoundTripper
}

// jwk is the public key as sent: {"kty","crv","x"}, in that order.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
}

// publicJWK is an Ed25519 public key as a JWK.
func publicJWK(pub ed25519.PublicKey) jwk {
	return jwk{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub)}
}

// Enroll enrols the server: it makes an Ed25519 key pair in memory, sends
// the code and the public key, and checks the 201 (contract 1; a client id
// of the right form; the configured issuer, byte for byte; the token
// endpoint <issuer>/oauth2/token; an organization rule 5 accepts). The key
// is returned only with a 201 that passes every check; otherwise it is
// dropped and the error is a *Failure: Lost() says the outcome is unknown
// (no answer, a 5xx, a 201 that did not name one well-formed client id),
// and ClientID names the server a 201 that failed its other checks created,
// for the operator to disable. Nothing is written.
func Enroll(ctx context.Context, r EnrolRequest) (*Enrolment, error) {
	if !ValidCode(r.Code) {
		return nil, fmt.Errorf("the enrolment code must be 1 to %d bytes of UTF-8", MaxCodeBytes)
	}
	if !ValidServerName(r.Name) {
		return nil, fmt.Errorf("the name must be 1 to %d printable ASCII characters", MaxServerNameBytes)
	}
	if r.Organization != "" && !ValidOrganization(r.Organization) {
		return nil, fmt.Errorf("organization %q is not one", r.Organization)
	}
	hc, err := newHTTPClient(transportConfig{
		issuer: r.Issuer, allowPlainHTTP: r.AllowPlainHTTP, rootCAs: r.RootCAs, base: r.baseTransport,
	})
	if err != nil {
		return nil, err
	}
	now, rnd := r.Now, r.Rand
	if now == nil {
		now = time.Now
	}
	if rnd == nil {
		rnd = rand.Reader
	}
	pub, priv, err := ed25519.GenerateKey(rnd)
	if err != nil {
		return nil, fmt.Errorf("make the key pair: %w", err)
	}
	body, err := json.Marshal(struct {
		Code      string `json:"code"`
		PublicKey jwk    `json:"public_key"`
		Name      string `json:"name"`
	}{r.Code, publicJWK(pub), r.Name})
	if err != nil {
		return nil, err
	}
	req, err := newRequest(ctx, http.MethodPost, r.Issuer+"/v1/servers/enroll", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	a, f := exchange(hc, req, StepEnroll, http.StatusCreated, MaxAnswerBody, now)
	if f != nil {
		return nil, f
	}
	if a.status != http.StatusCreated {
		return nil, statusFailure(StepEnroll, a)
	}
	e, err := checkEnrolled(a.body, r.Issuer, r.Organization)
	if err != nil {
		f := a.fail(StepEnroll, KindBadAnswer, err)
		if e != nil {
			f.ClientID = e.ClientID
		}
		return nil, f
	}
	e.Name = r.Name
	e.EnrolledAt = a.local.UTC().Truncate(time.Second)
	e.PrivateKey = priv
	return e, nil
}

// checkEnrolled checks a 201's body. With an error, the Enrolment returned
// (when not nil) carries the client id the body named, only when it named
// one unambiguously (one client_id member) in a client id's form.
func checkEnrolled(body []byte, issuer, organization string) (*Enrolment, error) {
	obj, err := parseDocument(body)
	if err != nil {
		return nil, err
	}
	e := &Enrolment{}
	v, once := obj.one("client_id")
	id, isString := rawString(v)
	switch {
	case obj.count("client_id") > 1:
		return e, errors.New("client_id is named twice")
	case !once || !isString || !ValidClientID(id):
		return e, fmt.Errorf("client_id %q is not srv_ and 32 lowercase hex digits", id)
	}
	e.ClientID = id
	if obj.duplicate() {
		return e, errors.New("a member is named twice")
	}
	if v, ok := obj.one("contract"); !ok || string(v) != "1" {
		return e, ErrContract
	}
	v, _ = obj.one("issuer")
	if s, _ := rawString(v); s != issuer || issuer == "" {
		return e, fmt.Errorf("issuer %q is not the configured %q", s, issuer)
	}
	e.Issuer = issuer
	v, _ = obj.one("token_endpoint")
	if s, _ := rawString(v); s != issuer+"/oauth2/token" {
		return e, fmt.Errorf("token_endpoint %q is not %q", s, issuer+"/oauth2/token")
	}
	e.TokenEndpoint = issuer + "/oauth2/token"
	v, _ = obj.one("organization")
	org, _ := rawString(v)
	if !ValidOrganization(org) {
		return e, fmt.Errorf("%w: organization %q is not one", ErrOrganization, org)
	}
	if organization != "" && org != organization {
		return e, fmt.Errorf("%w: %q, not the configured %q", ErrOrganization, org, organization)
	}
	e.Organization = org
	return e, nil
}

// EnrolmentFormat is enrolment.json's format number.
const EnrolmentFormat = 1

// enrolmentFile is enrolment.json.
type enrolmentFile struct {
	Format        int       `json:"format"`
	ClientID      string    `json:"client_id"`
	Issuer        string    `json:"issuer"`
	TokenEndpoint string    `json:"token_endpoint"`
	Organization  string    `json:"organization"`
	Name          string    `json:"name"`
	EnrolledAt    time.Time `json:"enrolled_at"`
	PrivateKey    string    `json:"private_key"`
}

var enrolmentFileMembers = []string{"format", "client_id", "issuer", "token_endpoint", "organization", "name", "enrolled_at", "private_key"}

// MarshalFile is e as enrolment.json, the private key a PKCS#8 PEM.
func (e *Enrolment) MarshalFile() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(e.PrivateKey)
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(enrolmentFile{
		Format:        EnrolmentFormat,
		ClientID:      e.ClientID,
		Issuer:        e.Issuer,
		TokenEndpoint: e.TokenEndpoint,
		Organization:  e.Organization,
		Name:          e.Name,
		EnrolledAt:    e.EnrolledAt.UTC(),
		PrivateKey:    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ParseEnrolmentFile decodes enrolment.json strictly: one object, no member
// unknown or named twice, nothing after it, format 1, and every field of the
// form an enrolment gives.
func ParseEnrolmentFile(data []byte) (*Enrolment, error) {
	var f enrolmentFile
	if err := decodeStrictObject(data, enrolmentFileMembers, &f); err != nil {
		return nil, err
	}
	switch {
	case f.Format != EnrolmentFormat:
		return nil, fmt.Errorf("format %d is not %d", f.Format, EnrolmentFormat)
	case !ValidClientID(f.ClientID):
		return nil, fmt.Errorf("client_id %q is not one", f.ClientID)
	case config.CheckDirectoryIssuer(f.Issuer) != nil:
		return nil, fmt.Errorf("issuer: %w", config.CheckDirectoryIssuer(f.Issuer))
	case f.TokenEndpoint != f.Issuer+"/oauth2/token":
		return nil, fmt.Errorf("issuer %q and token_endpoint %q do not agree", f.Issuer, f.TokenEndpoint)
	case !ValidOrganization(f.Organization):
		return nil, fmt.Errorf("organization %q is not one", f.Organization)
	case !ValidServerName(f.Name):
		return nil, fmt.Errorf("name %q is not one", f.Name)
	case f.EnrolledAt.IsZero():
		return nil, errors.New("no enrolled_at")
	}
	block, rest := pem.Decode([]byte(f.PrivateKey))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 ||
		!strings.HasPrefix(f.PrivateKey, "-----BEGIN PRIVATE KEY-----") {
		return nil, errors.New("private_key is not one PKCS#8 PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private_key: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private_key is not an Ed25519 key")
	}
	return &Enrolment{
		ClientID:      f.ClientID,
		Issuer:        f.Issuer,
		TokenEndpoint: f.TokenEndpoint,
		Organization:  f.Organization,
		Name:          f.Name,
		EnrolledAt:    f.EnrolledAt,
		PrivateKey:    priv,
	}, nil
}

// CheckConfig reports why e does not belong to the configured issuer and
// organization (organization "" is none configured), or nil. A mismatch
// means the server is not enrolled with the configured service.
func (e *Enrolment) CheckConfig(issuer, organization string) error {
	if e.Issuer != issuer {
		return fmt.Errorf("enrolled with the issuer %q, not the configured %q", e.Issuer, issuer)
	}
	if organization != "" && e.Organization != organization {
		return fmt.Errorf("enrolled in the organization %q, not the configured %q", e.Organization, organization)
	}
	return nil
}

// AcceptedOrganization is the organization answers must name: the
// configured one, or with none configured the enrolment's (rule 5).
func (e *Enrolment) AcceptedOrganization(configured string) string {
	if configured != "" {
		return configured
	}
	return e.Organization
}
