package version

import (
	"fmt"
	"regexp"
	"strings"
)

// BuildToolsImageBase is the canonical registry path for the
// shed-build-tools image (pinned mkfs.erofs / mkfs.ext4 used to mint
// rootfs erofs blobs and pre-formatted upper templates). Published tags
// are v-prefixed (vX.Y.Z), versioned in lockstep with shed-server.
const BuildToolsImageBase = "ghcr.io/charliek/shed-build-tools"

// RootfsImageBase is the registry/namespace under which the per-backend
// rootfs images (shed-vz-{base,extensions,full}, shed-fc-{...}) are
// published, in lockstep with each shed release. Used to synthesize the
// default image ref from the running server's version when default_image
// is unset, and to expand the ${shed.version} config token.
const RootfsImageBase = "ghcr.io/charliek"

// releaseVersionRE matches a clean semver tag with or without the leading
// "v" — release binaries embed "X.Y.Z" (no v), `git describe` yields
// "vX.Y.Z" (with v).
var releaseVersionRE = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// ReleaseTag normalizes a version/tag string to a v-prefixed release tag.
// It returns ("vX.Y.Z", true) for a release-shaped input (the v-prefix is
// added when absent), or ("", false) when s is not release-shaped (dev,
// dirty, ahead-of-tag, or a custom name).
//
// Published shed artifacts — build-tools and rootfs images alike — are
// v-prefixed, so concatenating a bare "0.5.4" version yields ":0.5.4", a
// tag that does not exist (this silently disabled the upper-template fast
// path in v0.5.4). Resolve every release tag through here so the prefix
// can't drift between call sites.
func ReleaseTag(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !releaseVersionRE.MatchString(s) {
		return "", false
	}
	if !strings.HasPrefix(s, "v") {
		s = "v" + s
	}
	return s, true
}

// CurrentReleaseTag returns the v-prefixed release tag for this binary's
// Version, or ("", false) for dev/dirty/ahead-of-tag builds.
func CurrentReleaseTag() (string, bool) {
	return ReleaseTag(Version)
}

// BuildToolsRefForTag returns the canonical shed-build-tools image ref
// (BuildToolsImageBase:vX.Y.Z, ALWAYS v-prefixed) for a release-shaped
// version/tag string, or "" if s is not release-shaped (dev, dirty,
// ahead-of-tag, or a custom name).
func BuildToolsRefForTag(s string) string {
	tag, ok := ReleaseTag(s)
	if !ok {
		return ""
	}
	return BuildToolsImageBase + ":" + tag
}

// ReleaseBuildToolsRef returns the canonical shed-build-tools ref for the
// current build's Version, or "" for dev/dirty/ahead-of-tag builds (the
// caller decides the fallback: a locally-built shed-build-tools:dev, or
// skipping the build-tools step entirely).
func ReleaseBuildToolsRef() string {
	return BuildToolsRefForTag(Version)
}

// RootfsRef returns the canonical rootfs image ref for the given backend
// ("vz" or "fc") and variant ("base", "extensions", or "full") at the
// given v-prefixed tag — e.g. RootfsRef("vz", "full", "v0.6.2") yields
// "ghcr.io/charliek/shed-vz-full:v0.6.2". Pair with CurrentReleaseTag to
// build the ref for the running binary's version.
func RootfsRef(backend, variant, tag string) string {
	return fmt.Sprintf("%s/shed-%s-%s:%s", RootfsImageBase, backend, variant, tag)
}
