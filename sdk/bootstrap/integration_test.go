package bootstrap_test

// Hermetic end-to-end tests that exercise the REAL system ssh client against an
// in-process SSH server. They validate the security contract that unit tests of
// classify() cannot: that a wrong host-key pin actually makes ssh emit the
// CHANGED banner and that Run maps it to the terminal bootstrap.ErrHostKeyMismatch.
//
// They skip when no ssh binary is present so a minimal CI image still passes.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/sdk"
	bootstrap "github.com/charliek/shed/sdk/bootstrap"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func requireSSH(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh binary not available")
	}
}

func newSigner(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, priv
}

// startServer stands up a minimal _bootstrap SSH server that accepts any client
// key and answers a single exec by writing bundleJSON then exiting 0. Returns the
// listen port and the host key it presents.
func startServer(t *testing.T, bundleJSON string) (port int, hostKey ssh.Signer) {
	port, hostKey, _ = startRecordingServer(t, bundleJSON)
	return port, hostKey
}

// startRecordingServer is startServer plus the exec request line the client
// sent, which is the only place the CSR argument is observable from outside the
// package. Sends on a buffered channel so a test reads exactly the one exchange
// it triggered.
func startRecordingServer(t *testing.T, bundleJSON string) (port int, hostKey ssh.Signer, commands <-chan string) {
	t.Helper()
	hostKey, _ = newSigner(t)
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	seen := make(chan string, 8)
	go func() {
		for {
			nConn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveOne(nConn, cfg, bundleJSON, seen)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, hostKey, seen
}

func serveOne(nConn net.Conn, cfg *ssh.ServerConfig, bundleJSON string, seen chan<- string) {
	conn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		_ = nConn.Close()
		return
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			return
		}
		go func() {
			defer ch.Close()
			for req := range chReqs {
				if req.Type == "exec" {
					if req.WantReply {
						_ = req.Reply(true, nil)
					}
					// The exec payload is a single string: the remote command line,
					// i.e. everything sshArgs appended after the host.
					var payload struct{ Command string }
					if err := ssh.Unmarshal(req.Payload, &payload); err == nil && seen != nil {
						select {
						case seen <- payload.Command:
						default:
						}
					}
					_, _ = ch.Write([]byte(bundleJSON))
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
					return
				}
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}()
	}
}

func writeKnownHosts(t *testing.T, dir string, port int, key ssh.PublicKey) string {
	t.Helper()
	addr := knownhosts.Normalize(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	line := knownhosts.Line([]string{addr}, key)
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunHostKeyMismatchIsTerminal is the core security test: ssh is pinned to a
// DIFFERENT host key than the server presents, so it must abort with the CHANGED
// banner and Run must return the terminal bootstrap.ErrHostKeyMismatch.
func TestRunHostKeyMismatchIsTerminal(t *testing.T) {
	requireSSH(t)
	port, _ := startServer(t, `{"token":"x"}`)

	// Pin a wrong key for [127.0.0.1]:port → the server's real key won't match.
	wrong, _ := newSigner(t)
	kh := writeKnownHosts(t, t.TempDir(), port, wrong.PublicKey())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := bootstrap.Run(ctx, bootstrap.Params{Host: "127.0.0.1", Port: port, KnownHostsPath: kh, Scope: "control", ClientKind: "test"})
	if err == nil {
		t.Fatal("expected a host key mismatch error")
	}
	if !errors.Is(err, bootstrap.ErrHostKeyMismatch) {
		t.Errorf("error %v is not terminal bootstrap.ErrHostKeyMismatch", err)
	}
}

// TestRunSuccess exercises the happy path end to end: correct pin + a client
// identity the (accept-all) server admits → a decoded bundle.
func TestRunSuccess(t *testing.T) {
	requireSSH(t)
	want := sdk.Bundle{HTTPSPort: 8443, TLSCertFingerprint: "sha256:abc", Token: "shed_control_xyz", Scope: "control", TokenID: "t1"}
	bundleJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	port, hostKey := startServer(t, string(bundleJSON))

	home := t.TempDir()
	kh := writeKnownHosts(t, home, port, hostKey.PublicKey())
	writeClientIdentity(t, home, port, kh)

	// ssh reads ~/.ssh/config from HOME for the IdentityFile; disable the agent so
	// only our generated key is offered.
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, err := bootstrap.Run(ctx, bootstrap.Params{Host: "127.0.0.1", Port: port, KnownHostsPath: kh, Scope: "control", ClientKind: "test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Token != want.Token || got.Scope != "control" || got.HTTPSPort != 8443 {
		t.Errorf("bundle = %+v, want %+v", got, want)
	}
}

// TestRunNeverSendsACSRButRunCredentialDoes pins the split between the two
// entry points, asserted from the SERVER's side: Run is token-only and must
// stay that way (the host-agent depends on it), while RunCredential submits a
// CSR to every server so a mode flip needs no client change.
//
// It drives the real Run/RunCredential over the real ssh client rather than
// inspecting the argv a hand-cleared Params would produce. That distinction is
// the whole point of the test: the property under protection is that RUN
// clears CSRBase64, and any assertion that clears the field itself would still
// pass after Run stopped doing so.
func TestRunNeverSendsACSRButRunCredentialDoes(t *testing.T) {
	requireSSH(t)

	// A token bundle: what a token-mode (or pre-mtls) server answers with. Both
	// entry points accept it, so one server serves both halves of the test.
	bundleJSON, err := json.Marshal(sdk.Bundle{
		HTTPSPort: 8443, TLSCertFingerprint: "sha256:abc", Token: "shed_control_xyz", Scope: "control",
	})
	if err != nil {
		t.Fatal(err)
	}
	port, hostKey, commands := startRecordingServer(t, string(bundleJSON))

	home := t.TempDir()
	kh := writeKnownHosts(t, home, port, hostKey.PublicKey())
	writeClientIdentity(t, home, port, kh)
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	p := bootstrap.Params{
		Host: "127.0.0.1", Port: port, KnownHostsPath: kh, Scope: "control", ClientKind: "test",
		// Set deliberately. Run must DISCARD it — a caller passing one (or a
		// struct copied from a RunCredential call site) must not turn Run into an
		// enrolling client behind the host-agent's back.
		CSRBase64: "QUJD",
	}

	nextCommand := func(t *testing.T) string {
		t.Helper()
		select {
		case cmd := <-commands:
			return cmd
		case <-time.After(10 * time.Second):
			t.Fatal("server never received an exec request")
			return ""
		}
	}

	t.Run("Run discards the CSR", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := bootstrap.Run(ctx, p); err != nil {
			t.Fatalf("Run: %v", err)
		}
		cmd := nextCommand(t)
		if strings.Contains(cmd, "csr=") {
			t.Errorf("Run sent a CSR: request line = %q", cmd)
		}
		if !strings.Contains(cmd, "control") {
			t.Errorf("request line = %q, want it to carry the scope", cmd)
		}
	})

	t.Run("RunCredential sends one", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cred, err := bootstrap.RunCredential(ctx, p)
		if err != nil {
			t.Fatalf("RunCredential: %v", err)
		}
		if cred.Mode() != sdk.AuthModeToken {
			t.Errorf("Mode() = %q, want token (the server answered with a token bundle)", cred.Mode())
		}
		cmd := nextCommand(t)
		if !strings.Contains(cmd, "csr=") {
			t.Fatalf("RunCredential sent no CSR: request line = %q — "+
				"the Run assertion above would pass vacuously if this path were also CSR-free", cmd)
		}
		// And it is a freshly generated one, not the placeholder Run was handed.
		if strings.Contains(cmd, "csr=QUJD ") || strings.HasSuffix(cmd, "csr=QUJD") {
			t.Errorf("RunCredential relayed the caller's CSRBase64 instead of generating one: %q", cmd)
		}
	})
}

// writeClientIdentity generates a client key and a ~/.ssh/config that offers it
// (IdentitiesOnly) for the test host, in the temp HOME dir.
func writeClientIdentity(t *testing.T, home string, port int, knownHosts string) {
	t.Helper()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, priv := newSigner(t)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(sshDir, "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := "Host 127.0.0.1\n    IdentityFile " + keyPath + "\n    IdentitiesOnly yes\n    UserKnownHostsFile " + knownHosts + "\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}
