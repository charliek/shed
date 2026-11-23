package delivery

import (
	"context"
	"log"

	"github.com/charliek/shed/internal/audit"
)

// auditRun writes the run's delivery event to the service's audit log: the
// shed (its name, id, and whose it is), who caused the run, and the record as
// the run last saved it (the prior record when it saved none). err is the
// run's own: a run that ended early is marked interrupted. Nothing here is a
// delivered value: the event is built from the record, which holds none.
func (r *Run) auditRun(ctx context.Context, err error) {
	if r.Service == nil {
		return
	}
	l := r.Service.audit.Load()
	if l == nil {
		return
	}
	rec := r.saved
	if rec == nil {
		rec = r.Prior
	}
	p := r.Params
	ev := audit.DeliveryEvent{
		Event:    audit.EventDelivery,
		Identity: runActor(ctx, p),
		ShedRef:  audit.ShedRef{Shed: p.Shed.Name, ShedID: p.Shed.ID, ShedUser: p.User, ShedOwner: p.Owner},
		Delivery: auditOutcome(rec, r.Reason, err != nil),
	}
	if p.Owner != "" {
		ev.ShedUser = ""
	}
	if werr := l.Record(ev); werr != nil {
		log.Printf("audit: %v", werr)
	}
}

// runActor is who caused a run: the actor its context carries (the access
// guard sets it from the caller), or, with none, the service client that owns
// the shed (only it creates its sheds), or unknown.
func runActor(ctx context.Context, p Params) audit.Identity {
	if id, ok := audit.ActorFrom(ctx); ok {
		return id
	}
	if p.Owner != "" {
		return audit.ClientIdentity(p.Owner, "")
	}
	return audit.UnknownIdentity()
}

// auditOutcome is rec as a delivery event carries it: the states, reasons,
// the source machine, and each item's name and result with the fingerprint
// the source chose; never a target, content or value, and of the problems
// only how many there were.
func auditOutcome(rec *Record, reason Reason, interrupted bool) audit.DeliveryOutcome {
	out := audit.DeliveryOutcome{Reason: string(reason), State: string(StateNone), Interrupted: interrupted}
	if rec == nil {
		return out
	}
	out.State = string(rec.State)
	if s := rec.Steps; s != nil {
		step := func(st Step) audit.DeliveryStep {
			return audit.DeliveryStep{State: string(st.State), Reason: string(st.Reason)}
		}
		out.Steps = &audit.DeliverySteps{Stage1: step(s.Stage1), Clone: step(s.Clone), Stage2: step(s.Stage2), Install: step(s.Install)}
	}
	if last := rec.Last; last != nil {
		out.Outcome = string(last.Outcome)
		if f := last.Failure; f != nil {
			out.Failure = &audit.DeliveryFailure{Reason: string(f.Reason), Rule: string(f.Rule), Item: f.Item, Result: string(f.Result)}
		}
		if src := last.Source; src != nil {
			out.Source = &audit.DeliverySource{MachineID: src.MachineID, MachineName: src.MachineName}
		}
		out.Problems = len(last.Problems)
	}
	for _, it := range rec.Items {
		out.Items = append(out.Items, audit.DeliveryItem{
			Name: it.Name, Provider: it.Provider, Stage: it.Stage, Kind: string(it.Kind),
			Result: string(it.Result), Fingerprint: it.Fingerprint, ExpiresAt: it.ExpiresAt,
		})
	}
	return out
}
