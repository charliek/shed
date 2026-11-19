// Package sshd provides an SSH server for connecting to shed containers.
package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/terminal"
	"github.com/charliek/shed/internal/users"
)

// Server is an SSH server that connects users to shed containers.
type Server struct {
	sshServer *ssh.Server
	// backend is always a *backend.Guard (or nil in tests that open no
	// channel): every call is decided by the access rule for the principal
	// each channel resolves from its connection's key (principalFor).
	backend     backend.Backend
	hostKeyPath string
	listenAddr  string
	hostKey     gossh.Signer
	listener    net.Listener
	termConfig  *terminal.Config
	keyAuth     KeyAuth
	bootstrap   *BootstrapInfo // static bundle metadata; nil until SetBootstrap
	// audit is the audit log a person's entry into a service client's shed
	// is recorded in (audit.go); nil records nothing. Set before Start.
	audit *audit.Log
	// entries holds each entry into a service client's shed from just
	// before its guarded exec or dial until its line is written (or it
	// failed): Shutdown waits for them (beginEntry).
	entries audit.Pending

	// connMu guards conns: the authenticated connections by the name of the
	// user each is bound to, kept only with KeyAuth.TrackConnections
	// (CloseConnectionsExcept). Empty otherwise.
	connMu sync.Mutex
	conns  map[string]map[*trackedConn]struct{}
	// beforeTrack, when set (by tests only), runs on an authenticating
	// connection after its identity is resolved and before it is tracked.
	beforeTrack func()
	// afterClose, when set (by tests only), runs at the end of every close
	// of a tracked connection (trackedConn.Close).
	afterClose func()
	// afterPreauthClose, when set (by tests only), runs after the first
	// close of every connection the server admitted (preauthConn.shut), once
	// its "SSH auth failed" line, if any, has been written (closeLogged).
	afterPreauthClose func()

	// The limits on connections that have not authenticated (preauth.go):
	// the login grace, the cap and each source's share of it. NewServer sets
	// the fixed defaults; only tests change them (before the server serves),
	// with afterFunc (the grace timer) and now (the clock of the cap's log
	// lines).
	loginGrace                  time.Duration
	maxUnauthenticated          int
	maxUnauthenticatedPerSource int
	afterFunc                   func(time.Duration, func()) graceTimer
	now                         func() time.Time
	// preMu guards the counts of unauthenticated connections, in all and by
	// source, and when a cap refusal was last logged: per source in
	// preLoggedSource, and in the order written in preLoggedOrder, which
	// preauthMayLog drops old records from.
	preMu           sync.Mutex
	preTotal        int
	preBySource     map[string]int
	preLoggedSource map[string]time.Time
	preLoggedOrder  []preauthLogged
	preLoggedTotal  time.Time

	// life is the server's lifetime: made by NewServer, ended by Shutdown.
	// The off-path check of a shed name's unknown key (unknownshed.go) runs
	// under it, never under a connection's context.
	life       context.Context
	lifeCancel context.CancelFunc
	// checking is held from the start of a check until its backend call
	// returns (at most one at a time). checkMu orders the start of a check
	// against Shutdown, and guards checkDone, closed when the last check
	// started has ended.
	checking  atomic.Bool
	checkMu   sync.Mutex
	checkDone chan struct{}
	// nudgeMu is held while a check, having found the shed, checks the
	// lifetime and nudges the lookup, and by Shutdown while it ends the
	// lifetime (endLife), so no nudge is asked once Shutdown has begun. It
	// is taken before checkMu where both are held; the login path never
	// takes it.
	nudgeMu sync.Mutex
	// shedCheckTimeout bounds one check (defaultShedCheckTimeout; tests
	// change it before the server serves).
	shedCheckTimeout time.Duration
	// afterShedFound, when set (by tests only, before the server serves),
	// runs in a check once the backend has found the shed, before the
	// nudge's lifetime check.
	afterShedFound func()
	// statsMu guards stats, the counts of shed-name offers logged once per
	// statsEvery (unknownShedStatsEvery) by a goroutine statsOnce starts.
	statsMu    sync.Mutex
	stats      unknownShedStats
	statsOnce  sync.Once
	statsEvery time.Duration
}

