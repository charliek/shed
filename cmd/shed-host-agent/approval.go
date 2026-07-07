package main

import "fmt"

// ApprovalOutcome carries audit detail about how a request was approved, for the
// durable log. The shed-desktop gate populates it from the app's response (who
// decided + the scope/TTL the app applied); the native Touch ID gate reports
// its scope; the approve-all/deny-all gates leave it empty.
type ApprovalOutcome struct {
	DecidedBy string // "user" | "touchid" | "policy" | "timeout" | ""
	Scope     string // approval scope applied (e.g. per-session)
	TTL       string // TTL applied (e.g. 4h)
}

// ApprovalGate decides whether a credential operation is allowed. Every request
// goes through a gate — the deny-all default fails closed.
type ApprovalGate interface {
	// Approve returns nil if the operation is allowed, an error (→ deny) otherwise.
	Approve(server, shedName, reason string) (ApprovalOutcome, error)
	// Method names the policy for the audit log: one of the Policy* constants.
	Method() string
}

// noopGate approves every request — the approve-all policy (allowlist/role
// still applies downstream in the AWS/Docker backends).
type noopGate struct{}

func (g *noopGate) Approve(_, _, _ string) (ApprovalOutcome, error) { return ApprovalOutcome{}, nil }
func (g *noopGate) Method() string                                  { return PolicyApproveAll }

// denyAllGate rejects every request — the deny-all policy and the safe default
// (an omitted/empty policy resolves here).
type denyAllGate struct{}

func (g *denyAllGate) Approve(_, _, _ string) (ApprovalOutcome, error) {
	return ApprovalOutcome{}, fmt.Errorf("denied: approval policy is deny-all")
}
func (g *denyAllGate) Method() string { return PolicyDenyAll }
