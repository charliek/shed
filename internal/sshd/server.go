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
	"strconv"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/terminal"
)

// Server is an SSH server that connects users to shed containers.
type Server struct {
	sshServer   *ssh.Server
	backend     backend.Backend
	hostKeyPath string
	port        int
	hostKey     gossh.Signer
	listener    net.Listener
	termConfig  *terminal.Config
}

// NewServer creates a new SSH server.
func NewServer(b backend.Backend, hostKeyPath string, port int, termConfig *terminal.Config) (*Server, error) {
	s := &Server{
		backend:     b,
		hostKeyPath: hostKeyPath,
		port:        port,
		termConfig:  termConfig,
	}

	// Load or generate the host key.
	hostKey, err := s.loadOrGenerateHostKey()
	if err != nil {
		return nil, fmt.Errorf("failed to load or generate host key: %w", err)
	}
	s.hostKey = hostKey

	// Create the SSH server.
	s.sshServer = &ssh.Server{
		Addr: fmt.Sprintf(":%d", port),
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			return s.handlePublicKey(ctx, key)
		},
		Handler: func(sess ssh.Session) {
			s.handleSession(sess)
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": s.handleDirectTCPIP,
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
	addr := fmt.Sprintf(":%d", s.port)
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

// handlePublicKey handles public key authentication.
// For MVP, we accept all keys and just log the fingerprint.
func (s *Server) handlePublicKey(ctx ssh.Context, key ssh.PublicKey) bool {
	fingerprint := gossh.FingerprintSHA256(key)
	user := ctx.User()

	log.Printf("SSH auth attempt: user=%s fingerprint=%s", user, fingerprint)

	// For MVP, accept all keys.
	// TODO: Implement proper key verification against stored keys.
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

	// Get network endpoint to forward to
	containerIP, err := s.backend.GetNetworkEndpoint(ctx, shed.Name)
	if err != nil {
		log.Printf("Port forward: cannot get network endpoint for %s: %v", user, err)
		_ = newChan.Reject(gossh.ConnectionFailed, "cannot get network endpoint")
		return
	}

	// Connect to container's port
	dest := net.JoinHostPort(containerIP, strconv.FormatUint(uint64(d.DestPort), 10))
	dconn, err := net.DialTimeout("tcp", dest, 30*time.Second)
	if err != nil {
		log.Printf("Port forward: failed to connect to %s: %v", dest, err)
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

	log.Printf("Port forward established: %s -> %s", user, dest)

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
