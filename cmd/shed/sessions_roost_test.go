package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/roostctl"
	"github.com/charliek/shed/internal/roostprovider"
)

// Tests for the roost-native `shed sessions` / `sessions kill` (plan 022 §3.3,
// C5c).
//
// Three fixtures, the same layering C5b's attach tests use: the `roostctl`
// shim (roostctl_shim_test.go) is the LOCAL app and answers the gate, the fake
// shed (roost_fakeshed_test.go) is the far side's roost-session over a fake
// ssh, and a small httptest server is the shed-server API the tmux half reads.
// Nothing between the argv and the parser is stubbed on either transport.

// -----------------------------------------------------------------------
// The shed-server API this command reads.
// -----------------------------------------------------------------------

// sessionsAPI answers the four endpoints `shed sessions` and `sessions kill`
// use: the shed list (which sheds to ask for tabs), one shed's sessions, every
// shed's sessions, and the session DELETE.
type sessionsAPI struct {
	*httptest.Server
	t        *testing.T
	sheds    []config.Shed
	sessions []config.Session
	// killed records every DELETE that reached the server, `<shed>/<session>`.
	killed []string
	// missing is the set of `<shed>/<session>` DELETEs answered 404, which is
	// how "there is no such tmux session" is staged.
	missing map[string]bool
	// sessionWarnings are returned alongside a per-shed session listing. The
	// real server DEGRADES rather than failing — a shed whose tmux could not
	// be reached answers with no rows and a warning — and staging that is the
	// only way to test what the destructive path does with an answer it cannot
	// trust.
	sessionWarnings []string
	// requests counts every HTTP request this server saw, of any kind — the
	// "zero calls" side of the --all/positional refusal test needs to prove
	// silence, not just the absence of one particular endpoint's traffic.
	requests int
}

func newSessionsAPI(t *testing.T, sheds []config.Shed, sessions []config.Session) *sessionsAPI {
	t.Helper()
	api := &sessionsAPI{t: t, sheds: sheds, sessions: sessions, missing: map[string]bool{}}
	api.Server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.Close)
	return api
}

