package config

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// ProjectMountBasename validates a host directory destined to be mounted under
// the shed user's home directory and returns the guest-directory basename it
// will be mounted at (HomePath/<basename>).
//
// Dotfile-style names are rejected because project mounts become siblings of
// the home directory's own infrastructure (~/.ssh, ~/.config, ~/.local, ...),
// all of which are dot-prefixed; refusing leading-dot basenames prevents a
// mount from shadowing them.
func ProjectMountBasename(hostDir string) (string, error) {
	base := filepath.Base(filepath.Clean(hostDir))
	switch {
	case base == "" || base == "." || base == ".." || base == string(filepath.Separator):
		return "", fmt.Errorf("cannot derive a mount directory name from %q", hostDir)
	case strings.HasPrefix(base, "."):
		return "", fmt.Errorf("mount directory %q starts with a dot; dotfile-style names are not allowed under the home directory", base)
	}
	return base, nil
}

// BuildProjectMounts performs the structural validation of --local-dir and
// --add-dir and returns the ordered project mounts (the --local-dir entry
// first, then each --add-dir) together with the landing directory.
//
// It validates flag combinations and basename uniqueness but performs NO
// filesystem checks (existence / is-a-directory) — those are done separately on
// whichever host actually owns the directories. When no local dir is given it
// returns (nil, HomePath, nil).
func BuildProjectMounts(localDir string, addDirs []string) ([]MountConfig, string, error) {
	if localDir == "" {
		if len(addDirs) > 0 {
			return nil, "", fmt.Errorf("--add-dir requires --local-dir")
		}
		return nil, HomePath, nil
	}

	dirs := append([]string{localDir}, addDirs...)
	seen := make(map[string]bool, len(dirs))
	mounts := make([]MountConfig, 0, len(dirs))
	landing := HomePath
	for i, d := range dirs {
		base, err := ProjectMountBasename(d)
		if err != nil {
			return nil, "", err
		}
		if seen[base] {
			return nil, "", fmt.Errorf("duplicate mount directory name %q: two mounted directories cannot share a basename", base)
		}
		seen[base] = true
		target := HomePath + "/" + base
		mounts = append(mounts, MountConfig{Source: d, Target: target})
		if i == 0 {
			landing = target
		}
	}
	return mounts, landing, nil
}

// ResolveCreateLayout computes the project mounts and landing directory for a
// (already-validated) create request. --repo lands in the cloned repository
// directory (no project mounts); --local-dir/--add-dir mount under the home
// directory and land in the --local-dir mount; otherwise the home directory.
func ResolveCreateLayout(req CreateShedRequest) (mounts []MountConfig, landingDir string, err error) {
	if req.Repo != "" {
		name, err := RepoDirName(req.Repo)
		if err != nil {
			return nil, "", err
		}
		return nil, HomePath + "/" + name, nil
	}
	return BuildProjectMounts(req.LocalDir, req.AddDirs)
}

// ProjectMountSources returns the host source directories of the given mounts.
func ProjectMountSources(mounts []MountConfig) []string {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]string, len(mounts))
	for i, m := range mounts {
		out[i] = m.Source
	}
	return out
}

// ProjectMountTagForTarget returns the VirtioFS/9P mount tag for a project mount
// given its guest target path (HomePath/<basename>). Guest paths always use '/'.
func ProjectMountTagForTarget(target string) string {
	return ProjectMountTag(path.Base(target))
}
