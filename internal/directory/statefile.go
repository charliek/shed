package directory

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
// and its opening, between the opened directory's check and the first
// operation on a file in it, in CreateOnce after the link (an error it
// returns is the error of a step after the link), and in syncDir before the
// directory is synced (an error it returns is the sync's: the rename, link
// or unlink before it has happened).
var (
	hookBeforeDirOpen   func()
	hookAfterDirChecked func()
	hookAfterLink       func() error
	hookBeforeDirSync   func() error
)

// ErrLocked is the error of StateDir.Lock when another process holds the
// lock.
var ErrLocked = errors.New("held by another process")

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

// StateDir is the state directory opened once and checked: a directory,
// not a symbolic link, owned by the user the server runs as, with mode 0700
// exactly. Every operation on a file in it goes through the directory
// opened, so a directory put in its place by name afterwards is neither
// read from nor written into, and a caller that does several steps (a
// lock, a listing, reads, a removal, a create) does them all in the one
// directory it checked. Close it when done.
type StateDir struct {
	path string
	root *os.Root
}

// OpenStateDir opens dir and checks it (see StateDir). dir's last
// component is never followed through a symbolic link: it is refused when
// Lstat shows one, and the directory opened must be the very directory
// that Lstat saw (one swapped for a link in between is refused).
func OpenStateDir(dir string) (*StateDir, error) {
	root, err := openStateDir(dir)
	if err != nil {
		return nil, err
	}
	return &StateDir{path: dir, root: root}, nil
}

// MakeStateDir creates dir with mode 0700 when it does not exist (its
// parent must), then opens and checks it (OpenStateDir). A directory it
// creates is given mode 0700 explicitly, whatever the umask took away; an
// existing directory is never changed.
func MakeStateDir(dir string) (*StateDir, error) {
	err := os.Mkdir(dir, stateDirMode)
	switch {
	case err == nil:
		if err := os.Chmod(dir, stateDirMode); err != nil {
			return nil, err
		}
	case !errors.Is(err, fs.ErrExist):
		return nil, err
	}
	return OpenStateDir(dir)
}

// Path is the directory's path, as opened.
func (d *StateDir) Path() string { return d.path }

// Join is the path of name in the directory, for messages.
func (d *StateDir) Join(name string) string { return filepath.Join(d.path, name) }

// Close closes the directory.
func (d *StateDir) Close() error { return d.root.Close() }

// EnsureStateDir is MakeStateDir, closing the directory.
func EnsureStateDir(dir string) error {
	d, err := MakeStateDir(dir)
	if err != nil {
		return err
	}
	return d.Close()
}

