package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/sshconfig"
	"github.com/charliek/shed/sdk"
)

// hostileShedNames are names a malicious or compromised server can put in its
// shed list: a line break that starts a `Host *` stanza with a ProxyCommand,
// a space, a pattern character, a token character and a quote. None is a
// shed name, and none may reach ~/.ssh/config.
var hostileShedNames = []string{
	"x\nHost *\n    ProxyCommand sh -c 'touch /tmp/pwned'",
	"two words",
	"star*",
	"pct%h",
	`quo"te`,
}

// namedShedsServer answers GET /api/sheds with names, as an open server.
func namedShedsServer(t *testing.T, names []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sheds" {
			http.NotFound(w, r)
			return
		}
		resp := config.ShedsResponse{}
		for _, n := range names {
			resp.Sheds = append(resp.Sheds, config.Shed{Name: n, Status: config.StatusRunning})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSSHConfigInstallSkipsShedNamesThatAreNotShedNames: a server whose shed
// list holds names that are not shed names (a line break with a ProxyCommand,
// a space, `*`, `%`, a quote) gets none of them into ~/.ssh/config through
// `shed ssh-config --install`: each such shed is skipped with exactly one
// warning line on standard error that shows its name escaped, and the
// well-formed sheds in the same answer are still written.
func TestSSHConfigInstallSkipsShedNamesThatAreNotShedNames(t *testing.T) {
	testClientConfig(t)
	stubBootstrapIdentity(sdk.Credential{}, errors.New("test: the bootstrap must not run"))
	names := append([]string{"good"}, hostileShedNames...)
	names = append(names, "also-good")
	srv := namedShedsServer(t, names)
	putServerEntry(t, "srv", config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, APIURL: srv.URL})

	origJSON, origInstall, origDry, origAll := jsonFlag, sshConfigInstall, sshConfigDryRun, sshConfigAll
	t.Cleanup(func() {
		jsonFlag, sshConfigInstall, sshConfigDryRun, sshConfigAll = origJSON, origInstall, origDry, origAll
	})
	jsonFlag, sshConfigInstall, sshConfigDryRun, sshConfigAll = false, true, false, false

	var runErr error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { runErr = runSSHConfig(sshConfigCmd, nil) })
	})
	if runErr != nil {
		t.Fatalf("ssh-config --install: %v\nstdout: %s\nstderr: %s", runErr, stdout, stderr)
	}

	written := string(must(os.ReadFile(sshconfig.GetSSHConfigPath())))
	for _, want := range []string{"Host shed-good\n", "    User good\n", "Host shed-also-good\n", "    User also-good\n"} {
		if !strings.Contains(written, want) {
			t.Errorf("the well-formed sheds were not written (%q):\n%s", want, written)
		}
	}
	for _, bad := range []string{"ProxyCommand", "Host *", "pwned", "two", "words", "star", "pct", "quo", `"te`} {
		if strings.Contains(written, bad) {
			t.Errorf("a hostile shed name reached ~/.ssh/config (%q):\n%s", bad, written)
		}
	}
	if got := len(sshconfig.Parse(written).ManagedEntries); got != 2 {
		t.Errorf("the managed block holds %d stanzas, want 2:\n%s", got, written)
	}

	if got := strings.Count(stderr, "warning: skipped shed "); got != len(hostileShedNames) {
		t.Errorf("%d skip warnings, want one per hostile shed (%d):\n%s", got, len(hostileShedNames), stderr)
	}
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Host ") || strings.HasPrefix(strings.TrimSpace(line), "ProxyCommand") {
			t.Errorf("a line break in a shed name reached standard error raw:\n%s", stderr)
		}
	}
	if !strings.Contains(stderr, `warning: skipped shed x\nHost *\n    ProxyCommand`) {
		t.Errorf("the warning does not show the name escaped:\n%s", stderr)
	}
	for _, alias := range []string{"ssh shed-good", "ssh shed-also-good"} {
		if !strings.Contains(stdout, alias) {
			t.Errorf("stdout lacks the hint %q:\n%s", alias, stdout)
		}
	}
}

