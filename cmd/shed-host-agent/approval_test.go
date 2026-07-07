package main

import "testing"

func TestNoopGateApproves(t *testing.T) {
	g := &noopGate{}
	if _, err := g.Approve("srv", "shed", "reason"); err != nil {
		t.Errorf("noopGate.Approve() = %v, want nil", err)
	}
	if got := g.Method(); got != PolicyApproveAll {
		t.Errorf("noopGate.Method() = %q, want %q", got, PolicyApproveAll)
	}
}

func TestDenyAllGateDenies(t *testing.T) {
	g := &denyAllGate{}
	if _, err := g.Approve("srv", "shed", "reason"); err == nil {
		t.Error("denyAllGate.Approve() = nil, want error")
	}
	if got := g.Method(); got != PolicyDenyAll {
		t.Errorf("denyAllGate.Method() = %q, want %q", got, PolicyDenyAll)
	}
}

// gateFor maps each policy value to the right gate; empty/unknown fails closed.
func TestGateForSelectsByPolicy(t *testing.T) {
	tests := []struct {
		policy string
		want   string // expected Method()
	}{
		{"", PolicyDenyAll},
		{PolicyDenyAll, PolicyDenyAll},
		{PolicyApproveAll, PolicyApproveAll},
		{PolicyShedDesktop, PolicyShedDesktop}, // desktop nil → denyGate, Method() still shed-desktop
	}
	for _, tt := range tests {
		t.Run(tt.policy, func(t *testing.T) {
			g := gateFor("ssh-agent", "sign", ApprovalConfig{Policy: tt.policy}, nil)
			if got := g.Method(); got != tt.want {
				t.Errorf("gateFor(%q).Method() = %q, want %q", tt.policy, got, tt.want)
			}
		})
	}
}

// A shed-desktop policy with no desktop channel fails closed.
func TestGateForShedDesktopNoChannelDenies(t *testing.T) {
	g := gateFor("ssh-agent", "sign", ApprovalConfig{Policy: PolicyShedDesktop}, nil)
	if _, err := g.Approve("srv", "shed", "x"); err == nil {
		t.Error("shed-desktop policy with no approval channel should deny")
	}
}
