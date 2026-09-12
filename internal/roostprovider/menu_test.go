package roostprovider

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// rowTitles is a menu's row titles in order.
func rowTitles(m Menu) []string {
	titles := make([]string, len(m.Items))
	for i, r := range m.Items {
		titles[i] = r.Title
	}
	return titles
}

func rowByTitle(t *testing.T, m Menu, title string) Row {
	t.Helper()
	for _, r := range m.Items {
		if r.Title == title {
			return r
		}
	}
	t.Fatalf("no row titled %q in %+v", title, m.Items)
	return Row{}
}

func TestListMenuSheds(t *testing.T) {
	inv := ShedInventory{
		Sheds: []RunningShed{
			{Name: "dev", Server: "my-server", LandingDir: "/home/shed/proj"},
			{Name: "bare", Server: "my-server"},
		},
		Tried:    []string{"my-server"},
		Answered: []string{"my-server"},
	}
	m := ListMenu(inv, nil)

	if m.Placeholder != "Start an agent on…" {
		t.Errorf("placeholder = %q", m.Placeholder)
	}
	dev := rowByTitle(t, m, "shed: dev")
	if dev.Subtitle != "my-server · /home/shed/proj" {
		t.Errorf("dev subtitle = %q", dev.Subtitle)
	}
	if dev.Actionable != nil {
		t.Errorf("an ordinary row must leave actionable absent, got %v", *dev.Actionable)
	}
	tok, err := ParseToken(dev.ID)
	if err != nil {
		t.Fatalf("dev row id does not parse: %v", err)
	}
	if tok != (Token{Shed: "dev", Server: "my-server"}) {
		t.Errorf("dev row token = %+v", tok)
	}

	// An older shed-server that reports no landing dir shows `~`, which is
	// also what the workdir step falls back to.
	if got := rowByTitle(t, m, "shed: bare").Subtitle; got != "my-server · ~" {
		t.Errorf("bare subtitle = %q", got)
	}
}

func TestListMenuMachines(t *testing.T) {
	machines := []config.MachineEntry{
		{Name: "plain", Host: "plain", SSHPort: 22},
		{Name: "full", Host: "mini2.local", User: "charliek", SSHPort: 2222},
		{Name: "porty", Host: "box", SSHPort: 2200},
		{Name: "usery", Host: "box2", User: "root", SSHPort: 22},
	}
	m := ListMenu(ShedInventory{}, machines)

	// `[user@]host[:port]` — each optional half omitted exactly where
	// shed-core's own ssh argv omits it, so the subtitle describes the
	// connection that will really be made rather than a normalized one.
	for _, tc := range []struct{ title, subtitle string }{
		{"machine: plain", "plain"},
		{"machine: full", "charliek@mini2.local:2222"},
		{"machine: porty", "box:2200"},
		{"machine: usery", "root@box2"},
	} {
		if got := rowByTitle(t, m, tc.title).Subtitle; got != tc.subtitle {
			t.Errorf("%s subtitle = %q, want %q", tc.title, got, tc.subtitle)
		}
	}

	tok, err := ParseToken(rowByTitle(t, m, "machine: full").ID)
	if err != nil {
		t.Fatalf("machine row id does not parse: %v", err)
	}
	if tok != (Token{Machine: "full"}) {
		t.Errorf("machine row token = %+v", tok)
	}
}

func TestListMenuShedsComeBeforeMachines(t *testing.T) {
	m := ListMenu(
		ShedInventory{Sheds: []RunningShed{{Name: "dev", Server: "srv"}}, Tried: []string{"srv"}, Answered: []string{"srv"}},
		[]config.MachineEntry{{Name: "mini2", Host: "mini2", SSHPort: 22}},
	)
	if len(m.Items) != 2 {
		t.Fatalf("want 2 rows, got %+v", m.Items)
	}
	if m.Items[0].Title != "shed: dev" || m.Items[1].Title != "machine: mini2" {
		t.Errorf("order = %q, %q", m.Items[0].Title, m.Items[1].Title)
	}
}

