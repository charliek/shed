package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/roostprovider"
)

// ---- roostProviderPhase ----

func TestRoostProviderPhase(t *testing.T) {
	t.Run("positional arg wins over the env var", func(t *testing.T) {
		t.Setenv("ROOST_PROVIDER_PHASE", "activate")
		if got := roostProviderPhase([]string{"list"}); got != "list" {
			t.Errorf("phase = %q, want %q", got, "list")
		}
	})
	t.Run("falls back to the env var with no arg", func(t *testing.T) {
		t.Setenv("ROOST_PROVIDER_PHASE", "activate")
		if got := roostProviderPhase(nil); got != "activate" {
			t.Errorf("phase = %q, want %q", got, "activate")
		}
	})
	t.Run("an empty positional arg still falls back", func(t *testing.T) {
		t.Setenv("ROOST_PROVIDER_PHASE", "list")
		if got := roostProviderPhase([]string{""}); got != "list" {
			t.Errorf("phase = %q, want %q", got, "list")
		}
	})
	t.Run("neither given is empty", func(t *testing.T) {
		t.Setenv("ROOST_PROVIDER_PHASE", "")
		if got := roostProviderPhase(nil); got != "" {
			t.Errorf("phase = %q, want empty", got)
		}
	})
}

// ---- selectedIDFrom / parseSelectedIDJSON ----

func TestSelectedIDFrom(t *testing.T) {
	calledFallback := false
	fallback := func() (string, bool) {
		calledFallback = true
		return "from-stdin", true
	}

	t.Run("the env value wins and the stdin fallback is never called", func(t *testing.T) {
		calledFallback = false
		got, err := selectedIDFrom("shed=dev&server=srv", fallback)
		if err != nil {
			t.Fatalf("selectedIDFrom: %v", err)
		}
		if got != "shed=dev&server=srv" {
			t.Errorf("id = %q", got)
		}
		if calledFallback {
			t.Errorf("the stdin fallback ran despite a non-empty env value")
		}
	})

	t.Run("an empty env value falls back to stdin", func(t *testing.T) {
		calledFallback = false
		got, err := selectedIDFrom("", fallback)
		if err != nil {
			t.Fatalf("selectedIDFrom: %v", err)
		}
		if got != "from-stdin" {
			t.Errorf("id = %q", got)
		}
		if !calledFallback {
			t.Errorf("the stdin fallback did not run")
		}
	})

	t.Run("neither source has an id is an error", func(t *testing.T) {
		_, err := selectedIDFrom("", func() (string, bool) { return "", false })
		if err == nil {
			t.Fatalf("expected an error with no id anywhere")
		}
	})
}

func TestParseSelectedIDJSON(t *testing.T) {
	t.Run("roost's real stdin shape", func(t *testing.T) {
		body := `{"v":1,"phase":"activate","selected_id":"machine=mini2","query":"","active_tab":{"cwd":"","title":""},"socket":"/tmp/roost.sock"}` + "\n"
		got, ok := parseSelectedIDJSON(strings.NewReader(body))
		if !ok {
			t.Fatalf("parseSelectedIDJSON did not find an id in %q", body)
		}
		if got != "machine=mini2" {
			t.Errorf("id = %q", got)
		}
	})
	t.Run("no selected_id field is not ok", func(t *testing.T) {
		_, ok := parseSelectedIDJSON(strings.NewReader(`{"v":1,"phase":"list"}`))
		if ok {
			t.Errorf("expected ok=false with no selected_id field")
		}
	})
	t.Run("malformed JSON is not ok", func(t *testing.T) {
		_, ok := parseSelectedIDJSON(strings.NewReader(`not json`))
		if ok {
			t.Errorf("expected ok=false on malformed JSON")
		}
	})
}

// ---- launcher content + label detection ----

func TestRoostProviderLauncherContent(t *testing.T) {
	got := roostProviderLauncherContent("/abs/path/to/shed")
	want := "#!/bin/sh\n" +
		"# @roost.label: shed\n" +
		"# @roost.title: Start an agent on a shed or machine\n" +
		"# Written by `shed roost-provider --install`; re-run it if shed moves. Roost runs\n" +
		"# this by absolute path with its own (possibly minimal) PATH, so the shed binary\n" +
		"# is pinned here and the usual install dirs are prefixed, as roost's example does.\n" +
		`PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:$HOME/.local/bin:$PATH"; export PATH` + "\n" +
		`exec '/abs/path/to/shed' roost-provider "$@"` + "\n"
	if got != want {
		t.Errorf("launcher content:\n got %q\nwant %q", got, want)
	}
}