// KeyAuth is how the SSH server decides public-key auth: the auth.ssh mode,
// the per-connection attempt cap, and the user seam that says whose a key is.
// The zero value is off (accept every key), the legacy behavior.
type KeyAuth struct {
	// Mode is config.SSHAuthOff, SSHAuthWarn or SSHAuthEnforce; "" is off.
	Mode string
	// MaxAuthTries caps public-key attempts per connection; 0 applies
	// defaultMaxAuthTries.
	MaxAuthTries int
	// Users resolves an offered key's fingerprint to its user. nil resolves
	// nothing, so enforce mode then accepts no key.
	Users users.Lookup
	// TrackConnections keeps every authenticated connection by the name of
	// the user it is bound to, so that CloseConnectionsExcept can close a
	// user's connections when they stop being a user (a directory server,
	// whose list changes while it runs). Off by default: then no connection
	// is wrapped or kept, and authentication runs as it always has.
	TrackConnections bool
}

// enforced reports whether keys the lookup does not resolve are refused: in
// enforce mode, and always on a server with a user list, where an unknown key
// is nobody's whatever the mode says (config load allows a user list in mtls
// only, which forces enforce; this holds for any caller).
func (k KeyAuth) enforced() bool {
	return k.Mode == config.SSHAuthEnforce || users.HasUserList(k.Users)
}

// off reports whether every key is accepted without a lookup: off mode, on a
// server with no user list.
func (k KeyAuth) off() bool {
	return (k.Mode == "" || k.Mode == config.SSHAuthOff) && !users.HasUserList(k.Users)
}

// known reports whether the lookup resolves the key's fingerprint to a user
// and the key has not expired.
func (k KeyAuth) known(fingerprint string) bool { return users.HasKey(k.Users, fingerprint) }

// script reports whether the lookup resolves the key's fingerprint to a script
// key (users.Key.Script).
func (k KeyAuth) script(fingerprint string) bool {
	if k.Users == nil {
		return false
	}
	_, key, ok := k.Users.UserForKey(fingerprint)
	return ok && key.Script
}

// expired reports whether the lookup resolves the key's fingerprint to a key
// whose expiry has passed. Such a key is refused, and never makes the server
// look for keys on GitHub.
func (k KeyAuth) expired(fingerprint string) bool {
	if k.Users == nil {
		return false
	}
	_, key, ok := k.Users.UserForKey(fingerprint)
	return ok && key.Expired(time.Now())
}

// owner describes, for an auth log line, the user the lookup resolves the
// key's fingerprint to now (ownerSuffix). Empty for a key the lookup does
// not resolve, so an unknown key's line says nothing more than before.
// Called only where the auth path has looked the key up already. The
// allowed line does not use it: it describes the user the connection was
// bound to (connIdentity.owner), which a later lookup may no longer give.
func (k KeyAuth) owner(fingerprint string) string {
	if k.Users == nil {
		return ""
	}
	u, key, ok := k.Users.UserForKey(fingerprint)
	if !ok {
		return ""
	}
	return ownerSuffix(u.Implicit, u.Name, u.Admin, key.Label)
}

// ownerSuffix is the end of an auth log line that names a key's user: the
// user's name and whether they are an admin, for a named user (the implicit
// user of a server with no user list is not named), and the key's label when
// it has one. A script key reads as its user's key.
func ownerSuffix(implicit bool, name string, admin bool, label string) string {
	var out string
	if !implicit {
		out = fmt.Sprintf(" key_user=%q admin=%t", name, admin)
	}
	if label != "" {
		out += fmt.Sprintf(" label=%q", label)
	}
	return out
}

// logUser is a requested SSH username as an auth log line prints it. The
// client chooses that name before it authenticates, so it may hold a newline,
// an escape sequence or a bidirectional override that would forge or disguise
// a log line. A name made only of the characters a shed name or a special
// name (git, _bootstrap, _api) can contain (a-z, 0-9, '-', '_') is printed as
// it is, so user=box and user=_bootstrap read as they always have; any other
// name, the empty one included, is printed quoted (%q).
func logUser(name string) string {
	plain := name != ""
	for i := 0; i < len(name) && plain; i++ {
		c := name[i]
		plain = 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_'
	}
	if plain {
		return name
	}
	return fmt.Sprintf("%q", name)
}

// logRemote is a connection's peer address as an auth log line prints it
// (remote=<host:port>), so a refused or accepted login can be traced, and
// blocked, by the address it came from. It is the transport's own peer
// address, which the client does not write the way it writes a username; "-"
// when there is none on record.
func logRemote(addr net.Addr) string {
	if addr == nil {
		return "-"
	}
	return addr.String()
}

