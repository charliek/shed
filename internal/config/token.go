package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// HTTP bearer-token scopes. The scope is encoded in the token itself
// (shed_<scope>_<random>) for fast rejection and readable logs. The legacy
// "admin" scope was removed in the auth-issuance-v2 redesign: minting and
// revocation are gated by SSH access, so there is no separate HTTP admin grant.
const (
	TokenScopeControl     = "control"     // shed lifecycle / control plane (CLI, desktop)
	TokenScopeCredentials = "credentials" // the credential bus (host-agent)
)

// ValidTokenScope reports whether s is a known token scope.
func ValidTokenScope(s string) bool {
	switch s {
	case TokenScopeControl, TokenScopeCredentials:
		return true
	}
	return false
}

// GenerateToken mints a token of the given scope: shed_<scope>_<random>.
//
// Superseded by internal/authtoken (the live token store) in the
// auth-issuance-v2 migration: this config-side minter has no production caller
// and is removed in sub-step 1d together with the static auth.http.tokens path.
func GenerateToken(scope string) (string, error) {
	if !ValidTokenScope(scope) {
		return "", fmt.Errorf("invalid token scope: %q (must be control or credentials)", scope)
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("shed_%s_%s", scope, base64.RawURLEncoding.EncodeToString(b)), nil
}

// TokenScope returns the scope encoded in a shed token, or "" if the token is
// malformed or carries an unknown scope.
func TokenScope(token string) string {
	rest, ok := strings.CutPrefix(token, "shed_")
	if !ok {
		return ""
	}
	scope, _, ok := strings.Cut(rest, "_")
	if !ok || !ValidTokenScope(scope) {
		return ""
	}
	return scope
}
