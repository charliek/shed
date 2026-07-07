package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// controlTokenProvider mints and caches CONTROL-scoped tokens for named servers
// on the desktop's behalf (the token.get UDS request). Minting is BROAD: it can
// mint for any server in the shed CLI config the agent's SSH key is allowlisted
// on — not only the discovery-scoped servers it brokers credentials for — so the
// desktop can reach a server's control plane without the agent watching it.
type controlTokenProvider struct {
	ctx        context.Context
	mint       minter
	sourcePath string // the shed CLI config (~/.shed/config.yaml) to resolve servers from

	mu      sync.Mutex
	sources map[string]*credentialSource // by server name, scopeControl
}

func newControlTokenProvider(ctx context.Context, m minter, sourcePath string) *controlTokenProvider {
	return &controlTokenProvider{
		ctx:        ctx,
		mint:       m,
		sourcePath: expandTilde(sourcePath),
		sources:    make(map[string]*credentialSource),
	}
}

// Token returns a control-scoped token (and its expiry) for the named server,
// minting over SSH. It errors when the server is unknown, has no SSH endpoint,
// or is not secure (open servers are rejected here, before any mint).
//
// It always mints a FRESH token rather than serving this source's cache. The
// desktop asks only when it needs one (it caches per token TTL and re-requests
// at-most-once on a 401), and a token cached here can have been silently
// invalidated by the target server restarting — which regenerates its token
// authority — with no signal to the agent. Serving the cached copy is what
// wedged a restarted server at 401 in the desktop until the agent was restarted;
// forcing a fresh mint lets the desktop's existing 401 retry recover on its own.
// (forceTokenWithExpiry still single-flight-coalesces concurrent asks; see its
// doc for the one narrow, self-healing race.)
func (p *controlTokenProvider) Token(serverName string) (string, time.Time, error) {
	target, err := p.resolve(serverName)
	if err != nil {
		return "", time.Time{}, err
	}
	return p.sourceFor(serverName, target).forceTokenWithExpiry()
}

// resolve looks the server up by name in the shed CLI config and returns its
// target. Minting requires both an SSH endpoint (to bootstrap over) and a secure
// (https) server — an open http server needs no token and can't be minted for.
func (p *controlTokenProvider) resolve(name string) (ServerTarget, error) {
	targets, err := LoadDiscoveredServers(p.sourcePath)
	if err != nil {
		return ServerTarget{}, fmt.Errorf("reading server config: %w", err)
	}
	for _, t := range targets {
		if t.Name == name {
			if t.SSHHost == "" || t.SSHPort == 0 {
				return ServerTarget{}, fmt.Errorf("server %q has no ssh endpoint to mint a control token over", name)
			}
			if !t.IsSecure() {
				return ServerTarget{}, fmt.Errorf("server %q is not a secure (https) server; control-token minting is unavailable", name)
			}
			return t, nil
		}
	}
	return ServerTarget{}, fmt.Errorf("unknown server %q", name)
}

// sourceFor returns the per-server control-token source, (re)creating it when the
// server's endpoint changed since it was cached.
func (p *controlTokenProvider) sourceFor(name string, target ServerTarget) *credentialSource {
	p.mu.Lock()
	defer p.mu.Unlock()
	if src, ok := p.sources[name]; ok && src.target == target {
		return src
	}
	src := newCredentialSource(p.ctx, p.mint, target, scopeControl)
	p.sources[name] = src
	return src
}
