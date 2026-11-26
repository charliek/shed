package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/firecracker"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/source"
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
		// Puts the connection on each request's context, so a
		// credential-bus stream can bound how long data written to it may
		// stay unacknowledged (a machine that went to sleep is reaped).
		ConnContext: api.ConnContext,
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
// the identity that certificate names has left the SSH allowlist or now
// belongs to another user than the one it was issued to (a user certificate:
// api.CertificateUser), or is not a configured auth.clients entry (a service
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
func buildHTTPSTLSConfig(cfg *config.ServerConfig, cert tls.Certificate, clientCA *servertls.CA, authorized func(*x509.Certificate) bool) *tls.Config {
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

// certificateAuthorized is the HTTPS listener's handshake check of a user's
// client certificate, against the live user lookup l: its key still resolves
// to the user it was issued to (api.CertificateUser, the rule the API
// middleware applies again on every request).
func certificateAuthorized(l users.Lookup) func(*x509.Certificate) bool {
	return func(leaf *x509.Certificate) bool {
		_, _, ok := api.CertificateUser(l, leaf, time.Now())
		return ok
	}
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

	// The one-time upgrade step for a user list: an unfinished step whose
	// users changed stops the start here, before any backend, store or
	// listener opens and before anything is stamped, adopted or recorded,
	// unless the operator abandons it on purpose; the flag with no such step
	// stops it too. stampUnassigned applies the same rule again.
	if err := checkUsersUpgrade(cfg, serverStateDir(cfg), serveAbandonUserUpgrade); err != nil {
		return err
	}

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
	// Provisioning by delivery: requests go to each user's bound machine
	// over the pin; the delivery routes hand answers to the same service the
	// backends' create and provision ask.
	deliveryService := delivery.NewService(pluginRegistry, cfg.DeliveryTimeout())
	// Who is asked (plan 040, section 3.2), from the static configuration
	// alone: on a directory server every name but a local admin's is the
	// account service's, and no machine is ever bound for it on the bus.
	deliveryService.SetOrigin(cfg.UserOrigin)
	pluginRegistry.SetServiceUsers(func(user string) bool {
		return cfg.UserOrigin(user) == delivery.OriginDirectory
	})

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
	// releaseResumes lets the creates the start-up pass resumed run their
	// delivery steps, once the origin migration has run (remote only; a
	// no-op otherwise).
	releaseResumes := func() {}
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
		fcClient.SetDelivery(deliveryService)
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
		vzClient.SetDelivery(deliveryService)
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
		rb.SetDelivery(deliveryService)
		// Creates resumed by the start-up pass wait for the origin
		// migration below (stampUserOrigins).
		rb.HoldResumes()
		releaseResumes = rb.ReleaseResumes
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

	// The credential bus pin: the store of each user's credential source,
	// in the backend's persistent state directory (beside the audit logs;
	// not the server state directory, which a parallel dev server may
	// share). The server does not start without it: starting with none would
	// let the next machine to connect become a user's source. The registry
	// takes it now, before anything can register a listener; the start-up
	// steps of the user list (stampUnassigned) act on it before the
	// listeners start.
	bindingsPath := filepath.Join(egressStateDir, source.FileName)
	stopWatch = stallwatch.Watch("opening the credential source bindings", bindingsPath)
	sourceBindings, created, err := source.OpenFileBindings(bindingsPath, source.Options{})
	stopWatch()
	if err != nil {
		return err
	}
	defer func() {
		if err := sourceBindings.Close(); err != nil {
			log.Printf("credential source bindings: closing %s: %v", bindingsPath, err)
		}
	}()
	if created {
		log.Printf("WARNING: %s", source.StartedEmptyWarning(bindingsPath))
	}
	log.Printf("Credential source bindings: %s", bindingsPath)
	pluginRegistry.SetBindings(sourceBindings)

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
		if _, _, err := stampUnassigned(cfg, be, sourceBindings, serveAbandonUserUpgrade); err != nil {
			return err
		}
		stampUserOrigins(cfg, be)
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
		if _, _, err := stampUnassigned(cfg, be, sourceBindings, serveAbandonUserUpgrade); err != nil {
			releaseResumes()
			return err
		}
		stampUserOrigins(cfg, be)
		releaseResumes()
	}

	reportReservedSheds(be)

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
	dirUsers, _ := userLookup.(*directory.Runtime)
	if dirUsers != nil {
		// The hosted source's handle, late-bound: set before any listener
		// starts, so every fresh delivery sees it. A create job resumed at
		// start-up (resumeChannels, which ran before this) never asks a
		// source; one that asked before it was set would get
		// source_offline, never a machine.
		deliveryService.SetHosted(dirUsers)
	}
	switch names := cfg.UserNames(); {
	case dirUsers != nil:
		for _, line := range directoryStartupLines(cfg, dirUsers, time.Now()) {
			log.Printf("%s", line)
		}
	case len(names) > 0:
		log.Printf("Users: %s (each key belongs to one user; an unknown key is nobody's)", strings.Join(names, ", "))
	default:
		log.Printf("Users: none configured (every accepted key is the implicit user, an admin)")
	}

	// Refresh GitHub-seeded keys in the background until shutdown. A
	// directory server starts its pulls once the SSH server exists (below),
	// so that every publication of its list can close connections.
	if dirUsers == nil {
		userLookup.StartRefresh(refreshCtx)
	}

	// The access guard: the one door both front doors call the backend
	// through. It applies the access rule (internal/backend/access.go) for
	// the principal each request or channel carries, and binds every
	// operation on a named shed to the incarnation it authorized. The
	// backend's own internal work (start-up resume, reconcile, the credential
	// bus) keeps the raw backend.
	guard := backend.NewGuard(be)

	// Initialize SSH server
	sshServer, err := sshd.NewServer(guard, hostKeyPath, cfg.SSHListenAddr(), cfg.Terminal, sshKeyAuth(userLookup))
	if err != nil {
		return fmt.Errorf("failed to create SSH server: %w", err)
	}
	hostKey := sshServer.GetHostPublicKey()
	if dirUsers != nil {
		// Every publication of the list closes the SSH connections of users
		// it no longer has; then the pulls and the trust watch start, until
		// shutdown.
		closeOnPublication(dirUsers, sshServer)
		userLookup.StartRefresh(refreshCtx)
	}

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

	// The audit log (service client requests, HTTPS execs, max_age reaps, a
	// person's entries into service clients' sheds, the shed lifecycle and
	// delivery events), next to the egress audit log in the backend's state
	// directory (openAuditLog). Once serving, runFrontDoors closes it, after
	// every server has drained; this close (a no-op then) is for a return
	// before that.
	auditLog, err := openAuditLog(cfg, egressStateDir, log.Printf)
	if err != nil {
		return err
	}
	if auditLog != nil {
		defer func() { _ = auditLog.Close() }()
	}

	// Initialize HTTP API server
	// The audit log is handed to both front doors by runFrontDoors, before
	// either serves.
	apiServer := api.NewServer(guard, cfg, hostKey, pluginRegistry, pluginBridge)
	apiServer.SetEgressAudit(egressAudit)         // nil-safe: no-op when egress disabled
	apiServer.SetEgressUserStore(egressUserStore) // nil-safe: no-op when egress disabled
	apiServer.SetKeyRefresher(userLookup)         // `shed users refresh`, in every mode
	apiServer.SetDelivery(deliveryService)
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
		httpsServer.TLSConfig = buildHTTPSTLSConfig(cfg, cert, clientCA, certificateAuthorized(userLookup))
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

	// The max_age reaper: the only server-side lifetime bound on a service
	// client's sheds. It runs only when clients are configured.
	var startReaper func() func()
	if cfg.ServiceClientsEnabled() {
		startReaper = func() func() {
			log.Printf("Starting the max_age reaper (every %s)", api.DefaultReapInterval)
			return apiServer.StartReaper(api.DefaultReapInterval, time.Now)
		}
	}
	var checkTrust func(context.Context) error
	if dirUsers != nil {
		checkTrust = dirUsers.CheckTrustAtStop
	}

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	return runFrontDoors(frontDoors{
		api:         apiServer,
		ssh:         sshServer,
		auditLog:    auditLog,
		doors:       serveDoors(httpServer, httpsServer, apiServer, sshServer),
		startReaper: startReaper,
		// Credential-bus streams never end on their own, so a graceful
		// shutdown would wait out its whole timeout for them: close them
		// (and refuse new subscriptions) first.
		beforeShutdown: func() {
			log.Printf("Closing credential bus streams...")
			pluginRegistry.Close()
			// Hosted delivery requests in flight end now (their calls are
			// aborted and their slots released); none starts after.
			deliveryService.Close()
		},
		stopping: func(shutdownServers func()) {
			stopUserSource(checkTrust, directory.StopTrustBound, cancelRefresh, shutdownServers, log.Printf)
		},
		timeout: shutdownTimeout,
	}, sigChan)
}

