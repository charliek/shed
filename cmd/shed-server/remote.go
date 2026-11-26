package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/provider/service"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/locallink"
	"github.com/charliek/shed/internal/remote/transport/sshlink"
)

// backendReadyTimeout is how long start-up waits for the remote backend's
// first complete list of the machines. Past it the server starts degraded:
// it serves its records, keeps existing links working and refuses creates,
// starts, stops, deletes and extends with 503 COMPUTE_UNAVAILABLE, and the
// start-up pass keeps retrying until the account service answers.
const backendReadyTimeout = 2 * time.Minute

// newRemoteBackend builds the remote backend from the server config and the
// process environment: the provider (the account service's compute, over
// the directory runtime's session, or the in-memory fake), the link factory
// (ssh with the server's client key, or the local test link) and the record
// cache. It makes no call to the service; the caller runs Reconcile.
func newRemoteBackend(cfg *config.ServerConfig, bridge *plugin.Bridge, session service.Session) (*remote.Backend, error) {
	rcfg := cfg.Remote
	if rcfg == nil {
		return nil, fmt.Errorf("remote backend enabled but the remote config block is missing")
	}
	// The one place the environment is read: the SSH client key and the
	// firewall's sources. Errors name the variable.
	rt, err := rcfg.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("remote backend: %w", err)
	}
	stateRoot := serverStateDir(cfg)
	firewallSources := rt.FirewallSources

	var prov provider.Provider
	switch rcfg.Provider {
	case config.RemoteProviderService:
		if session == nil {
			return nil, fmt.Errorf("remote backend: provider: %s needs auth.directory (the account service it gets machines from)", config.RemoteProviderService)
		}
		client, err := service.New(service.Config{Session: session, TestDrivers: rcfg.AllowTestDrivers})
		if err != nil {
			return nil, fmt.Errorf("remote backend: %w", err)
		}
		prov = client
	case config.RemoteProviderFake:
		// The fake's ingress carries whatever static mode expects, so the
		// check passes; with no ranges given it is the fake's own
		// documentation address.
		if rcfg.Firewall.Mode == config.RemoteFirewallStatic && len(firewallSources) == 0 {
			firewallSources = []string{fake.DefaultIngressSource}
		}
		prov = fake.New(fake.Options{IngressSources: firewallSources})
	default:
		return nil, fmt.Errorf("remote backend: unknown provider %q", rcfg.Provider)
	}

	var links transport.Factory
	var authorizedKeys []string
	switch rcfg.Transport {
	case config.RemoteTransportSSH:
		signer, err := sshlink.ParseClientKey(rt.SSHClientKey)
		if err != nil {
			return nil, fmt.Errorf("remote backend: %w", err)
		}
		factory, err := sshlink.New(sshlink.Config{Keys: []ssh.Signer{signer}})
		if err != nil {
			return nil, fmt.Errorf("remote backend: ssh transport: %w", err)
		}
		links = factory
		authorizedKeys = []string{sshlink.AuthorizedKey(signer)}
	case config.RemoteTransportLocal:
		agentPath := filepath.Join(rcfg.AgentDirPath(stateRoot), transport.AgentBinaryName(runtime.GOARCH))
		factory, err := locallink.New(locallink.Config{AgentPath: agentPath})
		if err != nil {
			return nil, fmt.Errorf("remote backend: local transport: %w", err)
		}
		links = factory
	default:
		return nil, fmt.Errorf("remote backend: unknown transport %q", rcfg.Transport)
	}

	return remote.New(remote.Options{
		Config:          rcfg,
		Server:          cfg,
		Provider:        prov,
		Links:           links,
		StateDir:        rcfg.StateDirPath(stateRoot),
		AuthorizedKeys:  authorizedKeys,
		FirewallSources: firewallSources,
		Bridge:          bridge,
	})
}
