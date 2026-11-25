package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/plugin"
)

// The activity feed (GET /api/activity, plan 039 §3.6): the guard's
// lifecycle events and the runner's delivery events, read back as each
// caller may see them.

// activityBackend keeps sheds in a map, gives each create a new incarnation,
// and runs a delivery (not applicable here: no source is bound) at create and
// at provision through the real runner, so the runner's event is written.
type activityBackend struct {
	routesFakeBackend
	svc   *delivery.Service
	mu    sync.Mutex
	sheds map[string]config.Shed
	clock time.Time
}

func newActivityBackend() *activityBackend {
	return &activityBackend{sheds: map[string]config.Shed{}, clock: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
}

func (b *activityBackend) Type() backend.Type { return backend.TypeFirecracker }

func (b *activityBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sheds[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	return &s, nil
}

func (b *activityBackend) ListSheds(context.Context) ([]config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []config.Shed
	for _, s := range b.sheds {
		out = append(out, s)
	}
	return out, nil
}

// put adds a shed directly (one the test needs without a create).
func (b *activityBackend) put(s config.Shed) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clock = b.clock.Add(time.Second)
	s.CreatedAt, s.Status = b.clock, config.StatusRunning
	b.sheds[s.Name] = s
}

func (b *activityBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	b.mu.Lock()
	if _, ok := b.sheds[req.Name]; ok {
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", config.ErrShedAlreadyExistsSentinel, req.Name)
	}
	b.clock = b.clock.Add(time.Second)
	sh := config.Shed{Name: req.Name, ID: req.ID, User: req.User, Owner: req.Owner, Status: config.StatusRunning, CreatedAt: b.clock}
	b.sheds[req.Name] = sh
	b.mu.Unlock()
	if err := b.deliver(ctx, sh, delivery.ReasonCreate); err != nil {
		return nil, err
	}
	return &sh, nil
}

func (b *activityBackend) deliver(ctx context.Context, sh config.Shed, reason delivery.Reason) error {
	run := &delivery.Run{
		Service: b.svc, Reason: reason,
		Params: delivery.Params{Origin: delivery.OriginLocal,
			User: sh.User, Owner: sh.Owner, ServiceClient: sh.Owner != "",
			Shed: config.DeliveryRequestShed(sh.Name, sh.ID, delivery.BackendFirecracker, "", "", ""),
		},
		Host: activityHost{},
	}
	_, err := run.Execute(ctx)
	return err
}

// mutate applies fn to the named shed of the incarnation ctx requires.
func (b *activityBackend) mutate(ctx context.Context, name string, fn func(*config.Shed)) (*config.Shed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sheds[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, name)
	}
	if err := backend.CheckCreatedAt(ctx, name, s.CreatedAt); err != nil {
		return nil, err
	}
	if fn != nil {
		fn(&s)
	}
	b.sheds[name] = s
	return &s, nil
}

func (b *activityBackend) DeleteShed(ctx context.Context, name string) error {
	if _, err := b.mutate(ctx, name, nil); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.sheds, name)
	b.mu.Unlock()
	return nil
}

func (b *activityBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.mutate(ctx, name, func(s *config.Shed) { s.Status = config.StatusRunning })
}

func (b *activityBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.mutate(ctx, name, func(s *config.Shed) { s.Status = config.StatusStopped })
}

func (b *activityBackend) ResetShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.mutate(ctx, name, nil)
}

func (b *activityBackend) AssignUser(ctx context.Context, name string, req config.AssignShedRequest) (*config.Shed, error) {
	var aerr error
	s, err := b.mutate(ctx, name, func(s *config.Shed) {
		if aerr = backend.CheckAssignable(s, req); aerr == nil {
			s.User = req.User
		}
	})
	if err != nil {
		return nil, err
	}
	return s, aerr
}

func (b *activityBackend) BeginProvision(_ context.Context, name string) (backend.ProvisionRun, error) {
	return activityRun{b: b, name: name}, nil
}

type activityRun struct {
	b    *activityBackend
	name string
}

func (r activityRun) Run(ctx context.Context) (*config.Shed, error) {
	sh, err := r.b.GetShed(ctx, r.name)
	if err != nil {
		return nil, err
	}
	if err := r.b.deliver(ctx, *sh, delivery.ReasonProvision); err != nil {
		return nil, err
	}
	return sh, nil
}