// frontDoor is one listener serve runs. serve runs it until it is shut down
// (an error it returns ends serve); shutdown stops it, until ctx ends: it
// stops accepting at once and then drains what it is serving. A door with no
// serve is only shut down.
type frontDoor struct {
	name     string
	serve    func() error
	shutdown func(context.Context) error
}

// serveDoors is serve's doors: the HTTP listener (when serving; never in
// mtls mode), the HTTPS listener (when https_port is set), the API server's
// Connect tunnels (a hijacked connection outlives its HTTP server's
// shutdown, so the tunnels a person was opening into service clients' sheds
// are drained on their own, for their audit lines), and the SSH server
// (which drains its own entries into service clients' sheds). runFrontDoors
// shuts them all down together.
func serveDoors(httpServer, httpsServer *http.Server, apiServer *api.Server, sshServer *sshd.Server) []frontDoor {
	var doors []frontDoor
	if httpServer != nil {
		doors = append(doors, frontDoor{
			name: "HTTP server",
			serve: func() error {
				log.Printf("HTTP server listening on %s", httpServer.Addr)
				if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
					return err
				}
				return nil
			},
			shutdown: httpServer.Shutdown,
		})
	}
	if httpsServer != nil {
		doors = append(doors, frontDoor{
			name: "HTTPS server",
			// The cert + key are already loaded into TLSConfig, so the file
			// args are empty.
			serve: func() error {
				log.Printf("HTTPS server listening on %s", httpsServer.Addr)
				if err := httpsServer.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
					return err
				}
				return nil
			},
			shutdown: httpsServer.Shutdown,
		})
	}
	doors = append(doors,
		frontDoor{name: "Connect tunnels", shutdown: apiServer.DrainEntries},
		frontDoor{name: "SSH server", serve: sshServer.Start, shutdown: sshServer.Shutdown},
	)
	return doors
}

