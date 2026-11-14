package vmutil

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/config"
)

// cloneEnvSeen runs CloneRepo against an in-memory guest and returns the
// environment of the git clone the guest was asked to run.
func cloneEnvSeen(t *testing.T, owned bool) []string {
	t.Helper()
	envs := make(chan []string, 1)
	dialer := &pipeDialer{handler: func(conn net.Conn) {
		defer conn.Close()
		msgType, data, err := agentproto.ReadMessage(conn)
		if err != nil || msgType != agentproto.MsgTypeExecRequest {
			return
		}
		var req agentproto.ExecRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}
		if len(req.Cmd) > 1 && req.Cmd[0] == "git" && req.Cmd[1] == "clone" {
			envs <- req.Env
		}
		_, _, _ = agentproto.ReadMessage(conn) // stdin EOF
		_ = agentproto.WriteExitCode(conn, 0)
	}}
	agent := NewAgentClient(dialer, 1024, 1026)
	serverCfg := &config.ServerConfig{EnvVars: map[string]string{"GITHUB_TOKEN": "sentinel-server-secret"}}
	if err := CloneRepo(context.Background(), agent, serverCfg, "https://github.com/example/repo.git", owned, []string{"HTTPS_PROXY=http://10.0.0.1:3128"}); err != nil {
		t.Fatalf("CloneRepo: %v", err)
	}
	select {
	case env := <-envs:
		return env
	default:
		t.Fatal("the guest never saw a git clone")
		return nil
	}
}

// TestCloneRepoOwnedShedGetsNoServerEnv: the server's env_file variables (its
// own credentials) never reach an owned shed's clone, which the owning client
// can read by exec while it runs; an admin shed's clone keeps them.
func TestCloneRepoOwnedShedGetsNoServerEnv(t *testing.T) {
	owned := cloneEnvSeen(t, true)
	for _, kv := range owned {
		if kv == "GITHUB_TOKEN=sentinel-server-secret" {
			t.Errorf("owned shed's clone env carries the server's secret: %v", owned)
		}
	}
	if !slices.Contains(owned, "HTTPS_PROXY=http://10.0.0.1:3128") {
		t.Errorf("owned shed's clone env lost the egress env: %v", owned)
	}

	admin := cloneEnvSeen(t, false)
	if !slices.Contains(admin, "GITHUB_TOKEN=sentinel-server-secret") || !slices.Contains(admin, "HTTPS_PROXY=http://10.0.0.1:3128") {
		t.Errorf("admin shed's clone env = %v, want the server env and the egress env", admin)
	}
}

// TestCloneRepoRedactsOnlyTheLog: a clone URL with credentials reaches the
// guest's git unchanged, while git's output on its way to the server log has
// the credentials replaced.
func TestCloneRepoRedactsOnlyTheLog(t *testing.T) {
	const repo = "https://user:sentinel-token@github.com/example/repo.git"
	argvs := make(chan []string, 1)
	dialer := &pipeDialer{handler: func(conn net.Conn) {
		defer conn.Close()
		msgType, data, err := agentproto.ReadMessage(conn)
		if err != nil || msgType != agentproto.MsgTypeExecRequest {
			return
		}
		var req agentproto.ExecRequest
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}
		argvs <- req.Cmd
		_, _, _ = agentproto.ReadMessage(conn) // stdin EOF
		_ = agentproto.WriteStderr(conn, []byte("fatal: unable to access '"+repo+"': 403\n"))
		_ = agentproto.WriteExitCode(conn, 128)
	}}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = saved })
	logged := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		logged <- string(b)
	}()

	agent := NewAgentClient(dialer, 1024, 1026)
	cloneErr := CloneRepo(context.Background(), agent, &config.ServerConfig{}, repo, true, nil)
	os.Stderr = saved
	_ = w.Close()
	out := <-logged

	if cloneErr == nil || strings.Contains(cloneErr.Error(), "sentinel-token") {
		t.Errorf("CloneRepo error = %v, want a failure without the credential", cloneErr)
	}
	if got := <-argvs; !slices.Equal(got, []string{"git", "clone", repo, "repo"}) {
		t.Errorf("guest ran %q, want the URL as given", got)
	}
	if strings.Contains(out, "sentinel-token") {
		t.Errorf("server log carries the credential: %q", out)
	}
	if !strings.Contains(out, "https://REDACTED@github.com/example/repo.git") {
		t.Errorf("server log = %q, want git's line with the URL redacted", out)
	}
}

// TestCloneRepoKeepsNoOutput: the guest decides how much git prints, so the
// server must not hold it. 48 MiB of clone output leaves the heap where it
// was.
func TestCloneRepoKeepsNoOutput(t *testing.T) {
	const chunk, chunks = 32 << 10, 1536 // 48 MiB
	line := []byte(strings.Repeat("remote: counting objects\n", chunk/25))
	live := make(chan uint64, 1)
	dialer := &pipeDialer{handler: func(conn net.Conn) {
		defer conn.Close()
		if _, _, err := agentproto.ReadMessage(conn); err != nil {
			return
		}
		_, _, _ = agentproto.ReadMessage(conn) // stdin EOF
		for i := 0; i < chunks; i++ {
			if err := agentproto.WriteData(conn, line); err != nil {
				return
			}
		}
		// Everything is written and read; whatever the server still
		// holds of it is live now.
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		live <- m.HeapAlloc
		_ = agentproto.WriteExitCode(conn, 0)
	}}

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	saved := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() { os.Stdout = saved })

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	agent := NewAgentClient(dialer, 1024, 1026)
	err = CloneRepo(context.Background(), agent, &config.ServerConfig{}, "https://github.com/example/repo.git", true, nil)
	os.Stdout = saved
	if err != nil {
		t.Fatalf("CloneRepo: %v", err)
	}
	after := <-live
	const allowed = 16 << 20
	if after > before.HeapAlloc && after-before.HeapAlloc > allowed {
		t.Fatalf("the heap grew by %d MiB while the guest printed %d MiB; want under %d MiB",
			(after-before.HeapAlloc)>>20, (chunk*chunks)>>20, allowed>>20)
	}
}