func (activityRun) Release() {}

// activityHost is a shed with nothing to clone and no hooks.
type activityHost struct{}

func (activityHost) Step(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }
func (activityHost) Save(context.Context, *delivery.Record) error                   { return nil }
func (activityHost) Agent() delivery.Agent                                          { return nil }
func (activityHost) Clone(context.Context, bool) delivery.Step {
	return delivery.Step{State: delivery.StepSkipped, Reason: delivery.ReasonNoRepo}
}
func (activityHost) Hooks(context.Context, bool, func(delivery.Step, bool) bool) (delivery.Step, bool) {
	return delivery.Step{State: delivery.StepSkipped, Reason: delivery.ReasonNoHook}, false
}

// activityTestServer is an mtls server with the users alice, bob and root
// (an admin), the service client testClientName, a delivery service and an
// audit log. It returns the log's path.
func activityTestServer(t *testing.T, b *activityBackend) (*Server, http.Handler, *audit.Log) {
	t.Helper()
	cfg := &config.ServerConfig{
		Name:           "test-server",
		DefaultBackend: config.BackendFirecracker,
		Firecracker:    &config.FirecrackerConfig{DefaultCPUs: 2, DefaultMemoryMB: 4096},
		Auth: &config.AuthConfig{
			Mode:    config.AuthModeMTLS,
			Clients: map[string]config.ServiceClientConfig{testClientName: {Policy: config.ServiceClientPolicy{NamePrefix: "nocturn-", MaxSheds: 5}}},
		},
	}
	reg := plugin.NewRegistry()
	s := NewServer(b, cfg, "", reg, plugin.NewBridge(reg, true))
	s.SetUserLookup(userLookup())
	pinBus(s)
	b.svc = delivery.NewService(reg, 10*time.Second)
	s.SetDelivery(b.svc)
	l, err := audit.Open(filepath.Join(t.TempDir(), audit.FileName), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	s.SetAuditLog(l)
	return s, s.Router(), l
}

// activityPage is one page of the feed, as leaf's caller reads it.
func activityPage(t *testing.T, h http.Handler, leafFP string, client bool, query string) ActivityResponse {
	t.Helper()
	leaf := testClientCert(leafFP, "control", time.Hour)
	if client {
		leaf = serviceLeaf(testClientName)
	}
	r := do(h, leaf, "GET", "/api/activity"+query, "")
	if r.code != http.StatusOK {
		t.Fatalf("GET /api/activity%s = %d %s", query, r.code, r.body)
	}
	var resp ActivityResponse
	if err := json.Unmarshal(r.body, &resp); err != nil {
		t.Fatalf("activity JSON %s: %v", r.body, err)
	}
	return resp
}

// activityAll reads every page.
func activityAll(t *testing.T, h http.Handler, fp string, client bool, query string) []ActivityEvent {
	t.Helper()
	var out []ActivityEvent
	since := ""
	for i := 0; ; i++ {
		if i > 1000 {
			t.Fatal("the feed does not end")
		}
		q := query
		if since != "" {
			sep := "?"
			if q != "" {
				sep = "&"
			}
			q += sep + "since=" + since
		}
		page := activityPage(t, h, fp, client, q)
		out = append(out, page.Events...)
		if page.Next == "" {
			return out
		}
		since = page.Next
	}
}

// summary is an event as "kind shed id user actor result", for comparing.
func summary(ev ActivityEvent) string {
	actor := ev.Actor.User
	switch {
	case ev.Actor.Client != "":
		actor = "client:" + ev.Actor.Client
	case ev.Actor.Kind != audit.KindUser:
		actor = ev.Actor.Kind
	}
	res := ev.Result
	if ev.Delivery != nil {
		res = ev.Delivery.Reason + "/" + ev.Delivery.State
	}
	return strings.Join([]string{ev.Kind, ev.Shed.Name, ev.Shed.ID, ev.Shed.User + ev.Shed.Owner, actor, res}, " ")
}

func summaries(evs []ActivityEvent) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, summary(ev))
	}
	return out
}