func (a *sessionsAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.requests++
	w.Header().Set("Content-Type", "application/json")
	writeJSON := func(v any) {
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == "/api/sheds":
		writeJSON(config.ShedsResponse{Sheds: a.sheds})
	case r.URL.Path == "/api/sessions":
		writeJSON(config.SessionsResponse{Sessions: a.sessions})
	case strings.HasPrefix(r.URL.Path, "/api/sheds/"):
		rest := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sheds/"), "/")
		shed := rest[0]
		switch {
		// GET /api/sheds/<shed>
		case len(rest) == 1:
			for _, s := range a.sheds {
				if s.Name == shed {
					writeJSON(s)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			writeJSON(map[string]string{"error": "no such shed"})
		// GET /api/sheds/<shed>/sessions
		case len(rest) == 2 && rest[1] == "sessions":
			var rows []config.Session
			for _, s := range a.sessions {
				if s.ShedName == shed {
					rows = append(rows, s)
				}
			}
			writeJSON(config.SessionsResponse{Sessions: rows, Warnings: a.sessionWarnings})
		// DELETE /api/sheds/<shed>/sessions/<session>
		case len(rest) == 3 && rest[1] == "sessions" && r.Method == http.MethodDelete:
			key := shed + "/" + rest[2]
			if a.missing[key] {
				w.WriteHeader(http.StatusNotFound)
				writeJSON(map[string]string{"error": "session not found"})
				return
			}
			a.killed = append(a.killed, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			a.t.Errorf("unexpected API request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		a.t.Errorf("unexpected API request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// -----------------------------------------------------------------------
// The rig.
// -----------------------------------------------------------------------

const (
	testSessionsServer = "mini3"
	testSessionsShed   = "myproj"
)

// sessionsRig wires one `shed sessions` run to all three fixtures: the local
// app (the shim), the far side (the fake shed), and the server API.
type sessionsRig struct {
	shim   *roostShim
	shed   *fakeShed
	api    *sessionsAPI
	errOut *bytes.Buffer
	roost  *roostSessions
}

// newSessionsRig installs a roost app that answers the GATE, a far side
// reporting `tabs`, and an API serving one running shed plus `sessions`.
func newSessionsRig(t *testing.T, tabs []roostprovider.Tab, sessions []config.Session) *sessionsRig {
	t.Helper()
	resetSessionsState(t)

	rig := &sessionsRig{
		shim:   newAvailableShim(t),
		shed:   newFakeShed(t, tabs, true),
		api:    newSessionsAPI(t, []config.Shed{{Name: testSessionsShed, Status: config.StatusRunning}}, sessions),
		errOut: &bytes.Buffer{},
	}
	rig.roost = &roostSessions{
		ctl:     &roostctl.Client{},
		sshBin:  rig.shed.sshBin,
		errOut:  rig.errOut,
		perShed: defaultRoostTabsPerShed,
		workers: defaultRoostTabsWorkers,
	}
	newRoostSessions = func() *roostSessions { return rig.roost }
	clientConfig = &config.ClientConfig{
		DefaultServer: testSessionsServer,
		Servers: map[string]config.ServerEntry{
			testSessionsServer: {Host: "mini3", APIURL: rig.api.URL, SSHPort: 2222},
		},
		Sheds: map[string]config.ShedCache{},
	}
	return rig
}

// resetSessionsState isolates a test from the sessions commands' package-level
// state — the flags, the config, the roost seam and SHED_ATTACH — and starts
// it from the defaults a fresh `shed sessions` would see.
func resetSessionsState(t *testing.T) {
	t.Helper()
	all, tmux, killTmux, killTab := sessionsAllFlag, sessionsTmuxFlag, sessionsKillTmuxFlag, sessionsKillTabFlag
	asJSON, cfg, server, newRoost := jsonFlag, clientConfig, serverFlag, newRoostSessions
	t.Cleanup(func() {
		sessionsAllFlag, sessionsTmuxFlag, sessionsKillTmuxFlag, sessionsKillTabFlag = all, tmux, killTmux, killTab
		jsonFlag, clientConfig, serverFlag, newRoostSessions = asJSON, cfg, server, newRoost
	})
	sessionsAllFlag, sessionsTmuxFlag, sessionsKillTmuxFlag, sessionsKillTabFlag = false, false, false, ""
	jsonFlag, serverFlag = false, ""
	t.Setenv("SHED_ATTACH", "")
}

// runSessionsCapturing runs the command with stdout captured.
func runSessionsCapturing(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	restore := redirectStdout(t, &out)
	err := runSessions(sessionsCmd, args)
	restore()
	return out.String(), err
}

// runSessionsKillCapturing runs the kill command with stdout captured.
func runSessionsKillCapturing(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	restore := redirectStdout(t, &out)
	err := runSessionsKill(sessionsKillCmd, args)
	restore()
	return out.String(), err
}

// tmuxRow is one tmux session row, stamped far enough in the past that
// formatTimeAgo renders a stable "N hours ago" whatever hour the suite runs at.
func tmuxRow(name string, windows int) config.Session {
	return config.Session{
		Name:        name,
		ShedName:    testSessionsShed,
		CreatedAt:   time.Now().Add(-2 * time.Hour),
		Attached:    false,
		WindowCount: windows,
	}
}

// roostTab is one far-side tab.
func roostTab(id, title, state, agent string) roostprovider.Tab {
	return roostprovider.Tab{
		ID: id, ProjectID: "1", Title: title, Cwd: "/home/shed/myproj",
		State: state, AgentLifecycle: agent,
		CreatedAt: time.Now().Add(-3 * time.Hour).Unix(),
	}
}

// -----------------------------------------------------------------------
// The gate, and the tmux floor behind it.
// -----------------------------------------------------------------------

// TestSessionsTmuxFloorIsUntouched is the floor's whole contract: with a roost
// app running and plenty of tabs to find, `--tmux` and `SHED_ATTACH=tmux`
// produce the listing `shed sessions` has always produced — and do no roost
// work at all.
//
// **The "no roost work" half is the point a table comparison alone would
// miss.** An implementation that listed the tabs and then threw them away
// would print the right table while ssh'ing to every shed in the fleet on
// every listing, which for a person who asked for tmux is a pause they never
// agreed to. So both transports are asserted silent: the local app's argv log
// is empty, and the far side saw no request line.
func TestSessionsTmuxFloorIsUntouched(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T)
	}{
		{name: "--tmux", setup: func(t *testing.T) { sessionsTmuxFlag = true }},
		{name: "SHED_ATTACH=tmux", setup: func(t *testing.T) { t.Setenv("SHED_ATTACH", "tmux") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSessionsRig(t,
				[]roostprovider.Tab{roostTab("5", "default", "running", "working")},
				[]config.Session{tmuxRow("default", 1)})
			tc.setup(t)

			got, err := runSessionsCapturing(t)
			if err != nil {
				t.Fatalf("runSessions: %v", err)
			}

			// Today's table, rendered by today's renderer from the same rows.
			var want bytes.Buffer
			restore := redirectStdout(t, &want)
			if err := printSessionsTable([]config.Session{tmuxRow("default", 1)}); err != nil {
				restore()
				t.Fatalf("printSessionsTable: %v", err)
			}
			restore()
			if got != want.String() {
				t.Errorf("the floor's table changed:\n got %q\nwant %q", got, want.String())
			}
			if strings.Contains(got, "TAB") {
				t.Errorf("the floor printed a roost column:\n%s", got)
			}

			if runs := rig.shim.argvRuns(); len(runs) != 0 {
				t.Errorf("the tmux floor was asked for, but roostctl ran %d times: %v", len(runs), runs)
			}
			if lines := rig.shed.requestLines(); len(lines) != 0 {
				t.Errorf("the tmux floor was asked for, but the far side saw %d requests: %v", len(lines), lines)
			}
			if rig.errOut.Len() != 0 {
				t.Errorf("the floor warned: %q", rig.errOut.String())
			}
		})
	}
}

// TestSessionsNoLocalRoostIsTheFloor: the OTHER way the gate declines — a
// machine with no roost app at all — is the same listing, and still asks the
// far side nothing.
func TestSessionsNoLocalRoostIsTheFloor(t *testing.T) {
	rig := newSessionsRig(t,
		[]roostprovider.Tab{roostTab("5", "default", "running", "working")},
		[]config.Session{tmuxRow("default", 1)})
	// A roostctl that is not there: Available() is false, and nothing on the
	// developer's own machine can make it true.
	rig.roost.ctl = &roostctl.Client{Bin: filepath.Join(t.TempDir(), "no-such-roostctl")}

	got, err := runSessionsCapturing(t)
	if err != nil {
		t.Fatalf("runSessions: %v", err)
	}
	if !strings.Contains(got, "SESSION") || strings.Contains(got, "TAB") {
		t.Errorf("want the tmux floor's table, got:\n%s", got)
	}
	if lines := rig.shed.requestLines(); len(lines) != 0 {
		t.Errorf("no local roost, but the far side saw %d requests: %v", len(lines), lines)
	}
}

// -----------------------------------------------------------------------
// The merged listing.
// -----------------------------------------------------------------------

// TestSessionsMergesTabsAboveTmuxRows is the product rule rendered: with a
// local roost, a shed's roost tabs come first and its tmux rows follow, each
// under its own header.
func TestSessionsMergesTabsAboveTmuxRows(t *testing.T) {
	rig := newSessionsRig(t, []roostprovider.Tab{
		roostTab("5", "default", "running", "working"),
		roostTab("7", "/home/shed", "idle", ""),
	}, []config.Session{tmuxRow("default", 2)})

	got, err := runSessionsCapturing(t)
	if err != nil {
		t.Fatalf("runSessions: %v", err)
	}
	if rig.errOut.Len() != 0 {
		t.Errorf("unexpected warning: %q", rig.errOut.String())
	}

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("want 6 lines (roost header, two tabs, blank, tmux header, one row), got:\n%s", got)
	}
	if !strings.HasPrefix(lines[0], "SHED") || !strings.Contains(lines[0], "TAB") ||
		!strings.Contains(lines[0], "AGENT") || !strings.Contains(lines[0], "CWD") {
		t.Errorf("roost header = %q", lines[0])
	}
	for _, want := range []string{"myproj", "default", "running", "working", "/home/shed/myproj", "3 hours ago"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("roost row %q is missing %q", lines[1], want)
		}
	}
	// A tab shed did not open shows its cwd as the title and has no agent —
	// that is roost's own shape, rendered as it comes (a `-` for the empty
	// lifecycle so the row keeps its columns).
	if !strings.Contains(lines[2], "/home/shed") || !strings.Contains(lines[2], "idle") ||
		!strings.Contains(lines[2], "-") {
		t.Errorf("untitled-tab row = %q", lines[2])
	}
	if strings.TrimSpace(lines[3]) != "" {
		t.Errorf("want a blank line between the sections, got %q", lines[3])
	}
	if !strings.Contains(lines[4], "SESSION") || !strings.Contains(lines[4], "WINDOWS") {
		t.Errorf("tmux header = %q", lines[4])
	}
	if !strings.Contains(lines[5], "detached") || !strings.Contains(lines[5], "2") {
		t.Errorf("tmux row = %q", lines[5])
	}

	// The far side was asked exactly once, and only to list.
	if n := rig.shed.countRequests("tab.list"); n != 1 {
		t.Errorf("tab.list requests = %d, want 1", n)
	}
	if lines := rig.shed.requestLines(); len(lines) != 1 {
		t.Errorf("a listing should be one round trip, got %d: %v", len(lines), lines)
	}
}

// TestSessionsUnreachableShedDegrades is the degradation rule: a shed that
// cannot be asked contributes its tmux rows and ONE warning, and the command
// still succeeds.
//
// Exit 0 is the part worth stating twice. A fleet listing that failed because
// one shed was asleep would make `shed sessions` unusable on any machine with
// a stopped VPN — the tmux rows are still true, and the warning is how the
// user learns the roost half is missing rather than empty.
func TestSessionsUnreachableShedDegrades(t *testing.T) {
	rig := newSessionsRig(t, nil, []config.Session{tmuxRow("default", 1)})
	rig.roost.sshBin = unreachableSSH(t)

	got, err := runSessionsCapturing(t)
	if err != nil {
		t.Fatalf("an unreachable shed must not fail the listing: %v", err)
	}
	if !strings.Contains(got, "SESSION") || strings.Contains(got, "TAB") {
		t.Errorf("want the tmux rows alone, got:\n%s", got)
	}

	warnings := strings.Split(strings.TrimRight(rig.errOut.String(), "\n"), "\n")
	if len(warnings) != 1 {
		t.Fatalf("want exactly one warning line, got %d: %q", len(warnings), rig.errOut.String())
	}
	for _, want := range []string{"Warning:", testSessionsShed, "roost tabs", "Connection refused"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("warning %q is missing %q", warnings[0], want)
		}
	}
}

// unreachableSSH is an `ssh` that fails the way a shed behind a dead network
// does: ssh's own 255, with its own message on stderr.
func unreachableSSH(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh")
	writeTestFile(t, path, "#!/bin/sh\n"+
		`printf '%s\n' "ssh: connect to host mini3 port 2222: Connection refused" >&2`+"\nexit 255\n", 0o755)
	return path
}

// TestMergeSessionsOrdersByServerThenShed pins the listing's order, which is
// what makes two runs of the same fleet print the same table (and what makes
// the --json golden reproducible).
func TestMergeSessionsOrdersByServerThenShed(t *testing.T) {
	tabs := []shedTabs{
		{shed: roostprovider.RunningShed{Name: "web", Server: "mini4"}, tabs: []roostprovider.Tab{{ID: "1"}}},
		{shed: roostprovider.RunningShed{Name: "api", Server: "mini3"}, tabs: []roostprovider.Tab{{ID: "2"}}},
	}
	sessions := []config.Session{
		{Name: "default", ShedName: "zeta", ServerName: "mini3"},
		{Name: "default", ShedName: "api", ServerName: "mini3"},
	}
	got := mergeSessions(tabs, sessions)
	var order []string
	for _, listing := range got {
		order = append(order, listing.server+"/"+listing.shed)
	}
	want := []string{"mini3/api", "mini3/zeta", "mini4/web"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
	// The api listing carries both halves: one tab and one session.
	if len(got[0].tabs) != 1 || len(got[0].tmux) != 1 {
		t.Errorf("mini3/api = %+v, want one tab and one session", got[0])
	}
}

// -----------------------------------------------------------------------
// --json.
// -----------------------------------------------------------------------

// sessionsJSONGolden is testdata/sessions_json.golden.json.
type sessionsJSONGolden struct {
	Header    []string               `json:"_header"`
	Scenarios []sessionsJSONScenario `json:"scenarios"`
}

type sessionsJSONScenario struct {
	Name     string                `json:"name"`
	Doc      string                `json:"doc"`
	Listings []sessionsJSONListing `json:"listings"`
	WantJSON json.RawMessage       `json:"want_json"`
}

type sessionsJSONListing struct {
	Server string              `json:"server"`
	Shed   string              `json:"shed"`
	Tabs   []roostprovider.Tab `json:"tabs"`
	Tmux   []config.Session    `json:"tmux"`
}

// TestSessionsJSONGolden pins the `--json` array: the flat shape, every key
// spelling, and the order the two sources appear in.
//
// The fixture carries the INPUT as well as the output, so a reader can see
// what produced each row without running anything — and so a change in the
// merge's order fails here rather than being silently re-recorded.
func TestSessionsJSONGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "sessions_json.golden.json"))
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	var golden sessionsJSONGolden
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("parsing the golden: %v", err)
	}
	if len(golden.Scenarios) == 0 {
		t.Fatal("the golden has no scenarios")
	}

	for _, scenario := range golden.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			listings := make([]shedListing, 0, len(scenario.Listings))
			for _, row := range scenario.Listings {
				listings = append(listings, shedListing{
					server: row.Server, shed: row.Shed, tabs: row.Tabs, tmux: row.Tmux,
				})
			}
			got, err := json.Marshal(mergedSessionRows(listings))
			if err != nil {
				t.Fatalf("marshalling the rows: %v", err)
			}
			// Compacted, not re-indented: compacting keeps every key, every
			// key's ORDER and every value, and drops only the whitespace the
			// fixture is pretty-printed with. A renamed, dropped, added or
			// reordered field fails here.
			var want bytes.Buffer
			if err := json.Compact(&want, scenario.WantJSON); err != nil {
				t.Fatalf("compacting want_json: %v", err)
			}
			if string(got) != want.String() {
				t.Errorf("--json document changed:\n got %s\nwant %s", got, want.String())
			}
		})
	}
}

