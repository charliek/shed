package clienttoken

// authfail.go re-exports the credential-refused classifier.
//
// The classifier itself lives in sdk/authfail, not here, because the in-tree
// CLI is not its only user: the SDK's bus client and the host-agent's egress
// subscriber face the identical question — "did the server refuse the
// credential we presented, in either of the two shapes that refusal takes?" —
// and they live in the sdk module, which cannot import internal/. One
// classifier with one alert allowlist is the point: a second copy would drift
// the moment a Go release re-words an alert, and the drift would be silent
// (the copy degrades to "401 only", which still compiles and still passes every
// token-mode test).
//
// It is re-exported rather than replaced so callers of this package keep asking
// their credential's own package the question.

import "github.com/charliek/shed/sdk/authfail"

// IsAuthFailure reports whether a request outcome means the server refused our
// credential and a re-mint is worth attempting: HTTP 401, or a peer TLS alert
// naming a certificate problem. See sdk/authfail for the full contract and the
// deliberate exclusion of alert 40.
func IsAuthFailure(status int, err error) bool { return authfail.IsAuthFailure(status, err) }
