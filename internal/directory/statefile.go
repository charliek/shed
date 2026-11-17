package directory

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// The state directory's names and limits (plan section 3.2 and 3.6). The
// directory, <state dir>/directory, holds the server's authorization state:
// its enrolment (its key) and its kept copy of the list.
const (
	// StateDirName is the directory under the server's state directory.
	StateDirName = "directory"
	// EnrolmentFileName holds the enrolment, written once.
	EnrolmentFileName = "enrolment.json"
	// CopyFileName holds the kept copy of the list.
	CopyFileName = "users.json"
	// MaxEnrolmentFileSize and MaxCopyFileSize bound what is read back.
	MaxEnrolmentFileSize = 16 << 10
	MaxCopyFileSize      = 12 << 20

	stateDirMode  fs.FileMode = 0o700
	stateFileMode fs.FileMode = 0o600
)

// Test hooks, nil outside tests: run between the directory's check by name
// and its opening, and between the opened directory's check and the first
// operation on a file in it.
var (
	hookBeforeDirOpen   func()
	hookAfterDirChecked func()
)

// FileCheckError is a state directory or file that failed a check: the path
// and the check, in words.
type FileCheckError struct {
	Path  string
	Check string
}

func (e *FileCheckError) Error() string { return e.Path + ": " + e.Check }

// owner is the user the server runs as: the owner its state must have.
func owner() uint32 { return uint32(os.Geteuid()) }

// checkOwner checks fi, the Lstat of path, is owned by the user the server
// runs as. A platform that does not say who owns a file fails the check.
func checkOwner(path string, fi fs.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != owner() {
		return &FileCheckError{path, fmt.Sprintf("is not owned by the user the server runs as (uid %d)", owner())}
	}
	return nil
}

// EnsureStateDir creates dir with mode 0700 when it does not exist (its
// parent must), then checks it (CheckStateDir). A directory it creates is
// given mode 0700 explicitly, whatever the umask took away; an existing
// directory is never changed.
func EnsureStateDir(dir string) error {
	err := os.Mkdir(dir, stateDirMode)
	switch {
	case err == nil:
		if err := os.Chmod(dir, stateDirMode); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrExist):
		return err
	}
	return CheckStateDir(dir)
}

// CheckStateDir checks dir is a directory, not a symbolic link, owned by the
// user the server runs as, with mode 0700 exactly.
func CheckStateDir(dir string) error {
	root, err := openStateDir(dir)
	if err != nil {
		return err
	}
	return root.Close()
}

// checkDirInfo checks fi, the Lstat of dir or the Stat of the directory
// opened: a directory, not a symbolic link, owned by the server's user,
// with mode 0700 exactly.
func checkDirInfo(dir string, fi fs.FileInfo) error {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return &FileCheckError{dir, "is a symbolic link"}
	case !fi.IsDir():
		return &FileCheckError{dir, "is not a directory"}
	}
	if err := checkOwner(dir, fi); err != nil {
		return err
	}
	if m := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); m != stateDirMode {
		return &FileCheckError{dir, fmt.Sprintf("has mode %04o, not 0700", uint32(m.Perm()))}
	}
	return nil
}

// openStateDir opens dir and checks the directory opened, and returns it
// as an os.Root: every operation on a state file then goes through that
// open directory, so a directory put in dir's place after the check is
// neither read from nor written into. dir's last component is never
// followed through a symbolic link: it is refused when Lstat shows one, and
// the directory opened must be the very directory that Lstat saw (one
// swapped for a link in between is refused).
func openStateDir(dir string) (*os.Root, error) {
	before, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if err := checkDirInfo(dir, before); err != nil {
		return nil, err
	}
	if hookBeforeDirOpen != nil {
		hookBeforeDirOpen()
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	switch {
	case err != nil:
	case !os.SameFile(before, opened):
		err = &FileCheckError{dir, "changed while it was opened"}
	default:
		err = checkDirInfo(dir, opened)
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if hookAfterDirChecked != nil {
		hookAfterDirChecked()
	}
	return root, nil
}

// checkStateFile checks fi, the Lstat of path: a regular file, not a
// symbolic link, owned by the server's user, with no group or other
// permission, at most limit bytes.
func checkStateFile(path string, fi fs.FileInfo, limit int64) error {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return &FileCheckError{path, "is a symbolic link"}
	case !fi.Mode().IsRegular():
		return &FileCheckError{path, "is not a regular file"}
	}
	if err := checkOwner(path, fi); err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return &FileCheckError{path, fmt.Sprintf("has mode %04o: group or others may use it", uint32(fi.Mode().Perm()))}
	}
	if fi.Size() > limit {
		return &FileCheckError{path, fmt.Sprintf("is %d bytes, over the %d it may be", fi.Size(), limit)}
	}
	return nil
}