// frontDoors is what serve runs once everything is built (runFrontDoors).
type frontDoors struct {
	api *api.Server
	ssh *sshd.Server
	// auditLog is the one audit log both front doors write; nil when audit
	// is off. runFrontDoors closes it last.
	auditLog *audit.Log
	// doors (serveDoors), shut down together; errors are logged in this
	// order.
	doors []frontDoor
	// startReaper starts the max_age reaper and returns what stops it (and
	// waits for a reap in progress); nil when no service client is
	// configured.
	startReaper func() (stop func())
	// beforeShutdown runs first in the servers' shutdown; may be nil.
	beforeShutdown func()
	// stopping runs the servers' shutdown (its argument) inside the user
	// source's stop (stopUserSource); nil runs it alone.
	stopping func(shutdownServers func())
	// timeout bounds the servers' shutdown.
	timeout time.Duration
}

// runFrontDoors hands the audit log to both front doors (attachAuditLog),
// starts every door and the max_age reaper, and waits for a signal on
// signals or for a door's serve to fail. Then, on either path, it stops in
// order. First, before anything waits, both front doors' gates for entries
// into service clients' sheds close (closeEntryGates): from then on a
// person's SSH channel or Connect into one is refused at once. Then the
// reaper (a reap that has begun finishes, so an expired shed is not left
// half torn down, nor its reap unrecorded). Then the servers, all together
// under the one bound (shutdownDoors): each stops accepting at once and
// then drains what it is serving, including the entries into service
// clients' sheds admitted before the gates closed, whose audit lines are
// written by then; no server's wait (an HTTP request that never ends, such
// as the egress stream) delays another's listener closing or uses up
// another's drain. Only then the audit log, which is closed last so that no
// admitted entry loses its line to the shutdown. It returns the door's
// error, or nil after a signal.
func runFrontDoors(fd frontDoors, signals <-chan os.Signal) error {
	attachAuditLog(fd.auditLog, fd.api, fd.ssh) // nil-safe: records nothing when audit is off

	// Each door reports a failed serve here; the buffer holds one per door,
	// so none blocks once the select below has taken the first.
	errChan := make(chan error, len(fd.doors))
	for _, d := range fd.doors {
		if d.serve == nil {
			continue
		}
		go func() {
			if err := d.serve(); err != nil {
				errChan <- fmt.Errorf("%s error: %w", d.name, err)
			}
		}()
	}

	stopReaper := func() {}
	if fd.startReaper != nil {
		stopReaper = fd.startReaper()
	}

	log.Printf("Shed server is ready")

	// Wait for signal or error
	var serveErr error
	select {
	case sig := <-signals:
		log.Printf("Received signal %v, initiating graceful shutdown...", sig)
	case serveErr = <-errChan:
		log.Printf("Server error: %v", serveErr)
		log.Printf("Shutting down the other servers after the error...")
	}

	closeEntryGates(fd.api, fd.ssh)

	if fd.startReaper != nil {
		log.Printf("Stopping the max_age reaper...")
	}
	stopReaper()

	shutdownServers := func() {
		ctx, cancel := context.WithTimeout(context.Background(), fd.timeout)
		defer cancel()
		if fd.beforeShutdown != nil {
			fd.beforeShutdown()
		}
		shutdownDoors(ctx, fd.doors)
	}
	if fd.stopping != nil {
		fd.stopping(shutdownServers)
	} else {
		shutdownServers()
	}

	// Every server has stopped and drained: no entry into a service
	// client's shed can still be on its way to a line.
	if err := fd.auditLog.Close(); err != nil {
		log.Printf("audit: closing the log: %v", err)
	}
	if serveErr != nil {
		return serveErr
	}
	log.Printf("Shutdown complete")
	return nil
}

