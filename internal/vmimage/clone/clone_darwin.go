//go:build darwin

package clone

import "golang.org/x/sys/unix"

// tryPlatformClone calls Clonefile(2). APFS always supports it; non-APFS
// volumes (exFAT, NFS) return ENOTSUP, which isFallback recognizes as a
// "try the next strategy" cue.
//
// Clonefile REQUIRES dst to not exist (returns EEXIST otherwise). The
// higher-level CopyRootfs caller guarantees this via os.Remove-then-clone.
func tryPlatformClone(src, dst string) (Strategy, error) {
	if err := unix.Clonefile(src, dst, 0); err != nil {
		return StrategyUnknown, err
	}
	return StrategyClonefile, nil
}

// tryCopyFileRange is a no-op on darwin; the io.Copy fallback follows
// Clonefile when it isn't supported. macOS doesn't expose copy_file_range
// and has no equivalent in-kernel bulk copy.
func tryCopyFileRange(src, dst string) (Strategy, error) {
	return StrategyUnknown, errNotSupported
}
