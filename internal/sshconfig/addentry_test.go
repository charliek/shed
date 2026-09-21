package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newEntry(name string) Entry {
	// The shape cmd/shed/ssh_config.go's generateEntries produces: no
	// IdentityFile.
	return Entry{
		Name:           name,
		Host:           "localhost",
		Port:           2222,
		User:           strings.TrimPrefix(name, "shed-"),
		KnownHostsFile: "/home/me/.shed/known_hosts",
	}
}

func configPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".ssh", "config")
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}
}

func readConfigFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	return string(data)
}

func addOrFail(t *testing.T, path string, entry Entry) AddOutcome {
	t.Helper()
	outcome, err := AddEntryIfAbsent(path, entry)
	if err != nil {
		t.Fatalf("AddEntryIfAbsent: %v", err)
	}
	return outcome
}

// TestAddToAMissingFile: no ~/.ssh at all is the first-run case, and it has to
// produce a complete, usable config rather than an error.
func TestAddToAMissingFile(t *testing.T) {
	path := configPath(t)

	if outcome := addOrFail(t, path, newEntry("shed-demo")); outcome != AddedEntry {
		t.Fatalf("outcome = %v, want AddedEntry", outcome)
	}

	got := readConfigFile(t, path)
	for _, want := range []string{
		BeginMarker, EndMarker,
		"Host shed-demo\n", "    HostName localhost\n", "    Port 2222\n",
		"    User demo\n", "    UserKnownHostsFile /home/me/.shed/known_hosts\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the written config is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "IdentityFile") {
		t.Errorf("an IdentityFile was written:\n%s", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

const userConfig = `# my own config, hands off
Host *
    ServerAliveInterval 60

Host bastion
    HostName bastion.example.com
    User me
    IdentityFile ~/.ssh/id_bastion
`

// TestAddToAFileWithNoManagedBlock: the user's own config must come through
// byte for byte, with the block appended after it.
func TestAddToAFileWithNoManagedBlock(t *testing.T) {
	path := configPath(t)
	writeConfig(t, path, userConfig)

	if outcome := addOrFail(t, path, newEntry("shed-demo")); outcome != AddedEntry {
		t.Fatalf("outcome = %v, want AddedEntry", outcome)
	}

	got := readConfigFile(t, path)
	if !strings.HasPrefix(got, userConfig) {
		t.Errorf("the user's config did not come through byte for byte:\n%s", got)
	}
	if !strings.Contains(got, "Host shed-demo\n") {
		t.Errorf("the entry was not written:\n%s", got)
	}
	if strings.Index(got, BeginMarker) < strings.Index(got, "Host bastion") {
		t.Errorf("the managed block landed ahead of the user's entries:\n%s", got)
	}
}

// managedConfig is a config with user content on BOTH sides of a managed block
// that already holds two entries — the shape every interesting case needs.
const managedConfig = `# above the block
Host bastion
    HostName bastion.example.com
    User me

` + BeginMarker + `
# Do not edit manually - managed by shed CLI
# Last updated: 2020-01-01T00:00:00Z

Host shed-alpha
    HostName localhost
    Port 2222
    User alpha
    UserKnownHostsFile /home/me/.shed/known_hosts

Host shed-beta
    HostName mini3
    Port 22
    User beta
    UserKnownHostsFile /home/me/.shed/known_hosts
` + EndMarker + `

# below the block
Host laptop
    HostName laptop.local
`

// TestOtherManagedEntriesSurvive is the regression this whole helper exists
// for.
//
// ComputeDiff + Write — the path `shed ssh-config install` takes — is a
// whole-block writer: handed one entry it treats every other managed entry as
// a removal and deletes it. A caller that just wants `shed-gamma` to resolve
// has exactly one entry in hand, so reaching for that pair is the natural
// mistake, and this test is what catches it.
//
// It also asserts the block's own "Last updated" header is unchanged, which no
// regenerating writer could manage: the block was EDITED, not rebuilt.
func TestOtherManagedEntriesSurvive(t *testing.T) {
	path := configPath(t)
	writeConfig(t, path, managedConfig)

	if outcome := addOrFail(t, path, newEntry("shed-gamma")); outcome != AddedEntry {
		t.Fatalf("outcome = %v, want AddedEntry", outcome)
	}

	got := readConfigFile(t, path)
	parsed := Parse(got)
	names := parsed.GetEntryNames()
	want := []string{"shed-alpha", "shed-beta", "shed-gamma"}
	if len(names) != len(want) {
		t.Fatalf("managed entries = %v, want %v\n%s", names, want, got)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("managed entry %d = %q, want %q (order is the block's own)", i, names[i], want[i])
		}
	}

	// The survivors kept their exact bytes, not a regenerated equivalent —
	// shed-beta's port and host are not what generateEntries would produce for
	// it, so a rebuild would have flattened them.
	for _, line := range []string{"    HostName mini3\n", "    Port 22\n", "    User beta\n"} {
		if !strings.Contains(got, line) {
			t.Errorf("shed-beta lost %q:\n%s", line, got)
		}
	}
	if !strings.Contains(got, "# Last updated: 2020-01-01T00:00:00Z") {
		t.Errorf("the block header was regenerated — this is a whole-block rewrite:\n%s", got)
	}
}

// TestContentOutsideTheBlockIsPreservedByteForByte pins the third guarantee
// against the exact bytes, not against a "contains" check.
func TestContentOutsideTheBlockIsPreservedByteForByte(t *testing.T) {
	path := configPath(t)
	writeConfig(t, path, managedConfig)

	addOrFail(t, path, newEntry("shed-gamma"))
	got := readConfigFile(t, path)

	before, _, _ := strings.Cut(managedConfig, BeginMarker)
	_, afterWant, _ := strings.Cut(managedConfig, EndMarker)
	gotBefore, _, _ := strings.Cut(got, BeginMarker)
	_, gotAfter, _ := strings.Cut(got, EndMarker)

	if gotBefore != before {
		t.Errorf("content above the block changed:\n got %q\nwant %q", gotBefore, before)
	}
	if gotAfter != afterWant {
		t.Errorf("content below the block changed:\n got %q\nwant %q", gotAfter, afterWant)
	}
}

// TestAnAliasAlreadyInTheManagedBlockIsLeftAlone.
func TestAnAliasAlreadyInTheManagedBlockIsLeftAlone(t *testing.T) {
	path := configPath(t)
	writeConfig(t, path, managedConfig)

	// Deliberately a DIFFERENT entry body for the same alias: if anything
	// rewrote it, the port would move.
	entry := newEntry("shed-beta")
	entry.Port = 9999
	if outcome := addOrFail(t, path, entry); outcome != AlreadyManaged {
		t.Fatalf("outcome = %v, want AlreadyManaged", outcome)
	}

	if got := readConfigFile(t, path); got != managedConfig {
		t.Errorf("the file was modified:\n got %q\nwant %q", got, managedConfig)
	}
}

// TestWhatCountsAsAnExistingDeclaration drives the predicate behind the
// "leave it alone" half of this function, in both directions on one fixture
// set — because the two directions fail in opposite, equally bad ways.
//
// A hand-written entry outside the managed block is never overwritten and
// never shadowed by a duplicate appended below it. The spellings are all
// ordinary ssh: an indented line, a lower-case keyword, several patterns on
// one Host line. Missing any of them means writing a second `Host shed-…`
// into the same file, which is the failure a user notices as "shed keeps
// ignoring my ProxyJump".
//
// The other direction matters just as much: nearly every config has a `Host *`
// defaults stanza, and a commented `Host shed-demo` is prose. Reading either
// as "this alias already exists" would mean never writing an entry at all.
func TestWhatCountsAsAnExistingDeclaration(t *testing.T) {
	// mine wraps a hand-written stanza in a config that also has a managed
	// block, so the alias has to be found OUTSIDE the block to be found.
	mine := func(stanza string) string { return "# mine\n" + stanza + "\n" + managedConfig }

	tests := []struct {
		name    string
		content string
		want    AddOutcome
	}{
		{"a plain entry", mine("Host shed-demo\n    ProxyJump bastion\n"), AlreadyUserDefined},
		{"an indented entry", mine("  Host shed-demo\n      ProxyJump bastion\n"), AlreadyUserDefined},
		{"a lower-case keyword", mine("host shed-demo\n    ProxyJump bastion\n"), AlreadyUserDefined},
		{"one of several patterns", mine("Host shed-demo shed-demo.local\n    ProxyJump bastion\n"), AlreadyUserDefined},
		{"a wildcard defaults stanza", "Host *\n    ServerAliveInterval 60\n", AddedEntry},
		{"a commented-out Host line", "# Host shed-demo would go here\nHost bastion # shed-demo\n    User me\n", AddedEntry},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := configPath(t)
			writeConfig(t, path, tc.content)

			if outcome := addOrFail(t, path, newEntry("shed-demo")); outcome != tc.want {
				t.Fatalf("outcome = %v, want %v", outcome, tc.want)
			}
			got := readConfigFile(t, path)
			if tc.want == AlreadyUserDefined {
				if got != tc.content {
					t.Errorf("the file was modified:\n got %q\nwant %q", got, tc.content)
				}
				return
			}
			if !strings.Contains(got, "Host shed-demo\n") {
				t.Errorf("the entry was not written:\n%s", got)
			}
		})
	}
}

