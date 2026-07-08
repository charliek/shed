package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/ext/protocol"
	"github.com/charliek/shed/internal/ext/testutil"
)

// TestRunGetNotFoundSpeaksDockerProtocol is the regression guard for the
// public-pull bug: when the host has no credential for an allowed registry, the
// helper must emit Docker's exact not-found message on stdout (with a non-zero
// exit) so Docker falls back to an anonymous pull instead of aborting. The
// string must match docker-credential-helpers' errCredentialsNotFoundMessage
// byte-for-byte.
func TestRunGetNotFoundSpeaksDockerProtocol(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(_ json.RawMessage) json.RawMessage {
		data, _ := json.Marshal(protocol.DockerErrorResponse{Error: "nope", Code: protocol.DockerCodeNotFound})
		return data
	})
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := runGet(srv.URL+"/v1/publish", strings.NewReader("https://index.docker.io/v1/\n"), &stdout, &stderr)

	if rc != 1 {
		t.Errorf("exit code = %d, want 1", rc)
	}
	if got := strings.TrimSpace(stdout.String()); got != credentialsNotFoundMessage {
		t.Errorf("stdout = %q, want %q (Docker's anonymous-fallback signal)", got, credentialsNotFoundMessage)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty (must not leak host error to Docker)", stderr.String())
	}
}

// TestRunGetRegistryNotAllowedIsHardWall guards the security boundary: a
// registry rejected by the allowlist must abort the pull, NOT fall back to an
// anonymous pull. The helper must therefore exit non-zero WITHOUT emitting the
// not-found signal on stdout (Docker would otherwise pull the image
// anonymously, defeating the allowlist).
func TestRunGetRegistryNotAllowedIsHardWall(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(_ json.RawMessage) json.RawMessage {
		data, _ := json.Marshal(protocol.DockerErrorResponse{Error: "registry not in allowlist", Code: protocol.DockerCodeNotAllowed})
		return data
	})
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := runGet(srv.URL+"/v1/publish", strings.NewReader("https://index.docker.io/v1/\n"), &stdout, &stderr)

	if rc != 1 {
		t.Errorf("exit code = %d, want 1", rc)
	}
	if strings.TrimSpace(stdout.String()) == credentialsNotFoundMessage {
		t.Error("REGISTRY_NOT_ALLOWED must not emit the not-found signal on stdout (that would let Docker pull anonymously)")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty for a hard wall", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Error("REGISTRY_NOT_ALLOWED should report the error on stderr")
	}
}

func TestRunGetSuccess(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(_ json.RawMessage) json.RawMessage {
		data, _ := json.Marshal(protocol.DockerGetResponse{
			ServerURL: "us-docker.pkg.dev",
			Username:  "_json_key",
			Secret:    "tok",
		})
		return data
	})
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := runGet(srv.URL+"/v1/publish", strings.NewReader("us-docker.pkg.dev"), &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", rc, stderr.String())
	}

	var out struct {
		ServerURL, Username, Secret string
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not valid credential JSON: %v (%q)", err, stdout.String())
	}
	if out.Username != "_json_key" || out.Secret != "tok" {
		t.Errorf("credential = %+v, want _json_key/tok", out)
	}
}

func TestRunGetHelperFailedIsHardError(t *testing.T) {
	srv := testutil.NewMockPublishServer(t, protocol.NamespaceDockerCredentials, func(_ json.RawMessage) json.RawMessage {
		data, _ := json.Marshal(protocol.DockerErrorResponse{Error: "boom", Code: protocol.DockerCodeHelperFailed})
		return data
	})
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := runGet(srv.URL+"/v1/publish", strings.NewReader("us-docker.pkg.dev"), &stdout, &stderr)
	if rc != 1 {
		t.Errorf("exit code = %d, want 1", rc)
	}
	// A genuine fault must NOT be disguised as the anonymous-fallback signal.
	if strings.TrimSpace(stdout.String()) == credentialsNotFoundMessage {
		t.Error("HELPER_FAILED must not emit the not-found signal on stdout")
	}
	if stderr.Len() == 0 {
		t.Error("HELPER_FAILED should report the error on stderr")
	}
}

func TestRunGetEmptyServerURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runGet("http://127.0.0.1:0/v1/publish", strings.NewReader("   \n"), &stdout, &stderr)
	if rc != 1 {
		t.Errorf("exit code = %d, want 1", rc)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}
