package roostprovider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Go leg of the three shared goldens under crates/fixtures/roost-vectors/
// (plan 019 §3.2, §3.3). The Rust leg is
// crates/shed-core/tests/roost_provider_vectors.rs, which asserts the same files
// against the live `roost-ipc` functions they were derived from. Neither leg is
// meaningful alone: this one proves Go matches the file, that one proves the
// file matches roost.

func vectorPath(name string) string {
	return filepath.Join("..", "..", "crates", "fixtures", "roost-vectors", name)
}

func readVector(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(vectorPath(name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return data
}

func readVectorJSON(t *testing.T, name string, into any) {
	t.Helper()
	if err := json.Unmarshal(readVector(t, name), into); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
}

// TestExecChainCommandMatchesGolden pins the hand-copied Go constant to roost's
// own generated ladder. The golden carries a trailing newline the command does
// not; exactly one is trimmed, the same way the Rust leg trims it.
func TestExecChainCommandMatchesGolden(t *testing.T) {
	golden := string(readVector(t, "bootstrap/exec-chain-command.txt"))
	trimmed, found := strings.CutSuffix(golden, "\n")
	if !found {
		t.Fatalf("the exec-chain golden must end in exactly one newline")
	}
	if ExecChainCommand != trimmed {
		t.Errorf("ExecChainCommand does not match the golden.\n got: %q\nwant: %q", ExecChainCommand, trimmed)
	}
}

// TestExecChainCommandCarriesNoEmbeddedSingleQuote is the property the Go
// constant has to keep even if somebody edits it: the whole ladder is ONE
// single-quoted word, because the close-escape-reopen trick — the only POSIX way
// to embed a quote in
// a single-quoted word — is not an escape in csh/tcsh/fish, and a `machines:`
// entry's login shell is entitled to be any of those.
func TestExecChainCommandCarriesNoEmbeddedSingleQuote(t *testing.T) {
	inner, ok := strings.CutPrefix(ExecChainCommand, "sh -c '")
	if !ok {
		t.Fatalf("the exec chain must start with `sh -c '`")
	}
	inner, ok = strings.CutSuffix(inner, "'")
	if !ok {
		t.Fatalf("the exec chain must end with a closing single quote")
	}
	if strings.Contains(inner, "'") {
		t.Errorf("the exec chain carries an embedded single quote: %q", inner)
	}
}

type goldenAgent struct {
	Kind   string `json:"kind"`
	Binary string `json:"binary"`
	Title  string `json:"title"`
}

// TestAgentTableMatchesGolden pins all three spellings, in the provider's
// display order. Order IS asserted on this side (unlike the Rust leg's set
// comparison against roost_capabilities) because the order is the menu's, and
// §3.2 pins it twice.
func TestAgentTableMatchesGolden(t *testing.T) {
	var golden struct {
		Agents []goldenAgent `json:"agents"`
	}
	readVectorJSON(t, "agent-table.json", &golden)

	if len(golden.Agents) != len(agentTable) {
		t.Fatalf("golden has %d agents, agentTable has %d", len(golden.Agents), len(agentTable))
	}
	for i, want := range golden.Agents {
		got := agentTable[i]
		if got.Kind != want.Kind || got.Binary != want.Binary || got.Title != want.Title {
			t.Errorf("agent %d: got %+v, want %+v", i, got, want)
		}
	}
}

// TestAgentBinaryListMatchesGolden pins the "no agents found" subtitle's list
// against the same golden — the sentence a user reads when nothing was found
// has to name exactly what was looked for.
func TestAgentBinaryListMatchesGolden(t *testing.T) {
	var golden struct {
		Agents []goldenAgent `json:"agents"`
	}
	readVectorJSON(t, "agent-table.json", &golden)

	names := make([]string, len(golden.Agents))
	for i, a := range golden.Agents {
		names[i] = a.Binary
	}
	if want := strings.Join(names, ", "); agentBinaryList() != want {
		t.Errorf("agentBinaryList() = %q, want %q", agentBinaryList(), want)
	}
}

type goldenClassCase struct {
	Name     string  `json:"name"`
	ExitCode *int    `json:"exit_code"`
	Stderr   string  `json:"stderr"`
	Class    string  `json:"class"`
	Detail   *string `json:"detail"`
}

type goldenRowCase struct {
	Name  string `json:"name"`
	Class string `json:"class"`
	Row   string `json:"row"`
}

func readStderrClasses(t *testing.T) (classes []goldenClassCase, rows []goldenRowCase) {
	t.Helper()
	var golden struct {
		Classes      []goldenClassCase `json:"classes"`
		ProviderRows []goldenRowCase   `json:"provider_rows"`
	}
	readVectorJSON(t, "stderr-classes.json", &golden)
	if len(golden.Classes) == 0 || len(golden.ProviderRows) == 0 {
		t.Fatalf("stderr-classes.json is missing a section")
	}
	return golden.Classes, golden.ProviderRows
}

// TestClassifySSHFailureMatchesGolden pins the Go port against the same cases
// the Rust leg runs through roost's own classifier — precedence cases included,
// since rule ORDER is the part that would break silently.
func TestClassifySSHFailureMatchesGolden(t *testing.T) {
	classes, _ := readStderrClasses(t)
	for _, tc := range classes {
		t.Run(tc.Name, func(t *testing.T) {
			class, detail := ClassifySSHFailure(tc.ExitCode, tc.Stderr)
			if string(class) != tc.Class {
				t.Errorf("class = %q, want %q", class, tc.Class)
			}
			wantDetail := ""
			if tc.Detail != nil {
				wantDetail = *tc.Detail
			}
			if detail != wantDetail {
				t.Errorf("detail = %q, want %q", detail, wantDetail)
			}
		})
	}
}

// TestClassifySSHFailureCoversEveryClass keeps the golden honest: a case set
// that quietly stopped exercising a branch would keep passing while that branch
// drifted.
func TestClassifySSHFailureCoversEveryClass(t *testing.T) {
	classes, _ := readStderrClasses(t)
	seen := map[string]bool{}
	for _, tc := range classes {
		seen[tc.Class] = true
	}
	for _, want := range []SSHClass{
		ClassChangedHostKey, ClassHostKeyUnknown, ClassAuth,
		ClassNoSession, ClassNotFound, ClassTransport,
	} {
		if !seen[string(want)] {
			t.Errorf("the golden covers no %q case", want)
		}
	}
}

// TestProviderRowMatchesGolden pins the SECOND, shed-only mapping in the same
// file: which of §3.2's rows a class becomes. Kept apart from the classifier
// assertion above on purpose — "unreachable" is the provider's reading of
// transport-plus-255/timeout, not a roost class.
func TestProviderRowMatchesGolden(t *testing.T) {
	_, rows := readStderrClasses(t)
	for _, tc := range rows {
		t.Run(tc.Name, func(t *testing.T) {
			if got := ProviderRow(SSHClass(tc.Class)); string(got) != tc.Row {
				t.Errorf("ProviderRow(%q) = %q, want %q", tc.Class, got, tc.Row)
			}
		})
	}
}

// TestSpokenProtocolMatchesVendoredVector is the Go end of a three-link chain:
// the vendored protocol-4 identify vector is asserted against roost's own
// SESSION_PROTOCOL_VERSION by the Rust leg, and against this constant here. A
// generation bump renames that vector file (shed vendors only the current
// generation), so this test fails on the missing path rather than silently
// reading a stale one.
func TestSpokenProtocolMatchesVendoredVector(t *testing.T) {
	var vector struct {
		Result struct {
			SessionProtocol int `json:"session_protocol"`
		} `json:"result"`
	}
	readVectorJSON(t, "session.identify.response.v4.json", &vector)
	if vector.Result.SessionProtocol != SpokenProtocol {
		t.Errorf("SpokenProtocol = %d, the vendored v4 vector says %d",
			SpokenProtocol, vector.Result.SessionProtocol)
	}
}
