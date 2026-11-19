package sshconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func goodEntry() Entry {
	return Entry{Name: "shed-box", Host: "shed.example.test", Port: 2222, User: "box",
		KnownHostsFile: "/home/pat/.shed/known_hosts"}
}

// refusedEntries are entries whose values could change what a stanza says,
// each with the field the refusal names.
func refusedEntries() []struct {
	name  string
	entry Entry
	field string
} {
	with := func(f func(*Entry)) Entry { e := goodEntry(); f(&e); return e }
	type refused = struct {
		name  string
		entry Entry
		field string
	}
	// Host names that are no literal host or address: shell syntax and ssh's
	// own `%` tokens (a ProxyCommand with `%h` in the person's config would
	// hand the value to a shell), `=` (ssh reads `HostName =x` as `x`), a
	// leading `-`, every shell operator and pattern character, a user part,
	// a path, an IPv6 zone and brackets.
	var hosts []refused
	for _, host := range []string{
		"$(uname).example.test", "%h.example.test", "=other.example.test", "-oProxyCommand=x", "-x.example.test",
		"a(b", "a)b", "a;b", "a|b", "a&b", "a`id`b", "a*b", "a?b", "a!b", "a,b", "pat@b", "a/b",
		"a$b", "a=b", "a%b", "a<b", "a>b", "a{b", "a~b",
		"x.example.test.", ".x.example.test", "_x.example.test", "x.example.test-",
		"fe80::1%eth0", "[::1]", "::ffff:g", "fd00::1/64", "a:b", "h\x00x", "hé.example.test",
	} {
		hosts = append(hosts, refused{fmt.Sprintf("host %q", host), with(func(e *Entry) { e.Host = host }), "HostName"})
	}
	return append(hosts, []refused{
		{"a user with a line break and a ProxyCommand", with(func(e *Entry) {
			e.User = "x\nHost *\n    ProxyCommand sh -c id"
		}), "User"},
		{"an alias with a line break", with(func(e *Entry) { e.Name = "shed-x\nHost *" }), "Host alias"},
		{"a user with a space", with(func(e *Entry) { e.User = "two words" }), "User"},
		{"an alias with a space", with(func(e *Entry) { e.Name = "shed-two words" }), "Host alias"},
		{"a user with a pattern", with(func(e *Entry) { e.User = "star*" }), "User"},
		{"an alias with a pattern", with(func(e *Entry) { e.Name = "shed-*" }), "Host alias"},
		{"an alias with a negation", with(func(e *Entry) { e.Name = "!shed-box" }), "Host alias"},
		{"a user with a token", with(func(e *Entry) { e.User = "pct%h" }), "User"},
		{"an alias with a token", with(func(e *Entry) { e.Name = "shed-%h" }), "Host alias"},
		{"a user with a quote", with(func(e *Entry) { e.User = `quo"te` }), "User"},
		{"a user with a hash", with(func(e *Entry) { e.User = "box#x" }), "User"},
		{"an upper-case user", with(func(e *Entry) { e.User = "Box" }), "User"},
		{"an empty user", with(func(e *Entry) { e.User = "" }), "User"},
		{"an alias starting with -", with(func(e *Entry) { e.Name = "-oProxyCommand=x" }), "Host alias"},
		{"an empty alias", with(func(e *Entry) { e.Name = "" }), "Host alias"},
		{"a host with a line break", with(func(e *Entry) { e.Host = "h\n    ProxyCommand id" }), "HostName"},
		{"a host with a space", with(func(e *Entry) { e.Host = "h x" }), "HostName"},
		{"a host with a hash", with(func(e *Entry) { e.Host = "h#x" }), "HostName"},
		{"a host with a quote", with(func(e *Entry) { e.Host = "h'x" }), "HostName"},
		{"an empty host", with(func(e *Entry) { e.Host = "" }), "HostName"},
		{"port zero", with(func(e *Entry) { e.Port = 0 }), "Port"},
		{"a port too large", with(func(e *Entry) { e.Port = 70000 }), "Port"},
		{"a known_hosts path with a line break", with(func(e *Entry) {
			e.KnownHostsFile = "/home/pat/kh\n    ProxyCommand id"
		}), "UserKnownHostsFile"},
		// ssh expands tokens and variables in this path, and reads it as a
		// list: none of these may be written, quoted or not.
		{"a known_hosts path with a token", with(func(e *Entry) { e.KnownHostsFile = "/home/pat/%h/known_hosts" }),
			"UserKnownHostsFile"},
		{"a known_hosts path with a variable", with(func(e *Entry) { e.KnownHostsFile = "/home/${USER}/known_hosts" }),
			"UserKnownHostsFile"},
		{"a known_hosts path with a quote", with(func(e *Entry) { e.KnownHostsFile = `/home/pat/k"h` }),
			"UserKnownHostsFile"},
		{"a known_hosts path with a backslash", with(func(e *Entry) { e.KnownHostsFile = `/home/pat/k\h` }),
			"UserKnownHostsFile"},
		{"a known_hosts path with a hash", with(func(e *Entry) { e.KnownHostsFile = "/home/pat/k#h" }),
			"UserKnownHostsFile"},
		{"a relative known_hosts path", with(func(e *Entry) { e.KnownHostsFile = ".shed/known_hosts" }),
			"UserKnownHostsFile"},
		{"an identity file with a quote", with(func(e *Entry) { e.IdentityFile = `/k"ey` }), "IdentityFile"},
		{"a relative identity file", with(func(e *Entry) { e.IdentityFile = "id_work" }), "IdentityFile"},
	}...)
}

