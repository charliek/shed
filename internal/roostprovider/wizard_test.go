package roostprovider

import (
	"encoding/json"
	"testing"
)

func TestNextStep(t *testing.T) {
	tests := []struct {
		name string
		tok  Token
		want Step
	}{
		{"a bare host token", Token{Machine: "mini2"}, StepAgents},
		{"an agent but no workdir", Token{Machine: "mini2", Agent: "codex", Home: "/root"}, StepWorkdirs},
		{"a complete token", Token{Machine: "mini2", Agent: "codex", Home: "/root", Cwd: "/root/src"}, StepOpen},
		{"a project-bearing token", Token{Machine: "mini2", Agent: "gx", Home: "/root", Cwd: "/src", Project: "3"}, StepOpen},
		// A cwd with no agent cannot open anything — there would be no argv.
		// The agent step is the honest place to send it back to.
		{"a cwd with no agent", Token{Machine: "mini2", Cwd: "/root/src"}, StepAgents},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextStep(tc.tok); got != tc.want {
				t.Errorf("NextStep = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWorkdirCandidates(t *testing.T) {
	t.Run("zero projects, no landing dir", func(t *testing.T) {
		got := WorkdirCandidates(nil, "/home/shed", "", false)
		want := []Workdir{{Title: "Home", Cwd: "/home/shed"}}
		assertSeq(t, "candidates", got, want)
	})

	t.Run("zero projects, a landing dir that exists", func(t *testing.T) {
		got := WorkdirCandidates(nil, "/home/shed", "/home/shed/proj", true)
		want := []Workdir{
			{Title: "Home", Cwd: "/home/shed"},
			{Title: "Landing dir", Cwd: "/home/shed/proj"},
		}
		assertSeq(t, "candidates", got, want)
	})

	// A landing dir the probe could not find is DROPPED, not offered: a tab
	// opened in a missing cwd dies at the PTY, which is worse than one fewer
	// row.
	t.Run("a landing dir that does not exist is dropped", func(t *testing.T) {
		got := WorkdirCandidates(nil, "/home/shed", "/home/shed/gone", false)
		assertSeq(t, "candidates", got, []Workdir{{Title: "Home", Cwd: "/home/shed"}})
	})

	t.Run("several projects, in far-side order, before Home", func(t *testing.T) {
		projects := []Project{
			{ID: "1", Name: "roost", Cwd: "/home/shed/roost"},
			{ID: "2", Name: "shed", Cwd: "/home/shed/shed"},
		}
		got := WorkdirCandidates(projects, "/home/shed", "/home/shed/proj", true)
		assertSeq(t, "candidates", got, []Workdir{
			{Title: "roost", Cwd: "/home/shed/roost", ProjectID: "1"},
			{Title: "shed", Cwd: "/home/shed/shed", ProjectID: "2"},
			{Title: "Home", Cwd: "/home/shed"},
			{Title: "Landing dir", Cwd: "/home/shed/proj"},
		})
	})

	// Dedup by cwd, projects winning. A project carries an id, so keeping it
	// files the tab in that project instead of roost's default one — and
	// without the dedup the one-candidate collapse below would never fire for
	// a session whose only project sits at $HOME.
	t.Run("a project at $HOME absorbs the Home row", func(t *testing.T) {
		projects := []Project{{ID: "4", Name: "home", Cwd: "/home/shed"}}
		got := WorkdirCandidates(projects, "/home/shed", "", false)
		assertSeq(t, "candidates", got, []Workdir{{Title: "home", Cwd: "/home/shed", ProjectID: "4"}})
	})

	t.Run("a landing dir equal to $HOME absorbs into Home", func(t *testing.T) {
		got := WorkdirCandidates(nil, "/home/shed", "/home/shed", true)
		assertSeq(t, "candidates", got, []Workdir{{Title: "Home", Cwd: "/home/shed"}})
	})

	// roost stores a project with an empty cwd (it defaults at tab-open time);
	// such a project is not a workdir.
	t.Run("a project with no cwd is skipped", func(t *testing.T) {
		projects := []Project{{ID: "9", Name: "empty"}}
		got := WorkdirCandidates(projects, "/home/shed", "", false)
		assertSeq(t, "candidates", got, []Workdir{{Title: "Home", Cwd: "/home/shed"}})
	})
}

// TestCollapseWorkdirs pins §3.2 step 3's one-candidate rule. A menu with one
// row asks a question with one answer, which in a palette reads as a bug.
func TestCollapseWorkdirs(t *testing.T) {
	base := Token{Shed: "dev", Server: "srv", Agent: "codex", Home: "/home/shed"}

	t.Run("exactly one candidate collapses", func(t *testing.T) {
		got, collapsed := CollapseWorkdirs(base, WorkdirCandidates(nil, "/home/shed", "", false))
		if !collapsed {
			t.Fatalf("one candidate did not collapse")
		}
		if got.Cwd != "/home/shed" || got.Project != "" {
			t.Errorf("collapsed token = %+v", got)
		}
		if NextStep(got) != StepOpen {
			t.Errorf("a collapsed token must land on StepOpen")
		}
	})

	t.Run("one candidate that is a project carries its id", func(t *testing.T) {
		candidates := WorkdirCandidates(
			[]Project{{ID: "4", Name: "home", Cwd: "/home/shed"}}, "/home/shed", "", false)
		got, collapsed := CollapseWorkdirs(base, candidates)
		if !collapsed {
			t.Fatalf("one candidate did not collapse")
		}
		if got.Project != "4" {
			t.Errorf("project = %q, want 4", got.Project)
		}
	})

	t.Run("two candidates do not collapse", func(t *testing.T) {
		candidates := WorkdirCandidates(nil, "/home/shed", "/home/shed/proj", true)
		got, collapsed := CollapseWorkdirs(base, candidates)
		if collapsed {
			t.Fatalf("two candidates collapsed")
		}
		if got != base {
			t.Errorf("a non-collapsing call must not touch the token: %+v", got)
		}
		if NextStep(got) != StepWorkdirs {
			t.Errorf("an uncollapsed token must stay on StepWorkdirs")
		}
	})
}

func TestWorkdirMenu(t *testing.T) {
	tok := Token{Shed: "dev", Server: "srv", Agent: "codex", Home: "/home/shed"}
	candidates := WorkdirCandidates(
		[]Project{{ID: "1", Name: "roost", Cwd: "/home/shed/roost"}},
		"/home/shed", "/home/shed/proj", true)
	m := WorkdirMenu(tok, candidates)

	if len(m.Items) != 3 {
		t.Fatalf("rows = %+v", m.Items)
	}
	project := m.Items[0]
	if project.Title != "roost" || project.Subtitle != "/home/shed/roost" {
		t.Errorf("project row = %+v", project)
	}
	got, err := ParseToken(project.ID)
	if err != nil {
		t.Fatalf("project row id does not parse: %v", err)
	}
	want := Token{Shed: "dev", Server: "srv", Agent: "codex", Home: "/home/shed", Cwd: "/home/shed/roost", Project: "1"}
	if got != want {
		t.Errorf("project token = %+v, want %+v", got, want)
	}

	home, err := ParseToken(m.Items[1].ID)
	if err != nil {
		t.Fatalf("home row id does not parse: %v", err)
	}
	if home.Project != "" || home.Cwd != "/home/shed" {
		t.Errorf("home token = %+v", home)
	}
}

func TestLaunchArgv(t *testing.T) {
	assertSeq(t, "argv", LaunchArgv("cursor-agent"),
		[]string{"bash", "-lc", `exec "$@"`, "shed", "cursor-agent"})
}

func TestTabOpenFor(t *testing.T) {
	t.Run("a workdir with no project gets the 0 sentinel", func(t *testing.T) {
		got, err := TabOpenFor(Token{Machine: "mini2", Agent: "claude-rc", Home: "/root", Cwd: "/root/src"})
		if err != nil {
			t.Fatalf("TabOpenFor: %v", err)
		}
		if got.ProjectID != "0" {
			t.Errorf("project_id = %q, want \"0\"", got.ProjectID)
		}
		if got.Cwd != "/root/src" {
			t.Errorf("cwd = %q", got.Cwd)
		}
		// The tab's title is the DISPLAY title (`claude`), not the RC kind
		// (`claude-rc`) — this string is what a human reads in roost's tab bar.
		if got.Title != "claude" {
			t.Errorf("title = %q, want \"claude\"", got.Title)
		}
		if got.Argv[len(got.Argv)-1] != "claude" {
			t.Errorf("argv = %q", got.Argv)
		}
	})

	t.Run("a project-bearing workdir keeps its id", func(t *testing.T) {
		got, err := TabOpenFor(Token{Machine: "mini2", Agent: "cursor", Home: "/root", Cwd: "/src", Project: "7"})
		if err != nil {
			t.Fatalf("TabOpenFor: %v", err)
		}
		if got.ProjectID != "7" {
			t.Errorf("project_id = %q", got.ProjectID)
		}
		// The BINARY is cursor-agent even though the kind and title are cursor.
		if got.Argv[len(got.Argv)-1] != "cursor-agent" {
			t.Errorf("argv = %q", got.Argv)
		}
	})

	t.Run("an unknown agent kind is refused", func(t *testing.T) {
		_, err := TabOpenFor(Token{Machine: "mini2", Agent: "shell", Home: "/root", Cwd: "/src"})
		if err == nil {
			t.Fatalf("an unlaunchable kind was accepted")
		}
	})

	t.Run("no workdir is refused", func(t *testing.T) {
		_, err := TabOpenFor(Token{Machine: "mini2", Agent: "codex", Home: "/root"})
		if err == nil {
			t.Fatalf("a token with no cwd was accepted")
		}
	})
}

// TestTabOpenParamsSerialization pins the four keys roost's
// `deny_unknown_fields` TabOpenParams accepts from this provider, and the one
// thing that is easy to get wrong from Go: `project_id` is a STRING.
func TestTabOpenParamsSerialization(t *testing.T) {
	params, err := TabOpenFor(Token{Machine: "mini2", Agent: "gx", Home: "/root", Cwd: "/root/src", Project: "12"})
	if err != nil {
		t.Fatalf("TabOpenFor: %v", err)
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"project_id":"12","cwd":"/root/src","title":"gx","argv":["bash","-lc","exec \"$@\"","shed","gx"]}`
	if string(data) != want {
		t.Errorf("params JSON:\n got %s\nwant %s", data, want)
	}
}
