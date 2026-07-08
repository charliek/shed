package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Standard SSH key filenames to look for.
var standardKeyFiles = []string{
	"id_ed25519",
	"id_rsa",
	"id_ecdsa",
}

// localKey holds a parsed SSH key pair.
type localKey struct {
	pubKey  ssh.PublicKey
	signer  ssh.Signer
	comment string
}

// localKeysBackend reads SSH keys directly from ~/.ssh/.
type localKeysBackend struct {
	keys   []localKey
	logger *slog.Logger
}

func newLocalKeysBackend(logger *slog.Logger) (*localKeysBackend, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("getting home directory: %w", err)
	}

	sshDir := filepath.Join(home, ".ssh")
	b := &localKeysBackend{logger: logger}

	for _, name := range standardKeyFiles {
		keyPath := filepath.Join(sshDir, name)
		data, err := os.ReadFile(keyPath)
		if err != nil {
			continue // Key file doesn't exist, skip
		}

		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			// Key might be encrypted — skip with warning for PoC
			logger.Warn("skipping SSH key (encrypted or invalid)", "path", keyPath, "error", err)
			continue
		}

		b.keys = append(b.keys, localKey{
			pubKey:  signer.PublicKey(),
			signer:  signer,
			comment: name,
		})
		logger.Info("loaded SSH key", "path", keyPath, "type", signer.PublicKey().Type())
	}

	if len(b.keys) == 0 {
		logger.Warn("no SSH keys found in ~/.ssh/")
	}

	return b, nil
}

func (b *localKeysBackend) List() ([]*agent.Key, error) {
	keys := make([]*agent.Key, len(b.keys))
	for i, k := range b.keys {
		keys[i] = &agent.Key{
			Format:  k.pubKey.Type(),
			Blob:    k.pubKey.Marshal(),
			Comment: k.comment,
		}
	}
	return keys, nil
}

func (b *localKeysBackend) Mode() string { return "local-keys" }

func (b *localKeysBackend) Sign(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	for _, k := range b.keys {
		if !keysEqual(key, k.pubKey) {
			continue
		}

		// Handle algorithm selection from flags
		var algorithm string
		switch {
		case flags&agent.SignatureFlagRsaSha256 != 0:
			algorithm = ssh.KeyAlgoRSASHA256
		case flags&agent.SignatureFlagRsaSha512 != 0:
			algorithm = ssh.KeyAlgoRSASHA512
		}

		if algorithm != "" {
			if as, ok := k.signer.(ssh.AlgorithmSigner); ok {
				return as.SignWithAlgorithm(rand.Reader, data, algorithm)
			}
		}

		return k.signer.Sign(rand.Reader, data)
	}

	return nil, fmt.Errorf("key not found")
}

// keysEqual compares two SSH public keys by their marshaled form.
func keysEqual(a, b ssh.PublicKey) bool {
	return bytes.Equal(a.Marshal(), b.Marshal())
}