// TestAliasDeclaredAnswersWithoutWriting covers the read-only half of the same
// question. It shares hostAliasDeclared with AddEntryIfAbsent — so what is
// asserted here is the wrapper's own contract, not the pattern's: the three
// answers, and that asking leaves the file exactly as it was (a caller that
// used it to decide a NAME must not create the file it asked about).
func TestAliasDeclaredAnswersWithoutWriting(t *testing.T) {
	t.Run("a missing file has no aliases in it", func(t *testing.T) {
		path := configPath(t)
		declared, err := AliasDeclared(path, "shed-demo")
		if err != nil {
			t.Fatalf("AliasDeclared: %v", err)
		}
		if declared {
			t.Error("a config that does not exist declared something")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("asking created the file: %v", err)
		}
	})

	t.Run("declared and not declared", func(t *testing.T) {
		path := configPath(t)
		content := "Host shed-demo\n    ProxyJump bastion\n"
		writeConfig(t, path, content)

		for alias, want := range map[string]bool{"shed-demo": true, "shed-other": false} {
			declared, err := AliasDeclared(path, alias)
			if err != nil {
				t.Fatalf("AliasDeclared(%q): %v", alias, err)
			}
			if declared != want {
				t.Errorf("AliasDeclared(%q) = %v, want %v", alias, declared, want)
			}
		}
		if got := readConfigFile(t, path); got != content {
			t.Errorf("asking modified the file:\n got %q\nwant %q", got, content)
		}
	})
}