// NewServer creates a new SSH server. listenAddr is the TCP bind address
// (e.g. "127.0.0.1:2222" for loopback (the default), "<tailnet-ip>:2222", or
// ":2222" for all interfaces).
// keyAuth gates public-key auth; its zero value (mode off) is the legacy
// accept-all behavior.
//
// b is wrapped in a backend.Guard unless it is one already (shed-server passes
// the one Guard the API server shares).
func NewServer(b backend.Backend, hostKeyPath string, listenAddr string, termConfig *terminal.Config, keyAuth KeyAuth) (*Server, error) {
	var guarded backend.Backend
	if b != nil {
		guarded = backend.Guarded(b)
	}
	s := &Server{
		backend:     guarded,
		hostKeyPath: hostKeyPath,
		listenAddr:  listenAddr,
		termConfig:  termConfig,
		keyAuth:     keyAuth,

		loginGrace:                  defaultLoginGrace,
		maxUnauthenticated:          defaultMaxUnauthenticated,
		maxUnauthenticatedPerSource: defaultMaxUnauthenticatedPerSource,
		afterFunc:                   realAfterFunc,
		now:                         time.Now,

		shedCheckTimeout: defaultShedCheckTimeout,
		statsEvery:       unknownShedStatsEvery,
	}
	s.life, s.lifeCancel = context.WithCancel(context.Background())

	// Load or generate the host key.
	hostKey, err := s.loadOrGenerateHostKey()
	if err != nil {
		return nil, fmt.Errorf("failed to load or generate host key: %w", err)
	}
	s.hostKey = hostKey

	// Create the SSH server.
	s.sshServer = &ssh.Server{
		Addr: listenAddr,
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			return s.handlePublicKey(ctx, key)
		},
		ServerConfigCallback: func(ctx ssh.Context) *gossh.ServerConfig {
			maxTries := s.effectiveMaxAuthTries()
			cfg := &gossh.ServerConfig{MaxAuthTries: maxTries}
			// x/crypto asks for the banner once, on the connection's first
			// authentication request, with the connection's live metadata:
			// kept so handlePublicKey can read each request's own username
			// (requestUser). No banner is sent.
			cfg.BannerCallback = func(conn gossh.ConnMetadata) string {
				ctx.SetValue(authConnKey{}, conn)
				return ""
			}
			// The connection's identity is the key whose signature x/crypto
			// verified, recorded only then (connIdentity). It is never taken
			// from a key the client merely asked about: a client may offer
			// several keys, and have more than one accepted, before it signs.
			//
			// conn.User() here is the username the signature was made for,
			// the one the connection authenticates as. It is the
			// authoritative check of the reserved username: a client may ask
			// about a key as another username and then sign as git on the same
			// connection, and handlePublicKey alone cannot be relied on to see
			// that switch (requestUser).
			cfg.VerifiedPublicKeyCallback = func(conn gossh.ConnMetadata, key gossh.PublicKey, perms *gossh.Permissions, _ string) (*gossh.Permissions, error) {
				remote := logRemote(conn.RemoteAddr())
				if config.IsReservedShedName(conn.User()) {
					log.Printf("SSH auth DENIED: user=%s fingerprint=%s remote=%s (reserved username)", logUser(conn.User()), gossh.FingerprintSHA256(key), remote)
					return nil, errReservedUser
				}
				// The identity records the user the key resolves to now,
				// which every later channel on the connection must still
				// find (principalFor). A key that resolves to nobody now
				// (it left the lookup since it was offered) does not
				// authenticate.
				id, err := s.newConnIdentity(key, conn.User())
				if err != nil {
					log.Printf("SSH auth DENIED: user=%s fingerprint=%s remote=%s (%v)", logUser(conn.User()), gossh.FingerprintSHA256(key), remote, err)
					return nil, err
				}
				setConnIdentity(ctx, id)
				if s.keyAuth.TrackConnections {
					if err := s.trackAuthenticated(ctx, id); err != nil {
						log.Printf("SSH auth DENIED: user=%s fingerprint=%s remote=%s (%v)", logUser(conn.User()), id.fingerprint, remote, err)
						return nil, err
					}
				}
				// The connection is authenticated: its login grace stops
				// and it leaves the unauthenticated counts. One whose grace
				// ended first, or that closed, is being closed and does not
				// authenticate.
				if err := preauthAuthenticated(ctx); err != nil {
					log.Printf("SSH auth DENIED: user=%s fingerprint=%s remote=%s (%v)", logUser(conn.User()), id.fingerprint, remote, err)
					return nil, err
				}
				// The one line that says a login happened: the client has
				// proved it holds this key (x/crypto verified its signature)
				// and the connection authenticates with it, in every mode.
				// A key the client only offered is never logged as allowed
				// (handlePublicKey).
				// The owner it names is the user this connection was bound to
				// above, from that same lookup, never a second one that a
				// list change since could answer differently.
				log.Printf("SSH auth allowed: user=%s fingerprint=%s remote=%s%s", logUser(id.user), id.fingerprint, remote, id.owner)
				return perms, nil
			}
			// Best-effort diagnostic from the transport's authoritative per-attempt
			// log (one closure per connection): warn once when a connection exhausts
			// the public-key attempt cap — usually a many-key agent (1Password,
			// Secretive) offering more keys than the cap before its allowlisted key
			// is reached. Kept here, not in handlePublicKey, so the per-key handler
			// stays a pure decision and the count comes from gossh, not a shadow.
			var pubkeyFails int
			cfg.AuthLogCallback = func(conn gossh.ConnMetadata, method string, err error) {
				if method != "publickey" || err == nil {
					return
				}
				if pubkeyFails++; pubkeyFails == maxTries {
					log.Printf("SSH auth: public-key attempt cap (%d) reached for user=%s remote=%s — the client may be offering more keys than the cap allows, so its allowlisted key may never be tried. Raise auth.ssh.max_auth_tries, or use a per-host IdentityFile + IdentitiesOnly on the client.", maxTries, logUser(conn.User()), logRemote(conn.RemoteAddr()))
				}
			}
			return cfg
		},
		Handler: func(sess ssh.Session) {
			s.handleSession(sess)
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": s.handleDirectTCPIP,
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": func(sess ssh.Session) {
				s.handleSFTP(sess)
			},
		},
	}

	// Every connection, in every mode: the login grace, the cap and its
	// per-source share, and the "SSH auth failed" line (preauth.go). It then
	// applies wrapConn when connections are tracked.
	s.sshServer.ConnCallback = s.admitConn

	// Add the host key to the server.
	s.sshServer.AddHostKey(hostKey)

	return s, nil
}

