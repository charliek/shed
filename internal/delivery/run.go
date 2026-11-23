package delivery

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/charliek/shed/internal/remote/guest"
)

// The tracked steps of a create and of `shed provision`, run in the one
// order (stage 1, clone, stage 2, install hook) by one runner that every
// backend calls: the Firecracker and VZ orchestrator at create, the remote
// backend's create job, and each backend's provision. A backend gives the
// runner a Host: the shed, its lock, its agent and its clone and hooks.

// Host is one shed as a run drives it. Each backend implements it.
type Host interface {
	// Step runs fn as one step: under the shed's lifecycle lock, after
	// checking the shed is still the one the run is for (its id) and still
	// running. An error from Step ends the run: the shed changed, stopped or
	// went, or ctx ended; fn's own error is returned as it is.
	Step(ctx context.Context, fn func(ctx context.Context) error) error
	// Save persists rec as the shed's record, durably. It is called from
	// inside Step, under the lock.
	Save(ctx context.Context, rec *Record) error
	// Agent is the shed's agent, which the stages write through.
	Agent() Agent
	// Clone is the clone step for a shed with a repository: done (cloned,
	// or the directory is already the repository's work tree), or failed
	// (clone_failed, project_dir_exists). With provision it never removes
	// anything.
	Clone(ctx context.Context, provision bool) Step
	// Hooks runs the repository's hooks, reading its provisioning config
	// once. With install it runs the install hook as the install step:
	// done (it ran and exited 0, or ran before), skipped no_hook, or failed
	// hook_failed; ran says it ran now. Then, when startup says so for that
	// step and ran (a zero Step without install), the startup hook.
	Hooks(ctx context.Context, install bool, startup func(st Step, ran bool) bool) (st Step, ran bool)
}

// Progress is where a run reports to the person waiting: a phase boundary
// for the server's timing log, a status line, and a warning. Every field may
// be nil. Nothing a run reports carries a value.
type Progress struct {
	Phase  func(name string)
	Status func(msg string)
	Warn   func(msg string)
}

