package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// SSHBackend is the interface for performing SSH key operations on the host.
type SSHBackend interface {
	// List returns the available SSH public keys.
	List() ([]*agent.Key, error)

	// Sign signs the data with the specified key.
	Sign(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error)

	// Mode returns the backend mode name ("agent-forward" or "local-keys").
	Mode() string
}

// ResolveSSHBackend auto-detects and returns the appropriate SSH backend.
// If the config specifies a mode, that mode is used. Otherwise:
//   - If SSH_AUTH_SOCK is set and the socket is reachable, agent-forward mode
//   - Otherwise, local-keys mode
func ResolveSSHBackend(cfg SSHConfig, logger *slog.Logger) (SSHBackend, error) {
	switch cfg.Mode {
	case "agent-forward":
		return newAgentForwardBackend(logger)
	case "local-keys":
		return newLocalKeysBackend(logger)
	case "":
		// Auto-detect
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			if conn, err := net.Dial("unix", sock); err == nil {
				conn.Close()
				logger.Info("auto-detected SSH backend", "mode", "agent-forward", "socket", sock)
				return newAgentForwardBackend(logger)
			}
		}
		logger.Info("auto-detected SSH backend", "mode", "local-keys")
		return newLocalKeysBackend(logger)
	default:
		return nil, fmt.Errorf("unknown ssh mode: %q (expected agent-forward, local-keys, or empty)", cfg.Mode)
	}
}
