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

// The JSONL turn-stream fixtures under testdata/jsonl/ are the folds' SHARED tables
// (plan 010 H4): this package's fold tests drive them through the opencode fold, and
// the Rust hub's fold mirror (crates/shed-broker/src/rc_hub/) consumes the crates-local
// copy. (The codex and cursor tables were dropped from both copies together with their
// lanes in A6, charliek/shed#322 — the sweep is what makes "together" enforceable.)
// Same directory-derived sweep as the panes above, same rationale.
func TestJSONLFixtureCopiesAreByteIdentical(t *testing.T) {
	assertFixtureDirCopyByteIdentical(t,
		"internal/ext/rc/testdata/jsonl",
		"crates/fixtures/jsonl",
		"`go test ./internal/ext/rc/` and `cd crates && cargo test -p shed-broker rc_hub`")
}

// assertFixtureDirCopyByteIdentical enumerates a canonical fixture directory and its
// crates-local copy and requires the two file SETS to be equal and every file to be
// byte-identical. Paths are repo-relative so a failure message prints commands a
// developer can paste at the repo root verbatim; reRun names the consuming test
// commands to re-run after a re-copy.
func assertFixtureDirCopyByteIdentical(t *testing.T, canonicalDir, copyDir, reRun string) {
	t.Helper()
	repoRoot := filepath.Join("..", "..", "..")

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
		t.Fatalf("no fixtures found under %s", canonicalDir)
	}
	copies := names(copyDir)

	for _, name := range canonical {
		if !slices.Contains(copies, name) {
			t.Errorf("%s/%s has no copy under %s.\n"+
				"The fixtures are byte-identical copies by convention — re-copy the whole "+
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
				"then re-run both consumers: %s.",
				copyDir, name, canonicalDir, name, canonicalDir, name, copyDir, name, reRun)
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
