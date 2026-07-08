package sdk

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
)

// certFingerprint returns the pin string clients compare against: "sha256:"
// plus the hex-encoded SHA-256 of the certificate's DER encoding. It mirrors
// the server's internal/servertls.Fingerprint — the sdk is a separate module
// and cannot import internal/, so the (tiny) function is duplicated here.
func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pinVerifier returns a tls.Config.VerifyPeerCertificate callback that trusts
// the server's self-signed cert by fingerprint instead of a CA chain.
func pinVerifier(fingerprint string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("server presented no TLS certificate")
		}
		if got := certFingerprint(rawCerts[0]); got != fingerprint {
			return fmt.Errorf("TLS cert fingerprint mismatch: server presented %s, pinned %s", got, fingerprint)
		}
		return nil
	}
}

// errorRoundTripper fails every request with err. It fails a HostClient closed
// when a TLS pin is configured but the endpoint is not https: rather than
// silently sending unpinned plaintext, every request errors with a clear cause.
type errorRoundTripper struct{ err error }

func (e errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }
