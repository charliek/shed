package vmutil

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestWithMountParentsArgv(t *testing.T) {
	cmd := []string{"/usr/local/bin/shed-agent", "mount-9p", "--target", "/home/shed/.config//gh/", "--readonly"}
	got := WithMountParents("/home/shed/.config//gh/", cmd)
	want := []string{
		"sudo", "sh", "-c", mountParentsScript, "sh", "/home/shed/.config/gh", "/home/shed", "shed",
		"/usr/local/bin/shed-agent", "mount-9p", "--target", "/home/shed/.config//gh/", "--readonly",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WithMountParents argv:\n got %q\nwant %q", got, want)
	}
}

// The prelude tests run mountParentsScript under the host's own sh with PATH
// set to a directory of shims and nothing else, so no real install / chown /
// stat / mountpoint is ever reachable: the test is independent of GNU tools
// (macOS's /bin/sh is bash in POSIX mode) and a deleted shim is a genuinely
// missing tool (exit 127). Each shim appends its argv to a shared log and
// plays a scripted part chosen by per-test list files in $SHIM_CFG:
//
//	install     mkdir's its last argument (so the walk proceeds) unless the
//	            path is listed in install_fail, which exits 1.
//	chown       does nothing (the test is not root) unless listed in
//	            chown_fail, which exits 1.
//	stat        -c %u: prints 0 for a path listed in stat_root, else 1000.
//	            -c %d: prints 2 for a path listed in dev_other, prints
//	            nothing and exits 1 for one listed in dev_fail, else 1 (so
//	            home and / share a device unless a case says otherwise).
//	mountpoint  exits 0 for a path listed in mountpoint_0, 1 for
//	            mountpoint_1, else 32 (util-linux: not a mountpoint).
//	wrapped     the wrapped mount command: records its argv and exits
//	            $WRAPPED_RC.
//
// Log records are fields joined by \037 and terminated by \036, so arguments
// carrying spaces or newlines survive intact.
const preludeShimCommon = `#!/bin/sh
rec() { { printf '%s\037' "$SHIM_NAME" "$@"; printf '\036'; } >>"$SHIM_LOG"; }
listed() {
	[ -f "$SHIM_CFG/$1" ] || return 1
	while IFS= read -r l; do [ "$l" = "$2" ] && return 0; done <"$SHIM_CFG/$1"
	return 1
}
last() { for a in "$@"; do p=$a; done; }
`

var preludeShims = map[string]string{
	"install": `rec "$@"; last "$@"
listed install_fail "$p" && exit 1
exec "$SHIM_MKDIR" -- "$p"
`,
	"chown": `rec "$@"; last "$@"
listed chown_fail "$p" && exit 1
exit 0
`,
	"stat": `rec "$@"; last "$@"
case "$2" in
%d)
	listed dev_fail "$p" && exit 1
	if listed dev_other "$p"; then echo 2; else echo 1; fi ;;
*)
	if listed stat_root "$p"; then echo 0; else echo 1000; fi ;;
esac
`,
	"mountpoint": `rec "$@"; last "$@"
listed mountpoint_0 "$p" && exit 0
listed mountpoint_1 "$p" && exit 1
exit 32
`,
	"wrapped": `rec "$@"
exit "${WRAPPED_RC:-0}"
`,
}

// preludeArgv is the wrapped command's argv in every case: spaces, quotes, an
// empty element, shell syntax and a newline must all reach it unchanged.
var preludeArgv = []string{"plain", "with space", `it's "quoted"`, "", "$HOME", "*", "new\nline", "--target"}

type preludeRig struct {
	home    string // the shed user's home ($2)
	outside string // a directory outside home: symlink destinations, outside targets
	work    string // the script's working directory (holds glob decoys)
	bin     string // the shims; the script's whole PATH
	cfg     string // the shims' list files
	log     string
	mkdir   string
}

