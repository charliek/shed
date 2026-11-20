package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// oldRefusalPhrases are the words of the refusal of a key that 0.10.0
// replaced (plan 037 §3.3). They read as a key problem whatever the cause (a
// passphrase key not in the agent, a user list not in force), and they send a
// person to auth.ssh settings a server that takes its users from an account
// service does not have.
var oldRefusalPhrases = []string{
	"(auth.ssh.github_users / authorized_keys)",
	"is not in this server's allowlist",
	"the key may not be on the server allowlist",
}

// refusalWordingTestFile is this file, relative to the repository root: it
// names the phrases, so it is not searched.
const refusalWordingTestFile = "cmd/shed/refusal_wording_test.go"

// refusalWordingExempt reports whether path (relative to the repository
// root, slash-separated) may keep the old words: the release notes and the
// upgrade guides quote what earlier releases printed.
func refusalWordingExempt(path string) bool {
	return path == "CHANGELOG.md" || strings.HasPrefix(path, "docs/upgrades/") || path == refusalWordingTestFile
}

// normaliseSpace returns data with every run of ASCII white space (space,
// tab, line feed, carriage return, vertical tab, form feed) replaced by one
// space.
func normaliseSpace(data []byte) []byte {
	out := make([]byte, 0, len(data))
	space := false
	for _, c := range data {
		switch c {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			if !space {
				out = append(out, ' ')
			}
			space = true
		default:
			out = append(out, c)
			space = false
		}
	}
	return out
}

// TestNormaliseSpace: a phrase wrapped across lines, with a list item's
// indent, reads as the phrase.
func TestNormaliseSpace(t *testing.T) {
	wrapped := []byte("fails with `Error: your SSH key is not in this\n   server's allowlist ...`")
	if !bytes.Contains(normaliseSpace(wrapped), []byte("is not in this server's allowlist")) {
		t.Errorf("normaliseSpace(%q) = %q", wrapped, normaliseSpace(wrapped))
	}
	if got := string(normaliseSpace([]byte(" a \t\r\n b\v\fc "))); got != " a b c " {
		t.Errorf("normaliseSpace = %q", got)
	}
}

// TestNoOldRefusalWording greps every file of the tree (tracked, or new and
// not ignored) and fails if any still holds one of the old refusal phrases,
// outside the release notes, the upgrade guides and this test.
func TestNoOldRefusalWording(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(refusalWordingTestFile))); err != nil {
		t.Fatalf("the repository root is not %s: %v", root, err)
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatalf("list the repository's files with git: %v", err)
	}
	searched := 0
	for _, path := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if path == "" || refusalWordingExempt(path) {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(full)
		if err != nil {
			if os.IsNotExist(err) {
				continue // deleted in the working tree, not yet in the index
			}
			t.Fatalf("stat %s: %v", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		searched++
		// A quotation in a doc wraps where its line ends: every run of
		// white space (line breaks and a list item's indent included) is
		// read as one space before searching.
		flat := normaliseSpace(data)
		for _, phrase := range oldRefusalPhrases {
			if bytes.Contains(flat, normaliseSpace([]byte(phrase))) {
				t.Errorf("%s still holds the old refusal wording %q", path, phrase)
			}
		}
	}
	// A listing that came back empty, or nearly so, searched nothing.
	if searched < 100 {
		t.Fatalf("searched only %d files under %s: the tree listing is not the repository's", searched, root)
	}
}
