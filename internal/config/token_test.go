package config

import "testing"

func TestGenerateAndScopeToken(t *testing.T) {
	for _, scope := range []string{TokenScopeControl, TokenScopeCredentials} {
		tok, err := GenerateToken(scope)
		if err != nil {
			t.Fatalf("GenerateToken(%q): %v", scope, err)
		}
		if got := TokenScope(tok); got != scope {
			t.Errorf("TokenScope(%q) = %q, want %q", tok, got, scope)
		}
		// Two tokens of the same scope must differ.
		tok2, _ := GenerateToken(scope)
		if tok == tok2 {
			t.Errorf("GenerateToken(%q) is not random", scope)
		}
	}

	for _, bad := range []string{"bogus", "admin"} {
		if _, err := GenerateToken(bad); err == nil {
			t.Errorf("GenerateToken(%q) should error (invalid scope)", bad)
		}
	}

	for _, bad := range []string{"", "nope", "shed_", "shed_control", "shed_bogus_abc", "token_control_abc"} {
		if s := TokenScope(bad); s != "" {
			t.Errorf("TokenScope(%q) = %q, want empty", bad, s)
		}
	}
}
