package config

// The CLI-written config fixture (plan 037 A8, the promise of the desktop
// app's config reader): this test WRITES crates/fixtures/clientconfig/ under
// UPDATE_CLIENTCONFIG=1 and CHECKS it otherwise, so a stale fixture fails
// `make test`. config.yaml is the widest file the CLI's own save path
// (ClientConfig.Save) writes for ordinary values: every ServerEntry field set
// on one entry, entry names with a colon and names the saver must quote,
// values with a `#`, a quote and non-ASCII letters, several entries, and the
// other top-level sections (default_server, sheds, create_timeout) plus a
// legacy `machines:` block the save preserves opaquely and does not decode.
// expected.json records, from the structs, what each server entry IS to the CLI.
// The Rust readers (shed-core's config reader and the broker's discovery)
// read the same files and must read every entry's host, ssh_port, user and
// identity_file as recorded. It lives under crates/ because the Docker core
// leg mounts only crates/.
//
//	UPDATE_CLIENTCONFIG=1 go test ./internal/config -run TestClientConfigFixture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

const clientConfigFixtureDir = "../../crates/fixtures/clientconfig"

// clientConfigFixtureMachines is a legacy `machines:` section the file starts
// with. The CLI does not decode it; loading and saving must keep it
// (ClientConfig.Machines), which is what proves the opaque passthrough.
const clientConfigFixtureMachines = `machines:
    mini3:
        host: mini3.example.test
        user: pat
        ssh_port: 2222
`

// clientConfigFixtureServers is every server entry, by name. The names and
// values are ones a person can give the CLI (`shed server add <name> ...`,
// `--identity-file`), or that it records from a server.
func clientConfigFixtureServers() map[string]ServerEntry {
	at := func(day int) time.Time { return time.Date(2026, 1, day, 3, 4, 5, 0, time.UTC) }
	return map[string]ServerEntry{
		// Every one of the 15 fields set.
		"prod": {
			Host:                  "prod.example.test",
			HTTPPort:              8080,
			SSHPort:               2222,
			AddedAt:               at(2),
			ControlToken:          "shed_control_0123#abc",
			CredentialsToken:      "shed_credentials_4567",
			ControlTokenExpiresAt: at(3),
			APIURL:                "https://prod.example.test:8443",
			TLSCertFingerprint:    "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
			AuthMode:              "mtls",
			ClientCertFile:        "/home/pat/.shed/creds/prod/client.crt",
			ClientKeyFile:         "/home/pat/.shed/creds/prod/client.key",
			ClientCertExpiresAt:   at(4),
			User:                  "pat",
			IdentityFile:          "/home/pat/.ssh/id_work",
		},
		// A name with a colon, written as a plain key.
		"prod:blue": {
			Host:         "blue.example.test",
			SSHPort:      2222,
			AddedAt:      at(2),
			User:         "pat",
			IdentityFile: "/home/pat/.ssh/id:blue",
		},
		// A name that is an address and a port.
		"127.0.0.1:8443": {
			Host:    "127.0.0.1",
			SSHPort: 12222,
			AddedAt: at(2),
			APIURL:  "https://127.0.0.1:8443",
		},
		// Names the saver must quote: a colon and a space, a word YAML reads
		// as a boolean, a number, and a ` #`.
		"team: blue": {Host: "team.example.test", SSHPort: 22, AddedAt: at(2), User: "admin"},
		"true":       {Host: "bool.example.test", SSHPort: 22, AddedAt: at(2)},
		"1234":       {Host: "number.example.test", SSHPort: 22, AddedAt: at(2)},
		"build #1":   {Host: "build.example.test", SSHPort: 22, AddedAt: at(2)},
		// Values with a `#` (inside a word, and after a space), a quote, and
		// non-ASCII letters.
		"hash": {
			Host:           "hash.example.test",
			SSHPort:        22,
			AddedAt:        at(2),
			ClientCertFile: "/home/pat/certs/a#b.crt",
			IdentityFile:   "/home/pat/.ssh/id #work",
		},
		"quote": {
			Host:          "quote.example.test",
			SSHPort:       22,
			AddedAt:       at(2),
			ClientKeyFile: `/home/pat/keys/"q".key`,
			IdentityFile:  "/home/pat/.ssh/pat's key",
		},
		"non-ascii": {
			Host:         "café.example.test",
			SSHPort:      22,
			AddedAt:      at(2),
			User:         "pat",
			IdentityFile: "/home/pat/.ssh/clé_travail",
		},
		// The fewest fields the saver writes.
		"minimal": {Host: "minimal.example.test"},
	}
}