// TestAddingTwiceIsIdempotent: the second call writes nothing and reports why.
func TestAddingTwiceIsIdempotent(t *testing.T) {
	path := configPath(t)

	addOrFail(t, path, newEntry("shed-demo"))
	first := readConfigFile(t, path)

	if outcome := addOrFail(t, path, newEntry("shed-demo")); outcome != AlreadyManaged {
		t.Fatalf("outcome = %v, want AlreadyManaged", outcome)
	}
	if got := readConfigFile(t, path); got != first {
		t.Errorf("the second call rewrote the file:\n got %q\nwant %q", got, first)
	}
}

// TestTheFileModeIsPreserved: this function edits somebody else's file, and
// re-permitting it is a side effect nobody asked for.
func TestTheFileModeIsPreserved(t *testing.T) {
	path := configPath(t)
	writeConfig(t, path, userConfig)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	addOrFail(t, path, newEntry("shed-demo"))

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644", perm)
	}
}

// TestConcurrentAddsAllSurvive drives the lock: a read-modify-write on a file
// two processes can reach means the second reader's write erases the first's
// entry unless the whole cycle is serialised.
//
// Goroutines rather than processes, but flock keys on the OPEN FILE
// DESCRIPTION, not on the process — each call opens the lock file itself, so
// these contend exactly as separate `shed` processes would.
func TestConcurrentAddsAllSurvive(t *testing.T) {
	path := configPath(t)
	writeConfig(t, path, managedConfig)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = AddEntryIfAbsent(path, newEntry(fmt.Sprintf("shed-c%d", i)))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}

	got := readConfigFile(t, path)
	names := map[string]bool{}
	for _, name := range Parse(got).GetEntryNames() {
		names[name] = true
	}
	for _, want := range []string{"shed-alpha", "shed-beta"} {
		if !names[want] {
			t.Errorf("%s was lost:\n%s", want, got)
		}
	}
	for i := 0; i < n; i++ {
		if want := fmt.Sprintf("shed-c%d", i); !names[want] {
			t.Errorf("%s was lost — a concurrent add overwrote it:\n%s", want, got)
		}
	}
}

// TestNoTempFilesAreLeftBehind: the atomic write must not litter ~/.ssh.
func TestNoTempFilesAreLeftBehind(t *testing.T) {
	path := configPath(t)
	addOrFail(t, path, newEntry("shed-demo"))

	names, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".shed-ssh-config-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("temp files left behind: %v", names)
	}
}

// The tests below cover the six defects sol's review of plan 022 C5a found in
// this file. Every one of them is a way to damage a config file the user owns,
// which is why they are worth their own block.

