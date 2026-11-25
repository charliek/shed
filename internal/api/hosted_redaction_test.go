package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/deliverytest"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/directory/refserver"
	"github.com/charliek/shed/internal/plugin"
)

// The hosted source end to end, and its redaction test (plan 040, section
// 3.3, criterion 7): a directory server's real API handlers, delivery
// service and runner, with the account service played by the in-tree
// reference service over loopback and reached by the real directory runtime
// (enrolment, delivery token, call). Sentinel values are placed in each
// item's content and env value, a problem's message, a token error's
// error_description, a /v1 error's message and a redirect's Location; they
// go through a create, a provision, a refused answer and each error status;
// then the server's log, its state directory (records, the directory's
// files, the audit log) and every API response are searched. Only the shed
// (the fake agent) may hold the delivered values, and a problem's message
// may appear only in last.problems of the record shown to the shed's user.

const (
	hostedFileSentinel     = "SENTINEL-hfile-3e8a1c"
	hostedEnvSentinel      = "SENTINEL-henv-6b0d42"
	hostedProblemSentinel  = "SENTINEL-hproblem-91c7"
	hostedDescSentinel     = "SENTINEL-hdesc-2f5e0a"
	hostedMessageSentinel  = "SENTINEL-hmsg-7a33d9"
	hostedLocationSentinel = "SENTINEL-hloc-c40be8"
)

// problemsMember is a record's or answer's problems list, the one place a
// problem's message may appear (the activity events carry only a count).
var problemsMember = regexp.MustCompile(`"problems":\s*\[[^\]]*\]`)

// hostedTokenTrap answers the next delivery token request (when armed) with
// a 400 whose error_description is a sentinel, and counts the delivery
// token requests.
type hostedTokenTrap struct {
	armed  atomic.Bool
	tokens atomic.Int64
}

func (h *hostedTokenTrap) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
			r.Body = io.NopCloser(bytes.NewReader(body))
			if form, err := url.ParseQuery(string(body)); err == nil && form.Get("scope") == "deliveries:request" {
				h.tokens.Add(1)
				if h.armed.Load() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprintf(w, `{"error":"invalid_request","error_description":%q}`, hostedDescSentinel)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sshKeyLine is a fresh authorized_keys line.
func sshKeyLine(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(k)))
}

// hostedService is a reference account service on loopback with its
// control directory, and a directory runtime enrolled with it whose state
// directory is under stateDir.
type hostedService struct {
	control string
	issuer  string
	trap    *hostedTokenTrap
	rt      *directory.Runtime
}

