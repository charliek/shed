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
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/firecracker"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/sshd"
	"github.com/charliek/shed/internal/stallwatch"
	"github.com/charliek/shed/internal/users"
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
// SSH-allowlist predicate (users.HasKey over the user lookup). Outside mtls the
// result is a plain server-auth-only config: no ClientAuth, no ClientCAs (open
// mode serves no HTTPS today, so this is the defensive case).
//
// Both conditions are required deliberately, and the belt-and-braces costs
// nothing: an mtls server that somehow reached here with no CA would produce a
// listener that asks for no certificate — but it would not be open, because the
// API middleware gates on cfg.MTLSMode() alone and refuses every request that
// arrives without one.
//
// mtls adds three things: trust only the internal CA, require a client
// certificate that verifies against it, and refuse the handshake outright when
// the identity that certificate names has left the SSH allowlist (a user
// certificate) or is not a configured auth.clients entry (a service
// certificate, which is never checked against the allowlist).
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
		// A service certificate (auth.clients) is accepted while its client
		// is configured and never checked against the SSH allowlist; every
		// other certificate goes through the allowlist as before.
		clientKnown := func(name string) bool {
			_, ok := cfg.ServiceClient(name)
			return ok
		}
		tlsCfg.VerifyConnection = servertls.ClientConnectionVerifier(authorized, clientKnown)
	}
	return tlsCfg
}

// serverStateDir returns the directory that holds the server's identity
// material: the SSH host key, the HTTPS cert + key (unless tls_cert_file /
// tls_key_file override them), the mtls client CA and the GitHub key cache.
// state_dir wins when set; otherwise it is /etc/shed when running as root
// (Linux servers) and ~/.shed otherwise (macOS development without sudo).
// `serve` and any command that must see the same identity (the CA, the TLS
// pin) resolve it here.
func serverStateDir(cfg *config.ServerConfig) string {
	if cfg != nil && cfg.StateDir != "" {
		return config.ExpandPath(cfg.StateDir)
	}
	if os.Getuid() == 0 {
		return "/etc/shed"
	}
	return config.ExpandPath("~/.shed")
}

