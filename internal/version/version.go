// Package version provides build-time version information.
package version

var (
	// Version is the semantic version (set via ldflags)
	Version = "dev"

	// GitCommit is the git commit hash (set via ldflags)
	GitCommit = "unknown"

	// BuildDate is the build timestamp (set via ldflags)
	BuildDate = "unknown"
)

// Info returns formatted version information
func Info() string {
	return Version
}

// FullInfo returns detailed version information
func FullInfo() string {
	return Version + " (commit: " + GitCommit + ", built: " + BuildDate + ")"
}