func TestHasRoostProviderLabel(t *testing.T) {
	t.Run("shed's own content, any pinned path", func(t *testing.T) {
		if !hasRoostProviderLabel(roostProviderLauncherContent("/one/shed")) {
			t.Errorf("shed's own content was not recognized")
		}
		if !hasRoostProviderLabel(roostProviderLauncherContent("/somewhere/else/shed")) {
			t.Errorf("a differently-pinned launcher was not recognized as shed's own")
		}
	})
	t.Run("an unrelated script is not shed's", func(t *testing.T) {
		if hasRoostProviderLabel("#!/bin/sh\necho hi\n") {
			t.Errorf("an unrelated script was misidentified as shed's own launcher")
		}
	})
	t.Run("empty content is not shed's", func(t *testing.T) {
		if hasRoostProviderLabel("") {
			t.Errorf("empty content was misidentified as shed's own launcher")
		}
	})
}

// ---- --install / --uninstall / --dry-run ----

// withRoostProviderConfig points $ROOST_CONFIG at a file under a fresh temp
// dir (the file itself need not exist — roostProviderDir only reads its
// parent) and returns the launcher path that implies.
func withRoostProviderConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ROOST_CONFIG", filepath.Join(dir, "config.conf"))
	return filepath.Join(dir, "providers", "shed")
}

