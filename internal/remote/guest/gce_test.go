package guest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The image scripts in gce/ copy the names in this package (they are shell,
// not Go). These tests keep the copies in step: a renamed path, unit, user or
// metadata key here fails until gce/ says the same.

func readGCE(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "gce", name))
	if err != nil {
		t.Fatalf("read gce/%s: %v", name, err)
	}
	return string(b)
}

func TestGCEScriptsUseGuestNames(t *testing.T) {
	sockDirFlag := "--listen unix:" + AgentSocketDir
	tests := []struct {
		file  string
		wants []string
	}{
		{"provision.sh", []string{
			fmt.Sprintf("SHED_LAYOUT=%d\n", Layout),
			LayoutFile,
			AgentBinaryPath,
			AgentInstallHelper,
			SudoersFile,
			"'" + SudoersRule + "'",
			SSHDConfigFile,
			"AllowUsers " + LinkUser + "\n",
			"HostKey " + HostKeyFile + "\n",
			"AuthorizedKeysFile " + AuthorizedKeysFile + "\n",
			filepath.Dir(AuthorizedKeysFile),
			"/etc/systemd/system/" + IdentityUnit,
			"/etc/systemd/system/" + AgentUnit,
			"useradd -m -s /bin/bash -u 1000 -G docker " + ShedUser,
			"groupadd --system " + LinkGroup,
			"--gid " + LinkGroup + " --shell /bin/sh",
			"--password '*'",
		}},
		{"shed-agent.service", []string{
			"ExecStart=" + AgentBinaryPath + " " + sockDirFlag + " --socket-group " + LinkGroup + " --shed-name ${SHED_NAME}\n",
			"EnvironmentFile=" + IdentityEnvFile + "\n",
			"Requires=" + IdentityUnit + "\n",
			"After=" + IdentityUnit,
			"KillMode=process\n",
			"Restart=always\n",
		}},
		{"shed-identity.service", []string{
			"Type=oneshot\n",
			"RemainAfterExit=yes\n",
			"After=network-online.target\n",
			"Before=ssh.service",
			AgentUnit,
			"ExecStart=/usr/local/sbin/shed-identity\n",
		}},
		{"shed-identity.sh", []string{
			"md_get " + MetadataShedName,
			"md_get " + MetadataShedID,
			"md_get " + MetadataAuthorizedKeys,
			"/run/shed/identity.env",
			HostKeyFile,
			AuthorizedKeysFile,
			"guest-attributes/" + HostKeyAttrNamespace + "/" + HostKeyAttrKey,
			"ssh-keygen -q -t ed25519",
			"SHED_NAME=%s\\nSHED_ID=%s",
		}},
		{"shed-agent-install.sh", []string{
			"agent=" + AgentBinaryPath + "\n",
			"unit=" + AgentUnit + "\n",
			"max_bytes=$((256 * 1024 * 1024))",
			`echo "installed ${want}"`,
		}},
		{"verify.sh", []string{
			LayoutFile,
			AgentSocketDir + " root:" + LinkGroup + " " + fmt.Sprintf("%o", AgentSocketDirMode),
			"root:" + LinkGroup + " " + fmt.Sprintf("%o", AgentSocketMode),
			fmt.Sprintf("for port in %d %d %d", ConsolePort, NotifyPort, TCPProxyPort),
			"\"allowusers " + LinkUser + "\"",
			"\"hostkey " + HostKeyFile + "\"",
			"\"authorizedkeysfile " + AuthorizedKeysFile + "\"",
			"\"" + SudoersRule + "\"",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			body := readGCE(t, tc.file)
			for _, want := range tc.wants {
				if !strings.Contains(body, want) {
					t.Errorf("gce/%s does not contain %q", tc.file, want)
				}
			}
		})
	}
}

// The identity script writes the env file the agent unit loads; both must
// name IdentityEnvFile, and the script must not use any other env file.
func TestGCEIdentityEnvFile(t *testing.T) {
	body := readGCE(t, "shed-identity.sh")
	re := regexp.MustCompile(`env_file="\$\{root\}([^"]+)"`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("gce/shed-identity.sh: no env_file assignment")
	}
	if m[1] != IdentityEnvFile {
		t.Errorf("gce/shed-identity.sh writes %q, want %q", m[1], IdentityEnvFile)
	}
}

// The sshd drop-in in provision.sh is SSHDConfig, line for line: the
// transport's real-sshd tests run exactly SSHDConfig, so the image must bake
// the same text.
func TestGCESSHDPolicy(t *testing.T) {
	body := readGCE(t, "provision.sh")
	start := strings.Index(body, "<<'SSHD_EOF'\n")
	end := strings.Index(body, "\nSSHD_EOF\n")
	if start < 0 || end < start {
		t.Fatal("gce/provision.sh: no SSHD_EOF heredoc")
	}
	var got []string
	for _, line := range strings.Split(body[start+len("<<'SSHD_EOF'\n"):end], "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		got = append(got, line)
	}
	want := strings.TrimSuffix(SSHDConfig, "\n")
	if strings.Join(got, "\n") != want {
		t.Errorf("sshd policy in gce/provision.sh:\n%s\nwant (guest.SSHDConfig):\n%s", strings.Join(got, "\n"), want)
	}
}
