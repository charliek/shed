package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/deliverytest"
	"github.com/charliek/shed/internal/delivery/refsource"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/users/userstest"
	"github.com/charliek/shed/internal/vmutil"
)

// The redaction test (plan 039 §3.9, criterion 3, the server's half):
// sentinel values go through a create, a provision, a refused answer, a
// failed write and a malformed writer reply, end to end (the reference
// source over the real delivery routes, the real service and runner, a
// backend that keeps its records as files and a fake agent as the shed),
// and then the server's log, its state directory, the audit log and every
// API response are searched for them. Only the shed (the fake agent) may
// hold them.
//
// redactionSurfaces lists what is searched; A4 adds its audit events and
// the activity route's answers to it.

// redactionSentinels are the delivered values: a file's content and an env
// value, each also searched in the forms it travels in.
var redactionSentinels = []string{"SENTINEL-file-4c1d9e", "SENTINEL-env-8a2b7f", redactionYAMLSentinel}

// redactionYAMLSentinel is a value in a delivered .shed/provision.yaml that
// does not parse (a timeout that is not a duration). It is short on
// purpose: a YAML error quotes a value of up to ten bytes whole.
const redactionYAMLSentinel = "yQ7zKx2W"

// redactionYAML is that file's content.
var redactionYAML = "timeout: " + redactionYAMLSentinel + "\n"

// redactionForms is every form a sentinel is searched in: as it is, and as
// the base64 a file's content travels in.
func redactionForms() []string {
	var out []string
	for _, s := range redactionSentinels {
		out = append(out, s, base64.StdEncoding.EncodeToString([]byte(s)))
	}
	// Inside a longer content base64 shifts with the offset: the YAML
	// file travels as its own whole content.
	return append(out, base64.StdEncoding.EncodeToString([]byte(redactionYAML)))
}

// redactionBackend is a shed backend whose sheds are a fake agent each and
// whose records are files under dir: it runs the real delivery runner.
type redactionBackend struct {
	routesFakeBackend
	svc   *delivery.Service
	dir   string
	agent *deliverytest.Agent
	mu    sync.Mutex
	sheds map[string]config.Shed
	recs  map[string]*delivery.Record
}

func (b *redactionBackend) Type() backend.Type { return backend.TypeFirecracker }

func (b *redactionBackend) host(name string) *redactionHost {
	return &redactionHost{b: b, name: name}
}

func (b *redactionBackend) run(ctx context.Context, name string, reason delivery.Reason) error {
	b.mu.Lock()
	sh := b.sheds[name]
	prior := b.recs[name].Clone()
	b.mu.Unlock()
	r := &delivery.Run{
		Service: b.svc, Reason: reason,
		Params:  delivery.Params{User: sh.User, Shed: config.DeliveryRequestShed(name, sh.ID, delivery.BackendFirecracker, sh.Repo, "", sh.LandingDir)},
		HasRepo: sh.Repo != "", Prior: prior, Host: b.host(name), Report: backend.DeliveryProgress(ctx),
	}
	_, err := r.Execute(ctx)
	return err
}

func (b *redactionBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	id, _ := config.NewShedID()
	sh := config.Shed{
		Name: req.Name, ID: id, User: req.User, Status: config.StatusRunning, CreatedAt: time.Now().UTC().Round(time.Second),
		Repo: req.Repo, LandingDir: config.HomePath + "/widgets", Backend: config.BackendFirecracker,
	}
	b.mu.Lock()
	b.sheds[req.Name] = sh
	b.recs[req.Name] = delivery.NewRecord()
	b.mu.Unlock()
	if err := b.run(ctx, req.Name, delivery.ReasonCreate); err != nil {
		return nil, err
	}
	return b.GetShed(ctx, req.Name)
}

func (b *redactionBackend) BeginProvision(_ context.Context, name string) (backend.ProvisionRun, error) {
	return redactionRun{b: b, name: name}, nil
}

