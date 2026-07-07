package main

import "fmt"

// gateFor builds the approval gate for one credential extension from its
// approval policy. Platform-neutral so the darwin Touch ID path and the !darwin
// stub stay behind their build tags (newApprovalGate). namespace/op identify the
// extension when delegating to shed-desktop. An empty/unknown policy fails closed
// (deny-all).
func gateFor(namespace, op string, approval ApprovalConfig, desktop *DesktopServer) ApprovalGate {
	switch approval.EffectivePolicy() {
	case PolicyApproveAll:
		return &noopGate{}
	case PolicyBiometrics, PolicyBiometricsOrPassword:
		// Native Touch ID (SSH only; config.Validate rejects it for AWS/Docker).
		return newApprovalGate(approval)
	case PolicyShedDesktop:
		if desktop == nil {
			// Defensive: the approval channel is always constructed in the daemon,
			// so this is unreachable there. Fail closed rather than panic.
			return &denyGate{}
		}
		return &desktopGate{server: desktop, namespace: namespace, op: op}
	default: // PolicyDenyAll and anything unexpected
		return &denyAllGate{}
	}
}

// denyGate fails every request closed — used when a shed-desktop policy is
// selected but no approval channel is available (defensive; not reachable in
// the daemon, which always constructs one).
type denyGate struct{}

func (g *denyGate) Approve(_, _, _ string) (ApprovalOutcome, error) {
	return ApprovalOutcome{}, fmt.Errorf("approval.policy is shed-desktop but the approval channel is unavailable")
}
func (g *denyGate) Method() string { return PolicyShedDesktop }
