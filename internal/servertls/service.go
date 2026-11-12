package servertls

// service.go is the service-client half of mtls: certificates for
// orchestrators (auth.clients) rather than for a person's SSH key.
//
// A service certificate is told apart from a user's by its shape alone. A
// user certificate (SignClientCSR) has CN = an SSH key fingerprint
// ("SHA256:..."), OU = its scope ("control" or "credentials") and no SANs. A
// service certificate has CN = the client name, OU = exactly ["service"], and
// exactly one URI SAN, shed-client://<name>. The client-name pattern has no
// ':', so it can never match a fingerprint, and a user certificate has neither
// the OU nor any URI SAN, so it is never service-shaped. A certificate that
// carries either service marker but not the exact shape is malformed and
// refused, never treated as a user's.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"
)

const (
	// ServiceOU is the Subject OU of every service certificate.
	ServiceOU = "service"
	// ServiceURIScheme is the scheme of a service certificate's one URI SAN.
	ServiceURIScheme = "shed-client"
)

// serviceNamePattern mirrors config.ValidClientName (this package does not
// import internal/config).
var serviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

var (
	// ErrInvalidServiceName is returned when a client name does not match the
	// service-client name pattern.
	ErrInvalidServiceName = errors.New("service certificate: invalid client name")
	// ErrMalformedServiceCert is returned for a certificate that carries a
	// service marker (OU "service" or a shed-client URI SAN) but not the exact
	// service shape.
	ErrMalformedServiceCert = errors.New("service certificate: malformed")
	// ErrUnknownServiceClient is returned at the handshake for a well-formed
	// service certificate whose client is not configured.
	ErrUnknownServiceClient = errors.New("service certificate: client is not configured")
)

// ServiceURI returns the URI SAN a service certificate for client carries.
func ServiceURI(client string) *url.URL {
	return &url.URL{Scheme: ServiceURIScheme, Host: client}
}

// SignServiceCert issues a service-client certificate for pub: CN = client,
// OU = ["service"], one URI SAN shed-client://<client>, no other SANs,
// keyUsage digitalSignature, extKeyUsage clientAuth. NotAfter is now+ttl,
// clamped to the CA's own expiry (issuanceWindow), and issuance is refused
// when the CA is about to expire.
func (c CA) SignServiceCert(pub *ecdsa.PublicKey, client string, ttl time.Duration) ([]byte, error) {
	if c.cert == nil || c.key == nil {
		return nil, ErrCANotInitialized
	}
	if !serviceNamePattern.MatchString(client) {
		return nil, ErrInvalidServiceName
	}
	if ttl <= 0 {
		return nil, ErrCAInvalidTTL
	}
	if pub == nil || pub.Curve != elliptic.P256() {
		return nil, ErrCSRUnsupportedKey
	}
	notBefore, notAfter, err := c.issuanceWindow(ttl)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	skid, err := subjectKeyID(pub)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: client, OrganizationalUnit: []string{ServiceOU}},
		URIs:                  []*url.URL{ServiceURI(client)},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		SubjectKeyId:          skid,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, pub, c.key)
	if err != nil {
		return nil, fmt.Errorf("issue service certificate: %w", err)
	}
	return der, nil
}

// ServiceClientName classifies a client certificate. isService is true when
// the leaf carries either service marker: an OU "service", or any URI SAN with
// the shed-client scheme. It is valid (err == nil) only when the OU is exactly
// ["service"], the CN matches the client-name pattern, there is exactly one URI
// SAN and it equals shed-client://<CN>, and there are no DNS, IP or email SANs.
// A service-shaped but malformed certificate returns isService = true and an
// error, and must be refused: never fall back to treating it as a user's.
func ServiceClientName(leaf *x509.Certificate) (name string, isService bool, err error) {
	if leaf == nil {
		return "", false, nil
	}
	for _, ou := range leaf.Subject.OrganizationalUnit {
		if ou == ServiceOU {
			isService = true
		}
	}
	for _, u := range leaf.URIs {
		if u != nil && u.Scheme == ServiceURIScheme {
			isService = true
		}
	}
	if !isService {
		return "", false, nil
	}
	cn := leaf.Subject.CommonName
	switch {
	case len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] != ServiceOU:
		return "", true, ErrMalformedServiceCert
	case !serviceNamePattern.MatchString(cn):
		return "", true, ErrMalformedServiceCert
	case len(leaf.URIs) != 1 || leaf.URIs[0] == nil || leaf.URIs[0].String() != ServiceURI(cn).String():
		return "", true, ErrMalformedServiceCert
	case len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 0 || len(leaf.EmailAddresses) != 0:
		return "", true, ErrMalformedServiceCert
	}
	return cn, true, nil
}

// ClientConnectionVerifier builds the HTTPS listener's VerifyConnection
// callback for mtls mode. A service-shaped leaf is accepted only when it is
// well formed and clientKnown(name) holds; authorized (the SSH allowlist) is
// never consulted for it. Any other leaf goes through the allowlist check,
// exactly as AllowlistConnectionVerifier does. A nil clientKnown knows no
// clients, so every service certificate fails closed.
//
// Like the allowlist check, this runs on every handshake, new and resumed, and
// is the first of two checks: the API middleware re-derives the identity on
// every request.
func ClientConnectionVerifier(authorized func(fingerprint string) bool, clientKnown func(name string) bool) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return ErrClientCertMissing
		}
		leaf := cs.PeerCertificates[0]
		name, isService, err := ServiceClientName(leaf)
		if isService {
			if err != nil {
				return err
			}
			if clientKnown == nil || !clientKnown(name) {
				return ErrUnknownServiceClient
			}
			return nil
		}
		if authorized == nil || !authorized(leaf.Subject.CommonName) {
			return ErrClientCertNotAuthorized
		}
		return nil
	}
}
