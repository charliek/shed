package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
)

// bootstrapSSHUser mirrors the server's reserved SSH username (internal/sshd
// reservedBootstrapUser): connecting as it mints + returns an HTTP token bundle.
const bootstrapSSHUser = "_bootstrap"

// bootstrapFn is the bootstrap entry point used by the token-refresh path,
// overridable in tests to avoid spawning a real ssh subprocess.
var bootstrapFn = bootstrapServer

// bootstrapSSHArgs builds the `ssh` argv for a bootstrap exchange: connect as
// the reserved _bootstrap user against the already-pinned known_hosts (strict,
// non-interactive), and run "<scope> [<clientKind>]" so the server mints and
// returns a token bundle. No PTY (-T) so the JSON bundle lands clean on stdout.
//
// scope must precede clientKind — the server parses Fields()[0] as the scope.
func bootstrapSSHArgs(host string, sshPort int, knownHostsPath, scope, clientKind string) []string {
	args := []string{
		"-T",
		"-p", strconv.Itoa(sshPort),
		"-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "StrictHostKeyChecking=yes",
		bootstrapSSHUser + "@" + host,
		scope,
	}
	if clientKind != "" {
		args = append(args, clientKind)
	}
	return args
}

// bootstrapServer runs the SSH bootstrap exchange and returns the minted token
// bundle. The server's SSH host key must already be pinned in known_hosts
// (StrictHostKeyChecking=yes), so this never trusts a new key — the host-key
// trust decision stays with confirmHostKey, run earlier in `server add`.
func bootstrapServer(host string, sshPort int, scope, clientKind string) (sdk.Bundle, error) {
	args := bootstrapSSHArgs(host, sshPort, config.GetKnownHostsPath(), scope, clientKind)
	cmd := exec.Command("ssh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return sdk.Bundle{}, fmt.Errorf("bootstrap over ssh failed: %s", msg)
		}
		return sdk.Bundle{}, fmt.Errorf("bootstrap over ssh failed: %w", err)
	}
	var b sdk.Bundle
	if err := json.Unmarshal(stdout.Bytes(), &b); err != nil {
		return sdk.Bundle{}, fmt.Errorf("bootstrap returned invalid JSON: %w", err)
	}
	if b.Token == "" {
		return sdk.Bundle{}, errors.New("bootstrap returned an empty token")
	}
	return b, nil
}
