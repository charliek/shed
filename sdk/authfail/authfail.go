// Package authfail classifies "the server refused our credential" — the trigger
// for a reactive re-mint.
//
// In token mode that signal is simple and has always been an HTTP 401. In mtls
// mode it usually is NOT an HTTP status at all, because the rejection happens
// during (or immediately after) the TLS handshake, before any HTTP response
// exists:
//
//   - TLS 1.2: the server rejects the client certificate inside the handshake,
//     so the client's Handshake/Dial fails outright.
//   - TLS 1.3: the client finishes its side of the handshake optimistically and
//     only learns of the rejection when the server's alert arrives — surfacing
//     as an error on the FIRST read or write of the connection, i.e. out of
//     http.Client.Do with no *http.Response at all.
//
// Both shapes have to trigger the same re-enrollment the 401 does, which is why
// this is one classifier over (status, err) rather than a status check with a
// TLS check bolted on somewhere else.
//
// It is deliberately applied REGARDLESS of the mode the client thinks it is in.
// A client whose stored entry says "token" talking to a server that has since
// been flipped to mtls will see a TLS alert, not a 401; a client whose entry
// says "mtls" talking to a server flipped back to token will see a 401 for its
// (now meaningless) certificate. Keying the trigger on the observed failure
// rather than the recorded mode is what makes the flip recoverable in both
// directions without operator action.
package authfail

import (
	"errors"
	"net"
	"net/http"
	"strings"
)

// authAlertDescriptions are the TLS alert descriptions that mean "your
// certificate is missing, unacceptable, or no longer valid" — as rendered by
// crypto/tls's alert.String(), which is adjective-first ("expired certificate",
// not "certificate expired"). The exact set was read off a live Go 1.25 server
// rather than guessed; TestIsAuthFailureAgainstRealTLSRejection re-derives it
// on every run so a wording change in a future Go fails loudly here instead of
// silently degrading this classifier to "401 only".
//
// The list is an ALLOWLIST on purpose, and one omission is deliberate and worth
// stating plainly: alert 40, "handshake failure".
//
// A Go TLS 1.2 server answers "client presented NO certificate" with alert 40,
// not with a certificate-specific alert (TLS 1.3 answers the same case with
// "certificate required", alert 116). Alert 40 is also what a genuine
// negotiation failure produces — no shared cipher, curve, or protocol version —
// so treating it as an auth failure would make an unfixable misconfiguration
// mint a new credential over SSH on every single request, forever, and still
// fail. Excluding it is the right trade, and it costs nothing here because the
// case it would have caught is already covered twice over:
//
//   - Our client and our server are both Go with MinVersion TLS 1.2 and no
//     maximum, so they always negotiate TLS 1.3, where the no-certificate case
//     is the unambiguous alert 116 that IS on this list.
//   - "We hold no credential at all" never needs an alert to be discovered:
//     a Source that can mint but holds nothing usable enrolls in EnsureFresh,
//     before the first dial (see Credential.Usable). The alert path only has to
//     catch rejection of a certificate we DO hold — expired, revoked, foreign
//     CA, de-authorized — and every one of those produces a specific alert in
//     BOTH TLS versions.
var authAlertDescriptions = []string{
	"certificate required",          // 116 — TLS 1.3, no cert presented to a RequireAndVerify listener
	"bad certificate",               // 42  — a malformed or unacceptable certificate
	"unsupported certificate",       // 43
	"revoked certificate",           // 44
	"expired certificate",           // 45  — observed for an expired cert on TLS 1.2 AND 1.3
	"unknown certificate",           // 46
	"unknown certificate authority", // 48  — observed for a foreign CA on TLS 1.2 AND 1.3
	"access denied",                 // 49  — the identity is not authorized
}

// altAuthAlertPhrases are the same conditions spelled noun-first. crypto/tls
// does not currently produce these, but other TLS stacks (and a future Go)
// render alert names in that order, and a proxy may re-word an error entirely.
// Matching both spellings costs nothing and keeps the classifier from silently
// degrading into "401 only" if the rendering ever changes.
var altAuthAlertPhrases = []string{
	"certificate expired",
	"certificate revoked",
	"certificate unknown",
	"unknown ca",
}

// IsAuthFailure reports whether a request outcome means the server refused our
// credential and a re-mint is worth attempting.
//
// status is the HTTP status of a completed response, or 0 when the request
// never produced one. err is the transport error, or nil on a completed
// response. Exactly one of the two is normally meaningful; passing both is
// harmless.
//
// It returns true for:
//   - HTTP 401 Unauthorized (token mode, and mtls per-request re-validation of
//     an expired or de-authorized certificate);
//   - a TLS alert from the peer naming a certificate problem (mtls handshake
//     rejection, in either TLS version's timing).
//
// It returns false for everything else, including 403 (authenticated but not
// permitted — re-minting the same identity cannot help), connection refused, no
// route, DNS failure, timeouts, EOF, a pin mismatch, and non-certificate
// handshake failures.
func IsAuthFailure(status int, err error) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	return isTLSAuthFailure(err)
}

// isTLSAuthFailure walks err for a peer TLS alert that names a certificate
// problem.
//
// It matches on strings because it has to: crypto/tls delivers a received alert
// as a *net.OpError whose Err is the UNEXPORTED alert type, so there is no
// exported type or sentinel to compare against (tls.AlertError exists but is
// not what this path produces). The matching is scoped as tightly as the
// standard library allows — first by locating the "remote error" OpError, and
// only falling back to a whole-string scan for the wrapped/reformatted case.
func isTLSAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	// Preferred path: find crypto/tls's own wrapper for a received alert. Its
	// Op is the literal "remote error", which is what distinguishes an alert the
	// PEER sent from a local failure that happens to mention certificates (a pin
	// mismatch, say, which must not trigger a re-mint).
	//
	// The chain is walked by hand rather than with errors.As because As stops at
	// the FIRST *net.OpError, and a dial failure nests its own OpError around the
	// handshake's — the alert we want may be the inner one.
	for e := err; e != nil; e = errors.Unwrap(e) {
		if opErr, ok := e.(*net.OpError); ok &&
			opErr.Op == "remote error" && opErr.Err != nil && matchesAuthAlert(opErr.Err.Error()) {
			return true
		}
	}
	// Fallback: the alert survived only as text (http.Transport and the url
	// package both wrap round-trip errors, and an intermediary may reformat).
	// Anchoring on the "remote error: tls: " prefix keeps this from matching a
	// local x509 verification failure.
	msg := err.Error()
	if i := strings.Index(msg, "remote error: tls: "); i >= 0 {
		return matchesAuthAlert(msg[i+len("remote error: "):])
	}
	return false
}

// matchesAuthAlert reports whether an alert rendering ("tls: expired
// certificate", possibly with trailing context) names one of the
// credential-refused conditions.
func matchesAuthAlert(s string) bool {
	desc := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "tls:")))
	for _, want := range authAlertDescriptions {
		if strings.HasPrefix(desc, want) {
			return true
		}
	}
	for _, want := range altAuthAlertPhrases {
		if strings.HasPrefix(desc, want) {
			return true
		}
	}
	return false
}
