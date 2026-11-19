package main

// The cross-language identity_file fixture (plan 037 §3.3): this test WRITES
// crates/fixtures/identityfile/ under UPDATE_IDENTITYFILE=1 and CHECKS it
// otherwise, so a stale fixture fails `make test`. The Rust readers (shed-core's
// config reader and the broker's discovery) read the same files and must
// produce the same argv and ssh config lines for each accepted entry, and
// refuse each refused one, before any ssh is spawned. It lives under crates/
// because the Docker core leg mounts only crates/.
//
//	UPDATE_IDENTITYFILE=1 go test ./cmd/shed -run TestIdentityFileFixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/sshconfig"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

const identityFixtureDir = "../../crates/fixtures/identityfile"

// The endpoint every fixture entry names, and what the argv and stanzas are
// computed against.
const (
	identityFixtureHost       = "shed.example.test"
	identityFixturePort       = 2222
	identityFixtureHome       = "/home/pat"
	identityFixtureKnownHosts = "/home/pat/.shed/known_hosts"
	identityFixtureShed       = "box"
)

// identityFixtureAccepted is each accepted entry: its config name and its
// identity_file ("" for an entry without one, whose argv and stanza must be
// exactly those of an entry from before the field existed). The paths with a
// space, an apostrophe, a colon (alone, and followed by a space, which makes
// the saver quote the value) and a non-ASCII letter are the ones a reader is
// most likely to get wrong.
var identityFixtureAccepted = []struct{ name, path string }{
	{"none", ""},
	{"plain", "/home/pat/.ssh/id_work"},
	{"space", "/home/pat/.ssh/id work"},
	{"apostrophe", "/home/pat/.ssh/pat's key"},
	{"colon", "/home/pat/.ssh/id:work"},
	{"colon-space", "/home/pat/.ssh/keys: work"},
	{"non-ascii", "/home/pat/.ssh/cl\xc3\xa9_travail"},
	// Whitespace in the middle of a path is not refused (only CR and LF are):
	// a tab, and a no-break space (U+00A0).
	{"tab-inside", "/home/pat/.ssh/id\twork"},
	{"nbsp-inside", "/home/pat/.ssh/id\xc2\xa0work"},
}

// identityFixtureRefusedYAML is the hand-written config of refused entries:
// each identity_file breaks the rule, and every reader must refuse it. The
// values are written the way a person would write them, not the way the saver
// would.
const identityFixtureRefusedYAML = `# Hand-written for the identity_file fixture (see README.md). Every
# identity_file below breaks the rule; the Go reader and both Rust readers
# must refuse each one before any ssh is spawned.
servers:
  nul:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "/home/pat/.ssh/id\0work"
  cr:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "/home/pat/.ssh/id\rwork"
  lf:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "/home/pat/.ssh/id\nwork"
  double-quote:
    host: shed.example.test
    ssh_port: 2222
    identity_file: '/home/pat/.ssh/id"work'
  backslash:
    host: shed.example.test
    ssh_port: 2222
    identity_file: '/home/pat/.ssh/id\work'
  hash:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "/home/pat/.ssh/id#work"
  percent:
    host: shed.example.test
    ssh_port: 2222
    identity_file: '/home/pat/.ssh/id_%h'
  dollar:
    host: shed.example.test
    ssh_port: 2222
    identity_file: '/home/pat/.ssh/${KEY}'
  leading-space:
    host: shed.example.test
    ssh_port: 2222
    identity_file: " /home/pat/.ssh/id_work"
  trailing-space:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "/home/pat/.ssh/id_work "
  whitespace-only:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "   "
  relative:
    host: shed.example.test
    ssh_port: 2222
    identity_file: .ssh/id_work
  tilde:
    host: shed.example.test
    ssh_port: 2222
    identity_file: ~/.ssh/id_work
  leading-tab:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "\t/home/pat/.ssh/id_work"
  leading-nbsp:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "\_/home/pat/.ssh/id_work"
  trailing-next-line:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "/home/pat/.ssh/id_work\N"
  trailing-line-separator:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "/home/pat/.ssh/id_work\L"
  leading-ideographic-space:
    host: shed.example.test
    ssh_port: 2222
    identity_file: "\U00003000/home/pat/.ssh/id_work"
`

