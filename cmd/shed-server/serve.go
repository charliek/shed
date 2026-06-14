package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/firecracker"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/sshd"
	"github.com/charliek/shed/internal/vz"
)

const (
	// shutdownTimeout is the maximum time to wait for graceful shutdown
	shutdownTimeout = 30 * time.Second

	// HTTP server timeouts. WriteTimeout is intentionally omitted (0):
	// SSE streams (create/pull progress and the plugin bus) write for
	// arbitrarily long, and a WriteTimeout would kill them mid-stream.
	// ReadHeaderTimeout/ReadTimeout bound slowloris-style header/body
	// reads; IdleTimeout reaps idle keep-alive conns (it does not apply
	// during an active SSE response).
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 60 * time.Second
	httpIdleTimeout       = 120 * time.Second

	// tokenSweepInterval is how often expired HTTP bearer tokens are reaped
	// from the in-memory store (expired tokens are also dropped on validate).
	tokenSweepInterval = 10 * time.Minute

	// defaultTokenTTL is the lifetime of a bootstrap-minted HTTP token until
	// auth.token_ttl is wired in (sub-step 1d).
	defaultTokenTTL = 24 * time.Hour
)

// newHTTPServer builds an http.Server with the shared, SSE-safe timeouts.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}

// defaultHostKeyPath returns the default path for the SSH host key.
// Uses /etc/shed/host_key when running as root (Linux servers), otherwise
// falls back to ~/.shed/host_key (macOS development without sudo).
func defaultHostKeyPath() string {
	if os.Getuid() == 0 {
		return "/etc/shed/host_key"
	}
	return config.ExpandPath("~/.shed/host_key")
}

// tlsCertPaths returns the cert + key file paths for the HTTPS listener,
// honoring tls_cert_file / tls_key_file and otherwise placing them in stateDir
// (the directory that holds the SSH host key: /etc/shed as root, ~/.shed
// otherwise).
func tlsCertPaths(cfg *config.ServerConfig, stateDir string) (certPath, keyPath string) {
	certPath = filepath.Join(stateDir, "tls_cert.pem")
	if cfg.TLSCertFile != "" {
		certPath = config.ExpandPath(cfg.TLSCertFile)
	}
	keyPath = filepath.Join(stateDir, "tls_key.pem")
	if cfg.TLSKeyFile != "" {
		keyPath = config.ExpandPath(cfg.TLSKeyFile)
	}
	return certPath, keyPath
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the shed server",
	Long:  `Start the shed server which provides HTTP API and SSH access to development environments.`,
	RunE:  runServe,
}

