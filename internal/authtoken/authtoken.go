// Package authtoken holds the credential vocabulary shed-server shares between
// the SSH bootstrap channel and the HTTP API: the two scopes a client
// certificate can carry, and the client kinds recorded at issuance.
//
// The package name is historical. It once also issued and validated HTTP
// bearer tokens for the removed token auth mode; that store, its minting and
// its validation were deleted with the mode. Every credential this server
// issues is now a short-lived client certificate (internal/servertls), whose
// Subject OU carries one of the scopes below and whose Subject O carries the
// client kind.
package authtoken

// Credential scopes. A client certificate carries exactly one, burned in at
// issuance; authorization is by route (internal/api authorizeScope). The legacy
// "admin" scope is intentionally absent: issuance is gated by SSH access, so
// there is no separate HTTP admin capability.
const (
	ScopeControl     = "control"     // shed lifecycle / control plane (CLI, desktop); also accepted on the Connect tunnel and egress stream
	ScopeCredentials = "credentials" // credential bus (/api/plugins/*); also accepted on the Connect tunnel + egress audit stream (host-agent)
)

// Client kinds are advisory metadata, recorded in the issued certificate for
// audit only.
const (
	ClientCLI       = "cli"
	ClientHostAgent = "host-agent"
	ClientDesktop   = "desktop"
	// ClientMobile is the shed mobile app. The app itself sends the literal
	// "shed-mobile" on the wire today; the bootstrap parser normalizes that
	// spelling to this canonical value (see internal/sshd/bootstrap.go).
	ClientMobile = "mobile"
)

// ValidScope reports whether s is an issuable scope.
func ValidScope(s string) bool {
	return s == ScopeControl || s == ScopeCredentials
}