// withRoostProviderFlags sets the install/uninstall/dry-run globals for one
// call and restores them on cleanup — the same pattern
// cmd/shed/server_add_transport_test.go uses for jsonFlag.
func withRoostProviderFlags(t *testing.T, install, uninstall, dryRun bool) {
	t.Helper()
	oi, ou, od := roostProviderInstall, roostProviderUninstall, roostProviderDryRun
	roostProviderInstall, roostProviderUninstall, roostProviderDryRun = install, uninstall, dryRun
	t.Cleanup(func() { roostProviderInstall, roostProviderUninstall, roostProviderDryRun = oi, ou, od })
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

func TestRoostProviderInstall_FreshWrite(t *testing.T) {
	launcherPath := withRoostProviderConfig(t)
	withRoostProviderFlags(t, true, false, false)

	if err := runRoostProviderInstall(); err != nil {
		t.Fatalf("runRoostProviderInstall: %v", err)
	}

	info, err := os.Stat(launcherPath)
	if err != nil {
		t.Fatalf("stat launcher: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 0755", info.Mode().Perm())
	}
	shedPath, err := resolvedShedExecutable()
	if err != nil {
		t.Fatalf("resolvedShedExecutable: %v", err)
	}
	if got, want := mustReadFile(t, launcherPath), roostProviderLauncherContent(shedPath); got != want {
		t.Errorf("launcher content:\n got %q\nwant %q", got, want)
	}
}

func TestRoostProviderInstall_DryRunWritesNothing(t *testing.T) {
	launcherPath := withRoostProviderConfig(t)
	withRoostProviderFlags(t, true, false, true)

	if err := runRoostProviderInstall(); err != nil {
		t.Fatalf("runRoostProviderInstall: %v", err)
	}
	if _, err := os.Stat(launcherPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run install must not write a file; stat err = %v", err)
	}
}

func TestRoostProviderInstall_IsIdempotent(t *testing.T) {
	launcherPath := withRoostProviderConfig(t)
	withRoostProviderFlags(t, true, false, false)

	if err := runRoostProviderInstall(); err != nil {
		t.Fatalf("first install: %v", err)
	}
	first := mustReadFile(t, launcherPath)

	if err := runRoostProviderInstall(); err != nil {
		t.Fatalf("second install: %v", err)
	}
	second := mustReadFile(t, launcherPath)

	if first != second {
		t.Fatalf("re-install changed the file:\n first  %q\n second %q", first, second)
	}
}

func TestRoostProviderUninstall_RemovesItsOwnLauncher(t *testing.T) {
	launcherPath := withRoostProviderConfig(t)
	withRoostProviderFlags(t, true, false, false)
	if err := runRoostProviderInstall(); err != nil {
		t.Fatalf("install: %v", err)
	}

	withRoostProviderFlags(t, false, true, false)
	if err := runRoostProviderUninstall(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(launcherPath); !os.IsNotExist(err) {
		t.Fatalf("launcher survived uninstall; stat err = %v", err)
	}
}

func TestRoostProviderUninstall_DryRunRemovesNothing(t *testing.T) {
	launcherPath := withRoostProviderConfig(t)
	withRoostProviderFlags(t, true, false, false)
	if err := runRoostProviderInstall(); err != nil {
		t.Fatalf("install: %v", err)
	}

	withRoostProviderFlags(t, false, true, true)
	if err := runRoostProviderUninstall(); err != nil {
		t.Fatalf("dry-run uninstall: %v", err)
	}
	if _, err := os.Stat(launcherPath); err != nil {
		t.Fatalf("dry-run uninstall removed the launcher: %v", err)
	}
}

func TestRoostProviderUninstall_NoFileIsANoop(t *testing.T) {
	withRoostProviderConfig(t)
	withRoostProviderFlags(t, false, true, false)
	if err := runRoostProviderUninstall(); err != nil {
		t.Fatalf("uninstall with nothing to remove: %v", err)
	}
}

// TestRoostProviderSafety is the prompt's required safety case: a
// `providers/shed` that exists WITHOUT the label header must never be deleted
// by --uninstall, and --install must never overwrite it either.
func TestRoostProviderSafety(t *testing.T) {
	launcherPath := withRoostProviderConfig(t)
	if err := os.MkdirAll(filepath.Dir(launcherPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "#!/bin/sh\n# somebody else's provider, nothing to do with shed\necho hi\n"
	if err := os.WriteFile(launcherPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("--uninstall leaves it alone", func(t *testing.T) {
		withRoostProviderFlags(t, false, true, false)
		if err := runRoostProviderUninstall(); err != nil {
			t.Fatalf("uninstall over a foreign file returned an error: %v", err)
		}
		if got := mustReadFile(t, launcherPath); got != foreign {
			t.Fatalf("a foreign file was modified by --uninstall:\n got %q\nwant %q", got, foreign)
		}
	})

	t.Run("--install refuses to overwrite it", func(t *testing.T) {
		withRoostProviderFlags(t, true, false, false)
		err := runRoostProviderInstall()
		if err == nil {
			t.Fatalf("--install silently overwrote a foreign file")
		}
		if got := mustReadFile(t, launcherPath); got != foreign {
			t.Fatalf("a foreign file was modified by a refused --install:\n got %q\nwant %q", got, foreign)
		}
	})
}

// ---- resolveTarget ----

// shedsHandler and openEntry mirror internal/roostprovider/inventory_test.go's
// helpers of the same name (unexported there, so this is a parallel copy, not
// a shared import — the two packages' tests do not reach into each other).
func shedsHandler(t *testing.T, sheds ...config.Shed) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(config.ShedsResponse{Sheds: sheds}); err != nil {
			t.Fatalf("encode sheds response: %v", err)
		}
	}
}

func openEntry(ts *httptest.Server) config.ServerEntry {
	return config.ServerEntry{Host: "127.0.0.1", APIURL: ts.URL, SSHPort: 2222}
}

func setMachinesYAML(t *testing.T, yamlDoc string) {
	t.Helper()
	var tmp config.ClientConfig
	if err := yaml.Unmarshal([]byte(yamlDoc), &tmp); err != nil {
		t.Fatalf("unmarshal machines yaml: %v", err)
	}
	clientConfig.Machines = tmp.Machines
}

func TestResolveTarget_UnconfiguredMachineIsUnreachable(t *testing.T) {
	testClientConfig(t)
	_, _, row := resolveTarget(t.Context(), roostprovider.Token{Machine: "ghost"})
	if row == nil || row.Actionable == nil || *row.Actionable {
		t.Fatalf("expected a non-actionable row, got %+v", row)
	}
}

func TestResolveTarget_ConfiguredMachine(t *testing.T) {
	testClientConfig(t)
	setMachinesYAML(t, "machines:\n  mini2:\n    host: mini2.local\n    ssh_port: 2200\n")

	target, landingDir, row := resolveTarget(t.Context(), roostprovider.Token{Machine: "mini2"})
	if row != nil {
		t.Fatalf("unexpected row: %+v", row)
	}
	if landingDir != "" {
		t.Errorf("a machine must never carry a landing dir, got %q", landingDir)
	}
	if target.Dest != "mini2.local" || target.Port != 2200 {
		t.Errorf("target = %+v", target)
	}
}

func TestResolveTarget_UnconfiguredServerIsUnreachable(t *testing.T) {
	testClientConfig(t)
	_, _, row := resolveTarget(t.Context(), roostprovider.Token{Shed: "dev", Server: "ghost-server"})
	if row == nil || row.Actionable == nil || *row.Actionable {
		t.Fatalf("expected a non-actionable row, got %+v", row)
	}
}

func TestResolveTarget_ShedNoLongerRunning(t *testing.T) {
	testClientConfig(t)
	ts := httptest.NewServer(shedsHandler(t)) // no sheds at all
	defer ts.Close()
	clientConfig.Servers["srv"] = openEntry(ts)

	_, _, row := resolveTarget(t.Context(), roostprovider.Token{Shed: "dev", Server: "srv"})
	if row == nil {
		t.Fatalf("expected a row for a shed the server no longer reports as running")
	}
	if !strings.Contains(row.Subtitle, "no longer running") {
		t.Errorf("subtitle = %q, want it to say the shed stopped", row.Subtitle)
	}
}

func TestResolveTarget_ShedRunning(t *testing.T) {
	testClientConfig(t)
	ts := httptest.NewServer(shedsHandler(t,
		config.Shed{Name: "dev", Status: config.StatusRunning, LandingDir: "/home/dev/proj"}))
	defer ts.Close()
	clientConfig.Servers["srv"] = openEntry(ts)

	target, landingDir, row := resolveTarget(t.Context(), roostprovider.Token{Shed: "dev", Server: "srv"})
	if row != nil {
		t.Fatalf("unexpected row: %+v", row)
	}
	if landingDir != "/home/dev/proj" {
		t.Errorf("landingDir = %q", landingDir)
	}
	if target.Dest != "dev@127.0.0.1" || target.Port != 2222 || !target.EmitPort {
		t.Errorf("target = %+v", target)
	}
}

func TestResolveTarget_ServerDidNotAnswer(t *testing.T) {
	testClientConfig(t)
	ts := httptest.NewServer(shedsHandler(t))
	addr := ts.URL
	ts.Close() // closed before use: connection refused, fast and deterministic

	clientConfig.Servers["srv"] = config.ServerEntry{Host: "127.0.0.1", APIURL: addr, SSHPort: 2222}

	_, _, row := resolveTarget(t.Context(), roostprovider.Token{Shed: "dev", Server: "srv"})
	if row == nil || !strings.Contains(row.Subtitle, "did not answer") {
		t.Fatalf("row = %+v, want a \"did not answer\" subtitle", row)
	}
}

// ---- end-to-end activate, over a fake ssh + fake roost-session ----
//
// This is deliberately a SMALL fixture, not a port of
// internal/roostprovider's exhaustive fake-ssh rig (fakessh_test.go): C2
// already covers the wire mechanics (stderr classification, exact request
// JSON, every non-actionable row) at the package level. What is new here is
// the CLI wiring this file adds on top — phase dispatch, resolveTarget,
// reachHost, the workdir collapse, and openTab's confirmation line — so this
// exercises exactly one path (a machine token) end to end through
// runRoostProviderActivate, real bash, real files, and a real ssh subprocess
// that runs the remote command locally.

// buildFakeSSH writes a fake `ssh` that parses the SUBSET of Remote.Argv's
// options this test's Target actually produces (no `-p`, no ControlMaster,
// no host-key pinning — see the machine entry below), resets HOME/USER/PATH
// to the fake far side, and runs the remote command through /bin/sh -c.
func buildFakeSSH(t *testing.T, dir, fakeHome string) string {
	t.Helper()
	path := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    -o) shift 2 ;;
    -T) shift ;;
    --) shift; break ;;
    *) break ;;
  esac
