package main

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/charliek/shed/sdk"
	"golang.org/x/sync/singleflight"
)

// relayMinter is the subset of CredentialMinter the desktop path needs: one
// bootstrap that carries a CSR the CALLER generated (or none at all). An
// interface so tests can inject a fake without a live SSH server.
type relayMinter interface {
	MintRelayed(ctx context.Context, t ServerTarget, scope, csrBase64 string) (sdk.Bundle, error)
}

// controlCredential is what the desktop gets back for a named server: the
// credential shape that server issues, and the material for it.
//
// Exactly one of Token / ClientCert is ever populated, decided by the server.
// The private key belonging to ClientCert is NOT here and never passes through
// this process — the app generated it, sent only its CSR, and keeps the key.
type controlCredential struct {
	AuthMode   string
	Token      string
	ClientCert string
	CertSerial string
	ExpiresAt  time.Time
}

// controlTokenProvider mints CONTROL-scoped credentials for named servers on the
// desktop's behalf (the credential.get / token.get UDS requests). Minting is
// BROAD: it can mint for any server in the shed CLI config the agent's SSH key
// is allowlisted on — not only the discovery-scoped servers it brokers
// credentials for — so the desktop can reach a server's control plane without
// the agent watching it.
type controlTokenProvider struct {
	ctx        context.Context
	mint       relayMinter
	sourcePath string // the shed CLI config (~/.shed/config.yaml) to resolve servers from

	// tokens coalesces concurrent CSR-FREE mints for the same server. It is
	// deliberately not applied to the certificate path: two credential.get
	// requests carry two different CSRs, so sharing one answer between them would
	// hand an app a certificate for a key it does not hold.
	tokens singleflight.Group
}

func newControlTokenProvider(ctx context.Context, m relayMinter, sourcePath string) *controlTokenProvider {
	return &controlTokenProvider{
		ctx:        ctx,
		mint:       m,
		sourcePath: expandTilde(sourcePath),
	}
}

// errDesktopTooOldForMTLS explains why a legacy token.get cannot be answered for
// an mtls server. It names the component to upgrade, because the version skew is
// real: shed-desktop and shed-host-agent are separately released, so an
// up-to-date agent routinely runs beside an app that predates certificates. The
// alternative — an empty token and a generic failure — would send a user hunting
// through server logs for a client-side version problem.
func errDesktopTooOldForMTLS(server string) error {
	return fmt.Errorf("server %q issues client certificates (auth.mode: mtls); this shed-desktop is too old to use one — upgrade the app", server)
}

// Token returns a control-scoped BEARER token (and its expiry) for the named
// server — the legacy `token.get` answer.
//
// It errors when the server is unknown, has no SSH endpoint, is not secure (open
// servers are rejected here, before any mint), or issues certificates rather
// than tokens. That last case is a version-skew error, not a server error, and
// says so.
//
// It always mints a FRESH token rather than caching one. The desktop asks only
// when it needs one (it caches per token TTL and re-requests at-most-once on a
// 401), and a token cached here can have been silently invalidated by the target
// server restarting — which regenerates its token authority — with no signal to
// the agent. Serving a cached copy is what wedged a restarted server at 401 in
// the desktop until the agent was restarted; forcing a fresh mint lets the
// desktop's existing 401 retry recover on its own. Concurrent asks for the same
// server still coalesce onto one SSH round-trip.
func (p *controlTokenProvider) Token(serverName string) (string, time.Time, error) {
	target, err := p.resolve(serverName)
	if err != nil {
		return "", time.Time{}, err
	}
	// Short-circuit on what the config already records, so the common case costs
	// no SSH round-trip to discover. A stale entry is caught below by the mode
	// the server actually answered with, so this is a shortcut, not the check.
	if target.IsMTLS() {
		return "", time.Time{}, errDesktopTooOldForMTLS(serverName)
	}
	v, err, _ := p.tokens.Do(serverName, func() (any, error) {
		return p.mint.MintRelayed(p.ctx, target, scopeControl, "")
	})
	if err != nil {
		return "", time.Time{}, err
	}
	bundle := v.(sdk.Bundle)
	if bundle.Mode() == sdk.AuthModeMTLS {
		return "", time.Time{}, errDesktopTooOldForMTLS(serverName)
	}
	return bundle.Token, bundle.ExpiresAt, nil
}

// Credential returns a control-scoped credential for the named server in
// whichever shape that server issues — the `credential.get` answer.
//
// csrBase64 is the app's certificate signing request, passed through verbatim.
// It is optional: an app talking to a token-mode server has no use for one, and
// omitting it against an mtls server produces the server's own explicit upgrade
// error rather than a silent failure.
//
// Like Token, it never caches: the credential belongs to the app, keyed to a
// private key this process has never seen, so there is nothing here worth
// holding on to.
func (p *controlTokenProvider) Credential(serverName, csrBase64 string) (controlCredential, error) {
	target, err := p.resolve(serverName)
	if err != nil {
		return controlCredential{}, err
	}
	bundle, err := p.mint.MintRelayed(p.ctx, target, scopeControl, csrBase64)
	if err != nil {
		return controlCredential{}, err
	}
	return controlCredential{
		AuthMode:   bundle.Mode(),
		Token:      bundle.Token,
		ClientCert: bundle.ClientCert,
		CertSerial: bundle.CertSerial,
		ExpiresAt:  bundle.ExpiresAt,
	}, nil
}

// resolve looks the server up by name in the shed CLI config and returns its
// target. Minting requires both an SSH endpoint (to bootstrap over) and a secure
// (https) server — an open http server needs no credential and can't be minted
// for.
func (p *controlTokenProvider) resolve(name string) (ServerTarget, error) {
	targets, err := LoadDiscoveredServers(p.sourcePath)
	if err != nil {
		return ServerTarget{}, fmt.Errorf("reading server config: %w", err)
	}
	for _, t := range targets {
		if t.Name == name {
			if t.SSHHost == "" || t.SSHPort == 0 {
				return ServerTarget{}, fmt.Errorf("server %q has no ssh endpoint to mint a control credential over", name)
			}
			if !t.IsSecure() {
				return ServerTarget{}, fmt.Errorf("server %q is not a secure (https) server; control-credential minting is unavailable", name)
			}
			return t, nil
		}
	}
	return ServerTarget{}, fmt.Errorf("unknown server %q", name)
}
