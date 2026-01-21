// Package sshd provides an SSH server for connecting to shed containers.
package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/terminal"
)

// DockerClient defines the interface for docker operations needed by the SSH server.
// This allows the sshd package to compile independently of the docker package.
type DockerClient interface {
	// GetShed returns a shed by name, or an error if not found.
	GetShed(ctx context.Context, name string) (*ShedInfo, error)

	// StartShed starts a stopped shed.
	StartShed(ctx context.Context, name string) error

	// ExecInContainer executes a command in a container with the given options.
	ExecInContainer(ctx context.Context, containerID string, opts ExecOptions) error
}

// ShedInfo contains information about a shed needed by the SSH server.
type ShedInfo struct {
	Name        string
	Status      string
	ContainerID string
}

// ExecOptions contains options for executing a command in a container.
type ExecOptions struct {
	// Cmd is the command to execute. If empty, defaults to the container's shell.
	Cmd []string

	// Stdin, Stdout, Stderr are the I/O streams.
	Stdin  ReadCloser
	Stdout WriteCloser
	Stderr WriteCloser

	// TTY indicates whether to allocate a pseudo-TTY.
	TTY bool

	// Env contains additional environment variables.
	Env []string

	// InitialSize is the initial terminal size (if TTY is true).
	InitialSize *TerminalSize

	// ResizeChan receives terminal resize events.
	ResizeChan <-chan TerminalSize
}

// TerminalSize represents terminal dimensions.
type TerminalSize struct {
	Width  uint
	Height uint
}

// ReadCloser is an interface for reading with close capability.
type ReadCloser interface {
	Read(p []byte) (n int, err error)
	Close() error
}

// WriteCloser is an interface for writing with close capability.
type WriteCloser interface {
	Write(p []byte) (n int, err error)
	Close() error
}

// Server is an SSH server that connects users to shed containers.
type Server struct {
	sshServer   *ssh.Server
	docker      DockerClient
	hostKeyPath string
	port        int
	hostKey     gossh.Signer
	listener    net.Listener
	termConfig  *terminal.Config
}

// NewServer creates a new SSH server.
func NewServer(dockerClient DockerClient, hostKeyPath string, port int, termConfig *terminal.Config) (*Server, error) {
	s := &Server{
		docker:      dockerClient,
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