// closeEntryGates closes both front doors' gates for entries into service
// clients' sheds (a person's SSH channel, a person's Connect), without
// waiting on anything: an entry not begun by now is refused, and every
// entry the drains will wait for was begun before the shutdown did.
func closeEntryGates(apiServer *api.Server, sshServer *sshd.Server) {
	log.Printf("Closing entry gates...")
	apiServer.CloseEntries()
	sshServer.CloseEntries()
}

// shutdownDoors starts every door's shutdown at once, under the one bound
// ctx, and waits for all of them; then it logs each error, in door order. A
// door that waits long (an HTTP request that never ends) neither keeps
// another door's listener open nor uses up another's wait.
func shutdownDoors(ctx context.Context, doors []frontDoor) {
	errs := make([]error, len(doors))
	var wg sync.WaitGroup
	for i, d := range doors {
		log.Printf("Shutting down %s...", d.name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.shutdown(ctx)
		}()
	}
	wg.Wait()
	for i, d := range doors {
		if errs[i] != nil {
			log.Printf("%s shutdown error: %v", d.name, errs[i])
		}
	}
}

// stopUserSource is the user source's part of a graceful stop, around the
// servers' shutdown (shutdown). GitHub key fetches (or a directory's pulls
// and trust watch) end first (cancelRefresh): none in flight outlives the
// shutdown. On a directory server (checkTrust set: its runtime's
// CheckTrustAtStop) the kept copy is then fenced, so no pull writes it from
// that moment, and the trust time is checked at once (an ended copy is
// removed); and once more as the last thing the server does with the
// directory, after the servers have finished shutting down, so a trust time
// that ends during a slow shutdown is caught too. Each check is waited for
// up to bound; the warning that one did not finish is written without
// waiting (a stuck log writer never holds the exit).
func stopUserSource(checkTrust func(context.Context) error, bound time.Duration, cancelRefresh, shutdown func(), logf func(string, ...any)) {
	cancelRefresh()
	check := func() {
		if checkTrust == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), bound)
		defer cancel()
		if err := checkTrust(ctx); err != nil {
			go logf("directory: WARNING: the trust time's check at stop did not finish within %s (%v); if the trust time has ended, the kept copy may remain on disk", bound, err)
		}
	}
	check()
	shutdown()
	check()
}

