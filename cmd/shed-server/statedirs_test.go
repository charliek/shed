package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/stallwatch"
)

func TestStateDirs(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.ServerConfig
		want []string
	}{
		{name: "nil config", cfg: nil, want: nil},
		{
			name: "firecracker",
			cfg: &config.ServerConfig{DefaultBackend: config.BackendFirecracker, Firecracker: &config.FirecrackerConfig{
				ImagesDir:    "/srv/fc/images",
				InstanceDir:  "/srv/fc/instances",
				SnapshotsDir: "/srv/fc/snapshots",
				UppersDir:    "/srv/fc/uppers",
				SocketDir:    "/run/shed/fc",
			}},
			want: []string{"/srv/fc/images", "/srv/fc/instances", "/srv/fc", "/srv/fc/snapshots", "/srv/fc/uppers", "/run/shed/fc"},
		},
		{
			name: "firecracker without a block uses the defaults, as runServe does",
			cfg:  &config.ServerConfig{DefaultBackend: config.BackendFirecracker},
			want: []string{
				config.DefaultFirecrackerImagesDir,
				"/var/lib/shed/firecracker/instances",
				"/var/lib/shed/firecracker",
				"/var/lib/shed/firecracker/snapshots",
				"/var/lib/shed/firecracker/uppers",
				"/var/run/shed/firecracker",
			},
		},
		{
			name: "vz, instance parent duplicates images_dir",
			cfg: &config.ServerConfig{DefaultBackend: config.BackendVZ, VZ: &config.VZConfig{
				ImagesDir:    "/Volumes/ext/shed/vz",
				InstanceDir:  "/Volumes/ext/shed/vz/instances",
				SnapshotsDir: "/Volumes/ext/shed/vz/snapshots",
				UppersDir:    "/Volumes/ext/shed/vz/uppers",
				SocketDir:    "/Users/me/.shed/vz/sockets",
			}},
			want: []string{
				"/Volumes/ext/shed/vz",
				"/Volumes/ext/shed/vz/instances",
				"/Volumes/ext/shed/vz/snapshots",
				"/Volumes/ext/shed/vz/uppers",
				"/Users/me/.shed/vz/sockets",
			},
		},
		{
			name: "vz without a block has nothing to probe",
			cfg:  &config.ServerConfig{DefaultBackend: config.BackendVZ},
			want: nil,
		},
		{
			name: "empty values are skipped, and an empty instance_dir adds no parent",
			cfg: &config.ServerConfig{DefaultBackend: config.BackendVZ, VZ: &config.VZConfig{
				ImagesDir: "/s/images",
				SocketDir: "/s/sockets",
			}},
			want: []string{"/s/images", "/s/sockets"},
		},
		{
			name: "exact duplicates are dropped, first position kept",
			cfg: &config.ServerConfig{DefaultBackend: config.BackendFirecracker, Firecracker: &config.FirecrackerConfig{
				ImagesDir:    "/s/images",
				InstanceDir:  "/s/instances",
				SnapshotsDir: "/s/images",
				UppersDir:    "/s/instances",
				SocketDir:    "/s",
			}},
			want: []string{"/s/images", "/s/instances", "/s"},
		},
		{
			// /s/link/../x can be a different directory from /s/x when link
			// is a symlink, so paths are probed exactly as configured.
			name: "paths are probed as configured, not cleaned",
			cfg: &config.ServerConfig{DefaultBackend: config.BackendFirecracker, Firecracker: &config.FirecrackerConfig{
				ImagesDir:    "/s/link/../images",
				SnapshotsDir: "/s/images",
			}},
			want: []string{"/s/link/../images", "/s/images"},
		},
		{
			name: "only the active backend's block counts",
			cfg: &config.ServerConfig{
				DefaultBackend: config.BackendVZ,
				VZ:             &config.VZConfig{ImagesDir: "/vz/images"},
				Firecracker:    &config.FirecrackerConfig{ImagesDir: "/fc/images", InstanceDir: "/fc/instances"},
			},
			want: []string{"/vz/images"},
		},
		{
			name: "unresolved backend",
			cfg:  &config.ServerConfig{DefaultBackend: config.BackendDetect, VZ: &config.VZConfig{ImagesDir: "/vz/images"}},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stateDirs(tt.cfg); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("stateDirs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStateDirWarning(t *testing.T) {
	const app = "/opt/homebrew/Cellar/shed/0.9.1/libexec/ShedServer.app"
	pathErr := func(errno syscall.Errno) error {
		return &fs.PathError{Op: "open", Path: "/Volumes/ext/shed", Err: errno}
	}
	darwinDenied := ". macOS denied access; grant Full Disk Access to " + app +
		" in System Settings → Privacy & Security → Full Disk Access, then restart shed-server."
	tests := []struct {
		name string
		goos string
		err  error
		want string
	}{
		{"readable", "darwin", nil, ""},
		{"missing is silent", "darwin", pathErr(syscall.ENOENT), ""},
		{"missing is silent on linux", "linux", pathErr(syscall.ENOENT), ""},
		{"EPERM on darwin", "darwin", pathErr(syscall.EPERM),
			"Warning: cannot read /Volumes/ext/shed: open /Volumes/ext/shed: operation not permitted" + darwinDenied},
		{"EACCES on darwin", "darwin", pathErr(syscall.EACCES),
			"Warning: cannot read /Volumes/ext/shed: open /Volumes/ext/shed: permission denied" + darwinDenied},
		{"EACCES on linux has no macOS hint", "linux", pathErr(syscall.EACCES),
			"Warning: cannot read /Volumes/ext/shed: open /Volumes/ext/shed: permission denied"},
		{"other errors are a plain warning", "darwin", pathErr(syscall.EIO),
			"Warning: cannot read /Volumes/ext/shed: open /Volumes/ext/shed: input/output error"},
		{"readdir error", "linux", &fs.PathError{Op: "readdirent", Path: "/Volumes/ext/shed", Err: syscall.ENOTDIR},
			"Warning: cannot read /Volumes/ext/shed: readdirent /Volumes/ext/shed: not a directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stateDirWarning(tt.goos, func() string { return app }, "/Volumes/ext/shed", tt.err); got != tt.want {
				t.Errorf("stateDirWarning()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// lineRecorder collects log lines.
type lineRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *lineRecorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *lineRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func TestCheckStateDirs(t *testing.T) {
	t.Run("logs the list, then one warning per unreadable directory", func(t *testing.T) {
		errs := map[string]error{
			"/s/missing": &fs.PathError{Op: "open", Path: "/s/missing", Err: syscall.ENOENT},
			"/s/denied":  &fs.PathError{Op: "open", Path: "/s/denied", Err: syscall.EPERM},
			"/s/broken":  &fs.PathError{Op: "readdirent", Path: "/s/broken", Err: syscall.EIO},
		}
		var read []string
		rec := &lineRecorder{}
		checkStateDirs([]string{"/s/ok", "/s/missing", "/s/denied", "/s/broken"}, func(dir string) error {
			read = append(read, dir)
			return errs[dir]
		}, "darwin", func() string { return "/Apps/ShedServer.app" }, rec.logf)

		if want := []string{"/s/ok", "/s/missing", "/s/denied", "/s/broken"}; !reflect.DeepEqual(read, want) {
			t.Errorf("read %q, want every directory in order %q", read, want)
		}
		got := rec.snapshot()
		want := []string{
			"Checking state directories: /s/ok, /s/missing, /s/denied, /s/broken",
			"Warning: cannot read /s/denied: open /s/denied: operation not permitted. macOS denied access; grant Full Disk Access to /Apps/ShedServer.app in System Settings → Privacy & Security → Full Disk Access, then restart shed-server.",
			"Warning: cannot read /s/broken: readdirent /s/broken: input/output error",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("lines\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("nothing to probe logs nothing", func(t *testing.T) {
		rec := &lineRecorder{}
		checkStateDirs(nil, func(string) error {
			t.Error("read called with no directories")
			return nil
		}, "darwin", func() string { return "/Apps/ShedServer.app" }, rec.logf)
		if got := rec.snapshot(); len(got) != 0 {
			t.Errorf("logged %q", got)
		}
	})

	// A blocked read produces the stall line naming the path, the probe waits
	// it out, and the stall stops once the read returns (#304, A6).
	t.Run("a blocked read stalls visibly and then carries on", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stalls := &lineRecorder{}
			oldFirst, oldEvery, oldLogf := stallwatch.FirstAfter, stallwatch.RepeatEvery, stallwatch.Logf
			t.Cleanup(func() { stallwatch.FirstAfter, stallwatch.RepeatEvery, stallwatch.Logf = oldFirst, oldEvery, oldLogf })
			stallwatch.FirstAfter, stallwatch.RepeatEvery, stallwatch.Logf = 10*time.Second, 60*time.Second, stalls.logf

			rec := &lineRecorder{}
			checkStateDirs([]string{"/Volumes/ext/shed", "/s/next"}, func(dir string) error {
				if dir == "/Volumes/ext/shed" {
					time.Sleep(75 * time.Second) // until someone answers the prompt
				}
				return nil
			}, "darwin", func() string { return "/Apps/ShedServer.app" }, rec.logf)

			got := stalls.snapshot()
			if len(got) != 2 {
				t.Fatalf("got %d stall line(s), want 2 (at 10s and 70s): %q", len(got), got)
			}
			for i, after := range []string{"after 10s", "after 1m10s"} {
				want := "startup is still waiting on /Volumes/ext/shed (checking state directory) " + after
				if !strings.Contains(got[i], want) {
					t.Errorf("stall line %d = %q, want it to contain %q", i, got[i], want)
				}
			}
			time.Sleep(time.Hour)
			synctest.Wait()
			if n := len(stalls.snapshot()); n != 2 {
				t.Errorf("stall lines kept coming after the probe finished: %d", n)
			}
			if want := []string{"Checking state directories: /Volumes/ext/shed, /s/next"}; !reflect.DeepEqual(rec.snapshot(), want) {
				t.Errorf("probe lines %q, want %q", rec.snapshot(), want)
			}
		})
	})
}

func TestReadStateDir(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	full := filepath.Join(root, "full")
	file := filepath.Join(root, "file")
	for _, d := range []string{empty, full} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "entry"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := readStateDir(empty); err != nil {
		t.Errorf("empty directory: %v, want nil", err)
	}
	if err := readStateDir(full); err != nil {
		t.Errorf("directory with an entry: %v, want nil", err)
	}
	if err := readStateDir(filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing directory: %v, want ErrNotExist", err)
	}
	if err := readStateDir(file); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("regular file: %v, want a non-ENOENT error", err)
	}
}

func TestCheckStateDirsResolvesTargetOnlyForADarwinDenial(t *testing.T) {
	denied := func(dir string) error { return &fs.PathError{Op: "open", Path: dir, Err: syscall.EPERM} }
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			calls := 0
			app := func() string { calls++; return "/Apps/ShedServer.app" }
			checkStateDirs([]string{"/s/ok", "/s/missing"}, func(dir string) error {
				if dir == "/s/missing" {
					return &fs.PathError{Op: "open", Path: dir, Err: syscall.ENOENT}
				}
				return nil
			}, goos, app, func(string, ...any) {})
			checkStateDirs([]string{"/s/denied"}, denied, goos, app, func(string, ...any) {})
			want := 0
			if goos == "darwin" {
				want = 1
			}
			if calls != want {
				t.Errorf("target resolved %d time(s), want %d", calls, want)
			}
		})
	}
}