type redactionRun struct {
	b    *redactionBackend
	name string
}

func (r redactionRun) Run(ctx context.Context) (*config.Shed, error) {
	if err := r.b.run(ctx, r.name, delivery.ReasonProvision); err != nil {
		return nil, err
	}
	return r.b.GetShed(ctx, r.name)
}

func (r redactionRun) Release() {}

func (b *redactionBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sh, ok := b.sheds[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	sh.Delivery = config.NewShedDelivery(b.recs[name], time.Now())
	return &sh, nil
}

func (b *redactionBackend) ListSheds(ctx context.Context) ([]config.Shed, error) {
	b.mu.Lock()
	var names []string
	for n := range b.sheds {
		names = append(names, n)
	}
	b.mu.Unlock()
	var out []config.Shed
	for _, n := range names {
		sh, _ := b.GetShed(ctx, n)
		out = append(out, *sh)
	}
	return out, nil
}

type redactionHost struct {
	b    *redactionBackend
	name string
}

func (h *redactionHost) Step(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

func (h *redactionHost) Save(_ context.Context, rec *delivery.Record) error {
	h.b.mu.Lock()
	h.b.recs[h.name] = rec.Clone()
	h.b.mu.Unlock()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.b.dir, h.name+".json"), data, 0o600)
}

func (h *redactionHost) Agent() delivery.Agent { return h.b.agent }

func (h *redactionHost) Clone(context.Context, bool) delivery.Step {
	return delivery.Step{State: delivery.StepDone}
}

// Hooks runs the real hooks step (vmutil's: the config read through the
// agent, parsed, and the install hook) over a guest whose project directory
// holds what was delivered to it.
func (h *redactionHost) Hooks(ctx context.Context, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool) {
	p := vmutil.NewProvisioner(vmutil.NewAgentClient(&guestFiles{agent: h.b.agent}, 1024, 1026), h.name)
	p.SetWorkDir(config.HomePath + "/widgets")
	p.SetOutput(io.Discard, io.Discard)
	return p.HooksStep(ctx, install, startup)
}

// guestFiles is a shed's agent as far as reading its project directory
// goes: a read of a file answers what the last stage 2 wrote there, or "no
// such file"; every other command succeeds and prints nothing.
type guestFiles struct{ agent *deliverytest.Agent }

func (g *guestFiles) Dial(_ context.Context, _ uint32) (net.Conn, error) {
	client, server := net.Pipe()
	go g.serve(server)
	return client, nil
}

func (g *guestFiles) serve(conn net.Conn) {
	defer conn.Close()
	_, data, err := agentproto.ReadMessage(conn)
	if err != nil {
		return
	}
	var req agentproto.ExecRequest
	_ = json.Unmarshal(data, &req)
	_, _, _ = agentproto.ReadMessage(conn) // stdin's end
	// The provision config read: sh -c <probe> sh <config path> <work dir>.
	if len(req.Cmd) == 6 && req.Cmd[0] == "sh" && strings.HasSuffix(req.Cmd[4], "/.shed/provision.yaml") {
		if content, ok := g.file(req.Cmd[4]); ok {
			_ = agentproto.WriteData(conn, content)
			_ = agentproto.WriteExitCode(conn, 0)
			return
		}
		_ = agentproto.WriteExitCode(conn, 66)
		return
	}
	_ = agentproto.WriteExitCode(conn, 0)
}

// file is what the last stage 2 wrote at path.
func (g *guestFiles) file(path string) ([]byte, bool) {
	var out []byte
	found := false
	for _, w := range g.agent.Writes() {
		for _, f := range w.Files {
			if f.Base == delivery.BaseProject && w.ProjectDir+"/"+f.Path == path {
				out, found = f.Content, true
			}
		}
	}
	return out, found
}