// upgradePassTimeout bounds the start-up pass over sheds with no user. On the
// remote backend each stamp is a cloud label write; one that does not finish
// in time is logged and tried again at the next start.
const upgradePassTimeout = 2 * time.Minute

// stampUnassigned runs the one-time upgrade step for a user list, recorded
// by a marker in the server's state directory (backend.OpenUsersUpgrade),
// and the start-up report of sheds and snapshots with no user. First the
// step's decision for this start is taken and recorded
// (backend.UsersUpgrade.Begin): with several users on the first start, done
// before anything is listed; with exactly one user on the first start, begun
// for that user (a pending marker naming them) before anything is given to
// anyone; an unfinished step is resumed only for the user it was begun for,
// while they are still the only configured user; otherwise the start is
// refused (checkUsersUpgrade already refused it before anything opened),
// unless abandon (serve's AbandonUsersUpgradeFlag) abandons it on purpose
// (marked done, nothing given). Then the credential source bindings follow
// that decision (applySourceUpgrade), then the pass over sheds and snapshots
// runs on the raw backend (backend.StampUnassigned), stamping only for the
// decided user, and the step is marked done once a stamping pass finishes
// (backend.UsersUpgrade.Finish). With no user list it stamps nothing and
// removes the marker. It logs how many sheds it found with no user, naming
// them when log_level is debug. An error (the step's refusal, or the source
// bindings' step) means the server must not start; the marker then still
// says what the next start resumes.
func stampUnassigned(cfg *config.ServerConfig, be backend.Backend, bindings source.Bindings, abandon bool) (backend.UpgradeReport, *backend.UsersUpgrade, error) {
	return stampUnassignedWith(cfg, be, serverStateDir(cfg), bindings, abandon)
}

// serveAbandonUserUpgrade is serve's AbandonUsersUpgradeFlag.
var serveAbandonUserUpgrade bool

func init() {
	serveCmd.Flags().BoolVar(&serveAbandonUserUpgrade, strings.TrimPrefix(backend.AbandonUsersUpgradeFlag, "--"), false,
		"abandon, at this one start, the unfinished one-time upgrade step for the user list after its users changed (the sheds and snapshots it has not reached keep no user); refused at start when there is no such step, so never leave it on a service's command line")
}

// checkUsersUpgrade is the one-time upgrade step's start-up check
// (backend.CheckUsersUpgrade), run by serve before it opens a backend, a
// store or a listener: a step pending for a user who is no longer the only
// configured user stops the start, unless abandon; abandon with no such
// step stops it too. It reads the marker and changes nothing.
//
// The users compared are upgradeUsers': auth.users, which beside
// auth.directory holds the local admins only. The account service's list
// never enters it, so a directory list that is not in force at start (not
// enrolled, the service unreachable with no kept copy) neither refuses nor
// abandons a step.
func checkUsersUpgrade(cfg *config.ServerConfig, stateDir string, abandon bool) error {
	return backend.CheckUsersUpgrade(stateDir, upgradeUsers(cfg), abandon)
}

