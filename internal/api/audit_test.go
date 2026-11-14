package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmutil"
)

// attachTestAudit opens an audit log in a temp dir on s and returns its path.
func attachTestAudit(t *testing.T, s *Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s.SetAuditLog(l)
	return path
}

// auditLines reads every line of the audit log, split by event.
func auditLines(t *testing.T, path string) (api, execEnd, other []map[string]any) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("audit line %q: %v", sc.Text(), err)
		}
		switch m["event"] {
		case audit.EventAPI:
			api = append(api, m)
		case audit.EventExecEnd:
			execEnd = append(execEnd, m)
		default:
			other = append(other, m)
		}
	}
	return api, execEnd, other
}

func TestAuditOneAPILinePerClientRequest(t *testing.T) {
	b := newClientFakeBackend(
		config.Shed{Name: "nocturn-a", Status: config.StatusRunning, Owner: testClientName},
		config.Shed{Name: "admin-x", Status: config.StatusRunning},
	)
	s, h := clientTestServer(t, b, defaultTestPolicy())
	path := attachTestAudit(t, s)
	client := serviceLeaf(testClientName)

	type want struct {
		method, path, route, shed string
		status                    int
	}
	reqs := []struct {
		body string
		leaf bool // false: an unknown client's certificate
		want want
	}{
		{"", true, want{"GET", "/api/info", "/api/info", "", 200}},
		{"", true, want{"GET", "/api/sheds", "/api/sheds", "", 200}},
		{"", true, want{"GET", "/api/system/df", "/api/system/df", "", 403}},
		{"", true, want{"GET", "/api/no/such/route", "", "", 403}},
		{"", true, want{"GET", "/api/no-such-route-1", "", "", 403}},
		{"", true, want{"GET", "/api/sheds/admin-x", "/api/sheds/{name}", "admin-x", 404}},
		{"", true, want{"DELETE", "/api/sheds/missing-y", "/api/sheds/{name}", "missing-y", 404}},
		{`{"name":"wrong-prefix"}`, true, want{"POST", "/api/sheds", "/api/sheds", "wrong-prefix", 403}},
		{`{"argv":["echo","a secret"]}`, true, want{"POST", "/api/sheds/nocturn-a/exec", "/api/sheds/{name}/exec", "nocturn-a", 200}},
		{"", false, want{"GET", "/api/sheds", "", "", 401}},
	}
	for _, r := range reqs {
		leaf := client
		if !r.leaf {
			leaf = serviceLeaf("stranger")
		}
		got := do(h, leaf, r.want.method, r.want.path, r.body)
		if got.code != r.want.status {
			t.Fatalf("%s %s: status %d, want %d (%s)", r.want.method, r.want.path, got.code, r.want.status, got.body)
		}
	}

	// Admins: requests are never logged (an exec still gets its exec.end).
	admin := adminLeaf()
	for _, p := range []string{"/api/sheds", "/api/info", "/api/sheds/admin-x", "/api/system/df"} {
		if got := do(h, admin, "GET", p, ""); got.code >= 400 {
			t.Fatalf("admin GET %s: %d %s", p, got.code, got.body)
		}
	}
	if got := do(h, admin, "POST", "/api/sheds/admin-x/exec", `{"argv":["id"]}`); got.code != 200 {
		t.Fatalf("admin exec: %d %s", got.code, got.body)
	}

	api, execEnd, other := auditLines(t, path)
	if len(other) != 0 {
		t.Errorf("unexpected lines: %v", other)
	}
	if len(api) != len(reqs) {
		t.Fatalf("want exactly %d api lines (one per client request, none for admins), got %d: %v", len(reqs), len(api), api)
	}
	for i, r := range reqs {
		line := api[i]
		w := r.want
		if line["method"] != w.method || line["path"] != w.path || line["route"] != w.route || line["status"] != float64(w.status) {
			t.Errorf("line %d = %v, want %+v", i, line, w)
		}
		if got, _ := line["shed"].(string); got != w.shed {
			t.Errorf("line %d shed = %q, want %q", i, got, w.shed)
		}
		id := line["identity"].(map[string]any)
		wantName := testClientName
		if !r.leaf {
			wantName = "stranger"
		}
		if id["kind"] != "client" || id["name"] != wantName || id["serial"] != "beef" {
			t.Errorf("line %d identity = %v", i, id)
		}
		for _, k := range []string{"ts", "duration_ms", "remote"} {
			if _, ok := line[k]; !ok {
				t.Errorf("line %d lacks %q", i, k)
			}
		}
	}

	if len(execEnd) != 2 {
		t.Fatalf("want 2 exec.end lines (client and admin), got %v", execEnd)
	}
	c, a := execEnd[0], execEnd[1]
	if c["shed"] != "nocturn-a" || c["argv0"] != "echo" || c["argc"] != float64(2) || c["exit_code"] != float64(0) {
		t.Errorf("client exec.end = %v", c)
	}
	if _, ok := c["argv"]; ok {
		t.Errorf("full argv logged by default: %v", c)
	}
	if id := c["identity"].(map[string]any); id["kind"] != "client" || id["name"] != testClientName || id["serial"] != "beef" {
		t.Errorf("client exec.end identity = %v", id)
	}
	if id := a["identity"].(map[string]any); id["kind"] != "admin" || id["subject"] != testAdminFP {
		t.Errorf("admin exec.end identity = %v", id)
	}
	for _, k := range []string{"timed_out", "truncated", "disconnected", "duration_ms"} {
		if _, ok := c[k]; !ok {
			t.Errorf("exec.end lacks %q", k)
		}
	}
}

