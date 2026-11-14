package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote"
	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
	"github.com/charliek/shed/internal/remote/provider/gce"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/locallink"
	"github.com/charliek/shed/internal/remote/transport/sshlink"
)

// backendReadyTimeout is how long start-up waits for the remote backend's
// first complete list of the cloud before giving up. The list is retried for
// that long; a server that cannot list its environment must not serve shed
// routes, because it cannot tell which sheds exist.
const backendReadyTimeout = 2 * time.Minute

// keepFailedCreateEnv, when set to "1", leaves the VM of a failed create in
// place for inspection instead of destroying it. Deleting the shed removes
// it.
const keepFailedCreateEnv = "SHED_REMOTE_KEEP_FAILED_CREATE"

// newRemoteBackend builds the remote backend from the server config and the
// process environment: the provider (gce, or the in-memory fake), the link
// factory (ssh with the server's client key, or the local test link) and the
// record cache. It makes no cloud call; the caller runs Reconcile.
func newRemoteBackend(cfg *config.ServerConfig, bridge *plugin.Bridge) (*remote.Backend, error) {
	rcfg := cfg.Remote
	if rcfg == nil {
		return nil, fmt.Errorf("remote backend enabled but the remote config block is missing")
	}
	// The one place the environment is read: project, zone, network, the
	// credentials and the SSH client key. Errors name the variable.
	rt, err := rcfg.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("remote backend: %w", err)
	}
	stateRoot := serverStateDir(cfg)
	firewallSources := rt.FirewallSources

	var prov provider.Provider
	switch rcfg.Provider {
	case config.RemoteProviderGCE:
		creds, err := gce.ParseCredentials(rt.Credentials)
		if err != nil {
			return nil, fmt.Errorf("remote backend: service account credentials: %w", err)
		}
		client, err := gce.New(gce.Config{
			Project: rt.Project,
			Zone:    rt.Zone,
			Env:     rcfg.Environment,
			Network: rt.Network,
			Subnet:  rt.Subnet,
			Sizes:   rcfg.SizeList(),
			Token:   creds,
		})
		if err != nil {
			return nil, fmt.Errorf("remote backend: gce provider: %w", err)
		}
		prov = client
	case config.RemoteProviderFake:
		// The fake's firewall rule carries whatever static mode expects, so
		// the check passes; with no ranges given it is the fake's own
		// documentation address.
		if rcfg.Firewall.Mode == config.RemoteFirewallStatic && len(firewallSources) == 0 {
			firewallSources = []string{fake.DefaultFirewallSource}
		}
		prov = fake.New(fake.Options{Env: rcfg.Environment, FirewallSources: firewallSources})
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
		Config:           rcfg,
		Server:           cfg,
		Provider:         prov,
		Links:            links,
		StateDir:         rcfg.StateDirPath(stateRoot),
		AuthorizedKeys:   authorizedKeys,
		FirewallSources:  firewallSources,
		Bridge:           bridge,
		KeepFailedCreate: os.Getenv(keepFailedCreateEnv) == "1",
	})
}