// tlsCertPaths returns the cert + key file paths for the HTTPS listener,
// honoring tls_cert_file / tls_key_file and otherwise placing them in stateDir
// (serverStateDir: the directory that holds the SSH host key).
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
	// Load configuration. Validation stats kernel_path, initrd_path and
	// local image paths, so an unreadable volume can stall it (#304).
	stopWatch := stallwatch.Watch("loading the config, which also checks kernel_path, initrd_path and local image paths", configWatchPath())
	cfg, err := loadConfig()
	stopWatch()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Enforced-mode preflight: refuse to start `auth.mode: mtls` without an
	// SSH key source (an empty enforced allowlist would lock
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

	// Touch every state directory before the backend does, so a store that is
	// waiting on a macOS permission prompt (or a wedged network filesystem)
	// stalls here, at a line naming the path (#304).
	probeStateDirs(cfg)

	// Initialize plugin system (before backends, since backends use the bridge)
	// The registry tracks pending requests in every mode, so a response is
	// always delivered to the shed connection its request came from. The
	// bridge routes each shed's requests to its user's listeners; a server
	// with no user list (auth.users) routes every shed as the implicit user,
	// whatever user its record carries.
	pluginRegistry := plugin.NewRegistry()
	pluginBridge := newPluginBridge(cfg, pluginRegistry)

	// Initialize the configured backend. egressSocketDir + attachEgress are
	// captured per-backend so the egress proxy (constructed once, below) can
	// place its control socket next to the active backend's sockets and hand
	// the manager to the right client.
	var be backend.Backend
	var egressSocketDir string // runtime dir for the control socket
	var egressStateDir string  // persistent dir for the durable audit log
	var attachEgress func(*egress.Manager)
	var attachEgressStore func(*config.UserProfileStore)
	// resumeChannels re-opens the message channel of every shed that is
	// already running (#315). Captured per-backend like attachEgress.
	var resumeChannels func(context.Context)
	// startNetPolicy installs the backend's network-policy tables from shed
	// metadata and starts their presence check (Firecracker only; nil
	// otherwise). It runs before the resume walk and the listeners.
	var startNetPolicy func(context.Context)
	// waitBackendReady blocks until the backend may serve shed routes
	// (remote only: its first complete list of the cloud; nil otherwise).
	var waitBackendReady func(context.Context) error
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
		resumeChannels = fcClient.ResumeRunningInstances
		startNetPolicy = fcClient.StartNetworkPolicy

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
		resumeChannels = vzClient.ResumeRunningInstances

	case config.BackendRemote:
		rb, err := newRemoteBackend(cfg, pluginBridge)
		if err != nil {
			return err
		}
		log.Printf("Initialized remote backend (provider=%s, transport=%s, environment=%s, default_image=%q)",
			cfg.Remote.Provider, cfg.Remote.Transport, cfg.Remote.Environment, cfg.Remote.DefaultImage)
		be = rb
		// Egress control is refused on remote at load, so these two are
		// never used; they are set so nothing below sees an empty path.
		egressSocketDir = cfg.Remote.StateDirPath(serverStateDir(cfg))
		egressStateDir = egressSocketDir
		// The start-up pass lists the cloud, repairs the record cache and
		// resumes the jobs and message channels a previous server left.
		resumeChannels = rb.Reconcile
		waitBackendReady = rb.WaitReady

	default:
		return fmt.Errorf("unsupported backend type: %s", cfg.DefaultBackend)
	}
	defer be.Close()

	// The host dials the guest, so nothing re-establishes a running shed's
	// message channel on its own after a server restart — the plugin bridge
	// would hold no registration and `shed list -vv` would report no
	// extension health until the shed was stopped and started by hand (#315).
	// Run in the background so a wedged VM can never delay startup; cancelled
	// on shutdown alongside the other background loops.
	// Isolated sheds' firewall rules go back in before anything else can
	// reach a shed: before the resume walk, any create or start, and the
	// listeners. A failure is logged loudly and the guarded TAPs go down; an
	// open-only server with no nft makes no call at all.
	if startNetPolicy != nil {
		netPolicyCtx, cancelNetPolicy := context.WithCancel(context.Background())
		defer cancelNetPolicy()
		startNetPolicy(netPolicyCtx)
	}

	// Sheds made before the server had a user list get their user (on a
	// server with one user) before anyone can reach them. A local backend's
	// pass runs before the resume walk, so a running shed's message channel
	// starts with its new user; the remote backend's runs once its first
	// cloud list has rebuilt its records, and moves the bus routes it
	// resumed.
	if waitBackendReady == nil {
		stampUnassigned(cfg, be)
	}
	resumeCtx, cancelResume := context.WithCancel(context.Background())
	defer cancelResume()
	log.Printf("Resuming message channels for already-running sheds in the background")
	go resumeChannels(resumeCtx)
	if waitBackendReady != nil {
		// Shed routes may be served only after the backend's first
		// complete list of the cloud, so the listeners wait for it.
		log.Printf("Waiting up to %s for the %s backend's first cloud list", backendReadyTimeout, cfg.DefaultBackend)
		readyCtx, cancelReady := context.WithTimeout(context.Background(), backendReadyTimeout)
		err := waitBackendReady(readyCtx)
		cancelReady()
		if err != nil {
			return err
		}
		stampUnassigned(cfg, be)
	}

	// Optional egress-control proxy child, started only when enabled. A start
	// failure is a hard startup error — never a silent disable (AC-11).
	var egressMgr *egress.Manager
	var egressAudit *egress.AuditLog
	var egressUserStore *config.UserProfileStore
	if cfg.Egress != nil && cfg.Egress.Enabled {
		lo, hi := cfg.Egress.PortRangeBounds()
		sockPath := filepath.Join(egressSocketDir, "egress-proxy.sock")
		proxyBin, err := resolveEgressProxyBin()
		if err != nil {
			return fmt.Errorf("egress enabled: %w", err)
		}
		auditPath := filepath.Join(egressStateDir, "egress-audit.jsonl")
		stopWatch = stallwatch.Watch("opening the egress audit log", auditPath)
		egressAudit, err = egress.OpenAuditLog(auditPath, 0)
		stopWatch()
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
		stopWatch = stallwatch.Watch("opening the egress profile store", storeDir)
		egressUserStore, err = config.OpenUserProfileStore(storeDir)
		stopWatch()
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
		log.Printf("Initialized egress-control proxy (proxy %s, ports %d-%d, socket %s, audit %s, profiles %s)", proxyBin, lo, hi, sockPath, auditPath, storeDir)
	}

	// Build the user lookup: the SSH key allowlist (default off = accept all),
	// each key belonging to a user with where it came from. Both the SSH
	// server and the mtls API ask it. GitHub-seeded keys are cached next to
	// the host key. Fails closed (returns an error) when mode=enforce but no
	// keys resolved.
	stateDir := serverStateDir(cfg)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("state dir %s: %w", stateDir, err)
	}
	log.Printf("State dir: %s", stateDir)
	hostKeyPath := filepath.Join(stateDir, "host_key")
	// Every GitHub fetch (the first, the scheduled refresh, `shed users
	// refresh`, an unknown key's) is bound to refreshCtx, cancelled at
	// shutdown so a fetch in flight ends then.
	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	defer cancelRefresh()
	userLookup, err := newUserLookup(refreshCtx, cfg, stateDir)
	if err != nil {
		return fmt.Errorf("ssh auth: %w", err)
	}
	log.Printf("SSH auth mode: %s", userLookup.Mode())
	if names := cfg.UserNames(); len(names) > 0 {
		log.Printf("Users: %s (each key belongs to one user; an unknown key is nobody's)", strings.Join(names, ", "))
	} else {
		log.Printf("Users: none configured (every accepted key is the implicit user, an admin)")
	}

	// Refresh GitHub-seeded keys in the background until shutdown.
	userLookup.StartRefresh(refreshCtx)

	// The access guard: the one door both front doors call the backend
	// through. It applies the access rule (internal/backend/access.go) for
	// the principal each request or channel carries, and binds every
	// operation on a named shed to the incarnation it authorized. The
	// backend's own internal work (start-up resume, reconcile, the credential
	// bus) keeps the raw backend.
	guard := backend.NewGuard(be)

	// Initialize SSH server
	sshServer, err := sshd.NewServer(guard, hostKeyPath, cfg.SSHListenAddr(), cfg.Terminal, sshd.KeyAuth{
		Mode:         userLookup.Mode(),
		MaxAuthTries: userLookup.MaxAuthTries(),
		Users:        userLookup,
	})
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
		caCertPath, caKeyPath := caCertPaths(stateDir)
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

	// The audit log (service client requests, HTTPS execs, max_age reaps),
	// next to the egress audit log in the backend's state directory. On when
	// auth.clients is set or audit.enabled; a server that needs it and can't
	// open it does not start.
	var auditLog *audit.Log
	if cfg.AuditEnabled() {
		auditPath := filepath.Join(egressStateDir, audit.FileName)
		auditLog, err = audit.Open(auditPath, audit.DefaultMaxBytes, audit.DefaultKeep)
		if err != nil {
			return fmt.Errorf("audit: open %s: %w", auditPath, err)
		}
		defer func() { _ = auditLog.Close() }()
		log.Printf("Audit log: %s", auditPath)
	}

	// Initialize HTTP API server
	apiServer := api.NewServer(guard, cfg, hostKey, pluginRegistry, pluginBridge)
	apiServer.SetAuditLog(auditLog)               // nil-safe: records nothing when audit is off
	apiServer.SetEgressAudit(egressAudit)         // nil-safe: no-op when egress disabled
	apiServer.SetEgressUserStore(egressUserStore) // nil-safe: no-op when egress disabled
	apiServer.SetKeyRefresher(userLookup)         // `shed users refresh`, in every mode
	if clientCA != nil {
		// mtls: the middleware re-validates every request's client certificate
		// against the LIVE user lookup, so removing an SSH key de-authorizes its
		// certificates on the next request.
		apiServer.SetUserLookup(userLookup)
		apiServer.SetClientCAInfo(clientCA.Fingerprint(), clientCA.NotAfter())
	}

	// Warn when bind_address is unset: as of v0.7.4 that defaults to loopback
	// (was all-interfaces), so a server that relied on the old default is now
	// local-only. This startup line is the only migration signal that reaches
	// every install channel (brew has no upgrade hook); the listeners log their
	// resolved addresses separately below.
	if cfg.BindAddress == "" {
		if cfg.PlainHTTPEnabled() {
			log.Printf("WARNING: bind_address is unset — binding loopback (127.0.0.1) only. This was all-interfaces before v0.7.4. An open-mode server is loopback only; to reach it off-box, switch to auth.mode: mtls and set bind_address (e.g. 0.0.0.0).")
		} else {
			log.Printf("bind_address is unset — binding loopback (127.0.0.1) only. Set bind_address (e.g. 0.0.0.0) to reach this mtls-mode server off-box.")
		}
	}

	// One router carries every route; the HTTP and (when enabled) HTTPS
	// listeners share this single handler.
	publicHandler := apiServer.Router()

	// Plain-HTTP listener. In mtls mode it is not started — only the pinned-TLS (https_port) listener faces clients.
	var httpServer *http.Server
	if cfg.PlainHTTPEnabled() {
		httpServer = newHTTPServer(cfg.HTTPListenAddr(), publicHandler)
	}

	// Optional HTTPS listener: same public router over TLS with a self-signed,
	// client-pinned cert. Off by default (https_port unset).
	var httpsServer *http.Server
	var tlsFingerprint string
	if cfg.HTTPSEnabled() {
		certPath, keyPath := tlsCertPaths(cfg, stateDir)
		cert, der, err := servertls.LoadOrGenerate(certPath, keyPath, cfg.TLSNames)
		if err != nil {
			return fmt.Errorf("tls cert: %w", err)
		}
		tlsFingerprint = servertls.Fingerprint(der)
		log.Printf("TLS cert fingerprint: %s", tlsFingerprint)
		httpsServer = newHTTPServer(cfg.HTTPSListenAddr(), publicHandler)
		httpsServer.TLSConfig = buildHTTPSTLSConfig(cfg, cert, clientCA, func(fp string) bool {
			return users.HasKey(userLookup, fp)
		})
	}

	// Wire the SSH bootstrap handler: in mtls mode it issues a client
	// certificate signed by clientCA over the authenticated SSH channel and
	// returns it (with the TLS pin + port), so `shed server add` needs no
	// manual pin step. An open server's allowlist is never enforced, so every
	// request gets the open-server refusal that released clients recognise.
	// The server issues no other credential: it holds no bearer-token code.
	sshServer.SetBootstrap(sshd.BootstrapInfo{
		HTTPSPort:      cfg.HTTPSPort,
		TLSFingerprint: tlsFingerprint,
		TokenTTL:       cfg.TokenTTL(),
		CA:             clientCA,
	})

	// Channel to collect errors from servers (HTTP, HTTPS, SSH).
	errChan := make(chan error, 3)

	// Start HTTP server in goroutine (skipped in mtls mode — TLS-only)
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

	// The max_age reaper: the only server-side lifetime bound on a service
	// client's sheds. It runs only when clients are configured. stopReaper
	// ends it and waits for a stop or delete in progress to finish: it runs
	// first thing at shutdown, and (deferred, for an error return) before the
	// audit log and the backend, deferred earlier, close.
	stopReaper := func() {}
	if cfg.ServiceClientsEnabled() {
		log.Printf("Starting the max_age reaper (every %s)", api.DefaultReapInterval)
		stopReaper = apiServer.StartReaper(api.DefaultReapInterval, time.Now)
	}
	defer stopReaper()

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

	// Graceful shutdown. The reaper first: a reap that has begun finishes
	// (an expired shed is not left half torn down, nor its reap unrecorded).
	if cfg.ServiceClientsEnabled() {
		log.Printf("Stopping the max_age reaper...")
	}
	stopReaper()
	// GitHub key fetches end now: none in flight outlives the shutdown.
	cancelRefresh()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Shutdown HTTP server (when serving — skipped in mtls mode)
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

