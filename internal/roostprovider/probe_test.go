package roostprovider

import (
	"strings"
	"testing"
)

// TestProbeScriptIsPinned makes the script visible in the repo, byte for byte.
// It is the one string in this package that runs on somebody else's machine
// under a login shell we do not control, so a change to it should read as a
// change, not slip through inside a refactor.
func TestProbeScriptIsPinned(t *testing.T) {
	want := `printf "%s\0" "shed-roost-probe-v1"; ` +
		`printf "%s\0" "${HOME:-}"; ` +
		`p=$(command -v claude 2>/dev/null) || p=""; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] || p="" ;; *) p="" ;; esac; printf "%s\0" "$p"; ` +
		`p=$(command -v codex 2>/dev/null) || p=""; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] || p="" ;; *) p="" ;; esac; printf "%s\0" "$p"; ` +
		`p=$(command -v cursor-agent 2>/dev/null) || p=""; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] || p="" ;; *) p="" ;; esac; printf "%s\0" "$p"; ` +
		`p=$(command -v opencode 2>/dev/null) || p=""; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] || p="" ;; *) p="" ;; esac; printf "%s\0" "$p"; ` +
		`p=$(command -v gx 2>/dev/null) || p=""; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] || p="" ;; *) p="" ;; esac; printf "%s\0" "$p"; ` +
		`p=$(command -v grok 2>/dev/null) || p=""; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] || p="" ;; *) p="" ;; esac; printf "%s\0" "$p"; ` +
		`if [ -n "${1:-}" ] && [ -d "$1" ]; then printf "%s\0" 1; else printf "%s\0" ""; fi; ` +
		`exit 0`
	if got := ProbeScript(); got != want {
		t.Errorf("ProbeScript():\n got %q\nwant %q", got, want)
	}
}

// TestProbeScriptCarriesNoSingleQuoteOrNewline is the portability property, and
// the reason the script is written with double quotes throughout: it becomes
// ONE single-quoted word in the remote command, and neither an embedded quote
// nor an embedded newline survives that in csh/tcsh/fish.
func TestProbeScriptCarriesNoSingleQuoteOrNewline(t *testing.T) {
	script := ProbeScript()
	if strings.Contains(script, "'") {
		t.Errorf("the probe script carries a single quote")
	}
	if strings.Contains(script, "\n") {
		t.Errorf("the probe script carries a newline")
	}
}

func TestProbeCommandShape(t *testing.T) {
	t.Run("a machine passes no positional argument", func(t *testing.T) {
		got := ProbeCommand("")
		want := "bash -lc " + shellQuote(ProbeScript())
		if got != want {
			t.Errorf("ProbeCommand(\"\") = %q", got)
		}
		// One single quote at each end and nowhere else: a machine's login
		// shell may be fish or csh, and this is what keeps the command
		// parseable there.
		if strings.Count(got, "'") != 2 {
			t.Errorf("a machine's probe command carries %d single quotes, want 2", strings.Count(got, "'"))
		}
	})
	t.Run("a shed passes its landing dir as $1", func(t *testing.T) {
		got := ProbeCommand("/home/shed/my proj")
		if !strings.HasSuffix(got, ` shed '/home/shed/my proj'`) {
			t.Errorf("ProbeCommand did not append the quoted landing dir: %q", got)
		}
	})
	t.Run("a landing dir with a quote is escaped", func(t *testing.T) {
		got := ProbeCommand("/home/shed/it's")
		if !strings.HasSuffix(got, ` shed '/home/shed/it'\''s'`) {
			t.Errorf("ProbeCommand did not escape the quote: %q", got)
		}
	})
}

// probeOutput builds a well-formed probe stdout: leading chatter, the sentinel,
// then the records.
func probeOutput(leading string, records ...string) []byte {
	out := leading + probeSentinel + "\x00"
	for _, r := range records {
		out += r + "\x00"
	}
	return []byte(out)
}

func fullRecords(home string, paths map[string]string, landing string) []string {
	records := []string{home}
	for _, a := range agentTable {
		records = append(records, paths[a.Binary])
	}
	return append(records, landing)
}

