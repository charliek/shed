package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rustBootstrapSource is the Rust broker's copy of this package, relative to
// this directory: its BootstrapError::NoSshIdentities text must be Go's.
const rustBootstrapSource = "../../crates/shed-source/src/bootstrap.rs"

// stderrCauseTable is the hand-written table of what ssh's stderr becomes as
// the refusal's cause, which the Rust broker's diag_text is held to as well.
const stderrCauseTable = "../../crates/fixtures/bootstrap-stderr/cases.json"

// rustNoSSHIdentitiesText returns the #[error("…")] literal of the
// NoSshIdentities variant of the production enum, `pub enum BootstrapError`,
// in the Rust source src. It is an error unless the file declares that enum
// exactly once, the file holds exactly one line that starts with
// NoSshIdentities (a variant definition: uses are qualified), that line is
// inside the enum and reads `NoSshIdentities(String),`, and the line before it
// is a one-line #[error("…")] attribute. So a moved, reworded, duplicated or
// decoy variant cannot pass by not being read, or by being read instead.
func rustNoSSHIdentitiesText(src string) (string, error) {
	lines := strings.Split(src, "\n")
	enumAt, enumEnd := -1, -1
	for i, line := range lines {
		if strings.TrimSpace(line) != "pub enum BootstrapError {" {
			continue
		}
		if enumAt >= 0 {
			return "", fmt.Errorf("pub enum BootstrapError is declared twice (lines %d and %d)", enumAt+1, i+1)
		}
		enumAt = i
	}
	if enumAt < 0 {
		return "", errors.New("no pub enum BootstrapError")
	}
	for i := enumAt + 1; i < len(lines); i++ {
		if lines[i] == "}" {
			enumEnd = i
			break
		}
	}
	if enumEnd < 0 {
		return "", errors.New("pub enum BootstrapError has no closing line")
	}
	variantAt := -1
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "NoSshIdentities") {
			continue
		}
		if variantAt >= 0 {
			return "", fmt.Errorf("NoSshIdentities is defined twice (lines %d and %d)", variantAt+1, i+1)
		}
		variantAt = i
	}
	switch {
	case variantAt < 0:
		return "", errors.New("no NoSshIdentities variant")
	case variantAt <= enumAt || variantAt >= enumEnd:
		return "", fmt.Errorf("the NoSshIdentities variant (line %d) is not inside pub enum BootstrapError (lines %d to %d)",
			variantAt+1, enumAt+1, enumEnd+1)
	case strings.TrimSpace(lines[variantAt]) != "NoSshIdentities(String),":
		return "", fmt.Errorf("the NoSshIdentities variant is not NoSshIdentities(String),: %q", lines[variantAt])
	}
	attr := strings.TrimSpace(lines[variantAt-1])
	const open, end = `#[error("`, `")]`
	if !strings.HasPrefix(attr, open) || !strings.HasSuffix(attr, end) {
		return "", fmt.Errorf("the line before NoSshIdentities is not a one-line #[error(\"…\")] attribute: %q", attr)
	}
	lit := strings.TrimSuffix(strings.TrimPrefix(attr, open), end)
	if strings.ContainsAny(lit, `\"`) {
		return "", fmt.Errorf("the NoSshIdentities literal holds an escape this test does not decode: %q", lit)
	}
	return lit, nil
}

// stderrCause is one case of the shared table: ssh's stderr and the cause the
// refusal keeps, each with pad 'a' characters before it.
type stderrCause struct {
	Name   string `json:"name"`
	Pad    int    `json:"pad"`
	Stderr string `json:"stderr"`
	Cause  string `json:"cause"`
}

