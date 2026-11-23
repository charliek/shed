package delivery_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/deliverytest"
)

// The tracked steps through delivery.Run on a fake host, a fake source and
// a fake agent: the one order, what each step waits for, the states and
// reasons of §3.3, the record (valid at every save), the warning, and
// provision's re-delivery.

const sentinel = "S3NTINEL-run-value-7f2a"

// fakeHost is a shed whose steps are recorded. Its project directory holds
// what the clone made; its hooks say whether an install hook exists.
type fakeHost struct {
	mu        sync.Mutex
	agent     *deliverytest.Agent
	saved     []*delivery.Record
	calls     []string
	clone     delivery.Step
	hasHook   bool
	installed bool
	// stepErr, when set, refuses the step numbered stepErrAt (from 1).
	stepErr   error
	stepErrAt int
	steps     int
}

func newHost() *fakeHost {
	return &fakeHost{agent: deliverytest.NewAgent(), clone: delivery.Step{State: delivery.StepDone}, hasHook: true}
}

func (h *fakeHost) call(c string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, c)
}

func (h *fakeHost) Step(ctx context.Context, fn func(context.Context) error) error {
	h.mu.Lock()
	h.steps++
	n := h.steps
	h.mu.Unlock()
	if h.stepErr != nil && n == h.stepErrAt {
		return h.stepErr
	}
	return fn(ctx)
}

func (h *fakeHost) Save(_ context.Context, rec *delivery.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.saved = append(h.saved, rec.Clone())
	return nil
}

func (h *fakeHost) Agent() delivery.Agent { return h.agent }

func (h *fakeHost) Clone(_ context.Context, provision bool) delivery.Step {
	if provision {
		h.call("clone(provision)")
	} else {
		h.call("clone")
	}
	return h.clone
}

func (h *fakeHost) Hooks(_ context.Context, install bool, startup func(delivery.Step, bool) bool) (delivery.Step, bool) {
	var st delivery.Step
	ran := false
	if install {
		h.call("install")
		switch {
		case !h.hasHook:
			st = delivery.Step{State: delivery.StepSkipped, Reason: delivery.ReasonNoHook}
		case h.installed:
			st = delivery.Step{State: delivery.StepDone}
		default:
			h.installed, ran = true, true
			st = delivery.Step{State: delivery.StepDone}
		}
	}
	if startup(st, ran) {
		h.call("startup")
	}
	return st, ran
}

func (h *fakeHost) last(t *testing.T) *delivery.Record {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.saved) == 0 {
		t.Fatal("nothing was saved")
	}
	return h.saved[len(h.saved)-1]
}

// progress collects a run's status lines and warnings.
type progress struct {
	mu     sync.Mutex
	status []string
	warn   []string
}

func (p *progress) report() delivery.Progress {
	return delivery.Progress{
		Status: func(m string) { p.mu.Lock(); p.status = append(p.status, m); p.mu.Unlock() },
		Warn:   func(m string) { p.mu.Lock(); p.warn = append(p.warn, m); p.mu.Unlock() },
	}
}

// source is a fake source and the service over it.
func source(t *testing.T, timeout time.Duration) (*deliverytest.Source, *delivery.Service) {
	t.Helper()
	src := deliverytest.NewSource()
	svc := delivery.NewService(src, timeout)
	src.Attach(svc)
	return src, svc
}

// canned answers with a stage 1 file holding the sentinel, an env value
// holding it, and a stage 2 project file.
func canned(req *delivery.Request) []byte {
	files := []deliverytest.FileItem{
		{Name: "ssh-key", Provider: "ssh-key", Stage: 1, Base: delivery.BaseHome, Path: ".ssh/id_ed25519", Mode: "0600", Content: []byte(sentinel)},
	}
	if req.Shed.ProjectDir != "" {
		files = append(files, deliverytest.FileItem{Name: "project-env", Provider: "files", Stage: 2, Base: delivery.BaseProject, Path: ".envrc", Mode: "0600", Content: []byte("export X=" + sentinel)})
	}
	envs := []deliverytest.EnvItem{{Name: "api-key", Provider: "api-keys", Variable: "EXAMPLE_API_KEY", Value: "a $b 'c' " + sentinel}}
	return deliverytest.AnswerOf(req.ID, files, envs, nil)
}

