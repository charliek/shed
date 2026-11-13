package tests

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/sshlink"
	"github.com/charliek/shed/internal/vmutil"
)

// TestLiveGuest checks a cloud shed image from the outside, through the SSH
// link and the agent, the way the server reaches it: the probe, the
// listeners, Docker, the tools, the network and the metadata server. It
// needs the same settings as TestLive plus SHED_LINK_GUEST=1, so it runs only
// against a VM made from a shed image (layout 1).
//
//	SHED_LINK_GUEST=1 SHED_LINK_HOST=... SHED_LINK_KEY_FILE=... \
//	SHED_LINK_HOST_KEY='ssh-ed25519 AAAA...' \
//	go test ./internal/remote/transport/sshlink/tests -run TestLiveGuest -count=1 -v
//
// SHED_LINK_GUEST_MARK=<text> also writes /home/shed/.shed-live-mark with that
// text when absent and reports it, so a stop and start can show the disk kept
// it.
func TestLiveGuest(t *testing.T) {
	if os.Getenv("SHED_LINK_GUEST") != "1" {
		t.Skip("set SHED_LINK_GUEST=1 and the TestLive settings to check a shed image")
	}
	cfg := loadLiveConfig(t)
	factory, err := sshlink.New(sshlink.Config{Keys: []ssh.Signer{cfg.key}, User: cfg.user, SocketDir: cfg.socketDir, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	link, err := factory.Open(context.Background(), transport.Target{
		ShedName: "live",
		Addr:     func(context.Context) (string, error) { return cfg.host, nil },
		HostKeys: sshlink.PinnedHostKey(cfg.hostKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	client := vmutil.NewAgentClient(link, transport.PortConsole, transport.PortNotify)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	start := time.Now()
	info, err := client.WaitForProbe(ctx, 2*time.Minute)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("probe after %v: %+v", time.Since(start).Round(time.Millisecond), info)
	if info.Protocol != guest.AgentProtocol || info.Layout != guest.Layout {
		t.Errorf("probe = protocol %d layout %d, want %d and %d", info.Protocol, info.Layout, guest.AgentProtocol, guest.Layout)
	}

	run := func(script string) (string, error) {
		var out bytes.Buffer
		err := client.Exec(ctx, backend.ExecOptions{
			Cmd:    []string{"/bin/bash", "-lc", script},
			Stdout: vmutil.NopWriteCloser(&out),
			Stderr: vmutil.NopWriteCloser(&out),
		})
		return strings.TrimSpace(out.String()), err
	}

	checks := []struct {
		name   string
		script string
		want   func(string) bool
	}{
		{"runs as shed", "id -un", func(s string) bool { return s == "shed" }},
		{"only port 22 listens off loopback",
			`ss -ltnH | awk '{print $4}' | grep -Ev '^(127\.|\[::1\]|::1)' | grep -Ev '(^|:)22$' || true`,
			func(s string) bool { return s == "" }},
		{"docker hello-world", "docker run --rm hello-world", func(s string) bool { return strings.Contains(s, "Hello from Docker!") }},
		{"craze", "craze --version", func(s string) bool { return s != "" }},
		{"gh", "gh --version", func(s string) bool { return strings.HasPrefix(s, "gh version") }},
		{"git clone over https",
			"rm -rf /tmp/live-prox && git clone -q --depth 1 https://github.com/charliek/prox /tmp/live-prox && test -d /tmp/live-prox/.git && echo cloned; rm -rf /tmp/live-prox",
			func(s string) bool { return strings.HasSuffix(s, "cloned") }},
		{"no IPv6 address", "ip -6 addr show scope global | grep inet6 || true", func(s string) bool { return s == "" }},
		{"no service-account token",
			`curl -s -o /dev/null -w '%{http_code}' -H 'Metadata-Flavor: Google' http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token`,
			func(s string) bool { return s == "404" }},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			out, err := run(c.script)
			t.Logf("%v: %q", time.Since(start).Round(time.Millisecond), out)
			if err != nil || !c.want(out) {
				t.Errorf("err %v, output %q", err, out)
			}
		})
	}

	if mark := os.Getenv("SHED_LINK_GUEST_MARK"); mark != "" {
		out, err := run("f=/home/shed/.shed-live-mark; [ -f $f ] || printf '%s' '" + strings.ReplaceAll(mark, "'", "") + "' > $f; cat $f")
		if err != nil {
			t.Errorf("mark: %v", err)
		}
		t.Logf("mark file holds %q", out)
		if out != strings.ReplaceAll(mark, "'", "") {
			t.Errorf("mark file holds %q, want %q", out, mark)
		}
	}
}
