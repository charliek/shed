package dockercred

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/charliek/shed/internal/ext/protocol"
	"github.com/charliek/shed/internal/ext/testutil"
)

func TestGet(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(payload json.RawMessage) json.RawMessage {
		var req protocol.DockerGetRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if req.Operation != protocol.DockerOpGet {
			t.Fatalf("expected get, got %q", req.Operation)
		}
		if req.ServerURL != "us-docker.pkg.dev" {
			t.Fatalf("expected us-docker.pkg.dev, got %q", req.ServerURL)
		}

		resp := protocol.DockerGetResponse{
			ServerURL: "us-docker.pkg.dev",
			Username:  "_json_key",
			Secret:    "gcloud-token-123",
		}
		data, _ := json.Marshal(resp)
		return data
	})
	defer srv.Close()

	h := New(WithPublishURL(srv.URL + "/v1/publish"))
	resp, err := h.Get(context.Background(), "us-docker.pkg.dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ServerURL != "us-docker.pkg.dev" {
		t.Errorf("ServerURL = %q, want %q", resp.ServerURL, "us-docker.pkg.dev")
	}
	if resp.Username != "_json_key" {
		t.Errorf("Username = %q, want %q", resp.Username, "_json_key")
	}
	if resp.Secret != "gcloud-token-123" {
		t.Errorf("Secret = %q, want %q", resp.Secret, "gcloud-token-123")
	}
}

func TestGetError(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(_ json.RawMessage) json.RawMessage {
		resp := protocol.DockerErrorResponse{
			Error: "registry not allowed",
			Code:  protocol.DockerCodeNotAllowed,
		}
		data, _ := json.Marshal(resp)
		return data
	})
	defer srv.Close()

	h := New(WithPublishURL(srv.URL + "/v1/publish"))
	_, err := h.Get(context.Background(), "blocked.io")
	if err == nil {
		t.Fatal("expected error")
	}
	// A registry rejected by the allowlist must hit a hard wall — it is NOT
	// wrapped as ErrCredentialsNotFound, so the guest binary fails the pull
	// instead of letting Docker fall back to an anonymous pull.
	if errors.Is(err, ErrCredentialsNotFound) {
		t.Errorf("%s must not map to ErrCredentialsNotFound, got %v", protocol.DockerCodeNotAllowed, err)
	}
}

func TestGetNotFound(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(_ json.RawMessage) json.RawMessage {
		resp := protocol.DockerErrorResponse{
			Error: "no credentials found",
			Code:  protocol.DockerCodeNotFound,
		}
		data, _ := json.Marshal(resp)
		return data
	})
	defer srv.Close()

	h := New(WithPublishURL(srv.URL + "/v1/publish"))
	_, err := h.Get(context.Background(), "unknown.io")
	if err == nil {
		t.Fatal("expected error for unknown registry")
	}
	if !errors.Is(err, ErrCredentialsNotFound) {
		t.Errorf("expected ErrCredentialsNotFound for %s, got %v", protocol.DockerCodeNotFound, err)
	}
}

func TestGetHelperFailedIsHardError(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(_ json.RawMessage) json.RawMessage {
		resp := protocol.DockerErrorResponse{
			Error: "docker-credential-gcloud failed",
			Code:  protocol.DockerCodeHelperFailed,
		}
		data, _ := json.Marshal(resp)
		return data
	})
	defer srv.Close()

	h := New(WithPublishURL(srv.URL + "/v1/publish"))
	_, err := h.Get(context.Background(), "us-docker.pkg.dev")
	if err == nil {
		t.Fatal("expected error")
	}
	// Genuine helper faults must stay hard errors, not be masked as not-found.
	if errors.Is(err, ErrCredentialsNotFound) {
		t.Errorf("HELPER_FAILED must not map to ErrCredentialsNotFound, got %v", err)
	}
}

func TestList(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(payload json.RawMessage) json.RawMessage {
		var req protocol.DockerListRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if req.Operation != protocol.DockerOpList {
			t.Fatalf("expected list, got %q", req.Operation)
		}

		resp := protocol.DockerListResponse{
			Registries: map[string]string{
				"gcr.io":  "user1",
				"ghcr.io": "user2",
			},
		}
		data, _ := json.Marshal(resp)
		return data
	})
	defer srv.Close()

	h := New(WithPublishURL(srv.URL + "/v1/publish"))
	registries, err := h.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(registries) != 2 {
		t.Errorf("count = %d, want 2", len(registries))
	}
	if registries["gcr.io"] != "user1" {
		t.Errorf("gcr.io = %q, want %q", registries["gcr.io"], "user1")
	}
}

func TestListEmpty(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(_ json.RawMessage) json.RawMessage {
		resp := protocol.DockerListResponse{
			Registries: map[string]string{},
		}
		data, _ := json.Marshal(resp)
		return data
	})
	defer srv.Close()

	h := New(WithPublishURL(srv.URL + "/v1/publish"))
	registries, err := h.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(registries) != 0 {
		t.Errorf("count = %d, want 0", len(registries))
	}
}