func shed(name string) delivery.RequestShed {
	return delivery.RequestShed{
		Name: name, ID: "abcdefghijklmnopqrstuvwxyz", Backend: delivery.BackendFirecracker,
		Repo:       &delivery.RequestRepo{URL: "https://git.example.test/example/" + name + ".git", Name: "example/" + name},
		ProjectDir: "/home/shed/" + name,
	}
}

func createRun(svc *delivery.Service, h *fakeHost, p *progress) *delivery.Run {
	return &delivery.Run{
		Service: svc, Reason: delivery.ReasonCreate,
		Params:  delivery.Params{User: "alice", Shed: shed("widgets")},
		HasRepo: true, Prior: delivery.NewRecord(), Host: h, Report: p.report(),
	}
}

func steps(r *delivery.Record) string {
	s := r.Steps
	f := func(st delivery.Step) string {
		if st.Reason != "" {
			return string(st.State) + "/" + string(st.Reason)
		}
		return string(st.State)
	}
	return strings.Join([]string{f(s.Stage1), f(s.Clone), f(s.Stage2), f(s.Install)}, " ")
}

// checkSaved holds every saved record to the contract and to having no
// delivered value in it.
func checkSaved(t *testing.T, h *fakeHost) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, r := range h.saved {
		if err := r.Validate(); err != nil {
			t.Errorf("saved record %d does not validate: %v", i, err)
		}
		b, _ := jsonOf(r)
		if strings.Contains(b, sentinel) {
			t.Errorf("saved record %d carries a delivered value: %s", i, b)
		}
	}
}

func TestRunCreateDeliversInTheOneOrder(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	h, p := newHost(), &progress{}
	rec, err := createRun(svc, h, p).Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := steps(rec); got != "done done done done" {
		t.Fatalf("steps = %s", got)
	}
	if rec.State != delivery.StateDelivered {
		t.Fatalf("state = %s", rec.State)
	}
	// Stage 1 is written before the clone, stage 2 after it, and the
	// install hook runs last, then the startup hook.
	writes := h.agent.Writes()
	if len(writes) != 2 || writes[0].Stage != 1 || writes[1].Stage != 2 {
		t.Fatalf("writes = %+v", writes)
	}
	if strings.Join(h.calls, ",") != "clone,install,startup" {
		t.Fatalf("calls = %v", h.calls)
	}
	if len(src.Requests()) != 1 || src.Requests()[0].Reason != delivery.ReasonCreate {
		t.Fatalf("requests = %+v", src.Requests())
	}
	if !h.agent.Wrote(sentinel) {
		t.Fatal("the bundle never reached the writer")
	}
	if len(rec.Items) != 3 {
		t.Fatalf("items = %+v", rec.Items)
	}
	for _, it := range rec.Items {
		if it.Result != delivery.ResultOK || it.WrittenAt == "" {
			t.Errorf("item %s: %s %q", it.Name, it.Result, it.WrittenAt)
		}
	}
	if len(p.warn) != 0 {
		t.Fatalf("warnings on a delivered create: %v", p.warn)
	}
	// The four steps are saved one by one.
	if len(h.saved) != 4 {
		t.Fatalf("%d saves, want one per step", len(h.saved))
	}
	checkSaved(t, h)
}

func TestRunSourceOffline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clone delivery.Step
		want  string
	}{
		{"private repository", delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonCloneFailed}, "failed/source_offline failed/clone_failed owed owed"},
		{"public repository", delivery.Step{State: delivery.StepDone}, "failed/source_offline done owed owed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, svc := source(t, 5*time.Second)
			src.Set("alice", deliverytest.Offline, nil)
			h, p := newHost(), &progress{}
			h.clone = tc.clone
			rec, err := createRun(svc, h, p).Execute(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := steps(rec); got != tc.want {
				t.Fatalf("steps = %s, want %s", got, tc.want)
			}
			if rec.State != delivery.StateNotDelivered {
				t.Fatalf("state = %s", rec.State)
			}
			want := "Credentials were not delivered (source_offline). Run: shed provision widgets"
			if len(p.warn) != 1 || p.warn[0] != want {
				t.Fatalf("warnings = %q, want [%q]", p.warn, want)
			}
			// The install hook has not run, and neither has the startup hook.
			for _, c := range h.calls {
				if c == "install" || c == "startup" {
					t.Fatalf("a hook ran with stage 1 failed: %v", h.calls)
				}
			}
			checkSaved(t, h)
		})
	}
}

