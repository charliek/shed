//go:build darwin

package main

import "testing"

// TestNewApprovalGateResolvesPolicy verifies that newApprovalGate maps the
// biometric policy to the right LocalAuthentication fallback. It must NOT call
// Approve — that would trigger a real macOS authentication prompt and block.
func TestNewApprovalGateResolvesPolicy(t *testing.T) {
	tests := []struct {
		name              string
		policy            string
		wantAllowPassword bool
		wantMethod        string
	}{
		{"strict", PolicyBiometrics, false, PolicyBiometrics},
		{"fallback", PolicyBiometricsOrPassword, true, PolicyBiometricsOrPassword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gate := newApprovalGate(ApprovalConfig{Policy: tt.policy, Scope: "per-session", SessionTTL: "1h"})
			g, ok := gate.(*touchIDGate)
			if !ok {
				t.Fatalf("newApprovalGate returned %T, want *touchIDGate", gate)
			}
			if g.allowPassword != tt.wantAllowPassword {
				t.Errorf("allowPassword = %v, want %v", g.allowPassword, tt.wantAllowPassword)
			}
			if got := g.Method(); got != tt.wantMethod {
				t.Errorf("Method() = %q, want %q", got, tt.wantMethod)
			}
			if g.scope != "per-session" {
				t.Errorf("scope = %q, want per-session", g.scope)
			}
		})
	}
}
