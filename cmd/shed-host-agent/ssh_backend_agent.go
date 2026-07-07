package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// agentForwardBackend proxies SSH operations to the host's existing SSH agent
// (via SSH_AUTH_SOCK). This supports Secretive, 1Password, ssh-agent, etc.
type agentForwardBackend struct {
	socketPath string
	logger     *slog.Logger
}

func newAgentForwardBackend(logger *slog.Logger) (*agentForwardBackend, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK not set")
	}
	return &agentForwardBackend{
		socketPath: sock,
		logger:     logger,
	}, nil
}

func (b *agentForwardBackend) List() ([]*agent.Key, error) {
	a, conn, err := b.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return a.List()
}

func (b *agentForwardBackend) Sign(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	a, conn, err := b.connect()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if flags != 0 {
		return a.SignWithFlags(key, data, flags)
	}
	return a.Sign(key, data)
}

func (b *agentForwardBackend) Mode() string { return "agent-forward" }

// connect opens a new connection to the host SSH agent.
// Each operation gets a fresh connection for simplicity and reliability.
func (b *agentForwardBackend) connect() (agent.ExtendedAgent, net.Conn, error) {
	conn, err := net.Dial("unix", b.socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to SSH agent at %s: %w", b.socketPath, err)
	}
	return agent.NewClient(conn), conn, nil
}
