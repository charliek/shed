package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/charliek/shed/internal/ext/protocol"
)

// mockDockerBackend implements DockerBackend for handler tests.
type mockDockerBackend struct {
	cred    *DockerCredential
	list    map[string]string
	err     error
	callLog []string
	mu      sync.Mutex
}

func (m *mockDockerBackend) GetCredentials(_ context.Context, _, _, serverURL string) (*DockerCredential, error) {
	m.mu.Lock()
	m.callLog = append(m.callLog, serverURL)
	m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return m.cred, nil
}

func (m *mockDockerBackend) ListCredentials(_ context.Context, _, _ string) (map[string]string, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.list, nil
}

func (m *mockDockerBackend) Status(_, _ string) (bool, int) {
	return false, 0
}

// mockExecutor implements helperExecutor for unit testing without real binaries.
type mockExecutor struct {
	cred *DockerCredential
	err  error
}

func (m *mockExecutor) execHelper(_ context.Context, helperName, serverURL string) (*DockerCredential, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.cred, nil
}

func TestNormalizeRegistry(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"us-docker.pkg.dev", "us-docker.pkg.dev"},
		{"https://us-docker.pkg.dev", "us-docker.pkg.dev"},
		{"https://index.docker.io/v1/", "index.docker.io"},
		{"https://index.docker.io/v1", "index.docker.io"},
		{"http://localhost:5000", "localhost:5000"},
		{"ghcr.io", "ghcr.io"},
		{"ghcr.io/", "ghcr.io"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeRegistry(tt.input)
			if got != tt.want {
				t.Errorf("normalizeRegistry(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDecodeInlineAuth(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("myuser:mypass"))
	cred, err := decodeInlineAuth("registry.example.com", encoded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Username != "myuser" {
		t.Errorf("Username = %q, want %q", cred.Username, "myuser")
	}
	if cred.Secret != "mypass" {
		t.Errorf("Secret = %q, want %q", cred.Secret, "mypass")
	}
}

func TestDecodeInlineAuthWithColonInPassword(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("user:pass:word:extra"))
	cred, err := decodeInlineAuth("registry.example.com", encoded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Username != "user" {
		t.Errorf("Username = %q, want %q", cred.Username, "user")
	}
	if cred.Secret != "pass:word:extra" {
		t.Errorf("Secret = %q, want %q", cred.Secret, "pass:word:extra")
	}
}

func TestDecodeInlineAuthInvalid(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("nocolon"))
	_, err := decodeInlineAuth("registry.example.com", encoded)
	if err == nil {
		t.Fatal("expected error for auth without colon")
	}
}

func TestGetCredentialsAllowlist(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	writeDockerConfig(t, configPath, dockerConfig{
		Auths: map[string]dockerAuthEntry{
			"allowed.io": {Auth: base64.StdEncoding.EncodeToString([]byte("user:pass"))},
			// blocked.io has a perfectly good local credential — the allowlist
			// must still refuse to serve it.
			"blocked.io": {Auth: base64.StdEncoding.EncodeToString([]byte("user:secret"))},
		},
	})

	b := &dockerHelperBackend{
		configPath: configPath,
		cfg:        DockerConfig{Registries: []string{"allowed.io"}},
		logger:     slog.Default(),
	}
	b.executor = b

	// Allowed registry should succeed
	cred, err := b.GetCredentials(context.Background(), "srv", "shed", "allowed.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Username != "user" {
		t.Errorf("Username = %q, want %q", cred.Username, "user")
	}

	// Blocked registry must fail with REGISTRY_NOT_ALLOWED even though a
	// credential for it exists locally — the allowlist is an explicit deny,
	// checked before any credential lookup, not just a credential filter.
	_, err = b.GetCredentials(context.Background(), "srv", "shed", "blocked.io")
	if err == nil {
		t.Fatal("expected error for blocked registry that has a local credential")
	}
	de, ok := err.(*dockerError)
	if !ok {
		t.Fatalf("expected *dockerError, got %T", err)
	}
	if de.code != protocol.DockerCodeNotAllowed {
		t.Errorf("code = %q, want %q", de.code, protocol.DockerCodeNotAllowed)
	}
}

func TestGetCredentialsAllowAll(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	writeDockerConfig(t, configPath, dockerConfig{
		Auths: map[string]dockerAuthEntry{
			"any-registry.io": {Auth: base64.StdEncoding.EncodeToString([]byte("user:pass"))},
		},
	})

	b := &dockerHelperBackend{
		configPath: configPath,
		cfg:        DockerConfig{AllowAll: true},
		logger:     slog.Default(),
	}
	b.executor = b

	cred, err := b.GetCredentials(context.Background(), "srv", "shed", "any-registry.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Username != "user" {
		t.Errorf("Username = %q, want %q", cred.Username, "user")
	}
}

func TestGetCredentialsCredHelper(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	writeDockerConfig(t, configPath, dockerConfig{
		CredHelpers: map[string]string{
			"us-docker.pkg.dev": "gcloud",
		},
	})

	b := &dockerHelperBackend{
		configPath: configPath,
		cfg:        DockerConfig{Registries: []string{"us-docker.pkg.dev"}},
		executor: &mockExecutor{
			cred: &DockerCredential{
				ServerURL: "us-docker.pkg.dev",
				Username:  "_json_key",
				Secret:    "gcloud-token",
			},
		},
		logger: slog.Default(),
	}

	cred, err := b.GetCredentials(context.Background(), "srv", "shed", "us-docker.pkg.dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Username != "_json_key" {
		t.Errorf("Username = %q, want %q", cred.Username, "_json_key")
	}
	if cred.Secret != "gcloud-token" {
		t.Errorf("Secret = %q, want %q", cred.Secret, "gcloud-token")
	}
}

func TestGetCredentialsCredsStore(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	writeDockerConfig(t, configPath, dockerConfig{
		CredsStore: "osxkeychain",
	})

	b := &dockerHelperBackend{
		configPath: configPath,
		cfg:        DockerConfig{AllowAll: true},
		executor: &mockExecutor{
			cred: &DockerCredential{
				ServerURL: "registry.example.com",
				Username:  "kc-user",
				Secret:    "kc-secret",
			},
		},
		logger: slog.Default(),
	}

	cred, err := b.GetCredentials(context.Background(), "srv", "shed", "registry.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Username != "kc-user" {
		t.Errorf("Username = %q, want %q", cred.Username, "kc-user")
	}
}

func TestGetCredentialsPriority(t *testing.T) {
	// credHelpers should take priority over credsStore and auths
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	writeDockerConfig(t, configPath, dockerConfig{
		CredHelpers: map[string]string{
			"registry.example.com": "custom-helper",
		},
		CredsStore: "osxkeychain",
		Auths: map[string]dockerAuthEntry{
			"registry.example.com": {Auth: base64.StdEncoding.EncodeToString([]byte("inline:creds"))},
		},
	})

	b := &dockerHelperBackend{
		configPath: configPath,
		cfg:        DockerConfig{AllowAll: true},
		executor: &mockExecutor{
			cred: &DockerCredential{
				ServerURL: "registry.example.com",
				Username:  "helper-user",
				Secret:    "helper-secret",
			},
		},
		logger: slog.Default(),
	}

	cred, err := b.GetCredentials(context.Background(), "srv", "shed", "registry.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Username != "helper-user" {
		t.Errorf("Username = %q, want %q (credHelper should win)", cred.Username, "helper-user")
	}
}

func TestGetCredentialsNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	writeDockerConfig(t, configPath, dockerConfig{})

	b := &dockerHelperBackend{
		configPath: configPath,
		cfg:        DockerConfig{AllowAll: true},
		logger:     slog.Default(),
	}
	b.executor = b

	_, err := b.GetCredentials(context.Background(), "srv", "shed", "unknown.io")
	if err == nil {
		t.Fatal("expected error for unknown registry")
	}
	de, ok := err.(*dockerError)
	if !ok {
		t.Fatalf("expected *dockerError, got %T", err)
	}
	if de.code != protocol.DockerCodeNotFound {
		t.Errorf("code = %q, want %q", de.code, protocol.DockerCodeNotFound)
	}
}

func TestGetCredentialsMissingConfig(t *testing.T) {
	b := &dockerHelperBackend{
		configPath: "/nonexistent/config.json",
		cfg:        DockerConfig{AllowAll: true},
		logger:     slog.Default(),
	}
	b.executor = b

	_, err := b.GetCredentials(context.Background(), "srv", "shed", "any.io")
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}

func TestListCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	writeDockerConfig(t, configPath, dockerConfig{
		CredHelpers: map[string]string{
			"gcr.io":     "gcloud",
			"blocked.io": "helper",
		},
		Auths: map[string]dockerAuthEntry{
			"ghcr.io": {Auth: base64.StdEncoding.EncodeToString([]byte("user:token"))},
		},
	})

	b := &dockerHelperBackend{
		configPath: configPath,
		cfg:        DockerConfig{Registries: []string{"gcr.io", "ghcr.io"}},
		logger:     slog.Default(),
	}
	b.executor = b

	result, err := b.ListCredentials(context.Background(), "srv", "shed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := result["gcr.io"]; !ok {
		t.Error("expected gcr.io in result")
	}
	if _, ok := result["ghcr.io"]; !ok {
		t.Error("expected ghcr.io in result")
	}
	if _, ok := result["blocked.io"]; ok {
		t.Error("blocked.io should not be in result")
	}
}