func runServe(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Public-exposure preflight: refuse to start an internet-facing deployment
	// without the full security bundle. Runs before anything binds; inert
	// unless public_exposure is set.
	if err := cfg.PreflightPublicExposure(); err != nil {
		return fmt.Errorf("public_exposure preflight: %w", err)
	}

	log.Printf("Starting shed-server...")
	log.Printf("HTTP listen: %s", cfg.HTTPListenAddr())
	log.Printf("SSH listen: %s", cfg.SSHListenAddr())

	// Initialize plugin system (before backends, since backends use the bridge)
	pluginRegistry := plugin.NewRegistry()
	// Track pending requests only when HTTP auth is enforced — the /respond
	// ownership gate is consulted only then, so an unauthenticated server does
	// no bookkeeping.
	if h := cfg.HTTPAuth(); h != nil && h.Mode == config.HTTPAuthEnforce {
		pluginRegistry.EnableOwnershipTracking()
	}
	pluginBridge := plugin.NewBridge(pluginRegistry)

	// HTTP bearer-token store: short-lived, scoped tokens minted over the SSH
	// bootstrap channel and validated by the auth middleware. In-memory by
	// design — a restart drops tokens and clients transparently re-mint over
	// SSH. Shared with the SSH server (bootstrap mint) and the API server
	// (validate).
	tokenStore := authtoken.NewStore()
	tokenCtx, cancelTokens := context.WithCancel(context.Background())
	defer cancelTokens()
	tokenStore.StartSweeper(tokenCtx, tokenSweepInterval)

	// Initialize the configured backend. egressSocketDir + attachEgress are
	// captured per-backend so the egress proxy (constructed once, below) can
	// place its control socket next to the active backend's sockets and hand
	// the manager to the right client.
	var be backend.Backend
	var egressSocketDir string // runtime dir for the control socket
	var egressStateDir string  // persistent dir for the durable audit log
	var attachEgress func(*egress.Manager)
	switch cfg.DefaultBackend {
	case config.BackendFirecracker:
		fcCfg := cfg.Firecracker
		if fcCfg == nil {
			fcCfg = config.DefaultFirecrackerConfig()
		}
		fcClient, err := firecracker.NewClient(fcCfg, cfg, pluginBridge)
		if err != nil {
			return fmt.Errorf("failed to create firecracker client: %w", err)
		}
		log.Printf("Initialized Firecracker backend (default_image=%q, pull_policy=%s)", fcCfg.DefaultImage, fcCfg.PullPolicy)
		be = firecracker.NewBackend(fcClient)
		egressSocketDir = fcCfg.SocketDir
		egressStateDir = filepath.Dir(fcCfg.InstanceDir)
		attachEgress = fcClient.SetEgressManager

	case config.BackendVZ:
		vzCfg := cfg.VZ
		if vzCfg == nil {
			return fmt.Errorf("vz backend enabled but vz config is missing")
		}
		vzClient, err := vz.NewClient(vzCfg, cfg, pluginBridge)
		if err != nil {
			return fmt.Errorf("failed to create vz client: %w", err)
		}
		log.Printf("Initialized VZ backend (default_image=%q, pull_policy=%s)", vzCfg.DefaultImage, vzCfg.PullPolicy)
		be = vz.NewBackend(vzClient)
		egressSocketDir = vzCfg.SocketDir
		egressStateDir = filepath.Dir(vzCfg.InstanceDir)
		attachEgress = vzClient.SetEgressManager

	default:
		return fmt.Errorf("unsupported backend type: %s", cfg.DefaultBackend)
	}
	defer be.Close()

	// Optional egress-control proxy child, started only when enabled. A start
	// failure is a hard startup error — never a silent disable (AC-11).
	var egressMgr *egress.Manager
	var egressAudit *egress.AuditLog
	if cfg.Egress != nil && cfg.Egress.Enabled {
		lo, hi := cfg.Egress.PortRangeBounds()
		sockPath := filepath.Join(egressSocketDir, "egress-proxy.sock")
		proxyBin, err := resolveEgressProxyBin(egressProxyBinDir())
		if err != nil {
			return fmt.Errorf("egress enabled: %w", err)
		}
		auditPath := filepath.Join(egressStateDir, "egress-audit.jsonl")
		egressAudit, err = egress.OpenAuditLog(auditPath, 0)
		if err != nil {
			return fmt.Errorf("egress: open audit log %s: %w", auditPath, err)
		}
		// Durable JSONL + recent-ring for `shed egress show`, plus a denial line
		// in the server log for at-a-glance observability.
		onAudit := func(rec egress.AuditRecord) {
			egressAudit.Record(rec)
			logEgressAudit(rec)
		}
		egressMgr, err = egress.StartManager(sockPath, proxyBin, lo, hi, onAudit)
		if err != nil {
			return fmt.Errorf("egress: start proxy: %w", err)
		}
		attachEgress(egressMgr)
		log.Printf("Initialized egress-control proxy (ports %d-%d, socket %s, audit %s)", lo, hi, sockPath, auditPath)
	}
	defer func() {
		if egressMgr != nil {
			_ = egressMgr.Close()
		}
		if egressAudit != nil {
			_ = egressAudit.Close()
		}
	}()

	// Build the SSH key allowlist (default off = accept all). GitHub-seeded
	// keys are cached next to the host key. Fails closed (returns an error)
	// when mode=enforce but no keys resolved.
	hostKeyPath := defaultHostKeyPath()
	githubCacheDir := filepath.Join(filepath.Dir(hostKeyPath), "github_keys")
	sshAllowlist, err := sshd.NewKeyAllowlist(cfg.SSHAuth(), githubCacheDir)
	if err != nil {
		return fmt.Errorf("ssh auth: %w", err)
	}
	log.Printf("SSH auth mode: %s", sshAllowlist.Mode())

	// Refresh GitHub-seeded keys in the background until shutdown.
	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	defer cancelRefresh()
	sshAllowlist.StartRefresh(refreshCtx)

	// Initialize SSH server
	sshServer, err := sshd.NewServer(be, hostKeyPath, cfg.SSHListenAddr(), cfg.Terminal, sshAllowlist)
	if err != nil {
		return fmt.Errorf("failed to create SSH server: %w", err)
	}
	hostKey := sshServer.GetHostPublicKey()

	// Initialize HTTP API server
	apiServer := api.NewServer(be, cfg, hostKey, pluginRegistry, pluginBridge)
	apiServer.SetEgressAudit(egressAudit) // nil-safe: no-op when egress disabled
	apiServer.SetTokenStore(tokenStore)   // shared with the SSH bootstrap handler (1c)

	// Public router (HTTP and, when enabled, HTTPS share one handler). When
	// the internal-listener split is enabled, it omits the credential bus +
	// Connect tunnel.
	publicHandler := apiServer.Router()
	httpServer := newHTTPServer(cfg.HTTPListenAddr(), publicHandler)

	// Optional internal (loopback) listener carrying the credential bus +
	// Connect tunnel, kept off the public interface.
	var internalServer *http.Server
	if addr := cfg.InternalHTTPListenAddr(); addr != "" {
		internalServer = newHTTPServer(addr, apiServer.InternalRouter())
	}

	// Optional HTTPS listener: same public router over TLS with a self-signed,
	// client-pinned cert. Off by default (https_port unset).
	var httpsServer *http.Server
	var tlsFingerprint string
	if cfg.HTTPSEnabled() {
		certPath, keyPath := tlsCertPaths(cfg, filepath.Dir(hostKeyPath))
		cert, der, err := servertls.LoadOrGenerate(certPath, keyPath, cfg.TLSNames)
		if err != nil {
			return fmt.Errorf("tls cert: %w", err)
		}
		tlsFingerprint = servertls.Fingerprint(der)
		log.Printf("TLS cert fingerprint: %s", tlsFingerprint)
		httpsServer = newHTTPServer(cfg.HTTPSListenAddr(), publicHandler)
		httpsServer.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	// Wire the SSH bootstrap handler: it mints HTTP tokens into the shared
	// store and returns them (with the TLS pin + ports) over the authenticated
	// SSH channel, so `shed server add` needs no manual token/pin step.
	sshServer.SetBootstrap(tokenStore, sshd.BootstrapInfo{
		HTTPPort:       cfg.HTTPPort,
		HTTPSPort:      cfg.HTTPSPort,
		TLSFingerprint: tlsFingerprint,
		TokenTTL:       defaultTokenTTL,
	})

	// Channel to collect errors from servers (HTTP, HTTPS, SSH, internal HTTP).
	errChan := make(chan error, 4)

	// Start HTTP server in goroutine
	go func() {
		log.Printf("HTTP server listening on %s", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("HTTP server error: %w", err)
		}
	}()

	// Start internal HTTP server in goroutine (when split enabled)
	if internalServer != nil {
		go func() {
			log.Printf("Internal HTTP server (bus+connect) listening on %s", internalServer.Addr)
			if err := internalServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errChan <- fmt.Errorf("internal HTTP server error: %w", err)
			}
		}()
	}

	// Start HTTPS server in goroutine (when https_port set). The cert + key
	// are already loaded into TLSConfig, so the file args are empty.
	if httpsServer != nil {
		go func() {
			log.Printf("HTTPS server listening on %s", httpsServer.Addr)
			if err := httpsServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				errChan <- fmt.Errorf("HTTPS server error: %w", err)
			}
		}()
	}

	// Start SSH server in goroutine
	go func() {
		if err := sshServer.Start(); err != nil {
			errChan <- fmt.Errorf("SSH server error: %w", err)
		}
	}()

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("Shed server is ready")

	// Wait for signal or error
	select {
	case sig := <-sigChan:
		log.Printf("Received signal %v, initiating graceful shutdown...", sig)
	case err := <-errChan:
		log.Printf("Server error: %v", err)
		return err
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Shutdown HTTP server
	log.Printf("Shutting down HTTP server...")
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	// Shutdown internal HTTP server (when enabled)
	if internalServer != nil {
		log.Printf("Shutting down internal HTTP server...")
		if err := internalServer.Shutdown(ctx); err != nil {
			log.Printf("internal HTTP server shutdown error: %v", err)
		}
	}

	// Shutdown HTTPS server (when enabled)
	if httpsServer != nil {
		log.Printf("Shutting down HTTPS server...")
		if err := httpsServer.Shutdown(ctx); err != nil {
			log.Printf("HTTPS server shutdown error: %v", err)
		}
	}

	// Shutdown SSH server
	log.Printf("Shutting down SSH server...")
	if err := sshServer.Shutdown(ctx); err != nil {
		log.Printf("SSH server shutdown error: %v", err)
	}

	log.Printf("Shutdown complete")
	return nil
}

// loadConfig loads the server configuration from the specified path or default locations.
func loadConfig() (*config.ServerConfig, error) {
	if configPath != "" {
		return config.LoadServerConfigFromPath(configPath)
	}
	return config.LoadServerConfig()
}
