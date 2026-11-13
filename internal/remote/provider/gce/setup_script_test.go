package gce

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const setupScript = "../../../../scripts/remote/gce-setup.sh"

// TestSetupScript checks that gce-setup.sh runs nothing (a failing gcloud on
// PATH is never called), refuses to work without --print, and grants exactly
// Permissions.
func TestSetupScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	stubDir := t.TempDir()
	marker := filepath.Join(stubDir, "called")
	stub := "#!/bin/sh\ntouch " + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(stubDir, "gcloud"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, int) {
		cmd := exec.Command("bash", append([]string{setupScript}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	good := []string{"--project", "example-project", "--env", "dev-box", "--region", "us-central1", "--source-range", "192.0.2.10/32"}

	t.Run("without --print", func(t *testing.T) {
		if _, code := run(good...); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
	})
	t.Run("bad flags", func(t *testing.T) {
		for _, bad := range [][]string{
			{"--env", "Dev"},
			{"--env", "dev;rm -rf"},
			{"--source-range", "0.0.0.0/0"},
			{"--source-range", "192.0.2.10"},
			{"--project", "$(id)"},
			{"--region", "us central"},
		} {
			args := append([]string{"--print"}, good...)
			args = append(args, bad...)
			if out, code := run(args...); code != 2 {
				t.Errorf("%v: exit %d, want 2\n%s", bad, code, out)
			}
		}
	})
	t.Run("prints", func(t *testing.T) {
		out, code := run(append([]string{"--print"}, good...)...)
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the script ran gcloud")
		}
		for _, want := range []string{
			"gcloud compute networks create shed-dev-box",
			"--subnet-mode=custom",
			"--stack-type=IPV4_ONLY",
			"gcloud compute firewall-rules create shed-dev-box-ssh",
			"--rules=tcp:22 --source-ranges=192.0.2.10/32 --target-tags=shed-dev-box",
			"gcloud iam roles create shedServer",
			"gcloud iam service-accounts create shed-dev-box",
			"shed-dev-box@example-project.iam.gserviceaccount.com",
			"keys create /dev/stdout",
			"| base64",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q", want)
			}
		}
		m := regexp.MustCompile(`--permissions=(\S+)`).FindStringSubmatch(out)
		if m == nil {
			t.Fatal("no --permissions line")
		}
		got := strings.Split(m[1], ",")
		want := slices.Clone(Permissions)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("role permissions differ from gce.Permissions:\n got %v\nwant %v", got, want)
		}
	})
}
