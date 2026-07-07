package main

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	sdk "github.com/charliek/shed/sdk"
)

// SharedDeps are the server-agnostic components shared by every per-server
// watcher group. Built once in main and injected into each group.
type SharedDeps struct {
	SSHBackend     SSHBackend
	AWSBackend     AWSBackend    // may be nil when AWS is not configured
	DockerBackend  DockerBackend // may be nil when Docker is not configured
	SSHApproval    ApprovalGate
	AWSApproval    ApprovalGate
	DockerApproval ApprovalGate
	Audit          *AuditLogger
	Logger         *slog.Logger
	// Minter mints the per-server credentials token over SSH (secure mode). nil
	// disables minting (every server then uses its configured Token).
	Minter *CredentialMinter
}

// watcherGroup owns the per-server HostClient and handler goroutines for one
// shed server. Cancelling its context tears the whole group down; done closes
// once all of its goroutines have exited.
type watcherGroup struct {
	target ServerTarget
	client *sdk.HostClient
	cancel context.CancelFunc
	done   chan struct{}
}

// shouldMint reports whether the agent should self-mint a credentials token for
// t (attaching a token provider) rather than send its configured, usually-empty
// static token. Minting is warranted only for a SECURE server, whose authoritative
// local signal is an https api_url (tokens ⟺ TLS ⟺ secure; `shed server add` always
// writes an https api_url for a secure server). It also needs a usable SSH endpoint
// to mint over and a configured minter. Note: SSHPort>0 alone is NOT the signal —
// every shed server has an SSH endpoint, including open-mode ones.
func shouldMint(deps SharedDeps, t ServerTarget) bool {
	return deps.Minter != nil && t.SSHHost != "" && t.SSHPort > 0 && t.IsSecure()
}

// startWatcherGroup builds a per-server HostClient and runs the SSH/AWS/Docker
// handlers (each only when its backend is present) under a child context. The
// SDK's Subscribe reconnects on its own, so an offline server simply retries in
// the background until the group is cancelled.
func startWatcherGroup(parent context.Context, t ServerTarget, deps SharedDeps) *watcherGroup {
	ctx, cancel := context.WithCancel(parent)
	log := deps.Logger.With("server", t.Name, "url", t.URL)

	// A SECURE server (reached over an https api_url) authenticates with a
	// self-minted, auto-refreshing credentials token: the mint happens lazily on
	// the first request (off this lock), re-mints near expiry / on a 401, and a
	// host-key pin mismatch fails closed. An OPEN-mode server (plain http) needs no
	// token and uses its (usually empty) configured static token. We must NOT mint
	// against an open server: shed-server's _bootstrap handler refuses unless SSH
	// enforce is on, so the mint fails and would log a WARN on every bus reconnect.
	// shouldMint encodes that decision.
	opts := []sdk.HostClientOption{
		sdk.WithServerURL(t.URL),
		sdk.WithLogger(log),
		sdk.WithTLSPin(t.TLSFingerprint),
	}
	var credSrc *credentialSource
	if shouldMint(deps, t) {
		credSrc = newCredentialSource(ctx, deps.Minter, t, scopeCredentials)
		opts = append(opts, sdk.WithTokenProvider(credSrc))
	} else {
		opts = append(opts, sdk.WithToken(t.Token))
	}
	client := sdk.NewHostClient(opts...)

	var wg sync.WaitGroup
	run := func(fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
		}()
	}

	// Proactively re-mint the credentials token (jittered) so an idle server's
	// token stays fresh and a reconnect never pays the mint latency inline.
	if credSrc != nil {
		run(credSrc.refreshLoop)
	}

	run(NewSSHHandler(deps.SSHBackend, client, deps.SSHApproval, deps.Audit, t.Name, log).Run)
	if deps.AWSBackend != nil {
		run(NewAWSHandler(deps.AWSBackend, client, deps.AWSApproval, deps.Audit, t.Name, log).Run)
	}
	if deps.DockerBackend != nil {
		run(NewDockerHandler(deps.DockerBackend, client, deps.DockerApproval, deps.Audit, t.Name, log).Run)
	}
	// Egress-audit fanout: stream this server's egress decisions into the audit
	// log + desktop feed (read-only; reconnects on its own, backing off hard when
	// egress is disabled). The stream route is CONTROL-scoped, so a secure server
	// gets its own self-minted control token (the bus token is credentials-scoped);
	// an open server sends none.
	var egressTokens tokenSource
	if shouldMint(deps, t) {
		egressTokens = newCredentialSource(ctx, deps.Minter, t, scopeControl)
	}
	run(NewEgressSubscriber(t, egressTokens, deps.Audit, log).Run)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	log.Info("watching server")
	return &watcherGroup{target: t, client: client, cancel: cancel, done: done}
}

