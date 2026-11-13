package sshtest

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/transport/conformance"
)

// RequireEnv, set to "1", turns "no real sshd available" from a skip into a
// failure. CI sets it, so the real-sshd leg cannot silently stop running.
const RequireEnv = "SHED_REQUIRE_SSHD"

// sshdImage is the local tag of the test image. Docker's layer cache makes a
// rebuild cheap; only the agent layer changes between runs.
const sshdImage = "shed-link-sshd-test:local"

// AgentHTTPPort is the port of the agent's plugin HTTP API inside the
// container: the agent's default.
const AgentHTTPPort = conformance.DefaultAgentHTTPPort

//go:embed sshd/Dockerfile sshd/entrypoint.sh
var sshdFiles embed.FS

// SSHDOptions configures the container.
type SSHDOptions struct {
	// AuthorizedKeys are the server's public keys, in authorized_keys
	// form. They reach the container as an environment variable, the way
	// a cloud guest gets them from instance metadata.
	AuthorizedKeys []string
	// ExtraConfig is sshd_config text that overrides guest.SSHDConfig,
	// for example "AllowTcpForwarding no". Use it to show what a policy
	// line is for by taking it away.
	ExtraConfig string
}

// SSHD is a container that stands in for a cloud shed's guest: Ubuntu 24.04,
// OpenSSH's sshd with guest layout 1's policy, the link user, and a
// shed-agent listening on unix sockets under /run/shed-agent.
type SSHD struct {
	id      string
	hostKey string
}

// StartSSHD builds the image and starts a container, removed when the test
// ends. Without a working Docker, or with -short, it skips the test, unless
// RequireEnv is "1", which makes that a failure.
func StartSSHD(t *testing.T, opts SSHDOptions) *SSHD {
	t.Helper()
	unavailable := func(format string, args ...any) {
		t.Helper()
		msg := fmt.Sprintf(format, args...)
		if os.Getenv(RequireEnv) == "1" {
			t.Fatalf("%s, and %s=1 forbids skipping the real-sshd leg", msg, RequireEnv)
		}
		t.Skipf("%s; skipping the real-sshd leg (set %s=1 to make this a failure)", msg, RequireEnv)
	}

	if testing.Short() && os.Getenv(RequireEnv) != "1" {
		t.Skip("the real-sshd leg builds and runs a container; skipped with -short")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		unavailable("docker is not installed")
	}
	arch, err := docker(30*time.Second, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		unavailable("the Docker daemon is not reachable (%v)", err)
	}
	goos, goarch, ok := strings.Cut(arch, "/")
	if !ok || goos != "linux" {
		unavailable("the Docker daemon runs %q containers, not linux", arch)
	}
	switch goarch {
	case "x86_64":
		goarch = "amd64"
	case "aarch64":
		goarch = "arm64"
	}

	// Build context: the embedded files, the guest's sshd policy exactly
	// as an image carries it, and an agent built for the container's
	// architecture from this source tree.
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "entrypoint.sh"} {
		data, err := sshdFiles.ReadFile("sshd/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "10-shed.conf"), []byte(guest.SSHDConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	agent, err := os.ReadFile(conformance.BuildAgent(t, goarch))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shed-agent"), agent, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := docker(10*time.Minute, "build", "--quiet", "--tag", sshdImage, dir); err != nil {
		unavailable("building the sshd test image failed (%v): %s", err, out)
	}

	id, err := docker(time.Minute, "run", "--detach",
		"--label", "shed.test=sshlink",
		"--env", "SHED_LINK_AUTHORIZED_KEYS="+strings.Join(opts.AuthorizedKeys, "\n"),
		"--env", "SHED_SSHD_EXTRA_CONFIG="+opts.ExtraConfig,
		"--publish", "127.0.0.1::22",
		sshdImage)
	if err != nil {
		unavailable("starting the sshd test container failed (%v): %s", err, id)
	}
	s := &SSHD{id: id}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("sshd container log:\n%s", s.Logs())
		}
		_, _ = docker(time.Minute, "rm", "--force", "--volumes", s.id)
	})

	// The entrypoint makes the host key on first start. Reading it through
	// "docker exec" is this harness's stand-in for the trusted path a
	// cloud guest's key takes (published by the guest, pinned by the
	// server).
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := docker(10*time.Second, "exec", s.id, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
		if fields := strings.Fields(out); err == nil && len(fields) >= 2 {
			s.hostKey = fields[0] + " " + fields[1]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the container never produced a host key: %v %s\n%s", err, out, s.Logs())
		}
		time.Sleep(100 * time.Millisecond)
	}
	return s
}

// HostKey returns the container's ed25519 host key in pin form.
func (s *SSHD) HostKey() string { return s.hostKey }

// Addr returns where sshd is reachable from this host now. Docker picks the
// host port, and may pick another at Restart, so this is asked each time,
// like a cloud guest's address. It has the signature of transport.Target.Addr.
func (s *SSHD) Addr(context.Context) (string, error) {
	out, err := docker(10*time.Second, "port", s.id, "22/tcp")
	if err != nil {
		return "", fmt.Errorf("docker port: %v: %s", err, out)
	}
	line, _, _ := strings.Cut(out, "\n")
	if line = strings.TrimSpace(line); line == "" {
		return "", fmt.Errorf("the container publishes no port")
	}
	return line, nil
}

// DirectAddr returns sshd's address on the container's own network, with no
// Docker port publishing in between, and whether this host can reach it (a
// Linux host can; Docker Desktop cannot). Published ports go through a
// userspace proxy with its own TCP connection to sshd, which hides how sshd's
// TCP settings meet the client's; timing tests need the direct path.
func (s *SSHD) DirectAddr() (string, bool) {
	ip, err := docker(10*time.Second, "inspect", "--format",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", s.id)
	if err != nil {
		return "", false
	}
	fields := strings.Fields(ip)
	if len(fields) == 0 {
		return "", false
	}
	addr := net.JoinHostPort(fields[0], "22")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", false
	}
	conn.Close()
	return addr, true
}

// Restart restarts the container: sshd and the agent both go down and come
// back, with the same host key.
func (s *SSHD) Restart(t *testing.T) {
	t.Helper()
	if out, err := docker(time.Minute, "restart", "--time", "1", s.id); err != nil {
		t.Fatalf("docker restart: %v: %s", err, out)
	}
}

// Exec runs a command in the container as root and returns its output.
func (s *SSHD) Exec(t *testing.T, args ...string) string {
	t.Helper()
	out, err := docker(time.Minute, append([]string{"exec", s.id}, args...)...)
	if err != nil {
		t.Fatalf("docker exec %v: %v: %s", args, err, out)
	}
	return out
}

// Logs returns the container's log: sshd's and the agent's stderr.
func (s *SSHD) Logs() string {
	out, _ := docker(30*time.Second, "logs", "--tail", "200", s.id)
	return out
}

// docker runs the docker CLI and returns its trimmed combined output.
func docker(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}
