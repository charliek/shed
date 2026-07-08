//go:build !darwin

package main

// ApprovalGate stub for non-darwin builds — Touch ID is not available, so a
// native biometric policy can't be honored. Fail CLOSED (deny-all) rather than
// silently approving every request.

func newApprovalGate(_ ApprovalConfig) ApprovalGate {
	return &denyAllGate{}
}
