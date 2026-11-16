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
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

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
	}

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
				if config.IsReservedShedName(conn.User()) {
					log.Printf("SSH auth DENIED: user=%s fingerprint=%s (reserved username)", conn.User(), gossh.FingerprintSHA256(key))
					return nil, errReservedUser
				}
				setConnIdentity(ctx, key, conn.User())
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
					log.Printf("SSH auth: public-key attempt cap (%d) reached for user=%s — the client may be offering more keys than the cap allows, so its allowlisted key may never be tried. Raise auth.ssh.max_auth_tries, or use a per-host IdentityFile + IdentitiesOnly on the client.", maxTries, conn.User())
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

// Shutdown gracefully shuts down the SSH server.
func (s *Server) Shutdown(ctx context.Context) error {
	log.Printf("Shutting down SSH server...")
	return s.sshServer.Shutdown(ctx)
}

// defaultMaxAuthTries is the per-connection public-key attempt cap applied when
// auth.ssh.max_auth_tries is unset. It is deliberately higher than OpenSSH's
// default of 6 so a developer whose agent holds many keys (1Password, Secretive,
// hardware) can still have the one allowlisted key tried before the server gives
// up — which is exactly the path the host-agent bootstrap and `shed server add`
// take. This is a mild loosening of a DoS cap; public-key auth is not guessable,
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

	// The username git is reserved (no shed may have that name), and it is
	// refused here first: in every mode, with or without a user list, before
	// any key lookup or GitHub key refetch. The VerifiedPublicKeyCallback
	// checks it again on the username the signature was made for, and every
	// channel refuses it once more where it turns the username into a shed
	// (shedNameFor). The match is exact: the default signature the user's
	// machine gives out is for exactly "git". A case variant ("Git") is not a
	// valid shed name, and shedNameFor refuses it before any shed lookup.
	if config.IsReservedShedName(user) {
		log.Printf("SSH auth DENIED: user=%s fingerprint=%s (reserved username)", user, fingerprint)
		return false
	}

	if s.keyAuth.off() {
		log.Printf("SSH auth attempt: user=%s fingerprint=%s (allowlist off)", user, fingerprint)
		return true
	}

	// On a server with a user list a user's key is the plain key: an SSH
	// certificate would be a second identity for whoever holds the key in
	// it (the lookup resolves none to a user either).
	if _, cert := key.(*gossh.Certificate); cert && users.HasUserList(s.keyAuth.Users) {
		log.Printf("SSH auth DENIED: user=%s fingerprint=%s (an SSH certificate; this server's users log in with plain keys)", user, fingerprint)
		return false
	}
	if s.keyAuth.expired(fingerprint) {
		log.Printf("SSH auth DENIED: user=%s fingerprint=%s (the key has expired)", user, fingerprint)
		return false
	}
	authorized := s.keyAuth.known(fingerprint)
	if !authorized && s.refetchUnknownKey(ctx, user, fingerprint) {
		log.Printf("SSH auth: key %s is known after fetching the GitHub logins again", fingerprint)
		authorized = true
	}
	if s.keyAuth.enforced() && !authorized {
		log.Printf("SSH auth DENIED: user=%s fingerprint=%s", user, fingerprint)
		return false
	}
	// Reaches here only in warn mode, or enforce mode with an authorized key.
	if authorized {
		log.Printf("SSH auth allowed: user=%s fingerprint=%s", user, fingerprint)
	} else {
		log.Printf("SSH auth WOULD DENY (warn mode): user=%s fingerprint=%s", user, fingerprint)
	}
	return true
}

// refetchUnknownKey asks the user lookup to look for a key it does not know on
// GitHub, and reports whether the key is known afterwards. Only in enforce
// mode (warn lets the key in anyway; off looks nothing up), only when the
// lookup can fetch (a users.UnknownKeyRefetcher), and only when the login is
// `_bootstrap` (`shed server add`) or a valid shed name. That check is on the
// name's syntax alone: nothing about sheds is looked up for a peer that has
// not authenticated. The lookup then allows one fetch a minute for the whole
// server, checked before it does anything else; only the login that takes
// that slot waits for the fetch (up to five seconds), every other returns at
// once. A key the lookup knows (an expired one included) never gets here, and
// the lookup refuses a key it dropped for a GitHub account mismatch.
func (s *Server) refetchUnknownKey(ctx ssh.Context, user, fingerprint string) bool {
	if !s.keyAuth.enforced() {
		return false
	}
	if user != reservedBootstrapUser && config.ValidateShedName(user) != nil {
		return false
	}
	r, ok := s.keyAuth.Users.(users.UnknownKeyRefetcher)
	if !ok {
		return false
	}
	return r.RefetchForUnknownKey(ctx, fingerprint)
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
	log.Printf("Port forward: user=%s dest=%s:%d", user, d.DestAddr, d.DestPort)

	// Only allow localhost destinations (security: prevent using shed as proxy)
	if d.DestAddr != "localhost" && d.DestAddr != "127.0.0.1" && d.DestAddr != "::1" {
		log.Printf("Port forward denied: non-localhost destination %s for user %s", d.DestAddr, user)
		_ = newChan.Reject(gossh.Prohibited, "only localhost forwarding allowed")
		return
	}

	// The channel's principal, from the key the connection authenticated
	// with, resolved again for this channel.
	p, err := s.principalFor(ctx)
	if err != nil {
		log.Printf("Port forward refused: user=%s: %v", user, err)
		_ = newChan.Reject(gossh.Prohibited, err.Error())
		return
	}
	bctx := backend.WithPrincipal(ctx, p)

	// The username names the shed; one that cannot (git, a case variant, a
	// special name) is answered as an unknown shed is, before any lookup.
	name, ok := shedNameFor(user)
	if !ok {
		log.Printf("Port forward refused: user=%s is not a shed name", user)
		_ = newChan.Reject(gossh.ConnectionFailed, "shed not found")
		return
	}

	// Look up the shed and check the right to enter it.
	shed, err := s.guard().Check(bctx, name, backend.ActionEnter)
	if err != nil {
		log.Printf("Port forward refused: user=%s principal=%s: %v", user, p, err)
		if errors.Is(err, config.ErrForbiddenSentinel) {
			_ = newChan.Reject(gossh.Prohibited, forbiddenMessage(user, err))
			return
		}
		_ = newChan.Reject(gossh.ConnectionFailed, "shed not found")
		return
	}
	bctx = backend.PinCreatedAt(bctx, shed.CreatedAt)

	// Validate port range (SSH protocol uses uint32, TCP ports are uint16).
	if d.DestPort > 65535 {
		log.Printf("Port forward denied: invalid port %d for user %s", d.DestPort, user)
		_ = newChan.Reject(gossh.ConnectionFailed, "invalid port number")
		return
	}

	// Connect to the service inside the VM via DialService.
	dialCtx, dialCancel := context.WithTimeout(bctx, 30*time.Second)
	defer dialCancel()
	dconn, err := s.backend.DialService(dialCtx, shed.Name, uint16(d.DestPort))
	if err != nil {
		log.Printf("Port forward: DialService failed for %s port %d: %v", user, d.DestPort, err)
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

	log.Printf("Port forward established: %s -> %s:%d", user, shed.Name, d.DestPort)

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
