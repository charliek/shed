package sdk

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
)

// certFingerprint returns the pin string clients compare against: "sha256:"
// plus the hex-encoded SHA-256 of the certificate's DER encoding. It mirrors
// the server's internal/servertls.Fingerprint — the sdk is a separate module
// and cannot import internal/, so the (tiny) function is duplicated here.
func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pinTLSConfig returns a tls.Config that trusts the server's self-signed cert
// by fingerprint instead of a CA chain. InsecureSkipVerify disables the default
// chain/hostname check precisely because the self-signed cert is its own trust
// anchor — VerifyPeerCertificate re-imposes trust by comparing the pin.
func pinTLSConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // not insecure: VerifyPeerCertificate pins the cert
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("server presented no TLS certificate")
			}
			if got := certFingerprint(rawCerts[0]); got != fingerprint {
				return fmt.Errorf("TLS cert fingerprint mismatch: server presented %s, pinned %s", got, fingerprint)
			}
			return nil
		},
	}
}
