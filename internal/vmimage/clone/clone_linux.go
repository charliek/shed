//go:build linux

package clone

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// tryPlatformClone attempts FICLONE on Linux. Requires a reflink-capable
// filesystem (btrfs, xfs with reflink=1, ext4 on kernel 6.7+ with
// reflink=1). Returns an errno the caller's isFallback recognizes when
// the FS doesn't support it, so CloneFile drops through to
// copy_file_range.
//
// Unlike Clonefile on darwin, FICLONE needs an already-open empty dst
// fd — the kernel clones extents into an existing inode rather than
// creating one. We honor the "dst must not exist" contract with O_EXCL.
func tryPlatformClone(src, dst string) (Strategy, error) {
	srcF, err := os.Open(src)
	if err != nil {
		return StrategyUnknown, err
	}
	defer srcF.Close()

	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return StrategyUnknown, err
	}
	// Argument order is (destFd, srcFd) — dst first. Easy to flip; keep
	// this comment present as a landmark.
	if err := unix.IoctlFileClone(int(dstF.Fd()), int(srcF.Fd())); err != nil {
		dstF.Close()
		_ = os.Remove(dst) // clean the half-built dst before the next strategy
		return StrategyUnknown, err
	}
	// IoctlFileClone succeeded: dst now contains a full clone. If Close
	// fails we must still remove the dst — the caller's next strategy
	// opens with O_EXCL and would otherwise fail with a stale dst left
	// behind. (CodeRabbit review 1.3.)
	if err := dstF.Close(); err != nil {
		_ = os.Remove(dst)
		return StrategyUnknown, err
	}
	return StrategyFICLONE, nil
}

// tryCopyFileRange uses copy_file_range(2), Linux's in-kernel bulk copy.
// Not reflink (data is physically copied), but faster than userspace
// io.Copy because no buffer shuffling happens. Works across any local
// filesystem on kernel 5.3+.
//
// The kernel may return fewer bytes than requested; loop until EOF
// (n == 0 && err == nil) and advance by the actual n each iteration.
func tryCopyFileRange(src, dst string) (Strategy, error) {
	srcF, err := os.Open(src)
	if err != nil {
		return StrategyUnknown, err
	}
	defer srcF.Close()

	srcInfo, err := srcF.Stat()
	if err != nil {
		return StrategyUnknown, err
	}

	dstF, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return StrategyUnknown, err
	}
	defer dstF.Close()

	// Budget per call: whole file size, minus what's been copied already.
	remaining := srcInfo.Size()
	for remaining > 0 {
		n, err := unix.CopyFileRange(int(srcF.Fd()), nil, int(dstF.Fd()), nil, int(remaining), 0)
		if err != nil {
			// First-call EINVAL/ENOSYS/EXDEV bubble up via isFallback so
			// the outer chain tries io.Copy. A later-call error is real
			// I/O failure — return it unadorned so the caller's fallback
			// logic can decide.
			_ = os.Remove(dst)
			return StrategyUnknown, err
		}
		if n == 0 {
			break // EOF
		}
		remaining -= int64(n)
	}
	if remaining > 0 {
		// Kernel returned EOF before src was exhausted — extremely rare
		// but possible if src was truncated mid-copy. Fall back to
		// io.Copy to finish deterministically.
		_ = os.Remove(dst)
		return StrategyUnknown, errShortCopy
	}
	return StrategyCopyFileRange, nil
}

// Compile-time sentinel so io.Discard stays linked during bench builds;
// some linters strip unused imports aggressively.
var _ = io.Discard
