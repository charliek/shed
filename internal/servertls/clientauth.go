package servertls

// clientauth.go is the handshake half of mtls client authentication. The TLS
// stack, given ClientCAs + RequireAndVerifyClientCert, already proves that a
// presented certificate chains to this server's internal CA and is inside its
// validity window. What it cannot know is whether the identity that
// certificate names is STILL authorized: the CA has no CRL, and a client cert
// outlives an SSH key's removal from the allowlist.
//
// AllowlistConnectionVerifier closes that gap at connect time. It is
// deliberately only half the story — see the per-request re-validation in
// internal/api's mtls middleware, which is the authoritative check. TLS
// establishes a peer's identity once per connection, so on a pooled keep-alive
// connection a certificate that expires (or an identity that is de-authorized)
// mid-connection would otherwise keep working until the client happens to
// reconnect. Rejecting at the handshake as well means a de-authorized client is
// refused at the door rather than allowed to open connections and collect 401s.
//
// It is wired as tls.Config.VerifyConnection and NOT as VerifyPeerCertificate,
// for a reason worth stating plainly: crypto/tls does not invoke
// VerifyPeerCertificate when a session is RESUMED (there is no certificate
// message to verify — the peer's identity comes back off the session ticket).
// A VerifyPeerCertificate-based allowlist check would therefore silently not
// run for exactly the long-lived, reconnecting clients it is meant to catch.
// VerifyConnection runs on every completed handshake, new and resumed alike.

import (
	"crypto/tls"
	"errors"
)

// ErrClientCertMissing is returned when the verifier is reached with no peer
// certificate. It should be unreachable under tls.RequireAndVerifyClientCert
// (the stack requires and verifies the chain before calling back, and a resumed
// session carries the peer certificates forward), so it exists purely so a
// future misconfiguration fails closed with a legible reason instead of
// silently authorizing.
var ErrClientCertMissing = errors.New("client certificate: none presented")

// ErrClientCertNotAuthorized is returned when the leaf's Subject CN is not in
// the live allowlist. The message names neither the CN nor the allowlist
// contents: it crosses the wire as a TLS alert to an unauthenticated peer.
var ErrClientCertNotAuthorized = errors.New("client certificate: identity is not authorized")

// AllowlistConnectionVerifier builds a tls.Config.VerifyConnection callback that
// accepts a client certificate only when its Subject CN — the SSH key
// fingerprint the certificate was issued against — is still in the live
// allowlist.
//
// When it runs: on EVERY completed handshake, both new and resumed. That is why
// this is a VerifyConnection callback and not a VerifyPeerCertificate one —
// crypto/tls skips VerifyPeerCertificate entirely on a resumed session, so an
// allowlist check installed there would not run for a client that resumes.
//
// What it is: defense-in-depth, ahead of the authoritative per-request check in
// internal/api's mtls middleware. A connection is verified at most once per
// handshake, so this callback cannot see a de-authorization that lands while a
// pooled connection is already open; the middleware can, and does.
//
// What it may assume: under tls.RequireAndVerifyClientCert this runs AFTER the
// stack's normal chain verification, so cs.PeerCertificates[0] is a leaf that
// has already been proven to chain to ClientCAs and to be inside its validity
// window. This callback adds only the authorization question on top of that.
//
// authorized is consulted on every handshake, so it must read live state rather
// than a snapshot taken at startup. A nil authorized fails every handshake
// closed: an mtls listener with no way to check authorization must not
// authorize anything.
func AllowlistConnectionVerifier(authorized func(fingerprint string) bool) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return ErrClientCertMissing
		}
		if authorized == nil {
			return ErrClientCertNotAuthorized
		}
		// PeerCertificates[0] is the leaf by definition (crypto/tls preserves
		// the peer's send order, leaf first) — the identity being asserted. An
		// issuer further up the chain must never be what gets checked.
		leaf := cs.PeerCertificates[0]
		if !authorized(leaf.Subject.CommonName) {
			return ErrClientCertNotAuthorized
		}
		return nil
	}
}
