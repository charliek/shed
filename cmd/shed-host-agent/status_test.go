package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSocketDirEnvOverride(t *testing.T) {
	t.Setenv("SHED_HOST_AGENT_SOCKET_DIR", "/custom/shed/dir")
	if got := socketDir(); got != "/custom/shed/dir" {
		t.Fatalf("socketDir() = %q, want the env override", got)
	}
	if got := desktopSocketPath(); got != "/custom/shed/dir/host-agent.sock" {
		t.Fatalf("desktopSocketPath() = %q", got)
	}
	if got := statusSocketPath(); got != "/custom/shed/dir/host-agent-status.sock" {
		t.Fatalf("statusSocketPath() = %q", got)
	}
}

func TestSocketDirDefault(t *testing.T) {
	// Empty env => fall through to the platform default, which lives under a
	// "shed" directory on every OS.
	t.Setenv("SHED_HOST_AGENT_SOCKET_DIR", "")
	if dir := socketDir(); filepath.Base(dir) != "shed" {
		t.Fatalf("default socketDir() = %q, want it to end in /shed", dir)
	}
}

func TestRunStatusNotRunning(t *testing.T) {
	// Point the socket dir at an empty temp dir: nothing is listening, so status
	// must report "not running" and exit 1 without touching any config file.
	t.Setenv("SHED_HOST_AGENT_SOCKET_DIR", shortDir(t))
	var out strings.Builder
	if code := runStatus(false, &out); code != 1 {
		t.Fatalf("runStatus with no agent = %d, want 1", code)
	}
}

func TestRunStatusEndToEnd(t *testing.T) {
	t.Setenv("SHED_HOST_AGENT_SOCKET_DIR", shortDir(t))
	want := LiveStatus{
		Schema: statusSchemaVersion, Version: "v9", Pid: 4242,
		ConfigPath: "/opt/homebrew/etc/shed/extensions.yaml",
		StartedAt:  "2026-06-11T00:00:00Z",
		Policies: map[string]string{
			"ssh-agent": PolicyShedDesktop, "aws-credentials": PolicyDenyAll, "docker-credentials": PolicyApproveAll,
		},
		GateNamespaces: []string{"ssh-agent"},
		ApprovalChannel: ApprovalChannelStatus{
			SocketPath: desktopSocketPath(), ConsumerConnected: true, ClientName: "ShedDesktop", ClientVersion: "1.2.0",
		},
		Servers: []ServerHealth{{Name: "mac", URL: "http://localhost:8080",
			Namespaces: []NamespaceHealth{{Namespace: "ssh-agent", State: "connected"}}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveStatusSocket(ctx, statusSocketPath(), func() LiveStatus { return want }, testLogger())
	waitForSocket(t, statusSocketPath())

	var out strings.Builder
	if code := runStatus(false, &out); code != 0 {
		t.Fatalf("runStatus = %d, want 0", code)
	}
	got := out.String()
	for _, sub := range []string{
		"pid 4242", "v9",
		"/opt/homebrew/etc/shed/extensions.yaml", // the config the daemon loaded (#29)
		"ssh-agent", "shed-desktop", "(decided in shed-desktop)",
		"connected (ShedDesktop 1.2.0)", // approval-channel consumer identity
		"mac", "http://localhost:8080",
	} {
		if !strings.Contains(got, sub) {
			t.Fatalf("status output missing %q:\n%s", sub, got)
		}
	}
}

func TestRunStatusRejectsUnrecognizedSchema(t *testing.T) {
	// Any schema mismatch must be refused (not rendered): schema bumps only on a
	// breaking change, so a different number means the payload can't be trusted.
	// Cover both the zero value (foreign/old process) and a non-matching version.
	for _, schema := range []int{0, statusSchemaVersion + 1} {
		t.Setenv("SHED_HOST_AGENT_SOCKET_DIR", shortDir(t))
		ctx, cancel := context.WithCancel(context.Background())
		go serveStatusSocket(ctx, statusSocketPath(), func() LiveStatus {
			return LiveStatus{Schema: schema, Pid: 7, Version: "x"}
		}, testLogger())
		waitForSocket(t, statusSocketPath())

		var out strings.Builder
		if code := runStatus(false, &out); code != 1 {
			t.Fatalf("schema %d: runStatus = %d, want 1", schema, code)
		}
		if out.Len() != 0 {
			t.Fatalf("schema %d: expected no rendered report, got:\n%s", schema, out.String())
		}
		cancel()
	}
}

func TestRenderStatusNoConsumer(t *testing.T) {
	ls := LiveStatus{
		Schema: statusSchemaVersion, Version: "v1", Pid: 1,
		Policies:        map[string]string{"ssh-agent": PolicyDenyAll, "aws-credentials": PolicyDenyAll, "docker-credentials": PolicyDenyAll},
		ApprovalChannel: ApprovalChannelStatus{SocketPath: "/x/host-agent.sock", ConsumerConnected: false},
	}
	var sb strings.Builder
	renderStatus(&sb, ls)
	out := sb.String()
	for _, sub := range []string{"none connected", "fail closed", "Servers (0)", "(none being watched)"} {
		if !strings.Contains(out, sub) {
			t.Fatalf("render missing %q:\n%s", sub, out)
		}
	}
}

func TestServeStatusSocketRoundTrip(t *testing.T) {
	sock := shortSocketPath(t) // short /tmp root: avoid the sun_path 104-byte cap
	want := LiveStatus{
		Schema: statusSchemaVersion, Version: "v1", Pid: 99, ConfigPath: "/etc/x.yaml", WrittenAt: "t0",
		Policies:        map[string]string{"ssh-agent": PolicyShedDesktop},
		ApprovalChannel: ApprovalChannelStatus{SocketPath: "/s.sock", ConsumerConnected: true, ClientName: "App"},
		Servers: []ServerHealth{{Name: "s1", URL: "u1",
			Namespaces: []NamespaceHealth{{Namespace: "ssh-agent", State: "connected"}}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveStatusSocket(ctx, sock, func() LiveStatus { return want }, testLogger())
	waitForSocket(t, sock)

	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	var got LiveStatus
	if err := json.NewDecoder(conn).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Schema != statusSchemaVersion || got.Pid != 99 || got.ConfigPath != "/etc/x.yaml" ||
		!got.ApprovalChannel.ConsumerConnected || got.ApprovalChannel.ClientName != "App" ||
		len(got.Servers) != 1 || got.Servers[0].Namespaces[0].State != "connected" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// waitForSocket blocks until the Unix socket file at path appears (the listener
// has bound) or a short deadline passes.
func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket never appeared at %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
