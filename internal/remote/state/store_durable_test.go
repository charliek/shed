package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// A write has two outcomes a caller can tell apart. Either nothing was put in
// place, or the file is in place and readable and only the flush of its
// directory failed (ErrNotDurable): this process and the next one both read
// it, so the caller must treat it as written.
func TestStoreWriteThatIsNotDurableIsInPlace(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	failSyncDir(t, 2)
	err = s.SaveMarker(&Marker{ID: "abcdefgh", Name: "one", Env: "test"})
	if !errors.Is(err, ErrNotDurable) {
		t.Fatalf("SaveMarker with a failing flush = %v, want ErrNotDurable", err)
	}
	if markers, merr := s.Markers(); merr != nil || len(markers) != 1 || markers[0].ID != "abcdefgh" {
		t.Fatalf("Markers = %+v, %v: the marker that was reported not durable is not in place", markers, merr)
	}
	err = s.SaveRecord(&Record{Shed: config.Shed{Name: "one", ID: "abcdefgh"}})
	if !errors.Is(err, ErrNotDurable) {
		t.Fatalf("SaveRecord with a failing flush = %v, want ErrNotDurable", err)
	}
	if records, _, rerr := s.Records(); rerr != nil || len(records) != 1 {
		t.Fatalf("Records = %+v, %v: the record that was reported not durable is not in place", records, rerr)
	}

	// A write that could not be made at all is another error, and leaves
	// nothing behind.
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := filepath.Join(s.Dir(), "deleting")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	err = s.SaveMarker(&Marker{ID: "ijklmnop", Name: "two", Env: "test"})
	if err == nil || errors.Is(err, ErrNotDurable) {
		t.Fatalf("SaveMarker into a read-only directory = %v, want an error that is not ErrNotDurable", err)
	}
	if markers, _ := s.Markers(); len(markers) != 1 {
		t.Fatalf("%d markers after a write that failed, want the earlier one only", len(markers))
	}
}