// redactionCanned is the reference source's answer: a stage 1 file and a
// stage 2 project file whose content is the file sentinel, an env value
// that is the env sentinel. extra adds items (a refused target).
func redactionCanned(extra ...map[string]any) refsource.Canned {
	file := base64.StdEncoding.EncodeToString([]byte(redactionSentinels[0]))
	items := []map[string]any{
		{"name": "ssh-key", "provider": "ssh-key", "kind": "file", "stage": 1, "base": "home", "path": ".ssh/id_ed25519", "mode": "0600", "content": file, "fingerprint": "fp-1"},
		{"name": "project-env", "provider": "files", "kind": "file", "stage": 2, "base": "project", "path": ".envrc", "mode": "0600", "content": file, "fingerprint": "fp-2"},
		{"name": "api-key", "provider": "api-keys", "kind": "env", "variable": "EXAMPLE_API_KEY", "value": redactionSentinels[1], "fingerprint": "fp-3"},
		{"name": "provision-yaml", "provider": "files", "kind": "file", "stage": 2, "base": "project", "path": ".shed/provision.yaml", "mode": "0644",
			"content": base64.StdEncoding.EncodeToString([]byte(redactionYAML)), "fingerprint": "fp-5"},
	}
	items = append(items, extra...)
	raw, _ := json.Marshal(items)
	return refsource.Canned{Items: raw}
}

// redactionSurfaces is everything the test searches: the server's log, every
// file under its state directory (the records and the audit log), every API
// response, and what the source heard back.
type redactionSurfaces struct {
	log       *syncBuffer
	stateDir  string
	responses []string
	events    []string
}

func (r *redactionSurfaces) leaks(t *testing.T) []string {
	t.Helper()
	var found []string
	check := func(where, text string) {
		for _, f := range redactionForms() {
			if strings.Contains(text, f) {
				found = append(found, fmt.Sprintf("%s holds %q", where, f))
			}
		}
	}
	check("the server log", r.log.String())
	_ = filepath.WalkDir(r.stateDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		check("state file "+filepath.Base(p), string(b))
		return nil
	})
	for i, resp := range r.responses {
		check(fmt.Sprintf("API response %d", i), resp)
	}
	for i, ev := range r.events {
		check(fmt.Sprintf("source event %d", i), ev)
	}
	return found
}

