// Package clone implements the fast-path file-copy strategy chain used by
// CopyRootfs. The chain prefers reflink/clonefile primitives that share
// disk extents (near-instant, near-zero physical cost) and transparently
// falls back to full byte-by-byte copies when those aren't supported.
//
// Strategy precedence:
//
//	darwin: Clonefile -> io.Copy
//	linux:  FICLONE   -> copy_file_range -> io.Copy
//	other:  io.Copy
//
// This package has no shed-internal dependencies by design. Like
// internal/diskstat, it may be consumed by any backend without creating an
// import cycle through config or vmimage's lifecycle code.
package clone

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Strategy identifies which primitive produced the copy. Operators can
// grep the info log emitted by CopyRootfs to confirm reflink is active.
type Strategy int

const (
	// StrategyUnknown is the zero value, meaning no strategy attempted.
	StrategyUnknown Strategy = iota
	// StrategyClonefile: darwin unix.Clonefile (APFS extent sharing).
	StrategyClonefile
	// StrategyFICLONE: linux ioctl_ficlone (btrfs/xfs/ext4 reflink).
	StrategyFICLONE
	// StrategyCopyFileRange: linux copy_file_range(2) — in-kernel bulk copy,
	// no reflink but still avoids userspace buffer shuffling.
	StrategyCopyFileRange
	// StrategyIOCopy: universal fallback — reads the source through userspace.
	StrategyIOCopy
)

// String returns a lowercase name suitable for log lines (e.g. "clonefile").
func (s Strategy) String() string {
	switch s {
	case StrategyClonefile:
		return "clonefile"
	case StrategyFICLONE:
		return "ficlone"
	case StrategyCopyFileRange:
		return "copy_file_range"
	case StrategyIOCopy:
		return "io_copy"
	default:
		return "unknown"
	}
}

// errNotSupported is returned by platform-specific helpers when the
// primitive is unavailable on this build target or filesystem. Callers
// treat it as a cue to try the next strategy.
var errNotSupported = errors.New("clone: strategy not supported")

// errShortCopy is a cross-platform sentinel referenced by isFallback.
// The linux build sets a real value via clone_linux.go's init; other
// builds leave it as a never-matched sentinel so errors.Is returns false.
var errShortCopy = errors.New("clone: copy_file_range returned EOF before source exhausted")

// isFallback reports whether err is a "try the next strategy" signal.
// Fallback triggers are filesystem / kernel signals meaning the primitive
// isn't supported HERE (ENOTSUP, EOPNOTSUPP, EXDEV, EINVAL, ENOTTY,
// ENOSYS, and our own errNotSupported/errShortCopy sentinels). Real
// errors (EIO, ENOSPC, EACCES, EPERM) propagate unchanged.
func isFallback(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errNotSupported) || errors.Is(err, errShortCopy) {
		return true
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	// ENOTSUP and EOPNOTSUPP are the same errno on linux (95); listing
	// both in the switch would duplicate the case. Check separately.
	if errno == syscall.ENOTSUP || errno == syscall.EOPNOTSUPP {
		return true
	}
	switch errno {
	case syscall.EXDEV, syscall.EINVAL, syscall.ENOTTY, syscall.ENOSYS:
		return true
	}
	return false
}

// forceFallback, when set via the ForceFallback test helper, disables all
// platform-specific primitives so CloneFile exercises the io.Copy path.
// Lives in this file (not a _test.go) so production callers can't see it.
var forceFallback bool

// ForceFallback is a test hook that disables clonefile / FICLONE /
// copy_file_range for the duration of the returned function's caller.
// Callers should defer the returned restore func.
func ForceFallback(enable bool) (restore func()) {
	prev := forceFallback
	forceFallback = enable
	return func() { forceFallback = prev }
}

// CloneFile copies src to dst using the fastest strategy available.
// dst MUST NOT exist — Clonefile fails with EEXIST otherwise, and the
// higher-level shed create flow already cleans up stale instance rootfs
// files before calling in.
//
// Returns the strategy that produced the copy so callers can log it.
func CloneFile(src, dst string) (Strategy, error) {
	if src == "" || dst == "" {
		return StrategyUnknown, errors.New("clone: src and dst must be non-empty")
	}

	if !forceFallback {
		if s, err := tryPlatformClone(src, dst); err == nil {
			return s, nil
		} else if !isFallback(err) {
			return StrategyUnknown, err
		}
	}

	if !forceFallback {
		if s, err := tryCopyFileRange(src, dst); err == nil {
			return s, nil
		} else if !isFallback(err) {
			return StrategyUnknown, err
		}
	}

	if err := ioCopy(src, dst); err != nil {
		return StrategyUnknown, err
	}
	return StrategyIOCopy, nil
}

// ioCopy is the universal fallback. Creates dst and streams src through
// userspace. Preserves source mode (0644 on base rootfs) via os.Create's
// umask-masked 0666 — identical to the pre-clone CopyRootfs behavior.
//
// Close errors are explicitly propagated: io.Copy can succeed while a
// deferred flush on Close fails (late ENOSPC is the classic case).
// Returning success there would leave a truncated rootfs treated as
// valid, so we close explicitly and unlink on failure.
func ioCopy(src, dst string) error {
	sf, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer sf.Close()

	// O_EXCL: dst must not already exist, same contract as Clonefile.
	df, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}

	if _, err := io.Copy(df, sf); err != nil {
		df.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}
	if err := df.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close dst: %w", err)
	}
	return nil
}
