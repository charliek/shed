package sshagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/ext/protocol"
	"github.com/charliek/shed/internal/ext/testutil"
)

func TestList(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	listResp := protocol.SSHListResponse{
		Keys: []protocol.SSHKeyInfo{
			{
				Format:  sshPub.Type(),
				Blob:    base64.StdEncoding.EncodeToString(sshPub.Marshal()),
				Comment: "test@host",
			},
		},
	}

	srv := newMockPublishServer(t, func(payload json.RawMessage) json.RawMessage {
		var req protocol.SSHListRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if req.Operation != protocol.SSHOpList {
			t.Fatalf("expected list operation, got %q", req.Operation)
		}
		data, _ := json.Marshal(listResp)
		return data
	})
	defer srv.Close()

	a := New(WithPublishURL(srv.URL + "/v1/publish"))

	keys, err := a.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].Comment != "test@host" {
		t.Errorf("comment mismatch: got %q, want %q", keys[0].Comment, "test@host")
	}
	if keys[0].Format != sshPub.Type() {
		t.Errorf("format mismatch: got %q, want %q", keys[0].Format, sshPub.Type())
	}
}

func TestSign(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	challengeData := []byte("challenge data to sign")

	srv := newMockPublishServer(t, func(payload json.RawMessage) json.RawMessage {
		var req protocol.SSHSignRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if req.Operation != protocol.SSHOpSign {
			t.Fatalf("expected sign operation, got %q", req.Operation)
		}

		data, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			t.Fatalf("decode data: %v", err)
		}

		sig, err := signer.Sign(rand.Reader, data)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}

		resp := protocol.SSHSignResponse{
			Format: sig.Format,
			Blob:   base64.StdEncoding.EncodeToString(sig.Blob),
		}
		out, _ := json.Marshal(resp)
		return out
	})
	defer srv.Close()

	a := New(WithPublishURL(srv.URL + "/v1/publish"))

	sig, err := a.Sign(sshPub, challengeData)
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	if sig.Format != sshPub.Type() {
		t.Errorf("format mismatch: got %q, want %q", sig.Format, sshPub.Type())
	}
	if len(sig.Blob) == 0 {
		t.Error("empty signature blob")
	}

	// Verify the signature is valid
	if err := sshPub.Verify(challengeData, sig); err != nil {
		t.Errorf("signature verification failed: %v", err)
	}
}

func TestSignErrorResponse(t *testing.T) {
	srv := newMockPublishServer(t, func(_ json.RawMessage) json.RawMessage {
		resp := protocol.SSHErrorResponse{
			Error: "key not found",
			Code:  protocol.SSHCodeKeyNotFound,
		}
		out, _ := json.Marshal(resp)
		return out
	})
	defer srv.Close()

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)

	a := New(WithPublishURL(srv.URL + "/v1/publish"))

	_, err := a.Sign(sshPub, []byte("data"))
	if err == nil {
		t.Fatal("expected error for KEY_NOT_FOUND response")
	}
}

func TestListEmpty(t *testing.T) {
	srv := newMockPublishServer(t, func(_ json.RawMessage) json.RawMessage {
		resp := protocol.SSHListResponse{Keys: []protocol.SSHKeyInfo{}}
		out, _ := json.Marshal(resp)
		return out
	})
	defer srv.Close()

	a := New(WithPublishURL(srv.URL + "/v1/publish"))

	keys, err := a.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected 0 keys, got %d", len(keys))
	}
}

func newMockPublishServer(t *testing.T, handler func(json.RawMessage) json.RawMessage) *httptest.Server {
	return testutil.NewMockPublishServer(t, protocol.NamespaceSSHAgent, handler)
}