// upgradePassTimeout bounds the start-up pass over sheds with no user. On the
// remote backend each stamp is a cloud label write; one that does not finish
// in time is logged and tried again at the next start.
const upgradePassTimeout = 2 * time.Minute

// stampUnassigned runs the start-up pass over sheds and snapshots made before
// the server had a user list (backend.StampUnassigned) on the raw backend, as
// a one-time upgrade step recorded by a marker in the server's state
// directory (backend.OpenUsersUpgrade). With no user list it stamps nothing
// and removes the marker. On the first start with a user list, with exactly
// one user, it gives them every shed and snapshot with no user (and logs
// each), and the marker is written once the pass has finished without a
// failure (a pass that did not finish runs again at the next start); with
// several it stamps nothing, and the marker is written before the pass lists
// anything, so a pass that then fails cannot lose it. On every later start nothing is
// stamped. It logs how many sheds it found with no user, naming them when
// log_level is debug. The returned UsersUpgrade carries the "first start
// with a user list" decision for the other one-time steps of adding one.
func stampUnassigned(cfg *config.ServerConfig, be backend.Backend) (backend.UpgradeReport, *backend.UsersUpgrade) {
	return stampUnassignedIn(cfg, be, serverStateDir(cfg))
}

// stampUnassignedIn is stampUnassigned with the marker in stateDir.
func stampUnassignedIn(cfg *config.ServerConfig, be backend.Backend, stateDir string) (backend.UpgradeReport, *backend.UsersUpgrade) {
	users := upgradeUsers(cfg)
	upgrade := backend.OpenUsersUpgrade(stateDir, len(users) > 0, log.Printf)
	ctx, cancel := context.WithTimeout(context.Background(), upgradePassTimeout)
	defer cancel()
	debugf := func(string, ...any) {}
	if cfg.LogLevel == "debug" {
		debugf = func(format string, args ...any) { log.Printf("DEBUG: "+format, args...) }
	}
	// With several users nothing is ever stamped, so the step's outcome is
	// known before any listing: it is recorded first, so a listing that then
	// fails or times out (or a crash) cannot leave a later start, with fewer
	// users, to take it as the first and stamp what was left unassigned on
	// purpose. With one user it is recorded only once the pass has finished.
	recordedFirst := false
	if upgrade.FirstUserListStart() && len(users) > 1 {
		if err := upgrade.MarkDone(time.Now()); err != nil {
			log.Printf("Users: the one-time upgrade step for the user list (several users: nothing is given a user automatically) could not be recorded; it runs again at the next start: %v", err)
		} else {
			recordedFirst = true
			log.Printf("Users: the one-time upgrade step for the user list is recorded (several users: nothing is given a user automatically); from now on a shed with no user is given one only by 'shed assign'")
		}
	}
	rep := backend.StampUnassigned(ctx, be, users, upgrade.FirstUserListStart(), backend.UpgradeLog{Infof: log.Printf, Debugf: debugf})
	if upgrade.FirstUserListStart() && !recordedFirst {
		switch {
		case rep.Incomplete:
			log.Printf("Users: the one-time upgrade step for the user list did not finish; it runs again at the next start")
		default:
			if err := upgrade.MarkDone(time.Now()); err != nil {
				log.Printf("Users: the one-time upgrade step for the user list finished, but its marker could not be written (it runs again at the next start): %v", err)
			} else {
				log.Printf("Users: the one-time upgrade step for the user list is done and recorded; from now on a shed with no user is given one only by 'shed assign'")
			}
		}
	}
	return rep, upgrade
}

