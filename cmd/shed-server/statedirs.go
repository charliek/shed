package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/stallwatch"
)

// probeStateDirs reads each of the active backend's state directories once,
// before the backend starts, so a store macOS has not yet granted access to
// (an external drive waiting on a permission prompt) or a wedged network
// filesystem stalls startup visibly, at a line naming the path, instead of
// hanging somewhere later with no message (#304). It is never fatal, and a
// clean probe does not mean later accesses cannot prompt.
func probeStateDirs(cfg *config.ServerConfig) {
	checkStateDirs(stateDirs(cfg), readStateDir, runtime.GOOS, stallwatch.FullDiskAccessTarget, log.Printf)
}

// stateDirs returns the directories the active backend keeps state in, in a
// fixed order: images_dir, instance_dir, the parent of instance_dir (where the
// egress audit log and profile store live), snapshots_dir, uppers_dir and
// socket_dir. Empty values are skipped and exact duplicates dropped. Paths are
// probed as configured, not lexically cleaned: with a symlink followed by
// "..", the cleaned path can name a different directory than the one the
// backend opens. A Firecracker
// server with no firecracker block uses the defaults, as runServe does; a VZ
// server with no vz block has nothing to probe (runServe refuses to start).
func stateDirs(cfg *config.ServerConfig) []string {
	if cfg == nil {
		return nil
	}
	var images, instances, snapshots, uppers, sockets string
	switch cfg.DefaultBackend {
	case config.BackendFirecracker:
		fc := cfg.Firecracker
		if fc == nil {
			fc = config.DefaultFirecrackerConfig()
		}
		images, instances, snapshots, uppers, sockets = fc.ImagesDir, fc.InstanceDir, fc.SnapshotsDir, fc.UppersDir, fc.SocketDir
	case config.BackendVZ:
		v := cfg.VZ
		if v == nil {
			return nil
		}
		images, instances, snapshots, uppers, sockets = v.ImagesDir, v.InstanceDir, v.SnapshotsDir, v.UppersDir, v.SocketDir
	default:
		return nil
	}

	var instancesParent string
	if instances != "" {
		instancesParent = filepath.Dir(instances)
	}

	var dirs []string
	seen := make(map[string]bool)
	for _, d := range []string{images, instances, instancesParent, snapshots, uppers, sockets} {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	return dirs
}

// checkStateDirs runs read on each directory under a stall watch and logs one
// warning per directory it could not read. read is a seam for tests; app
// names what a darwin permission warning tells the operator to grant Full
// Disk Access to, and is called only for that warning.
func checkStateDirs(dirs []string, read func(string) error, goos string, app func() string, logf func(string, ...any)) {
	if len(dirs) == 0 {
		return
	}
	logf("Checking state directories: %s", strings.Join(dirs, ", "))
	for _, dir := range dirs {
		stop := stallwatch.Watch("checking state directory", dir)
		err := read(dir)
		stop()
		if msg := stateDirWarning(goos, app, dir, err); msg != "" {
			logf("%s", msg)
		}
	}
}

// readStateDir opens dir and reads one entry: enough to trigger, and wait
// out, a macOS permission prompt for the volume holding it.
func readStateDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	_, err = f.Readdirnames(1)
	_ = f.Close()
	if errors.Is(err, io.EOF) { // an empty directory
		return nil
	}
	return err
}

// stateDirWarning classifies a probe result. A missing directory is expected
// on a fresh install and stays silent. A permission error gets its own hint on
// darwin, where it usually means "Don't Allow" was clicked on the access
// prompt, which would otherwise surface later as an unexplained failure.
// Anything else is a plain warning.
func stateDirWarning(goos string, app func() string, dir string, err error) string {
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	msg := fmt.Sprintf("Warning: cannot read %s: %v", dir, err)
	if goos == "darwin" && errors.Is(err, fs.ErrPermission) {
		msg += ". macOS denied access; grant Full Disk Access to " + app() +
			" in System Settings → Privacy & Security → Full Disk Access, then restart shed-server."
	}
	return msg
}
