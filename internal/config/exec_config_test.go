package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecConfigDefaults(t *testing.T) {
	for _, cfg := range []*ServerConfig{{}, {Exec: &ExecConfig{}}} {
		if got := cfg.ExecMaxStdinBytes(); got != 1<<20 {
			t.Errorf("max_stdin_bytes default %d", got)
		}
		if got := cfg.ExecMaxTimeout(); got != time.Hour {
			t.Errorf("max_timeout default %s", got)
		}
		if got := cfg.ExecMaxConcurrentPerShed(); got != 8 {
			t.Errorf("max_concurrent_per_shed default %d", got)
		}
		if err := cfg.validateExec(); err != nil {
			t.Errorf("defaults refused: %v", err)
		}
	}
	cfg := &ServerConfig{Exec: &ExecConfig{MaxStdinBytes: 10, MaxTimeout: Duration(2 * time.Minute), MaxConcurrentPerShed: 3}}
	if cfg.ExecMaxStdinBytes() != 10 || cfg.ExecMaxTimeout() != 2*time.Minute || cfg.ExecMaxConcurrentPerShed() != 3 {
		t.Errorf("set values not used: %d %s %d", cfg.ExecMaxStdinBytes(), cfg.ExecMaxTimeout(), cfg.ExecMaxConcurrentPerShed())
	}
}

func TestValidateExec(t *testing.T) {
	tests := []struct {
		name    string
		exec    ExecConfig
		wantErr string
	}{
		{"all set", ExecConfig{MaxStdinBytes: 1, MaxTimeout: Duration(time.Second), MaxConcurrentPerShed: 1}, ""},
		{"negative stdin", ExecConfig{MaxStdinBytes: -1}, "exec.max_stdin_bytes"},
		{"negative concurrency", ExecConfig{MaxConcurrentPerShed: -1}, "exec.max_concurrent_per_shed"},
		{"timeout under 1s", ExecConfig{MaxTimeout: Duration(500 * time.Millisecond)}, "exec.max_timeout"},
		{"negative timeout", ExecConfig{MaxTimeout: Duration(-time.Second)}, "exec.max_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ServerConfig{Exec: &tt.exec}
			err := cfg.validateExec()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %v, want one naming %s", err, tt.wantErr)
			}
		})
	}
}

// TestValidateExecUpperBounds: each exec setting is accepted at its upper
// bound and refused one step above it, with an error naming the key, the
// value and the bound.
func TestValidateExecUpperBounds(t *testing.T) {
	if MaxExecMaxStdinBytes != 1<<30 || MaxExecMaxTimeout != 7*24*time.Hour || MaxExecMaxConcurrentPerShed != 4096 {
		t.Fatalf("bounds %d %s %d, want 1 GiB, 7 days and 4096 (as documented)",
			MaxExecMaxStdinBytes, MaxExecMaxTimeout, MaxExecMaxConcurrentPerShed)
	}
	atBounds := ExecConfig{
		MaxStdinBytes:        MaxExecMaxStdinBytes,
		MaxTimeout:           Duration(MaxExecMaxTimeout),
		MaxConcurrentPerShed: MaxExecMaxConcurrentPerShed,
	}
	if err := (&ServerConfig{Exec: &atBounds}).validateExec(); err != nil {
		t.Fatalf("every setting at its bound refused: %v", err)
	}

	tests := []struct {
		name string
		exec ExecConfig
		want []string // all in the error: the key, the value, the bound
	}{
		{"stdin one byte over", ExecConfig{MaxStdinBytes: MaxExecMaxStdinBytes + 1},
			[]string{"exec.max_stdin_bytes", "1073741825", "at most 1073741824"}},
		{"stdin near int64's end", ExecConfig{MaxStdinBytes: 1 << 61},
			[]string{"exec.max_stdin_bytes", "2305843009213693952", "at most 1073741824"}},
		{"concurrency one over", ExecConfig{MaxConcurrentPerShed: MaxExecMaxConcurrentPerShed + 1},
			[]string{"exec.max_concurrent_per_shed", "4097", "at most 4096"}},
		{"timeout one second over", ExecConfig{MaxTimeout: Duration(MaxExecMaxTimeout + time.Second)},
			[]string{"exec.max_timeout", "168h0m1s", "at most 168h0m0s"}},
		{"timeout one nanosecond over", ExecConfig{MaxTimeout: Duration(MaxExecMaxTimeout + 1)},
			[]string{"exec.max_timeout", "168h0m0.000000001s", "at most 168h0m0s"}},
		{"timeout the largest duration", ExecConfig{MaxTimeout: Duration(math.MaxInt64)},
			[]string{"exec.max_timeout", "2562047h47m16.854775807s", "at most 168h0m0s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&ServerConfig{Exec: &tt.exec}).validateExec()
			if err == nil {
				t.Fatalf("accepted %+v", tt.exec)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

// TestExecConfigBoundsFromYAML: a server.yaml at every bound loads; one
// above a bound fails to load, naming the key.
func TestExecConfigBoundsFromYAML(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "server.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "name: t\ndefault_backend: firecracker\nfirecracker: {}\n"
	cfg, err := LoadServerConfigForCLI(write(base + "exec:\n  max_stdin_bytes: 1073741824\n  max_timeout: 168h\n  max_concurrent_per_shed: 4096\n"))
	if err != nil {
		t.Fatalf("a config at every bound refused: %v", err)
	}
	if cfg.ExecMaxStdinBytes() != 1<<30 || cfg.ExecMaxTimeout() != 168*time.Hour || cfg.ExecMaxConcurrentPerShed() != 4096 {
		t.Errorf("loaded %+v", cfg.Exec)
	}
	for _, tc := range []struct{ body, key string }{
		{"exec:\n  max_stdin_bytes: 1073741825\n", "exec.max_stdin_bytes"},
		{"exec:\n  max_timeout: 169h\n", "exec.max_timeout"},
		{"exec:\n  max_timeout: 2562047h\n", "exec.max_timeout"},
		{"exec:\n  max_concurrent_per_shed: 4097\n", "exec.max_concurrent_per_shed"},
	} {
		if _, err := LoadServerConfigForCLI(write(base + tc.body)); err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%q loaded (err %v), want a refusal naming %s", tc.body, err, tc.key)
		}
	}
}

// TestExecConfigFromYAML: the block loads through the CLI loader and a bad
// value is refused by ValidateNoHostCoupling.
func TestExecConfigFromYAML(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "server.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "name: t\ndefault_backend: firecracker\nfirecracker: {}\n"
	cfg, err := LoadServerConfigForCLI(write(base + "exec:\n  max_stdin_bytes: 2048\n  max_timeout: 10m\n  max_concurrent_per_shed: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecMaxStdinBytes() != 2048 || cfg.ExecMaxTimeout() != 10*time.Minute || cfg.ExecMaxConcurrentPerShed() != 2 {
		t.Errorf("loaded %+v", cfg.Exec)
	}
	if _, err := LoadServerConfigForCLI(write(base + "exec:\n  max_timeout: 100ms\n")); err == nil || !strings.Contains(err.Error(), "exec.max_timeout") {
		t.Errorf("a sub-second max_timeout was accepted: %v", err)
	}
}
