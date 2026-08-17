package rc

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The wire goldens live in this package's testdata/ and are COPIED into every tree
// that needs one locally: cmd/shed (the CLI's decode guard), crates/fixtures (the Rust
// core include_str!s it — the copy is crates-local on purpose, because
// `make -C desktop core-linux` mounts only crates/ and a cross-tree include_str! could
// not compile there), and, from C4b on, the desktop Swift fixture.
//
// Go tests read cross-tree fine, so this guard byte-compares the canonical against
// every copy rather than relying on discipline: the exact drift this convention exists
// to prevent (a fixture updated in one tree and forgotten in another) fails here
// instead of silently diverging until a client breaks.
//
// The desktop Swift fixture joined this guard in C4b, the same commit that refreshed it
// and updated RCTests.swift's assertions — listing it any earlier would have failed this
// test mid-branch (the Swift fixture was intentionally left stale from C1 through C4a to
// avoid breaking Swift tests before the mirror work landed; see plan 007 §3.8).
func TestGoldenCopiesAreByteIdentical(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..")

	// Paths are REPO-RELATIVE (resolved against repoRoot below) so a failure message
	// prints the copy command a developer can paste at the repo root verbatim.
	cases := []struct {
		name      string
		canonical string
		copies    []string
	}{
		{
			name:      "rcSessionDto.golden.json",
			canonical: "internal/ext/rc/testdata/rcSessionDto.golden.json",
			copies: []string{
				"cmd/shed/testdata/rcSessionDto.golden.json",
				"crates/fixtures/rcSessionDto.golden.json",
				"desktop/Tests/ShedKitTests/Fixtures/rcSessionDto.golden.json",
			},
		},
		{
			name:      "feedMessage.golden.json",
			canonical: "internal/ext/rc/testdata/feedMessage.golden.json",
			copies: []string{
				"crates/fixtures/feedMessage.golden.json",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(repoRoot, c.canonical))
			if err != nil {
				t.Fatalf("reading the canonical golden %s: %v", c.canonical, err)
			}
			for _, copyPath := range c.copies {
				got, err := os.ReadFile(filepath.Join(repoRoot, copyPath))
				if err != nil {
					t.Errorf("reading copy %s: %v", copyPath, err)
					continue
				}
				if !bytes.Equal(want, got) {
					t.Errorf("%s has drifted from the canonical %s.\n"+
						"The goldens are byte-identical copies by convention — re-copy the canonical over it "+
						"(from the repo root):\n"+
						"  cp %s %s\n"+
						"then re-run the consuming tests: cmd/shed, crates/, and the desktop Swift fixture.",
						copyPath, c.canonical, c.canonical, copyPath)
				}
			}
		})
	}
}

// The pane fixtures under testdata/panes/ are the classifier's drift guard, and from
// plan 009 (the Rust rc-engine port) they are consumed by BOTH implementations: this
// package's TestPaneFixturesClassify and the Rust registry's fixture sweep
// (crates/shed-core/src/rc_agents.rs). The Rust copy is crates-LOCAL for the same
// reason as the wire goldens above — `make -C desktop core-linux` mounts only crates/,
// so a Rust test reading across the tree could not find them there.
//
// This sweep is DIRECTORY-DERIVED, not count-pinned: it enumerates the canonical
// directory and the copy and requires the two file SETS to be equal and every file to
// be byte-identical. That is deliberate — a new fixture added to prove a classifier
// anchor is exactly the case where forgetting to mirror it would let the two
// implementations diverge silently, so an uncopied (or orphaned) fixture fails here.
func TestPaneFixtureCopiesAreByteIdentical(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..")

	// Repo-relative so a failure message prints a command a developer can paste at
	// the repo root verbatim.
	const canonicalDir = "internal/ext/rc/testdata/panes"
	const copyDir = "crates/fixtures/panes"

	names := func(dir string) []string {
		entries, err := os.ReadDir(filepath.Join(repoRoot, dir))
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		var out []string
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			out = append(out, e.Name())
		}
		slices.Sort(out)
		return out
	}

	canonical := names(canonicalDir)
	if len(canonical) == 0 {
		t.Fatalf("no pane fixtures found under %s", canonicalDir)
	}
	copies := names(copyDir)

	for _, name := range canonical {
		if !slices.Contains(copies, name) {
			t.Errorf("%s/%s has no copy under %s.\n"+
				"The pane fixtures are byte-identical copies by convention — re-copy the whole "+
				"directory (from the repo root):\n  cp -a %s/. %s/",
				canonicalDir, name, copyDir, canonicalDir, copyDir)
			continue
		}
		want, err := os.ReadFile(filepath.Join(repoRoot, canonicalDir, name))
		if err != nil {
			t.Errorf("reading the canonical fixture %s/%s: %v", canonicalDir, name, err)
			continue
		}
		got, err := os.ReadFile(filepath.Join(repoRoot, copyDir, name))
		if err != nil {
			t.Errorf("reading copy %s/%s: %v", copyDir, name, err)
			continue
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s/%s has drifted from the canonical %s/%s.\n"+
				"Re-copy it (from the repo root):\n  cp %s/%s %s/%s\n"+
				"then re-run both consumers: `go test ./internal/ext/rc/` and "+
				"`cd crates && cargo test -p shed-core rc_agents`.",
				copyDir, name, canonicalDir, name, canonicalDir, name, copyDir, name)
		}
	}
	for _, name := range copies {
		if !slices.Contains(canonical, name) {
			t.Errorf("%s/%s is orphaned — no such fixture under %s.\n"+
				"Delete it, or restore the canonical fixture it was copied from.",
				copyDir, name, canonicalDir)
		}
	}
}