done
dest=$1; shift
if [ "$1" = "--" ]; then shift; fi
cmd=$*
HOME=` + shellQuoteArg(fakeHome) + `
USER=fakeshed
PATH=/usr/bin:/bin
export HOME USER PATH
exec /bin/sh -c "$cmd"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	return path
}

// The two files the fake far side reads and writes, both under its $HOME so
// the fake ssh's own HOME reset is what finds them.
//
// farSideProtocolFile lets a test change what the session claims to speak
// BETWEEN activate calls — which is the real shape of the thing under test,
// because roost re-execs the provider per step and the far side is free to
// restart at a different version in between. farSideOpLog records every op the
// bridge was actually asked for, which is how "no tab was opened" is asserted
// as a fact about the wire rather than as the absence of a line on stdout.
const (
	farSideProtocolFile = ".protocol"
	farSideOpLog        = ".ops.log"
)

// buildFakeFarSide lays down a fake $HOME with a login-shell profile, one
// agent binary, and a fake `roost-session` that answers the three ops this
// package's Remote ever sends.
func buildFakeFarSide(t *testing.T, home string) {
	t.Helper()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteExec(t, filepath.Join(home, ".profile"), `PATH="$HOME/.local/bin:$PATH"; export PATH`+"\n", 0o644)
	mustWriteExec(t, filepath.Join(binDir, "claude"), "#!/bin/sh\necho claude\n", 0o755)

	session := `#!/bin/sh
case "$1" in
  client-bridge)
    line=$(head -n 1)
    proto=4
    if [ -f "$HOME/` + farSideProtocolFile + `" ]; then proto=$(cat "$HOME/` + farSideProtocolFile + `"); fi
    case "$line" in
      *session.identify*)
        printf '%s\n' session.identify >> "$HOME/` + farSideOpLog + `"
        printf '{"id":"1","ok":true,"result":{"session_protocol":%s}}\n' "$proto" ;;
      *tab.list*)
        printf '%s\n' tab.list >> "$HOME/` + farSideOpLog + `"
        printf '%s\n' '{"id":"1","ok":true,"result":{"projects":[]}}' ;;
      *tab.open*)
        printf '%s\n' tab.open >> "$HOME/` + farSideOpLog + `"
        printf '%s\n' '{"id":"1","ok":true,"result":{"tab":{"id":"tab-42"}}}' ;;
      *) printf '%s\n' '{"id":"1","ok":false,"error":{"code":"bad","message":"unexpected op"}}' ;;
    esac
    ;;
  *) echo "roost-session: unknown subcommand $1" >&2; exit 2 ;;
esac
`
	mustWriteExec(t, filepath.Join(binDir, "roost-session"), session, 0o755)
}

