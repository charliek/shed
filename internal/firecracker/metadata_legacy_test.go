//go:build linux

package firecracker

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
)

// v011Delivery is a delivery record as v0.11 (provision contract version 1)
// wrote it: its source has no kind.
const v011Delivery = `{"state":"delivered","steps":{"stage1":{"state":"done"},"clone":{"state":"done"},"stage2":{"state":"skipped","reason":"no_items"},"install":{"state":"done"}},` +
	`"last":{"at":"2026-11-23T10:00:00Z","reason":"create","outcome":"accepted","source":{"machine_id":"5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b","machine_name":"ada-laptop"}},` +
	`"items":[{"name":"ssh-key","provider":"ssh-key","stage":1,"kind":"file","base":"home","target":".ssh/id_ed25519","fingerprint":"k3y-fp_01","written_at":"2026-11-23T10:00:02Z","result":"ok"}]}`

// TestLoadMetadataLegacyDeliverySource: a shed's metadata written by v0.11
// holds a record whose source has no kind. LoadMetadata reads it as a
// machine source; a save writes the kind; and the record read back from
// disk is one version 2's strict reader accepts. A kind written empty or
// null, or a record v0.11 could not have written (the reason refresh), is
// not repaired and fails validation.
func TestLoadMetadataLegacyDeliverySource(t *testing.T) {
	for _, c := range []struct {
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
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := mustTempDir(t, "metadata-legacy")
			meta := &Metadata{Name: "old", Status: config.StatusRunning, CreatedAt: time.Now().Truncate(time.Second), Backend: config.BackendFirecracker}
			if err := meta.Save(dir); err != nil {
				t.Fatal(err)
			}
			// Put the record in as it was written.
			path := MetadataPath(dir, "old")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			doc["delivery"] = json.RawMessage(c.record)
			if raw, err = json.Marshal(doc); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}

			loaded, err := LoadMetadata(dir, "old")
			if err != nil {
				t.Fatal(err)
			}
			if !c.repaired {
				if k := loaded.Delivery.Last.Source.Kind; k != "" {
					t.Fatalf("repaired to %q", k)
				}
				if err := loaded.Delivery.Validate(); err == nil {
					t.Fatal("a record the loader must not repair validates")
				}
				return
			}
			if src := loaded.Delivery.Last.Source; src == nil || src.Kind != delivery.SourceKindMachine || src.MachineName != "ada-laptop" {
				t.Fatalf("loaded source %+v, want the machine with kind machine", src)
			}
			if err := loaded.Save(dir); err != nil {
				t.Fatal(err)
			}
			if raw, err = os.ReadFile(path); err != nil {
				t.Fatal(err)
			}
			var back struct {
				Delivery json.RawMessage `json:"delivery"`
			}
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			rec, r := delivery.ParseRecord(back.Delivery)
			if r != nil {
				t.Fatalf("the saved record is refused: %v (%s)", r, back.Delivery)
			}
			if rec.Last.Source.Kind != delivery.SourceKindMachine || rec.Last.Source.MachineID != "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b" {
				t.Fatalf("read back %+v", rec.Last.Source)
			}
		})
	}
}
