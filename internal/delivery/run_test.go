package delivery_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/delivery/deliverytest"
	"github.com/charliek/shed/internal/remote/guest"
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
		Params:  delivery.Params{Origin: delivery.OriginLocal, User: "alice", Shed: shed("widgets")},
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
		Params:  delivery.Params{Origin: delivery.OriginLocal, User: "alice", Shed: shed("widgets")},
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
		Params:  delivery.Params{Origin: delivery.OriginLocal, User: "alice", Shed: shed("widgets")},
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
		Params:  delivery.Params{Origin: delivery.OriginLocal, User: "alice", Shed: shed("widgets")},
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
	r.Params = delivery.Params{Origin: delivery.OriginLocal, Owner: "ci", ServiceClient: true, Shed: shed("built")}
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

// resumeAt is a create interrupted at a point: the record as it was saved
// there (stage 1 and the last delivery as the fresh run left them).
func resumeAt(t *testing.T, stage1 delivery.Step, last *delivery.Last, clone delivery.Step) *delivery.Record {
	t.Helper()
	rec := delivery.NewRecord()
	rec.Steps.Stage1, rec.Steps.Clone, rec.Last = stage1, clone, last
	rec.State = delivery.DeriveState(rec)
	if err := rec.Validate(); err != nil {
		t.Fatalf("the interrupted record does not validate: %v", err)
	}
	return rec
}

