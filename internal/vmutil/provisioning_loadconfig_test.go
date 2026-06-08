package vmutil

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/config"
)

// TestLoadConfigRetriesUnreadyMount verifies that a not-yet-serving project
// mount (probe exit 75) is retried rather than silently skipping provisioning.
func TestLoadConfigRetriesUnreadyMount(t *testing.T) {
	orig := provisionReadBackoffs
	provisionReadBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { provisionReadBackoffs = orig }()

	const yaml = "hooks:\n  install: .shed/scripts/install.sh\n"
	var calls int32
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			if atomic.AddInt32(&calls, 1) < 3 {
				agentproto.WriteExitCode(conn, 75) // mount not serving yet
				return
			}
			agentproto.WriteData(conn, []byte(yaml))
			agentproto.WriteExitCode(conn, 0)
		},
	}

	p := NewProvisioner(NewAgentClient(dialer, 1024, 1026), "test")
	p.SetWorkDir("/home/shed/proj")

	cfg, err := p.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg == nil || !cfg.HasInstallHook() {
		t.Fatalf("LoadConfig() did not parse hooks after retry: %+v", cfg)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("read attempts = %d, want 3 (2 retries then success)", got)
	}
}

// TestLoadConfigBareShedMissingConfigNoRetry verifies that a bare shed (landing
// dir == HomePath) reporting "no config" (probe exit 66) is terminal — the
// common no-provisioning case must not burn the backoff schedule.
func TestLoadConfigBareShedMissingConfigNoRetry(t *testing.T) {
	orig := provisionReadBackoffs
	// Long backoffs: if the bare-shed path retried, the test would hang.
	provisionReadBackoffs = []time.Duration{time.Hour, time.Hour}
	defer func() { provisionReadBackoffs = orig }()

	var calls int32
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			atomic.AddInt32(&calls, 1)
			agentproto.WriteExitCode(conn, 66) // dir serving; no config file
		},
	}

	p := NewProvisioner(NewAgentClient(dialer, 1024, 1026), "test")
	p.SetWorkDir(config.HomePath) // bare shed

	cfg, err := p.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg == nil || cfg.HasAnyHooks() {
		t.Fatalf("expected empty config for missing file, got %+v", cfg)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("read attempts = %d, want 1 (bare-shed missing config is terminal)", got)
	}
}

// TestLoadConfigProjectDirMissingConfigRetriesThenEmpty verifies that a project
// landing dir reporting "no config" (probe exit 66, which on a --local-dir mount
// can be a not-yet-coherent share) is retried, and once the retries drain we
// conclude there is genuinely no config — without surfacing an error.
func TestLoadConfigProjectDirMissingConfigRetriesThenEmpty(t *testing.T) {
	orig := provisionReadBackoffs
	provisionReadBackoffs = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { provisionReadBackoffs = orig }()

	var calls int32
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			atomic.AddInt32(&calls, 1)
			agentproto.WriteExitCode(conn, 66)
		},
	}

	p := NewProvisioner(NewAgentClient(dialer, 1024, 1026), "test")
	p.SetWorkDir("/home/shed/proj") // project landing dir

	cfg, err := p.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg == nil || cfg.HasAnyHooks() {
		t.Fatalf("expected empty config after draining retries, got %+v", cfg)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("read attempts = %d, want 3 (1 + 2 backoffs)", got)
	}
}

// TestLoadConfigProjectDirEmptyReadRetries verifies that a successful-but-empty
// read on a project mount (the file's dentry appeared before its data) is
// retried, and once real content arrives it is parsed.
func TestLoadConfigProjectDirEmptyReadRetries(t *testing.T) {
	orig := provisionReadBackoffs
	provisionReadBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { provisionReadBackoffs = orig }()

	const yaml = "hooks:\n  install: .shed/scripts/install.sh\n"
	var calls int32
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			if atomic.AddInt32(&calls, 1) < 3 {
				agentproto.WriteExitCode(conn, 0) // success, but empty content
				return
			}
			agentproto.WriteData(conn, []byte(yaml))
			agentproto.WriteExitCode(conn, 0)
		},
	}

	p := NewProvisioner(NewAgentClient(dialer, 1024, 1026), "test")
	p.SetWorkDir("/home/shed/proj") // project landing dir

	cfg, err := p.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg == nil || !cfg.HasInstallHook() {
		t.Fatalf("LoadConfig() did not parse hooks after empty-read retries: %+v", cfg)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("read attempts = %d, want 3 (2 empty reads then content)", got)
	}
}
