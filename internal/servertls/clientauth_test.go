package servertls

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func leafWithCN(cn string) *x509.Certificate {
	return &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
}

// connState builds the tls.ConnectionState the verifier sees. Only
// PeerCertificates matters: the callback is installed as VerifyConnection under
// RequireAndVerifyClientCert, so chain verification has already happened and
// the leaf is PeerCertificates[0] — on a resumed session too, which is the
// whole reason this is a VerifyConnection callback.
func connState(certs ...*x509.Certificate) tls.ConnectionState {
	return tls.ConnectionState{HandshakeComplete: true, PeerCertificates: certs}
}

func TestAllowlistConnectionVerifier(t *testing.T) {
	const allowed = "SHA256:aaaa"

	tests := []struct {
		name       string
		authorized func(string) bool
		state      tls.ConnectionState
		wantErr    error
	}{
		{
			name:       "allowlisted CN passes",
			authorized: func(fp string) bool { return fp == allowed },
			state:      connState(leafWithCN(allowed)),
		},
		{
			name:       "unlisted CN is refused",
			authorized: func(fp string) bool { return fp == allowed },
			state:      connState(leafWithCN("SHA256:bbbb")),
			wantErr:    ErrClientCertNotAuthorized,
		},
		{
			// The CA never issues an empty CN, but an empty allowlist entry must
			// not become a wildcard if one ever appeared.
			name:       "empty CN is refused",
			authorized: func(fp string) bool { return fp == allowed },
			state:      connState(leafWithCN("")),
			wantErr:    ErrClientCertNotAuthorized,
		},
		{
			// PeerCertificates[0] is the identity — an intermediate or root CN
			// further along the chain must never be what gets checked.
			name:       "leaf is checked, not the issuer",
			authorized: func(fp string) bool { return fp == allowed },
			state:      connState(leafWithCN("SHA256:bbbb"), leafWithCN(allowed)),
			wantErr:    ErrClientCertNotAuthorized,
		},
		{
			name:       "nil authorizer authorizes nothing",
			authorized: nil,
			state:      connState(leafWithCN(allowed)),
			wantErr:    ErrClientCertNotAuthorized,
		},
		{
			name:       "no peer certificate",
			authorized: func(string) bool { return true },
			state:      connState(),
			wantErr:    ErrClientCertMissing,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := AllowlistConnectionVerifier(tt.authorized)(tt.state)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestAllowlistConnectionVerifierReadsLiveState: the predicate is consulted per
// handshake, never memoized, so a key leaving the allowlist stops new (and
// resumed) connections at once.
func TestAllowlistConnectionVerifierReadsLiveState(t *testing.T) {
	const cn = "SHA256:aaaa"
	live := true
	verify := AllowlistConnectionVerifier(func(string) bool { return live })
	state := connState(leafWithCN(cn))

	if err := verify(state); err != nil {
		t.Fatalf("first handshake: %v", err)
	}
	live = false
	if err := verify(state); !errors.Is(err, ErrClientCertNotAuthorized) {
		t.Fatalf("after de-authorization: got %v, want ErrClientCertNotAuthorized", err)
	}
}

// TestAllowlistConnectionVerifierRunsOnResumedHandshake is the reason this is a
// VerifyConnection callback rather than a VerifyPeerCertificate one: crypto/tls
// does not call VerifyPeerCertificate when a session is RESUMED, so an
// allowlist check installed there would silently not run for exactly the
// long-lived, reconnecting clients it is meant to catch. This drives a real
// handshake, then a real resumed handshake through a shared
// ClientSessionCache, and asserts the callback fired on BOTH — and that the
// resumed one still saw the peer certificate.
func TestAllowlistConnectionVerifierRunsOnResumedHandshake(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrGenerateCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	serverCert, serverDER, err := LoadOrGenerate(filepath.Join(dir, "tls.pem"), filepath.Join(dir, "tls.key"), nil)
	if err != nil {
		t.Fatalf("generate server cert: %v", err)
	}
	serverLeaf, err := x509.ParseCertificate(serverDER)
	if err != nil {
		t.Fatalf("parse server cert: %v", err)
	}

	csrDER, clientKey := p256CSR(t)
	clientDER, err := ca.SignClientCSR(csrDER, testFingerprint, "shed", "cli", time.Hour)
	if err != nil {
		t.Fatalf("sign client CSR: %v", err)
	}
	clientCert := tls.Certificate{Certificate: [][]byte{clientDER}, PrivateKey: clientKey}

	// calls counts verifier invocations; sawPeerCert records whether each one
	// had a leaf to inspect (the resumed handshake must, or the check is inert).
	var (
		mu          sync.Mutex
		calls       int
		sawPeerCert []bool
	)
	verify := AllowlistConnectionVerifier(func(string) bool { return true })
	serverCfg := &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
		ClientCAs:    ca.Pool(),
		ClientAuth:   tls.RequireAndVerifyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			mu.Lock()
			calls++
			sawPeerCert = append(sawPeerCert, len(cs.PeerCertificates) > 0)
			mu.Unlock()
			return verify(cs)
		},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc := tls.Server(raw, serverCfg)
				if err := sc.Handshake(); err != nil {
					_ = sc.Close()
					return
				}
				// One byte of app data: reading it client-side guarantees the
				// client has processed the post-handshake session ticket.
				_, _ = sc.Write([]byte("x"))
				time.Sleep(50 * time.Millisecond)
				_ = sc.Close()
			}()
		}
	}()

	roots := x509.NewCertPool()
	roots.AddCert(serverLeaf)
	clientCfg := &tls.Config{
		RootCAs:            roots,
		ServerName:         "localhost",
		MinVersion:         tls.VersionTLS12,
		Certificates:       []tls.Certificate{clientCert},
		ClientSessionCache: tls.NewLRUClientSessionCache(4),
	}

	dial := func(t *testing.T) (resumed bool) {
		t.Helper()
		c, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 1)
		if _, err := c.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		return c.ConnectionState().DidResume
	}

	if dial(t) {
		t.Fatal("first handshake reported resumption")
	}
	mu.Lock()
	first := calls
	mu.Unlock()
	if first != 1 {
		t.Fatalf("after first handshake: verifier ran %d times, want 1", first)
	}

	resumed := dial(t)
	mu.Lock()
	total, peer := calls, append([]bool(nil), sawPeerCert...)
	mu.Unlock()
	if !resumed {
		t.Skip("second handshake did not resume — cannot exercise the resumption path on this Go version")
	}
	if total != 2 {
		t.Fatalf("resumed handshake: verifier ran %d times total, want 2 — "+
			"the allowlist check is being skipped on resumed sessions", total)
	}
	if !peer[1] {
		t.Error("resumed handshake had no PeerCertificates — the allowlist check would fail closed on every resumption")
	}
}
