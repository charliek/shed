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
			{"--source-range", "0.0.0.0/00"},
			{"--source-range", "192.0.2.0/0"},
			{"--source-range", "192.0.2.10"},
			{"--source-range", "999.0.2.10/32"},
			{"--source-range", "192.0.2.10/33"},
			{"--source-range", "192.0.02.10/32"},
			{"--subnet-range", "10.10.0.0/40"},
			{"--subnet-range", "10.256.0.0/24"},
			// A flag that comes last with no value is a usage error, not a
			// silent exit.
			{"--key-out"},
			{"--env"},
			{"--project", "$(id)"},
			{"--region", "us central"},
		} {
			args := append([]string{"--print"}, good...)
			args = append(args, bad...)
			out, code := run(args...)
			if code != 2 {
				t.Errorf("%v: exit %d, want 2\n%s", bad, code, out)
			}
			if strings.Contains(out, "gcloud ") {
				t.Errorf("%v: a refused run still printed commands", bad)
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
		lists := permissionLists(out)
		if len(lists) != 1 {
			t.Fatalf("%d --permissions lines, want 1", len(lists))
		}
		samePermissions(t, "the base role", lists[0], Permissions)
		// Without --firewall-auto nothing that can change a rule is
		// printed.
		for _, forbidden := range []string{"compute.firewalls.update", "compute.networks.updatePolicy", "compute.firewalls.create", "compute.firewalls.delete", "FirewallRule", "--condition='"} {
			if strings.Contains(out, forbidden) {
				t.Errorf("output without --firewall-auto holds %q", forbidden)
			}
		}
		if !strings.Contains(out, "SHED_REMOTE_SOURCE_RANGES=192.0.2.10/32") {
			t.Error("output does not name the source ranges variable")
		}
	})
	t.Run("prints the firewall-auto roles when asked", func(t *testing.T) {
		out, code := run(append([]string{"--print", "--firewall-auto"}, good...)...)
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the script ran gcloud")
		}
		lists := permissionLists(out)
		if len(lists) != 3 {
			t.Fatalf("%d --permissions lines, want 3 (base, rule, network)", len(lists))
		}
		samePermissions(t, "the base role", lists[0], Permissions)
		samePermissions(t, "the firewall rule role", lists[1], FirewallAutoRulePermissions)
		samePermissions(t, "the firewall network role", lists[2], FirewallAutoNetworkPermissions)
		for _, want := range []string{
			"gcloud iam roles create shedServerFirewallRule",
			"gcloud iam roles create shedServerFirewallNet",
			`--role="projects/$PROJECT/roles/shedServerFirewallRule"`,
			// The rule role is bound to the one derived rule, by name.
			`--condition='expression=resource.name == "projects/example-project/global/firewalls/shed-dev-box-ssh",title=shed-dev-box-ssh only,`,
			`--role="projects/$PROJECT/roles/shedServerFirewallNet" --condition=None`,
			"What a holder of these roles can do",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q", want)
			}
		}
	})
	t.Run("a long role id is refused with --firewall-auto", func(t *testing.T) {
		long := strings.Repeat("r", 53)
		if _, code := run(append([]string{"--print", "--firewall-auto", "--role-id", long}, good...)...); code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
		if _, code := run(append([]string{"--print", "--role-id", long}, good...)...); code != 0 {
			t.Errorf("without --firewall-auto: exit %d, want 0", code)
		}
	})
}

// TestPermissionListsDisjoint: the base role holds nothing that can change a
// firewall rule, and the rule role holds only permissions on a rule (the
// ones an IAM condition on the rule's name can govern).
func TestPermissionListsDisjoint(t *testing.T) {
	for _, p := range Permissions {
		if slices.Contains(FirewallAutoRulePermissions, p) || slices.Contains(FirewallAutoNetworkPermissions, p) {
			t.Errorf("%s is in the base role and in a firewall-auto role", p)
		}
		if strings.HasPrefix(p, "compute.firewalls.") && p != "compute.firewalls.list" {
			t.Errorf("the base role holds %s; it may only list rules", p)
		}
		if p == "compute.networks.updatePolicy" {
			t.Errorf("the base role holds %s", p)
		}
	}
	for _, p := range FirewallAutoRulePermissions {
		if !strings.HasPrefix(p, "compute.firewalls.") {
			t.Errorf("%s is bound under a condition on a firewall rule's name, which cannot govern it", p)
		}
	}
	for _, p := range FirewallAutoNetworkPermissions {
		if strings.HasPrefix(p, "compute.firewalls.") {
			t.Errorf("%s is bound without a condition; it belongs in the rule role", p)
		}
	}
	for _, list := range [][]string{Permissions, FirewallAutoRulePermissions, FirewallAutoNetworkPermissions} {
		if !slices.IsSorted(list) {
			t.Errorf("list is not sorted: %v", list)
		}
	}
}

// permissionLists returns every --permissions value in the script's output,
// in order.
func permissionLists(out string) [][]string {
	var lists [][]string
	for _, m := range regexp.MustCompile(`--permissions=(\S+)`).FindAllStringSubmatch(out, -1) {
		lists = append(lists, strings.Split(m[1], ","))
	}
	return lists
}

func samePermissions(t *testing.T, what string, got, want []string) {
	t.Helper()
	got, want = slices.Clone(got), slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s: the script's permissions differ from the Go list:\n got %v\nwant %v", what, got, want)
	}
}
