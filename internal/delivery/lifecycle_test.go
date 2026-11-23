package delivery

import (
	"testing"
	"time"
)

// The records a shed's life makes outside a run: each holds to the
// contract, and the shed document's derived members read them.
func TestLifecycleRecords(t *testing.T) {
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	for name, r := range map[string]*Record{
		"new":      NewRecord(),
		"snapshot": SnapshotRecord(at),
		"adopted":  AdoptedRecord(),
		"reset":    ResetRecord(nil),
	} {
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if AdoptedRecord().State != StateNone || SnapshotRecord(at).State != StateNotApplicable || NewRecord().State != StateNotDelivered {
		t.Fatal("wrong states")
	}

	delivered := &Record{
		Steps: &Steps{Stage1: Step{State: StepDone}, Clone: Step{State: StepDone}, Stage2: Step{State: StepDone}, Install: Step{State: StepDone}},
		Last:  &Last{At: FormatTime(at), Reason: ReasonCreate, Outcome: OutcomeAccepted, Source: &SourceMachine{MachineID: "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b", MachineName: "ada-laptop"}},
		Items: []RecordItem{
			{Name: "gh", Provider: "github", Stage: 1, Kind: KindFile, Base: BaseHome, Target: ".config/gh/hosts.yml", Fingerprint: "f1", ExpiresAt: FormatTime(at.Add(time.Hour)), WrittenAt: FormatTime(at), Result: ResultOK},
			{Name: "old", Provider: "files", Stage: 1, Kind: KindFile, Base: BaseHome, Target: "old", Fingerprint: "f2", ExpiresAt: FormatTime(at.Add(-time.Hour)), WrittenAt: FormatTime(at), Result: ResultNotInLastAnswer},
			{Name: "never", Provider: "files", Stage: 1, Kind: KindFile, Base: BaseHome, Target: "never", Fingerprint: "f3", ExpiresAt: FormatTime(at.Add(-time.Hour)), Result: ResultNotWritten},
		},
	}
	delivered.State = DeriveState(delivered)
	if err := delivered.Validate(); err != nil {
		t.Fatal(err)
	}
	// Expired: items in the shed whose time has come, not one never written.
	if got := ExpiredItems(delivered, at); len(got) != 1 || got[0] != "old" {
		t.Fatalf("expired at %s = %v", at, got)
	}
	if got := ExpiredItems(delivered, at.Add(2*time.Hour)); len(got) != 2 {
		t.Fatalf("expired later = %v", got)
	}

	// Reset: no item, every step owed, the last delivery kept.
	r := ResetRecord(delivered)
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 0 || r.Last == nil || r.Last.At != delivered.Last.At || len(OwedSteps(r)) != 4 || r.State != StateNotDelivered {
		t.Fatalf("reset record = %+v", r)
	}
	if len(delivered.Items) != 3 {
		t.Fatal("reset changed the record it was given")
	}

	if got := Reported(nil); got.State != StateNone || got.Steps != nil {
		t.Fatalf("a shed with no record reports %+v", got)
	}
	if w := Warning("box", &Failure{Reason: ReasonSourceTimeout}); w != "Credentials were not delivered (source_timeout). Run: shed provision box" {
		t.Fatalf("warning = %q", w)
	}
}