// identityFixtureRefused is each refused entry, in file order, with the value
// it decodes to and the reason class it is refused for.
var identityFixtureRefused = []struct{ name, value, class string }{
	{"nul", "/home/pat/.ssh/id\x00work", "nul"},
	{"cr", "/home/pat/.ssh/id\rwork", "cr"},
	{"lf", "/home/pat/.ssh/id\nwork", "lf"},
	{"double-quote", `/home/pat/.ssh/id"work`, "double_quote"},
	{"backslash", `/home/pat/.ssh/id\work`, "backslash"},
	{"hash", "/home/pat/.ssh/id#work", "hash"},
	{"percent", "/home/pat/.ssh/id_%h", "percent"},
	{"dollar", "/home/pat/.ssh/${KEY}", "dollar"},
	{"leading-space", " /home/pat/.ssh/id_work", "leading_whitespace"},
	{"trailing-space", "/home/pat/.ssh/id_work ", "trailing_whitespace"},
	{"whitespace-only", "   ", "whitespace_only"},
	{"relative", ".ssh/id_work", "not_absolute"},
	{"tilde", "~/.ssh/id_work", "not_absolute"},
	// Unicode White_Space at either end, not only ASCII (Go unicode.IsSpace,
	// Rust char::is_whitespace): a tab, U+00A0, U+0085, U+2028, U+3000.
	{"leading-tab", "\t/home/pat/.ssh/id_work", "leading_whitespace"},
	{"leading-nbsp", "\xc2\xa0/home/pat/.ssh/id_work", "leading_whitespace"},
	{"trailing-next-line", "/home/pat/.ssh/id_work\xc2\x85", "trailing_whitespace"},
	{"trailing-line-separator", "/home/pat/.ssh/id_work\xe2\x80\xa8", "trailing_whitespace"},
	{"leading-ideographic-space", "\xe3\x80\x80/home/pat/.ssh/id_work", "leading_whitespace"},
}

// identityReasonClass maps the Go reason to the fixture's class, so the
// fixture checks Go refuses each value for the reason it says.
func identityReasonClass(reason string) string {
	for _, c := range []struct{ words, class string }{
		{"NUL", "nul"},
		{"carriage return", "cr"},
		{"line feed", "lf"},
		{"double quote", "double_quote"},
		{"backslash", "backslash"},
		{"contains #", "hash"},
		{"contains %", "percent"},
		{"contains $", "dollar"},
		{"begins with whitespace", "leading_whitespace"},
		{"ends with whitespace", "trailing_whitespace"},
		{"only whitespace", "whitespace_only"},
		{"not an absolute path", "not_absolute"},
	} {
		if strings.Contains(reason, c.words) {
			return c.class
		}
	}
	return "unknown: " + reason
}

type identityFixtureAcceptedJSON struct {
	Entry        string   `json:"entry"`
	IdentityFile string   `json:"identity_file"`
	IdentityArgs []string `json:"identity_args"`
	// BootstrapArgv is the ssh argv (without the binary) of the entry's
	// `_bootstrap` exchange: scope control, client kind cli, no CSR.
	BootstrapArgv []string `json:"bootstrap_argv"`
	// SSHConfigLines are the lines the identity file adds to a Host stanza
	// (without indentation); SSHConfigStanza is the CLI's whole stanza for the
	// shed "box" on this entry.
	SSHConfigLines  []string `json:"ssh_config_lines"`
	SSHConfigStanza string   `json:"ssh_config_stanza"`
}

type identityFixtureRefusedJSON struct {
	Entry        string `json:"entry"`
	IdentityFile string `json:"identity_file"`
	Class        string `json:"class"`
}

type identityFixtureJSON struct {
	Comment    []string                      `json:"_comment"`
	Host       string                        `json:"host"`
	SSHPort    int                           `json:"ssh_port"`
	KnownHosts string                        `json:"known_hosts"`
	Shed       string                        `json:"shed"`
	Accepted   []identityFixtureAcceptedJSON `json:"accepted"`
	Refused    []identityFixtureRefusedJSON  `json:"refused"`
}