// upgradeUsers is the configured user list (auth.users) as the start-up pass
// takes it, sorted by name; nil with no user list.
func upgradeUsers(cfg *config.ServerConfig) []backend.UpgradeUser {
	names := cfg.UserNames()
	if len(names) == 0 {
		return nil
	}
	out := make([]backend.UpgradeUser, 0, len(names))
	for _, name := range names {
		out = append(out, backend.UpgradeUser{Name: name, Admin: bool(cfg.Auth.Users[name].Admin)})
	}
	return out
}

// newPluginBridge builds the credential bus bridge over reg. It routes each
// shed's requests to its user's listeners on a server with a user list
// (auth.users); without one, every shed routes as the implicit user whatever
// user its record carries.
func newPluginBridge(cfg *config.ServerConfig, reg *plugin.Registry) *plugin.Bridge {
	return plugin.NewBridge(reg, cfg.UsersConfigured())
}

// newUserLookup builds the user lookup both front doors ask: the SSH key
// allowlist (auth.ssh, default off = accept all) with every key the implicit
// user's, or, with auth.users, each user's keys. GitHub-seeded keys are cached
// in stateDir/github_keys, and each GitHub login is pinned to its numeric
// account id at its first good fetch, in the pin store next to the cache (a
// user's github_id, when configured, wins). Every fetch is bound to ctx.
func newUserLookup(ctx context.Context, cfg *config.ServerConfig, stateDir string) (*users.ConfigLookup, error) {
	var list map[string]config.UserConfig
	if cfg.UsersConfigured() {
		list = cfg.Auth.Users
	}
	return users.NewConfigLookup(users.ConfigOptions{
		SSH:            cfg.EffectiveSSHAuth(),
		GitHubCacheDir: filepath.Join(stateDir, "github_keys"),
		PinsPath:       filepath.Join(stateDir, users.PinsFileName),
		Context:        ctx,
		Users:          list,
	})
}

// configWatchPath names the config file in a startup stall line: the --config
// path, or every location the default search checks when none was given.
func configWatchPath() string {
	if configPath != "" {
		return config.ExpandPath(configPath)
	}
	return strings.Join(config.ServerConfigSearchPaths(), ", ")
}

// loadConfig loads the server configuration from the specified path or default locations.
func loadConfig() (*config.ServerConfig, error) {
	if configPath != "" {
		return config.LoadServerConfigFromPath(configPath)
	}
	return config.LoadServerConfig()
}