// loadOrGenerateHostKey loads an ED25519 host key from the configured path,
// or generates a new one if it doesn't exist.
func (s *Server) loadOrGenerateHostKey() (gossh.Signer, error) {
	// Check if the key file exists.
	keyData, err := os.ReadFile(s.hostKeyPath)
	if err == nil {
		// Key exists, parse it.
		signer, err := gossh.ParsePrivateKey(keyData)
		if err != nil {
			return nil, fmt.Errorf("failed to parse existing host key: %w", err)
		}
		log.Printf("Loaded existing host key from %s", s.hostKeyPath)
		return signer, nil
	}

	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read host key file: %w", err)
	}

	// Key doesn't exist, generate a new one.
	log.Printf("Generating new ED25519 host key...")
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ED25519 key: %w", err)
	}

	// Convert to OpenSSH format.
	signer, err := gossh.NewSignerFromKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %w", err)
	}

	// Marshal the private key to PEM format.
	pemBlock, err := gossh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}

	pemData := pem.EncodeToMemory(pemBlock)

	// Ensure the directory exists.
	dir := filepath.Dir(s.hostKeyPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	// Write the private key file with restricted permissions.
	if err := os.WriteFile(s.hostKeyPath, pemData, 0600); err != nil {
		return nil, fmt.Errorf("failed to write host key: %w", err)
	}

	log.Printf("Generated new host key: %s", s.hostKeyPath)
	log.Printf("Public key fingerprint: %s", gossh.FingerprintSHA256(signer.PublicKey()))

	// Also save the public key for convenience.
	pubKeyPath := s.hostKeyPath + ".pub"
	pubKeyData := gossh.MarshalAuthorizedKey(signer.PublicKey())
	if err := os.WriteFile(pubKeyPath, pubKeyData, 0644); err != nil {
		log.Printf("Warning: failed to write public key file: %v", err)
	}

	_ = pubKey // Silence unused variable warning.

	return signer, nil
}

// GetHostPublicKey returns the SSH public key in authorized_keys format.
func (s *Server) GetHostPublicKey() string {
	if s.hostKey == nil {
		return ""
	}
	return string(gossh.MarshalAuthorizedKey(s.hostKey.PublicKey()))
}