// ReadStateFile reads dir/name after checking both: the directory as
// CheckStateDir says (the file is then read through the directory checked),
// and the file a regular file (never followed through a symbolic link),
// owned by the server's user, with no group or other permission, of at most
// limit bytes. A missing file is an error satisfying errors.Is(err,
// fs.ErrNotExist); a failed check is a *FileCheckError.
func ReadStateFile(dir, name string, limit int64) ([]byte, error) {
	root, err := openStateDir(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	path := filepath.Join(dir, name)
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := checkStateFile(path, before, limit); err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The file opened must be the one checked, and still pass.
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) {
		return nil, &FileCheckError{path, "changed while it was read"}
	}
	if err := checkStateFile(path, opened, limit); err != nil {
		return nil, err
	}
	data, err := readLimited(f, limit)
	if errors.Is(err, errTooLarge) {
		return nil, &FileCheckError{path, fmt.Sprintf("is over the %d bytes it may be", limit)}
	}
	return data, err
}

// WriteStateFile replaces dir/name with data, mode 0600, durably: a
// temporary file in dir is written and synced, renamed over the name, and
// dir is synced, all through the directory checked (CheckStateDir). An
// error before the rename leaves the old file as it was; an error syncing
// dir leaves the new content in place, perhaps not durable: a caller treats
// both as not written.
func WriteStateFile(dir, name string, data []byte) error {
	root, err := openStateDir(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tmp, err := writeTemp(root, name, data)
	if err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return syncDir(root, dir)
}

// CreateStateFileOnce creates dir/name holding data, mode 0600, only if it
// does not exist: a temporary file in dir is written and synced, then
// linked to the name (so a second writer fails rather than replacing it),
// the temporary name removed, and dir synced, all through the directory
// checked (CheckStateDir). An existing file is an error satisfying
// errors.Is(err, fs.ErrExist), and is left as it was.
//
// An error from any step up to and including the link means dir/name was
// not created by this call. An error after a successful link (removing the
// temporary name, or syncing dir) means dir/name now exists holding data,
// perhaps not yet durable, and the temporary name may remain beside it
// holding the same data: the caller must not report that nothing was
// written (for an enrolment, the key is installed and the client id is the
// one to report). Telling the two apart is the caller's: after an error
// other than fs.ErrExist, dir/name holding exactly data means this call
// created it.
func CreateStateFileOnce(dir, name string, data []byte) error {
	root, err := openStateDir(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tmp, err := writeTemp(root, name, data)
	if err != nil {
		return err
	}
	linkErr := root.Link(tmp, name)
	rmErr := root.Remove(tmp)
	if linkErr != nil {
		return linkErr
	}
	if rmErr != nil {
		return rmErr
	}
	return syncDir(root, dir)
}

// RemoveStateFile removes dir/name durably, through the directory checked
// (CheckStateDir): unlinked, then dir synced. A file already absent is not
// an error.
func RemoveStateFile(dir, name string) error {
	root, err := openStateDir(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(root, dir)
}

// writeTemp writes data to a new temporary file in root, mode 0600, synced
// and closed, and returns its name. On an error nothing is left behind.
func writeTemp(root *os.Root, name string, data []byte) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	tmp := "." + name + "." + hex.EncodeToString(suffix[:]) + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, stateFileMode)
	if err != nil {
		return "", err
	}
	err = f.Chmod(stateFileMode)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = root.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// syncDir syncs the open directory root (dir names it in an error), so a
// rename, link or unlink in it is durable.
func syncDir(root *os.Root, dir string) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}
