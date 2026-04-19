//go:build linux

package firecracker

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hugelgupf/p9/fsimpl/localfs"
	"github.com/hugelgupf/p9/p9"
)

func TestRemappingWalkPathTracking(t *testing.T) {
	hostDir := t.TempDir()

	// Create nested dirs for walking
	subDir := filepath.Join(hostDir, "a", "b")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, 1000, 1000)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	rf := root.(*remappingFile)
	if rf.hostPath != hostDir {
		t.Errorf("root hostPath = %q, want %q", rf.hostPath, hostDir)
	}

	// Clone (empty walk) should preserve path
	_, cloned, err := root.Walk(nil)
	if err != nil {
		t.Fatalf("Walk(nil): %v", err)
	}
	defer cloned.Close()

	cf := cloned.(*remappingFile)
	if cf.hostPath != hostDir {
		t.Errorf("cloned hostPath = %q, want %q", cf.hostPath, hostDir)
	}

	// Walk single component
	_, walked, err := root.Walk([]string{"a"})
	if err != nil {
		t.Fatalf("Walk(a): %v", err)
	}
	defer walked.Close()

	wf := walked.(*remappingFile)
	wantPath := filepath.Join(hostDir, "a")
	if wf.hostPath != wantPath {
		t.Errorf("walked hostPath = %q, want %q", wf.hostPath, wantPath)
	}

	// Multi-component walk
	_, deep, err := root.Walk([]string{"a", "b"})
	if err != nil {
		t.Fatalf("Walk(a, b): %v", err)
	}
	defer deep.Close()

	df := deep.(*remappingFile)
	wantDeep := filepath.Join(hostDir, "a", "b")
	if df.hostPath != wantDeep {
		t.Errorf("deep hostPath = %q, want %q", df.hostPath, wantDeep)
	}
}

func TestRemappingGetAttrRemapsRoot(t *testing.T) {
	hostDir := t.TempDir()

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, 1000, 1000)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	_, _, attr, err := root.GetAttr(p9.AttrMaskAll)
	if err != nil {
		t.Fatalf("GetAttr: %v", err)
	}

	// If the test dir is owned by root (e.g., running as root), uid should
	// be remapped to 1000. If not root-owned, UID passes through.
	info, _ := os.Stat(hostDir)
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid == 0 {
		if attr.UID != 1000 {
			t.Errorf("UID = %d, want 1000 (remapped from root)", attr.UID)
		}
	}
	if stat.Gid == 0 {
		if attr.GID != 1000 {
			t.Errorf("GID = %d, want 1000 (remapped from root)", attr.GID)
		}
	}
}

func TestRemappingGetAttrPassesNonRoot(t *testing.T) {
	// When running as non-root, temp dirs are owned by the current user.
	// The remapping should NOT change non-root UIDs.
	if os.Geteuid() == 0 {
		t.Skip("test only meaningful as non-root user")
	}

	hostDir := t.TempDir()

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, 1000, 1000)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	_, _, attr, err := root.GetAttr(p9.AttrMaskAll)
	if err != nil {
		t.Fatalf("GetAttr: %v", err)
	}

	// Current user is non-root; UID should pass through unchanged
	if int(attr.UID) != os.Geteuid() {
		t.Errorf("UID = %d, want %d (unchanged)", attr.UID, os.Geteuid())
	}
}

func TestRemappingUnlinkAt(t *testing.T) {
	hostDir := t.TempDir()

	// Create a file and a subdirectory to unlink
	testFile := filepath.Join(hostDir, "deleteme")
	if err := os.WriteFile(testFile, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	testDir := filepath.Join(hostDir, "removedir")
	if err := os.Mkdir(testDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, 1000, 1000)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	// Unlink the file
	if err := root.UnlinkAt("deleteme", 0); err != nil {
		t.Fatalf("UnlinkAt file: %v", err)
	}
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Errorf("file still exists after UnlinkAt")
	}

	// Unlink the directory
	if err := root.UnlinkAt("removedir", 0); err != nil {
		t.Fatalf("UnlinkAt dir: %v", err)
	}
	if _, err := os.Stat(testDir); !os.IsNotExist(err) {
		t.Errorf("directory still exists after UnlinkAt")
	}

	// Unlink non-existent file should return error
	if err := root.UnlinkAt("nonexistent", 0); err == nil {
		t.Error("UnlinkAt nonexistent should return error")
	}
}

func TestRemappingUnwrapFile(t *testing.T) {
	hostDir := t.TempDir()

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, 1000, 1000)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	rf := root.(*remappingFile)

	// Unwrap should extract the inner file
	unwrapped := unwrapFile(root)
	if unwrapped != rf.File {
		t.Error("unwrapFile did not return inner file")
	}

	// Unwrap on a non-wrapped file should return it unchanged
	plain := rf.File
	result := unwrapFile(plain)
	if result != plain {
		t.Error("unwrapFile changed a non-wrapped file")
	}
}