// errBannerConfigured refuses to serve with a login banner configured: see
// checkNoBanner.
var errBannerConfigured = errors.New("an SSH login banner (Banner or BannerHandler) is configured; it would replace the BannerCallback that gives each authentication request's own username to handlePublicKey (requestUser)")

// checkNoBanner fails when a login banner is configured on the gliderlabs
// server. gliderlabs installs its own BannerCallback for one, replacing the
// one NewServer sets, and requestUser would then quietly fall back to the
// connection's first-seen username: a signed login as git is still refused
// (the VerifiedPublicKeyCallback), but the early refusal before any key
// lookup and the rule that git never makes the server fetch GitHub keys would
// be lost after a username change. A banner needs another way to see each
// request's username first.
func (s *Server) checkNoBanner() error {
	if s.sshServer.Banner != "" || s.sshServer.BannerHandler != nil {
		return errBannerConfigured
	}
	return nil
}

// Start begins listening for SSH connections.
func (s *Server) Start() error {
	if err := s.checkNoBanner(); err != nil {
		return err
	}
	addr := s.listenAddr
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	s.listener = listener

	log.Printf("SSH server listening on %s", addr)
	log.Printf("Host key fingerprint: %s", gossh.FingerprintSHA256(s.hostKey.PublicKey()))

	return s.sshServer.Serve(listener)
}

// Shutdown gracefully shuts down the SSH server. It first ends the server's
// lifetime, so the off-path check of a shed name's unknown key starts no more
// and asks for no pull; and it refuses new entries into service clients'
// sheds (CloseEntries). Only then does it wait on anything: it closes the
// listener and waits, until ctx ends, for the connections to end; then for
// the entries already being admitted to write their audit lines
// (beginEntry); then for a check still running. The audit log must be closed
// only after Shutdown returns. It logs nothing of its own: shed-server's
// shutdown writes the one "Shutting down SSH server..." line, in the same
// form as every other door's (cmd/shed-server shutdownDoors).
func (s *Server) Shutdown(ctx context.Context) error {
	s.endLife()
	// No new entry into a service client's shed starts from here (the
	// caller may have closed this gate already, CloseEntries).
	s.CloseEntries()
	// The listener closes at once; the wait is for the connections. A
	// begun entry's channel can outlive its connection, so the entries are
	// waited for on their own after it: each until its ssh line is written,
	// so the audit log the caller closes after Shutdown loses none. Drain
	// returns at once when none is left, even past ctx.
	err := s.sshServer.Shutdown(ctx)
	if derr := s.entries.Drain(ctx); err == nil {
		err = derr
	}
	if cerr := s.stopShedCheck(ctx); err == nil {
		err = cerr
	}
	return err
}

// CloseEntries refuses every later entry into a service client's shed (the
// channel is refused, saying the server is shutting down). It does not wait:
// Shutdown does. shed-server calls it first thing when its shutdown begins,
// before it waits on any server, so no other server's wait keeps this gate
// open; Shutdown calls it too.
func (s *Server) CloseEntries() { s.entries.Close() }

// defaultMaxAuthTries is the per-connection public-key attempt cap applied when
// auth.ssh.max_auth_tries is unset. It is deliberately higher than OpenSSH's
// default of 6 so a developer whose agent holds many keys (1Password, Secretive,
// hardware) can still have the one allowlisted key tried before the server gives
// up — which is exactly the path `shed server add` and the desktop app's
// broker bootstrap take. This is a mild loosening of a DoS cap; public-key auth is not guessable,
// so the risk is low. Operators can override it via auth.ssh.max_auth_tries.
const defaultMaxAuthTries = 10

// effectiveMaxAuthTries is the configured per-connection attempt cap, or
// defaultMaxAuthTries when unset.
func (s *Server) effectiveMaxAuthTries() int {
	if n := s.keyAuth.MaxAuthTries; n > 0 {
		return n
	}
	return defaultMaxAuthTries
}

// errReservedUser refuses a signed login as the reserved username git.
var errReservedUser = errors.New("reserved username")

// authConnKey is the connection-context key of the connection's live metadata
// during authentication (set by the BannerCallback).
type authConnKey struct{}

// requestUser is the username of the authentication request being decided.
// ctx.User() is not: gliderlabs records the username of the connection's
// first request there and never updates it, while SSH lets a client change
// usernames between requests. The live metadata the BannerCallback kept
// gives the current one; without it (a banner configured on the gliderlabs
// server, which replaces that callback and which Start refuses:
// checkNoBanner) this falls back to ctx.User(), and the
// VerifiedPublicKeyCallback still refuses a signed login as git.
func requestUser(ctx ssh.Context) string {
	if conn, ok := ctx.Value(authConnKey{}).(gossh.ConnMetadata); ok {
		return conn.User()
	}
	return ctx.User()
}

