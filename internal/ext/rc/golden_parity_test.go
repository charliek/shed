package rc

import (
	"bytes"
	"os"
	"path/filepath"
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
