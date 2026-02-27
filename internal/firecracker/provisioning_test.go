//go:build linux
// +build linux

package firecracker

import (
	"context"
	"testing"

	"github.com/charliek/shed/internal/provision"
)

func TestGetLastLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		n     int
		want  string
	}{
		{
			name:  "fewer lines than n",
			input: "line1\nline2",
			n:     5,
			want:  "line1\nline2",
		},
		{
			name:  "exact n lines",
			input: "line1\nline2\nline3",
			n:     3,
			want:  "line1\nline2\nline3",
		},
		{
			name:  "more lines than n",
			input: "line1\nline2\nline3\nline4\nline5",
			n:     2,
			want:  "line4\nline5",
		},
		{
			name:  "single line",
			input: "only",
			n:     3,
			want:  "only",
		},
		{
			name:  "empty string",
			input: "",
			n:     3,
			want:  "",
		},
		{
			name:  "trailing newline",
			input: "line1\nline2\nline3\n",
			n:     2,
			want:  "line3\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getLastLines(tt.input, tt.n)
			if got != tt.want {
				t.Errorf("getLastLines(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
			}
		})
	}
}

func TestEscapeUnescapeStateValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"simple string", "hello world"},
		{"with newline", "line1\nline2"},
		{"with backslash", `path\to\file`},
		{"with both", "line1\npath\\to\\file\nline3"},
		{"empty", ""},
		{"only newlines", "\n\n\n"},
		{"only backslashes", `\\\\`},
		{"backslash before n literal", `\n is not a newline here`},
		{"trailing backslash", `value\`},
		{"mixed escapes", "error: failed\npath: C:\\Users\\test\nstatus: ok"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			escaped := escapeStateValue(tt.input)
			unescaped := unescapeStateValue(escaped)

			if unescaped != tt.input {
				t.Errorf("round-trip failed:\n  input:     %q\n  escaped:   %q\n  unescaped: %q", tt.input, escaped, unescaped)
			}
		})
	}
}

func TestEscapeStateValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no escaping needed", "hello", "hello"},
		{"newline escaped", "a\nb", `a\nb`},
		{"backslash escaped", `a\b`, `a\\b`},
		{"backslash before newline", "a\\\nb", `a\\\nb`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapeStateValue(tt.input)
			if got != tt.want {
				t.Errorf("escapeStateValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestUnescapeStateValue(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no escapes", "hello", "hello"},
		{"escaped newline", `a\nb`, "a\nb"},
		{"escaped backslash", `a\\b`, `a\b`},
		{"unknown escape sequence", `a\tb`, `a\tb`},
		{"trailing backslash", `value\`, `value\`},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unescapeStateValue(tt.input)
			if got != tt.want {
				t.Errorf("unescapeStateValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNewProvisioner(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	p := NewProvisioner(vsock, "test-shed")

	if p.vsock != vsock {
		t.Error("vsock not set correctly")
	}
	if p.shedName != "test-shed" {
		t.Errorf("shedName = %q, want %q", p.shedName, "test-shed")
	}
	if p.output == nil {
		t.Error("output should not be nil")
	}
	if p.errOut == nil {
		t.Error("errOut should not be nil")
	}
}

func TestProvisionerBuildEnv(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	p := NewProvisioner(vsock, "my-shed")

	cfg := &provision.Config{
		Env: map[string]string{
			"FOO": "bar",
			"BAZ": "qux",
		},
	}

	env := p.buildEnv(cfg)

	// Should have 3 default vars + 2 user vars
	if len(env) != 5 {
		t.Fatalf("buildEnv() returned %d vars, want 5", len(env))
	}

	// Check defaults are present
	found := map[string]bool{}
	for _, e := range env {
		if e == "SHED_CONTAINER=true" {
			found["container"] = true
		}
		if e == "SHED_NAME=my-shed" {
			found["name"] = true
		}
		if e == "FOO=bar" {
			found["foo"] = true
		}
		if e == "BAZ=qux" {
			found["baz"] = true
		}
	}

	if !found["container"] {
		t.Error("missing SHED_CONTAINER=true")
	}
	if !found["name"] {
		t.Error("missing SHED_NAME=my-shed")
	}
	if !found["foo"] {
		t.Error("missing FOO=bar")
	}
	if !found["baz"] {
		t.Error("missing BAZ=qux")
	}
}

func TestProvisionerBuildEnvEmpty(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	p := NewProvisioner(vsock, "test")

	cfg := &provision.Config{
		Env: map[string]string{},
	}

	env := p.buildEnv(cfg)

	// Should have just the 3 default vars
	if len(env) != 3 {
		t.Fatalf("buildEnv() returned %d vars, want 3", len(env))
	}
}

func TestLogFileForHook(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	p := NewProvisioner(vsock, "test")

	tests := []struct {
		hookType provision.HookType
		want     string
	}{
		{provision.HookTypeInstall, provision.InstallLog},
		{provision.HookTypeStartup, provision.StartupLog},
		{provision.HookTypeShutdown, provision.ShutdownLog},
		{provision.HookType("custom"), "/var/log/shed/custom.log"},
	}

	for _, tt := range tests {
		t.Run(string(tt.hookType), func(t *testing.T) {
			got := p.logFileForHook(tt.hookType)
			if got != tt.want {
				t.Errorf("logFileForHook(%q) = %q, want %q", tt.hookType, got, tt.want)
			}
		})
	}
}

func TestRunProvisioningNilConfig(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	p := NewProvisioner(vsock, "test")

	// Nil config should be a no-op
	if err := p.RunProvisioning(context.Background(), nil, true); err != nil {
		t.Errorf("RunProvisioning(nil config) = %v, want nil", err)
	}
}

func TestRunProvisioningNoHooks(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	p := NewProvisioner(vsock, "test")

	cfg := &provision.Config{
		Env: map[string]string{},
	}

	// Config with no hooks should be a no-op
	if err := p.RunProvisioning(context.Background(), cfg, true); err != nil {
		t.Errorf("RunProvisioning(no hooks) = %v, want nil", err)
	}
}

func TestRunShutdownHookNilConfig(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	p := NewProvisioner(vsock, "test")

	// Nil config should be a no-op (no panic)
	p.RunShutdownHook(context.Background(), nil)
}

func TestRunShutdownHookNoHook(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	p := NewProvisioner(vsock, "test")

	cfg := &provision.Config{
		Env: map[string]string{},
	}

	// Config with no shutdown hook should be a no-op (no panic)
	p.RunShutdownHook(context.Background(), cfg)
}

func TestNewProvisionState(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	state := NewProvisionState(vsock)

	if state.vsock != vsock {
		t.Error("vsock not set correctly")
	}
}
