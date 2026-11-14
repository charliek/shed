package state

import (
	"errors"
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

// Every write and removal flushes the directory it changed: without that a
// rename or an unlink can be lost to a power cut after the call returned, and
// the server has by then acted in the cloud on the strength of it.
func TestStoreWritesAreDurable(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	orig := syncDir
	syncDir = func(dir string) error {
		synced = append(synced, filepath.Base(dir))
		return orig(dir)
	}
	defer func() { syncDir = orig }()

	rec := &Record{Shed: config.Shed{Name: "one", ID: "abcdefgh", Status: config.StatusStarting}, Env: "test"}
	tests := []struct {
		name string
		op   func() error
		want []string
	}{
		{"save record", func() error { return s.SaveRecord(rec) }, []string{"sheds"}},
		{"save marker", func() error { return s.SaveMarker(&Marker{ID: "abcdefgh", Name: "one", Env: "test"}) }, []string{"deleting"}},
		{"delete record", func() error { return s.DeleteRecord("one") }, []string{"sheds"}},
		{"delete marker", func() error { return s.DeleteMarker("abcdefgh") }, []string{"deleting"}},
		// A removal whose flush failed is retried as the removal of a file
		// that is not there, and the retry must still flush.
		{"delete a marker that is not there", func() error { return s.DeleteMarker("abcdefgh") }, []string{"deleting"}},
		{"delete a record that is not there", func() error { return s.DeleteRecord("one") }, []string{"sheds"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synced = nil
			if err := tt.op(); err != nil {
				t.Fatal(err)
			}
			if len(synced) != len(tt.want) || (len(synced) == 1 && synced[0] != tt.want[0]) {
				t.Fatalf("flushed directories %v, want %v", synced, tt.want)
			}
		})
	}
}

// failSyncDir makes the next n directory flushes fail.
func failSyncDir(t *testing.T, n int) {
	t.Helper()
	orig := syncDir
	syncDir = func(dir string) error {
		if n > 0 {
			n--
			return errors.New("flush failed")
		}
		return orig(dir)
	}
	t.Cleanup(func() { syncDir = orig })
}

// A removal that unlinked the file and could not flush the directory reports
// an error. The retry finds no file; it succeeds only once the directory has
// been flushed, because until then a power cut can bring the file back.
func TestStoreRemovalRetryFlushes(t *testing.T) {
	tests := []struct {
		name   string
		save   func(s *Store) error
		remove func(s *Store) error
	}{
		{"marker", func(s *Store) error { return s.SaveMarker(&Marker{ID: "abcdefgh", Name: "one", Env: "test"}) },
			func(s *Store) error { return s.DeleteMarker("abcdefgh") }},
		{"record", func(s *Store) error { return s.SaveRecord(&Record{Shed: config.Shed{Name: "one", ID: "abcdefgh"}}) },
			func(s *Store) error { return s.DeleteRecord("one") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.save(s); err != nil {
				t.Fatal(err)
			}
			failSyncDir(t, 2)
			if err := tt.remove(s); err == nil {
				t.Fatal("a removal whose directory flush failed reported success")
			}
			if err := tt.remove(s); err == nil {
				t.Fatal("the retry reported success though the directory was still not flushed")
			}
			if err := tt.remove(s); err != nil {
				t.Fatalf("the retry with a working flush: %v", err)
			}
		})
	}
}