// reportReservedSheds warns when a shed already has the reserved name git:
// SSH to it is refused now. Not found is silent; any other error is logged
// and does not stop start-up. It asks as the server's own principal, on
// whichever backend is running.
func reportReservedSheds(be backend.Backend) {
	ctx, cancel := context.WithTimeout(backend.WithPrincipal(context.Background(), backend.SystemPrincipal()), upgradePassTimeout)
	defer cancel()
	_, err := be.GetShed(ctx, config.ReservedShedName)
	switch {
	case err == nil:
		log.Printf("WARNING: a shed named %q exists. %q is now reserved: SSH to it is refused. Snapshot it and recreate under another name, or delete it.", config.ReservedShedName, config.ReservedShedName)
	case errors.Is(err, config.ErrShedNotFoundSentinel):
	default:
		log.Printf("Could not check for a shed named %q (a reserved name): %v", config.ReservedShedName, err)
	}
}

// stampUnassignedIn is stampUnassigned with the marker in stateDir, without
// the abandon flag.
func stampUnassignedIn(cfg *config.ServerConfig, be backend.Backend, stateDir string, bindings source.Bindings) (backend.UpgradeReport, *backend.UsersUpgrade, error) {
	return stampUnassignedWith(cfg, be, stateDir, bindings, false)
}

// stampUnassignedWith is stampUnassigned with the marker in stateDir.
func stampUnassignedWith(cfg *config.ServerConfig, be backend.Backend, stateDir string, bindings source.Bindings, abandon bool) (backend.UpgradeReport, *backend.UsersUpgrade, error) {
	// The check first, before opening the marker (which, with no user list,
	// removes it): a refused start changes nothing.
	if err := checkUsersUpgrade(cfg, stateDir, abandon); err != nil {
		return backend.UpgradeReport{}, nil, err
	}
	users := upgradeUsers(cfg)
	upgrade := backend.OpenUsersUpgrade(stateDir, len(users) > 0, log.Printf)
	// The decision is recorded before anything acts on it, so the source
	// bindings' step and the pass below follow the same recipient at every
	// start until the step is done.
	plan, err := upgrade.Begin(users, abandon, time.Now())
	if err != nil {
		return backend.UpgradeReport{}, upgrade, err
	}
	if err := applySourceUpgrade(bindings, plan, cfg.UserNames(), cfg.DirectoryConfigured()); err != nil {
		return backend.UpgradeReport{}, upgrade, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), upgradePassTimeout)
	defer cancel()
	debugf := func(string, ...any) {}
	if cfg.LogLevel == "debug" {
		debugf = func(format string, args ...any) { log.Printf("DEBUG: "+format, args...) }
	}
	passUsers, stamp := users, false
	if plan.StampFor != nil {
		passUsers, stamp = []backend.UpgradeUser{*plan.StampFor}, true
	}
	rep := backend.StampUnassigned(ctx, be, passUsers, stamp, backend.UpgradeLog{Infof: log.Printf, Debugf: debugf})
	upgrade.Finish(plan, rep, time.Now())
	return rep, upgrade, nil
}