func TestRunNotApplicable(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*delivery.Params)
		mode deliverytest.Mode
	}{
		{"no bound machine", func(*delivery.Params) {}, deliverytest.Unbound},
		{"a service client's shed", func(p *delivery.Params) { p.ServiceClient = true }, deliverytest.Answer},
		{"a script key's create", func(p *delivery.Params) { p.ScriptKey = true }, deliverytest.Answer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, svc := source(t, 5*time.Second)
			src.Set("alice", tc.mode, canned)
			h, p := newHost(), &progress{}
			run := createRun(svc, h, p)
			tc.edit(&run.Params)
			rec, err := run.Execute(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := steps(rec); got != "skipped/not_applicable done skipped/not_applicable done" {
				t.Fatalf("steps = %s", got)
			}
			if rec.State != delivery.StateNotApplicable || len(p.warn) != 0 || len(src.Requests()) != 0 {
				t.Fatalf("state %s, warnings %v, requests %d", rec.State, p.warn, len(src.Requests()))
			}
			if len(h.agent.Writes()) != 0 {
				t.Fatal("something was written")
			}
			if strings.Join(h.calls, ",") != "clone,install,startup" {
				t.Fatalf("calls = %v", h.calls)
			}
			checkSaved(t, h)
		})
	}
}

func TestRunFromSnapshot(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	h, p := newHost(), &progress{}
	run := createRun(svc, h, p)
	run.Params.FromSnapshot = true
	rec, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := steps(rec); got != "skipped/from_snapshot skipped/from_snapshot skipped/from_snapshot skipped/from_snapshot" {
		t.Fatalf("steps = %s", got)
	}
	if rec.State != delivery.StateNotApplicable || len(src.Requests()) != 0 || len(p.warn) != 0 {
		t.Fatalf("state %s, requests %d, warnings %v", rec.State, len(src.Requests()), p.warn)
	}
	// The restored shed's startup hook runs, as it always has; no install.
	if strings.Join(h.calls, ",") != "startup" {
		t.Fatalf("calls = %v", h.calls)
	}
	checkSaved(t, h)
}

func TestRunLocalDirAndNoProvision(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	h, p := newHost(), &progress{}
	run := createRun(svc, h, p)
	run.HasRepo = false
	run.Params.Shed.Repo = nil
	run.Params.Shed.LocalDir = "widgets"
	run.NoProvision = true
	rec, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := steps(rec); got != "done skipped/local_dir skipped/local_dir skipped/no_provision" {
		t.Fatalf("steps = %s", got)
	}
	if len(h.calls) != 0 {
		t.Fatalf("calls = %v", h.calls)
	}
	// The project file was in the answer, never written: not_written.
	for _, it := range rec.Items {
		if it.Stage == 2 && it.Result != delivery.ResultNotWritten {
			t.Errorf("stage 2 item %s: %s", it.Name, it.Result)
		}
	}
	checkSaved(t, h)
}

func TestRunImageTooOldAsksNothing(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	h, p := newHost(), &progress{}
	h.agent.Protocol = 1
	rec, err := createRun(svc, h, p).Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Both stages are skipped, with the failure kept: the clone and the
	// install hook run as they would with no delivery, and the shed reads
	// not_delivered.
	if got := steps(rec); got != "skipped/image_too_old done skipped/image_too_old done" {
		t.Fatalf("steps = %s", got)
	}
	if rec.State != delivery.StateNotDelivered {
		t.Fatalf("state = %s", rec.State)
	}
	if len(src.Requests()) != 0 || rec.Last.Outcome != delivery.OutcomeNotRequested ||
		rec.Last.Failure == nil || rec.Last.Failure.Reason != delivery.ReasonImageTooOld {
		t.Fatalf("requests %d, last %+v", len(src.Requests()), rec.Last)
	}
	want := "Credentials were not delivered (image_too_old). Recreate the shed from a current image."
	if len(p.warn) != 1 || p.warn[0] != want {
		t.Fatalf("warnings = %v", p.warn)
	}
	// A provision on the same image asks nothing and changes nothing.
	run := createRun(svc, h, p)
	run.Reason, run.Prior = delivery.ReasonProvision, rec
	again, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := steps(again); got != "skipped/image_too_old done skipped/image_too_old done" || len(src.Requests()) != 0 {
		t.Fatalf("provision: steps %s, %d requests", got, len(src.Requests()))
	}
	checkSaved(t, h)
}