// TestSSHConfigPrintSkipsShedNamesThatAreNotShedNames: the printed config
// (`shed ssh-config`, no --install) and its JSON hold no hostile shed either,
// and a server answering only hostile names fails the command with nothing
// printed or written.
func TestSSHConfigPrintSkipsShedNamesThatAreNotShedNames(t *testing.T) {
	testClientConfig(t)
	stubBootstrapIdentity(sdk.Credential{}, errors.New("test: the bootstrap must not run"))
	srv := namedShedsServer(t, append([]string{"good"}, hostileShedNames...))
	putServerEntry(t, "srv", config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, APIURL: srv.URL})

	origJSON, origInstall, origDry := jsonFlag, sshConfigInstall, sshConfigDryRun
	t.Cleanup(func() { jsonFlag, sshConfigInstall, sshConfigDryRun = origJSON, origInstall, origDry })
	sshConfigInstall, sshConfigDryRun = false, false

	for _, asJSON := range []bool{false, true} {
		jsonFlag = asJSON
		var runErr error
		var stdout string
		stderr := captureStderr(t, func() {
			stdout = captureStdout(t, func() { runErr = runSSHConfig(sshConfigCmd, nil) })
		})
		if runErr != nil {
			t.Fatalf("ssh-config (json %v): %v", asJSON, runErr)
		}
		if !strings.Contains(stdout, "shed-good") {
			t.Errorf("json %v: the well-formed shed is missing:\n%s", asJSON, stdout)
		}
		for _, bad := range []string{"ProxyCommand", "two words", "star", "pct", "quo"} {
			if strings.Contains(stdout, bad) {
				t.Errorf("json %v: a hostile shed name was printed (%q):\n%s", asJSON, bad, stdout)
			}
		}
		if got := strings.Count(stderr, "warning: skipped shed "); got != len(hostileShedNames) {
			t.Errorf("json %v: %d skip warnings, want %d:\n%s", asJSON, got, len(hostileShedNames), stderr)
		}
	}

	// Only hostile names: the command fails and nothing is written.
	only := namedShedsServer(t, hostileShedNames)
	putServerEntry(t, "srv", config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, APIURL: only.URL})
	jsonFlag, sshConfigInstall = false, true
	var runErr error
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() { runErr = runSSHConfig(sshConfigCmd, nil) })
	})
	if runErr == nil {
		t.Error("an answer of only hostile shed names did not fail the command")
	}
	if _, err := os.Stat(sshconfig.GetSSHConfigPath()); !os.IsNotExist(err) {
		t.Errorf("~/.ssh/config was written for an answer of only hostile names (err=%v)", err)
	}
}

// TestAttachAliasRefusesShedNamesThatAreNotShedNames: `shed attach`'s alias
// for a shed whose name is not a shed name is refused by the writer, with or
// without an identity file: nothing is written, and the error shows the name
// escaped.
func TestAttachAliasRefusesShedNamesThatAreNotShedNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	key := writeKey(t, t.TempDir(), "id_work")
	plain := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}
	keyed := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: key}
	for _, entry := range []*config.ServerEntry{plain, keyed} {
		for _, name := range hostileShedNames {
			path := filepath.Join(t.TempDir(), "config")
			var out bytes.Buffer
			a := &roostAttach{sshConfigPath: path, out: &out, shedServer: "srv"}
			err := a.ensureSSHAlias("shed-"+name, name, entry)
			var refused *sshconfig.EntryError
			if !errors.As(err, &refused) {
				t.Errorf("%q (identity %q): err = %v, want the writer's refusal", name, entry.IdentityFile, err)
				continue
			}
			if strings.ContainsRune(err.Error(), '\n') {
				t.Errorf("%q: the error carries a raw line break: %q", name, err)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Errorf("%q: an alias was written (err=%v)", name, statErr)
			}
			if out.Len() != 0 {
				t.Errorf("%q: output %q", name, out.String())
			}
		}
	}
	// The same alias with a well-formed name is written.
	path := filepath.Join(t.TempDir(), "config")
	a := &roostAttach{sshConfigPath: path, out: &bytes.Buffer{}, shedServer: "srv"}
	if err := a.ensureSSHAlias("shed-good", "good", plain); err != nil {
		t.Fatalf("a well-formed name: %v", err)
	}
	if got := string(must(os.ReadFile(path))); !strings.Contains(got, "Host shed-good\n") {
		t.Errorf("the well-formed alias was not written:\n%s", got)
	}
}