// setFarSideProtocol makes the fake session claim protocol n until the test
// (or subtest) that called this ends.
func setFarSideProtocol(t *testing.T, home string, n int) {
	t.Helper()
	path := filepath.Join(home, farSideProtocolFile)
	mustWriteExec(t, path, fmt.Sprintf("%d\n", n), 0o644)
	t.Cleanup(func() { _ = os.Remove(path) })
}

// farSideOps returns every op the fake bridge has been asked for since
// truncateFarSideOps, in order.
func farSideOps(t *testing.T, home string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, farSideOpLog))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the far side's op log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func truncateFarSideOps(t *testing.T, home string) {
	t.Helper()
	if err := os.Remove(filepath.Join(home, farSideOpLog)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clearing the far side's op log: %v", err)
	}
}

func mustWriteExec(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// requireBash skips the test when there is no real `bash` to run the probe
// script under (ProbeCommand always uses `bash -lc`) — this test execs real
// binaries, unlike the rest of the package's hermetic unit tests.
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on PATH")
	}
}

func TestRoostProviderActivate_MachineEndToEnd(t *testing.T) {
	requireBash(t)
	testClientConfig(t)
	setMachinesYAML(t, "machines:\n  mini2:\n    host: fake-host\n")

	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	buildFakeFarSide(t, home)
	sshBin := buildFakeSSH(t, dir, home)
	t.Setenv("PATH", filepath.Dir(sshBin)+string(os.PathListSeparator)+os.Getenv("PATH"))

	runActivate := func(t *testing.T, id string) string {
		t.Helper()
		t.Setenv("ROOST_SELECTED_ID", id)
		var buf bytes.Buffer
		restore := redirectStdout(t, &buf)
		err := runRoostProviderActivate()
		restore()
		if err != nil {
			t.Fatalf("runRoostProviderActivate(%q): %v\noutput so far: %s", id, err, buf.String())
		}
		return buf.String()
	}

	t.Run("step 2: a bare host token lists the found agent", func(t *testing.T) {
		out := runActivate(t, "machine=mini2")
		// The row id's "&" is JSON/HTML-escaped by the default encoder
		// (json.Encoder's SetEscapeHTML defaults true, and printMenu does not
		// turn it off — unlike the wire request encoder in internal/roostprovider,
		// which does for a byte-for-byte roost comparison this stdout contract
		// has no need of), so this checks the pieces rather than the whole id.
		if !strings.Contains(out, "agent=claude-rc") {
			t.Fatalf("agent menu missing the agent= token piece: %s", out)
		}
		if !strings.Contains(out, `"title":"claude"`) {
			t.Fatalf("agent menu missing the claude row: %s", out)
		}
	})

	t.Run("step 3 collapses (Home is the only candidate) straight to an opened tab", func(t *testing.T) {
		id := fmt.Sprintf("agent=claude-rc&home=%s&machine=mini2", home)
		out := runActivate(t, id)
		want := "opened tab tab-42 on mini2\n"
		if out != want {
			t.Fatalf("activate output = %q, want %q", out, want)
		}
	})

	// A COMPLETED token — one that already names a workdir — is the step roost
	// activates from the workdir menu, and the only step that reaches
	// `tab.open` without passing through reachHost's gate first. (The collapse
	// case above does not: it re-probes, so it identifies on the way.) These
	// two subtests are that step at both protocols.
	completed := roostprovider.Token{
		Machine: "mini2", Agent: "claude-rc", Home: home, Cwd: home,
	}.Encode()

	t.Run("step 4: a completed token opens a tab", func(t *testing.T) {
		truncateFarSideOps(t, home)
		out := runActivate(t, completed)
		if want := "opened tab tab-42 on mini2\n"; out != want {
			t.Fatalf("activate output = %q, want %q", out, want)
		}
		// The gate runs on this path too — cheaply, on the same connection —
		// and the op log is where that is visible.
		ops := farSideOps(t, home)
		if len(ops) == 0 || ops[0] != "session.identify" {
			t.Errorf("ops = %v, want session.identify before tab.open", ops)
		}
	})

	// The protocol gate belongs to the STEP, not to the wizard: roost re-execs
	// the provider per drill-down, so step 2's `session.identify` ran in a
	// process that has already exited by the time this token arrives. A far
	// side that restarted at a different version in between (a roost upgrade,
	// a downgrade, a rollback) must therefore be re-gated here — or the one
	// path that MUTATES the far side is the one path that never checks whether
	// it speaks the same wire.
	t.Run("step 4 re-gates on the protocol before opening", func(t *testing.T) {
		setFarSideProtocol(t, home, 3)
		truncateFarSideOps(t, home)

		out := runActivate(t, completed)

		if !strings.Contains(out, "roost-session on mini2 speaks protocol 3; this shed speaks 4") {
			t.Errorf("activate output = %q, want the pinned protocol-mismatch row", out)
		}
		if strings.Contains(out, "opened tab") {
			t.Fatalf("a protocol-mismatched far side still got a tab: %q", out)
		}
		// The wire, not just the words: nothing may have asked for tab.open.
		ops := farSideOps(t, home)
		for _, op := range ops {
			if op == "tab.open" {
				t.Fatalf("tab.open reached a protocol-3 far side (ops: %v)", ops)
			}
		}
	})
}

