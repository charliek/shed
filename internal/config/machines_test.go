package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// captureStderr is shared with auth_mode_test.go (this package already
// carries a direct-Fprintln-to-stderr warning it needs to assert against).

func mustParseMachines(t *testing.T, yamlDoc string) []MachineEntry {
	t.Helper()
	var cfg ClientConfig
	if err := yaml.Unmarshal([]byte(yamlDoc), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return cfg.DecodeMachines()
}

func byName(entries []MachineEntry, name string) *MachineEntry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

// strOrEmpty mirrors expected.json's null-means-absent convention: a *string
// field decodes to nil when the fixture entry omits the key, and MachineEntry
// represents "absent" as "" rather than a pointer.
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestDecodeMachines_AbsentSectionIsEmpty(t *testing.T) {
	entries := mustParseMachines(t, "servers:\n    a:\n        host: h\n")
	if len(entries) != 0 {
		t.Fatalf("expected no entries, got %#v", entries)
	}
}

func TestDecodeMachines_BareEntryDefaultsHostAndPort(t *testing.T) {
	entries := mustParseMachines(t, "machines:\n    mini2: {}\n")
	got := byName(entries, "mini2")
	if got == nil {
		t.Fatalf("mini2 missing: %#v", entries)
	}
	want := MachineEntry{Name: "mini2", Host: "mini2", SSHPort: 22}
	if *got != want {
		t.Fatalf("got %#v, want %#v", *got, want)
	}
}

func TestDecodeMachines_FullEntry(t *testing.T) {
	entries := mustParseMachines(t, `machines:
    localmac:
        host: localhost
        user: charliek
        ssh_port: 2022
        known_hosts: /Users/dev/.ssh/known_hosts
`)
	got := byName(entries, "localmac")
	if got == nil {
		t.Fatalf("localmac missing: %#v", entries)
	}
	want := MachineEntry{
		Name:       "localmac",
		Host:       "localhost",
		User:       "charliek",
		SSHPort:    2022,
		KnownHosts: "/Users/dev/.ssh/known_hosts",
	}
	if *got != want {
		t.Fatalf("got %#v, want %#v", *got, want)
	}
}

// TestDecodeMachines_UnknownKeysAndRcBinIgnored pins the tolerance contract:
// rc_bin (Rust-owned, plan 019 pin P7) and any other key Go doesn't model are
// silently ignored, never causing a skip.
func TestDecodeMachines_UnknownKeysAndRcBinIgnored(t *testing.T) {
	entries := mustParseMachines(t, `machines:
    withrc:
        host: side.example.com
        rc_bin: /opt/homebrew/bin/shed-machine-rc
        color: blue
`)
	got := byName(entries, "withrc")
	if got == nil {
		t.Fatalf("withrc missing (rc_bin/unknown key must not cause a skip): %#v", entries)
	}
	want := MachineEntry{Name: "withrc", Host: "side.example.com", SSHPort: 22}
	if *got != want {
		t.Fatalf("got %#v, want %#v", *got, want)
	}
}

// TestDecodeMachines_MalformedEntrySkippedNotFatal is the negative control's
// sibling: a malformed entry (its value is a scalar, not a mapping) is
// skipped with a stderr note, and entries around it still decode. Break the
// "still decode" half below by temporarily commenting out the "ok" entry to
// see this test fail on its own assertion — restored before commit.
func TestDecodeMachines_MalformedEntrySkippedNotFatal(t *testing.T) {
	var entries []MachineEntry
	stderr := captureStderr(t, func() {
		entries = mustParseMachines(t, `machines:
    ok:
        host: fine.example.com
    broken: not-a-mapping-value
`)
	})

	if byName(entries, "broken") != nil {
		t.Fatalf("malformed entry must be skipped, got %#v", entries)
	}
	if got := byName(entries, "ok"); got == nil || got.Host != "fine.example.com" {
		t.Fatalf("entry beside the malformed one must still decode: %#v", entries)
	}
	if !bytes.Contains([]byte(stderr), []byte("broken")) {
		t.Fatalf("expected a stderr note naming the skipped entry, got %q", stderr)
	}
}

// TestDecodeMachines_OptionShapedHostOrUserSkipped: an ssh destination that
// begins with a dash is not a host.
//
// OpenSSH parses options BEFORE the destination word, so `host:
// "-oProxyCommand=…"` reaches ssh as an option and runs that command on the
// USER'S OWN machine — verified against the local ssh: `ssh -G -oPort=7777 --
// echo hi` reports `port 7777` and `hostname echo`, so even a trailing `--`
// cannot save it. roost refuses the same shape in its own target classifier.
// Skipped with a note, like every other malformed entry, and — as with those —
// the entries around it must still decode.
func TestDecodeMachines_OptionShapedHostOrUserSkipped(t *testing.T) {
	tests := []struct {
		name    string
		yamlDoc string
		skipped string
	}{
		{
			"an option-shaped host",
			`machines:
    ok:
        host: fine.example.com
    evil:
        host: -oProxyCommand=touch /tmp/pwned
`,
			"evil",
		},
		{
			"an option-shaped user",
			`machines:
    ok:
        host: fine.example.com
    evil:
        host: mini2
        user: -oProxyCommand=id
`,
			"evil",
		},
		{
			// No explicit `host:`, so the KEY becomes the host — the same
			// injection by another route.
			"an option-shaped entry name with no host of its own",
			`machines:
    ok:
        host: fine.example.com
    -oProxyCommand=id: {}
`,
			"-oProxyCommand=id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var entries []MachineEntry
			stderr := captureStderr(t, func() { entries = mustParseMachines(t, tt.yamlDoc) })

			if got := byName(entries, tt.skipped); got != nil {
				t.Fatalf("an option-shaped entry must be skipped, got %#v", *got)
			}
			if got := byName(entries, "ok"); got == nil || got.Host != "fine.example.com" {
				t.Fatalf("the entry beside it must still decode: %#v", entries)
			}
			if !bytes.Contains([]byte(stderr), []byte(tt.skipped)) {
				t.Fatalf("expected a stderr note naming the skipped entry, got %q", stderr)
			}
			if !bytes.Contains([]byte(stderr), []byte("begins with a dash")) {
				t.Fatalf("expected the note to say why, got %q", stderr)
			}
		})
	}

	// A dash elsewhere is ordinary and must not be touched — `mini-3` is a
	// perfectly good hostname and `build-bot` a perfectly good user.
	t.Run("a dash that is not the first character is fine", func(t *testing.T) {
		entries := mustParseMachines(t, `machines:
    mini-3:
        host: mini-3.local
        user: build-bot
`)
		got := byName(entries, "mini-3")
		if got == nil {
			t.Fatalf("mini-3 missing: %#v", entries)
		}
		if got.Host != "mini-3.local" || got.User != "build-bot" {
			t.Fatalf("got %#v", *got)
		}
	})
}