// TestSessionsJSONFloorHasNoSourceKey: on the floor, `--json` is byte-for-byte
// what it has always been — a bare `config.Session` array with no `source` and
// no `roost`. A consumer that upgrades shed and keeps `--tmux` sees nothing
// change at all.
func TestSessionsJSONFloorHasNoSourceKey(t *testing.T) {
	rig := newSessionsRig(t,
		[]roostprovider.Tab{roostTab("5", "default", "running", "working")},
		[]config.Session{tmuxRow("default", 1)})
	sessionsTmuxFlag, jsonFlag = true, true

	got, err := runSessionsCapturing(t)
	if err != nil {
		t.Fatalf("runSessions: %v", err)
	}
	if strings.Contains(got, `"source"`) || strings.Contains(got, `"roost"`) {
		t.Errorf("the floor's JSON grew a key:\n%s", got)
	}
	var rows []config.Session
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("the floor's JSON is not a Session array: %v\n%s", err, got)
	}
	if len(rows) != 1 || rows[0].Name != "default" {
		t.Errorf("rows = %+v", rows)
	}
	if lines := rig.shed.requestLines(); len(lines) != 0 {
		t.Errorf("the floor asked the far side %d things: %v", len(lines), lines)
	}
}

// -----------------------------------------------------------------------
// `sessions kill`.
// -----------------------------------------------------------------------

