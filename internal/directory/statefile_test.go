package directory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// stateDir is a fresh <state>/directory, mode 0700.
func stateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), StateDirName)
	if err := EnsureStateDir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// leftovers lists dir's entries other than the names given.
func leftovers(t *testing.T, dir string, names ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
outer:
	for _, e := range entries {
		for _, n := range names {
			if e.Name() == n {
				continue outer
			}
		}
		out = append(out, e.Name())
	}
	return out
}

// TestStateDirChecks checks the directory is made 0700 and refused when it
// is not a directory owned by the server's user with mode 0700 exactly.
func TestStateDirChecks(t *testing.T) {
	dir := stateDir(t)
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("made with mode %04o", m)
	}
	if err := EnsureStateDir(dir); err != nil {
		t.Fatalf("an existing good directory: %v", err)
	}
	for _, m := range []fs.FileMode{0o750, 0o755, 0o701, 0o500, 0o777} {
		if err := os.Chmod(dir, m); err != nil {
			t.Fatal(err)
		}
		var fe *FileCheckError
		if err := EnsureStateDir(dir); !errors.As(err, &fe) {
			t.Errorf("mode %04o: %v", m, err)
		}
	}
	if err := os.Chmod(dir, 0o700|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := CheckStateDir(dir); err == nil {
		t.Error("a sticky directory was accepted")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	link := filepath.Join(parent, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckStateDir(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a symbolic link to a good directory: %v", err)
	}
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckStateDir(file); err == nil {
		t.Error("a file was accepted as the directory")
	}
	if os.Geteuid() != 0 {
		// The root directory is not this user's.
		if err := CheckStateDir("/"); err == nil || !strings.Contains(err.Error(), "not owned") {
			t.Errorf("a directory of another user: %v", err)
		}
		if fi, err := os.Lstat("/etc/hostname"); err == nil {
			if err := checkStateFile("/etc/hostname", fi, 1<<20); err == nil || !strings.Contains(err.Error(), "not owned") {
				t.Errorf("a file of another user: %v", err)
			}
		}
	}
}

// TestEnsureStateDirUnderARestrictiveUmask checks a directory EnsureStateDir
// creates is 0700 whatever the umask took away, and that an existing
// directory with another mode is refused and left as it was.
func TestEnsureStateDirUnderARestrictiveUmask(t *testing.T) {
	dir := filepath.Join(t.TempDir(), StateDirName)
	old := syscall.Umask(0o277)
	err := EnsureStateDir(dir)
	syscall.Umask(old)
	if err != nil {
		t.Fatalf("under umask 0277: %v", err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("made with mode %04o under umask 0277", m)
	}

	existing := filepath.Join(t.TempDir(), StateDirName)
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := EnsureStateDir(existing); err == nil {
		t.Error("an existing directory with mode 0750 was accepted")
	}
	if m := mode(t, existing); m != 0o750 {
		t.Errorf("an existing directory's mode changed to %04o", m)
	}
}

// swapDir moves dir aside (to dir+".moved") and puts in its place a
// symbolic link to a good state directory holding name with content "evil",
// mode 0600: owned by this user, 0700, a regular file, so every check of
// the directory and the file by its path would pass. It returns the
// directory the link names.
func swapDir(t *testing.T, dir, name string) string {
	t.Helper()
	evil := filepath.Join(t.TempDir(), "evil")
	if err := EnsureStateDir(evil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, name), []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, dir); err != nil {
		t.Fatal(err)
	}
	return evil
}

// setHook sets *hook to f for the rest of the test.
func setHook(t *testing.T, hook *func(), f func()) {
	t.Helper()
	*hook = f
	t.Cleanup(func() { *hook = nil })
}

// TestStateDirSwappedAfterItsCheck replaces the state directory by a
// symbolic link to another, good-looking directory after the directory was
// opened and checked: reads, writes, links and removals still go to the
// directory checked (now moved aside), never through the link.
func TestStateDirSwappedAfterItsCheck(t *testing.T) {
	for _, op := range []string{"read", "write", "create", "remove"} {
		t.Run(op, func(t *testing.T) {
			dir := stateDir(t)
			if err := WriteStateFile(dir, CopyFileName, []byte("good")); err != nil {
				t.Fatal(err)
			}
			var evil string
			setHook(t, &hookAfterDirChecked, func() {
				hookAfterDirChecked = nil
				evil = swapDir(t, dir, CopyFileName)
			})
			moved := dir + ".moved"
			switch op {
			case "read":
				got, err := ReadStateFile(dir, CopyFileName, 16)
				if err != nil || string(got) != "good" {
					t.Fatalf("read %q, %v: want the checked directory's file", got, err)
				}
			case "write":
				if err := WriteStateFile(dir, CopyFileName, []byte("new")); err != nil {
					t.Fatal(err)
				}
				if got, _ := os.ReadFile(filepath.Join(moved, CopyFileName)); string(got) != "new" {
					t.Errorf("the checked directory holds %q, want the write", got)
				}
			case "create":
				if err := CreateStateFileOnce(dir, EnrolmentFileName, []byte("new")); err != nil {
					t.Fatal(err)
				}
				if got, _ := os.ReadFile(filepath.Join(moved, EnrolmentFileName)); string(got) != "new" {
					t.Errorf("the checked directory holds %q, want the file created", got)
				}
				if _, err := os.Lstat(filepath.Join(evil, EnrolmentFileName)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("created through the link: %v", err)
				}
			case "remove":
				if err := RemoveStateFile(dir, CopyFileName); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(moved, CopyFileName)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the checked directory's file is still there: %v", err)
				}
			}
			if got, _ := os.ReadFile(filepath.Join(evil, CopyFileName)); string(got) != "evil" {
				t.Errorf("the directory behind the link was changed: %q", got)
			}
			if extra := leftovers(t, evil, CopyFileName); len(extra) != 0 {
				t.Errorf("left in the directory behind the link: %v", extra)
			}
		})
	}
}

// TestStateDirSwappedBeforeItIsOpened replaces the state directory by a
// symbolic link to another, good-looking directory between its check by
// name and its opening: the directory opened is not the one checked, and
// nothing is read from or written into it.
func TestStateDirSwappedBeforeItIsOpened(t *testing.T) {
	for _, op := range []string{"read", "write"} {
		t.Run(op, func(t *testing.T) {
			dir := stateDir(t)
			if err := WriteStateFile(dir, CopyFileName, []byte("good")); err != nil {
				t.Fatal(err)
			}
			var evil string
			setHook(t, &hookBeforeDirOpen, func() {
				hookBeforeDirOpen = nil
				evil = swapDir(t, dir, CopyFileName)
			})
			var err error
			var got []byte
			if op == "read" {
				got, err = ReadStateFile(dir, CopyFileName, 16)
			} else {
				err = WriteStateFile(dir, CopyFileName, []byte("new"))
			}
			var fe *FileCheckError
			if !errors.As(err, &fe) || !strings.Contains(err.Error(), "changed while it was opened") {
				t.Fatalf("%s: %q, %v; want the swap refused", op, got, err)
			}
			if got, _ := os.ReadFile(filepath.Join(evil, CopyFileName)); string(got) != "evil" {
				t.Errorf("the directory behind the link was changed: %q", got)
			}
			if extra := leftovers(t, evil, CopyFileName); len(extra) != 0 {
				t.Errorf("left in the directory behind the link: %v", extra)
			}
		})
	}
}

// TestReadStateFileChecks checks a state file is read only when it is a
// regular file (not a symbolic link), with no group or other permission,
// within its size limit, in a good directory.
func TestReadStateFileChecks(t *testing.T) {
	dir := stateDir(t)
	if err := WriteStateFile(dir, EnrolmentFileName, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadStateFile(dir, EnrolmentFileName, 4); err != nil || string(got) != "data" {
		t.Fatalf("read %q, %v", got, err)
	}
	if _, err := ReadStateFile(dir, EnrolmentFileName, 3); err == nil {
		t.Error("a file over its limit was read")
	}
	if _, err := ReadStateFile(dir, CopyFileName, 4); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
	path := filepath.Join(dir, EnrolmentFileName)
	for _, m := range []fs.FileMode{0o640, 0o604, 0o610, 0o601, 0o660} {
		if err := os.Chmod(path, m); err != nil {
			t.Fatal(err)
		}
		var fe *FileCheckError
		if _, err := ReadStateFile(dir, EnrolmentFileName, 4); !errors.As(err, &fe) {
			t.Errorf("mode %04o: %v", m, err)
		}
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateFile(dir, EnrolmentFileName, 4); err != nil {
		t.Errorf("mode 0400: %v", err)
	}

	// A symbolic link to a good file, inside the directory.
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, CopyFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateFile(dir, CopyFileName, 4); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a symbolic link: %v", err)
	}
	// A directory in the file's place.
	if err := os.Remove(filepath.Join(dir, CopyFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, CopyFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateFile(dir, CopyFileName, 4); err == nil {
		t.Error("a directory was read as a file")
	}
	// A good file in a directory that is not 0700.
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateFile(dir, EnrolmentFileName, 4); err == nil {
		t.Error("read from a directory with mode 0750")
	}
}

// TestWriteStateFile checks a durable write replaces the file whole, mode
// 0600, and leaves no temporary file.
func TestWriteStateFile(t *testing.T) {
	dir := stateDir(t)
	for _, content := range []string{"first", "second, longer"} {
		if err := WriteStateFile(dir, CopyFileName, []byte(content)); err != nil {
			t.Fatal(err)
		}
		got, err := ReadStateFile(dir, CopyFileName, MaxCopyFileSize)
		if err != nil || string(got) != content {
			t.Fatalf("read %q, %v", got, err)
		}
		if m := mode(t, filepath.Join(dir, CopyFileName)); m != 0o600 {
			t.Errorf("mode %04o", m)
		}
	}
	if extra := leftovers(t, dir, CopyFileName); len(extra) != 0 {
		t.Errorf("left behind: %v", extra)
	}
	if err := RemoveStateFile(dir, CopyFileName); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStateFile(dir, CopyFileName); err != nil {
		t.Fatalf("removing an absent file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, CopyFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("still there: %v", err)
	}
	// A directory that fails its check is not written to.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteStateFile(dir, CopyFileName, []byte("x")); err == nil {
		t.Error("written into a directory with mode 0755")
	}
	if err := CreateStateFileOnce(dir, EnrolmentFileName, []byte("x")); err == nil {
		t.Error("created in a directory with mode 0755")
	}
}

// TestCreateStateFileOnce checks the file is created once: a second
// create fails with fs.ErrExist and leaves the first, and of many at once
// exactly one wins.
func TestCreateStateFileOnce(t *testing.T) {
	dir := stateDir(t)
	if err := CreateStateFileOnce(dir, EnrolmentFileName, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := CreateStateFileOnce(dir, EnrolmentFileName, []byte("second")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("a second create: %v", err)
	}
	got, err := ReadStateFile(dir, EnrolmentFileName, MaxEnrolmentFileSize)
	if err != nil || string(got) != "first" {
		t.Fatalf("read %q, %v", got, err)
	}
	if m := mode(t, filepath.Join(dir, EnrolmentFileName)); m != 0o600 {
		t.Errorf("mode %04o", m)
	}
	if extra := leftovers(t, dir, EnrolmentFileName); len(extra) != 0 {
		t.Errorf("left behind: %v", extra)
	}

	dir = stateDir(t)
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = CreateStateFileOnce(dir, EnrolmentFileName, []byte{byte('a' + i)})
		}()
	}
	wg.Wait()
	won := -1
	for i, err := range errs {
		switch {
		case err == nil && won >= 0:
			t.Fatalf("writers %d and %d both created the file", won, i)
		case err == nil:
			won = i
		case !errors.Is(err, fs.ErrExist):
			t.Errorf("writer %d: %v", i, err)
		}
	}
	got, _ = ReadStateFile(dir, EnrolmentFileName, MaxEnrolmentFileSize)
	if won < 0 || string(got) != string([]byte{byte('a' + won)}) {
		t.Fatalf("winner %d, file %q", won, got)
	}
	if extra := leftovers(t, dir, EnrolmentFileName); len(extra) != 0 {
		t.Errorf("left behind: %v", extra)
	}
}
