package sdk

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// startBootstrapSSHServer stands up a minimal in-process SSH server that accepts
// any client key and answers a single exec request by writing bundleJSON to
// stdout and exiting 0 — mimicking the server's _bootstrap handler.
func startBootstrapSSHServer(t *testing.T, hostKey ssh.Signer, bundleJSON string) net.Listener {
	t.Helper()
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

	go func() {
		for {
			nConn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go serveOneBootstrap(nConn, cfg, bundleJSON)
		}
	}()
	return ln
}

func serveOneBootstrap(nConn net.Conn, cfg *ssh.ServerConfig, bundleJSON string) {
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

func TestBootstrap(t *testing.T) {
	hostKey := newTestSigner(t)
	want := Bundle{
		HTTPPort: 8080, HTTPSPort: 8443,
		TLSCertFingerprint: "sha256:abc",
		Token:              "shed_control_xyz",
		Scope:              "control",
		TokenID:            "tok1",
	}
	bundleJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	ln := startBootstrapSSHServer(t, hostKey, string(bundleJSON))

	pin := ssh.FingerprintSHA256(hostKey.PublicKey())
	got, err := Bootstrap(context.Background(), ln.Addr().String(), newTestSigner(t), pin, "control", "cli")
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got.Token != want.Token || got.Scope != "control" || got.HTTPSPort != 8443 ||
		got.HTTPPort != 8080 || got.TLSCertFingerprint != "sha256:abc" || got.TokenID != "tok1" {
		t.Errorf("bundle = %+v, want %+v", got, want)
	}
}

func TestBootstrapHostKeyMismatchFailsClosed(t *testing.T) {
	hostKey := newTestSigner(t)
	ln := startBootstrapSSHServer(t, hostKey, `{"token":"x"}`)

	// Pin a DIFFERENT key's fingerprint — the handshake must fail closed with the
	// typed sentinel so callers can refuse any weaker-credential fallback.
	wrongPin := ssh.FingerprintSHA256(newTestSigner(t).PublicKey())
	_, err := Bootstrap(context.Background(), ln.Addr().String(), newTestSigner(t), wrongPin, "control", "cli")
	if err == nil {
		t.Fatal("expected a host key pin mismatch error")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Errorf("error %v is not ErrHostKeyMismatch", err)
	}
}

func TestBootstrapValidatesArgs(t *testing.T) {
	if _, err := Bootstrap(context.Background(), "127.0.0.1:1", newTestSigner(t), "", "control", "cli"); err == nil {
		t.Error("empty host key pin must error")
	}
	if _, err := Bootstrap(context.Background(), "127.0.0.1:1", nil, "SHA256:x", "control", "cli"); err == nil {
		t.Error("nil signer must error")
	}
	if _, err := Bootstrap(context.Background(), "127.0.0.1:1", newTestSigner(t), "SHA256:x", "", "cli"); err == nil {
		t.Error("empty scope must error (a bare client-kind would be parsed as the scope)")
	}
}

func TestBundleJSONTagsMatchServer(t *testing.T) {
	// Guards the cross-module contract: the wire tags the server emits must
	// decode here. (The server side lives in internal/sshd/bootstrap.go.)
	const serverJSON = `{"http_port":80,"https_port":443,"tls_cert_fingerprint":"sha256:f","token":"t","scope":"credentials","token_id":"id","expires_at":"2030-01-02T03:04:05Z"}`
	var b Bundle
	if err := json.Unmarshal([]byte(serverJSON), &b); err != nil {
		t.Fatal(err)
	}
	if b.HTTPPort != 80 || b.HTTPSPort != 443 || b.TLSCertFingerprint != "sha256:f" ||
		b.Token != "t" || b.Scope != "credentials" || b.TokenID != "id" || b.ExpiresAt.IsZero() {
		t.Errorf("decoded bundle mismatch: %+v", b)
	}
}

func TestBootstrapDialTimeoutBounded(t *testing.T) {
	// A caller with NO deadline must still be bounded by bootstrapTimeout on the
	// DIAL — otherwise a blackholed endpoint blocks on the (much longer) OS TCP
	// connect timeout while a caller holds e.g. the host-agent supervisor lock.
	// 192.0.2.1 is TEST-NET-1 (RFC 5737): reserved, typically unrouted → blackhole.
	orig := bootstrapTimeout
	bootstrapTimeout = 300 * time.Millisecond
	defer func() { bootstrapTimeout = orig }()

	signer := newTestSigner(t)
	done := make(chan error, 1)
	go func() {
		_, err := Bootstrap(context.Background(), "192.0.2.1:22", signer, "SHA256:x", "control", "cli")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error dialing a blackholed endpoint")
		}
	case <-time.After(5 * time.Second):
		// 5s >> the 300ms bound: reaching here means the dial ignored the timeout
		// (the bug) and is blocking on the OS connect timeout.
		t.Fatalf("Bootstrap did not honor the dial timeout (bootstrapTimeout=%v); a no-deadline caller blocked on the OS connect timeout", bootstrapTimeout)
	}
}