func newPreludeRig(t *testing.T) *preludeRig {
	t.Helper()
	mkdir, err := exec.LookPath("mkdir")
	if err != nil {
		t.Fatalf("mkdir not found: %v", err)
	}
	root := t.TempDir()
	r := &preludeRig{
		home:    filepath.Join(root, "home", "shed"),
		outside: filepath.Join(root, "outside"),
		work:    filepath.Join(root, "work"),
		bin:     filepath.Join(root, "bin"),
		cfg:     filepath.Join(root, "cfg"),
		log:     filepath.Join(root, "calls.log"),
		mkdir:   mkdir,
	}
	for _, d := range []string{r.home, r.outside, r.work, r.bin, r.cfg} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range preludeShims {
		src := preludeShimCommon + "SHIM_NAME=" + name + "\n" + body
		if err := os.WriteFile(filepath.Join(r.bin, name), []byte(src), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// h returns the absolute path of rel under home.
func (r *preludeRig) h(rel string) string { return r.home + "/" + rel }

func (r *preludeRig) mkdirs(t *testing.T, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if err := os.MkdirAll(r.h(rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *preludeRig) symlink(t *testing.T, rel, dest string) {
	t.Helper()
	if err := os.Symlink(dest, r.h(rel)); err != nil {
		t.Fatal(err)
	}
}

// list appends home-relative paths to the shims' list file name.
func (r *preludeRig) list(t *testing.T, name string, rels ...string) {
	t.Helper()
	abs := make([]string, len(rels))
	for i, rel := range rels {
		abs[i] = r.h(rel)
	}
	r.listAbs(t, name, abs...)
}

// listAbs appends absolute paths (e.g. home itself, or /) to list file name.
func (r *preludeRig) listAbs(t *testing.T, name string, paths ...string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(r.cfg, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range paths {
		if _, err := f.WriteString(p + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

type preludeCall struct{ tool, path string }

func (c preludeCall) String() string { return c.tool + " " + strconv.Quote(c.path) }

type preludeResult struct {
	calls   []preludeCall // install/chown/stat/statdev/mountpoint, in order; paths home-relative, home itself "~"
	wrapped [][]string    // every argv the wrapped command received
	stderr  string
	rc      int
}

// run executes the prelude for target, wrapping the `wrapped` shim with
// preludeArgv, and returns what every shim saw. Each tool's fixed arguments
// are checked here; the cases compare the tool+path sequence.
func (r *preludeRig) run(t *testing.T, target string, wrappedRC int) preludeResult {
	t.Helper()
	args := append([]string{"-c", mountParentsScript, "sh", target, r.home, "shed", "wrapped"}, preludeArgv...)
	cmd := exec.Command("sh", args...)
	cmd.Dir = r.work
	cmd.Env = []string{
		"PATH=" + r.bin,
		"SHIM_LOG=" + r.log,
		"SHIM_CFG=" + r.cfg,
		"SHIM_MKDIR=" + r.mkdir,
		"WRAPPED_RC=" + strconv.Itoa(wrappedRC),
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	res := preludeResult{}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run prelude: %v", err)
		}
		res.rc = ee.ExitCode()
	}
	res.stderr = stderr.String()

	raw, err := os.ReadFile(r.log)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	for _, rec := range strings.Split(string(raw), "\x1e") {
		if rec == "" {
			continue
		}
		f := strings.Split(strings.TrimSuffix(rec, "\x1f"), "\x1f")
		tool, argv := f[0], f[1:]
		if tool == "wrapped" {
			res.wrapped = append(res.wrapped, argv)
			continue
		}
		var fixed []string
		switch tool {
		case "install":
			fixed = []string{"-d", "-o", "shed", "-g", "shed", "-m", "0755"}
		case "chown":
			fixed = []string{"shed:shed"}
		case "stat":
			fixed = []string{"-c", "%u"}
			if len(argv) > 1 && argv[1] == "%d" {
				tool, fixed = "statdev", []string{"-c", "%d"}
			}
		case "mountpoint":
			fixed = []string{"-q"}
		}
		if len(argv) != len(fixed)+1 || !reflect.DeepEqual(argv[:len(fixed)], fixed) {
			t.Fatalf("%s called with %q, want %q + one path", tool, argv, fixed)
		}
		p := argv[len(fixed)]
		if rel, ok := strings.CutPrefix(p, r.home+"/"); ok {
			p = rel
		} else if p == r.home {
			p = "~"
		}
		res.calls = append(res.calls, preludeCall{tool, p})
	}
	return res
}

func pcall(tool, rel string) preludeCall { return preludeCall{tool, rel} }

// walked is the call sequence of a prelude that got past the home checks:
// the device stats of home and / (which must match), then calls.
func walked(calls ...preludeCall) []preludeCall {
	return append([]preludeCall{pcall("statdev", "~"), pcall("statdev", "/")}, calls...)
}

func TestMountParentsScript(t *testing.T) {
	special := strings.Join([]string{"a b", "new\nline", "*", "q?", "$(touch pwned)"}, "/")
	specialPrefixes := func() []preludeCall {
		var calls []preludeCall
		parts := strings.Split(special, "/")
		for i := range parts {
			calls = append(calls, pcall("install", strings.Join(parts[:i+1], "/")))
		}
		return calls
	}

	cases := []struct {
		name   string
		setup  func(t *testing.T, r *preludeRig)
		target func(r *preludeRig) string // default: r.h(rel)
		rel    string
		rc     int // the wrapped command's exit status, which must propagate
		want   []preludeCall
		check  func(t *testing.T, r *preludeRig, res preludeResult)
	}{
		{
			name: "missing chain is created as shed in order",
			rel:  ".local/share/opencode",
			want: walked(
				pcall("install", ".local"), pcall("install", ".local/share"), pcall("install", ".local/share/opencode"),
			),
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertDir(t, r.h(".local/share/opencode"))
			},
		},
		{
			name:  "existing shed-owned dirs are untouched",
			setup: func(t *testing.T, r *preludeRig) { r.mkdirs(t, ".local/share/opencode") },
			rel:   ".local/share/opencode",
			want: walked(
				pcall("mountpoint", ".local"), pcall("stat", ".local"),
				pcall("mountpoint", ".local/share"), pcall("stat", ".local/share"),
				pcall("mountpoint", ".local/share/opencode"),
			),
		},
		{
			name:   "target outside home is untouched",
			target: func(r *preludeRig) string { return r.outside + "/x/y" },
			rc:     3,
			want:   nil,
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.outside+"/x")
			},
		},
		{
			name:   "a sibling sharing home's string prefix is outside home",
			target: func(r *preludeRig) string { return r.home + "2/.config/gh" },
			want:   nil,
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.home+"2")
			},
		},
		{
			name: "a symlinked home is untouched",
			setup: func(t *testing.T, r *preludeRig) {
				if err := os.Remove(r.home); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(r.outside, r.home); err != nil {
					t.Fatal(err)
				}
			},
			rel:  ".config/gh",
			want: nil,
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.outside+"/.config")
			},
		},
		{
			// A server mount at /home/shed or /home: the home is the host's.
			name:  "a home on another device than / (inside a mount) is untouched",
			setup: func(t *testing.T, r *preludeRig) { r.listAbs(t, "dev_other", r.home) },
			rel:   ".local/share/opencode",
			rc:    3,
			want:  []preludeCall{pcall("statdev", "~"), pcall("statdev", "/")},
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.h(".local"))
			},
		},
		{
			name:  "an unreadable home device is untouched",
			setup: func(t *testing.T, r *preludeRig) { r.listAbs(t, "dev_fail", r.home) },
			rel:   ".local/share/opencode",
			want:  []preludeCall{pcall("statdev", "~"), pcall("statdev", "/")},
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.h(".local"))
			},
		},
		{
			name:  "an unreadable root device is untouched",
			setup: func(t *testing.T, r *preludeRig) { r.listAbs(t, "dev_fail", "/") },
			rel:   ".local/share/opencode",
			want:  []preludeCall{pcall("statdev", "~"), pcall("statdev", "/")},
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.h(".local"))
			},
		},
		{
			// Two empty stat outputs must not compare equal.
			name:  "both devices unreadable is untouched",
			setup: func(t *testing.T, r *preludeRig) { r.listAbs(t, "dev_fail", r.home, "/") },
			rel:   ".local/share/opencode",
			rc:    3,
			want:  []preludeCall{pcall("statdev", "~"), pcall("statdev", "/")},
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.h(".local"))
			},
		},
		{
			name:  "a symlink at the first component stops the walk",
			setup: func(t *testing.T, r *preludeRig) { r.symlink(t, ".local", r.outside) },
			rel:   ".local/share/opencode",
			want:  walked(),
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertEmpty(t, r.outside)
			},
		},
		{
			name: "a symlink at an intermediate component stops the walk",
			setup: func(t *testing.T, r *preludeRig) {
				r.mkdirs(t, ".local")
				r.symlink(t, ".local/share", r.outside)
				r.list(t, "stat_root", ".local/share", ".local/share/opencode")
			},
			rel:  ".local/share/opencode",
			want: walked(pcall("mountpoint", ".local"), pcall("stat", ".local")),
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertEmpty(t, r.outside)
			},
		},
		{
			name: "a symlink at the leaf stops the walk",
			setup: func(t *testing.T, r *preludeRig) {
				r.mkdirs(t, ".config")
				r.symlink(t, ".config/gh", r.outside)
				r.list(t, "stat_root", ".config/gh")
			},
			rel:  ".config/gh",
			want: walked(pcall("mountpoint", ".config"), pcall("stat", ".config")),
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertEmpty(t, r.outside)
			},
		},
		{
			name: "a mounted ancestor (mountpoint 0) stops the walk",
			setup: func(t *testing.T, r *preludeRig) {
				r.mkdirs(t, ".local")
				r.list(t, "mountpoint_0", ".local")
				r.list(t, "stat_root", ".local")
			},
			rel:  ".local/share/opencode",
			want: walked(pcall("mountpoint", ".local")),
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.h(".local/share"))
			},
		},
		{
			name: "an uninspectable component (mountpoint 1) stops the walk",
			setup: func(t *testing.T, r *preludeRig) {
				r.mkdirs(t, ".local")
				r.list(t, "mountpoint_1", ".local")
				r.list(t, "stat_root", ".local")
			},
			rel:  ".local/share/opencode",
			want: walked(pcall("mountpoint", ".local")),
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.h(".local/share"))
			},
		},
		{
			name: "a missing mountpoint binary (127) stops the walk",
			setup: func(t *testing.T, r *preludeRig) {
				if err := os.Remove(filepath.Join(r.bin, "mountpoint")); err != nil {
					t.Fatal(err)
				}
				r.mkdirs(t, ".local")
				r.list(t, "stat_root", ".local")
			},
			rel:  ".local/share/opencode",
			rc:   3,
			want: walked(),
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertMissing(t, r.h(".local/share"))
			},
		},
		{
			name: "a non-directory component stops the walk",
			setup: func(t *testing.T, r *preludeRig) {
				if err := os.WriteFile(r.h(".local"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				r.list(t, "stat_root", ".local")
			},
			rel:  ".local/share/opencode",
			want: walked(),
		},
		{
			name: "a root-owned non-mountpoint parent is repaired",
			setup: func(t *testing.T, r *preludeRig) {
				r.mkdirs(t, ".local")
				r.list(t, "stat_root", ".local")
			},
			rel: ".local/share/opencode",
			want: walked(
				pcall("mountpoint", ".local"), pcall("stat", ".local"), pcall("chown", ".local"),
				pcall("install", ".local/share"), pcall("install", ".local/share/opencode"),
			),
		},
		{
			name: "the leaf is never chowned, even root-owned",
			setup: func(t *testing.T, r *preludeRig) {
				r.mkdirs(t, ".config/gh")
				r.list(t, "stat_root", ".config", ".config/gh")
			},
			rel: ".config/gh",
			want: walked(
				pcall("mountpoint", ".config"), pcall("stat", ".config"), pcall("chown", ".config"),
				pcall("mountpoint", ".config/gh"),
			),
		},
		{
			name:  "an install failure stops the walk and the wrapped command still runs",
			setup: func(t *testing.T, r *preludeRig) { r.list(t, "install_fail", ".local/share") },
			rel:   ".local/share/opencode",
			rc:    3,
			want:  walked(pcall("install", ".local"), pcall("install", ".local/share")),
			check: func(t *testing.T, r *preludeRig, res preludeResult) {
				assertMissing(t, r.h(".local/share"))
				if want := "shed: could not create " + r.h(".local/share") + "\n"; !strings.Contains(res.stderr, want) {
					t.Errorf("stderr = %q, want it to contain %q", res.stderr, want)
				}
			},
		},
		{
			name: "a chown failure warns and the walk continues",
			setup: func(t *testing.T, r *preludeRig) {
				r.mkdirs(t, ".local")
				r.list(t, "stat_root", ".local")
				r.list(t, "chown_fail", ".local")
			},
			rel: ".local/share",
			want: walked(
				pcall("mountpoint", ".local"), pcall("stat", ".local"), pcall("chown", ".local"),
				pcall("install", ".local/share"),
			),
			check: func(t *testing.T, r *preludeRig, res preludeResult) {
				if want := "shed: could not repair " + r.h(".local") + "\n"; !strings.Contains(res.stderr, want) {
					t.Errorf("stderr = %q, want it to contain %q", res.stderr, want)
				}
			},
		},
		{
			name: "spaces, newlines, globs and command substitutions are literal",
			setup: func(t *testing.T, r *preludeRig) {
				// Decoys a glob in the walk would expand to (relative to
				// the working directory).
				for _, d := range []string{"decoy", "qZ"} {
					if err := os.Mkdir(filepath.Join(r.work, d), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			},
			rel:  special,
			want: walked(specialPrefixes()...),
			check: func(t *testing.T, r *preludeRig, _ preludeResult) {
				assertDir(t, r.h(special))
				root := filepath.Dir(filepath.Dir(r.home))
				err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
					if err == nil && d.Name() == "pwned" {
						t.Errorf("command substitution ran: %q exists", p)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrapped argv is intact and exit 0 propagates",
			rel:  ".config/gh",
			rc:   0,
			want: walked(pcall("install", ".config"), pcall("install", ".config/gh")),
		},
		{
			name: "wrapped argv is intact and a non-zero exit propagates",
			rel:  ".config/gh",
			rc:   3,
			want: walked(pcall("install", ".config"), pcall("install", ".config/gh")),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPreludeRig(t)
			if tc.setup != nil {
				tc.setup(t, r)
			}
			target := r.h(tc.rel)
			if tc.target != nil {
				target = tc.target(r)
			}
			res := r.run(t, target, tc.rc)

			if !reflect.DeepEqual(res.calls, tc.want) {
				t.Errorf("tool calls:\n got %v\nwant %v", res.calls, tc.want)
			}
			if len(res.wrapped) != 1 || !reflect.DeepEqual(res.wrapped[0], preludeArgv) {
				t.Errorf("wrapped command ran with %q, want exactly once with %q", res.wrapped, preludeArgv)
			}
			if res.rc != tc.rc {
				t.Errorf("exit status = %d, want the wrapped command's %d (stderr %q)", res.rc, tc.rc, res.stderr)
			}
			if tc.check != nil {
				tc.check(t, r, res)
			}
		})
	}
}

func assertDir(t *testing.T, p string) {
	t.Helper()
	if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
		t.Errorf("%q is not a directory (err %v)", p, err)
	}
}

func assertMissing(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%q exists (or is unreadable: %v); the walk should not have created it", p, err)
	}
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("%q gained %q; nothing may be created through a symlink", dir, names)
	}
}