func TestParseProbe(t *testing.T) {
	t.Run("everything found", func(t *testing.T) {
		out := probeOutput("", fullRecords("/home/shed", map[string]string{
			"claude":       "/home/shed/.local/bin/claude",
			"cursor-agent": "/home/shed/.local/bin/cursor-agent",
			"opencode":     "/home/shed/.bun/bin/opencode",
		}, "1")...)
		p, err := ParseProbe(out)
		if err != nil {
			t.Fatalf("ParseProbe: %v", err)
		}
		if p.Home != "/home/shed" {
			t.Errorf("home = %q", p.Home)
		}
		if !p.LandingDirExists {
			t.Errorf("landing dir should exist")
		}
		if len(p.Found) != 3 {
			t.Fatalf("found = %v", p.Found)
		}
		// Keyed by KIND, not by binary — `cursor-agent` is the binary and
		// `cursor` is the kind, and the row id carries the kind.
		if p.Found["cursor"] != "/home/shed/.local/bin/cursor-agent" {
			t.Errorf("cursor = %q", p.Found["cursor"])
		}
		if _, ok := p.Found["codex"]; ok {
			t.Errorf("codex was not found but is in the map")
		}
	})

	t.Run("nothing found", func(t *testing.T) {
		p, err := ParseProbe(probeOutput("", fullRecords("/root", nil, "")...))
		if err != nil {
			t.Fatalf("ParseProbe: %v", err)
		}
		if len(p.Found) != 0 {
			t.Errorf("found = %v", p.Found)
		}
		if p.LandingDirExists {
			t.Errorf("landing dir should not exist")
		}
	})

	t.Run("a home path with spaces", func(t *testing.T) {
		p, err := ParseProbe(probeOutput("", fullRecords("/Users/First Last", map[string]string{
			"codex": "/Users/First Last/.bun/bin/codex",
		}, "")...))
		if err != nil {
			t.Fatalf("ParseProbe: %v", err)
		}
		if p.Home != "/Users/First Last" {
			t.Errorf("home = %q", p.Home)
		}
		if p.Found["codex"] != "/Users/First Last/.bun/bin/codex" {
			t.Errorf("codex = %q", p.Found["codex"])
		}
	})

	// The sentinel's whole reason to exist: a login shell that prints. Without
	// it, this chatter would fuse onto the front of the $HOME record and the
	// wizard would hand `tab.open` a cwd that does not exist.
	t.Run("leading login-shell chatter is discarded", func(t *testing.T) {
		p, err := ParseProbe(probeOutput("Welcome to mini3\nmise activated\n",
			fullRecords("/home/shed", nil, "")...))
		if err != nil {
			t.Fatalf("ParseProbe: %v", err)
		}
		if p.Home != "/home/shed" {
			t.Errorf("home = %q — the sentinel did not resynchronize the framing", p.Home)
		}
	})
}

// TestParseProbeRejectsMalformedOutput: acceptance criterion 3 — malformed
// far-side output stays a PROVIDER FAILURE, never a row. A row would claim to
// know something about the far side that this side never learned.
func TestParseProbeRejectsMalformedOutput(t *testing.T) {
	tests := []struct {
		name string
		out  []byte
		want string
	}{
		{"empty output", nil, "no shed-roost-probe-v1 record"},
		{
			"no sentinel",
			[]byte("/home/shed\x00/usr/bin/claude\x00"),
			"no shed-roost-probe-v1 record",
		},
		{
			"truncated after the sentinel",
			probeOutput("", "/home/shed", "/usr/bin/claude"),
			"carried 3 NUL-terminated fields",
		},
		{
			// One record short, so the run splits into exactly
			// probeRecordCount fields. Without the NUL-termination check this
			// would parse, and the landing-dir record would read as the empty
			// tail.
			"one record short",
			probeOutput("", fullRecords("/home/shed", nil, "")[:probeRecordCount-1]...),
			"carried 8 NUL-terminated fields",
		},
		{
			// The other direction, and the one the `>=` check let through: a
			// far side that emits one EXTRA NUL — inside `$HOME`, say —
			// shifts every later field by one. `$HOME` becomes its own
			// prefix, each agent's path becomes the previous agent's, and the
			// last agent's path is read as the landing-dir flag. That is a
			// wrong parse presented as a success, which ends as a tab opened
			// in a directory nobody named.
			"an extra NUL inside $HOME",
			probeOutput("", fullRecords("/home\x00shed", map[string]string{"grok": "/usr/bin/grok"}, "")...),
			"carried 10 NUL-terminated fields",
		},
		{
			// Exactly N+1 fields, but the last one is not the empty
			// terminator: every record arrived and then something else was
			// printed after them. Only the termination check catches this —
			// the field count is right.
			"the right number of fields, but not NUL-terminated",
			append(
				probeOutput("", fullRecords("/home/shed", nil, "")...),
				[]byte("logout\n")...,
			),
			"was not NUL-terminated",
		},
		{
			"an empty $HOME",
			probeOutput("", fullRecords("", nil, "")...),
			"no $HOME",
		},
		{
			// §3.2 pins that the probe returns the ABSOLUTE $HOME and that
			// `~` never reaches `tab.open` — which stores a non-empty cwd
			// verbatim. A relative value would travel in the row id and land
			// as a relative cwd, resolved against wherever roost's daemon
			// happens to be.
			"a relative $HOME",
			probeOutput("", fullRecords("home/shed", nil, "")...),
			"relative $HOME",
		},
		{
			"a tilde $HOME",
			probeOutput("", fullRecords("~", nil, "")...),
			"relative $HOME",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseProbe(tc.out)
			if err == nil {
				t.Fatalf("ParseProbe accepted malformed output")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
