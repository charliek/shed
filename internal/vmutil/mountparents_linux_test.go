//go:build linux

package vmutil

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestMountParentsScriptRealExec runs mountParentsScript with the host's real
// GNU install / stat and util-linux mountpoint (no shims), as the current
// user in place of shed, against a temporary home. It pins the tool
// assumptions the shim test cannot: the exact flags, stat -c %d, mountpoint's
// 32 for a plain directory, and install -d -o/-g/-m creating an owned 0755
// directory.
//
// The prelude only walks a home on the same filesystem as / (anything else is
// inside a mount), so the walking cases need a temporary home on /'s device;
// the "home inside another mount" case needs one on a different device.
func TestMountParentsScriptRealExec(t *testing.T) {
	for _, tool := range []string{"install", "chown", "stat", "mountpoint"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found on PATH: %v", tool, err)
		}
	}
	me, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	u := me.Username
	// The script passes its user as both owner and group (shed:shed in the
	// guest), so the current user's name must also name a group it is in.
	if g, err := user.LookupGroup(u); err != nil || !inGroup(t, g.Gid) {
		t.Skipf("user %q has no same-named group it belongs to (lookup err %v); "+
			"install -o %[1]s -g %[1]s cannot succeed here", u, err)
	}

	sameDev, otherDev := tempRoots(t)

	type rig struct{ home, outside, out string }
	newRig := func(t *testing.T, base string) rig {
		if base == "" {
			t.Skip("no writable temporary directory on the same filesystem as /; " +
				"the prelude only walks a home on /'s filesystem")
		}
		root, err := os.MkdirTemp(base, "mountparents-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(root) })
		r := rig{
			home:    filepath.Join(root, "home", "shed"),
			outside: filepath.Join(root, "outside"),
			out:     filepath.Join(root, "argv.out"),
		}
		for _, d := range []string{r.home, r.outside} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	// run wraps a recorder that writes its argv (\037-terminated fields) to
	// r.out and exits rc; it returns the recorded argv and the exit status.
	run := func(t *testing.T, r rig, target string, rc int) ([]string, int) {
		t.Helper()
		recorder := `out=$1; rc=$2; shift 2; printf '%s\037' "$@" >"$out"; exit "$rc"`
		args := []string{"-c", mountParentsScript, "sh", target, r.home, u,
			"sh", "-c", recorder, "sh", r.out, strconv.Itoa(rc)}
		cmd := exec.Command("sh", append(args, preludeArgv...)...)
		got := 0
		if out, err := cmd.CombinedOutput(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("run prelude: %v (output %q)", err, out)
			}
			got = ee.ExitCode()
		}
		raw, err := os.ReadFile(r.out)
		if err != nil {
			t.Fatalf("wrapped command never ran: %v", err)
		}
		return strings.Split(strings.TrimSuffix(string(raw), "\x1f"), "\x1f"), got
	}
	assertOwned := func(t *testing.T, p string) {
		t.Helper()
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if !fi.IsDir() || fi.Mode().Perm() != 0o755 || int(st.Uid) != os.Getuid() {
			t.Errorf("%q: mode %v uid %d, want a 0755 directory owned by uid %d", p, fi.Mode(), st.Uid, os.Getuid())
		}
	}

	t.Run("missing chain is created owned by the user", func(t *testing.T) {
		r := newRig(t, sameDev)
		argv, rc := run(t, r, r.home+"/.local/share/opencode", 0)
		for _, rel := range []string{".local", ".local/share", ".local/share/opencode"} {
			assertOwned(t, r.home+"/"+rel)
		}
		if rc != 0 || !reflect.DeepEqual(argv, preludeArgv) {
			t.Errorf("wrapped: rc %d argv %q, want 0 and %q", rc, argv, preludeArgv)
		}
	})

	t.Run("an existing parent is walked (mountpoint says 32) and the rest created", func(t *testing.T) {
		r := newRig(t, sameDev)
		if err := os.Mkdir(r.home+"/.config", 0o700); err != nil {
			t.Fatal(err)
		}
		_, rc := run(t, r, r.home+"/.config/gh", 0)
		assertOwned(t, r.home+"/.config/gh")
		if fi, err := os.Lstat(r.home + "/.config"); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("existing ~/.config changed: %v %v", fi.Mode(), err)
		}
		if rc != 0 {
			t.Errorf("rc %d, want 0", rc)
		}
	})

	t.Run("target outside home is untouched", func(t *testing.T) {
		r := newRig(t, sameDev)
		argv, rc := run(t, r, r.outside+"/x/y", 3)
		assertMissing(t, r.outside+"/x")
		if rc != 3 || !reflect.DeepEqual(argv, preludeArgv) {
			t.Errorf("wrapped: rc %d argv %q, want 3 and %q", rc, argv, preludeArgv)
		}
	})

	t.Run("a symlink component stops the walk", func(t *testing.T) {
		r := newRig(t, sameDev)
		if err := os.Symlink(r.outside, r.home+"/.local"); err != nil {
			t.Fatal(err)
		}
		_, rc := run(t, r, r.home+"/.local/share/opencode", 0)
		assertEmpty(t, r.outside)
		if rc != 0 {
			t.Errorf("rc %d, want 0", rc)
		}
	})

	t.Run("a home on another filesystem than / (inside a mount) is untouched", func(t *testing.T) {
		if otherDev == "" {
			t.Skip("no writable temporary directory on a filesystem other than /'s")
		}
		r := newRig(t, otherDev)
		argv, rc := run(t, r, r.home+"/.local/share/opencode", 3)
		assertMissing(t, r.home+"/.local")
		if rc != 3 || !reflect.DeepEqual(argv, preludeArgv) {
			t.Errorf("wrapped: rc %d argv %q, want 3 and %q", rc, argv, preludeArgv)
		}
	})

	for _, want := range []int{0, 3} {
		t.Run("wrapped argv is intact and exit "+strconv.Itoa(want)+" propagates", func(t *testing.T) {
			r := newRig(t, sameDev)
			argv, rc := run(t, r, r.home+"/.config/gh", want)
			if rc != want || !reflect.DeepEqual(argv, preludeArgv) {
				t.Errorf("wrapped: rc %d argv %q, want %d and %q", rc, argv, want, preludeArgv)
			}
		})
	}
}

