package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/delivery"
)

// v011Delivery is a delivery record as v0.11 (provision contract version 1)
// wrote it: its source has no kind.
const v011Delivery = `{"state":"delivered","steps":{"stage1":{"state":"done"},"clone":{"state":"done"},"stage2":{"state":"skipped","reason":"no_items"},"install":{"state":"done"}},` +
	`"last":{"at":"2026-11-23T10:00:00Z","reason":"create","outcome":"accepted","source":{"machine_id":"5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b","machine_name":"ada-laptop"}},` +
	`"items":[{"name":"ssh-key","provider":"ssh-key","stage":1,"kind":"file","base":"home","target":".ssh/id_ed25519","fingerprint":"k3y-fp_01","written_at":"2026-11-23T10:00:02Z","result":"ok"}]}`

// legacyCases are records the loader must, or must not, repair: only the
// shape v0.11 wrote (no kind member, the machine members, a reason v0.11
// could write) is given kind machine; a kind written empty or null, or a
// record v0.11 could not have written, is left as it is and fails
// validation as any malformed record does.
var legacyCases = []struct {
	name     string
	record   string
	repaired bool
}{
	{"v0.11", v011Delivery, true},
	{"kind empty", strings.Replace(v011Delivery, `"source":{`, `"source":{"kind":"",`, 1), false},
	{"kind null", strings.Replace(v011Delivery, `"source":{`, `"source":{"kind":null,`, 1), false},
	{"a reason v0.11 never wrote", strings.Replace(v011Delivery, `"reason":"create"`, `"reason":"refresh"`, 1), false},
	{"machine_name named twice", strings.Replace(v011Delivery, `"machine_name":"ada-laptop"`, `"machine_name":"ada-laptop","machine_name":"ada-laptop"`, 1), false},
	{"an unknown member in last", strings.Replace(v011Delivery, `"outcome":"accepted",`, `"outcome":"accepted","extra":1,`, 1), false},
}

// TestLegacyRecordLoadsAsMachine: a cloud shed's record written by v0.11 has
// a source with no kind. The store's loader reads it as a machine source,
// a save writes the kind, and the record read back is one version 2's
// strict reader accepts. The record as v0.11 wrote it is refused by that
// reader: the leniency is the loader's alone, and only for that shape.
func TestLegacyRecordLoadsAsMachine(t *testing.T) {
	if _, r := delivery.ParseRecord([]byte(v011Delivery)); r == nil || r.Rule != delivery.RuleMissingMember {
		t.Fatalf("ParseRecord of a v0.11 record: %v, want missing_member (the reader stays strict)", r)
	}
	for _, c := range legacyCases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "remote")
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			raw := `{"shed":{"name":"one","id":"abcdefgh","status":"running"},"env":"test","delivery":` + c.record + `}`
			if err := os.WriteFile(filepath.Join(dir, "sheds", "one.json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			records, bad, err := s.Records()
			if err != nil || len(bad) != 0 || len(records) != 1 {
				t.Fatalf("Records = %d, bad %v, err %v", len(records), bad, err)
			}
			rec := records[0].Delivery
			if !c.repaired {
				if rec.Last.Source.Kind != "" {
					t.Fatalf("repaired to %q", rec.Last.Source.Kind)
				}
				if err := rec.Validate(); err == nil {
					t.Fatal("a record the loader must not repair validates")
				}
				return
			}
			src := rec.Last.Source
			if src.Kind != delivery.SourceKindMachine || src.MachineID != "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b" || src.MachineName != "ada-laptop" {
				t.Fatalf("loaded source %+v, want the machine with kind machine", src)
			}
			if err := rec.Validate(); err != nil {
				t.Fatalf("the loaded record does not validate: %v", err)
			}
			if err := s.SaveRecord(records[0]); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(dir, "sheds", "one.json"))
			if err != nil {
				t.Fatal(err)
			}
			var back struct {
				Delivery json.RawMessage `json:"delivery"`
			}
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			got, r := delivery.ParseRecord(back.Delivery)
			if r != nil {
				t.Fatalf("the saved record is refused: %v (%s)", r, back.Delivery)
			}
			if got.Last.Source.Kind != delivery.SourceKindMachine || got.Last.Source.MachineName != "ada-laptop" {
				t.Fatalf("read back %+v", got.Last.Source)
			}
		})
	}
}