// buildIdentityFixture computes the three generated files: config.yaml (the
// CLI's own saver), refused.yaml (the hand-written text above) and
// expected.json (what the CLI computes for each entry).
func buildIdentityFixture(t *testing.T) map[string][]byte {
	t.Helper()
	t.Setenv("HOME", identityFixtureHome)
	if got := config.GetKnownHostsPath(); got != identityFixtureKnownHosts {
		t.Fatalf("known_hosts path = %q, want %q", got, identityFixtureKnownHosts)
	}

	// config.yaml, written by the saver every CLI command uses.
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.LoadClientConfigFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	addedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, a := range identityFixtureAccepted {
		cfg.Servers[a.name] = config.ServerEntry{Host: identityFixtureHost, SSHPort: identityFixturePort, AddedAt: addedAt, IdentityFile: a.path}
	}
	cfg.DefaultServer = "plain"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	configYAML, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	expected := identityFixtureJSON{
		Comment: []string{
			"Written by cmd/shed/identityfile_fixture_test.go (UPDATE_IDENTITYFILE=1 go test ./cmd/shed -run TestIdentityFileFixture); do not edit by hand.",
			"accepted: the entries of config.yaml; each reader must give the same identity_file, identity_args, bootstrap_argv and ssh_config_lines.",
			"refused: the entries of refused.yaml; each reader must refuse identity_file before any ssh is spawned.",
			"The file itself (a readable regular file whose public key can be learned without a passphrase) is checked at the time of use, separately: these paths need not exist. See README.md.",
		},
		Host: identityFixtureHost, SSHPort: identityFixturePort, KnownHosts: identityFixtureKnownHosts, Shed: identityFixtureShed,
		Accepted: []identityFixtureAcceptedJSON{},
		Refused:  []identityFixtureRefusedJSON{},
	}
	for _, a := range identityFixtureAccepted {
		if err := config.CheckIdentityFileValue(a.name, a.path); err != nil {
			t.Fatalf("accepted entry %s is refused: %v", a.name, err)
		}
		entry := cfg.Servers[a.name]
		argv, err := sdkbootstrap.Args(bootstrapParams(entry.Host, entry.SSHPort, entry.IdentityFile, "control", "cli"))
		if err != nil {
			t.Fatalf("bootstrap argv of %s: %v", a.name, err)
		}
		lines := strings.Split(strings.TrimSuffix(sshconfig.IdentityLines(entry.IdentityFile, ""), "\n"), "\n")
		if entry.IdentityFile == "" {
			lines = []string{}
		}
		identityArgs := sdkbootstrap.IdentityArgs(entry.IdentityFile)
		if identityArgs == nil {
			identityArgs = []string{}
		}
		expected.Accepted = append(expected.Accepted, identityFixtureAcceptedJSON{
			Entry:           a.name,
			IdentityFile:    entry.IdentityFile,
			IdentityArgs:    identityArgs,
			BootstrapArgv:   argv,
			SSHConfigLines:  lines,
			SSHConfigStanza: sshconfig.GenerateEntry(shedSSHConfigEntry(identityFixtureShed, &entry, config.GetKnownHostsPath())),
		})
	}
	for _, r := range identityFixtureRefused {
		expected.Refused = append(expected.Refused, identityFixtureRefusedJSON{Entry: r.name, IdentityFile: r.value, Class: r.class})
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
		"refused.yaml":  []byte(identityFixtureRefusedYAML),
		"expected.json": buf.Bytes(),
	}
}

// TestIdentityFileFixture writes or checks the fixture, and checks the Go side
// of its contract: config.yaml reads back to the same values, and each entry of
// refused.yaml decodes to the value recorded and is refused for its class.
func TestIdentityFileFixture(t *testing.T) {
	files := buildIdentityFixture(t)
	if os.Getenv("UPDATE_IDENTITYFILE") == "1" {
		if err := os.MkdirAll(identityFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(identityFixtureDir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(identityFixtureDir, name))
		if err != nil {
			t.Fatalf("read %s: %v (UPDATE_IDENTITYFILE=1 go test ./cmd/shed -run TestIdentityFileFixture writes it)", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("crates/fixtures/identityfile/%s is stale: run UPDATE_IDENTITYFILE=1 go test ./cmd/shed -run TestIdentityFileFixture", name)
		}
	}

	// config.yaml, read back as the CLI reads it.
	cfg, err := config.LoadClientConfigFromPath(filepath.Join(identityFixtureDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range identityFixtureAccepted {
		if got := cfg.Servers[a.name].IdentityFile; got != a.path {
			t.Errorf("config.yaml entry %s reads back as %q, want %q", a.name, got, a.path)
		}
	}

	// refused.yaml: each value decodes as recorded and is refused for its class.
	refused, err := config.LoadClientConfigFromPath(filepath.Join(identityFixtureDir, "refused.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(refused.Servers) != len(identityFixtureRefused) {
		t.Errorf("refused.yaml has %d entries, want %d", len(refused.Servers), len(identityFixtureRefused))
	}
	for _, r := range identityFixtureRefused {
		entry, ok := refused.Servers[r.name]
		if !ok {
			t.Errorf("refused.yaml has no entry %s", r.name)
			continue
		}
		if entry.IdentityFile != r.value {
			t.Errorf("refused.yaml entry %s decodes to %q, want %q", r.name, entry.IdentityFile, r.value)
		}
		err := config.CheckIdentityFileValue(r.name, entry.IdentityFile)
		var ie *config.IdentityFileError
		if !errors.As(err, &ie) {
			t.Errorf("refused.yaml entry %s is accepted", r.name)
			continue
		}
		if got := identityReasonClass(ie.Reason); got != r.class {
			t.Errorf("refused.yaml entry %s is refused as %q, want %q", r.name, got, r.class)
		}
	}
}