// redirectStdout points os.Stdout at a pipe that copies into buf for the
// duration of a call, restoring the original on the returned func. Needed
// because printMenu/openTab write straight to os.Stdout.
func redirectStdout(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan struct{})
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()
	return func() {
		os.Stdout = orig
		_ = w.Close()
		<-done
		_ = r.Close()
	}
}

// ---- installer safety: symlinks, non-regular files, and $ROOST_CONFIG ----
//
// The property under test in all of these is one sentence: `--install` and
// `--uninstall` act on a REGULAR FILE at `<dir>/providers/shed` that shed
// itself wrote, and on nothing else. The old shape (ReadFile to check the
// header, then WriteFile/Remove to act) followed symlinks at every step, which
// made a link planted at that path a way to have shed truncate its target.

// assertNoLauncherTempFiles fails when writeLauncherAtomically left a
// fragment behind. A leftover is not cosmetic: it is a mode-0755 partial
// script sitting in roost's providers directory.
func assertNoLauncherTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".shed-roost-provider-") {
			t.Errorf("leftover temp file %s in %s", e.Name(), dir)
		}
	}
}

func TestRoostProviderInstall_RefusesASymlink(t *testing.T) {
	// Two targets, because the refusal has to be about the SYMLINK and not
	// about what happens to be on the other end of it:
	//
	//   - a foreign file (`~/.ssh/authorized_keys`, the motivating case). A
	//     header check alone also refuses this one — by accident, from the
	//     content, having already read through the link.
	//   - a file carrying shed's OWN header (a launcher someone symlinked in
	//     from a dotfiles checkout). A header check WAVES THIS THROUGH, and
	//     the write then lands on the far end of the link.
	targets := []struct {
		name    string
		content string
	}{
		{"a foreign file", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 someone@somewhere\n"},
		{"a file carrying shed's own header", roostProviderLauncherContent("/elsewhere/bin/shed")},
	}
	for _, tc := range targets {
		t.Run(tc.name, func(t *testing.T) {
			launcherPath := withRoostProviderConfig(t)
			if err := os.MkdirAll(filepath.Dir(launcherPath), 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "target")
			mustWriteExec(t, target, tc.content, 0o600)
			if err := os.Symlink(target, launcherPath); err != nil {
				t.Fatalf("symlink: %v", err)
			}

			assertTargetIntact := func(t *testing.T) {
				t.Helper()
				if got := mustReadFile(t, target); got != tc.content {
					t.Fatalf("the symlink's TARGET was modified:\n got %q\nwant %q", got, tc.content)
				}
				info, err := os.Lstat(launcherPath)
				if err != nil {
					t.Fatalf("lstat %s: %v", launcherPath, err)
				}
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("the symlink at %s was replaced (mode %s)", launcherPath, info.Mode())
				}
			}

			t.Run("--install refuses", func(t *testing.T) {
				withRoostProviderFlags(t, true, false, false)
				err := runRoostProviderInstall()
				if err == nil {
					t.Fatalf("--install accepted a symlink at %s", launcherPath)
				}
				if !strings.Contains(err.Error(), launcherPath) {
					t.Errorf("the refusal does not name the path: %v", err)
				}
				assertTargetIntact(t)
			})

			t.Run("--uninstall refuses", func(t *testing.T) {
				withRoostProviderFlags(t, false, true, false)
				err := runRoostProviderUninstall()
				if err == nil {
					t.Fatalf("--uninstall accepted a symlink at %s", launcherPath)
				}
				if !strings.Contains(err.Error(), launcherPath) {
					t.Errorf("the refusal does not name the path: %v", err)
				}
				assertTargetIntact(t)
			})
		})
	}
}

