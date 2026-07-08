//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework LocalAuthentication -framework Foundation
#include <LocalAuthentication/LocalAuthentication.h>
#include <dispatch/dispatch.h>

static int authenticate(const char *reason, int allowPassword) {
    __block int result = 0;
    dispatch_semaphore_t sema = dispatch_semaphore_create(0);

    LAContext *context = [[LAContext alloc] init];
    NSError *error = nil;

    LAPolicy policy = allowPassword
        ? LAPolicyDeviceOwnerAuthentication
        : LAPolicyDeviceOwnerAuthenticationWithBiometrics;

    if (![context canEvaluatePolicy:policy error:&error]) {
        return -1;
    }

    NSString *nsReason = [NSString stringWithUTF8String:reason];
    [context evaluatePolicy:policy
            localizedReason:nsReason
                      reply:^(BOOL success, NSError *authError) {
        result = success ? 1 : 0;
        dispatch_semaphore_signal(sema);
    }];

    dispatch_semaphore_wait(sema, DISPATCH_TIME_FOREVER);
    return result;
}
*/
import "C"

import (
	"fmt"
	"sync"
	"time"
	"unsafe"
)

// touchIDGate implements ApprovalGate using native macOS Touch ID — the
// `biometrics` / `biometrics-or-password` policies, which work with no
// shed-desktop app running. `scope` (per-request/per-session/per-shed) +
// `sessionTTL` cache approvals.
type touchIDGate struct {
	scope         string
	allowPassword bool
	sessionTTL    time.Duration
	ttlText       string

	mu            sync.Mutex
	lastApproval  time.Time
	shedApprovals map[string]time.Time
}

func newApprovalGate(cfg ApprovalConfig) ApprovalGate {
	ttl, err := time.ParseDuration(cfg.SessionTTL)
	if err != nil {
		ttl = 4 * time.Hour
	}

	return &touchIDGate{
		scope:         cfg.Scope,
		allowPassword: resolveAllowPassword(cfg.Policy),
		sessionTTL:    ttl,
		ttlText:       cfg.SessionTTL,
		shedApprovals: make(map[string]time.Time),
	}
}

// Method returns the policy name for the audit log.
func (g *touchIDGate) Method() string {
	if g.allowPassword {
		return PolicyBiometricsOrPassword
	}
	return PolicyBiometrics
}

func (g *touchIDGate) Approve(server, shedName, reason string) (ApprovalOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := ApprovalOutcome{DecidedBy: "touchid", Scope: g.scope, TTL: g.ttlText}

	// Per-shed approvals are keyed by server/shed so identical shed names on
	// different servers do not share an approval. per-session stays global
	// (one approval covers the whole agent for the TTL).
	shedKey := serverShedKey(server, shedName)

	// Check cached approval based on scope
	now := time.Now()
	switch g.scope {
	case "per-session":
		if !g.lastApproval.IsZero() && now.Sub(g.lastApproval) < g.sessionTTL {
			out.DecidedBy = "policy" // served from the cached session approval
			return out, nil
		}
	case "per-shed":
		if t, ok := g.shedApprovals[shedKey]; ok && now.Sub(t) < g.sessionTTL {
			out.DecidedBy = "policy"
			return out, nil
		}
	case "per-request":
		// Always prompt
	}

	prompt := fmt.Sprintf("shed-extensions: %s (server: %s, shed: %s)", reason, server, shedName)
	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))

	var allowPassword C.int
	if g.allowPassword {
		allowPassword = 1
	}
	result := C.authenticate(cPrompt, allowPassword)

	switch result {
	case 1:
		g.lastApproval = now
		g.shedApprovals[shedKey] = now
		return out, nil
	case 0:
		return ApprovalOutcome{}, fmt.Errorf("touch ID authentication denied")
	default:
		return ApprovalOutcome{}, fmt.Errorf("touch ID not available on this device")
	}
}