// TestListMenuEmptyRows pins both of §3.2's empty-menu rows, including the
// distinction between them: an unreachable fleet is a different problem from an
// idle one, and telling a user with a dead VPN that they have no sheds would be
// the wrong news.
func TestListMenuEmptyRows(t *testing.T) {
	tests := []struct {
		name     string
		inv      ShedInventory
		title    string
		subtitle string
	}{
		{
			name:     "servers configured, none answered",
			inv:      ShedInventory{Tried: []string{"my-server", "mini3"}},
			title:    "no shed-server answered",
			subtitle: "my-server, mini3",
		},
		{
			name:     "a server answered with nothing running",
			inv:      ShedInventory{Tried: []string{"my-server"}, Answered: []string{"my-server"}},
			title:    "No running sheds or machines",
			subtitle: "shed start <name>, or add a machines: entry to ~/.shed/config.yaml",
		},
		{
			name:     "no servers configured at all",
			inv:      ShedInventory{},
			title:    "No running sheds or machines",
			subtitle: "shed start <name>, or add a machines: entry to ~/.shed/config.yaml",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := ListMenu(tc.inv, nil)
			if len(m.Items) != 1 {
				t.Fatalf("want exactly one row, got %+v", m.Items)
			}
			assertNoneRow(t, m.Items[0], tc.title, tc.subtitle)
		})
	}
}

// TestListMenuUnreachableFleetStillListsMachines: the "no shed-server answered"
// row is an EMPTY-menu row. A `machines:` entry needs no server, so it still
// fills the menu and the row must not appear.
func TestListMenuUnreachableFleetStillListsMachines(t *testing.T) {
	m := ListMenu(
		ShedInventory{Tried: []string{"my-server"}},
		[]config.MachineEntry{{Name: "mini2", Host: "mini2", SSHPort: 22}},
	)
	if len(m.Items) != 1 || m.Items[0].Title != "machine: mini2" {
		t.Fatalf("want just the machine row, got %+v", m.Items)
	}
}

func assertNoneRow(t *testing.T, got Row, title, subtitle string) {
	t.Helper()
	if got.ID != NoneID {
		t.Errorf("id = %q, want %q", got.ID, NoneID)
	}
	if got.Actionable == nil || *got.Actionable {
		t.Errorf("row must be actionable:false, got %v", got.Actionable)
	}
	if got.Title != title {
		t.Errorf("title = %q, want %q", got.Title, title)
	}
	if got.Subtitle != subtitle {
		t.Errorf("subtitle = %q, want %q", got.Subtitle, subtitle)
	}
}

// assertRow compares a whole row against the constructor that should have
// produced it.
//
// This is how every test ABOUT A MAPPING asserts its answer — which error
// becomes which row, which far-side state becomes which error. Re-typing the
// pinned copy at each of those sites would put §3.2's strings in three files
// and make a sanctioned reword a three-file edit; the copy itself is pinned
// once, in TestPinnedNonActionableRows. Row is comparable and every
// non-actionable row shares one `&falseVal`, so `==` covers the id, the copy
// and the actionable flag together — a test that picked the wrong constructor
// still fails, which is the property these sites are for.
func assertRow(t *testing.T, got, want Row) {
	t.Helper()
	if got != want {
		t.Errorf("row:\n got %+v\nwant %+v", got, want)
	}
}

// assertSeq compares two slices element-wise.
//
// One helper instead of the per-file `assertArgv`/`assertNames`/`assertWorkdirs`
// trio plus four hand-written copies of the same loop: the algorithm was
// identical every time and only the failure message differed, so the copies
// mostly disagreed about how a failure reads.
func assertSeq[T comparable](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}

// TestPinnedNonActionableRows pins the exact copy of every row plan 019 §3.2
// specifies. These strings are the contract with the user; a reword is a
// deliberate change, not a refactor.
func TestPinnedNonActionableRows(t *testing.T) {
	shed := Token{Shed: "dev", Server: "my-server"}
	machine := Token{Machine: "mini2"}

	t.Run("not installed", func(t *testing.T) {
		assertNoneRow(t, NotInstalledRow(shed),
			"roost-session is not installed on my-server/dev",
			"connect from the shed desktop or mobile app to install it")
	})
	t.Run("not running", func(t *testing.T) {
		assertNoneRow(t, NotRunningRow(machine),
			"roost-session is not running on mini2",
			"connect from the shed app to start it, or run roost-session start there")
	})
	t.Run("protocol mismatch", func(t *testing.T) {
		assertNoneRow(t, ProtocolMismatchRow(machine, 2),
			"roost-session on mini2 speaks protocol 2; this shed speaks 4",
			"upgrade whichever is older")
	})
	t.Run("unreachable", func(t *testing.T) {
		assertNoneRow(t, UnreachableRow(machine, "ssh: connect to host mini2 port 22: Connection refused"),
			"mini2 is unreachable",
			"ssh: connect to host mini2 port 22: Connection refused")
	})
	t.Run("no local ssh", func(t *testing.T) {
		// The subtitle is derived from what ResolveSSH really searches, so it
		// is pinned here against the full list rather than against a sample.
		assertNoneRow(t, NoSSHRow(),
			"ssh is not installed where roost can see it",
			"$PATH, /usr/bin/ssh, /opt/homebrew/bin/ssh, /usr/local/bin/ssh")
	})
	t.Run("no agents", func(t *testing.T) {
		assertNoneRow(t, NoAgentsRow(shed),
			"no agents found on my-server/dev",
			"looked for claude, codex, cursor-agent, opencode, gx, grok under bash -lc")
	})
}

