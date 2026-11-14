package conformance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// BuildAgent builds cmd/shed-agent from this source tree for linux/goarch and
// returns the binary's path, inside a directory the test owns. An empty
// goarch means the host's. The agent is Linux-only; callers that run it on
// this host skip on other systems first.
func BuildAgent(t testing.TB, goarch string) string {
	t.Helper()
	out, err := BuildAgentInto(t.TempDir(), goarch)
	if err != nil {
		t.Fatalf("conformance: %v", err)
	}
	return out
}

// BuildAgentInto is BuildAgent for callers that own the directory, such as a
// TestMain that builds once for a whole package.
func BuildAgentInto(dir, goarch string) (string, error) {
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	// "go test" puts its own toolchain first on the test's PATH.
	gobin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("no go toolchain to build the agent with: %w", err)
	}

	out := filepath.Join(dir, "shed-agent-linux-"+goarch)
	cmd := exec.Command(gobin, "build", "-o", out, "./cmd/shed-agent")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build shed-agent for linux/%s: %w\n%s", goarch, err, output)
	}
	return out, nil
}

// moduleRoot finds the directory holding go.mod, walking up from the test's
// working directory (a test runs in its package's directory).
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the working directory")
		}
		dir = parent
	}
}
