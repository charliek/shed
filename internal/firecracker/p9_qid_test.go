//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hugelgupf/p9/fsimpl/localfs"
	"github.com/hugelgupf/p9/p9"
)

// TestP9LocalfsQIDIsTheInode pins the QID derivation shed's FC mounts rely
// on: the localfs attacher reports a file's inode as its QID path, the same
// value on every request, with no per-lookup state.
//
// hugelgupf/p9 v0.5.1 replaced this with a packed dev+ino encoding whose
// fallback (an inode >= 1<<39, or a device number too wide to pack) keys a
// global map by a freshly allocated pointer: every lookup of the same file
// misses, gets a new QID and leaks a map entry, so a guest could change a
// file's identity and grow the server's memory by repeating GETATTR. shed
// stays on v0.3.0 until a release keys that map by value. A bump that
// changes how the QID is derived fails here, so it gets looked at.
func TestP9LocalfsQIDIsTheInode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(dir, "f"), &st); err != nil {
		t.Fatal(err)
	}

	root, err := localfs.Attacher(dir).Attach()
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = root.Close() }()

	var paths []uint64
	for i := 0; i < 3; i++ {
		qids, f, err := root.Walk([]string{"f"})
		if err != nil {
			t.Fatalf("walk %d: %v", i, err)
		}
		if len(qids) != 1 {
			t.Fatalf("walk %d: got %d qids, want 1", i, len(qids))
		}
		q, _, _, err := f.GetAttr(p9.AttrMaskAll)
		_ = f.Close()
		if err != nil {
			t.Fatalf("getattr %d: %v", i, err)
		}
		if q.Path != qids[0].Path {
			t.Fatalf("lookup %d: GETATTR qid %#x != WALK qid %#x", i, q.Path, qids[0].Path)
		}
		paths = append(paths, q.Path)
	}
	for i, p := range paths {
		if p != st.Ino {
			t.Errorf("lookup %d: qid path %#x, want the inode %#x", i, p, st.Ino)
		}
	}
}