// syncBuffer is a log sink safe for the server's goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestDeliveryRedaction(t *testing.T) {
	logs := &syncBuffer{}
	log.SetOutput(io.MultiWriter(logs))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	stateDir := t.TempDir()
	auditLog, err := audit.Open(filepath.Join(stateDir, audit.FileName), audit.DefaultMaxBytes, audit.DefaultKeep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })

	reg := plugin.NewRegistry()
	svc := delivery.NewService(reg, 10*time.Second)
	be := &redactionBackend{svc: svc, dir: stateDir, agent: deliverytest.NewAgent(), sheds: map[string]config.Shed{}, recs: map[string]*delivery.Record{}}
	s := NewServer(be, &config.ServerConfig{Name: "test-server", Auth: &config.AuthConfig{Mode: config.AuthModeOpen}}, "", reg, plugin.NewBridge(reg, true))
	s.SetUserLookup(userstest.Implicit())
	pinBus(s)
	s.SetDelivery(svc)
	s.SetAuditLog(auditLog)
	ts := httptest.NewServer(s.Router())
	t.Cleanup(ts.Close)

	surfaces := &redactionSurfaces{log: logs, stateDir: stateDir}
	machine := refsource.NewMachineID()
	var evMu sync.Mutex
	source := func(canned refsource.Canned) func() {
		src := startSource(t, ts.URL, refsource.Config{MachineID: machine, Canned: canned, Report: func(e refsource.Event) {
			b, _ := json.Marshal(e)
			evMu.Lock()
			surfaces.events = append(surfaces.events, string(b))
			evMu.Unlock()
		}})
		src.next("stream")
		return src.stop
	}
	call := func(method, path, body string, stream bool) string {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if stream {
			req.Header.Set("Accept", "text/event-stream")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		surfaces.responses = append(surfaces.responses, string(b))
		return string(b)
	}
	completed := func(stream string) *config.Shed {
		t.Helper()
		sc := bufio.NewScanner(strings.NewReader(stream))
		event := ""
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				event = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") && event == "complete" {
				var sh config.Shed
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &sh); err != nil {
					t.Fatal(err)
				}
				return &sh
			}
		}
		t.Fatalf("no complete event in %s", stream)
		return nil
	}

	// A create, and a provision, with the source answering.
	stop := source(redactionCanned())
	sh := completed(call(http.MethodPost, "/api/sheds", `{"name":"widgets","repo":"https://git.example.test/example/widgets.git"}`, true))
	if sh.Delivery == nil || sh.Delivery.Steps == nil || sh.Delivery.Steps.Stage2.State != delivery.StepDone {
		t.Fatalf("create's delivery = %+v", sh.Delivery)
	}
	for _, v := range redactionSentinels {
		if !be.agent.Wrote(v) {
			t.Fatalf("the sentinel %q never reached the shed: the test proves nothing", v)
		}
	}
	// The delivered config was read and did not load: the install step
	// failed through the real config path.
	if st := sh.Delivery.Steps.Install; st.State != delivery.StepFailed || st.Reason != delivery.ReasonHookFailed {
		t.Fatalf("install step = %+v, want failed hook_failed (the delivered config not loading)", st)
	}
	completed(call(http.MethodPost, "/api/sheds/widgets/provision", "", true))

	// A failed write, then a malformed writer reply (one that echoes the
	// bundle back).
	be.agent.Mode, be.agent.FailItem = deliverytest.WriteFailAt, "ssh-key"
	if d := completed(call(http.MethodPost, "/api/sheds/widgets/provision", "", true)).Delivery; d.Last.Failure == nil {
		t.Fatalf("a failed write recorded no failure: %+v", d)
	}
	be.agent.Mode = deliverytest.WriteMalformed
	if d := completed(call(http.MethodPost, "/api/sheds/widgets/provision", "", true)).Delivery; d.Last.Failure == nil {
		t.Fatalf("a malformed reply recorded no failure: %+v", d)
	}
	be.agent.Mode = deliverytest.WriteOK
	stop()

	// A refused answer: the same values beside a refused target.
	stop = source(redactionCanned(map[string]any{
		"name": "bad", "provider": "files", "kind": "file", "stage": 1, "base": "home", "path": ".ssh/authorized_keys", "mode": "0600",
		"content": base64.StdEncoding.EncodeToString([]byte(redactionSentinels[0])), "fingerprint": "fp-4",
	}))
	if d := completed(call(http.MethodPost, "/api/sheds/widgets/provision", "", true)).Delivery; d.Last.Outcome != delivery.OutcomeRefused {
		t.Fatalf("the refused answer was not refused: %+v", d.Last)
	}
	stop()

	// Every way the API shows a shed, as JSON and as a stream.
	call(http.MethodPost, "/api/sheds/widgets/provision", "", false)
	call(http.MethodGet, "/api/sheds", "", false)
	call(http.MethodGet, "/api/sheds/widgets", "", false)
	call(http.MethodGet, "/api/overview", "", false)

	if _, err := os.Stat(filepath.Join(stateDir, audit.FileName)); err != nil {
		t.Fatalf("the audit log is not where it is searched: %v", err)
	}
	// The delivered config's failure is reported with its line only.
	if !strings.Contains(logs.String(), "did not load (it does not parse), at line 1") {
		t.Error("the delivered config's parse failure was not logged with its line")
	}
	if leaks := surfaces.leaks(t); len(leaks) > 0 {
		t.Fatalf("delivered values leaked:\n  %s", strings.Join(leaks, "\n  "))
	}
}
