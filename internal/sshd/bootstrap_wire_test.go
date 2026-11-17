package sshd

import (
	"bytes"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users/userstest"
)

// serveForTest starts s on a loopback port and returns its address. The
// listener closes with the test. It makes the start-up check Start makes.
func serveForTest(t *testing.T, s *Server) string {
	t.Helper()
	if err := s.checkNoBanner(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.sshServer.Serve(ln) }()
	t.Cleanup(func() { _ = s.sshServer.Close() })
	return ln.Addr().String()
}

// runBootstrap runs one `_bootstrap` session against addr with signer and
// returns its stdout, stderr and exit status.
func runBootstrap(t *testing.T, addr string, signer gossh.Signer, cmd string) (stdout, stderr string, status int) {
	t.Helper()
	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            reservedBootstrapUser,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	var out, errOut bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &errOut
	err = sess.Run(cmd)
	var exit *gossh.ExitError
	switch {
	case err == nil:
		status = 0
	case errors.As(err, &exit):
		status = exit.ExitStatus()
	default:
		t.Fatalf("run: %v", err)
	}
	return out.String(), errOut.String(), status
}

// TestBootstrapOpenServerRefusalOnTheWire is the released-client contract seen
// from the client's side of a real SSH session: an open server answers
// `_bootstrap` with exactly this stderr line, exit status 1 and no stdout.
// Every released shed CLI recognises an open server by matching that line as a
// whole (cmd/shed/server_add.go isOpenServerRefusal) and then writes a
// plain-HTTP entry; any change to the bytes breaks `shed server add` against
// an open server for all of them.
func TestBootstrapOpenServerRefusalOnTheWire(t *testing.T) {
	const wantStderr = "bootstrap requires auth.ssh.mode: enforce\n"

	signer := testSigner(t)
	listed := KeyAuth{
		Mode:  config.SSHAuthWarn,
		Users: userstest.Implicit(gossh.FingerprintSHA256(signer.PublicKey())),
	}

	for _, tc := range []struct {
		name string
		auth KeyAuth
	}{
		{"allowlist off", KeyAuth{}},
		{"allowlist warn with the key listed", listed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, tc.auth)
			if err != nil {
				t.Fatal(err)
			}
			// What serve wires for an open server: no CA.
			s.SetBootstrap(BootstrapInfo{TokenTTL: time.Hour})
			addr := serveForTest(t, s)

			// What released clients send: the CLI and the host-agent both put a
			// CSR on the line since v0.8.1; older ones sent none.
			for _, cmd := range []string{"control cli csr=AAAA", "credentials host-agent csr=AAAA", "control cli", ""} {
				stdout, stderr, status := runBootstrap(t, addr, signer, cmd)
				if stderr != wantStderr {
					t.Errorf("%q: stderr = %q, want %q", cmd, stderr, wantStderr)
				}
				if status != 1 {
					t.Errorf("%q: exit status = %d, want 1", cmd, status)
				}
				if stdout != "" {
					t.Errorf("%q: stdout = %q, want nothing", cmd, stdout)
				}
			}
		})
	}
}