func TestRunWriteFailures(t *testing.T) {
	t.Run("a stage 1 item", func(t *testing.T) {
		src, svc := source(t, 5*time.Second)
		src.Set("alice", deliverytest.Answer, canned)
		h, p := newHost(), &progress{}
		h.agent.Mode, h.agent.FailItem = deliverytest.WriteFailAt, "ssh-key"
		rec, err := createRun(svc, h, p).Execute(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Stage 1 failed; stage 2 does not depend on it and was written;
		// the install hook waits.
		if got := steps(rec); got != "failed/write_failed done done owed" {
			t.Fatalf("steps = %s", got)
		}
		want := "Credentials were not delivered (write_failed: ssh-key is_directory). Run: shed provision widgets"
		if len(p.warn) != 1 || p.warn[0] != want {
			t.Fatalf("warnings = %q", p.warn)
		}
		checkSaved(t, h)
	})
	t.Run("a malformed writer reply", func(t *testing.T) {
		src, svc := source(t, 5*time.Second)
		src.Set("alice", deliverytest.Answer, canned)
		h, p := newHost(), &progress{}
		h.agent.Mode = deliverytest.WriteMalformed
		rec, err := createRun(svc, h, p).Execute(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rec.Steps.Stage1.State != delivery.StepFailed || rec.Last.Failure == nil || rec.Last.Failure.Result != delivery.ResultIO {
			t.Fatalf("stage 1 %+v, failure %+v", rec.Steps.Stage1, rec.Last.Failure)
		}
		if len(p.warn) != 1 || strings.Contains(p.warn[0], sentinel) {
			t.Fatalf("warnings = %q", p.warn)
		}
		checkSaved(t, h)
	})
}

func TestRunRefusedAnswer(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, func(req *delivery.Request) []byte {
		return deliverytest.AnswerOf(req.ID, []deliverytest.FileItem{
			{Name: "bad", Provider: "files", Stage: 1, Base: delivery.BaseHome, Path: ".ssh/authorized_keys", Mode: "0600", Content: []byte(sentinel)},
		}, nil, nil)
	})
	h, p := newHost(), &progress{}
	rec, err := createRun(svc, h, p).Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := steps(rec); got != "failed/answer_refused done owed owed" {
		t.Fatalf("steps = %s", got)
	}
	if len(h.agent.Writes()) != 0 || len(rec.Items) != 0 {
		t.Fatal("a refused answer wrote something")
	}
	if len(p.warn) != 1 || !strings.Contains(p.warn[0], "(answer_refused: refused_target)") {
		t.Fatalf("warnings = %q", p.warn)
	}
	checkSaved(t, h)
}

// TestRunProvisionFinishesWhatCreateLeft: a create with the source offline,
// then provision with it connected finishes all four (and the startup hook,
// after the first install); a second provision changes nothing but
// timestamps, re-writes the stages, never clones again and removes nothing.
func TestRunProvisionFinishesWhatCreateLeft(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Offline, nil)
	h, p := newHost(), &progress{}
	rec, err := createRun(svc, h, p).Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	src.Set("alice", deliverytest.Answer, canned)
	h.calls = nil
	prov := &delivery.Run{
		Service: svc, Reason: delivery.ReasonProvision,
		Params:  delivery.Params{User: "alice", Shed: shed("widgets")},
		HasRepo: true, Prior: rec, Host: h, Report: p.report(),
	}
	rec, err = prov.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := steps(rec); got != "done done done done" || rec.State != delivery.StateDelivered {
		t.Fatalf("after provision: %s %s", got, rec.State)
	}
	// The clone was done at create; provision did not clone again.
	if strings.Join(h.calls, ",") != "install,startup" {
		t.Fatalf("calls = %v", h.calls)
	}
	if got := src.Requests(); len(got) != 1 || got[0].Reason != delivery.ReasonProvision {
		t.Fatalf("requests = %+v", got)
	}

	// Again: a re-delivery and nothing else.
	h.calls = nil
	prov.Prior = rec
	again, err := prov.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("a second provision ran %v", h.calls)
	}
	if got := steps(again); got != "done done done done" || len(again.Items) != len(rec.Items) {
		t.Fatalf("second provision: %s, %d items", got, len(again.Items))
	}
	checkSaved(t, h)
}