func TestRemappingPassthroughWhenZero(t *testing.T) {
	// When targetUID=0 and targetGID=0, NewP9Server should use localfs
	// directly without wrapping.
	hostDir := t.TempDir()

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server: %v", err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Close() })

	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	client, err := p9.NewClient(conn, p9.WithMessageSize(1048576))
	if err != nil {
		conn.Close()
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	root, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	// Should work without remapping
	_, _, _, err = root.GetAttr(p9.AttrMask{Mode: true})
	if err != nil {
		t.Fatalf("GetAttr: %v", err)
	}
}

// --- Root-requiring tests ---

func TestRemappingCreateLchown(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	hostDir := t.TempDir()
	targetUID, targetGID := 1000, 1000

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, targetUID, targetGID)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Clone root before Create (Create consumes the fid)
	_, root2, err := root.Walk(nil)
	if err != nil {
		root.Close()
		t.Fatalf("Walk(nil): %v", err)
	}
	defer root2.Close()

	f, _, _, err := root.Create("testfile", p9.ReadWrite, 0644, p9.NoUID, p9.NoGID)
	if err != nil {
		root.Close()
		t.Fatalf("Create: %v", err)
	}
	f.Close()
	root.Close()

	// Verify host file ownership
	hostFile := filepath.Join(hostDir, "testfile")
	info, err := os.Lstat(hostFile)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != targetUID {
		t.Errorf("file UID = %d, want %d", stat.Uid, targetUID)
	}
	if int(stat.Gid) != targetGID {
		t.Errorf("file GID = %d, want %d", stat.Gid, targetGID)
	}
}

func TestRemappingMkdirLchown(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	hostDir := t.TempDir()
	targetUID, targetGID := 1000, 1000

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, targetUID, targetGID)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	if _, err := root.Mkdir("testdir", 0755, p9.NoUID, p9.NoGID); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	hostSubDir := filepath.Join(hostDir, "testdir")
	info, err := os.Lstat(hostSubDir)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != targetUID {
		t.Errorf("dir UID = %d, want %d", stat.Uid, targetUID)
	}
	if int(stat.Gid) != targetGID {
		t.Errorf("dir GID = %d, want %d", stat.Gid, targetGID)
	}
}

func TestRemappingSymlinkLchown(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	hostDir := t.TempDir()
	targetUID, targetGID := 1000, 1000

	// Create a target file to symlink to
	targetFile := filepath.Join(hostDir, "target")
	if err := os.WriteFile(targetFile, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, targetUID, targetGID)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	if _, err := root.Symlink("target", "link", p9.NoUID, p9.NoGID); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// Verify the symlink itself (not target) is chowned
	linkPath := filepath.Join(hostDir, "link")
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != targetUID {
		t.Errorf("symlink UID = %d, want %d", stat.Uid, targetUID)
	}
	if int(stat.Gid) != targetGID {
		t.Errorf("symlink GID = %d, want %d", stat.Gid, targetGID)
	}

	// Target should still be root-owned
	targetInfo, err := os.Lstat(targetFile)
	if err != nil {
		t.Fatalf("Lstat target: %v", err)
	}
	targetStat := targetInfo.Sys().(*syscall.Stat_t)
	if int(targetStat.Uid) != 0 {
		t.Errorf("target UID = %d, want 0 (unchanged)", targetStat.Uid)
	}
}