// TestResumeReadsTheRecordOnly: a resumed create (a job that lost the create
// request: a server restart, a cloud read that failed) asks no source, even
// one bound and connected by the time it runs, never rewrites a recorded
// step or the last delivery, and runs only the steps not yet recorded whose
// dependencies are recorded and that need no bundle (plan 040, section 3.3),
// at each point a create can be interrupted.
func TestResumeReadsTheRecordOnly(t *testing.T) {
	at := "2026-11-25T10:00:00Z"
	na := &delivery.Last{At: at, Reason: delivery.ReasonCreate, Outcome: delivery.OutcomeNotRequested}
	accepted := &delivery.Last{At: at, Reason: delivery.ReasonCreate, Outcome: delivery.OutcomeAccepted, Source: delivery.ServiceSource()}
	offline := &delivery.Last{At: at, Reason: delivery.ReasonCreate, Outcome: delivery.OutcomeNoAnswer,
		Failure: &delivery.Failure{Reason: delivery.ReasonSourceOffline}, Source: delivery.ServiceSource()}
	owed := delivery.Step{State: delivery.StepOwed}
	done := delivery.Step{State: delivery.StepDone}
	for _, tc := range []struct {
		name  string
		prior *delivery.Record
		// steps is the record after the resume; calls what ran.
		steps, calls string
		saves        int
	}{
		{
			name:  "before stage 1 was recorded: nothing runs, every step stays owed",
			prior: delivery.NewRecord(),
			steps: "owed owed owed owed", calls: "", saves: 0,
		},
		{
			name:  "after stage 1 recorded not applicable: the clone and the install hook run",
			prior: resumeAt(t, delivery.Step{State: delivery.StepSkipped, Reason: delivery.ReasonNotApplicable}, na, owed),
			steps: "skipped/not_applicable done skipped/not_applicable done", calls: "clone install startup", saves: 3,
		},
		{
			name:  "after stage 1 done with the clone not recorded: the clone runs, stage 2 and the hook stay owed",
			prior: resumeAt(t, done, accepted, owed),
			steps: "done done owed owed", calls: "clone", saves: 3,
		},
		{
			name:  "after stage 1 done and the clone done: stage 2 and the hook stay owed",
			prior: resumeAt(t, done, accepted, done),
			steps: "done done owed owed", calls: "", saves: 2,
		},
		{
			name:  "after stage 1 recorded failed: the clone runs as on a fresh run, stage 2 and the hook stay owed",
			prior: resumeAt(t, delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonSourceOffline}, offline, owed),
			steps: "failed/source_offline done owed owed", calls: "clone", saves: 3,
		},
		{
			name:  "a clone recorded failed is not tried again",
			prior: resumeAt(t, delivery.Step{State: delivery.StepSkipped, Reason: delivery.ReasonNotApplicable}, na, delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonCloneFailed}),
			steps: "skipped/not_applicable failed/clone_failed skipped/not_applicable owed", calls: "", saves: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A source that would answer at once: a resumed run must not ask it.
			src, svc := source(t, 5*time.Second)
			src.Set("alice", deliverytest.Answer, canned)
			h, p := newHost(), &progress{}
			run := createRun(svc, h, p)
			run.Resumed, run.Prior = true, tc.prior
			rec, err := run.Execute(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if n := len(src.Requests()); n != 0 {
				t.Fatalf("a resumed run sent %d request(s)", n)
			}
			if got := steps(rec); got != tc.steps {
				t.Fatalf("steps = %s, want %s", got, tc.steps)
			}
			if got := strings.Join(h.calls, " "); got != tc.calls {
				t.Fatalf("calls = %q, want %q", got, tc.calls)
			}
			if len(h.saved) != tc.saves {
				t.Fatalf("%d saves, want %d", len(h.saved), tc.saves)
			}
			// Stage 1 and the last delivery are never rewritten.
			if rec.Steps.Stage1 != tc.prior.Steps.Stage1 {
				t.Fatalf("stage 1 rewritten: %+v, was %+v", rec.Steps.Stage1, tc.prior.Steps.Stage1)
			}
			if a, b := mustJSON(t, rec.Last), mustJSON(t, tc.prior.Last); a != b {
				t.Fatalf("last rewritten: %s, was %s", a, b)
			}
			checkSaved(t, h)
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := jsonOf(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRunRefusesAShedWhoseOriginChanged: a delivery to a shed whose recorded
// user origin is not its user's current one is refused before anything is
// asked or saved, and writes no audit event.
func TestRunRefusesAShedWhoseOriginChanged(t *testing.T) {
	src, svc := source(t, 5*time.Second)
	src.Set("alice", deliverytest.Answer, canned)
	svc.SetOrigin(func(user string) string { return delivery.OriginDirectory })
	h := newHost()
	run := createRun(svc, h, &progress{})
	run.Reason = delivery.ReasonProvision
	run.Params.Origin = delivery.OriginLocal
	prior := run.Prior.Clone()
	rec, err := run.Execute(context.Background())
	if !errors.Is(err, delivery.ErrShedOriginChanged) {
		t.Fatalf("Execute = %v, want ErrShedOriginChanged", err)
	}
	if len(src.Requests()) != 0 || len(h.saved) != 0 || len(h.calls) != 0 {
		t.Fatalf("a refused run asked %d, saved %d, called %v", len(src.Requests()), len(h.saved), h.calls)
	}
	if mustJSON(t, rec) != mustJSON(t, prior) {
		t.Fatalf("the record changed: %s", mustJSON(t, rec))
	}
	// The same origin is not refused; no recorded origin is (it fails
	// closed), and a run for such a shed asks nothing and saves nothing.
	if err := svc.CheckOrigin("alice", delivery.OriginDirectory); err != nil {
		t.Fatalf("CheckOrigin(directory) = %v", err)
	}
	if err := svc.CheckOrigin("alice", ""); !errors.Is(err, delivery.ErrShedOriginUnknown) {
		t.Fatalf("CheckOrigin(\"\") = %v, want ErrShedOriginUnknown", err)
	}
	run = createRun(svc, h, &progress{})
	run.Params.Origin = ""
	if _, err := run.Execute(context.Background()); !errors.Is(err, delivery.ErrShedOriginUnknown) {
		t.Fatalf("Execute with no origin = %v", err)
	}
	if len(src.Requests()) != 0 || len(h.saved) != 0 {
		t.Fatal("a shed with no origin was asked for or saved")
	}
}

// pausedAgent is a shed's agent whose probe waits at gate (after telling
// entered it began).
type pausedAgent struct {
	*deliverytest.Agent
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (a *pausedAgent) Probe(ctx context.Context) (guest.AgentInfo, error) {
	a.once.Do(func() { close(a.entered) })
	<-a.gate
	return a.Agent.Probe(ctx)
}

// pausedHost is a fakeHost whose agent's probe pauses.
type pausedHost struct {
	*fakeHost
	agent *pausedAgent
}

func (h *pausedHost) Agent() delivery.Agent { return h.agent }

// countingHosted is an account service that answers every request with an
// accepted answer, after gate (when set), whatever its context says (a late
// reply), and counts the requests.
type countingHosted struct {
	mu    sync.Mutex
	calls int
	gate  chan struct{}
}

func (c *countingHosted) Deliver(_ context.Context, request []byte) (delivery.HostedStatus, []byte) {
	c.mu.Lock()
	c.calls++
	gate := c.gate
	c.mu.Unlock()
	if gate != nil {
		<-gate
	}
	req, _ := delivery.ParseRequest(request)
	return delivery.HostedAnswer, canned(req)
}

func (c *countingHosted) NotDelivering() bool { return false }

func (c *countingHosted) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// hostedRun is a directory user's create on a service whose source is h.
func hostedRun(h *countingHosted) (*delivery.Service, *fakeHost) {
	svc := delivery.NewService(deliverytest.NewSource(), 5*time.Second)
	svc.SetOrigin(func(string) string { return delivery.OriginDirectory })
	svc.SetHosted(h)
	return svc, newHost()
}

// TestHostedRunEndsWithItsShed: a directory user's run is ended by a stop or
// delete of its shed from its admission on (plan 040, section 3.3): paused
// in the agent's probe, or admitted and not yet started, it never asks the
// account service and saves nothing; and a reply that comes after the stop
// is never written. The shed's slot is free afterwards.
func TestHostedRunEndsWithItsShed(t *testing.T) {
	t.Run("stopped during the probe", func(t *testing.T) {
		hosted := &countingHosted{}
		svc, fh := hostedRun(hosted)
		agent := &pausedAgent{Agent: fh.agent, entered: make(chan struct{}), gate: make(chan struct{})}
		run := createRun(svc, fh, &progress{})
		run.Params.Origin = delivery.OriginDirectory
		run.Host = &pausedHost{fakeHost: fh, agent: agent}
		run.Ticket = svc.Admit("widgets", shed("widgets").ID)
		errc := make(chan error, 1)
		go func() { _, err := run.Execute(context.Background()); errc <- err }()
		<-agent.entered
		svc.EndShed("widgets", shed("widgets").ID)
		close(agent.gate)
		if err := <-errc; !errors.Is(err, delivery.ErrShedEnded) {
			t.Fatalf("Execute = %v, want ErrShedEnded", err)
		}
		if hosted.count() != 0 || len(fh.saved) != 0 || len(fh.agent.Writes()) != 0 {
			t.Fatalf("an ended run asked %d times, saved %d, wrote %d", hosted.count(), len(fh.saved), len(fh.agent.Writes()))
		}
		// The slot is free, and a new run of the shed asks.
		next := createRun(svc, newHost(), &progress{})
		next.Params.Origin = delivery.OriginDirectory
		if rec, err := next.Execute(context.Background()); err != nil || hosted.count() != 1 || rec.Last.Outcome != delivery.OutcomeAccepted {
			t.Fatalf("the next run = %v (%d asked)", err, hosted.count())
		}
	})
	t.Run("stopped between its admission and its start", func(t *testing.T) {
		hosted := &countingHosted{}
		svc, fh := hostedRun(hosted)
		run := createRun(svc, fh, &progress{})
		run.Params.Origin = delivery.OriginDirectory
		run.Ticket = svc.Admit("widgets", shed("widgets").ID)
		svc.EndShed("widgets", shed("widgets").ID)
		if _, err := run.Execute(context.Background()); !errors.Is(err, delivery.ErrShedEnded) {
			t.Fatalf("Execute = %v, want ErrShedEnded", err)
		}
		if hosted.count() != 0 || len(fh.saved) != 0 {
			t.Fatalf("asked %d, saved %d", hosted.count(), len(fh.saved))
		}
	})
	t.Run("a reply after the stop is not written", func(t *testing.T) {
		hosted := &countingHosted{gate: make(chan struct{})}
		svc, fh := hostedRun(hosted)
		run := createRun(svc, fh, &progress{})
		run.Params.Origin = delivery.OriginDirectory
		errc := make(chan error, 1)
		go func() { _, err := run.Execute(context.Background()); errc <- err }()
		for hosted.count() == 0 {
			time.Sleep(time.Millisecond)
		}
		svc.EndShed("widgets", shed("widgets").ID)
		close(hosted.gate)
		if err := <-errc; !errors.Is(err, delivery.ErrShedEnded) {
			t.Fatalf("Execute = %v, want ErrShedEnded", err)
		}
		if len(fh.agent.Writes()) != 0 || len(fh.saved) != 0 {
			t.Fatalf("a late reply was written (%d writes, %d saves)", len(fh.agent.Writes()), len(fh.saved))
		}
	})
}

// TestStaleStopLeavesAReplacementAlone: a stop or delete is matched to the
// shed incarnation it was checked against (its permanent id): one of an
// older shed of the same name, delivered late, does not end a replacement's
// run, paused in its probe; the replacement's own stop does.
func TestStaleStopLeavesAReplacementAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stopID  string
		wantErr error
	}{
		{"the old incarnation's stop", "oldincarnation0000000000000", nil},
		{"its own stop", shed("widgets").ID, delivery.ErrShedEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosted := &countingHosted{}
			svc, fh := hostedRun(hosted)
			agent := &pausedAgent{Agent: fh.agent, entered: make(chan struct{}), gate: make(chan struct{})}
			run := createRun(svc, fh, &progress{})
			run.Params.Origin = delivery.OriginDirectory
			run.Host = &pausedHost{fakeHost: fh, agent: agent}
			run.Ticket = svc.Admit("widgets", shed("widgets").ID)
			errc := make(chan error, 1)
			go func() { _, err := run.Execute(context.Background()); errc <- err }()
			<-agent.entered
			svc.EndShed("widgets", tc.stopID)
			close(agent.gate)
			err := <-errc
			if tc.wantErr == nil && (err != nil || hosted.count() != 1) {
				t.Fatalf("the replacement's run = %v (%d asked), want it delivered", err, hosted.count())
			}
			if tc.wantErr != nil && (!errors.Is(err, tc.wantErr) || hosted.count() != 0) {
				t.Fatalf("the run = %v (%d asked), want %v", err, hosted.count(), tc.wantErr)
			}
		})
	}
}

// TestEndedRunKeepsItsCause: a run whose shed is stopped after its answer
// arrived and before its step reports ErrShedEnded (so a provision maps it to
// 409), not a bare cancellation.
func TestEndedRunKeepsItsCause(t *testing.T) {
	hosted := &countingHosted{}
	svc, fh := hostedRun(hosted)
	run := createRun(svc, fh, &progress{})
	run.Params.Origin = delivery.OriginDirectory
	run.Host = &stoppingHost{fakeHost: fh, stop: func() { svc.EndShed("widgets", shed("widgets").ID) }}
	_, err := run.Execute(context.Background())
	if !errors.Is(err, delivery.ErrShedEnded) {
		t.Fatalf("Execute = %v, want ErrShedEnded", err)
	}
	if hosted.count() != 1 || len(fh.agent.Writes()) != 0 || len(fh.saved) != 0 {
		t.Fatalf("asked %d, wrote %d, saved %d", hosted.count(), len(fh.agent.Writes()), len(fh.saved))
	}
}

// stoppingHost is a fakeHost whose shed is stopped as its first step
// begins, as a stop that lands between the answer and the write: the step
// then refuses, as a real host's does, with its context's cause.
type stoppingHost struct {
	*fakeHost
	stop func()
}

func (h *stoppingHost) Step(ctx context.Context, fn func(context.Context) error) error {
	h.stop()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return h.fakeHost.Step(ctx, fn)
}

// endingAgent is an agent that stops the shed while it writes stage 1: the
// write is done, and the save after it finds the run ended.
type endingAgent struct {
	*deliverytest.Agent
	stop func()
}

func (a *endingAgent) Run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer, max int) (int, error) {
	code, err := a.Agent.Run(ctx, argv, stdin, stdout, max)
	a.stop()
	return code, err
}

// agentHost is a fakeHost with another agent.
type agentHost struct {
	*fakeHost
	agent delivery.Agent
}

func (h *agentHost) Agent() delivery.Agent { return h.agent }

// TestEndedDuringItsWriteKeepsItsCause: a stop that lands while stage 1 is
// written leaves the save to find the run ended: it reports ErrShedEnded,
// not a bare cancellation, and nothing is saved.
func TestEndedDuringItsWriteKeepsItsCause(t *testing.T) {
	hosted := &countingHosted{}
	svc, fh := hostedRun(hosted)
	run := createRun(svc, fh, &progress{})
	run.Params.Origin = delivery.OriginDirectory
	run.Host = &agentHost{fakeHost: fh, agent: &endingAgent{Agent: fh.agent, stop: func() { svc.EndShed("widgets", shed("widgets").ID) }}}
	_, err := run.Execute(context.Background())
	if !errors.Is(err, delivery.ErrShedEnded) {
		t.Fatalf("Execute = %v, want ErrShedEnded", err)
	}
	if len(fh.saved) != 0 {
		t.Fatalf("%d saves after the run ended", len(fh.saved))
	}
}
