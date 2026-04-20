// Package diskstat reports both logical (apparent) and physical (allocated)
// byte counts for a single file. Physical bytes come from stat.Blocks * 512
// (POSIX convention on darwin and linux alike).
//
// This package has no shed-internal dependencies; it must not import config
// or backend. The rule mirrors vmimage's import-cycle constraint so diskstat
// can be used by any package without creating a cycle.
package diskstat

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// BlockSize is the POSIX block size for stat.Blocks accounting.
// Both darwin and linux report st_blocks in 512-byte units regardless of
// the filesystem's actual block size.
const BlockSize = 512

// Stat returns logical (apparent) and physical (allocated) bytes for path.
// Follows symlinks. Logical bytes come from stat.Size; physical from
// stat.Blocks * BlockSize.
//
// Returns an error if path does not exist or cannot be stat'd.
func Stat(path string) (logical, physical int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("diskstat: %w", err)
	}

	logical = info.Size()

	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return logical, logical, errors.New("diskstat: Stat_t unavailable on this platform")
	}
	physical = int64(sys.Blocks) * BlockSize
	return logical, physical, nil
}