// handlePublicKey gates public-key auth through the user lookup: a key is
// authorized when the lookup resolves its fingerprint to a user and it has not
// expired (an expired key is refused in every mode the lookup is asked in). With
// key auth off (default), every key is accepted (legacy). In warn mode an
// unresolved key is logged but accepted; in enforce mode, and on any server
// with a user list, it is rejected. The username (which
// selects the shed) does not change who the key is — identity comes from the
// key, GitHub-style — except the reserved username git, which is refused
// whatever the key (config.IsReservedShedName). Otherwise applies uniformly
// to all usernames, including `_api`.
//
// This is the only way a connection authenticates: public key is the only
// method configured, so "none" is never accepted. In off mode a login as a
// username that names no shed is accepted here, through this handler, and is
// told the shed does not exist when it opens a channel.
//
// It decides only: it records nothing. A client may ask about several keys
// before it signs with one, so a key this accepts is not yet the connection's
// identity. The key whose signature x/crypto then verifies is recorded on the
// connection by the VerifiedPublicKeyCallback (connIdentity), and each channel
// resolves its principal from that (principalFor), so a key that leaves the
// lookup or expires stops opening new channels. identity_test.go pins that a
// key that was asked about but not used to sign never becomes the identity,
// of a session or of a certificate at _bootstrap.
func (s *Server) handlePublicKey(ctx ssh.Context, key ssh.PublicKey) bool {
	fingerprint := gossh.FingerprintSHA256(key)
	// The username of this request, not of the connection's first one: it
	// decides the reserved-name refusal, the refetch eligibility and the logs.
	user := requestUser(ctx)
	remote := logRemote(ctx.RemoteAddr())
	// Counted for the connection's "SSH auth failed" line, written if it
	// closes without proving a key (preauthConn.Close).
	noteOffer(ctx, user)

	// The username git is reserved (no shed may have that name), and it is
	// refused here first: in every mode, with or without a user list, before
	// any key lookup or GitHub key refetch. The VerifiedPublicKeyCallback
	// checks it again on the username the signature was made for, and every
	// channel refuses it once more where it turns the username into a shed
	// (shedNameFor). The match is exact: the default signature the user's
	// machine gives out is for exactly "git". A case variant ("Git") is not a
	// valid shed name, and shedNameFor refuses it before any shed lookup.
	if config.IsReservedShedName(user) {
		log.Printf("SSH auth DENIED: user=%s fingerprint=%s remote=%s (reserved username)", logUser(user), fingerprint, remote)
		return false
	}

	if s.keyAuth.off() {
		log.Printf("SSH auth attempt: user=%s fingerprint=%s remote=%s (allowlist off)", logUser(user), fingerprint, remote)
		return true
	}

	// On a server with a user list a user's key is the plain key: an SSH
	// certificate would be a second identity for whoever holds the key in
	// it (the lookup resolves none to a user either).
	if _, cert := key.(*gossh.Certificate); cert && users.HasUserList(s.keyAuth.Users) {
		log.Printf("SSH auth DENIED: user=%s fingerprint=%s remote=%s (an SSH certificate; this server's users log in with plain keys)", logUser(user), fingerprint, remote)
		return false
	}
	if s.keyAuth.expired(fingerprint) {
		log.Printf("SSH auth DENIED: user=%s fingerprint=%s remote=%s (the key has expired)%s", logUser(user), fingerprint, remote, s.keyAuth.owner(fingerprint))
		return false
	}
	authorized := s.keyAuth.known(fingerprint)
	if !authorized && s.refetchUnknownKey(ctx, user, fingerprint) {
		// A lookup whose users change while the server runs (a directory's
		// list) pulled its list; any other fetched GitHub logins.
		if _, live := s.keyAuth.Users.(users.BoundCredentialsOnly); live {
			log.Printf("SSH auth: key %s remote=%s is known after pulling the directory again", fingerprint, remote)
		} else {
			log.Printf("SSH auth: key %s remote=%s is known after fetching the GitHub logins again", fingerprint, remote)
		}
		authorized = true
	}
	if s.keyAuth.enforced() && !authorized {
		log.Printf("SSH auth DENIED: user=%s fingerprint=%s remote=%s", logUser(user), fingerprint, remote)
		return false
	}
	// Reaches here only in warn mode, or enforce mode with an authorized key.
	// An accepted key is not logged here: the client has only offered it, and
	// may never sign with it. "SSH auth allowed" is written once the client
	// has proved it holds the key (the VerifiedPublicKeyCallback).
	if !authorized {
		log.Printf("SSH auth WOULD DENY (warn mode): user=%s fingerprint=%s remote=%s", logUser(user), fingerprint, remote)
	}
	return true
}