// applySourceUpgrade is the start-up step for the credential source bindings
// when a user list is added later, run before any listener can register. It
// follows the one-time step's decision for this start
// (backend.UsersUpgrade.Begin), already recorded:
//
//   - When the step gives sheds to one user (plan.StampFor: the first start
//     with exactly one user, or a resumed step for that same user), the
//     implicit user's binding and remembered machines become that user's
//     (AdoptImplicit; when the user already has records of their own, the
//     implicit ones are dropped instead). Because the pending marker naming
//     the user is written before this, a start that ends between the two
//     resumes for the same user only, and AdoptImplicit run again finds no
//     implicit records (or the ones a crash left) and adopts them for that
//     same user: the adoption happens once, for the recipient recorded, and
//     is never redirected. An abandoned step adopts nothing.
//   - On the first start with several users (plan.SeveralUsers) they are
//     dropped.
//   - When the step could not begin (plan.Deferred: its pending marker
//     could not be written) the implicit user's records are kept for the
//     start that takes it.
//   - At every start, the records of every user that is not configured are
//     dropped (RetainUsers with the configured names, or with the implicit
//     user alone when there is no user list), so a name configured again
//     later starts clean. With no user list nothing is adopted, and only
//     named users' records (unreachable without a user list) are dropped.
//     On a directory server (directory true) the names are the local admins
//     alone, and every other user is the account service's (plan 040,
//     section 3.2): their bindings are removed, each logged, and none is
//     ever made again (the bus refuses to admit a machine for them).
//
// A store that cannot be written fails the start; the marker still says what
// the next start does.
func applySourceUpgrade(b source.Bindings, plan backend.UpgradePlan, names []string, directory bool) error {
	if b == nil {
		return nil
	}
	keep := names
	switch {
	case len(names) == 0:
		keep = []string{source.ImplicitUser}
	case plan.StampFor != nil:
		name := plan.StampFor.Name
		ad, err := b.AdoptImplicit(name)
		if err != nil {
			return fmt.Errorf("credential source bindings: giving the server's credential source to user %q: %w", name, err)
		}
		switch {
		case ad.Adopted && ad.Binding != nil:
			log.Printf("Users: the credential source of the server before its user list (machine %s) is now user %s's, with %d remembered machine(s)", ad.Binding.MachineID(), name, ad.Machines)
		case ad.Adopted:
			log.Printf("Users: the %d machine(s) remembered before the user list are now user %s's", ad.Machines, name)
		case ad.Dropped:
			log.Printf("Users: user %s already has credential source records; those from before the user list (%d machine(s)) were dropped", name, ad.Machines)
		}
	case plan.SeveralUsers:
		dropped, err := b.Drop(source.ImplicitUser)
		if err != nil {
			return fmt.Errorf("credential source bindings: dropping the credential source from before the user list: %w", err)
		}
		if dropped {
			log.Printf("Users: several users are configured, so the credential source from before the user list was dropped; each user's first machine to connect becomes theirs")
		}
	case plan.Deferred:
		keep = append(append([]string(nil), names...), source.ImplicitUser)
	}
	dropped, err := b.RetainUsers(keep)
	if err != nil {
		return fmt.Errorf("credential source bindings: dropping the records of users no longer configured: %w", err)
	}
	for _, u := range dropped {
		switch {
		case u == source.ImplicitUser:
			log.Printf("Users: dropped the credential source records from before the user list")
		case directory:
			log.Printf("Credential source: removed the binding of %q: the account service answers for this user on this server, and no machine does", u)
		default:
			log.Printf("Users: dropped the credential source records of %q, which is not a configured user", u)
		}
	}
	return nil
}

// stampUserOrigins is the start-up step that records where the user of each
// shed made before user_origin came from (plan 040, section 3.2), from the
// one decision the first successful migration recorded in the origin migration file
// (backend.MigrateUserOrigins: <state dir>/delivery/origin-migration.json),
// never from a later start's configuration. A backend that keeps no origin
// is skipped. A failure is logged; a shed it leaves unstamped is refused
// deliveries (SHED_ORIGIN_UNKNOWN) until a start stamps it.
func stampUserOrigins(cfg *config.ServerConfig, be backend.Backend) {
	st, ok := be.(backend.OriginStamper)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), upgradePassTimeout)
	defer cancel()
	path := filepath.Join(serverStateDir(cfg), backend.OriginMigrationDir, backend.OriginMigrationFile)
	stamped, err := backend.MigrateUserOrigins(ctx, st, path, cfg.UserOrigin, time.Now(), log.Printf)
	if err != nil {
		log.Printf("WARNING: Users: recording where the users of older sheds came from: %v", err)
	}
	if len(stamped) > 0 {
		log.Printf("Users: recorded where the users of %d older shed(s) came from: %s", len(stamped), strings.Join(stamped, ", "))
	}
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

// userSource is the user lookup serve builds, whichever kind it is: a
// users.ConfigLookup (no user list, or auth.users) or a directory.Runtime
// (auth.directory). Both front doors ask it; serve also asks it for the SSH
// mode and attempt cap, and starts its background refresh.
type userSource interface {
	users.Lookup
	users.Refresher
	Mode() string
	MaxAuthTries() int
	StartRefresh(ctx context.Context)
}