func TestFindDockerConfigEnvVar(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DOCKER_CONFIG", tmpDir)
	got := findDockerConfig()
	if got != configPath {
		t.Errorf("findDockerConfig() = %q, want %q", got, configPath)
	}
}

func TestStatus(t *testing.T) {
	b := &dockerHelperBackend{
		cfg:    DockerConfig{Registries: []string{"a.io", "b.io"}},
		logger: slog.Default(),
	}

	allowAll, count := b.Status("srv", "shed")
	if allowAll {
		t.Error("expected allowAll=false")
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func writeDockerConfig(t *testing.T, path string, cfg dockerConfig) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestReadConfigEmpty(t *testing.T) {
	b := &dockerHelperBackend{
		configPath: "",
		logger:     slog.Default(),
	}

	cfg, err := b.readConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CredHelpers != nil || cfg.CredsStore != "" || cfg.Auths != nil {
		t.Error("expected empty config when no path set")
	}
}

func TestDockerErrorInterface(t *testing.T) {
	err := &dockerError{msg: "test error", code: "TEST_CODE"}
	if err.Error() != "test error" {
		t.Errorf("Error() = %q, want %q", err.Error(), "test error")
	}

	// Verify it satisfies the error interface
	var _ error = err
	_ = fmt.Sprintf("%v", err) // should not panic
}

func TestLookHelperPath(t *testing.T) {
	dir := t.TempDir()
	binName := "docker-credential-faketest-look"
	binPath := filepath.Join(dir, binName)
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("found in extra dir when off PATH", func(t *testing.T) {
		got, err := lookHelperPath(binName, []string{dir})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != binPath {
			t.Errorf("path = %q, want %q", got, binPath)
		}
	})

	t.Run("missing binary errors and names searched dirs", func(t *testing.T) {
		_, err := lookHelperPath("docker-credential-does-not-exist", []string{dir})
		if err == nil {
			t.Fatal("expected error for missing helper")
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error %q should name searched dir %q", err, dir)
		}
	})

	t.Run("non-executable file is skipped", func(t *testing.T) {
		plain := "docker-credential-faketest-plain"
		if err := os.WriteFile(filepath.Join(dir, plain), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := lookHelperPath(plain, []string{dir}); err == nil {
			t.Error("expected non-executable file to be skipped")
		}
	})

	t.Run("directory matching the name is skipped", func(t *testing.T) {
		dirBin := "docker-credential-faketest-dir"
		if err := os.Mkdir(filepath.Join(dir, dirBin), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := lookHelperPath(dirBin, []string{dir}); err == nil {
			t.Error("expected a directory candidate to be skipped")
		}
	})
}

func TestAugmentPATH(t *testing.T) {
	sep := string(os.PathListSeparator)

	t.Run("appends missing dirs and preserves other env", func(t *testing.T) {
		env := []string{"HOME=/home/user", "PATH=/usr/bin" + sep + "/bin"}
		got := augmentPATH(env, []string{"/usr/local/bin", "/opt/homebrew/bin"})

		if !slices.Contains(got, "HOME=/home/user") {
			t.Error("HOME entry should be preserved")
		}
		pathVal := pathValue(t, got)
		for _, want := range []string{"/usr/bin", "/bin", "/usr/local/bin", "/opt/homebrew/bin"} {
			if !slices.Contains(filepath.SplitList(pathVal), want) {
				t.Errorf("PATH %q missing %q", pathVal, want)
			}
		}
	})

	t.Run("does not duplicate dirs already present", func(t *testing.T) {
		env := []string{"PATH=/usr/local/bin" + sep + "/usr/bin"}
		got := augmentPATH(env, []string{"/usr/local/bin"})
		dirs := filepath.SplitList(pathValue(t, got))
		if n := slices.Index(dirs, "/usr/local/bin"); n < 0 {
			t.Fatal("/usr/local/bin should be present")
		}
		count := 0
		for _, d := range dirs {
			if d == "/usr/local/bin" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("/usr/local/bin appears %d times, want 1", count)
		}
	})

	t.Run("adds PATH entry when absent", func(t *testing.T) {
		env := []string{"HOME=/home/user"}
		got := augmentPATH(env, []string{"/opt/homebrew/bin"})
		if !slices.Contains(filepath.SplitList(pathValue(t, got)), "/opt/homebrew/bin") {
			t.Errorf("expected PATH entry containing /opt/homebrew/bin, got %v", got)
		}
	})

	t.Run("augments the last PATH entry when duplicated", func(t *testing.T) {
		// exec.Cmd.Env uses the last duplicate, so that one must be augmented.
		env := []string{"PATH=/early", "HOME=/home/user", "PATH=/usr/bin"}
		got := augmentPATH(env, []string{"/opt/homebrew/bin"})
		dirs := filepath.SplitList(pathValue(t, got))
		if !slices.Contains(dirs, "/opt/homebrew/bin") {
			t.Errorf("effective PATH %v should contain /opt/homebrew/bin", dirs)
		}
		if !slices.Contains(dirs, "/usr/bin") {
			t.Errorf("effective PATH %v should retain /usr/bin", dirs)
		}
		if got[0] != "PATH=/early" {
			t.Errorf("first PATH entry = %q, want it left unchanged", got[0])
		}
	})

	t.Run("empty PATH value yields only the extra dirs", func(t *testing.T) {
		got := augmentPATH([]string{"PATH="}, []string{"/opt/homebrew/bin"})
		// Must not produce a leading separator (an empty PATH element means
		// "current directory", a footgun we don't want to introduce).
		if v := pathValue(t, got); v != "/opt/homebrew/bin" {
			t.Errorf("PATH = %q, want %q", v, "/opt/homebrew/bin")
		}
	})
}

// pathValue returns the effective PATH value in env — the last PATH= entry,
// matching exec.Cmd.Env's last-key-wins semantics — failing if none exists.
func pathValue(t *testing.T, env []string) string {
	t.Helper()
	val, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			val, found = v, true
		}
	}
	if !found {
		t.Fatal("no PATH entry found")
	}
	return val
}

func TestExecHelperResolvesViaExtraDir(t *testing.T) {
	dir := t.TempDir()
	// Fake helper: drains stdin (the serverURL) and emits a credential JSON.
	script := "#!/bin/sh\ncat >/dev/null\n" +
		`printf '%s' '{"ServerURL":"registry.example.com","Username":"fake-user","Secret":"fake-secret"}'` + "\n"
	helper := filepath.Join(dir, "docker-credential-faketest")
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	b := &dockerHelperBackend{
		helperDirs: []string{dir}, // dir is not on PATH, so this exercises the fallback
		logger:     slog.Default(),
	}

	cred, err := b.execHelper(context.Background(), "faketest", "registry.example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred.Username != "fake-user" {
		t.Errorf("Username = %q, want %q", cred.Username, "fake-user")
	}
	if cred.Secret != "fake-secret" {
		t.Errorf("Secret = %q, want %q", cred.Secret, "fake-secret")
	}
}

func TestExecHelperMissingBinary(t *testing.T) {
	b := &dockerHelperBackend{
		helperDirs: []string{t.TempDir()}, // empty dir, binary nowhere
		logger:     slog.Default(),
	}

	_, err := b.execHelper(context.Background(), "definitely-missing", "registry.example.com")
	if err == nil {
		t.Fatal("expected error for missing helper")
	}
	de, ok := err.(*dockerError)
	if !ok {
		t.Fatalf("expected *dockerError, got %T", err)
	}
	if de.code != protocol.DockerCodeHelperFailed {
		t.Errorf("code = %q, want %q", de.code, protocol.DockerCodeHelperFailed)
	}
}