func readStderrCauseTable(t *testing.T) []stderrCause {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(stderrCauseTable))
	if err != nil {
		t.Fatalf("read the shared table: %v", err)
	}
	var table struct {
		Cases []stderrCause `json:"cases"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatalf("decode %s: %v", stderrCauseTable, err)
	}
	if len(table.Cases) < 10 {
		t.Fatalf("the shared table holds only %d cases", len(table.Cases))
	}
	for i := range table.Cases {
		pad := strings.Repeat("a", table.Cases[i].Pad)
		table.Cases[i].Stderr = pad + table.Cases[i].Stderr
		table.Cases[i].Cause = pad + table.Cases[i].Cause
	}
	return table.Cases
}

// TestNoSSHIdentitiesTextMatchesRust: the refusal of every key reads the same
// whichever client met it — the CLI (this package) or the desktop's source
// (crates/shed-source's bootstrap). It reads the Rust source string and requires it to be
// Go's sentinel and hint, then ssh's words, byte for byte; and, for every case
// of the table both languages read (crates/fixtures/bootstrap-stderr/), Go's
// error is the Rust text with that case's cause, escaped and clipped as the
// table says (an ESC sequence, line breaks, a character at the clip boundary).
// The Rust test no_ssh_identities_cause_shared_table holds Rust to the same
// table.
func TestNoSSHIdentitiesTextMatchesRust(t *testing.T) {
	src, err := os.ReadFile(filepath.FromSlash(rustBootstrapSource))
	if err != nil {
		t.Fatalf("read the Rust broker's bootstrap source: %v", err)
	}
	rust, err := rustNoSSHIdentitiesText(string(src))
	if err != nil {
		t.Fatalf("%s: %v", rustBootstrapSource, err)
	}
	if want := ErrNoSSHIdentities.Error() + " " + noSSHIdentitiesHint + ": {0}"; rust != want {
		t.Errorf("Rust literal = %q, want %q", rust, want)
	}

	for _, tc := range readStderrCauseTable(t) {
		t.Run(tc.Name, func(t *testing.T) {
			goErr := classify(errors.New("exit status 255"), 255, false, tc.Stderr)
			if !errors.Is(goErr, ErrNoSSHIdentities) {
				t.Fatalf("classify = %v, want ErrNoSSHIdentities", goErr)
			}
			// Rust formats its one field, the cause, where Go formats it.
			if want := strings.Replace(rust, "{0}", tc.Cause, 1); goErr.Error() != want {
				t.Errorf("Go and Rust texts differ:\n  go:   %q\n  rust: %q", goErr.Error(), want)
			}
		})
	}

	// Bytes that are not UTF-8 cannot be written in the JSON table; Go shows
	// each as U+FFFD, escaped, as Rust's lossy decoding does for a lone byte.
	goErr := classify(errors.New("exit status 255"), 255, false, "a\xffb\x1b Permission denied (publickey).")
	if want := strings.Replace(rust, "{0}", `a\u{fffd}b\u{1b} Permission denied (publickey).`, 1); goErr.Error() != want {
		t.Errorf("a byte that is not UTF-8:\n  got  %q\n  want %q", goErr.Error(), want)
	}
}

// TestRustTextReaderFailsClosed holds the reader of the Rust source to its
// promise: a duplicated variant, a decoy enum declared before the production
// one, a second production enum, a variant outside the enum, a reworded
// variant and a variant with no attribute each make it an error, and it reads
// the production enum's literal.
func TestRustTextReaderFailsClosed(t *testing.T) {
	const prod = "pub enum BootstrapError {\n    #[error(\"real: {0}\")]\n    NoSshIdentities(String),\n}\n"
	if got, err := rustNoSSHIdentitiesText(prod); err != nil || got != "real: {0}" {
		t.Fatalf("the reader read %q, %v from the production enum", got, err)
	}
	for name, src := range map[string]string{
		"a duplicate variant": "pub enum BootstrapError {\n    #[error(\"real: {0}\")]\n    NoSshIdentities(String),\n" +
			"    #[error(\"real: {0}\")]\n    NoSshIdentities(String),\n}\n",
		"a decoy enum before it":     "enum Decoy {\n    #[error(\"old: {0}\")]\n    NoSshIdentities(String),\n}\n" + prod,
		"a second production enum":   prod + "mod tests {\npub enum BootstrapError {\n}\n}\n",
		"a variant outside the enum": "pub enum BootstrapError {\n}\n#[error(\"real: {0}\")]\nNoSshIdentities(String),\n",
		"a reworded variant":         "pub enum BootstrapError {\n    #[error(\"real: {0}\")]\n    NoSshIdentities(String, u8),\n}\n",
		"no attribute":               "pub enum BootstrapError {\n    /// doc\n    NoSshIdentities(String),\n}\n",
	} {
		if got, err := rustNoSSHIdentitiesText(src); err == nil {
			t.Errorf("%s: the reader read %q, want an error", name, got)
		}
	}
}
