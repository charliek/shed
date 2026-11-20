package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// identityLinesFor is the four lines a stanza with identity file path carries.
func identityLinesFor(path string) string {
	return "    IdentityFile \"" + path + "\"\n" +
		"    IdentitiesOnly yes\n" +
		"    ControlMaster no\n" +
		"    ControlPath none\n"
}

// TestGenerateEntryIdentityFile pins a stanza without an identity file (byte
// for byte what it was before the field existed) and with one, which gains
// `IdentityFile "<path>"`, `IdentitiesOnly yes`, `ControlMaster no` and
// `ControlPath none`.
func TestGenerateEntryIdentityFile(t *testing.T) {
	entry := Entry{Name: "shed-box", Host: "shed.example.test", Port: 2222, User: "box", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	without := "Host shed-box\n" +
		"    HostName shed.example.test\n" +
		"    Port 2222\n" +
		"    User box\n" +
		"    UserKnownHostsFile \"/home/pat/.shed/known_hosts\"\n"
	if got := GenerateEntry(entry); got != without {
		t.Errorf("stanza without an identity file =\n%s\nwant\n%s", got, without)
	}

	entry.IdentityFile = "/home/pat/.ssh/id work"
	with := without + identityLinesFor("/home/pat/.ssh/id work")
	if got := GenerateEntry(entry); got != with {
		t.Errorf("stanza with an identity file =\n%s\nwant\n%s", got, with)
	}
}

// TestComputeDiffChangedOnlyForIdentityLines: without an identity file an
// entry is compared by name only, exactly as before the field existed, so a
// stanza edited by hand (a ProxyJump, an identity of its own) is never
// rewritten for that; with one, a stanza is rewritten when, and only when,
// its identity lines are not exactly the entry's.
func TestComputeDiffChangedOnlyForIdentityLines(t *testing.T) {
	plain := Entry{Name: "shed-box", Host: "shed.example.test", Port: 2222, User: "box", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	keyed := plain
	keyed.IdentityFile = "/home/pat/.ssh/id_work"
	edited := func(stanza, extra string) []ManagedEntry {
		return []ManagedEntry{{Name: "shed-box", RawContent: strings.TrimSpace(stanza + extra)}}
	}

	for _, tc := range []struct {
		name    string
		current []ManagedEntry
		desired Entry
		changed bool
	}{
		{"no identity: the same stanza", Parse(GenerateManagedBlock([]Entry{plain})).ManagedEntries, plain, false},
		{"no identity: a hand-added ProxyJump is kept", edited(GenerateEntry(plain), "    ProxyJump bastion\n"), plain, false},
		{"no identity: hand-added identity lines are kept", edited(GenerateEntry(plain), identityLinesFor("/home/pat/.ssh/other")), plain, false},
		{"no identity: a stanza written with an identity file is kept", Parse(GenerateManagedBlock([]Entry{keyed})).ManagedEntries, plain, false},
		{"no identity: CRLF is kept", edited(strings.ReplaceAll(GenerateEntry(plain), "\n", "\r\n"), ""), plain, false},
		{"identity: the same stanza", Parse(GenerateManagedBlock([]Entry{keyed})).ManagedEntries, keyed, false},
		{"identity: CRLF with the same lines", edited(strings.ReplaceAll(GenerateEntry(keyed), "\n", "\r\n"), ""), keyed, false},
		{"identity: another line edited by hand is kept", edited(GenerateEntry(keyed), "    ProxyJump bastion\n"), keyed, false},
		{"identity: a stanza with no identity lines", Parse(GenerateManagedBlock([]Entry{plain})).ManagedEntries, keyed, true},
		{"identity: another identity file", edited(GenerateEntry(plain), identityLinesFor("/home/pat/.ssh/other")), keyed, true},
		{"identity: an older stanza with only two of the lines", edited(GenerateEntry(plain),
			"    IdentityFile \"/home/pat/.ssh/id_work\"\n    IdentitiesOnly yes\n"), keyed, true},
		{"identity: an extra IdentityFile added by hand", edited(GenerateEntry(keyed), "    IdentityFile ~/.ssh/id_other\n"), keyed, true},
		{"identity: a ControlPath added by hand", edited(GenerateEntry(plain),
			"    IdentityFile \"/home/pat/.ssh/id_work\"\n    IdentitiesOnly yes\n    ControlMaster auto\n    ControlPath none\n"), keyed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diff := ComputeDiff(tc.current, []Entry{tc.desired})
			if tc.changed {
				if !diff.HasChanges() || !slices.Equal(diff.Changed, []string{"shed-box"}) || len(diff.Unchanged) != 0 {
					t.Fatalf("diff = %+v, want shed-box changed", diff)
				}
				return
			}
			if diff.HasChanges() || !slices.Equal(diff.Unchanged, []string{"shed-box"}) {
				t.Fatalf("diff = %+v, want shed-box unchanged", diff)
			}
		})
	}
}

// TestAddEntryIfAbsentWritesTheIdentityLines: the roost alias carries the
// identity file like every other generated stanza.
func TestAddEntryIfAbsentWritesTheIdentityLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	entry := Entry{Name: "shed-srv-box", Host: "shed.example.test", Port: 2222, User: "box",
		KnownHostsFile: "/home/pat/.shed/known_hosts", IdentityFile: "/home/pat/.ssh/cl\xc3\xa9_travail"}
	if _, err := AddEntryIfAbsent(path, entry); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := Parse(string(data)).FindEntry("shed-srv-box")
	if got == nil {
		t.Fatalf("no entry written:\n%s", data)
	}
	want := strings.TrimSpace("Host shed-srv-box\n" +
		"    HostName shed.example.test\n" +
		"    Port 2222\n" +
		"    User box\n" +
		"    UserKnownHostsFile \"/home/pat/.shed/known_hosts\"\n" +
		identityLinesFor("/home/pat/.ssh/cl\xc3\xa9_travail"))
	if got.RawContent != want {
		t.Errorf("entry =\n%s\nwant\n%s", got.RawContent, want)
	}
}

