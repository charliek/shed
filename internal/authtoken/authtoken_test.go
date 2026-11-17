package authtoken

import "testing"

func TestValidScope(t *testing.T) {
	tests := []struct {
		scope string
		want  bool
	}{
		{ScopeControl, true},
		{ScopeCredentials, true},
		{"admin", false}, // the removed legacy scope
		{"", false},
		{"Control", false},
		{"control ", false},
	}
	for _, tt := range tests {
		t.Run(tt.scope, func(t *testing.T) {
			if got := ValidScope(tt.scope); got != tt.want {
				t.Errorf("ValidScope(%q) = %v, want %v", tt.scope, got, tt.want)
			}
		})
	}
}
