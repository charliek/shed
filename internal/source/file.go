package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// FileName is the store's file name in shed-server's backend state
// directory.
const FileName = "source-bindings.json"

// LockSuffix is appended to the store's path to name its lock file, which
// sits beside it and is never removed.
const LockSuffix = ".lock"

// FileBindings is a Store kept in one file. It holds the file's lock from
// OpenFileBindings until Close.
type FileBindings struct {
	*Store
	path string
	// lock is the open lock file whose flock this store holds; nil once
	// closed. Guarded by Store.writeMu.
	lock *os.File
	// closed: Close has run; every later save fails with ErrClosed. Guarded
	// by Store.writeMu (saves run under it).
	closed bool
	// write saves data to path; tests replace it.
	write func(path string, data []byte) error
}

var _ Bindings = (*FileBindings)(nil)

// OpenFileBindings opens the store at path and takes its lock, and reports
// whether the store was newly started (created) or found.
//
// The lock is an exclusive, non-blocking advisory lock (flock) on path plus
// LockSuffix, held until Close. A second open of the same path, in this
// process or another, fails with ErrLocked naming the path: two stores on one
// file would each keep their own state and overwrite each other's
// acknowledged bindings.
//
// A path with no directory entry is the first start: an empty store is
// written to path, durably, before OpenFileBindings returns (so a directory
// that cannot be written fails here, at start-up, rather than at the first
// admission), and created is true. The store cannot tell this from a file
// that was deleted or not restored from a backup; the caller logs
// StartedEmptyWarning when created is true. See the package comment.
//
// Anything else that is not a valid store fails: an entry that cannot be read
// (a directory, a dangling symlink, no permission), a file that does not
// parse, is incomplete or holds an invalid record (ErrCorrupt), and a file of
// a newer version (ErrNewerVersion). The server must not start then: starting
// with no bindings would let the next machine to connect become each user's
// source. The file is left exactly as it was. To recover, an operator either
// restores the file from a backup (or, for a newer version, runs that
// shed-server again), or moves the file aside and starts the server: every
// binding and remembered machine is then forgotten, each user's next machine
// to connect is bound, and each user should check `shed source status` and
// move it with `shed source use` if it is not the machine they want.
func OpenFileBindings(path string, opts Options) (fb *FileBindings, created bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, createError(path, err)
	}
	lock, err := lockStore(path)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	fb = &FileBindings{path: path, write: writeFileDurable, lock: lock}
	persist := func(data []byte) error {
		// Called under Store.writeMu, as Close is.
		if fb.closed {
			return fmt.Errorf("%w: %s", ErrClosed, fb.path)
		}
		return fb.write(fb.path, data)
	}

	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		st := NewStore(persist, opts)
		data, err := encode(st.cur.Load())
		if err != nil {
			return nil, false, createError(path, err)
		}
		if err := fb.write(path, data); err != nil {
			return nil, false, createError(path, err)
		}
		fb.Store = st
		return fb, true, nil
	} else if err != nil {
		return nil, false, openError(path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, openError(path, err)
	}
	st, err := LoadStore(data, persist, opts)
	if err != nil {
		return nil, false, openError(path, err)
	}
	fb.Store = st
	return fb, false, nil
}

// Path is the store's file.
func (fb *FileBindings) Path() string { return fb.path }

// Close releases the store's lock, after any change in progress has
// finished. Reads keep working on the last state; every later change fails
// with ErrClosed. Closing twice is a no-op.
func (fb *FileBindings) Close() error {
	fb.writeMu.Lock()
	defer fb.writeMu.Unlock()
	fb.closed = true
	if fb.lock == nil {
		return nil
	}
	err := fb.lock.Close() // closing the descriptor releases the flock
	fb.lock = nil
	return err
}

// StartedEmptyWarning is the warning the caller logs when OpenFileBindings
// reports a newly started store.
func StartedEmptyWarning(path string) string {
	return fmt.Sprintf("credential source bindings: no file at %s, so an empty one was created: no user has a credential source, "+
		"and the first machine to connect for each user will become that user's source. "+
		"If this server had bindings before (the file was deleted, or not restored with the rest of the state), "+
		"every user should check 'shed source status' and move it with 'shed source use' if it is not their machine", path)
}

// lockStore opens (creating, 0600) the lock file beside path and takes an
// exclusive, non-blocking flock on it.
func lockStore(path string) (*os.File, error) {
	lockPath := path + LockSuffix
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("credential source bindings %s: opening the lock file: %w; the server does not start without its bindings", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err == nil {
		return f, nil
	}
	_ = f.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, fmt.Errorf("credential source bindings %s: %w: another shed-server, or another open in this process, holds %s; "+
			"two stores on one file would overwrite each other's bindings", path, ErrLocked, lockPath)
	}
	return nil, fmt.Errorf("credential source bindings %s: locking %s: %w; the server does not start without its bindings", path, lockPath, err)
}

// createError explains a first start whose empty store cannot be written.
func createError(path string, err error) error {
	return fmt.Errorf("credential source bindings %s: creating the empty store: %w; the server does not start until it can write it", path, err)
}

// openError explains a store that cannot be opened.
func openError(path string, err error) error {
	hint := "restore it from a backup, or move it aside and restart (every user's next machine to connect is then bound; check with 'shed source status')"
	if errors.Is(err, ErrNewerVersion) {
		hint = "run the shed-server that wrote it, or move it aside and restart (every user's next machine to connect is then bound; check with 'shed source status')"
	}
	return fmt.Errorf("credential source bindings %s: %w; the server does not start without them, because starting with none would let the next machine to connect become a user's source; the file is left as it is: %s", path, err, hint)
}

// writeFileDurable writes data to path, mode 0600, so that once it returns
// nil the content survives a crash: a temporary file in the same directory is
// written, synced and renamed over path, and the directory is synced. An
// error before the rename leaves path unchanged; an error syncing the
// directory leaves the new content at path, perhaps not durable. Callers
// treat both as "not written". (The same steps as the upgrade marker's
// writer in internal/backend.)
func writeFileDurable(path string, data []byte) error {
	return writeDurable(path, data, (*os.File).Sync, syncDir)
}

// writeDurable is writeFileDurable with its two syncs given (tests fail them).
func writeDurable(path string, data []byte, syncFile func(*os.File) error, syncDirectory func(string) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}

// syncDir syncs a directory, so a rename in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