// TestDecodeMachines_SSHPortOutOfRangeFallsBackTo22 pins the Go decoder's
// range check against the Rust decoder's: shed-core's MachineEntry holds
// ssh_port as a u16, so a value that doesn't parse into 1..65535 falls back
// to 22 there. Go's plain `int` field would otherwise happily decode 70000
// or -1 verbatim; this asserts it instead falls back the same way, for
// every out-of-range shape EXCEPT 0 (see TestDecodeMachines_SSHPortZero
// below for that one, deliberately-still-divergent case).
func TestDecodeMachines_SSHPortOutOfRangeFallsBackTo22(t *testing.T) {
	tests := []struct {
		name string
		val  string
	}{
		{"too high", "70000"},
		{"negative", "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := mustParseMachines(t, "machines:\n    m:\n        ssh_port: "+tt.val+"\n")
			got := byName(entries, "m")
			if got == nil {
				t.Fatalf("m missing: %#v", entries)
			}
			if got.SSHPort != 22 {
				t.Fatalf("ssh_port = %d, want 22 (out-of-range fallback)", got.SSHPort)
			}
		})
	}
}

// TestDecodeMachines_SSHPortZero pins the ONE remaining Go/Rust divergence
// (documented in crates/fixtures/machines/README.md): Go's decoder falls
// back to 22 for ssh_port: 0 same as any other out-of-range value, while
// Rust's u16 parse accepts 0 verbatim. This is deliberately not "fixed" to
// match Rust — port 0 is meaningless for ssh either way, and the Rust
// parser is out of scope here.
func TestDecodeMachines_SSHPortZero(t *testing.T) {
	entries := mustParseMachines(t, "machines:\n    m:\n        ssh_port: 0\n")
	got := byName(entries, "m")
	if got == nil {
		t.Fatalf("m missing: %#v", entries)
	}
	if got.SSHPort != 22 {
		t.Fatalf("ssh_port = %d, want 22 (Go's fallback for the zero case)", got.SSHPort)
	}
}

// TestDecodeMachines_NonScalarKeyIsSkipped pins fix 5: a YAML complex key
// (`? {}` / `: {}`) must not reach the provider menu as a blank row. Break
// the "still decode" half below by temporarily commenting out the "ok" entry
// to see this test fail on its own assertion — restored before commit.
func TestDecodeMachines_NonScalarKeyIsSkipped(t *testing.T) {
	var entries []MachineEntry
	stderr := captureStderr(t, func() {
		entries = mustParseMachines(t, "machines:\n    ok:\n        host: fine.example.com\n    ? {}\n    : {}\n")
	})
	if len(entries) != 1 {
		t.Fatalf("want exactly the 1 well-formed entry, got %#v", entries)
	}
	if got := byName(entries, "ok"); got == nil || got.Host != "fine.example.com" {
		t.Fatalf("entry beside the complex-key one must still decode: %#v", entries)
	}
	if stderr == "" {
		t.Fatalf("expected a stderr note for the skipped complex-key entry")
	}
}