// TestRunRedeliveryKeepsWhatTheAnswerDropped: a file item absent from the
// new answer stays in the record as not_in_last_answer (it stays on disk);
// an env item absent from it is gone (the env file was replaced).
func TestRunRedeliveryKeepsWhatTheAnswerDropped(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	h, p := newHost(), &progress{}
	rec, err := createRun(svc, h, p).Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	src.Set("alice", deliverytest.Answer, func(req *delivery.Request) []byte {
		return deliverytest.AnswerOf(req.ID, []deliverytest.FileItem{
			{Name: "gh", Provider: "github", Stage: 1, Base: delivery.BaseHome, Path: ".config/gh/hosts.yml", Mode: "0600", Content: []byte("x")},
		}, nil, nil)
	})
	prov := &delivery.Run{
		Service: svc, Reason: delivery.ReasonProvision,
		Params:  delivery.Params{User: "alice", Shed: shed("widgets")},
		HasRepo: true, Prior: rec, Host: h, Report: p.report(),
	}
	rec, err = prov.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]delivery.ItemResult{}
	for _, it := range rec.Items {
		got[it.Name] = it.Result
	}
	want := map[string]delivery.ItemResult{"gh": delivery.ResultOK, "ssh-key": delivery.ResultNotInLastAnswer, "project-env": delivery.ResultNotInLastAnswer}
	if len(got) != len(want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("items = %v, want %v", got, want)
		}
	}
	if steps(rec) != "done done skipped/no_items done" || rec.State != delivery.StateDelivered {
		t.Fatalf("steps %s state %s", steps(rec), rec.State)
	}

	// A provision with no accepted answer changes no item.
	src.Set("alice", deliverytest.Offline, nil)
	prov.Prior = rec
	after, err := prov.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Items) != len(rec.Items) || after.Steps.Stage1.Reason != delivery.ReasonSourceOffline || after.State != delivery.StatePartial {
		t.Fatalf("after an offline provision: %+v", after)
	}
	checkSaved(t, h)
}

// TestRunNoRecordStartsFromStageOne: provision on a shed from before
// delivery starts with every step owed; the clone and the install hook take
// what is there (the host's own checks: an existing work tree, the hook's
// state file).
func TestRunNoRecordStartsFromStageOne(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Unbound, nil)
	h, p := newHost(), &progress{}
	h.installed = true
	prov := &delivery.Run{
		Service: svc, Reason: delivery.ReasonProvision,
		Params:  delivery.Params{User: "alice", Shed: shed("widgets")},
		HasRepo: true, Host: h, Report: p.report(),
	}
	rec, err := prov.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := steps(rec); got != "skipped/not_applicable done skipped/not_applicable done" {
		t.Fatalf("steps = %s", got)
	}
	// The hook ran before: no startup hook after it.
	if strings.Join(h.calls, ",") != "clone(provision),install" {
		t.Fatalf("calls = %v", h.calls)
	}
}

// TestRunEndsWhenTheHostRefusesAStep: a step the host refuses (the shed
// stopped, changed or went) ends the run; the steps not reached keep their
// states, and an answer that arrived is dropped unwritten.
func TestRunEndsWhenTheHostRefusesAStep(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	h, p := newHost(), &progress{}
	stopped := errors.New("the shed stopped")
	h.stepErr, h.stepErrAt = stopped, 1
	rec, err := createRun(svc, h, p).Execute(context.Background())
	if !errors.Is(err, stopped) {
		t.Fatalf("err = %v", err)
	}
	if len(h.saved) != 0 || len(h.agent.Writes()) != 0 {
		t.Fatalf("saved %d, wrote %d", len(h.saved), len(h.agent.Writes()))
	}
	if got := steps(rec); got != "owed owed owed owed" {
		t.Fatalf("steps = %s", got)
	}
}

// TestRunInterruptedStepIsNotSaved: a step whose context ends while it runs
// was interrupted, not decided; it is not saved and stays owed.
func TestRunInterruptedStepIsNotSaved(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	ctx, cancel := context.WithCancel(context.Background())
	h, p := newHost(), &progress{}
	h.clone = delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonCloneFailed}
	run := createRun(svc, h, p)
	run.Host = &cancelOnClone{fakeHost: h, cancel: cancel}
	_, err := run.Execute(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	last := h.last(t)
	if got := steps(last); got != "done owed owed owed" {
		t.Fatalf("last saved steps = %s", got)
	}
}

type cancelOnClone struct {
	*fakeHost
	cancel context.CancelFunc
}