// TestValidate: an entry of the shape shed writes passes; every value that
// could end a line, start a stanza, or be read as a pattern or a token is
// refused, naming its field, with the value escaped.
func TestValidate(t *testing.T) {
	for _, good := range []Entry{
		goodEntry(),
		{Name: "shed-srv.example_1-box", Host: "10.0.0.1", Port: 22, User: "a1-b", IdentityFile: "/home/pat/.ssh/id work"},
		{Name: "shed-box", Host: "shed.example.test", Port: 65535, User: "box", KnownHostsFile: "/home/pat x/.shed/known_hosts"},
		// Every literal host shape: an IPv4 address, IPv6 literals, a name with
		// `_`, a one-label name, a one-character name, upper case.
		{Name: "shed-box", Host: "127.0.0.1", Port: 22, User: "box"},
		{Name: "shed-box", Host: "::1", Port: 22, User: "box"},
		{Name: "shed-box", Host: "fd00::1", Port: 22, User: "box"},
		{Name: "shed-box", Host: "FD00:0:0:0:0:0:0:1", Port: 22, User: "box"},
		{Name: "shed-box", Host: "host_1.example.test", Port: 22, User: "box"},
		{Name: "shed-box", Host: "localhost", Port: 22, User: "box"},
		{Name: "shed-box", Host: "h", Port: 22, User: "box"},
		{Name: "shed-box", Host: "Shed-1.Example.test", Port: 22, User: "box"},
	} {
		if err := good.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", good, err)
		}
	}
	for _, tc := range refusedEntries() {
		err := tc.entry.Validate()
		var refused *EntryError
		if !errors.As(err, &refused) {
			t.Errorf("%s: Validate = %v, want an *EntryError", tc.name, err)
			continue
		}
		if refused.Field != tc.field {
			t.Errorf("%s: field %q, want %q", tc.name, refused.Field, tc.field)
		}
		if strings.ContainsAny(err.Error(), "\n\r\x00") {
			t.Errorf("%s: the message carries a raw control character: %q", tc.name, err)
		}
		// The refusal names the field and shows the value escaped (quoted).
		if want := tc.field + " " + fmt.Sprintf("%q", refused.Value); !strings.Contains(err.Error(), want) {
			t.Errorf("%s: message %q does not hold %q", tc.name, err, want)
		}
	}
}

// TestWritersRefuseAnEntryValidateRefuses: Write, AddEntryIfAbsent and
// EnsureEntry each refuse such an entry and leave the file byte-identical (or
// absent), so no caller can put one into an ssh config.
func TestWritersRefuseAnEntryValidateRefuses(t *testing.T) {
	original := "Host mine\n    HostName mine.example.test\n\n" + GenerateManagedBlock([]Entry{goodEntry()})
	for _, tc := range refusedEntries() {
		t.Run(tc.name, func(t *testing.T) {
			for _, write := range []struct {
				name string
				fn   func(path string) error
			}{
				{"Write", func(path string) error {
					return Write(path, "Host mine\n    HostName mine.example.test\n", []Entry{goodEntry(), tc.entry}, "")
				}},
				{"AddEntryIfAbsent", func(path string) error { _, err := AddEntryIfAbsent(path, tc.entry); return err }},
				{"EnsureEntry", func(path string) error { _, err := EnsureEntry(path, tc.entry); return err }},
				{"EnsureEntry with an identity file", func(path string) error {
					e := tc.entry
					if e.IdentityFile == "" {
						e.IdentityFile = "/home/pat/.ssh/id_work"
					}
					_, err := EnsureEntry(path, e)
					return err
				}},
			} {
				for _, exists := range []bool{false, true} {
					path := filepath.Join(t.TempDir(), ".ssh", "config")
					if exists {
						if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					err := write.fn(path)
					var refused *EntryError
					if !errors.As(err, &refused) {
						t.Errorf("%s (file exists %v): err = %v, want the refusal", write.name, exists, err)
					}
					got, readErr := os.ReadFile(path)
					switch {
					case exists && string(got) != original:
						t.Errorf("%s: a refused entry changed the file:\n%s", write.name, got)
					case !exists && !os.IsNotExist(readErr):
						t.Errorf("%s: a refused entry created the file (err=%v):\n%s", write.name, readErr, got)
					}
				}
			}
		})
	}
}

// TestKnownHostsPathIsOneQuotedValue: UserKnownHostsFile takes a list of file
// names, so the path is written inside double quotes: a home directory with a
// space stays one file.
func TestKnownHostsPathIsOneQuotedValue(t *testing.T) {
	e := goodEntry()
	e.KnownHostsFile = "/home/pat x/.shed/known_hosts"
	if err := e.Validate(); err != nil {
		t.Fatalf("a path with a space was refused: %v", err)
	}
	want := "    UserKnownHostsFile \"/home/pat x/.shed/known_hosts\"\n"
	if got := GenerateEntry(e); !strings.Contains(got, want) {
		t.Fatalf("stanza =\n%s\nwant the line %q", got, want)
	}
}
