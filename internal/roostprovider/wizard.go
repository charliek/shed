package roostprovider

import "fmt"

// Step is where an `activate` lands, derived from the row id alone.
//
// roost re-execs the provider from scratch for every step (activate is
// recursive, and each sub-menu's rows activate in turn), so there is no state
// between steps except the id — and this function is the whole state machine
// that reads it.
type Step int

const (
	// StepAgents — a host token with no agent chosen: probe the far side, gate
	// on `session.identify`, list the agents it found (plan 019 §3.2 step 2).
	StepAgents Step = iota
	// StepWorkdirs — an agent is chosen but no cwd: list the far side's
	// projects plus Home plus, for a shed, its landing dir (step 3).
	StepWorkdirs
	// StepOpen — a cwd is chosen: `tab.open`, print the confirmation, done
	// (step 4).
	StepOpen
)

func (s Step) String() string {
	switch s {
	case StepAgents:
		return "agents"
	case StepWorkdirs:
		return "workdirs"
	case StepOpen:
		return "open"
	}
	return fmt.Sprintf("step(%d)", int(s))
}

// NextStep reads a token and says which step it lands on.
//
// The order of the two tests matters and is not arbitrary: Cwd is checked
// FIRST, so a token that somehow carries a cwd but no agent opens nothing —
// it falls back to the agent step rather than opening a tab with no argv. The
// only way to reach StepOpen is through a token that named an agent first,
// which is what the second test enforces.
func NextStep(t Token) Step {
	if t.Agent == "" {
		return StepAgents
	}
	if t.Cwd == "" {
		return StepWorkdirs
	}
	return StepOpen
}

// Workdir is one candidate for the workdir step.
type Workdir struct {
	// Title is the row's text: a project's name, "Home", or "Landing dir".
	Title string
	// Cwd is the absolute directory. Always absolute — `tab.open` stores a
	// non-empty cwd VERBATIM and hands it to the PTY with no `~` expansion
	// (verified in plan 019 §2), so a `~` reaching here is a tab that fails to
	// start in a directory literally named `~`.
	Cwd string
	// ProjectID is a far-side project id, or "" for Home and the landing dir.
	// Empty becomes `"0"` on the wire.
	ProjectID string
}

// Titles for the two synthetic candidates. A project's row is titled with its
// own name; these two are named after what they ARE, with the path in the
// subtitle, so the three kinds of candidate read as one list.
const (
	homeTitle    = "Home"
	landingTitle = "Landing dir"
)

// WorkdirCandidates builds the workdir step's candidate list (plan 019 §3.2
// step 3): every far-side project, then Home, then — for a shed — its landing
// dir when that differs and exists.
//
// **Deduplicated by cwd, first occurrence winning.** A session whose only
// project already sits at $HOME would otherwise offer the same directory twice
// under two names, and — worse — would never collapse, because the collapse
// rule counts candidates. Projects come first in that precedence deliberately:
// a project carries an id, so keeping it over the bare Home row means
// `tab.open` lands the tab IN that project rather than in roost's default one.
//
// landingDir is passed through as-is and is only ever non-empty for a shed;
// landingExists is the probe's answer for it. A landing dir that does not exist
// on the far side is dropped rather than offered — a tab opened in a missing
// cwd fails at the PTY, which is a worse failure than one fewer row.
func WorkdirCandidates(projects []Project, home, landingDir string, landingExists bool) []Workdir {
	var out []Workdir
	seen := map[string]bool{}
	add := func(w Workdir) {
		if w.Cwd == "" || seen[w.Cwd] {
			return
		}
		seen[w.Cwd] = true
		out = append(out, w)
	}
	for _, p := range projects {
		// A project with no cwd of its own has nothing to offer as a workdir —
		// `add` drops it. roost stores such a project (an empty cwd defaults at
		// tab-open time), so this is a real shape, not a defensive branch.
		add(Workdir{Title: p.Name, Cwd: p.Cwd, ProjectID: p.ID})
	}
	add(Workdir{Title: homeTitle, Cwd: home})
	if landingExists {
		add(Workdir{Title: landingTitle, Cwd: landingDir})
	}
	return out
}

// CollapseWorkdirs implements plan 019 §3.2 step 3's one-candidate rule:
// exactly one candidate skips the step and opens.
//
// Returns the token to open with, and whether the step collapsed. The point is
// not saving a keystroke — it is that a menu with one row asks a question with
// one answer, which in a palette reads as "something went wrong" rather than as
// a choice.
func CollapseWorkdirs(tok Token, candidates []Workdir) (Token, bool) {
	if len(candidates) != 1 {
		return tok, false
	}
	tok.Cwd = candidates[0].Cwd
	tok.Project = candidates[0].ProjectID
	return tok, true
}

// LaunchArgv is the argv a tab is opened with (plan 019 §3.2 step 4).
//
// `bash -lc 'exec "$@"' shed <binary>`: the login shell resolves the agent off
// the same PATH the probe searched, then `exec`s it, so the TAB IS the agent
// process — no shell lingering as its parent, which is what makes roost's own
// lifecycle reporting describe the agent rather than a wrapper.
//
// The binary is a POSITIONAL WORD, never spliced into the script string. That
// is the security gate: the script is a constant this package wrote, and
// nothing derived from config, from a far side, or from a row id can become
// shell source. (Today `binary` comes from a table of six literals, so nothing
// hostile can reach it anyway — the shape is chosen so that stays true if the
// table ever becomes configurable.)
func LaunchArgv(binary string) []string {
	return []string{"bash", "-lc", `exec "$@"`, "shed", binary}
}

// TabOpenFor builds the `tab.open` params for a token that reached StepOpen.
//
// The tab's title is the agent's display Title (`claude`), not its RC Kind
// (`claude-rc`): this string is what a human reads in roost's tab bar, and the
// kind's wire spelling is an implementation detail of shed's own vocabulary.
func TabOpenFor(t Token) (TabOpenParams, error) {
	agent, ok := AgentByKind(t.Agent)
	if !ok {
		return TabOpenParams{}, fmt.Errorf("unknown agent kind %q", t.Agent)
	}
	if t.Cwd == "" {
		return TabOpenParams{}, fmt.Errorf("no workdir chosen")
	}
	project := t.Project
	if project == "" {
		project = "0"
	}
	return TabOpenParams{
		ProjectID: project,
		Cwd:       t.Cwd,
		Title:     agent.Title,
		Argv:      LaunchArgv(agent.Binary),
	}, nil
}
