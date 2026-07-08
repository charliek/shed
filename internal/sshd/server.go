// Package sshd provides an SSH server for connecting to shed containers.
package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/terminal"
)

// Server is an SSH server that connects users to shed containers.
type Server struct {
	sshServer   *ssh.Server
	backend     backend.Backend
	hostKeyPath string
	listenAddr  string
	hostKey     gossh.Signer
	listener    net.Listener
	termConfig  *terminal.Config
	allowlist   *KeyAllowlist
	tokens      *authtoken.Store // shared HTTP token store; nil until SetBootstrap
	bootstrap   *BootstrapInfo   // static bundle metadata; nil until SetBootstrap
}

// NewServer creates a new SSH server. listenAddr is the TCP bind address
// (e.g. "127.0.0.1:2222" for loopback (the default), "<tailnet-ip>:2222", or
// ":2222" for all interfaces).
// allowlist gates public-key auth; pass an "off"-mode allowlist (or one built
// from nil config) for the legacy accept-all behavior.
func NewServer(b backend.Backend, hostKeyPath string, listenAddr string, termConfig *terminal.Config, allowlist *KeyAllowlist) (*Server, error) {
	s := &Server{
		backend:     b,
		hostKeyPath: hostKeyPath,
		listenAddr:  listenAddr,
		termConfig:  termConfig,
		allowlist:   allowlist,
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
		ServerConfigCallback: func(ssh.Context) *gossh.ServerConfig {
			maxTries := s.effectiveMaxAuthTries()
			cfg := &gossh.ServerConfig{MaxAuthTries: maxTries}
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

// Start begins listening for SSH connections.
func (s *Server) Start() error {
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
	if s.allowlist != nil {
		if n := s.allowlist.MaxAuthTries(); n > 0 {
			return n
		}
	}
	return defaultMaxAuthTries
}

// handlePublicKey gates public-key auth through the allowlist. With the
// allowlist off (default), every key is accepted (legacy). In warn mode an
// unlisted key is logged but accepted; in enforce mode it is rejected. The
// username (which selects the shed) is unaffected — identity comes from the
// key, GitHub-style. Applies uniformly to all users, including `_api`.
func (s *Server) handlePublicKey(ctx ssh.Context, key ssh.PublicKey) bool {
	fingerprint := gossh.FingerprintSHA256(key)
	user := ctx.User()

	if s.allowlist == nil || s.allowlist.Mode() == config.SSHAuthOff {
		log.Printf("SSH auth attempt: user=%s fingerprint=%s (allowlist off)", user, fingerprint)
		return true
	}

	authorized := s.allowlist.IsAuthorized(key)
	if s.allowlist.Mode() == config.SSHAuthEnforce && !authorized {
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

	user := ctx.User()
	log.Printf("Port forward: user=%s dest=%s:%d", user, d.DestAddr, d.DestPort)

	// Only allow localhost destinations (security: prevent using shed as proxy)
	if d.DestAddr != "localhost" && d.DestAddr != "127.0.0.1" && d.DestAddr != "::1" {
		log.Printf("Port forward denied: non-localhost destination %s for user %s", d.DestAddr, user)
		_ = newChan.Reject(gossh.Prohibited, "only localhost forwarding allowed")
		return
	}

	// Look up container for this shed
	shed, err := s.backend.GetShed(ctx, user)
	if err != nil {
		log.Printf("Port forward: shed not found for user %s: %v", user, err)
		_ = newChan.Reject(gossh.ConnectionFailed, "shed not found")
		return
	}

	// Validate port range (SSH protocol uses uint32, TCP ports are uint16).
	if d.DestPort > 65535 {
		log.Printf("Port forward denied: invalid port %d for user %s", d.DestPort, user)
		_ = newChan.Reject(gossh.ConnectionFailed, "invalid port number")
		return
	}

	// Connect to the service inside the VM via DialService.
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
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
