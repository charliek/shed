package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// attachTmuxGoldenPath is the committed fixture pinning the plain-tmux `shed
// attach` argv (plan 022 C3/C5). It is the tmux FLOOR: a later commit makes
// `shed attach` roost-native, but with no local roost, or with --tmux, or with
// SHED_ATTACH=tmux, the produced ssh/tmux argv must stay byte-identical to what
// this golden pins today. If this test goes red after a roost-native change,
// the fix is almost always in the new code, not in this golden — do not "fix"
// the fixture without first confirming the tmux floor path is still supposed to
// behave identically. See the fixture's own "_header" field for the full story.
const attachTmuxGoldenPath = "testdata/attach_tmux_argv.golden.json"

// attachTmuxGolden is the fixture's top-level shape: a human-readable header
// (JSON has no comment syntax) plus the scenario list.
type attachTmuxGolden struct {
	Header    []string                   `json:"_header"`
	Scenarios []attachTmuxGoldenScenario `json:"scenarios"`
}

// attachTmuxGoldenScenario is one scenario: a plain-attach invocation (as
// attachPlain's parameters) and the exact ssh argv it must produce via execSSH
// (the syscall.Exec seam).
type attachTmuxGoldenScenario struct {
	Name       string   `json:"name"`
	Doc        string   `json:"doc"`
	Session    string   `json:"session"`
	New        bool     `json:"new"`
	LandingDir string   `json:"landing_dir"`
	WantArgv   []string `json:"want_argv"`
}

// TestAttachPlainTmuxArgvGolden pins the exact ssh argv (and, embedded in its
// final element, the exact tmux command string) that attachPlain hands to
// execSSH for the plain-tmux paths: default session, a named session, --new,
// and a landing directory that needs shell-quoting. See attachTmuxGoldenPath's
// doc comment for why this must never silently "self-heal".
func TestAttachPlainTmuxArgvGolden(t *testing.T) {
	// Pin GetKnownHostsPath() (~/.shed/known_hosts, resolved from $HOME) to a
	// fixed value so the golden's UserKnownHostsFile= option is
	// machine-independent.
	t.Setenv("HOME", "/home/tester")

	data, err := os.ReadFile(attachTmuxGoldenPath)
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	var golden attachTmuxGolden
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("decoding golden fixture: %v", err)
	}
	if len(golden.Scenarios) == 0 {
		t.Fatal("golden fixture has no scenarios")
	}

	origSession, origNew := attachSessionFlag, attachNewFlag
	origExec := execSSH
	origConfig := clientConfig
	t.Cleanup(func() {
		attachSessionFlag, attachNewFlag = origSession, origNew
		execSSH = origExec
		clientConfig = origConfig
	})

	for _, sc := range golden.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			attachSessionFlag = sc.Session
			attachNewFlag = sc.New

			var captured []string
			execSSH = func(_ string, argv []string, _ []string) error {
				captured = argv
				return nil
			}

			entry := &config.ServerEntry{Host: "mini3", SSHPort: 2222}
			if sc.New {
				// --new lists existing sessions first (empty here, so the create
				// proceeds); backed by a fake server so no real network is hit.
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(config.SessionsResponse{})
				}))
				t.Cleanup(srv.Close)
				entry = &config.ServerEntry{Host: "mini3", APIURL: srv.URL, SSHPort: 2222}
				// listShedSessions (--new's existing-session check) resolves through
				// NewAPIClientFromNamedEntry, which verifies "myserver" against the
				// live clientConfig before it will use the name — wire up a matching
				// entry so that verification succeeds.
				clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{"myserver": *entry}}
			}

			shed := &config.Shed{LandingDir: sc.LandingDir}
			if err := attachPlain("myshed", "myserver", entry, shed); err != nil {
				t.Fatalf("attachPlain: %v", err)
			}

			if len(captured) != len(sc.WantArgv) {
				t.Fatalf("argv length = %d, want %d\n got:  %#v\n want: %#v", len(captured), len(sc.WantArgv), captured, sc.WantArgv)
			}
			for i := range sc.WantArgv {
				if captured[i] != sc.WantArgv[i] {
					t.Errorf("argv[%d] = %q, want %q\n got:  %#v\n want: %#v", i, captured[i], sc.WantArgv[i], captured, sc.WantArgv)
				}
			}
		})
	}
}