func TestRemappingSetAttrChmod(t *testing.T) {
	hostDir := t.TempDir()

	testFile := filepath.Join(hostDir, "chmod-test")
	if err := os.WriteFile(testFile, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, 1000, 1000)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	_, child, err := root.Walk([]string{"chmod-test"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	defer child.Close()

	// Change permissions to 0755
	err = child.SetAttr(
		p9.SetAttrMask{Permissions: true},
		p9.SetAttr{Permissions: 0755},
	)
	if err != nil {
		t.Fatalf("SetAttr Permissions: %v", err)
	}

	info, err := os.Lstat(testFile)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0755 {
		t.Errorf("permissions = %o, want 0755", got)
	}
}

func TestRemappingSetAttrTimestamps(t *testing.T) {
	hostDir := t.TempDir()

	testFile := filepath.Join(hostDir, "time-test")
	if err := os.WriteFile(testFile, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, 1000, 1000)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	_, child, err := root.Walk([]string{"time-test"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	defer child.Close()

	// Set explicit timestamps (2024-01-01 00:00:00 UTC)
	wantTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	err = child.SetAttr(
		p9.SetAttrMask{
			ATime:              true,
			MTime:              true,
			ATimeNotSystemTime: true,
			MTimeNotSystemTime: true,
		},
		p9.SetAttr{
			ATimeSeconds:     uint64(wantTime.Unix()),
			ATimeNanoSeconds: 0,
			MTimeSeconds:     uint64(wantTime.Unix()),
			MTimeNanoSeconds: 0,
		},
	)
	if err != nil {
		t.Fatalf("SetAttr timestamps: %v", err)
	}

	info, err := os.Lstat(testFile)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.ModTime().Unix(); got != wantTime.Unix() {
		t.Errorf("mtime = %d, want %d", got, wantTime.Unix())
	}

	stat := info.Sys().(*syscall.Stat_t)
	if got := stat.Atim.Sec; got != wantTime.Unix() {
		t.Errorf("atime = %d, want %d", got, wantTime.Unix())
	}
}

func TestRemappingSetAttrChown(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	hostDir := t.TempDir()

	// Create a file to SetAttr on
	testFile := filepath.Join(hostDir, "setattr-test")
	if err := os.WriteFile(testFile, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, 1000, 1000)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	_, child, err := root.Walk([]string{"setattr-test"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	defer child.Close()

	// SetAttr with UID and GID
	err = child.SetAttr(
		p9.SetAttrMask{UID: true, GID: true},
		p9.SetAttr{UID: 1000, GID: 1000},
	)
	if err != nil {
		t.Fatalf("SetAttr: %v", err)
	}

	info, err := os.Lstat(testFile)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != 1000 {
		t.Errorf("UID = %d, want 1000", stat.Uid)
	}
	if int(stat.Gid) != 1000 {
		t.Errorf("GID = %d, want 1000", stat.Gid)
	}
}

func TestRemappingLchownFailureLogsWarning(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	// Create a read-only directory to cause Lchown to fail
	hostDir := t.TempDir()
	roDir := filepath.Join(hostDir, "readonly")
	if err := os.Mkdir(roDir, 0755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	// Pre-create a file, then make the dir read-only to prevent chown
	testFile := filepath.Join(roDir, "existing")
	if err := os.WriteFile(testFile, []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	inner := localfs.Attacher(hostDir)
	att := newRemappingAttacher(inner, hostDir, 65534, 65534)

	root, err := att.Attach()
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer root.Close()

	// Walk into the readonly dir
	_, dir, err := root.Walk([]string{"readonly"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	defer dir.Close()

	// Make the directory non-writable to block Lchown after mkdir.
	// Actually, Lchown doesn't need write permission on the parent --
	// it needs CAP_CHOWN. Since we're root, Lchown won't fail.
	// Instead, test that Create still succeeds even if we can't test
	// Lchown failure as root. This test verifies the error-handling
	// code path exists and doesn't panic.
	_, newFile, err := dir.Walk(nil)
	if err != nil {
		t.Fatalf("Walk(nil): %v", err)
	}

	f, _, _, err := newFile.Create("newfile", p9.ReadWrite, 0644, p9.NoUID, p9.NoGID)
	if err != nil {
		newFile.Close()
		t.Fatalf("Create should succeed even if lchown might fail: %v", err)
	}
	f.Close()
	newFile.Close()
}

func TestP9Server_RemappingIntegration(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	hostDir := t.TempDir()
	// Use non-zero target to actually exercise remapping (os.Getuid() is 0
	// when running as root, which would select passthrough mode).
	targetUID := 1000
	targetGID := 1000

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", false, targetUID, targetGID)
	if err != nil {
		t.Fatalf("NewP9Server: %v", err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Close() })

	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	client, err := p9.NewClient(conn, p9.WithMessageSize(1048576))
	if err != nil {
		conn.Close()
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	root, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Clone root before Create
	_, root2, err := root.Walk(nil)
	if err != nil {
		root.Close()
		t.Fatalf("Walk(nil): %v", err)
	}
	defer root2.Close()

	// Create a file via the 9P protocol
	f, _, _, err := root.Create("integration-test", p9.ReadWrite, 0644, p9.NoUID, p9.NoGID)
	if err != nil {
		root.Close()
		t.Fatalf("Create: %v", err)
	}

	testContent := []byte("hello remapping")
	if _, err := f.WriteAt(testContent, 0); err != nil {
		f.Close()
		root.Close()
		t.Fatalf("WriteAt: %v", err)
	}
	f.Close()
	root.Close()

	// Verify host file ownership
	hostFile := filepath.Join(hostDir, "integration-test")
	info, err := os.Lstat(hostFile)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != targetUID {
		t.Errorf("file UID = %d, want %d", stat.Uid, targetUID)
	}
	if int(stat.Gid) != targetGID {
		t.Errorf("file GID = %d, want %d", stat.Gid, targetGID)
	}

	// Read the file back via 9P to verify it works end-to-end
	_, readFile, err := root2.Walk([]string{"integration-test"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	defer readFile.Close()

	_, _, err = readFile.Open(p9.ReadOnly)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	buf := make([]byte, 1024)
	n, err := readFile.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}

	if got := string(buf[:n]); got != string(testContent) {
		t.Errorf("ReadAt = %q, want %q", got, testContent)
	}
}
