package refsource

import (
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
)

const (
	refsourcePkg = "github.com/charliek/shed/internal/delivery/refsource"
	refsourceCmd = refsourcePkg + "/cmd/shed-provision-ref"
)

// mainDeps lists every main package of the module under one GOOS, each
// with its transitive dependencies (go list -deps, per main).
func mainDeps(t *testing.T, goos string) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", `{{if eq .Name "main"}}{{.ImportPath}} {{join .Deps " "}}{{end}}`, "github.com/charliek/shed/...")
	cmd.Env = append(cmd.Environ(), "GOOS="+goos)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list (GOOS=%s): %v\n%s", goos, err, ee.Stderr)
		}
		t.Fatalf("go list (GOOS=%s): %v", goos, err)
	}
	mains := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			mains[f[0]] = f[1:]
		}
	}
	return mains
}

// TestImportedByNoOtherMain checks the reference source is reached by no
// main package of the module but its own command, on Linux and on macOS:
// it can never ride into a shipped binary.
//
// Its limits: it lists the packages under the default build constraints of
// each of those two systems on the host's architecture only. An import
// behind a custom build tag, or in a file built only for another
// architecture, is not seen. The release builds (the goreleaser files) name
// their mains explicitly and use no custom tags.
func TestImportedByNoOtherMain(t *testing.T) {
	if testing.Short() {
		t.Skip("lists the module's packages")
	}
	for _, goos := range []string{"linux", "darwin"} {
		mains := mainDeps(t, goos)
		if len(mains) < 3 {
			t.Fatalf("GOOS=%s: only %d main packages listed", goos, len(mains))
		}
		// The control: its own command reaches it, so the check can see it.
		if !slices.Contains(mains[refsourceCmd], refsourcePkg) {
			t.Fatalf("GOOS=%s: %s does not reach %s; the check would pass vacuously", goos, refsourceCmd, refsourcePkg)
		}
		var names []string
		for m := range mains {
			names = append(names, m)
		}
		sort.Strings(names)
		for _, m := range names {
			if m == refsourceCmd {
				continue
			}
			for _, d := range mains[m] {
				if d == refsourcePkg || strings.HasPrefix(d, refsourcePkg+"/") {
					t.Errorf("GOOS=%s: %s reaches %s", goos, m, d)
				}
			}
		}
	}
}