// TestEnsureEntry: for an entry with an identity file, an alias shed wrote
// (inside its managed block) is rewritten when its stanza is not the entry's,
// in place, every other byte kept; one shed did not write (outside the block)
// refuses, naming the alias and the identity file, with the file untouched;
// without an identity file it is AddEntryIfAbsent, leaving both alone.
func TestEnsureEntry(t *testing.T) {
	plain := Entry{Name: "shed-srv-box", Host: "shed.example.test", Port: 2222, User: "box", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	keyed := plain
	keyed.IdentityFile = "/home/pat/.ssh/id_work"
	other := Entry{Name: "shed-other", Host: "shed.example.test", Port: 2222, User: "other", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	userStanza := "Host shed-srv-box\n    HostName shed.example.test\n    ProxyJump bastion\n"
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	read := func(t *testing.T, path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	head := "Host *\n    ServerAliveInterval 30\n\n"

	t.Run("a managed alias with no identity lines is rewritten in place", func(t *testing.T) {
		before := head + GenerateManagedBlock([]Entry{other, plain}) + "\nHost tail\n    HostName tail.example.test\n"
		path := write(t, before)
		outcome, err := EnsureEntry(path, keyed)
		if err != nil || outcome != RewroteEntry {
			t.Fatalf("EnsureEntry = %v, %v; want rewritten", outcome, err)
		}
		after := read(t, path)
		if got := Parse(after).FindEntry(keyed.Name); got == nil || StanzaDiffers(got.RawContent, keyed) {
			t.Fatalf("the alias is not the entry's stanza:\n%s", after)
		}
		// Every other byte is kept: putting the old stanza back gives the
		// file as it was.
		restored := strings.Replace(after, strings.TrimSpace(GenerateEntry(keyed)), strings.TrimSpace(GenerateEntry(plain)), 1)
		if restored != before {
			t.Errorf("bytes outside the stanza changed:\nbefore\n%s\nafter\n%s", before, after)
		}
		// A second run finds it right and writes nothing.
		if outcome, err := EnsureEntry(path, keyed); err != nil || outcome != AlreadyManaged {
			t.Errorf("second EnsureEntry = %v, %v; want already managed", outcome, err)
		}
	})

	t.Run("a managed alias with another key is rewritten", func(t *testing.T) {
		stale := keyed
		stale.IdentityFile = "/home/pat/.ssh/id_old"
		path := write(t, GenerateManagedBlock([]Entry{stale}))
		if outcome, err := EnsureEntry(path, keyed); err != nil || outcome != RewroteEntry {
			t.Fatalf("EnsureEntry = %v, %v; want rewritten", outcome, err)
		}
		if after := read(t, path); strings.Contains(after, "id_old") || !strings.Contains(after, identityLinesFor(keyed.IdentityFile)) {
			t.Errorf("the alias still names the old key:\n%s", after)
		}
	})

	t.Run("an alias shed did not write is refused, the file untouched", func(t *testing.T) {
		before := userStanza + "\n" + GenerateManagedBlock([]Entry{other})
		path := write(t, before)
		_, err := EnsureEntry(path, keyed)
		var unmanaged *UnmanagedAliasError
		if !errors.As(err, &unmanaged) {
			t.Fatalf("err = %v, want *UnmanagedAliasError", err)
		}
		for _, want := range []string{"Host shed-srv-box", path, `"/home/pat/.ssh/id_work"`, "not written by shed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message lacks %q: %v", want, err)
			}
		}
		if after := read(t, path); after != before {
			t.Errorf("the file changed:\n%s", after)
		}
	})

	t.Run("without an identity file nothing existing is touched", func(t *testing.T) {
		for _, before := range []string{
			userStanza,
			GenerateManagedBlock([]Entry{keyed}),
			GenerateManagedBlock([]Entry{{Name: plain.Name, Host: "elsewhere.example.test", Port: 22, User: "box"}}),
		} {
			path := write(t, before)
			if _, err := EnsureEntry(path, plain); err != nil {
				t.Fatalf("EnsureEntry: %v", err)
			}
			if after := read(t, path); after != before {
				t.Errorf("the file changed without an identity file:\nbefore\n%s\nafter\n%s", before, after)
			}
		}
	})

	t.Run("an absent alias is added", func(t *testing.T) {
		path := write(t, head)
		if outcome, err := EnsureEntry(path, keyed); err != nil || outcome != AddedEntry {
			t.Fatalf("EnsureEntry = %v, %v; want added", outcome, err)
		}
		if got := Parse(read(t, path)).FindEntry(keyed.Name); got == nil || StanzaDiffers(got.RawContent, keyed) {
			t.Errorf("the added alias is not the entry's stanza")
		}
	})
}

// TestEnsureEntryOwnsOnlyWhatShedWrote: for an entry with an identity file,
// the ownership check does not depend on finding a managed copy, and shed
// rewrites a managed stanza only when it is entirely shed's (a Host line that
// is exactly `Host <alias>`, then only blank lines and lines whose keyword the
// generator writes, ending at an unindented Host line or the end marker).
// Each refusal leaves the file byte-identical, the hand-written line included.
// The negative control is the last case: the same block with a stanza that is
// entirely shed's is rewritten, so the file would have changed had the
// refusals not held.
func TestEnsureEntryOwnsOnlyWhatShedWrote(t *testing.T) {
	plain := Entry{Name: "shed-box", Host: "shed.example.test", Port: 2222, User: "box", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	keyed := plain
	keyed.IdentityFile = "/home/pat/.ssh/id_work"
	other := Entry{Name: "shed-other", Host: "shed.example.test", Port: 2222, User: "other", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	managed := GenerateManagedBlock([]Entry{other, plain})
	stale := strings.TrimSpace(GenerateEntry(plain))

	for _, tc := range []struct {
		name, content string
		inBlock       bool
	}{
		{"a hand-written alias before the block, with a managed copy too",
			"Host shed-box\n    HostName shed.example.test\n    ProxyJump bastion\n\n" + managed, false},
		{"a hand-written alias after the block, with a managed copy too",
			managed + "\nhost=shed-box\n    ProxyJump bastion\n", false},
		{"a Host line in the block naming other hosts too",
			strings.Replace(managed, "Host shed-box\n", "Host shed-box shed-extra\n", 1), true},
		{"a Match line after the alias's stanza in the block",
			strings.Replace(managed, stale, stale+"\nMatch exec \"true\"\n    IdentityFile /home/pat/.ssh/id_other", 1), true},
		{"an indented match line after the alias's stanza in the block",
			strings.Replace(managed, stale, stale+"\n  match all\n    IdentityFile /home/pat/.ssh/id_other", 1), true},
		{"two stanzas for the alias in the block",
			strings.Replace(managed, stale, stale+"\n\n"+stale, 1), true},
		{"an Include after the alias's lines (the review's case)",
			strings.Replace(managed, stale, stale+"\n    Include ~/.ssh/extra.conf", 1), true},
		{"a comment in the alias's stanza",
			strings.Replace(managed, stale, stale+"\n    # mine", 1), true},
		{"a ProxyJump added by hand",
			strings.Replace(managed, stale, stale+"\n    ProxyJump bastion", 1), true},
		{"an indented host line after the alias's stanza",
			strings.Replace(managed, stale, stale+"\n  host shed-extra\n    HostName extra.example.test", 1), true},
		{"a Host line with a trailing comment",
			strings.Replace(managed, "Host shed-box\n", "Host shed-box # mine\n", 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := EnsureEntry(path, keyed)
			var refused *UnmanagedAliasError
			if !errors.As(err, &refused) || refused.InManagedBlock != tc.inBlock || refused.Alias != keyed.Name {
				t.Fatalf("err = %#v, want an alias refusal (in the block: %v)", err, tc.inBlock)
			}
			if !strings.Contains(err.Error(), "Host shed-box in "+path) || !strings.Contains(err.Error(), "nothing was written") {
				t.Errorf("message = %v", err)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != tc.content {
				t.Errorf("the file changed after a refusal:\nbefore\n%s\nafter\n%s", tc.content, after)
			}
		})
	}

	t.Run("negative control: a stanza of shed's shape is rewritten", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(path, []byte(managed), 0o600); err != nil {
			t.Fatal(err)
		}
		if outcome, err := EnsureEntry(path, keyed); err != nil || outcome != RewroteEntry {
			t.Fatalf("EnsureEntry = %v, %v; want rewritten", outcome, err)
		}
	})
}

// TestUnmanagedAliasErrorEscapes: the alias and the path in the refusal are
// shown with control characters escaped, never raw.
func TestUnmanagedAliasErrorEscapes(t *testing.T) {
	for _, inBlock := range []bool{false, true} {
		err := &UnmanagedAliasError{Path: "/home/pat/.ssh/\x1b[31mconfig", Alias: "shed-\x1b]0;x\x07box",
			IdentityFile: "/home/pat/.ssh/id_\x1b[0mwork", InManagedBlock: inBlock}
		if msg := err.Error(); strings.ContainsAny(msg, "\x1b\x07") {
			t.Errorf("the message carries a raw control byte: %q", msg)
		} else if !strings.Contains(msg, `\x1b[31mconfig`) {
			t.Errorf("the path is not shown escaped: %q", msg)
		}
	}
}

// quotedMarkerStanza is a hand-written stanza whose quoted value holds the
// BEGIN marker's text: a line that is not the marker.
const quotedMarkerStanza = "Host local\n    RemoteCommand \"echo " + BeginMarker + "\"\n\n"

// TestParseReadsOnlyStandaloneMarkerLines (the third review's install
// corruption): marker text inside a quoted value before the block is no
// boundary; the line stays in BeforeBlock, whole, and the block is the real
// one. A marker line ending in CR is still a marker line.
func TestParseReadsOnlyStandaloneMarkerLines(t *testing.T) {
	a := Entry{Name: "shed-a", Host: "shed.example.test", Port: 2222, User: "a", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	for _, tc := range []struct{ name, content string }{
		{"a quoted marker before the block", quotedMarkerStanza + GenerateManagedBlock([]Entry{a})},
		{"a marker in a comment before the block", "# " + EndMarker + "\n" + GenerateManagedBlock([]Entry{a})},
		{"CRLF marker lines", strings.ReplaceAll(GenerateManagedBlock([]Entry{a}), "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed := Parse(tc.content)
			if !parsed.HasManagedBlock || len(parsed.ManagedEntries) != 1 || parsed.ManagedEntries[0].Name != "shed-a" {
				t.Fatalf("parsed = %+v, want the one real block", parsed)
			}
			if !strings.HasPrefix(tc.content, parsed.BeforeBlock) || strings.Contains(parsed.BeforeBlock, BeginMarker+"\n") {
				t.Errorf("BeforeBlock = %q", parsed.BeforeBlock)
			}
			if strings.HasPrefix(tc.content, "Host local") && parsed.BeforeBlock != strings.TrimSuffix(quotedMarkerStanza, "\n") {
				t.Errorf("BeforeBlock = %q, want the RemoteCommand line whole", parsed.BeforeBlock)
			}
		})
	}
}

// markerCases are files whose marker lines frame no single block: both
// writers refuse each, the file byte-identical.
func markerCases() []struct{ name, content string } {
	a := Entry{Name: "shed-a", Host: "shed.example.test", Port: 2222, User: "a", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	block := GenerateManagedBlock([]Entry{a})
	return []struct{ name, content string }{
		{"two complete blocks", block + "\n" + block},
		{"two BEGIN lines", strings.Replace(block, BeginMarker+"\n", BeginMarker+"\n"+BeginMarker+"\n", 1)},
		{"two END lines", block + EndMarker + "\n"},
		{"END before BEGIN", EndMarker + "\n" + strings.Replace(block, EndMarker+"\n", "", 1)},
		{"a BEGIN with no END", strings.Replace(block, EndMarker+"\n", "", 1)},
		{"an END with no BEGIN", strings.Replace(block, BeginMarker+"\n", "", 1)},
		{"both marker lines with trailing spaces", strings.NewReplacer(BeginMarker+"\n", BeginMarker+"  \n", EndMarker+"\n", EndMarker+"  \n").Replace(block)},
		{"both marker lines indented", strings.NewReplacer(BeginMarker+"\n", "\t"+BeginMarker+"\n", EndMarker+"\n", "\t"+EndMarker+"\n").Replace(block)},
	}
}

// TestMarkerLinesFramingNoSingleBlockAreRefused: AddEntryIfAbsent (attach
// without an identity file) and EnsureEntry (with one) refuse a file whose
// markers frame no single block, naming the file, and leave it byte-identical.
// The negative control: one well-formed block, which both write to.
func TestMarkerLinesFramingNoSingleBlockAreRefused(t *testing.T) {
	plain := Entry{Name: "shed-new", Host: "shed.example.test", Port: 2222, User: "new", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	keyed := plain
	keyed.IdentityFile = "/home/pat/.ssh/id_work"
	writers := map[string]func(string, Entry) (AddOutcome, error){
		"AddEntryIfAbsent": func(p string, _ Entry) (AddOutcome, error) { return AddEntryIfAbsent(p, plain) },
		"EnsureEntry":      func(p string, _ Entry) (AddOutcome, error) { return EnsureEntry(p, keyed) },
	}
	for writer, write := range writers {
		for _, tc := range markerCases() {
			t.Run(writer+": "+tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config")
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := write(path, keyed)
				var me *MarkerError
				if !errors.As(err, &me) || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "nothing was written") {
					t.Fatalf("err = %v, want the marker refusal naming %s", err, path)
				}
				if got, _ := os.ReadFile(path); string(got) != tc.content {
					t.Errorf("the file changed after a refusal:\n%s", got)
				}
			})
		}
		t.Run(writer+": negative control: one block", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, []byte(GenerateManagedBlock([]Entry{{Name: "shed-a", Host: "h", Port: 22, User: "a"}})), 0o600); err != nil {
				t.Fatal(err)
			}
			if outcome, err := write(path, keyed); err != nil || outcome != AddedEntry {
				t.Fatalf("write = %v, %v; want added", outcome, err)
			}
		})
	}
}

// TestEnsureEntryKeepsAQuotedMarkerLine: a stale managed alias after a
// hand-written stanza whose quoted value holds the marker's text is rewritten
// in the real block, and the quoted line is kept whole.
func TestEnsureEntryKeepsAQuotedMarkerLine(t *testing.T) {
	keyed := Entry{Name: "shed-box", Host: "shed.example.test", Port: 2222, User: "box",
		KnownHostsFile: "/home/pat/.shed/known_hosts", IdentityFile: "/home/pat/.ssh/id_work"}
	stale := keyed
	stale.IdentityFile = ""
	before := quotedMarkerStanza + GenerateManagedBlock([]Entry{stale})
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if outcome, err := EnsureEntry(path, keyed); err != nil || outcome != RewroteEntry {
		t.Fatalf("EnsureEntry = %v, %v; want rewritten", outcome, err)
	}
	after := readFile(t, path)
	want := strings.Replace(before, strings.TrimSpace(GenerateEntry(stale)), strings.TrimSpace(GenerateEntry(keyed)), 1)
	if after != want {
		t.Errorf("after =\n%s\nwant\n%s", after, want)
	}
}

// TestComputeDiffHeldTwice (the third review's duplicate-alias case): an
// entry with an identity file whose alias the block holds twice is changed,
// whichever copy carries its identity lines, so the whole block is written
// again with one stanza for it; without an identity file a name held twice is
// compared by name only, as before.
func TestComputeDiffHeldTwice(t *testing.T) {
	plain := Entry{Name: "shed-box", Host: "shed.example.test", Port: 2222, User: "box", KnownHostsFile: "/home/pat/.shed/known_hosts"}
	keyed := plain
	keyed.IdentityFile = "/home/pat/.ssh/id_work"
	old := keyed
	old.IdentityFile = "/home/pat/.ssh/id_old"
	twice := append(Parse(GenerateManagedBlock([]Entry{old})).ManagedEntries, Parse(GenerateManagedBlock([]Entry{keyed})).ManagedEntries...)
	if diff := ComputeDiff(twice, []Entry{keyed}); !slices.Equal(diff.Changed, []string{"shed-box"}) || !diff.HasChanges() {
		t.Errorf("identity, held twice: diff = %+v, want shed-box changed", diff)
	}
	plainTwice := append(Parse(GenerateManagedBlock([]Entry{plain})).ManagedEntries, Parse(GenerateManagedBlock([]Entry{plain})).ManagedEntries...)
	if diff := ComputeDiff(plainTwice, []Entry{plain}); diff.HasChanges() {
		t.Errorf("no identity, held twice: diff = %+v, want no change (name only)", diff)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