// CheckStateDir checks dir is a directory, not a symbolic link, owned by the
// user the server runs as, with mode 0700 exactly.
func CheckStateDir(dir string) error {
	d, err := OpenStateDir(dir)
	if err != nil {
		return err
	}
	return d.Close()
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

// openStateDir opens dir, checks the directory opened, and returns it as an
// os.Root (see OpenStateDir).
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

// ReadStateFile reads dir/name after checking both (StateDir.ReadFile).
func ReadStateFile(dir, name string, limit int64) ([]byte, error) {
	d, err := OpenStateDir(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.ReadFile(name, limit)
}

// ReadFile reads name, checked: a regular file (never followed through a
// symbolic link, never opened when it is a FIFO or a device), owned by the
// server's user, with no group or other permission, of at most limit bytes.
// A missing file is an error satisfying errors.Is(err, fs.ErrNotExist); a
// failed check is a *FileCheckError.
func (d *StateDir) ReadFile(name string, limit int64) ([]byte, error) {
	path := d.Join(name)
	before, err := d.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := checkStateFile(path, before, limit); err != nil {
		return nil, err
	}
	f, err := d.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
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

// WriteStateFile replaces dir/name with data (StateDir.WriteFile).
func WriteStateFile(dir, name string, data []byte) error {
	d, err := OpenStateDir(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.WriteFile(name, data)
}

// WriteFile replaces name with data, mode 0600, durably: a temporary file
// is written and synced, renamed over the name, and the directory synced.
// An error before the rename leaves the old file as it was; an error
// syncing the directory leaves the new content in place, perhaps not
// durable: a caller treats both as not written.
func (d *StateDir) WriteFile(name string, data []byte) error {
	tmp, err := writeTemp(d.root, name, data)
	if err != nil {
		return err
	}
	if err := d.root.Rename(tmp, name); err != nil {
		_ = d.root.Remove(tmp)
		return err
	}
	return syncDir(d.root, d.path)
}

// CreateStateFileOnce creates dir/name holding data, only if it does not
// exist (StateDir.CreateOnce).
func CreateStateFileOnce(dir, name string, data []byte) (linked bool, err error) {
	d, err := OpenStateDir(dir)
	if err != nil {
		return false, err
	}
	defer d.Close()
	return d.CreateOnce(name, data)
}

// CreateOnce creates name holding data, mode 0600, only if it does not
// exist: a temporary file is written and synced, then linked to the name
// (so a second writer fails rather than replacing it), the temporary name
// removed, and the directory synced. An existing file is an error
// satisfying errors.Is(err, fs.ErrExist), and is left as it was.
//
// linked reports whether the link happened, apart from err: when it is
// true, name now exists holding data, whatever err says. An error with
// linked true is from a step after the link (removing the temporary name,
// which may then remain beside name holding the same data, or syncing the
// directory, so the file may not yet be durable): the caller must not
// report that nothing was written (for an enrolment, the key is installed).
// An error with linked false means name was not created by this call.
func (d *StateDir) CreateOnce(name string, data []byte) (linked bool, err error) {
	tmp, err := writeTemp(d.root, name, data)
	if err != nil {
		return false, err
	}
	if err := d.root.Link(tmp, name); err != nil {
		_ = d.root.Remove(tmp)
		return false, err
	}
	if hookAfterLink != nil {
		if err := hookAfterLink(); err != nil {
			return true, err
		}
	}
	if err := d.root.Remove(tmp); err != nil {
		return true, err
	}
	return true, syncDir(d.root, d.path)
}

// RemoveStateFile removes dir/name durably (StateDir.Remove).
func RemoveStateFile(dir, name string) error {
	d, err := OpenStateDir(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Remove(name)
}

// Remove removes name durably: unlinked, then the directory synced. A file
// already absent is not an error.
func (d *StateDir) Remove(name string) error {
	if err := d.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(d.root, d.path)
}

// Names lists the names in the directory, sorted.
func (d *StateDir) Names() ([]string, error) {
	f, err := d.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names, nil
}

// Lock takes an exclusive lock (flock) on the file name in the directory,
// made mode 0600 when absent, without waiting: when another process holds
// it, the error satisfies errors.Is(err, ErrLocked). The file must be a
// regular file (never followed through a symbolic link, never opened
// blocking when it is a FIFO). The returned function releases the lock.
func (d *StateDir) Lock(name string) (unlock func(), err error) {
	path := d.Join(name)
	f, err := d.root.OpenFile(name, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, stateFileMode)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	switch {
	case err != nil:
	case !fi.Mode().IsRegular():
		err = &FileCheckError{path, "is not a regular file"}
	default:
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = fmt.Errorf("%s: %w", path, ErrLocked)
		} else if err != nil {
			err = fmt.Errorf("lock %s: %w", path, err)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	// Closing the file releases the lock.
	return func() { _ = f.Close() }, nil
}

// tempSuffixBytes is the random part of a temporary file's name, in bytes
// (written as twice as many lowercase hex digits).
const tempSuffixBytes = 8

// tempName is the temporary file's name for name: "." + name + "." + the
// suffix in lowercase hex + ".tmp".
func tempName(name string, suffix []byte) string {
	return "." + name + "." + hex.EncodeToString(suffix) + ".tmp"
}

// IsTempFileOf reports whether file has the name WriteFile and CreateOnce
// give their temporary file for name. A process that stops between writing
// the temporary file and removing it (a crash, a kill) leaves it behind,
// mode 0600, holding the data meant for name: for enrolment.json, a key.
func IsTempFileOf(file, name string) bool {
	prefix, suffix := "."+name+".", ".tmp"
	if !strings.HasPrefix(file, prefix) || !strings.HasSuffix(file, suffix) ||
		len(file) != len(prefix)+2*tempSuffixBytes+len(suffix) {
		return false
	}
	for _, c := range []byte(file[len(prefix) : len(file)-len(suffix)]) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// writeTemp writes data to a new temporary file in root, mode 0600, synced
// and closed, and returns its name. On an error nothing is left behind.
func writeTemp(root *os.Root, name string, data []byte) (string, error) {
	var suffix [tempSuffixBytes]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	tmp := tempName(name, suffix[:])
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
	if hookBeforeDirSync != nil {
		if err := hookBeforeDirSync(); err != nil {
			return fmt.Errorf("sync directory %s: %w", dir, err)
		}
	}
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