// TestMarkerTextInsideADirectiveIsNotABlockBoundary: the markers are located
// as standalone lines, never as loose substrings.
//
// The discriminator is a decoy EndMarker quoted inside a directive that sits
// BEFORE a real managed block. A substring search finds the decoy first, so
// endIdx lands before beginIdx, the "no usable block" branch is taken, and a
// SECOND managed block is appended — leaving the file with two. Locating the
// marker as a whole line finds the real one and splices into the block that
// already exists.
func TestMarkerTextInsideADirectiveIsNotABlockBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	decoy := "Host decoy\n    RemoteCommand echo \"" + EndMarker + "\"\n\n"
	original := decoy + GenerateManagedBlock([]Entry{newEntry("shed-first")})
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := AddEntryIfAbsent(path, newEntry("shed-second")); err != nil {
		t.Fatalf("AddEntryIfAbsent: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), BeginMarker); n != 1 {
		t.Errorf("managed blocks = %d, want exactly 1 — the decoy was mistaken for a boundary\n%s", n, got)
	}
	if !strings.Contains(string(got), decoy) {
		t.Errorf("the decoy directive was not preserved verbatim:\n%s", got)
	}
	for _, want := range []string{"Host shed-first", "Host shed-second"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// TestEqualsAndQuotedHostSyntaxCountAsDeclared: OpenSSH accepts `Host=name`
// and `Host "name"`. Missing either appends a duplicate alias, and ssh then
// silently uses whichever came first — the user's.
func TestEqualsAndQuotedHostSyntaxCountAsDeclared(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"equals_no_spaces", "Host=shed-dup\n    HostName example.com\n"},
		{"equals_spaced", "Host = shed-dup\n    HostName example.com\n"},
		{"double_quoted", "Host \"shed-dup\"\n    HostName example.com\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			if err := os.WriteFile(path, []byte(tc.line), 0o600); err != nil {
				t.Fatal(err)
			}
			outcome, err := AddEntryIfAbsent(path, Entry{Name: "shed-dup", Host: "h", Port: 22, User: "shed-dup"})
			if err != nil {
				t.Fatalf("AddEntryIfAbsent: %v", err)
			}
			if outcome != AlreadyUserDefined {
				t.Errorf("outcome = %v, want AlreadyUserDefined", outcome)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.line {
				t.Errorf("file was modified despite an existing declaration:\n%s", got)
			}
		})
	}
}

// TestASymlinkedConfigIsWrittenThrough: ~/.ssh/config symlinked into a
// dotfiles repo is common. os.ReadFile follows the link but os.Rename
// REPLACES it, so a naive temp-and-rename reads the repo's copy and then
// swaps the link for a regular file, leaving the repo untouched.
func TestASymlinkedConfigIsWrittenThrough(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles-ssh-config")
	link := filepath.Join(dir, "config")
	if err := os.WriteFile(real, []byte("Host existing\n    HostName e.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := AddEntryIfAbsent(link, Entry{Name: "shed-y", Host: "h", Port: 22, User: "shed-y"}); err != nil {
		t.Fatalf("AddEntryIfAbsent: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a regular file")
	}
	body, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Host shed-y") {
		t.Errorf("the entry did not reach the symlink target:\n%s", body)
	}
	if !strings.Contains(string(body), "Host existing") {
		t.Errorf("the target's existing content was lost:\n%s", body)
	}
}

// TestAReadOnlyConfigIsRefused: temp-file-plus-rename needs only the
// DIRECTORY to be writable, so a mode-0400 config is otherwise silently
// replaced — the exact outcome someone who chmod'd it was preventing.
func TestAReadOnlyConfigIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	original := "Host existing\n    HostName e.com\n"
	if err := os.WriteFile(path, []byte(original), 0o400); err != nil {
		t.Fatal(err)
	}

	if _, err := AddEntryIfAbsent(path, Entry{Name: "shed-z", Host: "h", Port: 22, User: "shed-z"}); err == nil {
		t.Fatal("editing a read-only config must fail")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("a read-only config was modified:\n%s", got)
	}
}

// TestCRLFConfigKeepsItsLineEndings: editing a CRLF config with LF output
// leaves it with mixed endings. Both write paths need covering — appending a
// fresh managed block and splicing into one that already exists compute the
// newline separately, and a fix to one does not fix the other.
func TestCRLFConfigKeepsItsLineEndings(t *testing.T) {
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

	for _, tc := range []struct {
		name     string
		original string
	}{
		{
			name:     "appends_a_fresh_block",
			original: crlf("Host existing\n    HostName e.com\n"),
		},
		{
			name:     "splices_into_an_existing_block",
			original: crlf("Host existing\n    HostName e.com\n\n") + crlf(GenerateManagedBlock([]Entry{newEntry("shed-old")})),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			if err := os.WriteFile(path, []byte(tc.original), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := AddEntryIfAbsent(path, newEntry("shed-crlf")); err != nil {
				t.Fatalf("AddEntryIfAbsent: %v", err)
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Every LF in the result must be half of a CRLF pair.
			if lf, pairs := strings.Count(string(got), "\n"), strings.Count(string(got), "\r\n"); lf != pairs {
				t.Errorf("CRLF file gained %d bare LF endings:\n%q", lf-pairs, got)
			}
			if !strings.Contains(string(got), "Host shed-crlf") {
				t.Errorf("the entry was not added:\n%q", got)
			}
		})
	}
}