// A streamed (SSE) create still goes through the audit wrapper, which keeps
// the writer flushable, and its api line names the shed.
// TestAuditExpiredServiceCertificate: a service client's request refused
// because its certificate has expired (sent over a connection that opened
// while it was valid; the middleware's request-time check is what refuses it)
// still gets its api line, with the 401. An expired admin certificate's
// request gets none.
func TestAuditExpiredServiceCertificate(t *testing.T) {
	s, h := clientTestServer(t, newClientFakeBackend(), defaultTestPolicy())
	path := attachTestAudit(t, s)

	expired := serviceLeaf(testClientName)
	expired.NotAfter = time.Now().Add(-time.Second)
	if got := do(h, expired, "GET", "/api/sheds", ""); got.code != http.StatusUnauthorized {
		t.Fatalf("expired service cert = %d, want 401", got.code)
	}
	if got := do(h, testClientCert(testAdminFP, "control", -time.Second), "GET", "/api/sheds", ""); got.code != http.StatusUnauthorized {
		t.Fatalf("expired admin cert = %d, want 401", got.code)
	}

	api, _, other := auditLines(t, path)
	if len(other) != 0 || len(api) != 1 {
		t.Fatalf("want one api line (the client's), got api %v, other %v", api, other)
	}
	line := api[0]
	if line["status"] != float64(http.StatusUnauthorized) || line["path"] != "/api/sheds" {
		t.Errorf("line = %v", line)
	}
	if id := line["identity"].(map[string]any); id["kind"] != "client" || id["name"] != testClientName {
		t.Errorf("identity = %v", id)
	}
}

func TestAuditSSECreate(t *testing.T) {
	b := newClientFakeBackend()
	s, h := clientTestServer(t, b, defaultTestPolicy())
	path := attachTestAudit(t, s)
	got := do(h, serviceLeaf(testClientName), "POST", "/api/sheds", `{"name":"nocturn-sse","image":"itest"}`, "Accept", "text/event-stream")
	if got.code != http.StatusOK || !strings.Contains(string(got.body), "event: complete") {
		t.Fatalf("SSE create: %d %s", got.code, got.body)
	}
	api, _, _ := auditLines(t, path)
	if len(api) != 1 || api[0]["shed"] != "nocturn-sse" || api[0]["route"] != "/api/sheds" || api[0]["status"] != float64(200) {
		t.Errorf("api lines = %v", api)
	}
}

func TestAuditOffWritesNothing(t *testing.T) {
	b := newClientFakeBackend(config.Shed{Name: "nocturn-a", Status: config.StatusRunning, Owner: testClientName})
	_, h := clientTestServer(t, b, defaultTestPolicy())
	// No audit log attached: requests are served as before.
	if got := do(h, serviceLeaf(testClientName), "GET", "/api/sheds", ""); got.code != 200 {
		t.Fatalf("status %d", got.code)
	}
	if got := do(h, serviceLeaf(testClientName), "POST", "/api/sheds/nocturn-a/exec", `{"argv":["true"]}`); got.code != 200 {
		t.Fatalf("exec status %d", got.code)
	}
}

func TestAuditExecEndOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		exec     func(ctx context.Context, opts backend.ExecOptions) error
		req      config.ExecRequest
		argvFull bool
		check    func(t *testing.T, line map[string]any)
	}{
		{
			name: "exit code",
			exec: func(_ context.Context, opts backend.ExecOptions) error {
				opts.OnStarted()
				return &vmutil.ExitError{Code: 3}
			},
			req: config.ExecRequest{Argv: []string{"bash", "-lc", "exit 3"}},
			check: func(t *testing.T, l map[string]any) {
				if l["exit_code"] != float64(3) || l["argv0"] != "bash" || l["argc"] != float64(3) || l["timed_out"] != false {
					t.Errorf("line = %v", l)
				}
			},
		},
		{
			name: "full argv",
			exec: func(_ context.Context, opts backend.ExecOptions) error {
				opts.OnStarted()
				return nil
			},
			req:      config.ExecRequest{Argv: []string{"craze", "send", "a prompt"}},
			argvFull: true,
			check: func(t *testing.T, l map[string]any) {
				argv, _ := l["argv"].([]any)
				if len(argv) != 3 || argv[2] != "a prompt" {
					t.Errorf("argv = %v", l["argv"])
				}
				if _, ok := l["argv0"]; ok {
					t.Errorf("argv0 with exec_argv: full: %v", l)
				}
				if _, ok := l["argc"]; ok {
					t.Errorf("argc with exec_argv: full: %v", l)
				}
			},
		},
		{
			// The command runs as given; the audit line has URL credentials
			// redacted and never carries env.
			name: "full argv redacts credentials and omits env",
			exec: func(_ context.Context, opts backend.ExecOptions) error {
				if opts.Cmd[2] != "git clone https://u:sentinel-pass@h/r.git" || !slices.Contains(opts.Env, "TOKEN=sentinel-env") {
					return fmt.Errorf("backend got cmd %q env %q", opts.Cmd, opts.Env)
				}
				opts.OnStarted()
				return nil
			},
			req: config.ExecRequest{
				Argv: []string{"bash", "-c", "git clone https://u:sentinel-pass@h/r.git"},
				Env:  map[string]string{"TOKEN": "sentinel-env"},
			},
			argvFull: true,
			check: func(t *testing.T, l map[string]any) {
				raw, _ := json.Marshal(l)
				if strings.Contains(string(raw), "sentinel") || strings.Contains(string(raw), "TOKEN") {
					t.Errorf("audit line carries a credential or env: %s", raw)
				}
				argv, _ := l["argv"].([]any)
				if len(argv) != 3 || argv[2] != "git clone https://REDACTED@h/r.git" || l["exit_code"] != float64(0) {
					t.Errorf("line = %v", l)
				}
			},
		},
		{
			name: "argv0 redacts credentials",
			exec: func(_ context.Context, opts backend.ExecOptions) error {
				opts.OnStarted()
				return nil
			},
			req: config.ExecRequest{Argv: []string{"https://u:sentinel-pass@h/tool", "x"}},
			check: func(t *testing.T, l map[string]any) {
				if l["argv0"] != "https://REDACTED@h/tool" || l["argc"] != float64(2) {
					t.Errorf("line = %v", l)
				}
			},
		},
		{
			name: "timeout",
			exec: func(ctx context.Context, opts backend.ExecOptions) error {
				opts.OnStarted()
				<-ctx.Done()
				return ctx.Err()
			},
			req: config.ExecRequest{Argv: []string{"sleep", "30"}, TimeoutSeconds: 1},
			check: func(t *testing.T, l map[string]any) {
				if l["exit_code"] != float64(124) || l["timed_out"] != true {
					t.Errorf("line = %v", l)
				}
			},
		},
		{
			name: "truncated",
			exec: func(_ context.Context, opts backend.ExecOptions) error {
				_, _ = opts.Stdout.Write(make([]byte, 100))
				return nil
			},
			req: config.ExecRequest{Argv: []string{"yes"}, MaxOutputBytes: 10},
			check: func(t *testing.T, l map[string]any) {
				if l["truncated"] != true || l["exit_code"] != float64(0) {
					t.Errorf("line = %v", l)
				}
			},
		},
		{
			name: "pre-start failure",
			exec: func(context.Context, backend.ExecOptions) error {
				return errors.New("dial failed")
			},
			req: config.ExecRequest{Argv: []string{"true"}},
			check: func(t *testing.T, l map[string]any) {
				if l["exit_code"] != float64(-1) {
					t.Errorf("line = %v", l)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &execFakeBackend{shed: runningShed(), exec: func(ctx context.Context, _ string, opts backend.ExecOptions) error {
				return tt.exec(ctx, opts)
			}}
			s := newExecTestServer(b, nil)
			if tt.argvFull {
				s.cfg.Audit = &config.AuditConfig{Enabled: true, ExecArgv: config.AuditExecArgvFull}
			}
			path := attachTestAudit(t, s)
			postExec(t, s, "box", tt.req)
			api, execEnd, _ := auditLines(t, path)
			if len(api) != 0 {
				t.Errorf("an open-mode (admin) exec wrote api lines: %v", api)
			}
			if len(execEnd) != 1 {
				t.Fatalf("want one exec.end line, got %v", execEnd)
			}
			l := execEnd[0]
			if l["shed"] != "box" {
				t.Errorf("shed = %v", l["shed"])
			}
			ident := l["identity"].(map[string]any)
			if ident["kind"] != "admin" {
				t.Errorf("identity = %v", ident)
			}
			if _, ok := ident["subject"]; ok {
				t.Errorf("open mode has no admin subject: %v", l)
			}
			tt.check(t, l)
		})
	}
}

// A refused exec (validation, not running) never reached the backend and
// writes no exec.end line.
func TestAuditNoExecEndForRefusedExec(t *testing.T) {
	stopped := runningShed()
	stopped.Status = config.StatusStopped
	b := &execFakeBackend{shed: stopped}
	s := newExecTestServer(b, nil)
	path := attachTestAudit(t, s)
	if res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"true"}}); res.status != http.StatusConflict {
		t.Fatalf("status %d", res.status)
	}
	if res := postExec(t, s, "box", `{"argv":[]}`); res.status != http.StatusBadRequest {
		t.Fatalf("status %d", res.status)
	}
	if _, execEnd, _ := auditLines(t, path); len(execEnd) != 0 {
		t.Errorf("refused execs wrote exec.end: %v", execEnd)
	}
}