// TestSessionsKillClosesTheRoostTab: one tab of that title and no tmux session
// of that name is an unambiguous match, and it is closed with `tab.close`.
func TestSessionsKillClosesTheRoostTab(t *testing.T) {
	rig := newSessionsRig(t,
		[]roostprovider.Tab{roostTab("7", "debug", "running", "waiting")},
		[]config.Session{tmuxRow("default", 1)})

	out, err := runSessionsKillCapturing(t, testSessionsShed, "debug")
	if err != nil {
		t.Fatalf("runSessionsKill: %v", err)
	}
	line := rig.shed.requestFor("tab.close")
	if !strings.Contains(line, `"tab_id":"7"`) {
		t.Errorf("tab.close request = %s", line)
	}
	if len(rig.api.killed) != 0 {
		t.Errorf("a roost tab was named, but the server was asked to kill %v", rig.api.killed)
	}
	if !strings.Contains(out, "Closed roost tab") || !strings.Contains(out, "debug") ||
		!strings.Contains(out, "7") {
		t.Errorf("output = %q", out)
	}
}

// TestSessionsKillRefusesAmbiguity is the refusal, in both shapes it has: two
// tabs of a title (which `--new` legitimately creates), and a tab plus a tmux
// session of one name.
//
// What is asserted is not only the failure: NOTHING is closed or killed. A
// refusal that had already acted would be the worst of both.
func TestSessionsKillRefusesAmbiguity(t *testing.T) {
	t.Run("two tabs of one title", func(t *testing.T) {
		rig := newSessionsRig(t, []roostprovider.Tab{
			roostTab("5", "default", "running", "working"),
			roostTab("9", "default", "idle", "inactive"),
		}, nil)

		_, err := runSessionsKillCapturing(t, testSessionsShed, "default")
		if err == nil {
			t.Fatal("two tabs of one title must refuse")
		}
		for _, want := range []string{"2 roost tabs", `"default"`, "5", "9", "--tab <id>"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q is missing %q", err, want)
			}
		}
		if n := rig.shed.countRequests("tab.close"); n != 0 {
			t.Errorf("the refusal closed %d tabs", n)
		}
		if len(rig.api.killed) != 0 {
			t.Errorf("the refusal killed %v", rig.api.killed)
		}
	})

	t.Run("a tab and a tmux session of one name", func(t *testing.T) {
		rig := newSessionsRig(t,
			[]roostprovider.Tab{roostTab("5", "default", "running", "working")},
			[]config.Session{tmuxRow("default", 1)})

		_, err := runSessionsKillCapturing(t, testSessionsShed, "default")
		if err == nil {
			t.Fatal("a tab and a tmux session of one name must refuse")
		}
		for _, want := range []string{"a roost tab", "tab 5", "tmux session", "--tab <id>", "--tmux"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q is missing %q", err, want)
			}
		}
		if n := rig.shed.countRequests("tab.close"); n != 0 {
			t.Errorf("the refusal closed %d tabs", n)
		}
		if len(rig.api.killed) != 0 {
			t.Errorf("the refusal killed %v", rig.api.killed)
		}
	})
}

