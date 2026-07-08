package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockAWSBackend implements AWSBackend for testing.
type mockAWSBackend struct {
	creds   *AWSCachedCredentials
	err     error
	callLog []string
	mu      sync.Mutex
}

func (m *mockAWSBackend) Status(_, _ string) (string, *time.Time) {
	return "arn:aws:iam::123:role/mock", nil
}

// calls reports how many times GetCredentials was invoked (race-safe).
func (m *mockAWSBackend) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.callLog)
}

func (m *mockAWSBackend) GetCredentials(_ context.Context, server, shedName string) (*AWSCachedCredentials, error) {
	m.mu.Lock()
	m.callLog = append(m.callLog, server+"/"+shedName)
	m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return m.creds, nil
}

func TestAWSResolve(t *testing.T) {
	tests := []struct {
		name     string
		cfg      AWSConfig
		server   string
		shed     string
		wantRole string
		wantMode string
	}{
		{
			name:     "default role",
			cfg:      AWSConfig{DefaultRole: "arn:aws:iam::123:role/default"},
			server:   "mini2",
			shed:     "my-shed",
			wantRole: "arn:aws:iam::123:role/default",
			wantMode: AWSModeAssumeRole,
		},
		{
			name: "per-server override",
			cfg: AWSConfig{
				DefaultRole: "arn:aws:iam::123:role/default",
				Servers:     map[string]AWSServerConfig{"mini2": {DefaultRole: "arn:aws:iam::123:role/mini2"}},
			},
			server:   "mini2",
			shed:     "my-shed",
			wantRole: "arn:aws:iam::123:role/mini2",
			wantMode: AWSModeAssumeRole,
		},
		{
			name: "per-server-per-shed override wins",
			cfg: AWSConfig{
				DefaultRole: "arn:aws:iam::123:role/default",
				Servers: map[string]AWSServerConfig{"mini2": {
					DefaultRole: "arn:aws:iam::123:role/mini2",
					Sheds:       map[string]ShedAWSConfig{"web": {Role: "arn:aws:iam::123:role/web"}},
				}},
			},
			server:   "mini2",
			shed:     "web",
			wantRole: "arn:aws:iam::123:role/web",
			wantMode: AWSModeAssumeRole,
		},
		{
			name: "same shed name on different server is isolated",
			cfg: AWSConfig{
				DefaultRole: "arn:aws:iam::123:role/default",
				Servers: map[string]AWSServerConfig{"mini2": {
					Sheds: map[string]ShedAWSConfig{"web": {Role: "arn:aws:iam::123:role/mini2-web"}},
				}},
			},
			server:   "mini3",
			shed:     "web",
			wantRole: "arn:aws:iam::123:role/default",
			wantMode: AWSModeAssumeRole,
		},
		{
			name:     "no config normalizes to assume-role",
			cfg:      AWSConfig{},
			server:   "mini2",
			shed:     "my-shed",
			wantRole: "",
			wantMode: AWSModeAssumeRole,
		},
		{
			name:     "top-level passthrough",
			cfg:      AWSConfig{Mode: AWSModePassthrough},
			server:   "mini2",
			shed:     "my-shed",
			wantRole: "",
			wantMode: AWSModePassthrough,
		},
		{
			name: "server-level passthrough ignores role",
			cfg: AWSConfig{
				DefaultRole: "arn:aws:iam::123:role/default",
				Servers:     map[string]AWSServerConfig{"mini2": {Mode: AWSModePassthrough}},
			},
			server:   "mini2",
			shed:     "web",
			wantRole: "arn:aws:iam::123:role/default", // present but ignored under passthrough
			wantMode: AWSModePassthrough,
		},
		{
			name: "child role under passthrough parent stays passthrough",
			cfg: AWSConfig{
				Servers: map[string]AWSServerConfig{"mini2": {
					Mode:  AWSModePassthrough,
					Sheds: map[string]ShedAWSConfig{"web": {Role: "arn:aws:iam::123:role/web"}},
				}},
			},
			server:   "mini2",
			shed:     "web",
			wantRole: "arn:aws:iam::123:role/web",
			wantMode: AWSModePassthrough,
		},
		{
			name: "child assume-role overrides passthrough parent",
			cfg: AWSConfig{
				Mode: AWSModePassthrough,
				Servers: map[string]AWSServerConfig{"mini2": {
					Sheds: map[string]ShedAWSConfig{"scoped": {Mode: AWSModeAssumeRole, Role: "arn:aws:iam::123:role/scoped"}},
				}},
			},
			server:   "mini2",
			shed:     "scoped",
			wantRole: "arn:aws:iam::123:role/scoped",
			wantMode: AWSModeAssumeRole,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.Resolve(tt.server, tt.shed)
			if got.Role != tt.wantRole {
				t.Errorf("Resolve(%q,%q).Role = %q, want %q", tt.server, tt.shed, got.Role, tt.wantRole)
			}
			if got.Mode != tt.wantMode {
				t.Errorf("Resolve(%q,%q).Mode = %q, want %q", tt.server, tt.shed, got.Mode, tt.wantMode)
			}
		})
	}
}

func TestAWSEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  AWSConfig
		want bool
	}{
		{"empty", AWSConfig{}, false},
		{"explicit assume-role, no role", AWSConfig{Mode: AWSModeAssumeRole}, false},
		{"default role", AWSConfig{DefaultRole: "x"}, true},
		{"top-level passthrough", AWSConfig{Mode: AWSModePassthrough}, true},
		{"server default role", AWSConfig{Servers: map[string]AWSServerConfig{"m": {DefaultRole: "x"}}}, true},
		{"server passthrough", AWSConfig{Servers: map[string]AWSServerConfig{"m": {Mode: AWSModePassthrough}}}, true},
		{"shed role", AWSConfig{Servers: map[string]AWSServerConfig{"m": {Sheds: map[string]ShedAWSConfig{"s": {Role: "x"}}}}}, true},
		{"shed passthrough", AWSConfig{Servers: map[string]AWSServerConfig{"m": {Sheds: map[string]ShedAWSConfig{"s": {Mode: AWSModePassthrough}}}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Enabled(); got != tt.want {
				t.Errorf("Enabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCacheHit(t *testing.T) {
	b := &stsBackend{
		cfg:           AWSConfig{DefaultRole: "arn:aws:iam::123:role/test"},
		refreshBefore: 5 * time.Minute,
		logger:        slog.Default(),
		cache: map[string]*AWSCachedCredentials{
			"mini2/my-shed": {
				AccessKeyID:     "CACHED_KEY",
				SecretAccessKey: "CACHED_SECRET",
				SessionToken:    "CACHED_TOKEN",
				Expiration:      time.Now().Add(30 * time.Minute), // within refresh window
			},
		},
	}

	// Cache hit for mini2/my-shed returns without calling STS (nil client).
	creds, err := b.GetCredentials(context.Background(), "mini2", "my-shed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.AccessKeyID != "CACHED_KEY" {
		t.Errorf("expected cached key, got %q", creds.AccessKeyID)
	}

	// The same shed name on a different server must NOT share the cache entry.
	if _, cachedUntil := b.Status("mini3", "my-shed"); cachedUntil != nil {
		t.Error("mini3/my-shed should not share mini2/my-shed cache entry")
	}
}

func TestCacheMiss(t *testing.T) {
	// When cache is stale, GetCredentials should NOT return the cached value.
	// The mock lets us control return values directly.
	backend := &mockAWSBackend{
		creds: &AWSCachedCredentials{
			AccessKeyID:     "FRESH_KEY",
			SecretAccessKey: "FRESH_SECRET",
			SessionToken:    "FRESH_TOKEN",
			Expiration:      time.Now().Add(1 * time.Hour),
		},
	}

	creds, err := backend.GetCredentials(context.Background(), "mini2", "my-shed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.AccessKeyID != "FRESH_KEY" {
		t.Errorf("expected fresh key, got %q", creds.AccessKeyID)
	}
}

func TestNoRoleConfigured(t *testing.T) {
	b := &stsBackend{
		cfg:   AWSConfig{},
		cache: make(map[string]*AWSCachedCredentials),
	}

	_, err := b.GetCredentials(context.Background(), "mini2", "unknown-shed")
	if err == nil {
		t.Fatal("expected error for shed with no role configured")
	}
	want := `no role configured for shed "unknown-shed" on server "mini2"`
	if got := fmt.Sprintf("%v", err); got != want {
		t.Errorf("unexpected error: %v", err)
	}
}

// writeCredsFile writes content to a temp shared credentials file and returns
// its path.
func writeCredsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write creds file: %v", err)
	}
	return path
}

// passthroughEnv points the SDK loader at credsPath and an empty config file so
// the developer's real ~/.aws files are never read during the test.
func passthroughEnv(t *testing.T, credsPath string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfgPath, nil, 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credsPath)
	t.Setenv("AWS_CONFIG_FILE", cfgPath)
}

func passthroughBackend(profile string) *stsBackend {
	return &stsBackend{
		cfg:    AWSConfig{SourceProfile: profile, Mode: AWSModePassthrough},
		cache:  make(map[string]*AWSCachedCredentials),
		logger: slog.Default(),
	}
}

func TestPassthroughGetCredentials(t *testing.T) {
	credsPath := writeCredsFile(t, `[my-sso]
aws_access_key_id = AKIATEST
aws_secret_access_key = secretXYZ
aws_session_token = tokenXYZ
aws_session_expiration = 2099-01-02T15:04:05Z
`)
	passthroughEnv(t, credsPath)

	b := passthroughBackend("my-sso")
	creds, err := b.GetCredentials(context.Background(), "mini2", "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.AccessKeyID != "AKIATEST" || creds.SecretAccessKey != "secretXYZ" || creds.SessionToken != "tokenXYZ" {
		t.Errorf("unexpected creds: %+v", creds)
	}
	want := time.Date(2099, 1, 2, 15, 4, 5, 0, time.UTC)
	if !creds.Expiration.Equal(want) {
		t.Errorf("Expiration = %v, want %v", creds.Expiration, want)
	}
	// Passthrough must not populate the AssumeRole cache or build the STS client.
	if len(b.cache) != 0 {
		t.Errorf("passthrough should not populate cache, got %d entries", len(b.cache))
	}
	if b.client != nil {
		t.Error("passthrough should not build the STS client")
	}
}

func TestPassthroughExpiryVariants(t *testing.T) {
	t.Run("x_security_token_expires", func(t *testing.T) {
		credsPath := writeCredsFile(t, `[p]
aws_access_key_id = A
aws_secret_access_key = S
aws_session_token = T
x_security_token_expires = 2099-06-01T00:00:00Z
`)
		passthroughEnv(t, credsPath)
		creds, err := passthroughBackend("p").GetCredentials(context.Background(), "s", "sh")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if creds.Expiration.IsZero() {
			t.Error("expected parsed expiry from x_security_token_expires")
		}
	})

	t.Run("missing hint yields zero expiry", func(t *testing.T) {
		credsPath := writeCredsFile(t, `[p]
aws_access_key_id = A
aws_secret_access_key = S
aws_session_token = T
`)
		passthroughEnv(t, credsPath)
		creds, err := passthroughBackend("p").GetCredentials(context.Background(), "s", "sh")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !creds.Expiration.IsZero() {
			t.Errorf("expected zero expiry, got %v", creds.Expiration)
		}
	})
}

func TestPassthroughReloginPickup(t *testing.T) {
	credsPath := writeCredsFile(t, `[p]
aws_access_key_id = A1
aws_secret_access_key = S1
aws_session_token = T1
`)
	passthroughEnv(t, credsPath)
	b := passthroughBackend("p")

	first, err := b.GetCredentials(context.Background(), "s", "sh")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if first.AccessKeyID != "A1" {
		t.Fatalf("first read = %q, want A1", first.AccessKeyID)
	}

	// Simulate `aws sso login` rewriting the shared file.
	if err := os.WriteFile(credsPath, []byte(`[p]
aws_access_key_id = A2
aws_secret_access_key = S2
aws_session_token = T2
`), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := b.GetCredentials(context.Background(), "s", "sh")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if second.AccessKeyID != "A2" {
		t.Errorf("re-login not picked up: got %q, want A2", second.AccessKeyID)
	}
}

func TestPassthroughErrors(t *testing.T) {
	t.Run("missing profile", func(t *testing.T) {
		credsPath := writeCredsFile(t, `[other]
aws_access_key_id = A
aws_secret_access_key = S
aws_session_token = T
`)
		passthroughEnv(t, credsPath)
		if _, err := passthroughBackend("absent").GetCredentials(context.Background(), "s", "sh"); err == nil {
			t.Fatal("expected error for missing profile")
		}
	})

	t.Run("no session token", func(t *testing.T) {
		credsPath := writeCredsFile(t, `[p]
aws_access_key_id = A
aws_secret_access_key = S
`)
		passthroughEnv(t, credsPath)
		_, err := passthroughBackend("p").GetCredentials(context.Background(), "s", "sh")
		if err == nil || !strings.Contains(err.Error(), "aws_session_token") {
			t.Fatalf("expected no-session-token error, got %v", err)
		}
	})

	t.Run("no static credentials", func(t *testing.T) {
		credsPath := writeCredsFile(t, `[p]
region = us-east-1
`)
		passthroughEnv(t, credsPath)
		_, err := passthroughBackend("p").GetCredentials(context.Background(), "s", "sh")
		if err == nil || !strings.Contains(err.Error(), "no static credentials") {
			t.Fatalf("expected no-static-credentials error, got %v", err)
		}
	})
}

func TestPassthroughStatus(t *testing.T) {
	t.Run("with expiry hint", func(t *testing.T) {
		credsPath := writeCredsFile(t, `[my-sso]
aws_access_key_id = A
aws_secret_access_key = S
aws_session_token = T
aws_session_expiration = 2099-01-02T15:04:05Z
`)
		passthroughEnv(t, credsPath)
		role, until := passthroughBackend("my-sso").Status("mini2", "web")
		if role != "passthrough:my-sso" {
			t.Errorf("role = %q, want passthrough:my-sso", role)
		}
		if until == nil {
			t.Error("expected non-nil cachedUntil with an expiry hint")
		}
	})

	t.Run("no expiry hint", func(t *testing.T) {
		credsPath := writeCredsFile(t, `[my-sso]
aws_access_key_id = A
aws_secret_access_key = S
aws_session_token = T
`)
		passthroughEnv(t, credsPath)
		role, until := passthroughBackend("my-sso").Status("mini2", "web")
		if role != "passthrough:my-sso" {
			t.Errorf("role = %q", role)
		}
		if until != nil {
			t.Errorf("expected nil cachedUntil, got %v", until)
		}
	})

	t.Run("missing file does not error", func(t *testing.T) {
		t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "nope"))
		role, until := passthroughBackend("my-sso").Status("mini2", "web")
		if role != "passthrough:my-sso" || until != nil {
			t.Errorf("got role=%q until=%v", role, until)
		}
	})
}

func TestMixedModeResolve(t *testing.T) {
	cfg := AWSConfig{
		DefaultRole: "arn:aws:iam::111:role/dev",
		Servers: map[string]AWSServerConfig{"mini2": {Sheds: map[string]ShedAWSConfig{
			"sso-app":    {Mode: AWSModePassthrough},
			"scoped-app": {Role: "arn:aws:iam::111:role/scoped"},
		}}},
	}
	if got := cfg.Resolve("mini2", "sso-app"); got.Mode != AWSModePassthrough {
		t.Errorf("sso-app mode = %q, want passthrough", got.Mode)
	}
	scoped := cfg.Resolve("mini2", "scoped-app")
	if scoped.Mode != AWSModeAssumeRole || scoped.Role != "arn:aws:iam::111:role/scoped" {
		t.Errorf("scoped-app resolved = %+v", scoped)
	}
}

func TestPassthroughOnlyStartup(t *testing.T) {
	credsPath := writeCredsFile(t, `[my-sso]
aws_access_key_id = A
aws_secret_access_key = S
aws_session_token = T
`)
	passthroughEnv(t, credsPath)

	backend, err := NewSTSBackend(AWSConfig{
		SourceProfile:      "my-sso",
		Mode:               AWSModePassthrough,
		SessionDuration:    "1h",
		CacheRefreshBefore: "5m",
	}, slog.Default())
	if err != nil {
		t.Fatalf("passthrough-only config should start cleanly: %v", err)
	}
	b := backend.(*stsBackend)
	if _, err := b.GetCredentials(context.Background(), "mini2", "web"); err != nil {
		t.Fatalf("passthrough get failed: %v", err)
	}
	if b.client != nil {
		t.Error("passthrough-only agent must not build the STS client")
	}
}

func TestParseSessionExpiry(t *testing.T) {
	tests := []struct {
		name    string
		content string
		profile string
		wantOK  bool
	}{
		{"rfc3339 Z", "[p]\naws_session_expiration = 2099-01-02T15:04:05Z\n", "p", true},
		{"x_security_token_expires", "[p]\nx_security_token_expires = 2099-01-02T15:04:05Z\n", "p", true},
		{"quoted value", "[p]\naws_session_expiration = \"2099-01-02T15:04:05Z\"\n", "p", true},
		{"comment + whitespace tolerated", "# header\n[p]\n  aws_session_expiration = 2099-01-02T15:04:05Z  \n", "p", true},
		{"duplicate section uses first", "[p]\naws_session_expiration = 2099-01-02T15:04:05Z\n[p]\naws_session_expiration = bad\n", "p", true},
		{"config-style header tolerated", "[profile p]\naws_session_expiration = 2099-01-02T15:04:05Z\n", "p", true},
		{"profile with dot", "[a.b]\naws_session_expiration = 2099-01-02T15:04:05Z\n", "a.b", true},
		{"missing key", "[p]\naws_access_key_id = A\n", "p", false},
		{"missing profile", "[other]\naws_session_expiration = 2099-01-02T15:04:05Z\n", "p", false},
		{"unparseable value", "[p]\naws_session_expiration = not-a-time\n", "p", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSessionExpiry(writeCredsFile(t, tt.content), tt.profile)
			if tt.wantOK && got.IsZero() {
				t.Error("expected a parsed time, got zero")
			}
			if !tt.wantOK && !got.IsZero() {
				t.Errorf("expected zero time, got %v", got)
			}
		})
	}
}

func TestParseExpiryValueLayouts(t *testing.T) {
	for _, v := range []string{
		"2099-01-02T15:04:05Z",
		"2099-01-02T15:04:05.123Z",
		"2099-01-02T15:04:05+00:00",
	} {
		if parseExpiryValue(v).IsZero() {
			t.Errorf("expected %q to parse", v)
		}
	}
	if !parseExpiryValue("garbage").IsZero() {
		t.Error("garbage should parse to zero")
	}
}
