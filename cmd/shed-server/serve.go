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

	// caExpiryWarnWindow is how much remaining client-CA lifetime triggers a
	// startup warning. Rotation is a manual, fleet-wide re-enrollment, so the
	// notice has to arrive long before issuance actually stops.
	caExpiryWarnWindow = 90 * 24 * time.Hour
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

// buildHTTPSTLSConfig assembles the tls.Config the HTTPS listener runs with.
// It is the ONE place the client-authentication posture is decided, extracted
// from runServe so a test can assert on the real production assembly rather
// than on a hand-built lookalike — if this function stops requiring client
// certificates in mtls mode, TestBuildHTTPSTLSConfig fails.
//
// clientCA is non-nil only in mtls mode (runServe loads it there and nowhere
// else, and aborts startup if the load fails); authorized is the live
// SSH-allowlist predicate. In token and open mode the result is a plain
// server-auth-only config: no ClientAuth, no ClientCAs.
//
// Both conditions are required deliberately, and the belt-and-braces costs
// nothing: an mtls server that somehow reached here with no CA would produce a
// listener that asks for no certificate — but it would not be open, because the
// API middleware gates on cfg.MTLSMode() alone and refuses every request that
// arrives without one.
//
// mtls adds three things: trust only the internal CA, require a client
// certificate that verifies against it, and refuse the handshake outright when
// the identity that certificate names has left the SSH allowlist.
//
// The allowlist check is the FIRST of two, not the only one. A TLS peer's
// identity is established at the handshake and then held for the life of the
// connection, so on a pooled keep-alive connection neither expiry nor
// de-authorization would ever be noticed again — the API middleware re-derives
// both from the presented certificate on every request, and is the
// authoritative gate. Checking here as well means a de-authorized client is
// turned away at the door rather than allowed to open connections and collect
// 401s. It is installed as VerifyConnection specifically because crypto/tls
// skips VerifyPeerCertificate on RESUMED sessions (see servertls/clientauth.go).
func buildHTTPSTLSConfig(cfg *config.ServerConfig, cert tls.Certificate, clientCA *servertls.CA, authorized func(string) bool) *tls.Config {
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if cfg.MTLSMode() && clientCA != nil {
		tlsCfg.ClientCAs = clientCA.Pool()
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		tlsCfg.VerifyConnection = servertls.AllowlistConnectionVerifier(authorized)
	}
	return tlsCfg
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

// caCertPaths returns the client-CA cert + key file paths, alongside the TLS
// listener's own material in stateDir. It mirrors tlsCertPaths but takes no
// config override: the CA is server-internal (it exists only to sign the client
// certs this server then trusts), so there is no tls_cert_file-style knob for
// pointing it elsewhere.
func caCertPaths(stateDir string) (certPath, keyPath string) {
	return filepath.Join(stateDir, "ca_cert.pem"), filepath.Join(stateDir, "ca_key.pem")
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

	// Enforced-mode preflight: refuse to start `auth.mode: token` or `mtls`
	// without an SSH key source (an empty enforced allowlist would lock
	// everyone out). Runs before anything binds; inert in open mode.
	if err := cfg.PreflightAuth(); err != nil {
		return fmt.Errorf("auth preflight: %w", err)
	}

	// mtls authenticates clients at the TLS handshake, so the HTTPS listener IS
	// the enforcement point — there is no other listener a client could reach.
	// Config load defaults https_port in every enforced mode, so this is
	// unreachable through a validated config; it exists so a hand-built config
	// (a test, a tool) can never produce an mtls server with nothing enforcing.
	if cfg.MTLSMode() && !cfg.HTTPSEnabled() {
		return fmt.Errorf("auth.mode: mtls requires https_port (client certificates are verified at the TLS handshake)")
	}

	log.Printf("Starting shed-server...")
	if cfg.PlainHTTPEnabled() {
		log.Printf("HTTP listen: %s", cfg.HTTPListenAddr())
	}
	log.Printf("SSH listen: %s", cfg.SSHListenAddr())

	// Initialize plugin system (before backends, since backends use the bridge)
	pluginRegistry := plugin.NewRegistry()
	// Track pending requests only when auth is enforced (token or mtls) — the
	// /respond ownership gate is consulted only then, so an unauthenticated
	// open-mode server does no bookkeeping.
	if cfg.AuthEnforced() {
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
	var attachEgressStore func(*config.UserProfileStore)
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
		attachEgressStore = fcClient.SetEgressUserStore

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
		attachEgressStore = vzClient.SetEgressUserStore

	default:
		return fmt.Errorf("unsupported backend type: %s", cfg.DefaultBackend)
	}
	defer be.Close()

	// Optional egress-control proxy child, started only when enabled. A start
	// failure is a hard startup error — never a silent disable (AC-11).
	var egressMgr *egress.Manager
	var egressAudit *egress.AuditLog
	var egressUserStore *config.UserProfileStore
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
		// Register cleanup now — before the user-profile-store open below can
		// early-return, which would otherwise leak the proxy child + audit log.
		defer func() {
			if egressMgr != nil {
				_ = egressMgr.Close()
			}
			if egressAudit != nil {
				_ = egressAudit.Close()
			}
		}()

		// Runtime user-editable profile store (`shed egress profile ...`), in the
		// state dir next to the audit log. Fails startup on a malformed file.
		storeDir := filepath.Join(egressStateDir, "egress-profiles")
		egressUserStore, err = config.OpenUserProfileStore(storeDir)
		if err != nil {
			return fmt.Errorf("egress: open user profile store: %w", err)
		}
		// Config profiles are the read-only baseline: a user profile shadowed by a
		// same-named config profile resolves to config. Warn so the dead user file
		// is visible (the API rejects new colliding PUTs; this catches a config edit
		// that collides with a pre-existing user profile).
		for name := range cfg.Egress.Profiles {
			if _, ok := egressUserStore.Get(name); ok {
				log.Printf("egress: user profile %q is shadowed by a server-config profile of the same name (config wins)", name)
			}
		}
		attachEgressStore(egressUserStore)
		log.Printf("Initialized egress-control proxy (ports %d-%d, socket %s, audit %s, profiles %s)", lo, hi, sockPath, auditPath, storeDir)
	}

	// Build the SSH key allowlist (default off = accept all). GitHub-seeded
	// keys are cached next to the host key. Fails closed (returns an error)
	// when mode=enforce but no keys resolved.
	hostKeyPath := defaultHostKeyPath()
	githubCacheDir := filepath.Join(filepath.Dir(hostKeyPath), "github_keys")
	sshAllowlist, err := sshd.NewKeyAllowlist(cfg.EffectiveSSHAuth(), githubCacheDir)
	if err != nil {
		return fmt.Errorf("ssh auth: %w", err)
	}
	log.Printf("SSH auth mode: %s", sshAllowlist.Mode())

	// Revoke a key's HTTP tokens when it leaves the allowlist (removed from
	// github_users or the user's GitHub keys). Authoritative diff only — a failed
	// refetch falls back to last-known-good, so a transient outage never revokes.
	sshAllowlist.SetRevokeHook(func(subjects []string) {
		for _, s := range subjects {
			if n := tokenStore.RevokeBySubject(s); n > 0 {
				log.Printf("auth: revoked %d HTTP token(s) for removed key %s", n, s)
			}
		}
	})

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

	// Client CA (mtls mode only): the internal root that signs the short-lived
	// client certificates issued over the SSH bootstrap channel, and the trust
	// anchor the HTTPS listener verifies presented client certificates against.
	// Loaded — never silently regenerated — from the state dir.
	var clientCA *servertls.CA
	if cfg.MTLSMode() {
		caCertPath, caKeyPath := caCertPaths(filepath.Dir(hostKeyPath))
		ca, err := servertls.LoadOrGenerateCA(caCertPath, caKeyPath)
		if err != nil {
			return fmt.Errorf("client CA: %w", err)
		}
		clientCA = &ca
		notAfter := ca.NotAfter()
		log.Printf("Client CA fingerprint: %s (expires %s)", ca.Fingerprint(), notAfter.UTC().Format(time.RFC3339))
		// The CA is issued for a decade and is never rotated automatically —
		// rotating it invalidates every client certificate fleet-wide, so the
		// operator has to schedule it. Start saying so with a season's notice.
		if remaining := time.Until(notAfter); remaining < caExpiryWarnWindow {
			log.Printf("WARNING: client CA expires in %d days (%s) — rotate it (delete %s and %s, then re-enroll every client) before issuance stops.",
				int(remaining.Hours()/24), notAfter.UTC().Format(time.RFC3339), caCertPath, caKeyPath)
		}
	}

	// Initialize HTTP API server
	apiServer := api.NewServer(be, cfg, hostKey, pluginRegistry, pluginBridge)
	apiServer.SetEgressAudit(egressAudit)         // nil-safe: no-op when egress disabled
	apiServer.SetEgressUserStore(egressUserStore) // nil-safe: no-op when egress disabled
	apiServer.SetTokenStore(tokenStore)           // shared with the SSH bootstrap handler (1c)
	if clientCA != nil {
		// mtls: the middleware re-validates every request's client certificate
		// against the LIVE allowlist, so removing an SSH key de-authorizes its
		// certificates on the next request — the same "revocation lands
		// immediately" property the token store gets from RevokeBySubject.
		apiServer.SetClientCertAuthorizer(sshAllowlist.IsAuthorizedFingerprint)
		apiServer.SetClientCAInfo(clientCA.Fingerprint(), clientCA.NotAfter())
	}

	// Warn when bind_address is unset: as of v0.7.4 that defaults to loopback
	// (was all-interfaces), so a server that relied on the old default is now
	// local-only. This startup line is the only migration signal that reaches
	// every install channel (brew has no upgrade hook); the listeners log their
	// resolved addresses separately below.
	if cfg.BindAddress == "" {
		if cfg.PlainHTTPEnabled() {
			log.Printf("WARNING: bind_address is unset — binding loopback (127.0.0.1) only. This was all-interfaces before v0.7.4. To reach this open-mode server off-box, set bind_address (e.g. 0.0.0.0) + allow_insecure_exposure: true, or switch to auth.mode: token.")
		} else {
			log.Printf("bind_address is unset — binding loopback (127.0.0.1) only. Set bind_address (e.g. 0.0.0.0) to reach this token/mtls-mode server off-box.")
		}
	}

	// One router carries every route; the HTTP and (when enabled) HTTPS
	// listeners share this single handler.
	publicHandler := apiServer.Router()

	// Plain-HTTP listener. In an enforced mode (token or mtls) it is not
	// started — only the pinned-TLS (https_port) listener faces clients.
	var httpServer *http.Server
	if cfg.PlainHTTPEnabled() {
		httpServer = newHTTPServer(cfg.HTTPListenAddr(), publicHandler)
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
		httpsServer.TLSConfig = buildHTTPSTLSConfig(cfg, cert, clientCA, sshAllowlist.IsAuthorizedFingerprint)
	}

	// Wire the SSH bootstrap handler: it issues an HTTP credential over the
	// authenticated SSH channel and returns it (with the TLS pin + ports), so
	// `shed server add` needs no manual token/pin step. In token mode that is a
	// bearer token minted into the shared store; in mtls mode it is a client
	// certificate signed by clientCA, and the token store is deliberately NOT
	// handed over — that path must never mint a bearer token.
	bootstrapTokens := tokenStore
	if cfg.MTLSMode() {
		bootstrapTokens = nil
	}
	sshServer.SetBootstrap(bootstrapTokens, sshd.BootstrapInfo{
		HTTPPort:       cfg.HTTPPort,
		HTTPSPort:      cfg.HTTPSPort,
		TLSFingerprint: tlsFingerprint,
		TokenTTL:       cfg.TokenTTL(),
		AuthMode:       cfg.AuthModeValue(),
		CA:             clientCA,
	})

	// Channel to collect errors from servers (HTTP, HTTPS, SSH).
	errChan := make(chan error, 3)

	// Start HTTP server in goroutine (skipped in token mode — TLS-only)
	if httpServer != nil {
		go func() {
			log.Printf("HTTP server listening on %s", httpServer.Addr)
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errChan <- fmt.Errorf("HTTP server error: %w", err)
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

	// Shutdown HTTP server (when serving — skipped in token mode)
	if httpServer != nil {
		log.Printf("Shutting down HTTP server...")
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
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