// TestActivityVisibility drives the lifecycle through the API, and reads the
// feed back as each caller: a person sees their own sheds' events, by whose
// the shed was when each was written, so a deleted shed whose name bob took
// keeps alice's events hers and bob's his; an admin sees all, the older
// kinds included; a service client sees its own sheds'; a refused call
// writes nothing.
func TestActivityVisibility(t *testing.T) {
	b := newActivityBackend()
	_, h, l := activityTestServer(t, b)
	created := func(fp string, client bool, body string) string {
		t.Helper()
		leaf := testClientCert(fp, "control", time.Hour)
		if client {
			leaf = serviceLeaf(testClientName)
		}
		r := do(h, leaf, "POST", "/api/sheds", body)
		if r.code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", body, r.code, r.body)
		}
		var sh config.Shed
		_ = json.Unmarshal(r.body, &sh)
		if !config.ValidShedID(sh.ID) {
			t.Fatalf("created shed has no id: %s", r.body)
		}
		return sh.ID
	}
	ok := func(r apiResp, what string) {
		t.Helper()
		if r.code >= 300 {
			t.Fatalf("%s = %d %s", what, r.code, r.body)
		}
	}

	aliceWeb := created(aliceFP, false, `{"name":"web"}`)
	ok(doAs(h, aliceFP, "POST", "/api/sheds/web/stop", ""), "alice stop")
	ok(doAs(h, aliceFP, "POST", "/api/sheds/web/start", ""), "alice start")
	ok(doAs(h, rootFP, "POST", "/api/sheds/web/stop", ""), "root stops alice's shed")
	ok(doAs(h, aliceFP, "POST", "/api/sheds/web/start", ""), "alice start")
	ok(doAs(h, aliceFP, "POST", "/api/sheds/web/provision", ""), "alice provision")
	// Refused: bob may not touch alice's shed; nothing is written.
	if r := doAs(h, bobFP, "DELETE", "/api/sheds/web", ""); r.code != http.StatusNotFound {
		t.Fatalf("bob's delete of alice's shed = %d", r.code)
	}
	ok(doAs(h, aliceFP, "DELETE", "/api/sheds/web", ""), "alice delete")
	// bob takes the name.
	bobWeb := created(bobFP, false, `{"name":"web"}`)
	ok(doAs(h, bobFP, "POST", "/api/sheds/web/reset", ""), "bob reset")
	// An unassigned shed, given to bob.
	b.put(config.Shed{Name: "free", ID: "freefreefreefreefreefreefr"})
	ok(doAs(h, rootFP, "POST", "/api/sheds/free/assign", `{"user":"bob"}`), "assign")
	// root's own, and the client's.
	rootOps := created(rootFP, false, `{"name":"ops"}`)
	clientShed := created("", true, `{"name":"nocturn-a"}`)
	// An older kind about alice's shed name: admin only.
	if err := l.Record(audit.ExecEndEvent{Event: audit.EventExecEnd, Identity: audit.UserIdentity("bob", false, false, false, "", ""), Shed: "web", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}

	aliceWant := []string{
		"shed.delete web " + aliceWeb + " alice alice ok",
		"shed.provision.end web " + aliceWeb + " alice alice ok",
		"delivery web " + aliceWeb + " alice alice provision/not_applicable",
		"shed.provision.start web " + aliceWeb + " alice alice ok",
		"shed.start web " + aliceWeb + " alice alice ok",
		"shed.stop web " + aliceWeb + " alice root ok",
		"shed.start web " + aliceWeb + " alice alice ok",
		"shed.stop web " + aliceWeb + " alice alice ok",
		// The create's delivery runs inside the create: its event comes
		// first, and the create's own once the create has returned.
		"shed.create web " + aliceWeb + " alice alice ok",
		"delivery web " + aliceWeb + " alice alice create/not_applicable",
	}
	bobWant := []string{
		"shed.assign free freefreefreefreefreefreefr bob root ok",
		"shed.reset web " + bobWeb + " bob bob ok",
		"shed.create web " + bobWeb + " bob bob ok",
		"delivery web " + bobWeb + " bob bob create/not_applicable",
	}
	clientWant := []string{
		"shed.create nocturn-a " + clientShed + " " + testClientName + " client:" + testClientName + " ok",
		"delivery nocturn-a " + clientShed + " " + testClientName + " client:" + testClientName + " create/not_applicable",
	}

	if got := summaries(activityAll(t, h, aliceFP, false, "")); !slices.Equal(got, aliceWant) {
		t.Errorf("alice sees:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(aliceWant, "\n  "))
	}
	if got := summaries(activityAll(t, h, aliceFP, false, "?shed=web")); !slices.Equal(got, aliceWant) {
		t.Errorf("alice's shed=web: the new owner's events must not show:\n  %s", strings.Join(got, "\n  "))
	}
	if got := summaries(activityAll(t, h, aliceFP, false, "?shed="+aliceWeb)); !slices.Equal(got, aliceWant) {
		t.Errorf("alice's shed=<id> sees:\n  %s", strings.Join(got, "\n  "))
	}
	if got := activityAll(t, h, aliceFP, false, "?shed="+bobWeb); len(got) != 0 {
		t.Errorf("alice sees bob's shed by its id: %v", summaries(got))
	}
	if got := summaries(activityAll(t, h, bobFP, false, "")); !slices.Equal(got, bobWant) {
		t.Errorf("bob sees:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(bobWant, "\n  "))
	}
	if got := summaries(activityAll(t, h, bobFP, false, "?shed=web")); !slices.Equal(got, bobWant[1:]) {
		t.Errorf("bob's shed=web: the old owner's events must not show:\n  %s", strings.Join(got, "\n  "))
	}
	if got := summaries(activityAll(t, h, "", true, "")); !slices.Equal(got, clientWant) {
		t.Errorf("the client sees:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(clientWant, "\n  "))
	}

	// The admin sees everything, the older kinds included (the client's
	// requests wrote api lines), newest first. A delivery of another user's
	// shed is its state alone.
	root := activityAll(t, h, rootFP, false, "")
	var rootNoAPI []string
	for _, ev := range root {
		if ev.Kind != audit.EventAPI {
			rootNoAPI = append(rootNoAPI, summary(ev))
		}
	}
	stateOnly := func(evs []string) []string {
		var out []string
		for _, e := range evs {
			out = append(out, strings.NewReplacer(" create/", " /", " provision/", " /").Replace(e))
		}
		return out
	}
	rootWant := append([]string{
		"exec.end web   bob ",
	}, clientWant...)
	rootWant = append(rootWant, "shed.create ops "+rootOps+" root root ok", "delivery ops "+rootOps+" root root create/not_applicable")
	rootWant = append(rootWant, stateOnly(bobWant)...)
	rootWant = append(rootWant, stateOnly(aliceWant)...)
	if !slices.Equal(rootNoAPI, rootWant) {
		t.Errorf("root sees:\n  %s\nwant:\n  %s", strings.Join(rootNoAPI, "\n  "), strings.Join(rootWant, "\n  "))
	}
	for i := 1; i < len(root); i++ {
		a, _ := audit.ParseCursor(root[i-1].ID)
		b, _ := audit.ParseCursor(root[i].ID)
		if a.Seq <= b.Seq {
			t.Fatalf("not newest first at %d: %s then %s", i, root[i-1].ID, root[i].ID)
		}
	}
	// The older kind is shown to nobody else, whatever its shed is called.
	for _, fp := range []string{aliceFP, bobFP} {
		for _, ev := range activityAll(t, h, fp, false, "") {
			if !audit.IsShedEventKind(ev.Kind) {
				t.Errorf("a person sees an older kind: %+v", ev)
			}
		}
	}
	// The refused delete wrote no event.
	for _, ev := range root {
		if ev.Kind == audit.EventShedDelete && ev.Actor.User == "bob" {
			t.Errorf("a refused call was recorded: %+v", ev)
		}
	}
}

// TestActivityOlderKindsAdminOnly: every older kind, whatever it names, is
// shown to an admin only (it carries no shed id and no shed user).
func TestActivityOlderKindsAdminOnly(t *testing.T) {
	b := newActivityBackend()
	_, h, l := activityTestServer(t, b)
	bob := audit.UserIdentity("bob", false, false, false, "", "")
	for _, ev := range []any{
		audit.APIEvent{Event: audit.EventAPI, Identity: audit.ClientIdentity(testClientName, "1"), Method: "GET", Path: "/api/sheds", Shed: "nocturn-a"},
		audit.ExecEndEvent{Event: audit.EventExecEnd, Identity: bob, Shed: "web"},
		audit.ReapEvent{Event: audit.EventReap, Identity: audit.SystemIdentity(), Shed: "nocturn-a", Owner: testClientName},
		audit.SSHEvent{Event: audit.EventSSH, Identity: bob, Shed: "nocturn-a", Owner: testClientName, Channel: audit.SSHChannelShell},
		audit.ConnectEvent{Event: audit.EventConnect, Identity: bob, Shed: "nocturn-a", Owner: testClientName, Port: 80},
	} {
		if err := l.Record(ev); err != nil {
			t.Fatal(err)
		}
	}
	// root first: the client's read below writes an api line of its own.
	got := activityAll(t, h, rootFP, false, "")
	for _, who := range []struct {
		name   string
		fp     string
		client bool
	}{{"alice", aliceFP, false}, {"bob", bobFP, false}, {"the client", "", true}} {
		if got := activityAll(t, h, who.fp, who.client, ""); len(got) != 0 {
			t.Errorf("%s sees older kinds: %v", who.name, summaries(got))
		}
	}
	if len(got) != 5 {
		t.Fatalf("root sees %v", summaries(got))
	}
	if got[0].Kind != audit.EventConnect || got[0].Shed.Owner != testClientName || got[4].Kind != audit.EventAPI || got[4].Actor.Client != testClientName {
		t.Errorf("root's view: %+v", got)
	}
}

// TestActivityDeliveryDetail: a delivery event's detail (steps, source,
// items, failure) is shown to whoever may enter the shed as it was recorded;
// an admin looking at another user's shed gets its state, as the shed
// document shows it.
func TestActivityDeliveryDetail(t *testing.T) {
	b := newActivityBackend()
	_, h, l := activityTestServer(t, b)
	full := audit.DeliveryOutcome{
		Reason: "create", State: "partial", Outcome: "accepted",
		Failure: &audit.DeliveryFailure{Reason: "write_failed", Item: "token", Result: "io"},
		Source:  &audit.DeliverySource{Kind: "machine", MachineID: "0123456789abcdef0123456789abcdef", MachineName: "laptop"},
		Steps: &audit.DeliverySteps{
			Stage1: audit.DeliveryStep{State: "failed", Reason: "write_failed"}, Clone: audit.DeliveryStep{State: "done"},
			Stage2: audit.DeliveryStep{State: "owed"}, Install: audit.DeliveryStep{State: "owed"},
		},
		Items:    []audit.DeliveryItem{{Name: "token", Provider: "gh", Stage: 1, Kind: "file", Result: "io", Fingerprint: "fp"}},
		Problems: 1,
	}
	if err := l.Record(audit.DeliveryEvent{Event: audit.EventDelivery, Identity: audit.UserIdentity("alice", false, false, false, "", ""),
		ShedRef: audit.ShedRef{Shed: "web", ShedID: "abcdefghijklmnopqrstuvwxyz", ShedUser: "alice"}, Delivery: full}); err != nil {
		t.Fatal(err)
	}
	alice := activityAll(t, h, aliceFP, false, "")
	if len(alice) != 1 || alice[0].Delivery == nil {
		t.Fatalf("alice sees %+v", alice)
	}
	gotJSON, _ := json.Marshal(alice[0].Delivery)
	wantJSON, _ := json.Marshal(full)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("alice's delivery = %s, want %s", gotJSON, wantJSON)
	}
	root := activityAll(t, h, rootFP, false, "")
	if len(root) != 1 || root[0].Delivery == nil {
		t.Fatalf("root sees %+v", root)
	}
	if d := *root[0].Delivery; d.State != "partial" || d.Reason != "" || d.Items != nil || d.Source != nil || d.Steps != nil || d.Failure != nil || d.Problems != 0 {
		t.Errorf("root (who may not enter alice's shed) sees detail: %+v", d)
	}
}

// writeActivity writes n events for user's shed name.
func writeActivity(t *testing.T, l *audit.Log, user, shed string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := l.Record(audit.ShedEvent{Event: audit.EventShedStart, Identity: audit.UserIdentity(user, false, false, false, "", ""),
			ShedRef: audit.ShedRef{Shed: shed, ShedUser: user}, Result: audit.ResultOK}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestActivityPaging: newest first, each page continuing where the last
// ended, with no event twice or missed; the visibility rule and the shed
// filter are applied before the limit (every page but the last is full);
// the limit is capped at 500; a page looks at a bounded number of records.
func TestActivityPaging(t *testing.T) {
	b := newActivityBackend()
	s, h, l := activityTestServer(t, b)
	// alice's and bob's events interleaved, and alice's on two sheds.
	for i := 0; i < 30; i++ {
		writeActivity(t, l, "alice", "web", 1)
		writeActivity(t, l, "bob", "web", 2)
		writeActivity(t, l, "alice", "api", 1)
	}
	var seen []uint64
	since := ""
	for page := 0; ; page++ {
		q := "?limit=4&shed=web"
		if since != "" {
			q += "&since=" + since
		}
		resp := activityPage(t, h, aliceFP, false, q)
		if resp.Next != "" && len(resp.Events) != 4 {
			t.Fatalf("page %d holds %d events, want 4 (filter before paging)", page, len(resp.Events))
		}
		for _, ev := range resp.Events {
			if ev.Shed.User != "alice" || ev.Shed.Name != "web" {
				t.Fatalf("page %d holds %+v", page, ev)
			}
			c, _ := audit.ParseCursor(ev.ID)
			seen = append(seen, c.Seq)
		}
		if resp.Next == "" {
			break
		}
		if last := resp.Events[len(resp.Events)-1].ID; resp.Next != last {
			t.Errorf("a full page's next is its last event's id: %s, %s", resp.Next, last)
		}
		since = resp.Next
	}
	// alice's web events are seq 1, 5, 9, ... (four records a round).
	var want []uint64
	for i := 29; i >= 0; i-- {
		want = append(want, uint64(4*i+1))
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("alice's web events: %v, want %v", seen, want)
	}

	// The limit: default 100, at most 500.
	if n := len(activityPage(t, h, rootFP, false, "").Events); n != 100 {
		t.Errorf("default page = %d events, want 100", n)
	}
	writeActivity(t, l, "bob", "web", 500)
	resp := activityPage(t, h, rootFP, false, "?limit=100000")
	if len(resp.Events) != ActivityMaxLimit || resp.Next == "" {
		t.Errorf("limit over the cap: %d events, next %q; want %d and a cursor", len(resp.Events), resp.Next, ActivityMaxLimit)
	}

	// The scan budget: alice's one event under 50 of bob's, ten records a
	// page: empty pages with a cursor, then hers.
	s.activityScan = 10
	writeActivity(t, l, "alice", "late", 1)
	writeActivity(t, l, "bob", "web", 50)
	since, pages, found := "", 0, false
	for pages < 100 && !found {
		pages++
		q := "?shed=late"
		if since != "" {
			q += "&since=" + since
		}
		resp := activityPage(t, h, aliceFP, false, q)
		found = len(resp.Events) == 1
		if !found && (len(resp.Events) != 0 || resp.Next == "") {
			t.Fatalf("page %d: %d events, next %q", pages, len(resp.Events), resp.Next)
		}
		since = resp.Next
	}
	if !found || pages != 6 {
		t.Errorf("found %v after %d pages, want after 6 (50 records ten a page, then hers)", found, pages)
	}
}

// TestActivityBadQuery: a malformed query is refused, never read as
// something else.
func TestActivityBadQuery(t *testing.T) {
	_, h, _ := activityTestServer(t, newActivityBackend())
	enc := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	for _, q := range []string{
		"?limit=0", "?limit=-1", "?limit=x", "?since=nope",
		"?since=" + enc("a1:5"), // the earlier, seq-only form
		"?since=" + enc("a2:7:0123456789abcdef:0:12:0"),
		"?shed=Not_A_Name", "?shed=" + strings.Repeat("a", 300),
	} {
		if r := doAs(h, aliceFP, "GET", "/api/activity"+q, ""); r.code != http.StatusBadRequest {
			t.Errorf("GET /api/activity%s = %d %s, want 400", q, r.code, r.body)
		}
	}
}

// TestActivityWithoutTheLog: a server whose audit log is off answers an
// empty page and does not advertise the feed; with the log it does.
func TestActivityWithoutTheLog(t *testing.T) {
	s, h := usersTestServer(t, newUserFakeBackend())
	resp := activityPage(t, h, rootFP, false, "")
	if len(resp.Events) != 0 || resp.Next != "" {
		t.Errorf("no log: %+v", resp)
	}
	if slices.Contains(s.serverFeatures(), FeatureActivity) {
		t.Error("the activity token is advertised with no audit log")
	}
	s2, _, _ := activityTestServer(t, newActivityBackend())
	if !slices.Contains(s2.serverFeatures(), FeatureActivity) {
		t.Error("the activity token is not advertised with an audit log")
	}
}