func TestAgentMenuRowsPerFoundAgent(t *testing.T) {
	host := Token{Shed: "dev", Server: "srv"}
	p := Probe{
		Home: "/home/shed",
		Found: map[string]string{
			"codex":     "/home/shed/.bun/bin/codex",
			"claude-rc": "/home/shed/.local/bin/claude",
			"gx":        "/home/shed/.local/bin/gx",
		},
	}
	m := AgentMenu(host, p)

	// Display order, not map order: claude, codex, …, gx.
	assertSeq(t, "titles", rowTitles(m), []string{"claude", "codex", "gx"})

	claude := rowByTitle(t, m, "claude")
	if claude.Subtitle != "/home/shed/.local/bin/claude" {
		t.Errorf("claude subtitle = %q", claude.Subtitle)
	}
	tok, err := ParseToken(claude.ID)
	if err != nil {
		t.Fatalf("claude row id does not parse: %v", err)
	}
	if tok != (Token{Shed: "dev", Server: "srv", Agent: "claude-rc", Home: "/home/shed"}) {
		t.Errorf("claude token = %+v", tok)
	}
}

func TestAgentMenuWithNothingFound(t *testing.T) {
	m := AgentMenu(Token{Machine: "mini2"}, Probe{Home: "/root", Found: map[string]string{}})
	if len(m.Items) != 1 {
		t.Fatalf("want one row, got %+v", m.Items)
	}
	assertNoneRow(t, m.Items[0], "no agents found on mini2",
		"looked for claude, codex, cursor-agent, opencode, gx, grok under bash -lc")
}

// TestAgentMenuHomeWithSpacesSurvivesTheRowId is the shape that breaks a naive
// id: the probed $HOME travels in the token so step 3 need not re-probe.
func TestAgentMenuHomeWithSpacesSurvivesTheRowId(t *testing.T) {
	m := AgentMenu(Token{Machine: "laptop"}, Probe{
		Home:  "/Users/First Last",
		Found: map[string]string{"codex": "/Users/First Last/.bun/bin/codex"},
	})
	tok, err := ParseToken(m.Items[0].ID)
	if err != nil {
		t.Fatalf("row id does not parse: %v", err)
	}
	if tok.Home != "/Users/First Last" {
		t.Errorf("home = %q", tok.Home)
	}
}

// TestMenuSerializesInRoostsSchema pins the JSON keys roost actually reads
// (`roost_ui_model::provider::ProviderOutputItem`), including the one that must
// be ABSENT on an ordinary row: roost reads `actionable` as "absent ⇒
// actionable", so emitting `true` everywhere would be noise and emitting the
// zero value would disable every row.
func TestMenuSerializesInRoostsSchema(t *testing.T) {
	m := Menu{
		Placeholder: "Start an agent on…",
		Items: []Row{
			{ID: "machine=mini2", Title: "machine: mini2", Subtitle: "mini2"},
			NoneRow("nope", "not this time"),
			{ID: "machine=bare", Title: "machine: bare"},
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"placeholder":"Start an agent on…","items":[` +
		`{"id":"machine=mini2","title":"machine: mini2","subtitle":"mini2"},` +
		`{"id":"_none","title":"nope","subtitle":"not this time","actionable":false},` +
		`{"id":"machine=bare","title":"machine: bare"}]}`
	if string(data) != want {
		t.Errorf("menu JSON:\n got %s\nwant %s", data, want)
	}
}

// TestEmptyMenuSerializesItemsAsAnArray: `items` has no omitempty, so a menu
// with no rows is `"items":[]` rather than a missing key roost would have to
// tolerate.
func TestEmptyMenuSerializesItemsAsAnArray(t *testing.T) {
	data, err := json.Marshal(Menu{Items: []Row{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != `{"items":[]}` {
		t.Errorf("empty menu = %s", data)
	}
}
