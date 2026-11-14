package remote

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// A delete has one of two outcomes, and the API's answer says which: it is
// acknowledged and the VM is destroyed, or it is refused and the VM is never
// destroyed for it, not by the periodic pass and not after a restart. A
// marker that was put in place, with only the flush of its directory failing,
// is one every later pass and every later server reads, so it is an
// acknowledged delete; a marker that was not put in place is a refused one.
//
// The faults are made with directory permissions: a directory that can be
// written and not read takes the marker and refuses the flush, and one that
// can be read and not written takes nothing.
func TestDeleteAnswerMatchesWhatHappensToTheVM(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not held back by directory permissions")
	}
	tests := []struct {
		name     string
		mode     os.FileMode
		accepted bool
	}{
		{"the marker is in place and its directory cannot be flushed", 0o300, true},
		{"the marker cannot be written", 0o500, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			const name = "doomed"
			e.running(name)
			id := e.settledID(name)
			markers := filepath.Join(e.dir, "deleting")
			if err := os.Chmod(markers, tt.mode); err != nil {
				t.Fatal(err)
			}
			restored := false
			restore := func() {
				if !restored {
					restored = true
					if err := os.Chmod(markers, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			defer restore()

			err := e.b.DeleteShed(ctxT(t), name)
			e.idle()
			restore()
			if (err == nil) != tt.accepted {
				t.Errorf("DeleteShed = %v, want accepted=%v", err, tt.accepted)
			}

			// Whatever was answered must stay true through a periodic pass
			// and a restart.
			e.sweep()
			e.idle()
			e.restart()
			e.idle()
			e.sweep()
			e.idle()
			if err == nil {
				if e.instances() != 0 {
					t.Fatal("the delete was acknowledged and the VM still exists")
				}
				if _, gerr := e.b.GetShed(ctxT(t), name); gerr == nil {
					t.Fatal("the delete was acknowledged and the shed is still listed")
				}
				if n := markerCount(t, e.dir); n != 0 {
					t.Fatalf("%d delete markers left after the delete finished", n)
				}
				return
			}
			if e.instances() != 1 {
				t.Fatalf("the delete was refused (%v) and the VM was destroyed all the same", err)
			}
			shed, gerr := e.b.GetShed(ctxT(t), name)
			if gerr != nil || shed.ID != id || shed.Status != config.StatusRunning {
				t.Fatalf("the delete was refused and the shed reads %+v, %v; want it running as before", shed, gerr)
			}
		})
	}
}