// Run is one delivery.
type Run struct {
	// Service asks the source and writes the stages; nil makes every
	// delivery not applicable (a server built without one).
	Service *Service
	// Reason is create or provision.
	Reason Reason
	// Params is the request's: the shed's user and the shed, and what makes
	// a delivery not applicable (a service client's shed, a script key's
	// create, a snapshot restore). Its Reason is set from Run's.
	Params Params
	// HasRepo says the shed has a repository to clone (its request names it
	// when the URL is within the contract's bound).
	HasRepo bool
	// NoProvision is a create's --no-provision: the install hook is
	// skipped.
	NoProvision bool
	// Prior is the shed's record before the run; nil for a shed with no
	// record. Initial, when Prior is nil, is the steps to start from (the
	// remote backend's reading of a record from before delivery); nil is
	// every step owed.
	Prior   *Record
	Initial *Steps
	Host    Host
	Report  Progress
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Execute runs the delivery and returns the record as last saved. Each step
// is saved, durably, once it is decided. The error, when not nil, ended the
// run before its last step (the host refused a step: the shed changed,
// stopped or went, or ctx ended; or a record did not validate); the steps
// not reached keep the states they had.
//
// The bundle is asked for once, before stage 1, held in memory until stage
// 2 is written or skipped, and then dropped. It is never saved or logged.
func (r *Run) Execute(ctx context.Context) (*Record, error) {
	rec := r.Prior.Clone()
	if rec == nil {
		rec = &Record{}
	}
	if rec.Steps == nil {
		if r.Initial != nil {
			s := *r.Initial
			rec.Steps = &s
		} else {
			rec.Steps = NewRecord().Steps
		}
	}
	p := r.Params
	p.Reason = r.Reason
	name := p.Shed.Name
	provision := r.Reason == ReasonProvision

	if r.Reason == ReasonCreate && p.FromSnapshot {
		// The files are already in the snapshot: no request, every step
		// skipped. The startup hook runs as a restored shed's always has.
		err := r.Host.Step(ctx, func(ctx context.Context) error {
			rec = SnapshotRecord(r.now())
			if err := r.save(ctx, rec); err != nil {
				return err
			}
			if !r.NoProvision {
				r.Host.Hooks(ctx, false, func(Step, bool) bool { return true })
			}
			return nil
		})
		return rec, err
	}

	// Stage 1. The request is its attempt, made outside any step: the wait
	// for a source takes no lock.
	r.phase("delivery")
	res, err := r.request(ctx, p)
	if err != nil {
		return rec, err
	}
	bundle := res.Bundle
	res.Bundle = nil
	defer func() { bundle = nil }()

	warned := false
	warn := func(f *Failure) {
		if f != nil && !warned {
			warned = true
			if r.Report.Warn != nil {
				r.Report.Warn(Warning(name, f))
			}
		}
	}

	if err := r.Host.Step(ctx, func(ctx context.Context) error {
		var failure *Failure
		switch {
		case bundle != nil:
			r.status("Writing delivered credentials...")
			st := r.Service.Write(ctx, r.Host.Agent(), 1, bundle, p.Shed.ProjectDir)
			rec.Steps.Stage1 = Step{State: st.State, Reason: st.Reason}
			rec.Items = stageOneItems(rec.Items, bundle, st, FormatTime(r.now()))
			failure = st.Failure
		case res.Reason == ReasonNotApplicable:
			rec.Steps.Stage1 = Step{State: StepSkipped, Reason: ReasonNotApplicable}
		default:
			rec.Steps.Stage1 = Step{State: StepFailed, Reason: res.Reason}
			failure = &Failure{Reason: res.Reason, Rule: res.Rule}
		}
		rec.Last = &Last{
			At: FormatTime(res.At), Reason: r.Reason, Outcome: res.Outcome,
			Failure: failure, Source: res.Source, Problems: bundle.Problems(),
		}
		if res.Outcome == OutcomeNotRequested {
			rec.Last.Source = nil
		}
		if err := r.save(ctx, rec); err != nil {
			return err
		}
		warn(failure)
		if failure == nil && bundle != nil {
			msg := fmt.Sprintf("Credentials delivered (%d items)", len(bundle.StageItems(1)))
			if n := len(bundle.Problems()); n > 0 {
				msg += fmt.Sprintf("; the source reported %d problems (shed provision %s --status)", n, name)
			}
			r.status(msg)
		}
		return nil
	}); err != nil {
		return rec, err
	}

	// The clone.
	if attempt(rec.Steps.Clone) {
		if err := r.Host.Step(ctx, func(ctx context.Context) error {
			switch {
			case p.Shed.LocalDir != "":
				rec.Steps.Clone = Step{State: StepSkipped, Reason: ReasonLocalDir}
			case !r.HasRepo:
				rec.Steps.Clone = Step{State: StepSkipped, Reason: ReasonNoRepo}
			default:
				rec.Steps.Clone = r.Host.Clone(ctx, provision)
			}
			return r.save(ctx, rec)
		}); err != nil {
			return rec, err
		}
	}

	// Stage 2: when owed or failed, and always when a bundle arrived.
	if attempt(rec.Steps.Stage2) || bundle != nil {
		if err := r.Host.Step(ctx, func(ctx context.Context) error {
			switch {
			case res.Outcome == OutcomeNotRequested && res.Reason == ReasonNotApplicable:
				rec.Steps.Stage2 = Step{State: StepSkipped, Reason: ReasonNotApplicable}
			case p.Shed.LocalDir != "":
				rec.Steps.Stage2 = Step{State: StepSkipped, Reason: ReasonLocalDir}
			case bundle == nil || !settled(rec.Steps.Clone):
				rec.Steps.Stage2 = Step{State: StepOwed}
			case p.Shed.ProjectDir == "":
				rec.Steps.Stage2 = Step{State: StepSkipped, Reason: ReasonNoProjectDir}
			default:
				st := r.Service.Write(ctx, r.Host.Agent(), 2, bundle, p.Shed.ProjectDir)
				rec.Steps.Stage2 = Step{State: st.State, Reason: st.Reason}
				stageTwoItems(rec.Items, bundle, st, FormatTime(r.now()))
				if st.Failure != nil && rec.Last != nil && rec.Last.Failure == nil {
					rec.Last.Failure = st.Failure
				}
			}
			bundle = nil
			if err := r.save(ctx, rec); err != nil {
				return err
			}
			if rec.Last != nil {
				warn(rec.Last.Failure)
			}
			return nil
		}); err != nil {
			return rec, err
		}
	}
	bundle = nil

	// The install hook, then the startup hook when it is due: on create
	// after an install step that is done (or found no install hook), and on
	// provision after an install hook that ran for the first time.
	if attempt(rec.Steps.Install) {
		if err := r.Host.Step(ctx, func(ctx context.Context) error {
			switch {
			case r.NoProvision:
				rec.Steps.Install = Step{State: StepSkipped, Reason: ReasonNoProvision}
			case !settled(rec.Steps.Stage1) || !settled(rec.Steps.Clone) || !settled(rec.Steps.Stage2):
				rec.Steps.Install = Step{State: StepOwed}
			default:
				startup := func(st Step, ran bool) bool {
					if provision {
						return ran && st.State == StepDone
					}
					return st.State == StepDone || (st.State == StepSkipped && st.Reason == ReasonNoHook)
				}
				st, _ := r.Host.Hooks(ctx, true, startup)
				rec.Steps.Install = st
			}
			return r.save(ctx, rec)
		}); err != nil {
			return rec, err
		}
	}
	return rec, nil
}

// request asks for the bundle when a delivery is applicable. Before it asks
// it probes the shed's agent: an image whose agent cannot take items is not
// worth a source's answer (image_too_old, not requested). Only ctx ending,
// or a request the service could not make, is an error.
func (r *Run) request(ctx context.Context, p Params) (Result, error) {
	now := r.now().UTC()
	if r.Service == nil || !r.Service.Applicable(p) {
		return Result{Outcome: OutcomeNotRequested, Reason: ReasonNotApplicable, At: now}, nil
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	info, err := r.Host.Agent().Probe(pctx)
	cancel()
	if err == nil && info.Protocol < guest.ApplyItemsProtocol {
		// The guest's number is not repeated: only that it is below.
		log.Printf("delivery: shed %q: its agent speaks a protocol below %d: its image cannot take items; nothing is requested",
			p.Shed.Name, guest.ApplyItemsProtocol)
		return Result{Outcome: OutcomeNotRequested, Reason: ReasonImageTooOld, At: now}, nil
	}
	// A probe that failed is not judged here: the stage's own probe judges
	// it once there is something to write.
	r.status("Requesting credentials from your source...")
	return r.Service.Request(ctx, p)
}

func (r *Run) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Run) phase(name string) {
	if r.Report.Phase != nil {
		r.Report.Phase(name)
	}
}

func (r *Run) status(msg string) {
	if r.Report.Status != nil {
		r.Report.Status(msg)
	}
}

// save derives rec's state, holds it to the contract and persists it. A
// step whose context ended while it ran was interrupted, not decided: it is
// not saved, so it keeps the state it had (owed, at an interrupted create),
// and the run ends. A record that breaks a rule is a bug in this package: it
// is not saved either, and the run ends (the rule and place are logged,
// never a value).
func (r *Run) save(ctx context.Context, rec *Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rec.State = DeriveState(rec)
	if err := rec.Validate(); err != nil {
		log.Printf("delivery: the record for shed %q does not hold to the contract and is not saved: %v", r.Params.Shed.Name, err)
		return fmt.Errorf("delivery record for shed %q: %w", r.Params.Shed.Name, err)
	}
	return r.Host.Save(ctx, rec.Clone())
}

// attempt reports whether a step that is not stage 1 is tried: when it is
// owed or failed.
func attempt(s Step) bool { return s.State == StepOwed || s.State == StepFailed }

// settled reports whether what depends on s may run: s is done or skipped.
func settled(s Step) bool { return s.State == StepDone || s.State == StepSkipped }

// itemKey is the target an item holds: two items with one key are one
// target, the newer replacing the older.
func itemKey(kind Kind, base Base, target string) string {
	return string(kind) + "\x00" + string(base) + "\x00" + target
}

// recordItem is it as the record keeps it: no content, no value.
func recordItem(it Item) RecordItem {
	ri := RecordItem{
		Name: it.Name, Provider: it.Provider, Stage: it.ItemStage(), Kind: it.Kind,
		Target: it.Target(), Fingerprint: it.Fingerprint, ExpiresAt: it.ExpiresAt,
	}
	if it.Kind == KindFile {
		ri.Base = it.Base
	}
	return ri
}

// stageOneItems is the record's items after stage 1 wrote st from bundle:
// every item of the answer (stage 1 with its result, stage 2 not_written
// until its stage runs), then each item from before that the answer no
// longer holds and that is still in the shed, as not_in_last_answer: a
// delivery never deletes a file. An env item from before is gone when stage
// 1 replaced the env file (done or skipped), so it is dropped then.
func stageOneItems(prior []RecordItem, bundle *Bundle, st StageResult, at string) []RecordItem {
	results := map[string]ItemResult{}
	for _, o := range st.Items {
		results[o.Name] = o.Result
	}
	var out []RecordItem
	keys := map[string]bool{}
	for _, it := range bundle.Items() {
		ri := recordItem(it)
		ri.Result = ResultNotWritten
		if it.ItemStage() == 1 {
			if res, ok := results[it.Name]; ok {
				ri.Result = res
			}
		}
		if ri.Result == ResultOK {
			ri.WrittenAt = at
		}
		keys[itemKey(ri.Kind, ri.Base, ri.Target)] = true
		out = append(out, ri)
	}
	envReplaced := st.State == StepDone || st.State == StepSkipped
	for _, old := range prior {
		if keys[itemKey(old.Kind, old.Base, old.Target)] {
			continue
		}
		if old.Result != ResultOK && old.Result != ResultNotInLastAnswer {
			continue // never in the shed
		}
		if old.Kind == KindEnv && envReplaced {
			continue
		}
		old.Result = ResultNotInLastAnswer
		out = append(out, old)
	}
	return out
}

// stageTwoItems records stage 2's results on the answer's stage 2 items.
func stageTwoItems(items []RecordItem, bundle *Bundle, st StageResult, at string) {
	results := map[string]ItemResult{}
	for _, o := range st.Items {
		results[o.Name] = o.Result
	}
	for _, it := range bundle.StageItems(2) {
		key := itemKey(it.Kind, it.Base, it.Target())
		for i := range items {
			ri := &items[i]
			if itemKey(ri.Kind, ri.Base, ri.Target) != key || ri.Name != it.Name {
				continue
			}
			if res, ok := results[it.Name]; ok {
				ri.Result = res
			}
			ri.WrittenAt = ""
			if ri.Result == ResultOK {
				ri.WrittenAt = at
			}
		}
	}
}