// TestRoostProviderInstall_RefusesADanglingSymlink is the nastier half of the
// symlink case: ReadFile and Stat both report ENOENT through a dangling link,
// so an installer that trusts them reads "nothing is there, this is a fresh
// install" and CREATES the link's target — a file at a path the user never
// pointed shed at.
func TestRoostProviderInstall_RefusesADanglingSymlink(t *testing.T) {
	launcherPath := withRoostProviderConfig(t)
	if err := os.MkdirAll(filepath.Dir(launcherPath), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "not-there-yet")
	if err := os.Symlink(target, launcherPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	withRoostProviderFlags(t, true, false, false)
	if err := runRoostProviderInstall(); err == nil {
		t.Fatalf("--install accepted a dangling symlink at %s", launcherPath)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("--install created the dangling link's target %s (err = %v)", target, err)
	}
}

// TestRoostProviderInstall_RefusesANonRegularFile: a FIFO is the same class of
// mistake as a symlink — opening it for the header check blocks, and writing
// through it goes to a reader, not to a file.
func TestRoostProviderInstall_RefusesANonRegularFile(t *testing.T) {
	launcherPath := withRoostProviderConfig(t)
	if err := os.MkdirAll(filepath.Dir(launcherPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(launcherPath, 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	withRoostProviderFlags(t, true, false, false)
	if err := runRoostProviderInstall(); err == nil {
		t.Fatalf("--install accepted a FIFO at %s", launcherPath)
	}
	info, err := os.Lstat(launcherPath)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the FIFO was replaced (mode %s)", info.Mode())
	}
}

func TestRoostProviderDir_RefusesADirectoryConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROOST_CONFIG", dir)

	if got, err := roostProviderDir(); err == nil {
		t.Fatalf("a directory $ROOST_CONFIG resolved to %q instead of being refused", got)
	}

	// And through the command, so the refusal really is on the path an
	// operator takes rather than only in the helper.
	withRoostProviderFlags(t, true, false, false)
	if err := runRoostProviderInstall(); err == nil {
		t.Fatalf("--install accepted a directory $ROOST_CONFIG")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "providers", "shed")); !os.IsNotExist(err) {
		t.Fatalf("--install wrote beside a directory $ROOST_CONFIG (err = %v)", err)
	}
}

// TestRoostProviderDir_RefusesTheFilesystemRoot pins both spellings of the
// same mistake: `ROOST_CONFIG=/` (a directory, and the root one) and
// `ROOST_CONFIG=/roost.conf`, whose parent is `/` and which used to resolve to
// the launcher path `/providers/shed`.
func TestRoostProviderDir_RefusesTheFilesystemRoot(t *testing.T) {
	for _, cfg := range []string{"/", "/roost.conf"} {
		t.Run(cfg, func(t *testing.T) {
			t.Setenv("ROOST_CONFIG", cfg)
			got, err := roostProviderDir()
			if err == nil {
				t.Fatalf("$ROOST_CONFIG=%q resolved to %q instead of being refused", cfg, got)
			}
		})
	}
}

func TestRoostProviderInstall_IsAtomic(t *testing.T) {
	t.Run("a successful install leaves no temp file", func(t *testing.T) {
		launcherPath := withRoostProviderConfig(t)
		withRoostProviderFlags(t, true, false, false)
		if err := runRoostProviderInstall(); err != nil {
			t.Fatalf("runRoostProviderInstall: %v", err)
		}
		assertNoLauncherTempFiles(t, filepath.Dir(launcherPath))
	})

	t.Run("a failed write leaves no temp file", func(t *testing.T) {
		dir := t.TempDir()
		// A rename onto an existing DIRECTORY fails (EISDIR): a forced failure
		// at the last step, after the temp file has been created, written and
		// chmod'd — which is exactly the window a cleanup-on-error path has to
		// cover.
		dest := filepath.Join(dir, "shed")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeLauncherAtomically(dest, roostProviderLauncherContent("/abs/path/to/shed")); err == nil {
			t.Fatalf("renaming over a directory unexpectedly succeeded")
		}
		assertNoLauncherTempFiles(t, dir)
	})

	t.Run("an update repairs a launcher chmod'd down", func(t *testing.T) {
		launcherPath := withRoostProviderConfig(t)
		if err := os.MkdirAll(filepath.Dir(launcherPath), 0o755); err != nil {
			t.Fatal(err)
		}
		// shed's own header, a stale pinned path, and no execute bit: the
		// "updated" branch, which os.WriteFile alone would leave at 0600.
		mustWriteExec(t, launcherPath, roostProviderLauncherContent("/an/old/path/to/shed"), 0o600)

		withRoostProviderFlags(t, true, false, false)
		if err := runRoostProviderInstall(); err != nil {
			t.Fatalf("runRoostProviderInstall: %v", err)
		}
		info, err := os.Stat(launcherPath)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("mode after an update = %o, want 0755", info.Mode().Perm())
		}
		assertNoLauncherTempFiles(t, filepath.Dir(launcherPath))
	})
}

