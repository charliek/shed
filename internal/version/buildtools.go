package version

import (
	"regexp"
	"strings"
)

// BuildToolsImageBase is the canonical registry path for the
// shed-build-tools image (pinned mkfs.erofs / mkfs.ext4 used to mint
// rootfs erofs blobs and pre-formatted upper templates). Published tags
// are v-prefixed (vX.Y.Z), versioned in lockstep with shed-server.
const BuildToolsImageBase = "ghcr.io/charliek/shed-build-tools"

// releaseVersionRE matches a clean semver tag with or without the leading
// "v" — release binaries embed "X.Y.Z" (no v), `git describe` yields
// "vX.Y.Z" (with v).
var releaseVersionRE = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// BuildToolsRefForTag returns the canonical shed-build-tools image ref
// (BuildToolsImageBase:vX.Y.Z, ALWAYS v-prefixed) for a release-shaped
// version/tag string, or "" if s is not release-shaped (dev, dirty,
// ahead-of-tag, or a custom name).
//
// The v-prefix is added when absent. This is the whole point of having
// one shared implementation: published build-tools tags are v-prefixed,
// so concatenating a bare "0.5.4" version yields ":0.5.4" — a tag that
// does not exist, which silently disabled the upper-template fast path in
// v0.5.4 (it fell back to slow in-guest mkfs). Resolve every build-tools
// ref through here so the prefix can't drift between call sites.
func BuildToolsRefForTag(s string) string {
	s = strings.TrimSpace(s)
	if !releaseVersionRE.MatchString(s) {
		return ""
	}
	if !strings.HasPrefix(s, "v") {
		s = "v" + s
	}
	return BuildToolsImageBase + ":" + s
}

// ReleaseBuildToolsRef returns the canonical shed-build-tools ref for the
// current build's Version, or "" for dev/dirty/ahead-of-tag builds (the
// caller decides the fallback: a locally-built shed-build-tools:dev, or
// skipping the build-tools step entirely).
func ReleaseBuildToolsRef() string {
	return BuildToolsRefForTag(Version)
}