// TestSessionsKillDisambiguators: `--tab <id>` picks one of the tabs the
// refusal named, and `--tmux` picks the tmux session — the two remedies the
// refusal advertises actually work.
func TestSessionsKillDisambiguators(t *testing.T) {
	t.Run("--tab picks one tab", func(t *testing.T) {
		rig := newSessionsRig(t, []roostprovider.Tab{
			roostTab("5", "default", "running", "working"),
			roostTab("9", "default", "idle", "inactive"),
		}, []config.Session{tmuxRow("default", 1)})
		sessionsKillTabFlag = "9"

		if _, err := runSessionsKillCapturing(t, testSessionsShed, "default"); err != nil {
			t.Fatalf("runSessionsKill: %v", err)
		}
		if line := rig.shed.requestFor("tab.close"); !strings.Contains(line, `"tab_id":"9"`) {
			t.Errorf("tab.close request = %s", line)
		}
		if len(rig.api.killed) != 0 {
			t.Errorf("--tab killed a tmux session: %v", rig.api.killed)
		}
	})

	t.Run("--tmux picks the tmux session", func(t *testing.T) {
		rig := newSessionsRig(t,
			[]roostprovider.Tab{roostTab("5", "default", "running", "working")},
			[]config.Session{tmuxRow("default", 1)})
		sessionsKillTmuxFlag = true

		if _, err := runSessionsKillCapturing(t, testSessionsShed, "default"); err != nil {
			t.Fatalf("runSessionsKill: %v", err)
		}
		if got := strings.Join(rig.api.killed, ","); got != testSessionsShed+"/default" {
			t.Errorf("killed = %q", got)
		}
		if runs := rig.shim.argvRuns(); len(runs) != 0 {
			t.Errorf("--tmux ran roostctl %d times: %v", len(runs), runs)
		}
		if lines := rig.shed.requestLines(); len(lines) != 0 {
			t.Errorf("--tmux asked the far side %d things: %v", len(lines), lines)
		}
	})

	t.Run("--tab and --tmux together are refused", func(t *testing.T) {
		newSessionsRig(t, nil, nil)
		sessionsKillTabFlag, sessionsKillTmuxFlag = "9", true
		_, err := runSessionsKillCapturing(t, testSessionsShed, "default")
		if err == nil || !strings.Contains(err.Error(), "pass only one") {
			t.Fatalf("err = %v, want a refusal naming both flags", err)
		}
	})

	t.Run("--tab naming no tab of that title is an error", func(t *testing.T) {
		rig := newSessionsRig(t,
			[]roostprovider.Tab{roostTab("5", "default", "running", "working")},
			nil)
		sessionsKillTabFlag = "42"
		_, err := runSessionsKillCapturing(t, testSessionsShed, "default")
		if err == nil || !strings.Contains(err.Error(), "no roost tab 42") {
			t.Fatalf("err = %v, want a refusal naming the tab id", err)
		}
		if n := rig.shed.countRequests("tab.close"); n != 0 {
			t.Errorf("a bad --tab closed %d tabs", n)
		}
	})
}