func inGroup(t *testing.T, gid string) bool {
	t.Helper()
	n, err := strconv.Atoi(gid)
	if err != nil {
		return false
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("getgroups: %v", err)
	}
	return n == os.Getgid() || slices.Contains(groups, n)
}

// tempRoots returns a writable temporary directory on the same filesystem
// (st_dev) as / and one on a different filesystem, or "" for either it cannot
// find. Candidates: the test temp dir, the package directory (same-device
// fallback, e.g. when /tmp is a tmpfs), /dev/shm and $XDG_RUNTIME_DIR
// (other-device candidates).
func tempRoots(t *testing.T) (sameDev, otherDev string) {
	t.Helper()
	rootDev, ok := devOf("/")
	if !ok {
		t.Fatal("cannot stat /")
	}
	cwd, _ := os.Getwd()
	for _, c := range []string{t.TempDir(), cwd, "/dev/shm", os.Getenv("XDG_RUNTIME_DIR")} {
		if c == "" || !writableDir(c) {
			continue
		}
		dev, ok := devOf(c)
		switch {
		case !ok:
		case dev == rootDev && sameDev == "":
			sameDev = c
		case dev != rootDev && otherDev == "":
			otherDev = c
		}
	}
	return sameDev, otherDev
}

func devOf(p string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

func writableDir(p string) bool {
	d, err := os.MkdirTemp(p, ".probe-")
	if err != nil {
		return false
	}
	os.Remove(d)
	return true
}
