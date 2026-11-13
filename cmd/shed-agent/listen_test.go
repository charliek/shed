//go:build linux

package main

import (
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestParseListen(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		group    string
		wantMode string
		wantDir  string
		wantErr  string
	}{
		{name: "default is vsock", spec: "", wantMode: listenVsock},
		{name: "vsock", spec: "vsock", wantMode: listenVsock},
		{name: "unix dir", spec: "unix:/run/shed-agent", wantMode: listenUnix, wantDir: "/run/shed-agent"},
		{name: "unix dir cleaned", spec: "unix:/run//shed-agent/", wantMode: listenUnix, wantDir: "/run/shed-agent"},
		{name: "unix with group", spec: "unix:/run/shed-agent", group: "shed-link", wantMode: listenUnix, wantDir: "/run/shed-agent"},
		{name: "unix without dir", spec: "unix:", wantErr: "needs a directory"},
		{name: "unix relative dir", spec: "unix:run/x", wantErr: "must be absolute"},
		{name: "group without unix", spec: "vsock", group: "shed-link", wantErr: "--socket-group"},
		{name: "tcp refused", spec: "tcp:127.0.0.1", wantErr: "want"},
		{name: "unknown", spec: "pipe", wantErr: "want"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseListen(tt.spec, tt.group)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseListen(%q, %q) error = %v, want %q", tt.spec, tt.group, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseListen(%q) error = %v", tt.spec, err)
			}
			if got.mode != tt.wantMode || got.dir != tt.wantDir || got.group != tt.group {
				t.Errorf("parseListen(%q) = %+v, want mode %q dir %q group %q", tt.spec, got, tt.wantMode, tt.wantDir, tt.group)
			}
		})
	}
}

// ownGroup returns a group this process may chown to: its primary group.
func ownGroup(t *testing.T) (name string, gid int) {
	t.Helper()
	g, err := user.LookupGroupId(strconv.Itoa(os.Getegid()))
	if err != nil {
		t.Skipf("cannot name own group: %v", err)
	}
	return g.Name, os.Getegid()
}

func statOwner(t *testing.T, path string) (fs.FileMode, uint32, uint32) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	sys := st.Sys().(*syscall.Stat_t)
	return st.Mode(), sys.Uid, sys.Gid
}

func TestUnixListenModes(t *testing.T) {
	groupName, gid := ownGroup(t)
	tests := []struct {
		name     string
		group    string
		wantDir  fs.FileMode
		wantSock fs.FileMode
		wantGid  int // -1: not checked
	}{
		{name: "owner only", group: "", wantDir: 0o700, wantSock: 0o600, wantGid: -1},
		{name: "with socket group", group: groupName, wantDir: 0o750, wantSock: 0o660, wantGid: gid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A permissive umask must not leak into the modes.
			old := syscall.Umask(0)
			defer syscall.Umask(old)

			dir := filepath.Join(t.TempDir(), "agent") // created by prepare
			lc, err := parseListen("unix:"+dir, tt.group)
			if err != nil {
				t.Fatal(err)
			}
			if err := lc.prepare(1024, 1026, 1028); err != nil {
				t.Fatalf("prepare: %v", err)
			}
			for _, port := range []uint32{1024, 1026, 1028} {
				ln, err := lc.listen(port)
				if err != nil {
					t.Fatalf("listen %d: %v", port, err)
				}
				defer ln.Close()
			}

			mode, uid, g := statOwner(t, dir)
			if mode.Perm() != tt.wantDir || !mode.IsDir() {
				t.Errorf("dir mode = %v, want %v", mode, tt.wantDir)
			}
			if int(uid) != os.Geteuid() {
				t.Errorf("dir uid = %d, want %d", uid, os.Geteuid())
			}
			if tt.wantGid >= 0 && int(g) != tt.wantGid {
				t.Errorf("dir gid = %d, want %d", g, tt.wantGid)
			}
			for _, name := range []string{"1024.sock", "1026.sock", "1028.sock"} {
				mode, _, g := statOwner(t, filepath.Join(dir, name))
				if mode.Type() != fs.ModeSocket {
					t.Errorf("%s is %v, want a socket", name, mode.Type())
				}
				if mode.Perm() != tt.wantSock {
					t.Errorf("%s mode = %v, want %v", name, mode.Perm(), tt.wantSock)
				}
				if tt.wantGid >= 0 && int(g) != tt.wantGid {
					t.Errorf("%s gid = %d, want %d", name, g, tt.wantGid)
				}
			}
		})
	}
}

func TestUnixListenPrepareErrors(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string) // dir is the socket dir path
		group   string
		longDir bool // make the socket paths exceed sun_path
		wantErr string
	}{
		{
			name:    "unknown group",
			group:   "shed-no-such-group-xyz",
			wantErr: "socket group",
		},
		{
			name: "dir is a file",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(dir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "socket dir",
		},
		{
			name: "dir is a symlink",
			setup: func(t *testing.T, dir string) {
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "not a directory",
		},
		{
			name:    "socket path too long",
			longDir: true,
			wantErr: "the limit is",
		},
		{
			name: "non-socket file where a socket goes",
			setup: func(t *testing.T, dir string) {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "1026.sock"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "not a socket",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "agent")
			if tt.longDir {
				dir = filepath.Join(dir, strings.Repeat("d", 120))
			}
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			lc, err := parseListen("unix:"+dir, tt.group)
			if err != nil {
				t.Fatal(err)
			}
			err = lc.prepare(1024, 1026, 1028)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("prepare error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// A socket left by a crashed agent is removed, so the restart can bind.
func TestUnixListenRemovesStaleSocket(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "1026.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the file: a crashed agent never unlinks its socket.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()

	lc, err := parseListen("unix:"+dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := lc.prepare(1024, 1026, 1028); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	ln, err := lc.listen(1026)
	if err != nil {
		t.Fatalf("listen after stale socket: %v", err)
	}
	ln.Close()
}

// Two ports with the same number would share a socket path; the second
// listen must fail rather than steal the first.
func TestUnixListenDuplicatePortFails(t *testing.T) {
	lc, err := parseListen("unix:"+filepath.Join(t.TempDir(), "agent"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := lc.prepare(1024, 1024); err != nil {
		t.Fatal(err)
	}
	first, err := lc.listen(1024)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lc.listen(1024); err == nil {
		second.Close()
		t.Fatal("second listen on the same port succeeded")
	}
}
