package guest

import (
	"strings"
	"testing"
)

// SSHDConfig is a literal so it can be read as the file it becomes. This
// keeps the names inside it in step with the constants they repeat.
func TestSSHDConfigAgreesWithTheConstants(t *testing.T) {
	var permit []string
	for _, port := range []uint32{ConsolePort, NotifyPort, TCPProxyPort} {
		permit = append(permit, "["+AgentSocketDir+"/"+SocketName(port)+"]:*")
	}
	want := map[string]string{
		"PermitRootLogin":              "no",
		"PasswordAuthentication":       "no",
		"KbdInteractiveAuthentication": "no",
		"AllowUsers":                   LinkUser,
		"HostKey":                      HostKeyFile,
		"AuthorizedKeysFile":           AuthorizedKeysFile,
		"AllowStreamLocalForwarding":   "local",
		"AllowTcpForwarding":           "local",
		"PermitOpen":                   strings.Join(permit, " "),
		"AllowAgentForwarding":         "no",
		"X11Forwarding":                "no",
		"PermitTunnel":                 "no",
		"PermitTTY":                    "no",
		"PermitUserRC":                 "no",
	}

	if !strings.HasSuffix(SSHDConfig, "\n") {
		t.Error("SSHDConfig does not end with a newline")
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(SSHDConfig, "\n"), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			t.Errorf("line %q is not \"Keyword value\"", line)
			continue
		}
		if _, dup := got[key]; dup {
			t.Errorf("%s is set twice; sshd keeps only the first", key)
		}
		got[key] = value
	}
	for key, value := range want {
		t.Run(key, func(t *testing.T) {
			if got[key] != value {
				t.Errorf("%s is %q, want %q", key, got[key], value)
			}
		})
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("SSHDConfig sets %s, which this test does not know", key)
		}
	}
}
