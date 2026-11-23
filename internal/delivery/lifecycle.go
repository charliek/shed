package delivery

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// The record over a shed's life: what create starts it as, what a snapshot
// restore, an adoption and a reset make of it, and what the shed document
// reads from it (the owed steps, the expired items, the warning text). None
// of it holds a value: a record never does.

// NewRecord is the record a new shed starts with, before any step has run:
// every step owed, nothing delivered. A create interrupted before its steps
// reports it.
func NewRecord() *Record {
	r := &Record{Steps: &Steps{
		Stage1: Step{State: StepOwed}, Clone: Step{State: StepOwed},
		Stage2: Step{State: StepOwed}, Install: Step{State: StepOwed},
	}}
	r.State = DeriveState(r)
	return r
}

// SnapshotRecord is the record of a shed restored from a snapshot: it
// carries the snapshot's files, no request was made, and every step is
// skipped as from_snapshot. `shed provision` delivers afresh.
func SnapshotRecord(at time.Time) *Record {
	skip := Step{State: StepSkipped, Reason: ReasonFromSnapshot}
	r := &Record{
		Steps: &Steps{Stage1: skip, Clone: skip, Stage2: skip, Install: skip},
		Last:  &Last{At: FormatTime(at), Reason: ReasonCreate, Outcome: OutcomeNotRequested},
	}
	r.State = DeriveState(r)
	return r
}

// AdoptedRecord is the record of a cloud shed adopted at start-up, whose
// history the server does not know: every step skipped as adopted, and the
// state none. Nothing is marked done that the server did not see.
func AdoptedRecord() *Record {
	skip := Step{State: StepSkipped, Reason: ReasonAdopted}
	r := &Record{Steps: &Steps{Stage1: skip, Clone: skip, Stage2: skip, Install: skip}}
	r.State = DeriveState(r)
	return r
}

// ResetRecord is prior after `shed reset` wiped the shed's disk: no item
// (they went with the disk), every step owed, the last delivery kept as it
// was. prior may be nil (a shed with no record).
func ResetRecord(prior *Record) *Record {
	r := NewRecord()
	if prior != nil && prior.Last != nil {
		last := *prior.Last
		last.Problems = append([]Problem(nil), prior.Last.Problems...)
		if prior.Last.Failure != nil {
			f := *prior.Last.Failure
			last.Failure = &f
		}
		if prior.Last.Source != nil {
			s := *prior.Last.Source
			last.Source = &s
		}
		r.Last = &last
	}
	r.State = DeriveState(r)
	return r
}

// Clone is a deep copy of r (nil for nil).
func (r *Record) Clone() *Record {
	if r == nil {
		return nil
	}
	out := *r
	if r.Steps != nil {
		s := *r.Steps
		out.Steps = &s
	}
	if r.Last != nil {
		l := *r.Last
		if r.Last.Failure != nil {
			f := *r.Last.Failure
			l.Failure = &f
		}
		if r.Last.Source != nil {
			s := *r.Last.Source
			l.Source = &s
		}
		l.Problems = append([]Problem(nil), r.Last.Problems...)
		out.Last = &l
	}
	out.Items = append([]RecordItem(nil), r.Items...)
	return &out
}

// OwedSteps is the names of r's steps that are owed, in the order they run
// (none for a shed with no record).
func OwedSteps(r *Record) []string {
	if r == nil || r.Steps == nil {
		return nil
	}
	var out []string
	for _, s := range r.Steps.list() {
		if s.step.State == StepOwed {
			out = append(out, s.name)
		}
	}
	return out
}

// ExpiredItems is the names of r's items that are in the shed (written, by
// the last delivery or one before it) and whose expires_at is not after
// now, sorted and without repeats.
func ExpiredItems(r *Record, now time.Time) []string {
	if r == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, it := range r.Items {
		if it.ExpiresAt == "" || (it.Result != ResultOK && it.Result != ResultNotInLastAnswer) {
			continue
		}
		at, ok := ParseTime(it.ExpiresAt)
		if !ok || at.After(now) || seen[it.Name] {
			continue
		}
		seen[it.Name] = true
		out = append(out, it.Name)
	}
	sort.Strings(out)
	return out
}

// Reported is the record as the shed document carries it: r itself, or for a
// shed with no record {"state": "none"}.
func Reported(r *Record) Record {
	if r == nil {
		return Record{State: StateNone}
	}
	out := *r.Clone()
	out.State = DeriveState(&out)
	return out
}

// FailureText is how the warning names a failure: its reason, with the rule
// an answer broke, or the item a write failed at and its result. Every part
// is from a closed list or a validated item name: none is a value.
func FailureText(f *Failure) string {
	if f == nil {
		return ""
	}
	switch {
	case f.Reason == ReasonAnswerRefused && f.Rule != "":
		return fmt.Sprintf("%s: %s", f.Reason, f.Rule)
	case f.Reason == ReasonWriteFailed && f.Item != "":
		return fmt.Sprintf("%s: %s %s", f.Reason, f.Item, f.Result)
	case f.Reason == ReasonWriteFailed:
		return fmt.Sprintf("%s: the env file %s", f.Reason, f.Result)
	}
	return string(f.Reason)
}

// Warning is the fixed text a failed delivery is warned with, naming the
// command that finishes it; for an image too old to take a delivery, which
// no `shed provision` can finish, the recreate that does.
func Warning(shed string, f *Failure) string {
	if f != nil && f.Reason == ReasonImageTooOld {
		return fmt.Sprintf("Credentials were not delivered (%s). Recreate the shed from a current image.", FailureText(f))
	}
	return fmt.Sprintf("Credentials were not delivered (%s). Run: shed provision %s", FailureText(f), shed)
}

// SameRecord reports whether two records are the same as persisted (both
// nil, or equal as their documents are). A run compares the record it last
// saw with the persisted one before each step: a record that changed under
// it (a reset) ends the run.
func SameRecord(a, b *Record) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}
