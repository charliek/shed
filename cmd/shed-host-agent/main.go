// shed-host-agent is the host-side daemon that handles credential operations
// for shed microVMs. It subscribes to shed-server's plugin message bus and
// performs SSH signing and AWS credential vending using the developer's local
// credentials.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/ext/protocol"
	"github.com/charliek/shed/internal/version"
)

// desktopGateNamespaces lists the credential namespaces whose approval policy is
// shed-desktop — advertised to the app so it shows the matching approval UI.
func desktopGateNamespaces(cfg Config) []string {
	var ns []string
	if cfg.SSH.Approval.EffectivePolicy() == PolicyShedDesktop {
		ns = append(ns, protocol.NamespaceSSHAgent)
	}
	if cfg.AWS.Approval.EffectivePolicy() == PolicyShedDesktop {
		ns = append(ns, protocol.NamespaceAWSCredentials)
	}
	if cfg.Docker.Approval.EffectivePolicy() == PolicyShedDesktop {
		ns = append(ns, protocol.NamespaceDockerCredentials)
	}
	return ns
}

func main() {
	configPath := flag.String("config", "~/.config/shed/extensions.yaml", "Path to config file")
	logFile := flag.String("log-file", "", "Write the operational log to this file (rotated); empty = stderr")
	flag.Parse()

	// Handle version subcommand
	if flag.NArg() > 0 && flag.Arg(0) == "version" {
		fmt.Println(version.FullInfo())
		os.Exit(0)
	}

	// Handle status subcommand: query the running daemon over its read-only
	// status socket and print its authoritative self-report. It does not read
	// the config file — the daemon owns the truth about what it loaded.
	if flag.NArg() > 0 && flag.Arg(0) == "status" {
		jsonOut := false
		for _, a := range flag.Args()[1:] {
			switch a {
			case "--json", "-json":
				jsonOut = true
			case "--live", "-live":
				fmt.Fprintln(os.Stderr, "status: --live was removed; `status` now always queries the running agent")
				os.Exit(2)
			default:
				fmt.Fprintf(os.Stderr, "status: unknown argument %q\n", a)
				os.Exit(2)
			}
		}
		os.Exit(runStatus(jsonOut, os.Stdout))
	}

	logWriter := newLogWriter(*logFile)
	if c, ok := logWriter.(io.Closer); ok {
		defer c.Close()
	}
	logger := slog.New(slog.NewTextHandler(logWriter, nil))
	slog.SetDefault(logger)

	logger.Info("starting shed-host-agent", "version", version.FullInfo())

	startedAt := time.Now().UTC().Format(time.RFC3339)

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		logger.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}
	// The absolute config path the daemon actually loaded, surfaced verbatim in
	// `status` so a user can confirm which file is in effect (the original #29
	// confusion). Best-effort: fall back to the tilde-expanded path.
	resolvedConfigPath := expandTilde(*configPath)
	if abs, err := filepath.Abs(resolvedConfigPath); err == nil {
		resolvedConfigPath = abs
	}
	// approval_timeout was validated in LoadConfig; the duration is reusable here.
	approvalTimeout, _ := cfg.ApprovalTimeoutDuration()

	// Initialize audit logger (also fans entries out to the desktop channel)
	audit := NewAuditLogger(cfg.Logging, logger)
	defer audit.Close()

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-sigCh
		logger.Info("shutting down")
		cancel()
	}()

	// Initialize SSH backend
	sshBackend, err := ResolveSSHBackend(cfg.SSH, logger)
	if err != nil {
		logger.Error("failed to initialize SSH backend", "error", err)
		os.Exit(1)
	}

	var wg sync.WaitGroup

	// Approval channel: always on. The agent serves a local UDS at a fixed path
	// (sockets.go) that a single consumer — normally the shed-desktop app — uses
	// for the all-namespace audit/event stream and shed-desktop-policy approval
	// decisions. It's the program's public interface, so it's not gated on
	// config; with no consumer connected, delegated approvals fail closed.
	// Credentials minter: for a secure (https) server the agent mints its own
	// credentials token over the server's SSH _bootstrap channel via the system ssh
	// client, whose host key is verified against the pin shed already wrote to
	// ~/.shed/known_hosts. The SSH identity is resolved by ssh (agent / Keychain /
	// IdentityAgent / ~/.ssh/config), so no key file is read here. An open-mode
	// (http) server needs no token and keeps its (empty) configured token — see
	// shouldMint. The same minter backs the desktop's token.get (control tokens),
	// so build it before the desktop.
	minter := NewCredentialMinter("~/.shed/known_hosts")

	// token.get resolves servers from the shed CLI config (the discovery source, or
	// its default in single-server mode) and mints CONTROL tokens for any allowlisted
	// server there — broad minting, not limited to the servers the agent brokers.
	configSource := DefaultDiscoverySource
	if cfg.Discovery != nil {
		configSource = cfg.Discovery.Source // already defaulted + tilde-expanded by applyDefaults
	}

	gateNamespaces := desktopGateNamespaces(cfg)
	desktop := NewDesktopServer(desktopSocketPath(), approvalTimeout, audit, version.FullInfo(), gateNamespaces, logger)
	desktop.SetControlTokens(newControlTokenProvider(ctx, minter, configSource)) // before Listen
	wg.Add(1)
	go func() {
		defer wg.Done()
		desktop.Listen(ctx)
	}()

	// Build the per-extension approval gates now that the desktop channel (if
	// any) exists. An empty/omitted policy resolves to deny-all (fail closed).
	sshApproval := gateFor(protocol.NamespaceSSHAgent, protocol.SSHOpSign, cfg.SSH.Approval, desktop)
	awsApproval := gateFor(protocol.NamespaceAWSCredentials, protocol.AWSOpGetCredentials, cfg.AWS.Approval, desktop)
	dockerApproval := gateFor(protocol.NamespaceDockerCredentials, protocol.DockerOpGet, cfg.Docker.Approval, desktop)
	logger.Info("approval policies",
		"ssh", cfg.SSH.Approval.EffectivePolicy(),
		"aws", cfg.AWS.Approval.EffectivePolicy(),
		"docker", cfg.Docker.Approval.EffectivePolicy())

	// Build the server-agnostic backends once; per-server handlers share them.
	// AWS and Docker are optional — a missing/unconfigured backend stays nil and
	// its handler simply isn't started for any server.
	var awsBackend AWSBackend
	if b, err := NewSTSBackend(cfg.AWS, logger); err != nil {
		logger.Warn("AWS handler disabled", "error", err)
	} else {
		awsBackend = b
	}

	var dockerBackend DockerBackend
	if b, err := NewDockerBackend(cfg.Docker, logger); err != nil {
		logger.Warn("Docker handler disabled", "error", err)
	} else {
		dockerBackend = b
	}

	deps := SharedDeps{
		SSHBackend:     sshBackend,
		AWSBackend:     awsBackend,
		DockerBackend:  dockerBackend,
		SSHApproval:    sshApproval,
		AWSApproval:    awsApproval,
		DockerApproval: dockerApproval,
		Audit:          audit,
		Logger:         logger,
		Minter:         minter,
	}
	sup := NewSupervisor(ctx, deps)

	// Serve the read-only status socket so `shed-host-agent status` can query
	// this daemon's authoritative self-report (config + policies + per-server
	// connection state).
	statusSock := statusSocketPath()
	wg.Add(1)
	go func() {
		defer wg.Done()
		serveStatusSocket(ctx, statusSock, func() LiveStatus {
			return buildLiveStatus(sup, desktop, cfg, resolvedConfigPath, startedAt, version.FullInfo())
		}, logger)
	}()

	if cfg.Discovery != nil && cfg.Server != "" && cfg.Server != "http://localhost:8080" {
		logger.Warn("`server:` is ignored when `discovery:` is configured", "server", cfg.Server)
	}

	// computeDesired resolves the set of servers to broker for. The bool is
	// false only on a discovery read error, in which case the caller skips the
	// reconcile and keeps the current servers (rather than tearing them all
	// down on a transient/partial-write read). Shares resolveTargets with
	// `status` so the two never disagree about which servers are watched.
	computeDesired := func() ([]ServerTarget, bool) {
		targets, err := resolveTargets(cfg)
		if err != nil {
			logger.Warn("discovery read failed; keeping current servers",
				"source", cfg.Discovery.Source, "error", err)
			return nil, false
		}
		return targets, true
	}
	reconcile := func() {
		if desired, ok := computeDesired(); ok {
			sup.Reconcile(desired)
		}
	}

	// One path for both modes. Legacy single-server is just a discovery config
	// that reconciles once and never reloads (watch: off); computeDesired
	// already returns the single target when cfg.Discovery is nil.
	watchCfg := DiscoveryConfig{Watch: "off"}
	if cfg.Discovery != nil {
		watchCfg = *cfg.Discovery
		logger.Info("multi-server discovery enabled", "source", watchCfg.Source, "watch", watchCfg.Watch)
	} else {
		logger.Info("brokering for single server", "server", cfg.Server)
	}
	runWatchLoop(ctx, watchCfg, reconcile, logger)

	logger.Info("stopping server watchers")
	sup.Shutdown()

	wg.Wait()
	logger.Info("stopped")
}
