package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/sshconfig"
	"github.com/charliek/shed/sdk"
)

// hostileAliases are names a hand-edited config.yaml or ~/.ssh/config can put
// in front of `shed ssh-config`: a line break, an ESC sequence, a shell
// metacharacter, a leading `-` (ssh would read it as an option), an `@` (ssh
// would read a user and another host) and a space (no host name at all).
var hostileAliases = []string{"shed-a\nb", "shed-x\x1b[2J", "shed-a;id", "-oProxyCommand=x", "shed-pat@other-host", "shed-a b"}

// noAliasCommand is the tail of the hint for an alias no command line is
// shown for.
const noAliasCommand = "(its name is not one ssh reads as a plain host alias, so no command line is shown)"

// TestSSHAliasHint: the `ssh <alias>` line install prints is a command only
// for an alias of the shape shed generates (letters, digits, `.`, `_`, `-`,
// not starting with `-`); every other alias is named escaped, with no command
// line, whether or not the shell could quote it: ssh reads the destination
// again, so `ssh 'shed-pat@other-host'` would reach another host.
func TestSSHAliasHint(t *testing.T) {
	cases := []struct{ alias, want string }{
		{"shed-box", "ssh shed-box"},
		{"shed-srv.example_1-box", "ssh shed-srv.example_1-box"},
		{"shed-a;id", `the Host alias "shed-a;id" ` + noAliasCommand},
		{"shed-pat@other-host", `the Host alias "shed-pat@other-host" ` + noAliasCommand},
		{"shed-a b", `the Host alias "shed-a b" ` + noAliasCommand},
		{"shed:22", `the Host alias "shed:22" ` + noAliasCommand},
		{"shed-a\nb", `the Host alias "shed-a\nb" ` + noAliasCommand},
		{"shed-x\x1b[2J", `the Host alias "shed-x\x1b[2J" ` + noAliasCommand},
		{"-oProxyCommand=x", `the Host alias "-oProxyCommand=x" ` + noAliasCommand},
		{"shed-\u00e9", `the Host alias "shed-é" ` + noAliasCommand},
		{"shed-\u202ebox", `the Host alias "shed-\u202ebox" ` + noAliasCommand},
		{"", `the Host alias "" ` + noAliasCommand},
	}
	for _, tc := range cases {
		if got := sshAliasHint(tc.alias); got != tc.want {
			t.Errorf("sshAliasHint(%q) = %q, want %q", tc.alias, got, tc.want)
		}
	}
}