// TestDecodeMachines_EmptyKeyIsSkipped is NonScalarKeyIsSkipped's sibling: a
// scalar key whose value is the empty string is equally a blank-row hazard
// and must be skipped the same way.
func TestDecodeMachines_EmptyKeyIsSkipped(t *testing.T) {
	var entries []MachineEntry
	stderr := captureStderr(t, func() {
		entries = mustParseMachines(t, "machines:\n    ok:\n        host: fine.example.com\n    \"\":\n        host: blank.example.com\n")
	})
	if len(entries) != 1 {
		t.Fatalf("want exactly the 1 well-formed entry, got %#v", entries)
	}
	if got := byName(entries, "ok"); got == nil || got.Host != "fine.example.com" {
		t.Fatalf("entry beside the empty-key one must still decode: %#v", entries)
	}
	if stderr == "" {
		t.Fatalf("expected a stderr note for the skipped empty-key entry")
	}
}

// TestDecodeMachines_SharedFixture is the Go half of the two-language fixture
// under crates/fixtures/machines/ (see its README). expected.json's `rc_bin`
// key is deliberately NOT part of the Go comparison struct below — Go's
// decoder never models that field (pin P7), and json.Unmarshal silently
// ignores a key with no matching field, which is what makes the omission
// here a documented choice rather than an accident (spelled out in the
// fixture's README).
func TestDecodeMachines_SharedFixture(t *testing.T) {
	fixtureDir := filepath.Join("..", "..", "crates", "fixtures", "machines")

	cfg, err := LoadClientConfigFromPath(filepath.Join(fixtureDir, "sample.yaml"))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	got := cfg.DecodeMachines()

	expectedJSON, err := os.ReadFile(filepath.Join(fixtureDir, "expected.json"))
	if err != nil {
		t.Fatalf("read expected.json: %v", err)
	}
	var expected struct {
		Entries []struct {
			Name       string  `json:"name"`
			Host       string  `json:"host"`
			User       *string `json:"user"`
			SSHPort    int     `json:"ssh_port"`
			KnownHosts *string `json:"known_hosts"`
			// rc_bin intentionally not decoded here — see the doc comment above.
		} `json:"entries"`
	}
	if err := json.Unmarshal(expectedJSON, &expected); err != nil {
		t.Fatalf("unmarshal expected.json: %v", err)
	}

	// broken must be skipped, not merely absent from the expectation — pin
	// the count so a decoder that silently drops a THIRD entry can't pass by
	// accident.
	if len(got) != len(expected.Entries) {
		t.Fatalf("got %d entries, want %d: %#v", len(got), len(expected.Entries), got)
	}

	for _, want := range expected.Entries {
		entry := byName(got, want.Name)
		if entry == nil {
			t.Fatalf("fixture entry %q missing from decode", want.Name)
		}
		if entry.Host != want.Host {
			t.Errorf("%s: host = %q, want %q", want.Name, entry.Host, want.Host)
		}
		wantUser := strOrEmpty(want.User)
		if entry.User != wantUser {
			t.Errorf("%s: user = %q, want %q", want.Name, entry.User, wantUser)
		}
		if entry.SSHPort != want.SSHPort {
			t.Errorf("%s: ssh_port = %d, want %d", want.Name, entry.SSHPort, want.SSHPort)
		}
		wantKnownHosts := strOrEmpty(want.KnownHosts)
		if entry.KnownHosts != wantKnownHosts {
			t.Errorf("%s: known_hosts = %q, want %q", want.Name, entry.KnownHosts, wantKnownHosts)
		}
	}
	if byName(got, "broken") != nil {
		t.Fatalf("the malformed entry must be skipped, not decoded")
	}

	// ORDER, not just membership. expected.json is written sorted by name and
	// the fixture's document order deliberately is not, so this is the
	// assertion that proves the sort rather than assuming it — the byName
	// lookups above would pass on any permutation. Its Rust twin
	// (crates/shed-core/src/config.rs) asserts the same sequence.
	gotNames := make([]string, len(got))
	for i, m := range got {
		gotNames[i] = m.Name
	}
	wantNames := make([]string, len(expected.Entries))
	for i, e := range expected.Entries {
		wantNames[i] = e.Name
	}
	if !slices.Equal(gotNames, wantNames) {
		t.Errorf("machines: entries must be sorted by name: got %v, want %v", gotNames, wantNames)
	}
}
