package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/remote/provider"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "remote")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := &Record{
		Shed:    config.Shed{Name: "one", ID: "abcdefgh", Status: config.StatusStarting, CreatedAt: time.Now().UTC().Round(0)},
		Env:     "test",
		Ref:     "zone/shed-test-abcdefgh",
		Pending: &provider.Pending{Ref: "zone/shed-test-abcdefgh", Operation: "op-1", Want: provider.StateRunning},
		Spec:    &provider.Spec{Name: "shed-test-abcdefgh", Labels: map[string]string{"shed_id": "abcdefgh"}},
		Job:     JobCreate,
	}
	if err := s.SaveRecord(rec); err != nil {
		t.Fatal(err)
	}
	// A file that is not a record is reported, not fatal, and left alone.
	junk := filepath.Join(dir, "sheds", "junk.json")
	if err := os.WriteFile(junk, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	records, bad, err := reopened.Records()
	if err != nil || len(records) != 1 || len(bad) != 1 || bad[0] != junk {
		t.Fatalf("Records = %d records, bad %v, err %v", len(records), bad, err)
	}
	got := records[0]
	if got.Shed.Name != "one" || got.Job != JobCreate || got.Pending == nil || got.Pending.Operation != "op-1" ||
		got.Spec == nil || got.Spec.Labels["shed_id"] != "abcdefgh" || !got.Shed.CreatedAt.Equal(rec.Shed.CreatedAt) {
		t.Fatalf("record read back differs: %+v", got)
	}
	clone := got.Clone()
	clone.Spec.Labels["shed_id"] = "changed"
	clone.Pending.Operation = "changed"
	if got.Spec.Labels["shed_id"] != "abcdefgh" || got.Pending.Operation != "op-1" {
		t.Fatal("Clone shares state with its source")
	}

	entries, _ := os.ReadDir(filepath.Join(dir, "sheds"))
	if len(entries) != 2 {
		t.Fatalf("%d files in sheds/, want the record and the junk file (no temp files)", len(entries))
	}
	if err := reopened.DeleteRecord("one"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.DeleteRecord("one"); err != nil {
		t.Fatalf("deleting a missing record: %v", err)
	}
}

func TestStoreMarkers(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bbbbbbbb", "aaaaaaaa"} {
		if err := s.SaveMarker(&Marker{ID: id, Name: "shed-" + id, Env: "test", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	markers, err := s.Markers()
	if err != nil || len(markers) != 2 || markers[0].ID != "aaaaaaaa" {
		t.Fatalf("Markers = %+v, %v", markers, err)
	}
	if err := s.DeleteMarker("aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if markers, _ := s.Markers(); len(markers) != 1 {
		t.Fatalf("%d markers after deleting one, want 1", len(markers))
	}
}

func TestStoreRefusesUnsafeNames(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "..", "a/b", "../escape"} {
		if err := s.SaveRecord(&Record{Shed: config.Shed{Name: name}}); err == nil {
			t.Errorf("SaveRecord(%q) succeeded", name)
		}
		if err := s.SaveMarker(&Marker{ID: name}); err == nil {
			t.Errorf("SaveMarker(%q) succeeded", name)
		}
	}
}
