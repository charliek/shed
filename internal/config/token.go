package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// HTTP bearer-token scopes. The scope is encoded in the token itself
// (shed_<scope>_<random>) so the server can authorize from the token alone,
// and the credential/admin grants can split out later without re-issuing.
const (
	TokenScopeControl     = "control"     // shed lifecycle / control plane (CLI, desktop)
	TokenScopeCredentials = "credentials" // the credential bus (host-agent)
	TokenScopeAdmin       = "admin"       // destructive/admin operations
)

// ValidTokenScope reports whether s is a known token scope.
func ValidTokenScope(s string) bool {
	switch s {
	case TokenScopeControl, TokenScopeCredentials, TokenScopeAdmin:
		return true
	}
	return false
}

// GenerateToken mints a token of the given scope: shed_<scope>_<random>.
func GenerateToken(scope string) (string, error) {
	if !ValidTokenScope(scope) {
		return "", fmt.Errorf("invalid token scope: %q (must be control, credentials, or admin)", scope)
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