var (
	_ userSource = (*users.ConfigLookup)(nil)
	_ userSource = (*directory.Runtime)(nil)
)

// sshKeyAuth is the SSH server's key auth over l. A directory server's users
// change while it runs, so every authenticated connection is tracked by its
// user (closeOnPublication closes those of a user who leaves); no other
// server tracks any.
func sshKeyAuth(l userSource) sshd.KeyAuth {
	_, dir := l.(*directory.Runtime)
	return sshd.KeyAuth{
		Mode:             l.Mode(),
		MaxAuthTries:     l.MaxAuthTries(),
		Users:            l,
		TrackConnections: dir,
	}
}

// openAuditLog opens the server's audit log in dir when cfg turns it on
// (config.ServerConfig.AuditEnabled), and returns nil when it is off. A log
// that is required (auth.clients, or audit.enabled: true) and cannot be
// opened is an error: the server does not start. A log on only by the mtls
// default that cannot be opened is a warning (through logf), and the server
// starts without it, as it would have before the default.
func openAuditLog(cfg *config.ServerConfig, dir string, logf func(format string, args ...any)) (*audit.Log, error) {
	if !cfg.AuditEnabled() {
		return nil, nil
	}
	path := filepath.Join(dir, audit.FileName)
	l, err := audit.Open(path, audit.DefaultMaxBytes, audit.DefaultKeep)
	if err != nil {
		if cfg.AuditRequired() {
			return nil, fmt.Errorf("audit: open %s: %w", path, err)
		}
		logf("WARNING: audit: the audit log %s, on by default in mtls mode, could not be opened (%v): starting without it, so no events are recorded and the activity feed is empty. audit.enabled: true makes it required; audit.enabled: false turns it off.", path, err)
		return nil, nil
	}
	logf("Audit log: %s", path)
	return l, nil
}

// attachAuditLog hands the one audit log to both front doors: the API server
// (service client requests, HTTPS execs, max_age reaps; and, through it, the
// access guard's lifecycle events and the delivery service's events) and the
// SSH server (a person's entry into a service client's shed). Called before
// either serves. A nil log (audit off) records nothing on either.
func attachAuditLog(l *audit.Log, apiServer *api.Server, sshServer *sshd.Server) {
	apiServer.SetAuditLog(l)
	sshServer.SetAuditLog(l)
}

// closeOnPublication makes every publication of rt's list (a pull, the end of
// its trust time, a withdrawal) close the SSH connections of users the new
// state no longer has.
func closeOnPublication(rt *directory.Runtime, s *sshd.Server) {
	rt.SetPublicationHook(func(p directory.Publication) { s.CloseConnectionsExcept(p.Has) })
}

// newUserLookup builds the user lookup both front doors ask. With
// auth.directory it is the directory's (newDirectoryUsers). Otherwise it is
// the SSH key allowlist (auth.ssh, default off = accept all) with every key
// the implicit user's, or, with auth.users, each user's keys. GitHub-seeded
// keys are cached in stateDir/github_keys, and each GitHub login is pinned to
// its numeric account id at its first good fetch, in the pin store next to
// the cache (a user's github_id, when configured, wins). Every fetch is
// bound to ctx.
func newUserLookup(ctx context.Context, cfg *config.ServerConfig, stateDir string) (userSource, error) {
	if cfg.DirectoryConfigured() {
		return newDirectoryUsers(cfg, stateDir)
	}
	var list map[string]config.UserConfig
	if cfg.UsersConfigured() {
		list = cfg.Auth.Users
	}
	l, err := users.NewConfigLookup(users.ConfigOptions{
		SSH:            cfg.EffectiveSSHAuth(),
		GitHubCacheDir: filepath.Join(stateDir, "github_keys"),
		PinsPath:       filepath.Join(stateDir, users.PinsFileName),
		Context:        ctx,
		Users:          list,
	})
	if err != nil {
		return nil, err
	}
	return l, nil
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