func newHostedService(t *testing.T, stateDir string) *hostedService {
	t.Helper()
	hs := &hostedService{control: t.TempDir(), trap: &hostedTokenTrap{}}
	if err := os.Chmod(hs.control, 0o700); err != nil {
		t.Fatal(err)
	}
	hs.write(t, "users.json", "[]")
	hs.write(t, "codes", "srv-1 code-one\n")
	cd, err := refserver.OpenControlDir(hs.control)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cd.Close() })
	ts := httptest.NewUnstartedServer(nil)
	hs.issuer = "http://" + ts.Listener.Addr().String()
	ref, err := refserver.New(refserver.Config{Issuer: hs.issuer, Organization: "example", Inputs: cd, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = hs.trap.wrap(ref)
	ts.Start()
	t.Cleanup(ts.Close)

	e, err := directory.Enroll(context.Background(), directory.EnrolRequest{Issuer: hs.issuer, Code: "code-one", Name: "build-1.example.test", AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir, directory.StateDirName)
	data, err := e.MarshalFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.EnsureStateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := directory.WriteStateFile(dir, directory.EnrolmentFileName, data); err != nil {
		t.Fatal(err)
	}
	lookup, err := directory.NewLookup(directory.LookupOptions{LocalAdmins: map[string]config.UserConfig{
		"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: sshKeyLine(t), Label: "desk"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	hs.rt, err = directory.NewRuntime(directory.RuntimeOptions{
		Lookup: lookup, StateDir: dir, Issuer: hs.issuer, AllowPlainHTTP: true, Refresh: time.Hour, Trust: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs.rt.Load()
	return hs
}

func (hs *hostedService) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(hs.control, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// deliver sets alice's source on the service.
func (hs *hostedService) deliver(t *testing.T, d map[string]any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"alice": d})
	if err != nil {
		t.Fatal(err)
	}
	hs.write(t, "deliveries.json", string(b))
}

// hostedItems are alice's entries: a stage 1 key file and a stage 2 project
// file holding the file sentinel, an env value that is the env sentinel.
func hostedItems(extra ...map[string]any) []map[string]any {
	file := base64.StdEncoding.EncodeToString([]byte(hostedFileSentinel))
	items := []map[string]any{
		{"name": "ssh-key", "provider": "ssh-key", "kind": "file", "stage": 1, "base": "home", "path": ".ssh/id_ed25519", "mode": "0600", "content": file, "fingerprint": "fp-1"},
		{"name": "project-env", "provider": "files", "kind": "file", "stage": 2, "base": "project", "path": ".envrc", "mode": "0600", "content": file, "fingerprint": "fp-2"},
		{"name": "api-key", "provider": "api-keys", "kind": "env", "variable": "EXAMPLE_API_KEY", "value": hostedEnvSentinel, "fingerprint": "fp-3"},
	}
	return append(items, extra...)
}

// hostedSurfaces is what the test searches.
type hostedSurfaces struct {
	log      *syncBuffer
	stateDir string
	// responses are the API's answers to alice (who may see her record's
	// problems) and others (who may not).
	own, others []string
}

func (s *hostedSurfaces) leaks(t *testing.T) []string {
	t.Helper()
	var found []string
	delivered := []string{hostedFileSentinel, base64.StdEncoding.EncodeToString([]byte(hostedFileSentinel)), hostedEnvSentinel}
	remote := []string{hostedDescSentinel, hostedMessageSentinel, hostedLocationSentinel}
	check := func(where, text string, problemsAllowed bool) {
		for _, v := range append(append([]string(nil), delivered...), remote...) {
			if strings.Contains(text, v) {
				found = append(found, fmt.Sprintf("%s holds %q", where, v))
			}
		}
		if problemsAllowed {
			text = problemsMember.ReplaceAllString(text, `"problems":[]`)
		}
		if strings.Contains(text, hostedProblemSentinel) {
			found = append(found, fmt.Sprintf("%s holds the problem message outside a record's last.problems", where))
		}
	}
	check("the server log", s.log.String(), false)
	_ = filepath.WalkDir(s.stateDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		// A shed's record (the backend's metadata) is the record: its
		// last.problems may hold the message. The audit log may not.
		check("state file "+filepath.Base(p), string(b), filepath.Base(p) != audit.FileName)
		return nil
	})
	for i, r := range s.own {
		check(fmt.Sprintf("API response %d to the shed's user", i), r, true)
	}
	for i, r := range s.others {
		check(fmt.Sprintf("API response %d to another caller", i), r, false)
	}
	return found
}

func TestHostedDeliveryRedaction(t *testing.T) {
	logs := &syncBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	stateDir := t.TempDir()
	auditLog, err := audit.Open(filepath.Join(stateDir, audit.FileName), audit.DefaultMaxBytes, audit.DefaultKeep)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })
	hs := newHostedService(t, stateDir)

	cfg := &config.ServerConfig{Name: "test-server", Auth: &config.AuthConfig{
		Mode:      config.AuthModeMTLS,
		Users:     map[string]config.UserConfig{"root": {Admin: true}},
		Directory: &config.DirectoryConfig{URL: hs.issuer},
	}}
	reg := plugin.NewRegistry()
	reg.SetServiceUsers(func(u string) bool { return cfg.UserOrigin(u) == delivery.OriginDirectory })
	svc := delivery.NewService(reg, 10*time.Second)
	svc.SetOrigin(cfg.UserOrigin)
	svc.SetHosted(hs.rt)
	svc.SetAuditLog(auditLog)
	be := &redactionBackend{svc: svc, dir: stateDir, agent: deliverytest.NewAgent(), sheds: map[string]config.Shed{}, recs: map[string]*delivery.Record{}}
	s := NewServer(be, cfg, "", reg, plugin.NewBridge(reg, true))
	s.SetUserLookup(userLookup())
	pinBus(s)
	s.SetDelivery(svc)
	s.SetAuditLog(auditLog)
	h := s.Router()

	surfaces := &hostedSurfaces{log: logs, stateDir: stateDir}
	var mu sync.Mutex
	call := func(fp, method, path, body string, stream bool) string {
		t.Helper()
		var hdr []string
		if stream {
			hdr = []string{"Accept", "text/event-stream"}
		}
		r := do(h, testClientCert(fp, "control", time.Hour), method, path, body, hdr...)
		mu.Lock()
		if fp == aliceFP {
			surfaces.own = append(surfaces.own, string(r.body))
		} else {
			surfaces.others = append(surfaces.others, string(r.body))
		}
		mu.Unlock()
		return string(r.body)
	}
	completed := func(stream string) *config.Shed {
		t.Helper()
		sc := bufio.NewScanner(strings.NewReader(stream))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
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
	provision := func() *delivery.Last {
		t.Helper()
		d := completed(call(aliceFP, http.MethodPost, "/api/sheds/widgets/provision", "", true)).Delivery
		if d == nil || d.Last == nil {
			t.Fatalf("a provision recorded no delivery: %+v", d)
		}
		return d.Last
	}

	// A create for a directory user: the service answers; every item is in
	// the shed, the record names the service, and the problem's message is
	// in the record alice is shown.
	hs.deliver(t, map[string]any{"items": hostedItems(), "problems": []map[string]any{{"provider": "files", "code": "missing_input", "message": hostedProblemSentinel}}})
	sh := completed(call(aliceFP, http.MethodPost, "/api/sheds", `{"name":"widgets","repo":"https://git.example.test/example/widgets.git"}`, true))
	if sh.Delivery == nil || sh.Delivery.Steps == nil || sh.Delivery.Steps.Stage2.State != delivery.StepDone || sh.Delivery.Last.Source == nil ||
		sh.Delivery.Last.Source.Kind != delivery.SourceKindService || sh.Delivery.Last.Source.MachineID != "" {
		t.Fatalf("create's delivery = %+v", sh.Delivery)
	}
	for _, v := range []string{hostedFileSentinel, hostedEnvSentinel} {
		if !be.agent.Wrote(v) {
			t.Fatalf("the sentinel %q never reached the shed: the test proves nothing", v)
		}
	}
	if !strings.Contains(call(aliceFP, http.MethodGet, "/api/sheds/widgets", "", false), hostedProblemSentinel) {
		t.Fatal("the problem's message is not in the record shown to the shed's user: the test proves nothing")
	}
	// An admin sees the shed's state alone.
	call(rootFP, http.MethodGet, "/api/sheds/widgets", "", false)
	if got := provision(); got.Outcome != delivery.OutcomeAccepted {
		t.Fatalf("provision = %+v", got)
	}

	// A refused answer: the same values beside a refused target.
	refused, _ := json.Marshal(map[string]any{"version": 2, "id": "0123456789abcdef0123456789abcdef", "problems": []any{},
		"items": hostedItems(map[string]any{"name": "bad", "provider": "files", "kind": "file", "stage": 1, "base": "home", "path": ".ssh/authorized_keys",
			"mode": "0600", "content": base64.StdEncoding.EncodeToString([]byte(hostedFileSentinel)), "fingerprint": "fp-4"})})
	hs.deliver(t, map[string]any{"mode": "body", "body_base64": base64.StdEncoding.EncodeToString(refused)})
	if got := provision(); got.Outcome != delivery.OutcomeRefused || got.Failure.Rule != delivery.RuleRefusedTarget {
		t.Fatalf("the refused answer = %+v", got)
	}

	// Each error status, with a /v1 error message and a redirect's Location
	// that are sentinels.
	for _, tc := range []struct {
		status int
		code   string
		want   delivery.StepReason
	}{
		{400, "BAD_REQUEST", delivery.ReasonSourceRefused},
		{403, "SERVER_DISABLED", delivery.ReasonSourceRefused},
		{404, "NOT_FOUND", delivery.ReasonSourceRefused},
		{410, "DEADLINE_PASSED", delivery.ReasonSourceTimeout},
		{413, "PAYLOAD_TOO_LARGE", delivery.ReasonSourceRefused},
		{418, "TEAPOT", delivery.ReasonSourceRefused},
		{429, "RATE_LIMITED", delivery.ReasonSourceOffline},
		{500, "INTERNAL", delivery.ReasonSourceOffline},
		{503, "UNAVAILABLE", delivery.ReasonSourceOffline},
		{302, "", delivery.ReasonSourceRefused},
	} {
		d := map[string]any{"mode": "status", "status": tc.status,
			"headers": map[string]string{"Location": "https://elsewhere.example.test/" + hostedLocationSentinel, "X-Detail": hostedMessageSentinel},
			"body":    map[string]any{"error": map[string]any{"code": tc.code, "message": hostedMessageSentinel}}}
		hs.deliver(t, d)
		if got := provision(); got.Outcome != delivery.OutcomeNoAnswer || got.Failure == nil || got.Failure.Reason != tc.want {
			t.Fatalf("%d %s = %+v, want %s", tc.status, tc.code, got, tc.want)
		}
	}
	// An unparseable error body holding a sentinel.
	text := "<html>" + hostedMessageSentinel + "</html>"
	hs.deliver(t, map[string]any{"mode": "status", "status": 404, "headers": map[string]string{"Content-Type": "text/html"}, "text": text})
	if got := provision(); got.Failure == nil || got.Failure.Reason != delivery.ReasonSourceRefused {
		t.Fatalf("an unparseable 404 = %+v", got)
	}
	// A 401 makes the server sign in again, and that token request is
	// refused with an error_description that is a sentinel.
	hs.trap.armed.Store(true)
	hs.deliver(t, map[string]any{"mode": "status", "status": 401, "body": map[string]any{"error": map[string]any{"code": "invalid_token", "message": hostedMessageSentinel}}})
	if got := provision(); got.Failure == nil || got.Failure.Reason != delivery.ReasonSourceRefused {
		t.Fatalf("a refused sign-in = %+v", got)
	}
	if hs.trap.tokens.Load() < 2 {
		t.Fatal("the refused sign-in never happened: the test proves nothing")
	}
	hs.trap.armed.Store(false)
	// An answer that echoes the bearer.
	hs.deliver(t, map[string]any{"mode": "echo"})
	if got := provision(); got.Outcome != delivery.OutcomeRefused || got.Failure.Rule != delivery.RuleEchoesCredential {
		t.Fatalf("an echo = %+v", got)
	}

	// Every way the API shows a shed and its deliveries.
	for _, fp := range []string{aliceFP, rootFP} {
		call(fp, http.MethodGet, "/api/sheds", "", false)
		call(fp, http.MethodGet, "/api/sheds/widgets", "", false)
		call(fp, http.MethodGet, "/api/overview", "", false)
		call(fp, http.MethodGet, "/api/activity", "", false)
		call(fp, http.MethodGet, "/api/activity?shed=widgets", "", false)
		call(fp, http.MethodGet, "/api/sources", "", false)
	}
	call(rootFP, http.MethodGet, "/api/sources?user=alice", "", false)

	lines, err := readAuditLines(filepath.Join(stateDir, audit.FileName), auditTestMaxLine)
	if err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	for _, l := range lines {
		if ev, _ := l["event"].(string); ev == audit.EventDelivery {
			deliveries++
			d, _ := l["delivery"].(map[string]any)
			if src, _ := d["source"].(map[string]any); src["kind"] != "service" {
				t.Errorf("a delivery event's source = %v, want the service", d["source"])
			}
		}
	}
	if deliveries != 16 {
		t.Errorf("%d delivery events, want 16", deliveries)
	}
	if leaks := surfaces.leaks(t); len(leaks) > 0 {
		t.Fatalf("remote text or delivered values leaked:\n  %s", strings.Join(leaks, "\n  "))
	}
}