// refetchUnknownKey asks the user lookup to look for a key it does not know
// (on GitHub, or in a directory's list), and reports whether the key is known
// on this attempt. Only in enforce mode (warn lets the key in anyway; off
// looks nothing up), and by the login name:
//
//   - `_bootstrap` (`shed server add`), on a lookup that can fetch (a
//     users.UnknownKeyRefetcher): the lookup takes its server-wide slot,
//     checked before it does anything else; only the login that takes it
//     waits for the fetch, every other returns at once. With GitHub logins
//     (auth.users) the slot is one fetch a minute and the wait up to five
//     seconds, and the lookup refuses a key it dropped for a GitHub account
//     mismatch. With a directory's list (auth.directory) the slot is one pull
//     a minute while pulls succeed, one per retry interval while they fail,
//     none inside a pending Retry-After. The login that takes it is let in as
//     soon as its key is known (from any pull), and if its key is still
//     unknown it is refused when its 8-second deadline ends
//     (directory.UnknownKeyWait), never earlier, whenever its pull ended.
//   - a valid shed name, on a lookup that can nudge (a
//     users.UnknownKeyNudger: a directory's list): refused at once, whether
//     a shed of that name exists or not, without waiting for anything about
//     the name. Off the login path the server checks whether it exists, and
//     only then nudges a pull on the lookup's shed-name slot (one every 5
//     minutes while pulls succeed, one per retry interval while they fail,
//     none inside a Retry-After); no login waits for it, and the person's
//     next attempt finds the key known (nudgeIfShed).
//   - a valid shed name on a lookup that can fetch and not nudge (GitHub
//     logins): as `_bootstrap`, on the name's syntax alone.
//   - any other name: nothing is asked, and the key is refused.
//
// What a peer whose key stays unknown can learn about which shed names
// exist: nothing from the shed-name login's answer, which never waits for
// the check. A shed-name pull can delay a later `_bootstrap` pull that queues
// behind it, but a `_bootstrap` login whose key stays unknown is answered at
// its fixed deadline, so that delay does not show. Except in three cases, in
// which whether a shed-name pull was sent changes when the `_bootstrap` slot
// may next be taken, so a peer probing then can tell, at most one guess per
// `_bootstrap` slot take: while pulls fail; while a Retry-After is pending;
// and when the shed-name pull itself fails while pulls had been succeeding
// (the server then enters the failed state, and a `_bootstrap` login that
// would have taken the slot and waited is refused at once). Apart from that,
// a person whose key is listed is let in when a pull brings it, so a pull
// already running ahead of theirs delays their own first login, which can
// show that some pull was in flight; that is accepted.
//
// A key the lookup knows (an expired one included) never gets here.
func (s *Server) refetchUnknownKey(ctx ssh.Context, user, fingerprint string) bool {
	if !s.keyAuth.enforced() {
		return false
	}
	if user == reservedBootstrapUser {
		r, ok := s.keyAuth.Users.(users.UnknownKeyRefetcher)
		return ok && r.RefetchForUnknownKey(ctx, fingerprint)
	}
	if config.ValidateShedName(user) != nil {
		return false
	}
	if n, ok := s.keyAuth.Users.(users.UnknownKeyNudger); ok {
		s.nudgeIfShed(n, user, fingerprint)
		return false
	}
	r, ok := s.keyAuth.Users.(users.UnknownKeyRefetcher)
	return ok && r.RefetchForUnknownKey(ctx, fingerprint)
}

// localForwardChannelData matches RFC4254 Section 7.2
type localForwardChannelData struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

