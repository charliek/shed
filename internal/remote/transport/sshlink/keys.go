package sshlink

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/transport"
)

// maxKeyFile caps how much of a key file is read. An OpenSSH ed25519 private
// key is under 1 KiB.
const maxKeyFile = 64 * 1024

// ParseClientKey parses the server's SSH private key: an unencrypted OpenSSH
// ed25519 key. One key serves a whole environment; only its public half
// reaches a guest, through instance metadata.
func ParseClientKey(pem []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, errors.New("the SSH client key is passphrase-protected; the server needs an unencrypted key")
		}
		// x/crypto's parse errors describe the format and never quote
		// key material.
		return nil, fmt.Errorf("parse SSH client key: %w", err)
	}
	if t := signer.PublicKey().Type(); t != ssh.KeyAlgoED25519 {
		return nil, fmt.Errorf("the SSH client key is %s; the server needs an %s key", t, ssh.KeyAlgoED25519)
	}
	return signer, nil
}

// LoadClientKey resolves the client key from the process environment the way
// the server config names it: keyEnv is a variable holding the base64 of the
// OpenSSH private key (remote.ssh.client_key_env), fileEnv a variable holding
// a path to it (remote.ssh.client_key_file_env). Exactly one must be set.
// Errors name the variable and never the value.
func LoadClientKey(keyEnv, fileEnv string) (ssh.Signer, error) {
	return loadClientKey(keyEnv, fileEnv, os.Getenv)
}

func loadClientKey(keyEnv, fileEnv string, getenv func(string) string) (ssh.Signer, error) {
	var inline, path string
	if keyEnv != "" {
		inline = getenv(keyEnv)
	}
	if fileEnv != "" {
		path = getenv(fileEnv)
	}
	names := strings.Join(nonEmpty(keyEnv, fileEnv), " or ")

	switch {
	case inline != "" && path != "":
		return nil, fmt.Errorf("both %s and %s are set; set exactly one", keyEnv, fileEnv)
	case inline != "":
		pem, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(inline), ""))
		if err != nil {
			return nil, fmt.Errorf("%s is not valid base64", keyEnv)
		}
		signer, err := ParseClientKey(pem)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", keyEnv, err)
		}
		return signer, nil
	case path != "":
		signer, err := ReadClientKeyFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fileEnv, err)
		}
		return signer, nil
	case names == "":
		return nil, errors.New("no SSH client key variable is configured")
	default:
		return nil, fmt.Errorf("the SSH client key is not set: set %s", names)
	}
}

// ReadClientKeyFile reads and parses a client key file.
func ReadClientKeyFile(path string) (ssh.Signer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st.Size() > maxKeyFile {
		return nil, fmt.Errorf("%s is %d bytes; a key file is under %d", path, st.Size(), maxKeyFile)
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseClientKey(pem)
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// AuthorizedKey returns a key's public half as one authorized_keys line with
// no comment and no newline: "ssh-ed25519 AAAA...". It is what goes into the
// instance metadata key guest.MetadataAuthorizedKeys.
func AuthorizedKey(signer ssh.Signer) string {
	return FormatHostKey(signer.PublicKey())
}

// FormatHostKey renders a public key the way pins and published host keys are
// stored: authorized_keys form, no comment, no newline.
func FormatHostKey(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// ParseHostKey parses a pinned or published host key. It accepts only an
// ed25519 key (guest.HostKeyType); a trailing comment is ignored, options are
// refused.
func ParseHostKey(line string) (ssh.PublicKey, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, errors.New("host key is empty")
	}
	if strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("host key holds more than one line")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, fmt.Errorf("parse host key: %w", err)
	}
	if len(options) > 0 || len(rest) > 0 {
		return nil, errors.New("host key has options or trailing data")
	}
	if key.Type() != guest.HostKeyType {
		return nil, fmt.Errorf("host key is %s; only %s is accepted", key.Type(), guest.HostKeyType)
	}
	return key, nil
}

// sameKey reports whether two public keys are the same key.
func sameKey(a, b ssh.PublicKey) bool {
	return a.Type() == b.Type() && bytes.Equal(a.Marshal(), b.Marshal())
}

// PinnedHostKey returns a transport.HostKeyStore that holds one fixed pin and
// never learns another. It serves callers that already know the guest's key:
// tests, the live check, and adoption of a shed whose pin was read from
// instance metadata. With an empty pin every connection fails with
// transport.ErrNoHostKey.
func PinnedHostKey(pin string) transport.HostKeyStore {
	return pinnedHostKey(strings.TrimSpace(pin))
}

type pinnedHostKey string

func (p pinnedHostKey) Pin(context.Context) (string, error) { return string(p), nil }

func (p pinnedHostKey) Learn(context.Context, string) error {
	return errors.New("this link has a fixed pin and learns no host key")
}
