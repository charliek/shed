package bootstrap

// csr.go is the client half of mtls enrollment: generate a fresh keypair, wrap
// its public half in a PKCS#10 CertificationRequest, and hand the base64 DER to
// the bootstrap request line. The private key never crosses the SSH channel —
// only the CSR does, and the server treats it as a carrier for the public key
// alone (every subject field and attribute it could request is discarded; the
// issued identity comes from the authenticated SSH key). See
// internal/servertls.CA.SignClientCSR.
//
// Everything here is standard library: crypto/ecdsa + crypto/x509. The server
// accepts ECDSA P-256 signed with ECDSA-SHA256 and nothing else, so those are
// not configurable.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
)

// clientKeyPair is a freshly generated enrollment keypair plus the encodings
// the two consumers need: base64 DER of the CSR for the request line, and PEM
// of the private key for the caller's credential store.
type clientKeyPair struct {
	key    *ecdsa.PrivateKey
	csrB64 string
	keyPEM []byte
}

// newClientKeyPair generates a P-256 key and the matching CSR.
//
// The CSR carries an EMPTY subject on purpose. The server ignores whatever
// subject a CSR requests, so populating one would be decoration that implies an
// influence the client does not have — and a client that reads its own CSR back
// would see a subject that bears no relation to the certificate it is issued.
func newClientKeyPair() (clientKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return clientKeyPair{}, fmt.Errorf("sdk/bootstrap: generate client key: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{SignatureAlgorithm: x509.ECDSAWithSHA256}, key)
	if err != nil {
		return clientKeyPair{}, fmt.Errorf("sdk/bootstrap: create CSR: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return clientKeyPair{}, fmt.Errorf("sdk/bootstrap: marshal client key: %w", err)
	}
	return clientKeyPair{
		key: key,
		// STANDARD base64 with padding — the server decodes with
		// base64.StdEncoding.Strict() and rejects the URL-safe alphabet.
		csrB64: base64.StdEncoding.EncodeToString(csrDER),
		keyPEM: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// matchesIssuedCert verifies that the certificate the server returned actually
// certifies the key we just generated.
//
// This is not a trust check — the server is already authenticated by the SSH
// host key pin, and the certificate's own trustworthiness is established when
// the TLS listener verifies it against its CA. It is a correctness check: a
// mismatched pair produces a TLS handshake failure at some later, unrelated
// moment ("bad certificate" from a server that looks broken), and catching it
// at enrollment turns that into a legible error at the point of the mistake.
func (kp clientKeyPair) matchesIssuedCert(certPEM []byte) error {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return errNoCertificatePEM
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("sdk/bootstrap: parse issued certificate: %w", err)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(kp.key.Public()) {
		return errCertKeyMismatch
	}
	return nil
}