// ---- the phase budgets ----

// TestRoostProviderBudgetsFitInsideRoosts is the invariant the numbers exist to
// satisfy: roost kills a provider at its own timeout (5s by default), so an
// internal budget at or past that one cannot print the row it was waiting for —
// §3.2's "every expected state exits zero with a row" quietly stops holding on a
// default install.
func TestRoostProviderBudgetsFitInsideRoosts(t *testing.T) {
	const roostDefaultProviderTimeout = 5 * time.Second
	for _, tc := range []struct {
		name   string
		budget time.Duration
	}{
		{"list", roostProviderListBudget},
		{"activate", roostProviderActivateBudget},
	} {
		if tc.budget >= roostDefaultProviderTimeout {
			t.Errorf("the %s budget is %s, which is not inside roost's own %s default",
				tc.name, tc.budget, roostDefaultProviderTimeout)
		}
	}
}

func TestRoostProviderBudgetFrom(t *testing.T) {
	const def = 4 * time.Second
	tests := []struct {
		name     string
		raw      string
		want     time.Duration
		wantWarn bool
	}{
		{"unset uses the default", "", def, false},
		{"a raised budget is honoured", "30s", 30 * time.Second, false},
		{"a compound duration parses", "1m500ms", time.Minute + 500*time.Millisecond, false},
		{"garbage falls back and warns", "soon", def, true},
		{"a bare number falls back and warns", "30", def, true},
		{"zero falls back and warns", "0s", def, true},
		{"negative falls back and warns", "-5s", def, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var warn bytes.Buffer
			got := roostProviderBudgetFrom(tc.raw, def, &warn)
			if got != tc.want {
				t.Errorf("budget = %s, want %s", got, tc.want)
			}
			if gotWarn := warn.Len() > 0; gotWarn != tc.wantWarn {
				t.Errorf("warned = %t, want %t (warning: %q)", gotWarn, tc.wantWarn, warn.String())
			}
		})
	}
}

// ---- the bounded stdin fallback ----

func TestReadSelectedIDWithin(t *testing.T) {
	t.Run("a writer that closes the pipe is read", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		go func() {
			_, _ = w.WriteString(`{"v":1,"phase":"activate","selected_id":"machine=mini2"}` + "\n")
			_ = w.Close()
		}()
		id, ok := readSelectedIDWithin(r, 5*time.Second)
		if !ok || id != "machine=mini2" {
			t.Fatalf("id = %q, ok = %t", id, ok)
		}
	})

	t.Run("a writer that holds the pipe open gives up at the deadline", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		// Deliberately never closed inside the test body: this is the shape
		// that used to hang activate forever. The byte cap does not help —
		// io.ReadAll waits for EOF, and EOF is what this writer withholds.
		defer w.Close()
		if _, err := w.WriteString(`{"selected_id":"machine=mini2"}` + "\n"); err != nil {
			t.Fatal(err)
		}

		start := time.Now()
		id, ok := readSelectedIDWithin(r, 50*time.Millisecond)
		elapsed := time.Since(start)

		if ok {
			t.Fatalf("a held-open pipe answered with %q instead of giving up", id)
		}
		if elapsed > 2*time.Second {
			t.Errorf("the read took %s — the deadline did not bound it", elapsed)
		}
	})
}