func (c *cancelOnClone) Clone(ctx context.Context, provision bool) delivery.Step {
	st := c.fakeHost.Clone(ctx, provision)
	c.cancel()
	return st
}

func jsonOf(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// auditLines attaches an audit log to svc and returns a reader of its
// lines, raw.
func auditLines(t *testing.T, svc *delivery.Service) func() []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), audit.FileName)
	l, err := audit.Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	svc.SetAuditLog(l)
	return func() []string {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

// TestRunWritesItsAuditEvent: every run writes one delivery event: the
// shed by name, id and user, who caused it (the actor on its context), and
// the record as last saved, with the items' names, results and fingerprints
// and never a value. A run that ended early is marked interrupted.
func TestRunWritesItsAuditEvent(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	lines := auditLines(t, svc)
	alice := audit.UserIdentity("alice", false, false, false, "SHA256:k", "")
	if _, err := createRun(svc, newHost(), &progress{}).Execute(audit.WithActor(context.Background(), alice)); err != nil {
		t.Fatal(err)
	}
	got := lines()
	if len(got) != 1 {
		t.Fatalf("lines = %v", got)
	}
	if strings.Contains(got[0], sentinel) {
		t.Fatalf("the event carries a delivered value: %s", got[0])
	}
	var ev audit.DeliveryEvent
	if err := json.Unmarshal([]byte(got[0]), &ev); err != nil {
		t.Fatal(err)
	}
	d := ev.Delivery
	if ev.Event != audit.EventDelivery || ev.Identity.User != "alice" || ev.Identity.Subject != "SHA256:k" ||
		ev.Shed != "widgets" || ev.ShedID != "abcdefghijklmnopqrstuvwxyz" || ev.ShedUser != "alice" || ev.ShedOwner != "" {
		t.Errorf("event = %+v", ev)
	}
	if d.Reason != "create" || d.State != "delivered" || d.Outcome != "accepted" || d.Interrupted || d.Steps == nil || d.Steps.Install.State != "done" {
		t.Errorf("outcome = %+v", d)
	}
	if d.Source == nil || d.Source.MachineID == "" {
		t.Errorf("no source machine: %+v", d)
	}
	if len(d.Items) != 3 || d.Items[0].Name != "ssh-key" || d.Items[0].Result != "ok" || d.Items[0].Kind != "file" {
		t.Errorf("items = %+v", d.Items)
	}

	// Ended early: interrupted, with what was saved; no actor on the
	// context and no owner: unknown.
	h := newHost()
	h.stepErr, h.stepErrAt = errors.New("the shed went"), 2
	if _, err := createRun(svc, h, &progress{}).Execute(context.Background()); err == nil {
		t.Fatal("the run did not end")
	}
	got = lines()
	_ = json.Unmarshal([]byte(got[1]), &ev)
	if !ev.Delivery.Interrupted || ev.Delivery.Steps == nil || ev.Delivery.Steps.Stage1.State != "done" || ev.Delivery.Steps.Clone.State != "owed" ||
		ev.Identity.Kind != audit.KindUnknown {
		t.Errorf("interrupted run's event = %+v", ev)
	}

	// A service client's shed: not applicable, the client named as the
	// owner and, with no actor on the context, as the one who caused it.
	r := createRun(svc, newHost(), &progress{})
	r.Params = delivery.Params{Owner: "ci", ServiceClient: true, Shed: shed("built")}
	if _, err := r.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = lines()
	ev = audit.DeliveryEvent{}
	_ = json.Unmarshal([]byte(got[2]), &ev)
	if ev.ShedOwner != "ci" || ev.ShedUser != "" || ev.Identity.Kind != audit.KindClient || ev.Identity.Name != "ci" || ev.Delivery.State != "not_applicable" {
		t.Errorf("a client's shed's event = %+v", ev)
	}
}

// TestResumedRunNeverAsksASource: a resumed create (a job that lost the
// create request) asks no source even when one is bound and connected by
// the time it runs: its stages read not applicable and nothing is sent.
func TestResumedRunNeverAsksASource(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	h, p := newHost(), &progress{}
	run := createRun(svc, h, p)
	run.Resumed = true
	rec, err := run.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(src.Requests()); n != 0 {
		t.Fatalf("a resumed run sent %d request(s)", n)
	}
	if got := steps(rec); got != "skipped/not_applicable done skipped/not_applicable done" {
		t.Fatalf("steps = %s", got)
	}
}
