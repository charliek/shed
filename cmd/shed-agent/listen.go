//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charliek/shed/internal/remote/guest"
	"github.com/mdlayher/vsock"
)

// Listen modes accepted by --listen.
const (
	listenVsock = "vsock"
	listenUnix  = "unix"
)

// Socket modes without --socket-group: owner only. With a group the modes
// come from guest.AgentSocketDirMode and guest.AgentSocketMode.
const (
	privateSocketDirMode fs.FileMode = 0o700
	privateSocketMode    fs.FileMode = 0o600
)

// maxSocketPath is the longest unix socket path Linux binds (sun_path is 108
// bytes including the terminating NUL).
const maxSocketPath = 107

// listenConfig says where the agent's three ports listen. The zero value is
// vsock, today's behaviour on Firecracker and VZ.
type listenConfig struct {
	mode  string // listenVsock or listenUnix; "" means vsock
	dir   string // unix only: the socket directory
	group string // unix only: the group that may open the sockets ("" = owner only)

	gid int // resolved from group by prepare; -1 when no group
}

// parseListen parses the --listen and --socket-group flags.
func parseListen(spec, group string) (listenConfig, error) {
	switch {
	case spec == "" || spec == listenVsock:
		if group != "" {
			return listenConfig{}, errors.New("--socket-group needs --listen unix:<dir>")
		}
		return listenConfig{mode: listenVsock, gid: -1}, nil
	case strings.HasPrefix(spec, listenUnix+":"):
		dir := strings.TrimPrefix(spec, listenUnix+":")
		if dir == "" {
			return listenConfig{}, errors.New("--listen unix: needs a directory")
		}
		if !filepath.IsAbs(dir) {
			return listenConfig{}, fmt.Errorf("--listen unix:%s: directory must be absolute", dir)
		}
		return listenConfig{mode: listenUnix, dir: filepath.Clean(dir), group: group, gid: -1}, nil
	default:
		return listenConfig{}, fmt.Errorf("--listen %q: want %q or %q", spec, listenVsock, listenUnix+":<dir>")
	}
}

func (lc listenConfig) isUnix() bool { return lc.mode == listenUnix }

// socketPath is the unix socket for a port: <dir>/<port>.sock.
func (lc listenConfig) socketPath(port uint32) string {
	return filepath.Join(lc.dir, guest.SocketName(port))
}

// describe names the listen target for the start-up log line.
func (lc listenConfig) describe() string {
	if lc.isUnix() {
		if lc.group != "" {
			return fmt.Sprintf("unix sockets in %s (group %s)", lc.dir, lc.group)
		}
		return "unix sockets in " + lc.dir
	}
	return "vsock ports"
}

// prepare readies the socket directory before any listen. For unix it
// resolves the group, creates the directory if missing, sets its owner group
// and mode, and removes stale sockets for the given ports. vsock needs
// nothing.
func (lc *listenConfig) prepare(ports ...uint32) error {
	if !lc.isUnix() {
		return nil
	}

	dirMode, gid := privateSocketDirMode, -1
	if lc.group != "" {
		g, err := user.LookupGroup(lc.group)
		if err != nil {
			return fmt.Errorf("socket group %q: %w", lc.group, err)
		}
		id, err := strconv.Atoi(g.Gid)
		if err != nil {
			return fmt.Errorf("socket group %q: bad gid %q", lc.group, g.Gid)
		}
		dirMode, gid = guest.AgentSocketDirMode, id
	}
	lc.gid = gid

	if err := os.MkdirAll(lc.dir, privateSocketDirMode); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	st, err := os.Lstat(lc.dir)
	if err != nil {
		return fmt.Errorf("socket dir: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("socket dir %s is not a directory", lc.dir)
	}
	// The owner is the agent's own user (root in an image) even when the
	// directory already existed; the group is set only with --socket-group.
	// The mode is set explicitly, never left to the umask.
	if err := os.Chown(lc.dir, os.Geteuid(), gid); err != nil {
		return fmt.Errorf("chown socket dir: %w", err)
	}
	if err := os.Chmod(lc.dir, dirMode); err != nil {
		return fmt.Errorf("chmod socket dir: %w", err)
	}

	for _, port := range ports {
		path := lc.socketPath(port)
		if len(path) > maxSocketPath {
			return fmt.Errorf("socket path %s is %d bytes; the limit is %d", path, len(path), maxSocketPath)
		}
		st, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if st.Mode().Type() != fs.ModeSocket {
			return fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}
	return nil
}

// listen opens the listener for one port. For unix the socket's group and
// mode are set after bind; the directory's mode (set by prepare) guards the
// moment in between.
func (lc listenConfig) listen(port uint32) (net.Listener, error) {
	if !lc.isUnix() {
		return vsock.Listen(port, nil)
	}

	path := lc.socketPath(port)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	mode := privateSocketMode
	if lc.gid >= 0 {
		mode = guest.AgentSocketMode
		if err := os.Chown(path, -1, lc.gid); err != nil {
			ln.Close()
			return nil, fmt.Errorf("chown %s: %w", path, err)
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return ln, nil
}
