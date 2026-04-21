//go:build !darwin && !linux

package clone

// tryPlatformClone and tryCopyFileRange are no-ops on platforms with no
// native extent-sharing primitive; the io.Copy fallback takes over.
func tryPlatformClone(src, dst string) (Strategy, error) { return StrategyUnknown, errNotSupported }

func tryCopyFileRange(src, dst string) (Strategy, error) { return StrategyUnknown, errNotSupported }