// Supervisor reconciles the running per-server watcher groups against a desired
// set of servers. Safe for concurrent Reconcile/Shutdown.
type Supervisor struct {
	parent context.Context
	deps   SharedDeps
	logger *slog.Logger

	// newGroup is the group factory; overridable in tests.
	newGroup func(context.Context, ServerTarget, SharedDeps) *watcherGroup

	mu       sync.Mutex
	groups   map[string]*watcherGroup
	closed   bool
	wasEmpty bool
}

// NewSupervisor creates a supervisor bound to a parent context and shared deps.
func NewSupervisor(ctx context.Context, deps SharedDeps) *Supervisor {
	return &Supervisor{
		parent:   ctx,
		deps:     deps,
		logger:   deps.Logger,
		newGroup: startWatcherGroup,
		groups:   make(map[string]*watcherGroup),
	}
}

// Reconcile diffs the desired servers against the running groups: it stops
// groups whose name is gone or whose URL changed, then starts groups for new
// names. Unchanged (same name+URL) groups are left running, so a no-op config
// rewrite causes no churn. Idempotent; a Reconcile after Shutdown is a no-op.
func (s *Supervisor) Reconcile(desired []ServerTarget) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}

	want := make(map[string]ServerTarget, len(desired))
	for _, t := range desired {
		want[t.Name] = t
	}

	// Stop removed or changed groups. Compare the whole target (URL, Token,
	// TLSFingerprint), not just URL: a rotated/added credentials token or TLS
	// pin for the same URL must restart the watcher so it takes effect, rather
	// than silently keeping the stale value until process restart (an HTTPS
	// target that newly gains a pin would otherwise stay unpinned). Name is the
	// map key, so the struct compare is exactly the credential-bearing fields.
	// Cancel under the lock (fast); drain after releasing it so a slow handler
	// can't block other reconciles.
	var draining []*watcherGroup
	for name, g := range s.groups {
		if t, ok := want[name]; !ok || t != g.target {
			s.logger.Info("stopping server watcher", "server", name, "url", g.target.URL)
			g.cancel()
			draining = append(draining, g)
			delete(s.groups, name)
		}
	}

	// Start new (or restarted-on-change) groups.
	for name, t := range want {
		if _, ok := s.groups[name]; !ok {
			s.groups[name] = s.newGroup(s.parent, t, s.deps)
		}
	}

	// Warn only when the desired set newly becomes empty (wasEmpty starts
	// false, so the first reconcile with an empty set warns once).
	empty := len(s.groups) == 0
	warnEmpty := empty && !s.wasEmpty
	s.wasEmpty = empty
	s.mu.Unlock()

	for _, g := range draining {
		<-g.done
	}
	if warnEmpty {
		s.logger.Warn("no servers to watch (discovery returned an empty set)")
	}
}

// Shutdown cancels all groups and waits for them to drain. After Shutdown,
// further Reconcile calls are no-ops.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()
	s.closed = true
	groups := s.groups
	s.groups = make(map[string]*watcherGroup)
	for _, g := range groups {
		g.cancel()
	}
	s.mu.Unlock()

	for _, g := range groups {
		<-g.done
	}
}

// Health returns the running daemon's per-server connection snapshot for
// `status`: each watched server with its per-namespace SDK subscription
// state. Sorted by name. The supervisor lock is released before calling into
// each client so a slow Status() can't stall reconciles.
func (s *Supervisor) Health() []ServerHealth {
	s.mu.Lock()
	type ref struct {
		name, url string
		client    *sdk.HostClient
	}
	refs := make([]ref, 0, len(s.groups))
	for _, g := range s.groups {
		refs = append(refs, ref{g.target.Name, g.target.URL, g.client})
	}
	s.mu.Unlock()

	out := make([]ServerHealth, 0, len(refs))
	for _, r := range refs {
		sh := ServerHealth{Name: r.name, URL: r.url}
		if r.client != nil {
			for _, st := range r.client.Status() {
				sh.Namespaces = append(sh.Namespaces, NamespaceHealth{
					Namespace: st.Namespace,
					State:     st.State,
					LastError: st.LastError,
					Since:     st.Since.UTC().Format(time.RFC3339),
				})
			}
		}
		out = append(out, sh)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