// TestSessionsKillFallsBackToTmux: a name no roost tab carries is the tmux
// path, unchanged — including the error the server produces when there is no
// such session either.
func TestSessionsKillFallsBackToTmux(t *testing.T) {
	t.Run("no tab of that title kills the tmux session", func(t *testing.T) {
		rig := newSessionsRig(t,
			[]roostprovider.Tab{roostTab("5", "default", "running", "working")},
			[]config.Session{tmuxRow("build", 1)})

		out, err := runSessionsKillCapturing(t, testSessionsShed, "build")
		if err != nil {
			t.Fatalf("runSessionsKill: %v", err)
		}
		if got := strings.Join(rig.api.killed, ","); got != testSessionsShed+"/build" {
			t.Errorf("killed = %q", got)
		}
		if !strings.Contains(out, `Killed session "build"`) {
			t.Errorf("output = %q", out)
		}
	})

	t.Run("no match at all keeps today's error", func(t *testing.T) {
		rig := newSessionsRig(t, nil, nil)
		rig.api.missing[testSessionsShed+"/ghost"] = true

		_, err := runSessionsKillCapturing(t, testSessionsShed, "ghost")
		if err == nil || !strings.HasPrefix(err.Error(), "failed to kill session") {
			t.Fatalf("err = %v, want today's `failed to kill session` error", err)
		}
	})

	t.Run("an unreachable shed warns once and falls back", func(t *testing.T) {
		rig := newSessionsRig(t, nil, []config.Session{tmuxRow("default", 1)})
		rig.roost.sshBin = unreachableSSH(t)

		if _, err := runSessionsKillCapturing(t, testSessionsShed, "default"); err != nil {
			t.Fatalf("runSessionsKill: %v", err)
		}
		if got := strings.Join(rig.api.killed, ","); got != testSessionsShed+"/default" {
			t.Errorf("killed = %q", got)
		}
		warnings := strings.Split(strings.TrimRight(rig.errOut.String(), "\n"), "\n")
		if len(warnings) != 1 || !strings.Contains(warnings[0], "roost tabs") {
			t.Errorf("want exactly one warning about the tabs, got %q", rig.errOut.String())
		}
	})
}