type clientConfigFixtureEntry struct {
	Name         string `json:"name"`
	Host         string `json:"host"`
	SSHPort      int    `json:"ssh_port"`
	User         string `json:"user"`
	IdentityFile string `json:"identity_file"`
}

type clientConfigFixtureJSON struct {
	Comment       []string                   `json:"_comment"`
	DefaultServer string                     `json:"default_server"`
	Servers       []clientConfigFixtureEntry `json:"servers"`
}

// buildClientConfigFixture computes the two generated files: config.yaml (the
// CLI's own load and save) and expected.json (the values the structs hold).
func buildClientConfigFixture(t *testing.T) map[string][]byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(clientConfigFixtureMachines), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadClientConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	servers := clientConfigFixtureServers()
	for name, entry := range servers {
		cfg.Servers[name] = entry
	}
	cfg.DefaultServer = "prod:blue"
	cfg.CreateTimeout = 5 * time.Minute
	cfg.Sheds["box"] = ShedCache{Server: "prod:blue", Status: "running", UpdatedAt: time.Date(2026, 1, 5, 6, 7, 8, 0, time.UTC)}
	cfg.Sheds["café"] = ShedCache{Server: "team: blue", Status: "stopped", UpdatedAt: time.Date(2026, 1, 5, 6, 7, 8, 0, time.UTC)}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	configYAML, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Save must keep the legacy section. expected.json does not record it:
	// shed no longer decodes `machines:`, and a fixture that dropped the
	// block would still satisfy the server assertions.
	if !bytes.Contains(configYAML, []byte("machines:")) || !bytes.Contains(configYAML, []byte("mini3.example.test")) {
		t.Fatalf("Save did not preserve the machines: section:\n%s", configYAML)
	}

	expected := clientConfigFixtureJSON{
		Comment: []string{
			"Written by internal/config/clientconfig_fixture_test.go (UPDATE_CLIENTCONFIG=1 go test ./internal/config -run TestClientConfigFixture); do not edit by hand.",
			"config.yaml is what the CLI's save path (config.ClientConfig.Save) writes for these values; servers are what the CLI holds for each entry. A machines: section in config.yaml is an opaque passthrough and is not decoded.",
			"Each Rust reader (shed-core's config reader, the broker's discovery) must read every server's host, ssh_port, user and identity_file as recorded here. See README.md.",
		},
		DefaultServer: cfg.DefaultServer,
		Servers:       []clientConfigFixtureEntry{},
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := servers[name]
		expected.Servers = append(expected.Servers, clientConfigFixtureEntry{
			Name: name, Host: e.Host, SSHPort: e.SSHPort, User: e.User, IdentityFile: e.IdentityFile,
		})
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(expected); err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{
		"config.yaml":   configYAML,
		"expected.json": buf.Bytes(),
	}
}

// TestClientConfigFixture writes or checks the fixture, and checks the Go side
// of it: config.yaml reads back, through the CLI's own loader, to every value
// the structs held.
func TestClientConfigFixture(t *testing.T) {
	files := buildClientConfigFixture(t)
	if os.Getenv("UPDATE_CLIENTCONFIG") == "1" {
		if err := os.MkdirAll(clientConfigFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(clientConfigFixtureDir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(clientConfigFixtureDir, name))
		if err != nil {
			t.Fatalf("read %s: %v (UPDATE_CLIENTCONFIG=1 go test ./internal/config -run TestClientConfigFixture writes it)", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("crates/fixtures/clientconfig/%s is stale: run UPDATE_CLIENTCONFIG=1 go test ./internal/config -run TestClientConfigFixture", name)
		}
	}

	cfg, err := LoadClientConfigFromPath(filepath.Join(clientConfigFixtureDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := clientConfigFixtureServers()
	if len(cfg.Servers) != len(want) {
		t.Errorf("config.yaml has %d servers, want %d", len(cfg.Servers), len(want))
	}
	for name, w := range want {
		got, ok := cfg.Servers[name]
		if !ok {
			t.Errorf("config.yaml has no server %q", name)
			continue
		}
		if !got.AddedAt.Equal(w.AddedAt) || !got.ControlTokenExpiresAt.Equal(w.ControlTokenExpiresAt) || !got.ClientCertExpiresAt.Equal(w.ClientCertExpiresAt) {
			t.Errorf("server %q reads back with other times: %+v, want %+v", name, got, w)
		}
		got.AddedAt, got.ControlTokenExpiresAt, got.ClientCertExpiresAt = w.AddedAt, w.ControlTokenExpiresAt, w.ClientCertExpiresAt
		if got != w {
			t.Errorf("server %q reads back as %+v, want %+v", name, got, w)
		}
	}
	if cfg.DefaultServer != "prod:blue" {
		t.Errorf("default_server reads back as %q", cfg.DefaultServer)
	}
}
