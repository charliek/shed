package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

type recordingNFT struct {
	missing bool
	calls   []string
	stdin   []string
}

func (r *recordingNFT) Available() bool { return !r.missing }

func (r *recordingNFT) Run(_ context.Context, stdin string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	r.stdin = append(r.stdin, stdin)
	if r.missing {
		return nil, exec.ErrNotFound
	}
	return nil, nil
}

func TestNetworkTeardownDeletesOnlyItsTables(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "metadata.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("iso", `{"name":"iso","network_profile":"isolated"}`)
	write("open", `{"name":"open","network_profile":"open"}`)
	write("legacy", `{"name":"legacy"}`)
	write("broken", `{`)

	cfg := &config.ServerConfig{Name: "mini3-dev", Firecracker: &config.FirecrackerConfig{InstanceDir: dir}}
	r := &recordingNFT{}
	var out bytes.Buffer
	if err := networkTeardown(context.Background(), &out, cfg, r); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0] != "-f -" {
		t.Fatalf("calls = %v, want one nft -f -", r.calls)
	}
	for _, line := range strings.Split(strings.TrimSpace(r.stdin[0]), "\n") {
		if !strings.HasSuffix(line, " shed_mini3_dev") {
			t.Errorf("teardown line %q is not about this server's tables", line)
		}
	}
	if !strings.Contains(out.String(), "[iso]") {
		t.Errorf("output = %q, want the isolated shed listed", out.String())
	}

	// No nft: success, no calls.
	r = &recordingNFT{missing: true}
	if err := networkTeardown(context.Background(), &out, cfg, r); err != nil {
		t.Errorf("missing nft: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("missing nft made calls: %v", r.calls)
	}
}