// TestKillAmbiguityMessageShapes pins the refusal's two sentences directly, so
// the copy is readable in one place rather than inferred from substrings.
func TestKillAmbiguityMessageShapes(t *testing.T) {
	two := []roostprovider.Tab{{ID: "5", Title: "default"}, {ID: "9", Title: "default"}}
	one := two[:1]

	cases := []struct {
		name    string
		tabs    []roostprovider.Tab
		hasTmux bool
		want    string
	}{
		{
			name: "two tabs",
			tabs: two,
			want: `shed "myproj" has 2 roost tabs titled "default" (tabs 5, 9); pass --tab <id> to close a tab`,
		},
		{
			name:    "a tab and a session",
			tabs:    one,
			hasTmux: true,
			want: `shed "myproj" has a roost tab titled "default" (tab 5) and a tmux session called "default"; ` +
				`pass --tab <id> to close a tab, or --tmux to kill the tmux session`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := killAmbiguityMessage("myproj", "default", tc.tabs, tc.hasTmux); got != tc.want {
				t.Errorf("message =\n %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestSessionsWantsTmuxReadsTheEnvironment pins that `shed sessions` reads the
// same one escape hatch `shed attach` does, in the same spellings.
func TestSessionsWantsTmuxReadsTheEnvironment(t *testing.T) {
	cases := []struct {
		env  string
		flag bool
		want bool
	}{
		{env: "", flag: false, want: false},
		{env: "", flag: true, want: true},
		{env: "tmux", want: true},
		{env: "TMUX", want: true},
		{env: " tmux ", want: true},
		{env: "roost", want: false},
		{env: "1", want: false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("env=%q flag=%v", tc.env, tc.flag), func(t *testing.T) {
			resetSessionsState(t)
			t.Setenv("SHED_ATTACH", tc.env)
			sessionsTmuxFlag = tc.flag
			if got := sessionsWantsTmux(); got != tc.want {
				t.Errorf("sessionsWantsTmux() = %v, want %v", got, tc.want)
			}
			sessionsKillTmuxFlag = tc.flag
			if got := sessionsKillWantsTmux(); got != tc.want {
				t.Errorf("sessionsKillWantsTmux() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestKillRefusesWhenTheTmuxAnswerCannotBeTrusted: `GET /sessions` degrades
// rather than failing — a shed whose tmux is unreachable answers with NO ROWS
// and a warning. Reading that as "there is no tmux session of this name" would
// close a roost tab while a same-named tmux session sat behind an unread
// warning, which is exactly the ambiguity the refusal exists to prevent.
//
// sol review finding: an incomplete response was being taken as proof of
// absence on the one path in this command that destroys something.
func TestKillRefusesWhenTheTmuxAnswerCannotBeTrusted(t *testing.T) {
	rig := newSessionsRig(t, []roostprovider.Tab{
		roostTab("5", "default", "running", "inactive"),
	}, nil)
	// One tab, no tmux rows — but the server could not actually look.
	rig.api.sessionWarnings = []string{"tmux unavailable in shed p022: dial: connection refused"}

	_, err := runSessionsKillCapturing(t, testSessionsShed, "default")
	if err == nil {
		t.Fatal("a tab must not be closed on the strength of an answer the server did not give")
	}
	for _, want := range []string{"could not be determined", "--tab 5", "--tmux"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q: %v", want, err)
		}
	}
	if n := rig.shed.countRequests("tab.close"); n != 0 {
		t.Errorf("tab.close was sent %d times despite the refusal", n)
	}
}

// TestKillProceedsWhenTheTmuxAnswerIsComplete is the other half: the same
// shape WITHOUT warnings is a real "no such tmux session", and the single
// matching tab is closed.
func TestKillProceedsWhenTheTmuxAnswerIsComplete(t *testing.T) {
	rig := newSessionsRig(t, []roostprovider.Tab{
		roostTab("5", "default", "running", "inactive"),
	}, nil)

	if _, err := runSessionsKillCapturing(t, testSessionsShed, "default"); err != nil {
		t.Fatalf("runSessionsKill: %v", err)
	}
	if n := rig.shed.countRequests("tab.close"); n != 1 {
		t.Errorf("tab.close sent %d times, want 1", n)
	}
}

// TestAllWithPositionalRefused replaces TestAllIgnoresThePositionalInBothHalves.
//
// sol review finding: `--all` made the tmux half call ListAllSessions, which
// never looked at a positional shed argument, while the roost half filtered
// on it — so `shed sessions --all web` showed EVERY shed's tmux rows but only
// `web`'s roost tabs, one command answering two different questions at once.
//
// The fix is refusal, in cobra's Args validation on sessionsCmd, before
// either half of the command runs at all: `--all` together with a positional
// is rejected up front, so the combination that used to answer two different
// questions now answers none. This drives the real cobra pipeline
// (rootCmd.Execute, not runSessions directly) precisely so that the ordering
// -- flags parsed, Args validated, and only THEN would PersistentPreRunE and
// RunE ever touch the API client or roostctl -- is exercised for real, not
// asserted by reading the source.
func TestAllWithPositionalRefused(t *testing.T) {
	const wantMsg = "--all lists every shed; drop the argument or drop --all"

	for _, tc := range []struct {
		name string
		args []string
	}{
		// The default path: no --json, no --tmux, whatever runSessions would
		// otherwise have picked based on roost availability.
		{name: "default", args: []string{"sessions", "--all", testSessionsShed}},
		// The JSON output path.
		{name: "json", args: []string{"sessions", "--all", testSessionsShed, "--json"}},
		// The tmux-only path.
		{name: "tmux", args: []string{"sessions", "--all", testSessionsShed, "--tmux"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSessionsRig(t, []roostprovider.Tab{
				roostTab("5", "default", "running", "inactive"),
			}, nil)
			rig.api.sheds = append(rig.api.sheds, config.Shed{Name: "other", Status: config.StatusRunning})

			// rootCmd is package-level: leave its argv the way we found it so a
			// later Execute() without SetArgs does not replay this refusal
			// (sol review finding).
			rootCmd.SetArgs(tc.args)
			t.Cleanup(func() { rootCmd.SetArgs(nil) })
			err := rootCmd.Execute()

			if err == nil {
				t.Fatal("Execute() returned nil error, want the --all/positional refusal")
			}
			if !strings.Contains(err.Error(), wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), wantMsg)
			}
			if n := rig.api.requests; n != 0 {
				t.Errorf("the fake API client saw %d requests despite the refusal", n)
			}
			if runs := rig.shim.argvRuns(); len(runs) != 0 {
				t.Errorf("the roostctl shim saw %d invocations despite the refusal: %v", len(runs), runs)
			}
		})
	}
}
