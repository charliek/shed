package directory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// contractRoot is tests/directory-contract/ from this package's directory.
const contractRoot = "../../tests/directory-contract"

// contractDir is the version 1 contract.
var contractDir = filepath.Join(contractRoot, "v1")

// updating reports whether this run records values instead of checking
// them: UPDATE_CONTRACT=1 rewrites the key corpus's shed-side values and the
// manifest.
func updating() bool { return os.Getenv("UPDATE_CONTRACT") == "1" }

// readFixture reads a file of the contract, by its path under v1/.
func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

// decodeFixture decodes a JSON file of the contract into v.
func decodeFixture(t *testing.T, rel string, v any) {
	t.Helper()
	if err := json.Unmarshal(readFixture(t, rel), v); err != nil {
		t.Fatalf("decode %s: %v", rel, err)
	}
}

// fixtureFiles lists the files directly under v1/<dir> with the extension,
// by their paths under v1/, and fails when there are none (a check over an
// empty folder would pass vacuously).
func fixtureFiles(t *testing.T, dir, ext string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(contractDir, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ext {
			out = append(out, dir+"/"+e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s files in %s", ext, dir)
	}
	return out
}
