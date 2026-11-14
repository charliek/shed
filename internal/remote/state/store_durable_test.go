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
func TestStoreWriteOutcomes(t *testing.T) {
	marker := func(t *testing.T, s *Store) (int, error) {
		err := s.SaveMarker(&Marker{ID: "abcdefgh", Name: "one", Env: "test"})
		markers, merr := s.Markers()
		if merr != nil {
			t.Fatalf("Markers: %v", merr)
		}
		return len(markers), err
	}
	record := func(t *testing.T, s *Store) (int, error) {
		err := s.SaveRecord(&Record{Shed: config.Shed{Name: "one", ID: "abcdefgh"}})
		records, _, rerr := s.Records()
		if rerr != nil {
			t.Fatalf("Records: %v", rerr)
		}
		return len(records), err
	}
	tests := []struct {
		name string
		// write makes the write and returns how many files of its kind are
		// in place afterwards.
		write func(*testing.T, *Store) (int, error)
		// failFlush makes the flush of the directory fail after the file is
		// in place. Otherwise the directory is made read-only, so the file
		// cannot be written at all.
		failFlush  bool
		notDurable bool
		inPlace    int
	}{
		{name: "a marker whose directory cannot be flushed is in place", write: marker, failFlush: true, notDurable: true, inPlace: 1},
		{name: "a record whose directory cannot be flushed is in place", write: record, failFlush: true, notDurable: true, inPlace: 1},
		{name: "a marker that cannot be written leaves nothing", write: marker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if tt.failFlush {
				failSyncDir(t, 2)
			} else {
				if os.Geteuid() == 0 {
					t.Skip("root writes to a read-only directory")
				}
				dir := filepath.Join(s.Dir(), "deleting")
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(dir, 0o700) }()
			}
			inPlace, err := tt.write(t, s)
			if err == nil || errors.Is(err, ErrNotDurable) != tt.notDurable {
				t.Fatalf("the write = %v, want an error with ErrNotDurable=%v", err, tt.notDurable)
			}
			if inPlace != tt.inPlace {
				t.Fatalf("%d files in place after the write (%v), want %d", inPlace, err, tt.inPlace)
			}
		})
	}
}