// TestSSHConfigInstallEscapesListedNames: every name `ssh-config --install`
// and `--uninstall` list (entries to add, to remove, to update, unchanged,
// and the `ssh <alias>` hint) is shown escaped. No line break or ESC from a
// name reaches the terminal raw, and no hint is a command a person could paste
// as something else. A hostile alias is listed by a dry run, but the writer
// refuses to put it into the file (sshconfig.Entry.Validate): the install
// fails and the file is unchanged.
func TestSSHConfigInstallEscapesListedNames(t *testing.T) {
	f := newInstallFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		t.Fatal(err)
	}
	// The block on disk (written by hand, or by an older shed) holds one plain
	// alias, which stays; one hostile alias whose stanza lacks the identity
	// lines its entry now has, listed as updated; and two hostile ones, which
	// go.
	keep := f.stanza("shed-keep", "keep")
	const changed = "shed-chg\x1b[1m"
	content := sshconfig.GenerateManagedBlock([]sshconfig.Entry{
		keep, f.stanza(changed, "box"), f.stanza("shed-old\x1b[31m", "old"), f.stanza("-oOld=x", "old"),
	})
	if err := os.WriteFile(f.path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	withKey := f.stanza(changed, "box")
	withKey.IdentityFile = filepath.Join(t.TempDir(), "id_work")
	entries := []sshconfig.Entry{keep, withKey}
	for _, name := range hostileAliases {
		entries = append(entries, f.stanza(name, "box"))
	}

	origJSON, origDry, origVerbose := jsonFlag, sshConfigDryRun, verboseLevel
	t.Cleanup(func() { jsonFlag, sshConfigDryRun, verboseLevel = origJSON, origDry, origVerbose })
	jsonFlag, sshConfigDryRun, verboseLevel = false, true, 1

	// A dry run lists every name.
	var runErr error
	out := captureStdout(t, func() { runErr = runSSHConfigInstall(entries, nil) })
	if runErr != nil {
		t.Fatalf("install --dry-run: %v", runErr)
	}
	// The install itself is refused, and writes nothing.
	sshConfigDryRun = false
	out += captureStdout(t, func() { runErr = runSSHConfigInstall(entries, nil) })
	var refused *sshconfig.EntryError
	if !errors.As(runErr, &refused) {
		t.Fatalf("install of hostile aliases: err = %v, want the writer's refusal", runErr)
	}
	if strings.ContainsRune(runErr.Error(), '\x1b') || strings.ContainsRune(runErr.Error(), '\n') {
		t.Errorf("the refusal carries a raw control character: %q", runErr)
	}
	if got := string(must(os.ReadFile(f.path))); got != content {
		t.Errorf("a refused install changed the file:\n%s", got)
	}
	sshConfigDryRun = true
	out += captureStdout(t, func() { runErr = runSSHConfigUninstall() })
	if runErr != nil {
		t.Fatalf("uninstall: %v", runErr)
	}
	// An install of the plain alias alone is written, and its hint is a
	// command.
	sshConfigDryRun = false
	out += captureStdout(t, func() { runErr = runSSHConfigInstall([]sshconfig.Entry{keep}, nil) })
	if runErr != nil {
		t.Fatalf("install: %v", runErr)
	}

	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("the output carries a raw ESC: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		// A raw line break in a name would leave its tail, "b", as a line.
		if strings.TrimSpace(line) == "b" {
			t.Errorf("a name's line break reached the output raw: %q", out)
		}
	}
	for _, want := range []string{
		`  + shed-a\nb`, `  + shed-x\x1b[2J`, `  + shed-a;id`, `  + -oProxyCommand=x`, `  + shed-pat@other-host`, // Entries to add
		`  - shed-old\x1b[31m`, `  - -oOld=x`, // Entries to remove (install, then uninstall)
		`  ~ shed-chg\x1b[1m`, // Entries to update
		`  = shed-keep`,       // Entries unchanged
		"  ssh shed-keep",     // the one hint that is a command
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"ssh shed-a", "ssh 'shed", "ssh -oProxyCommand=x", "ssh shed-x", "ssh shed-pat", "ssh shed-chg"} {
		if strings.Contains(out, bad) {
			t.Errorf("the output holds the pasteable command %q:\n%s", bad, out)
		}
	}
}

// TestTunnelPortFlagHint: the -t advice for a shed with no tunnel config names
// the shed as config.ShellWord gives it, bare or single-quoted, and a shed
// name no pasted word can carry in words, escaped, with no command line.
func TestTunnelPortFlagHint(t *testing.T) {
	cases := []struct{ shed, want string }{
		{"box", "Or use -t flag: shed tunnels start box -t 3000"},
		{"a;id", "Or use -t flag: shed tunnels start 'a;id' -t 3000"},
		{"x$(id)|sh", "Or use -t flag: shed tunnels start 'x$(id)|sh' -t 3000"},
		{"box\x1b[2J", `Or use the -t flag of shed tunnels start for the shed "box\x1b[2J" ` +
			"(its name cannot be pasted safely as one word, so no command line is shown)"},
		{"-x", `Or use the -t flag of shed tunnels start for the shed "-x" ` +
			"(its name cannot be pasted safely as one word, so no command line is shown)"},
	}
	for _, tc := range cases {
		if got := tunnelPortFlagHint("start", tc.shed); got != tc.want {
			t.Errorf("tunnelPortFlagHint(%q) = %q, want %q", tc.shed, got, tc.want)
		}
	}
}

// TestSSHConfigWarningEscapesTheEntryName: with -v, `shed ssh-config` warns
// about a server it could not reach, naming its entry; the name comes from
// config.yaml, so it is shown escaped, never raw.
func TestSSHConfigWarningEscapesTheEntryName(t *testing.T) {
	testClientConfig(t)
	stubBootstrapIdentity(sdk.Credential{}, errors.New("test: the bootstrap must not run"))
	// Nothing listens on port 1: the server cannot be reached.
	putServerEntry(t, "srv\x1b[2J", config.ServerEntry{Host: "127.0.0.1", APIURL: "http://127.0.0.1:1"})

	origVerbose := verboseLevel
	t.Cleanup(func() { verboseLevel = origVerbose })
	verboseLevel = 1

	stderr := captureStderr(t, func() { _, _ = getAllShedsInfo() })
	if strings.ContainsRune(stderr, '\x1b') {
		t.Errorf("the warning carries a raw ESC: %q", stderr)
	}
	if !strings.Contains(stderr, `Warning: could not reach srv\x1b[2J: `) {
		t.Errorf("stderr = %q, want the warning with the name escaped", stderr)
	}
}