// handleDirectTCPIP handles local port forwarding (-L) by proxying to the container.
func (s *Server) handleDirectTCPIP(srv *ssh.Server, conn *gossh.ServerConn,
	newChan gossh.NewChannel, ctx ssh.Context) {

	d := localForwardChannelData{}
	if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
		_ = newChan.Reject(gossh.ConnectionFailed, "error parsing forward data")
		return
	}

	// The username the connection authenticated as, as on every other
	// channel: ctx.User() is its first-seen username, which a client may
	// have changed before it signed.
	user := authenticatedUser(ctx)
	log.Printf("Port forward: user=%s dest=%q port=%d", logUser(user), d.DestAddr, d.DestPort)

	// Only allow localhost destinations (security: prevent using shed as proxy)
	if d.DestAddr != "localhost" && d.DestAddr != "127.0.0.1" && d.DestAddr != "::1" {
		log.Printf("Port forward denied: non-localhost destination %q for user %s", d.DestAddr, logUser(user))
		_ = newChan.Reject(gossh.Prohibited, "only localhost forwarding allowed")
		return
	}

	// The channel's principal, from the key the connection authenticated
	// with, resolved again for this channel.
	p, err := s.principalFor(ctx)
	if err != nil {
		log.Printf("Port forward refused: user=%s: %v", logUser(user), err)
		_ = newChan.Reject(gossh.Prohibited, err.Error())
		return
	}
	bctx := backend.WithPrincipal(ctx, p)

	// The username names the shed; one that cannot (git, a case variant, a
	// special name) is answered as an unknown shed is, before any lookup.
	name, ok := shedNameFor(user)
	if !ok {
		log.Printf("Port forward refused: user=%s is not a shed name", logUser(user))
		_ = newChan.Reject(gossh.ConnectionFailed, "shed not found")
		return
	}

	// Look up the shed and check the right to enter it.
	shed, err := s.guard().Check(bctx, name, backend.ActionEnter)
	if err != nil {
		log.Printf("Port forward refused: user=%s principal=%s: %v", logUser(user), p, err)
		if errors.Is(err, config.ErrForbiddenSentinel) {
			_ = newChan.Reject(gossh.Prohibited, forbiddenMessage(user, err))
			return
		}
		_ = newChan.Reject(gossh.ConnectionFailed, "shed not found")
		return
	}
	// The dial acts on the shed that was checked, with the owner it was
	// checked against (pinEntry).
	bctx = pinEntry(bctx, shed)

	// Validate port range (SSH protocol uses uint32, TCP ports are uint16).
	if d.DestPort > 65535 {
		log.Printf("Port forward denied: invalid port %d for user %s", d.DestPort, logUser(user))
		_ = newChan.Reject(gossh.ConnectionFailed, "invalid port number")
		return
	}

	// An entry into a service client's shed is held open in the server's
	// pending entries from here until its line is written (or it fails), so
	// a shutdown waits for that line before the audit log is closed; once
	// the shutdown has begun, no new one starts.
	id, _ := connIdentityFrom(ctx)
	// (A tunnel writes its line itself, below, so the exec hook beginEntry
	// also returns is not used, and needs no peer address.)
	_, endEntry, ok := s.beginEntry(p, id.fingerprint, nil, shed, audit.SSHChannelTunnel)
	if !ok {
		log.Printf("Port forward refused: user=%s: %v", logUser(user), errShuttingDown)
		_ = newChan.Reject(gossh.ConnectionFailed, errShuttingDown.Error())
		return
	}
	defer endEntry()

	// Connect to the service inside the VM via DialService.
	dialCtx, dialCancel := context.WithTimeout(bctx, 30*time.Second)
	defer dialCancel()
	dconn, err := s.backend.DialService(dialCtx, shed.Name, uint16(d.DestPort))
	if err != nil {
		log.Printf("Port forward: DialService failed for %s port %d: %v", logUser(user), d.DestPort, err)
		_ = newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}

	// Accept the SSH channel
	ch, reqs, err := newChan.Accept()
	if err != nil {
		dconn.Close()
		return
	}
	go gossh.DiscardRequests(reqs)

	// The entry is admitted: the guarded dial passed its own check and
	// connected, and the channel is open. Only now is it recorded (a
	// service client's shed only; auditEntry).
	s.auditEntry(p, id.fingerprint, conn.RemoteAddr(), shed, audit.SSHChannelTunnel, int(d.DestPort))
	endEntry()

	log.Printf("Port forward established: %s -> %s:%d", logUser(user), shed.Name, d.DestPort)

	// Bidirectional proxy
	go func() {
		defer ch.Close()
		defer dconn.Close()
		_, _ = io.Copy(ch, dconn)
	}()
	go func() {
		defer ch.Close()
		defer dconn.Close()
		_, _ = io.Copy(dconn, ch)
	}()
}
