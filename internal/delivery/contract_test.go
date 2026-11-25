package delivery

import (
	"os"
	"path/filepath"
	"testing"
)

// contractRoot is tests/provision-contract/ from this package's directory.
const contractRoot = "../../tests/provision-contract"

// contractDir is the version 2 contract.
var contractDir = filepath.Join(contractRoot, "v2")

// updating reports whether this run records values instead of checking
// them: UPDATE_CONTRACT=1 rewrites the manifest.
func updating() bool { return os.Getenv("UPDATE_CONTRACT") == "1" }

// readContractFile reads a file of the contract, by its path under v2/.
func readContractFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

// fixtureFiles lists the .json files directly under v2/<dir>, by their paths
// under v2/, and fails when there are none (a check over an empty folder
// would pass vacuously).
func fixtureFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(contractDir, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			out = append(out, dir+"/"+e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no .json files in %s", dir)
	}
	return out
}
