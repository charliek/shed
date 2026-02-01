package tunnels

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// FileLock provides file-based locking using flock.
type FileLock struct {
	path string
	file *os.File
}

// NewFileLock creates a new file lock for the given path.
// The lock file will be created at path + ".lock".
func NewFileLock(path string) *FileLock {
	return &FileLock{
		path: path + ".lock",
	}
}

// Lock acquires an exclusive lock on the file.
// It creates the lock file if it doesn't exist.
func (fl *FileLock) Lock() error {
	// Ensure directory exists
	dir := filepath.Dir(fl.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create lock directory: %w", err)
	}

	// Open or create the lock file
	file, err := os.OpenFile(fl.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("failed to open lock file: %w", err)
	}
	fl.file = file

	// Acquire exclusive lock (blocking)
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		fl.file = nil
		return fmt.Errorf("failed to acquire lock: %w", err)
	}

	return nil
}

// TryLock attempts to acquire an exclusive lock without blocking.
// Returns true if the lock was acquired, false if it's already held.
func (fl *FileLock) TryLock() (bool, error) {
	// Ensure directory exists
	dir := filepath.Dir(fl.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, fmt.Errorf("failed to create lock directory: %w", err)
	}

	// Open or create the lock file
	file, err := os.OpenFile(fl.path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, fmt.Errorf("failed to open lock file: %w", err)
	}
	fl.file = file

	// Try to acquire exclusive lock (non-blocking)
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		fl.file = nil
		if err == syscall.EWOULDBLOCK {
			return false, nil
		}
		return false, fmt.Errorf("failed to acquire lock: %w", err)
	}

	return true, nil
}

// Unlock releases the lock and closes the lock file.
func (fl *FileLock) Unlock() error {
	if fl.file == nil {
		return nil
	}

	// Release the lock
	if err := syscall.Flock(int(fl.file.Fd()), syscall.LOCK_UN); err != nil {
		fl.file.Close()
		fl.file = nil
		return fmt.Errorf("failed to release lock: %w", err)
	}

	if err := fl.file.Close(); err != nil {
		fl.file = nil
		return fmt.Errorf("failed to close lock file: %w", err)
	}

	fl.file = nil
	return nil
}
