//go:build linux
// +build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/shed/internal/delivery"
)

// layout is a scratch tree: home (the writer's base) and outside (a
// directory beside it that links may point at).
type layout struct {
	home, outside string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	dir := t.TempDir()
	l := layout{home: filepath.Join(dir, "home"), outside: filepath.Join(dir, "outside")}
	for _, d := range []string{l.home, l.outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(l.outside, "victim"), "outside content\n", 0o644)
	return l
}

func mustWrite(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func mustFifo(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshot records every entry under dir: its type and mode, a link's
// target, a file's content hash. Links are not followed.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		desc := fi.Mode().String()
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			desc += " -> " + target
		case fi.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			desc += " " + hex.EncodeToString(sum[:8])
		}
		out[rel] = desc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// noTempFiles fails if a writer temporary file is left anywhere under dir.
func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".shed-") && strings.HasSuffix(d.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", p)
		}
		return nil
	})
}

func file(name, path, mode, content string) delivery.WriterFile {
	return delivery.WriterFile{Name: name, Base: delivery.BaseHome, Path: path, Mode: mode, Content: []byte(content)}
}

func projectFile(name, path, content string) delivery.WriterFile {
	return delivery.WriterFile{Name: name, Base: delivery.BaseProject, Path: path, Mode: "0600", Content: []byte(content)}
}

func env(name, variable, value string) delivery.WriterEnv {
	return delivery.WriterEnv{Name: name, Variable: variable, Value: value}
}

func stage1(files []delivery.WriterFile, envs ...delivery.WriterEnv) delivery.WriterInput {
	if files == nil {
		files = []delivery.WriterFile{}
	}
	if envs == nil {
		envs = []delivery.WriterEnv{}
	}
	return delivery.WriterInput{Version: 1, Stage: 1, Files: files, Env: envs}
}

func stage2(projectDir string, files ...delivery.WriterFile) delivery.WriterInput {
	return delivery.WriterInput{Version: 1, Stage: 2, ProjectDir: projectDir, Files: files, Env: []delivery.WriterEnv{}}
}

type runResult struct {
	code   int
	lines  []delivery.WriterLine
	raw    string
	stderr string
}

// run runs the writer on in (marshalled as the server would) under home,
// failing the test if it takes more than a few seconds (a FIFO would block
// it forever).
func run(t *testing.T, home string, in delivery.WriterInput) runResult {
	t.Helper()
	body, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return runRaw(t, home, body)
}

func runRaw(t *testing.T, home string, body []byte) runResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- applyItems(bytes.NewReader(body), &stdout, &stderr, home) }()
	var code int
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("apply-items blocked")
	}
	res := runResult{code: code, raw: stdout.String(), stderr: stderr.String()}
	for _, l := range strings.Split(strings.TrimSuffix(res.raw, "\n"), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("output line %q: %v", l, err)
		}
		if len(m) != 2 {
			t.Fatalf("output line %q: want exactly name and result", l)
		}
		var wl delivery.WriterLine
		if err := json.Unmarshal([]byte(l), &wl); err != nil {
			t.Fatal(err)
		}
		ok := false
		for _, r := range delivery.WriterLineResults {
			ok = ok || r == wl.Result
		}
		if !ok {
			t.Fatalf("output line %q: result not in the closed list", l)
		}
		res.lines = append(res.lines, wl)
	}
	return res
}

func lines(pairs ...string) []delivery.WriterLine {
	var out []delivery.WriterLine
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, delivery.WriterLine{Name: pairs[i], Result: delivery.ItemResult(pairs[i+1])})
	}
	return out
}

func wantFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s: mode %v, want a regular file", p, fi.Mode())
	}
	if fi.Mode().Perm() != mode {
		t.Errorf("%s: mode %04o, want %04o", p, fi.Mode().Perm(), mode)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != content {
		t.Errorf("%s: content %q, want %q", p, b, content)
	}
}

func wantDirMode(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != mode {
		t.Errorf("%s: mode %v, want a directory %04o", p, fi.Mode(), mode)
	}
}

// TestApplyItemsRefusals covers every path and link case that refuses an
// item. In each the file system, inside home and outside it, is left
// exactly as it was, the item's result names why, and the exit code is 1.
func TestApplyItemsRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, l layout) delivery.WriterInput
		want  delivery.ItemResult
	}{
		{
			name: "symlink as the target, pointing outside",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustSymlink(t, filepath.Join(l.outside, "victim"), filepath.Join(l.home, ".config/gh/hosts.yml"))
				return stage1([]delivery.WriterFile{file("gh", ".config/gh/hosts.yml", "0600", "token")})
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "symlink as the target, pointing inside",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustWrite(t, filepath.Join(l.home, "real"), "keep\n", 0o644)
				mustSymlink(t, "../real", filepath.Join(l.home, "sub/target"))
				return stage1([]delivery.WriterFile{file("f", "sub/target", "0600", "x")})
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "dangling symlink as the target",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustSymlink(t, filepath.Join(l.outside, "nothing-here"), filepath.Join(l.home, "target"))
				return stage1([]delivery.WriterFile{file("f", "target", "0600", "x")})
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "symlink as an intermediate directory, pointing outside",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustMkdir(t, filepath.Join(l.home, "a"))
				mustSymlink(t, l.outside, filepath.Join(l.home, "a/b"))
				return stage1([]delivery.WriterFile{file("f", "a/b/victim", "0600", "x")})
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "symlink as an intermediate directory, pointing inside",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustMkdir(t, filepath.Join(l.home, "a/real"))
				mustSymlink(t, "real", filepath.Join(l.home, "a/b"))
				return stage1([]delivery.WriterFile{file("f", "a/b/c/new", "0600", "x")})
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "symlink as the path's first component",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustSymlink(t, l.outside, filepath.Join(l.home, ".ssh"))
				return stage1([]delivery.WriterFile{file("key", ".ssh/id_ed25519", "0600", "key")})
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "regular file where a directory is needed",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustWrite(t, filepath.Join(l.home, ".config"), "not a dir\n", 0o644)
				return stage1([]delivery.WriterFile{file("f", ".config/gh/hosts.yml", "0600", "x")})
			},
			want: delivery.ResultNotDirectory,
		},
		{
			name: "FIFO where a directory is needed",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustFifo(t, filepath.Join(l.home, "pipe"))
				return stage1([]delivery.WriterFile{file("f", "pipe/x", "0600", "x")})
			},
			want: delivery.ResultNotDirectory,
		},
		{
			name: "directory as the target",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustMkdir(t, filepath.Join(l.home, ".netrc"))
				mustWrite(t, filepath.Join(l.home, ".netrc/inside"), "keep\n", 0o600)
				return stage1([]delivery.WriterFile{file("netrc", ".netrc", "0600", "x")})
			},
			want: delivery.ResultIsDirectory,
		},
		{
			name: "project directory is a symlink inside home",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustMkdir(t, filepath.Join(l.home, "real"))
				mustSymlink(t, "real", filepath.Join(l.home, "proj"))
				return stage2(filepath.Join(l.home, "proj"), projectFile("env", ".env", "x"))
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "project directory is a symlink outside home",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustSymlink(t, l.outside, filepath.Join(l.home, "proj"))
				return stage2(filepath.Join(l.home, "proj"), projectFile("env", "victim", "x"))
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "project directory's first component is a symlink",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustMkdir(t, filepath.Join(l.outside, "proj"))
				mustSymlink(t, l.outside, filepath.Join(l.home, "src"))
				return stage2(filepath.Join(l.home, "src/proj"), projectFile("env", ".env", "x"))
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "project directory missing",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				return stage2(filepath.Join(l.home, "proj"), projectFile("env", ".env", "x"))
			},
			want: delivery.ResultNotDirectory,
		},
		{
			name: "env directory is a symlink",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustMkdir(t, filepath.Join(l.home, ".config"))
				mustSymlink(t, l.outside, filepath.Join(l.home, ".config/environment.d"))
				return stage1(nil, env("tok", "GH_TOKEN", "x"))
			},
			want: delivery.ResultSymlink,
		},
		{
			name: "env file is a symlink",
			setup: func(t *testing.T, l layout) delivery.WriterInput {
				mustSymlink(t, filepath.Join(l.outside, "victim"), filepath.Join(l.home, delivery.EnvFile))
				return stage1(nil, env("tok", "GH_TOKEN", "x"))
			},
			want: delivery.ResultSymlink,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLayout(t)
			in := tc.setup(t, l)
			beforeHome, beforeOut := snapshot(t, l.home), snapshot(t, l.outside)
			res := run(t, l.home, in)
			if res.code != delivery.WriterExitFailed {
				t.Errorf("exit %d, want %d (stderr %q)", res.code, delivery.WriterExitFailed, res.stderr)
			}
			if len(res.lines) != 1 || res.lines[0].Result != tc.want {
				t.Errorf("lines %+v, want one %s", res.lines, tc.want)
			}
			if got := snapshot(t, l.home); !reflect.DeepEqual(got, beforeHome) {
				t.Errorf("home changed:\nbefore %v\nafter  %v", beforeHome, got)
			}
			if got := snapshot(t, l.outside); !reflect.DeepEqual(got, beforeOut) {
				t.Errorf("outside changed:\nbefore %v\nafter  %v", beforeOut, got)
			}
		})
	}
}

// TestApplyItemsWrites covers what the writer writes, and how.
func TestApplyItemsWrites(t *testing.T) {
	t.Run("new file with missing parents created 0700", func(t *testing.T) {
		l := newLayout(t)
		res := run(t, l.home, stage1([]delivery.WriterFile{file("gh", ".config/gh/hosts.yml", "0600", "token: x\n")}))
		if res.code != 0 || !reflect.DeepEqual(res.lines, lines("gh", "ok")) {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		wantFile(t, filepath.Join(l.home, ".config/gh/hosts.yml"), "token: x\n", 0o600)
		wantDirMode(t, filepath.Join(l.home, ".config"), 0o700)
		wantDirMode(t, filepath.Join(l.home, ".config/gh"), 0o700)
		noTempFiles(t, l.home)
	})

	t.Run("mode honoured exactly whatever the umask", func(t *testing.T) {
		for _, mask := range []int{0o022, 0o077, 0o277, 0o000} {
			for _, mode := range []string{"0400", "0600", "0644"} {
				t.Run(fmt.Sprintf("umask %04o mode %s", mask, mode), func(t *testing.T) {
					l := newLayout(t)
					old := syscall.Umask(mask)
					res := run(t, l.home, stage1([]delivery.WriterFile{file("f", "d/e/f", mode, "x")}))
					syscall.Umask(old)
					if res.code != 0 {
						t.Fatalf("got %d %+v", res.code, res.lines)
					}
					wantFile(t, filepath.Join(l.home, "d/e/f"), "x", delivery.WriterFile{Mode: mode}.FileMode())
					wantDirMode(t, filepath.Join(l.home, "d"), 0o700)
					wantDirMode(t, filepath.Join(l.home, "d/e"), 0o700)
				})
			}
		}
	})

	t.Run("pre-existing target replaced with the item's mode", func(t *testing.T) {
		l := newLayout(t)
		mustWrite(t, filepath.Join(l.home, ".netrc"), "old content that is longer\n", 0o644)
		res := run(t, l.home, stage1([]delivery.WriterFile{file("netrc", ".netrc", "0400", "new\n")}))
		if res.code != 0 {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		wantFile(t, filepath.Join(l.home, ".netrc"), "new\n", 0o400)
		noTempFiles(t, l.home)
	})

	t.Run("FIFO at the target is replaced without blocking", func(t *testing.T) {
		l := newLayout(t)
		mustFifo(t, filepath.Join(l.home, "target"))
		res := run(t, l.home, stage1([]delivery.WriterFile{file("f", "target", "0600", "x")}))
		if res.code != 0 || !reflect.DeepEqual(res.lines, lines("f", "ok")) {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		wantFile(t, filepath.Join(l.home, "target"), "x", 0o600)
	})

	t.Run("existing parents are used as they are", func(t *testing.T) {
		l := newLayout(t)
		mustMkdir(t, filepath.Join(l.home, ".ssh"))
		if err := os.Chmod(filepath.Join(l.home, ".ssh"), 0o755); err != nil {
			t.Fatal(err)
		}
		res := run(t, l.home, stage1([]delivery.WriterFile{file("k", ".ssh/id_ed25519", "0600", "key")}))
		if res.code != 0 {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		wantDirMode(t, filepath.Join(l.home, ".ssh"), 0o755)
		wantFile(t, filepath.Join(l.home, ".ssh/id_ed25519"), "key", 0o600)
	})

	t.Run("binary content byte for byte", func(t *testing.T) {
		l := newLayout(t)
		content := string([]byte{0, 1, 2, 0xff, '\n', '"', '\\'})
		res := run(t, l.home, stage1([]delivery.WriterFile{file("b", "bin", "0600", content)}))
		if res.code != 0 {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		wantFile(t, filepath.Join(l.home, "bin"), content, 0o600)
		if strings.Contains(res.raw, "AAEC") || strings.Contains(res.raw, "bin\"") {
			t.Errorf("output carries content or path: %q", res.raw)
		}
	})

	t.Run("project base under home", func(t *testing.T) {
		l := newLayout(t)
		proj := filepath.Join(l.home, "src/widgets")
		mustMkdir(t, proj)
		res := run(t, l.home, stage2(proj, projectFile("dotenv", ".env", "A=1\n"), projectFile("cfg", "config/local.yml", "x"), file("h", ".config/x", "0644", "y")))
		if res.code != 0 || !reflect.DeepEqual(res.lines, lines("dotenv", "ok", "cfg", "ok", "h", "ok")) {
			t.Fatalf("got %d %+v %q", res.code, res.lines, res.stderr)
		}
		wantFile(t, filepath.Join(proj, ".env"), "A=1\n", 0o600)
		wantFile(t, filepath.Join(proj, "config/local.yml"), "x", 0o600)
		wantFile(t, filepath.Join(l.home, ".config/x"), "y", 0o644)
	})

	t.Run("stage stops at the first failure", func(t *testing.T) {
		l := newLayout(t)
		mustWrite(t, filepath.Join(l.home, delivery.EnvFile), "OLD=1\n", 0o600)
		mustMkdir(t, filepath.Join(l.home, "blocked"))
		in := stage1([]delivery.WriterFile{
			file("first", "one", "0600", "1"),
			file("second", "blocked", "0600", "2"),
			file("third", "three", "0600", "3"),
		}, env("tok", "GH_TOKEN", "x"))
		res := run(t, l.home, in)
		want := lines("first", "ok", "second", "is_directory", "third", "not_written", "tok", "not_written")
		if res.code != delivery.WriterExitFailed || !reflect.DeepEqual(res.lines, want) {
			t.Fatalf("got %d %+v, want %+v", res.code, res.lines, want)
		}
		wantFile(t, filepath.Join(l.home, "one"), "1", 0o600)
		if _, err := os.Lstat(filepath.Join(l.home, "three")); !os.IsNotExist(err) {
			t.Errorf("third was written: %v", err)
		}
		wantFile(t, filepath.Join(l.home, delivery.EnvFile), "OLD=1\n", 0o600)
		noTempFiles(t, l.home)
	})

	t.Run("stage 2 with a failing project directory stops before later home items", func(t *testing.T) {
		l := newLayout(t)
		mustSymlink(t, l.outside, filepath.Join(l.home, "proj"))
		res := run(t, l.home, stage2(filepath.Join(l.home, "proj"), file("h", "home-file", "0600", "x"), projectFile("p", "victim", "y"), file("h2", "later", "0600", "z")))
		want := lines("h", "ok", "p", "symlink", "h2", "not_written")
		if !reflect.DeepEqual(res.lines, want) {
			t.Fatalf("got %+v, want %+v", res.lines, want)
		}
		wantFile(t, filepath.Join(l.outside, "victim"), "outside content\n", 0o644)
	})
}

// TestApplyItemsEnv covers the env file.
func TestApplyItemsEnv(t *testing.T) {
	envPath := func(l layout) string { return filepath.Join(l.home, delivery.EnvFile) }

	t.Run("written 0600 with its directories 0700, byte for byte", func(t *testing.T) {
		l := newLayout(t)
		old := syscall.Umask(0o022)
		defer syscall.Umask(old)
		value := `a "quoted" 'single' $HOME and x==y`
		res := run(t, l.home, stage1(nil, env("a", "ALPHA", "1"), env("b", "QUOTED", value)))
		if res.code != 0 || !reflect.DeepEqual(res.lines, lines("a", "ok", "b", "ok")) {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		wantFile(t, envPath(l), "ALPHA=1\nQUOTED="+value+"\n", 0o600)
		wantDirMode(t, filepath.Join(l.home, ".config"), 0o700)
		wantDirMode(t, filepath.Join(l.home, ".config/environment.d"), 0o700)
		if strings.Contains(res.raw, "quoted") {
			t.Errorf("output carries a value: %q", res.raw)
		}
	})

	t.Run("replaced whole", func(t *testing.T) {
		l := newLayout(t)
		mustWrite(t, envPath(l), "OLD=1\nGH_TOKEN=old\n", 0o644)
		res := run(t, l.home, stage1(nil, env("tok", "GH_TOKEN", "new")))
		if res.code != 0 {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		wantFile(t, envPath(l), "GH_TOKEN=new\n", 0o600)
	})

	t.Run("files written before env", func(t *testing.T) {
		l := newLayout(t)
		res := run(t, l.home, stage1([]delivery.WriterFile{file("f", "x", "0600", "1")}, env("tok", "GH_TOKEN", "v")))
		if !reflect.DeepEqual(res.lines, lines("f", "ok", "tok", "ok")) {
			t.Fatalf("got %+v", res.lines)
		}
	})

	t.Run("removed by a stage 1 with no env item", func(t *testing.T) {
		l := newLayout(t)
		mustWrite(t, envPath(l), "GH_TOKEN=old\n", 0o600)
		mustWrite(t, filepath.Join(l.home, ".config/environment.d/10-mine.conf"), "MINE=1\n", 0o644)
		res := run(t, l.home, stage1([]delivery.WriterFile{file("f", "x", "0600", "1")}))
		if res.code != 0 || !reflect.DeepEqual(res.lines, lines("f", "ok")) {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		if _, err := os.Lstat(envPath(l)); !os.IsNotExist(err) {
			t.Errorf("env file not removed: %v", err)
		}
		wantFile(t, filepath.Join(l.home, ".config/environment.d/10-mine.conf"), "MINE=1\n", 0o644)
	})

	t.Run("no env file and no env item is nothing to do", func(t *testing.T) {
		l := newLayout(t)
		before := snapshot(t, l.home)
		res := run(t, l.home, stage1(nil))
		if res.code != 0 || len(res.lines) != 0 {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		if got := snapshot(t, l.home); !reflect.DeepEqual(got, before) {
			t.Errorf("home changed: %v", got)
		}
	})

	t.Run("left alone by a stage 2", func(t *testing.T) {
		l := newLayout(t)
		mustWrite(t, envPath(l), "GH_TOKEN=old\n", 0o600)
		res := run(t, l.home, stage2("", file("f", "x", "0600", "1")))
		if res.code != 0 {
			t.Fatalf("got %d %+v %q", res.code, res.lines, res.stderr)
		}
		wantFile(t, envPath(l), "GH_TOKEN=old\n", 0o600)
	})

	removalRefusals := []struct {
		name  string
		setup func(t *testing.T, l layout)
	}{
		{"env file is a symlink", func(t *testing.T, l layout) {
			mustSymlink(t, filepath.Join(l.outside, "victim"), envPath(l))
		}},
		{"env directory is a symlink", func(t *testing.T, l layout) {
			mustMkdir(t, filepath.Join(l.outside, "envd"))
			mustWrite(t, filepath.Join(l.outside, "envd", delivery.EnvFileName), "X=1\n", 0o600)
			mustMkdir(t, filepath.Join(l.home, ".config"))
			mustSymlink(t, filepath.Join(l.outside, "envd"), filepath.Join(l.home, ".config/environment.d"))
		}},
		{"env file is a directory", func(t *testing.T, l layout) {
			mustMkdir(t, envPath(l))
		}},
		{"env file is a FIFO", func(t *testing.T, l layout) {
			mustFifo(t, envPath(l))
		}},
	}
	for _, tc := range removalRefusals {
		t.Run("removal refused: "+tc.name, func(t *testing.T) {
			l := newLayout(t)
			tc.setup(t, l)
			beforeHome, beforeOut := snapshot(t, l.home), snapshot(t, l.outside)
			res := run(t, l.home, stage1(nil))
			if res.code != delivery.WriterExitFailed || len(res.lines) != 0 {
				t.Errorf("got %d %+v, want exit 1 and no line", res.code, res.lines)
			}
			if got := snapshot(t, l.home); !reflect.DeepEqual(got, beforeHome) {
				t.Errorf("home changed: %v", got)
			}
			if got := snapshot(t, l.outside); !reflect.DeepEqual(got, beforeOut) {
				t.Errorf("outside changed: %v", got)
			}
		})
	}
}

// TestApplyItemsRaces changes the file system between the writer's check and
// its use: a directory swapped for a link after it was checked, a target
// replaced by a directory after the temporary file was written. The writer
// refuses, writes nothing through the link, and leaves no temporary file.
func TestApplyItemsRaces(t *testing.T) {
	t.Cleanup(func() { applyItemsTestHook = nil })

	t.Run("intermediate directory swapped for a link to outside", func(t *testing.T) {
		l := newLayout(t)
		mustMkdir(t, filepath.Join(l.home, "a"))
		applyItemsTestHook = func(point string, dir *os.Root, name string) {
			if point == "descend" && name == "a" {
				_ = os.Remove(filepath.Join(l.home, "a"))
				_ = os.Symlink(l.outside, filepath.Join(l.home, "a"))
			}
		}
		defer func() { applyItemsTestHook = nil }()
		beforeOut := snapshot(t, l.outside)
		res := run(t, l.home, stage1([]delivery.WriterFile{file("f", "a/victim", "0600", "x")}))
		if len(res.lines) != 1 || res.lines[0].Result == delivery.ResultOK {
			t.Fatalf("got %+v, want a refusal", res.lines)
		}
		if got := snapshot(t, l.outside); !reflect.DeepEqual(got, beforeOut) {
			t.Errorf("outside changed: %v", got)
		}
	})

	t.Run("intermediate directory swapped for a link inside", func(t *testing.T) {
		l := newLayout(t)
		mustMkdir(t, filepath.Join(l.home, "a"))
		mustMkdir(t, filepath.Join(l.home, "other"))
		applyItemsTestHook = func(point string, dir *os.Root, name string) {
			if point == "descend" && name == "a" {
				_ = os.Remove(filepath.Join(l.home, "a"))
				_ = os.Symlink("other", filepath.Join(l.home, "a"))
			}
		}
		defer func() { applyItemsTestHook = nil }()
		res := run(t, l.home, stage1([]delivery.WriterFile{file("f", "a/x", "0600", "x")}))
		if !reflect.DeepEqual(res.lines, lines("f", "symlink")) {
			t.Fatalf("got %+v", res.lines)
		}
		if _, err := os.Lstat(filepath.Join(l.home, "other/x")); !os.IsNotExist(err) {
			t.Errorf("written through the link: %v", err)
		}
	})

	t.Run("target replaced by a directory before the rename", func(t *testing.T) {
		l := newLayout(t)
		applyItemsTestHook = func(point string, dir *os.Root, name string) {
			if point == "rename" {
				_ = os.Mkdir(filepath.Join(l.home, "target"), 0o755)
				_ = os.WriteFile(filepath.Join(l.home, "target/inside"), []byte("keep"), 0o600)
			}
		}
		defer func() { applyItemsTestHook = nil }()
		res := run(t, l.home, stage1([]delivery.WriterFile{file("f", "target", "0600", "x")}))
		if !reflect.DeepEqual(res.lines, lines("f", "is_directory")) {
			t.Fatalf("got %+v", res.lines)
		}
		wantFile(t, filepath.Join(l.home, "target/inside"), "keep", 0o600)
		noTempFiles(t, l.home)
	})

	t.Run("target replaced by a link before the rename replaces the link, not its target", func(t *testing.T) {
		l := newLayout(t)
		applyItemsTestHook = func(point string, dir *os.Root, name string) {
			if point == "rename" {
				_ = os.Symlink(filepath.Join(l.outside, "victim"), filepath.Join(l.home, "target"))
			}
		}
		defer func() { applyItemsTestHook = nil }()
		beforeOut := snapshot(t, l.outside)
		run(t, l.home, stage1([]delivery.WriterFile{file("f", "target", "0600", "x")}))
		if got := snapshot(t, l.outside); !reflect.DeepEqual(got, beforeOut) {
			t.Errorf("outside changed: %v", got)
		}
		noTempFiles(t, l.home)
	})
}

// TestApplyItemsPermission needs a user the file system's permissions bind:
// root passes every permission check.
func TestApplyItemsPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not bind root, so the permission result cannot be produced")
	}
	l := newLayout(t)
	mustMkdir(t, filepath.Join(l.home, "locked"))
	if err := os.Chmod(filepath.Join(l.home, "locked"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(l.home, "locked"), 0o755) })
	before := snapshot(t, l.home)
	res := run(t, l.home, stage1([]delivery.WriterFile{file("a", "locked/new", "0600", "x"), file("b", "locked/sub/new", "0600", "x")}))
	if !reflect.DeepEqual(res.lines, lines("a", "permission", "b", "not_written")) {
		t.Fatalf("got %+v", res.lines)
	}
	if got := snapshot(t, l.home); !reflect.DeepEqual(got, before) {
		t.Errorf("home changed: %v", got)
	}
}

// TestApplyItemsInputRefused: the writer checks its input itself and
// refuses a bad one whole, writing nothing and printing no line.
func TestApplyItemsInputRefused(t *testing.T) {
	l := newLayout(t)
	ok := file("f", "x", "0600", "1")
	cases := []struct {
		name string
		body func() []byte
	}{
		{"not JSON", func() []byte { return []byte("{") }},
		{"unknown member", func() []byte { return []byte(`{"version":1,"stage":1,"files":[],"env":[],"extra":1}`) }},
		{"null member", func() []byte { return []byte(`{"version":1,"stage":null,"files":[],"env":[]}`) }},
		{"duplicate member", func() []byte { return []byte(`{"version":1,"stage":1,"stage":2,"files":[],"env":[]}`) }},
		{"bad stage", func() []byte {
			return marshal(t, delivery.WriterInput{Version: 1, Stage: 3, Files: []delivery.WriterFile{ok}, Env: []delivery.WriterEnv{}})
		}},
		{"dot-dot path", func() []byte {
			return marshal(t, stage1([]delivery.WriterFile{ok, file("g", "../escape", "0600", "x")}))
		}},
		{"absolute path", func() []byte { return marshal(t, stage1([]delivery.WriterFile{file("g", "/etc/passwd", "0600", "x")})) }},
		{"dot segment", func() []byte { return marshal(t, stage1([]delivery.WriterFile{file("g", "a/./b", "0600", "x")})) }},
		{"refused target authorized_keys", func() []byte {
			return marshal(t, stage1([]delivery.WriterFile{file("g", ".ssh/authorized_keys", "0600", "x")}))
		}},
		{"refused target in the env directory", func() []byte {
			return marshal(t, stage1([]delivery.WriterFile{file("g", ".config/environment.d/99.conf", "0600", "x")}))
		}},
		{"project file landing on a refused target", func() []byte {
			return marshal(t, stage2(filepath.Join(l.home, ".ssh"), projectFile("g", "authorized_keys", "x")))
		}},
		{"mode outside the allowed set", func() []byte { return marshal(t, stage1([]delivery.WriterFile{file("g", "y", "0755", "x")})) }},
		{"refused variable", func() []byte { return marshal(t, stage1(nil, env("e", "LD_PRELOAD", "/x.so"))) }},
		{"value with a control character", func() []byte { return marshal(t, stage1(nil, env("e", "OK", "a\nb"))) }},
		{"env item in stage 2", func() []byte {
			in := stage2("", ok)
			in.Env = []delivery.WriterEnv{env("e", "OK", "v")}
			return marshal(t, in)
		}},
		{"project file in stage 1", func() []byte {
			in := stage1([]delivery.WriterFile{projectFile("p", "x", "y")})
			in.ProjectDir = filepath.Join(l.home, "proj")
			return marshal(t, in)
		}},
		{"project directory outside home", func() []byte { return marshal(t, stage2(l.outside, projectFile("p", "x", "y"))) }},
		{"project directory is home", func() []byte { return marshal(t, stage2(l.home, projectFile("p", "x", "y"))) }},
		{"project directory not canonical", func() []byte {
			return marshal(t, stage2(l.home+"/a/../../outside", projectFile("p", "x", "y")))
		}},
		{"duplicate target", func() []byte { return marshal(t, stage1([]delivery.WriterFile{ok, file("g", "x", "0600", "2")})) }},
		{"nested target", func() []byte { return marshal(t, stage1([]delivery.WriterFile{ok, file("g", "x/y", "0600", "2")})) }},
		{"oversize input", func() []byte { return bytes.Repeat([]byte(" "), delivery.MaxWriterInputBytes+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshot(t, filepath.Dir(l.home))
			res := runRaw(t, l.home, tc.body())
			if res.code != delivery.WriterExitRefused || res.raw != "" {
				t.Errorf("got %d %q, want exit 2 and no output", res.code, res.raw)
			}
			if got := snapshot(t, filepath.Dir(l.home)); !reflect.DeepEqual(got, before) {
				t.Errorf("file system changed: %v", got)
			}
		})
	}
}

func marshal(t *testing.T, in delivery.WriterInput) []byte {
	t.Helper()
	b, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRunApplyItemsStartChecks: the subcommand takes no argument and never
// runs as root.
func TestRunApplyItemsStartChecks(t *testing.T) {
	t.Run("an argument is refused", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runApplyItems([]string{"secret"}, strings.NewReader(""), &out, &errb); code != delivery.WriterExitRefused || out.Len() != 0 {
			t.Errorf("got %d %q", code, out.String())
		}
		if strings.Contains(errb.String(), "secret") {
			t.Errorf("stderr echoes the argument: %q", errb.String())
		}
	})
	t.Run("root is refused", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("not running as root: the root refusal is exercised under root (the Docker gate)")
		}
		var out, errb bytes.Buffer
		in := marshal(t, stage1([]delivery.WriterFile{file("f", "x", "0600", "1")}))
		if code := runApplyItems(nil, bytes.NewReader(in), &out, &errb); code != delivery.WriterExitRefused || out.Len() != 0 {
			t.Errorf("got %d %q", code, out.String())
		}
	})
}

// TestApplyItemsHomeLink: a link at home, or at a directory above it,
// refuses the whole stage: every item symlink, nothing written.
func TestApplyItemsHomeLink(t *testing.T) {
	for _, tc := range []struct {
		name string
		home func(l layout) string
	}{
		{"home is a link", func(l layout) string {
			link := filepath.Join(filepath.Dir(l.home), "home-link")
			mustSymlink(t, l.home, link)
			return link
		}},
		{"a directory above home is a link", func(l layout) string {
			link := filepath.Join(filepath.Dir(l.home), "above-link")
			mustSymlink(t, filepath.Dir(l.home), link)
			return filepath.Join(link, "home")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLayout(t)
			home := tc.home(l)
			before := snapshot(t, filepath.Dir(l.home))
			res := run(t, home, stage1([]delivery.WriterFile{file("a", "x", "0600", "1"), file("b", "d/y", "0600", "2")}, env("e", "GH_TOKEN", "v")))
			want := lines("a", "symlink", "b", "symlink", "e", "symlink")
			if res.code != delivery.WriterExitFailed || !reflect.DeepEqual(res.lines, want) {
				t.Errorf("got %d %+v, want %+v", res.code, res.lines, want)
			}
			if got := snapshot(t, filepath.Dir(l.home)); !reflect.DeepEqual(got, before) {
				t.Errorf("file system changed: %v", got)
			}
			if got := loadUserEnvironmentD(home); got != nil {
				t.Errorf("loader read through a linked home: %q", got)
			}
		})
	}
}

// TestApplyItemsHomeFIFO: a FIFO where home should be is refused before
// it is opened, so neither the writer nor the loader blocks on it.
func TestApplyItemsHomeFIFO(t *testing.T) {
	l := newLayout(t)
	fifo := filepath.Join(filepath.Dir(l.home), "home-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		res := run(t, fifo, stage1([]delivery.WriterFile{file("a", "x", "0600", "1")}))
		if res.code != delivery.WriterExitFailed || !reflect.DeepEqual(res.lines, lines("a", "not_directory")) {
			t.Errorf("got %d %+v", res.code, res.lines)
		}
		if got := loadUserEnvironmentD(fifo); got != nil {
			t.Errorf("loader read a FIFO home: %q", got)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("blocked on a FIFO home")
	}
}

// TestApplyItemsUmask0777: the writer sets its own umask, so a parent's
// umask of 0777 neither breaks it nor changes a mode.
func TestApplyItemsUmask0777(t *testing.T) {
	l := newLayout(t)
	old := syscall.Umask(0o777)
	res := run(t, l.home, stage1([]delivery.WriterFile{file("f", "d/e/f", "0644", "x")}, env("e", "GH_TOKEN", "v")))
	syscall.Umask(old)
	if res.code != 0 || !reflect.DeepEqual(res.lines, lines("f", "ok", "e", "ok")) {
		t.Fatalf("got %d %+v %q", res.code, res.lines, res.stderr)
	}
	wantFile(t, filepath.Join(l.home, "d/e/f"), "x", 0o644)
	wantDirMode(t, filepath.Join(l.home, "d"), 0o700)
	wantDirMode(t, filepath.Join(l.home, "d/e"), 0o700)
	wantFile(t, filepath.Join(l.home, delivery.EnvFile), "GH_TOKEN=v\n", 0o600)
	wantDirMode(t, filepath.Join(l.home, ".config/environment.d"), 0o700)
}

// TestDevOf: two files in one directory are on one file system; /proc is
// on another.
func TestDevOf(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a"), "a", 0o600)
	a, err := os.Lstat(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.Lstat("/proc")
	if err != nil {
		t.Skip("no /proc")
	}
	if devOf(a) == 0 || devOf(a) != devOf(d) {
		t.Errorf("same directory: %d and %d", devOf(a), devOf(d))
	}
	if devOf(p) == devOf(d) {
		t.Errorf("/proc and a temp directory share device %d", devOf(p))
	}
}

// otherFilesystemMount finds a tmpfs mount point whose parent is a
// directory on another file system, with no link on the parent's path:
// /dev/shm under /dev, usually, both on a host and in a container. The test
// uses the parent as home, so the mount is a directory inside it.
func otherFilesystemMount(t *testing.T) (parent, mount string) {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Skip("no /proc/self/mountinfo: cannot find a mount to cross")
	}
	var candidates []string
	for _, line := range strings.Split(string(data), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		f, r := strings.Fields(left), strings.Fields(right)
		if !ok || len(f) < 5 || len(r) < 1 || r[0] != "tmpfs" {
			continue
		}
		candidates = append(candidates, f[4])
	}
	// Prefer /dev/shm.
	for i, c := range candidates {
		if c == "/dev/shm" {
			candidates[0], candidates[i] = candidates[i], candidates[0]
		}
	}
	for _, m := range candidates {
		p := filepath.Dir(m)
		if p == "/" || strings.ContainsAny(m, `\ `) || linkOnPath(m) {
			continue
		}
		pi, err1 := os.Lstat(p)
		mi, err2 := os.Lstat(m)
		if err1 != nil || err2 != nil || !pi.IsDir() || !mi.IsDir() || devOf(pi) == devOf(mi) {
			continue
		}
		return p, m
	}
	t.Skip("no tmpfs mount under a directory on another file system (and mounting one needs CAP_SYS_ADMIN): only TestDevOf covers the comparison here")
	return "", ""
}

// TestApplyItemsOtherFilesystem: a directory on the way to the target on
// another file system than home (as a --local-dir host mount is) refuses
// the item other_filesystem, and nothing is written there; likewise a
// project directory that is such a mount.
func TestApplyItemsOtherFilesystem(t *testing.T) {
	parent, mount := otherFilesystemMount(t)
	probe := fmt.Sprintf("shed-a2-probe-%d", os.Getpid())
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(mount, probe)) })

	t.Run("home item through the mount", func(t *testing.T) {
		res := run(t, parent, stage1([]delivery.WriterFile{file("f", filepath.Base(mount)+"/"+probe+"/x", "0600", "x")}))
		if !reflect.DeepEqual(res.lines, lines("f", "other_filesystem")) {
			t.Errorf("got %+v", res.lines)
		}
		if _, err := os.Lstat(filepath.Join(mount, probe)); !os.IsNotExist(err) {
			t.Errorf("written on the other file system: %v", err)
		}
	})
	t.Run("project directory is the mount", func(t *testing.T) {
		res := run(t, parent, stage2(mount, projectFile("p", probe, "x")))
		if !reflect.DeepEqual(res.lines, lines("p", "other_filesystem")) {
			t.Errorf("got %+v", res.lines)
		}
		if _, err := os.Lstat(filepath.Join(mount, probe)); !os.IsNotExist(err) {
			t.Errorf("written on the other file system: %v", err)
		}
	})
}

// TestApplyItemsEnvAtMax: an answer whose env file is exactly the contract's
// maximum is accepted, written whole and read back whole by the loader;
// one byte more is refused by the answer rules, and the writer, given it
// anyway, fails the env items io and writes no env file.
func TestApplyItemsEnvAtMax(t *testing.T) {
	build := func(size int) []delivery.Item {
		items := make([]delivery.Item, 32)
		left := size
		for i := range items {
			v := fmt.Sprintf("V%02d", i)
			line := left / (32 - i)
			items[i] = delivery.Item{Name: fmt.Sprintf("e%02d", i), Provider: "p", Kind: delivery.KindEnv, Variable: v, Value: strings.Repeat("v", line-len(v)-2), Fingerprint: "f"}
			left -= line
		}
		return items
	}
	req := &delivery.Request{ID: "0123456789abcdef0123456789abcdef"}

	t.Run("exactly the maximum", func(t *testing.T) {
		l := newLayout(t)
		items := build(delivery.MaxEnvFileBytes)
		body, err := json.Marshal(delivery.Answer{Version: 1, ID: req.ID, Items: items})
		if err != nil {
			t.Fatal(err)
		}
		a, r := delivery.ParseAnswer(body, req)
		if r != nil {
			t.Fatal(r)
		}
		res := run(t, l.home, delivery.NewWriterInput(1, "", a.Items))
		if res.code != 0 || len(res.lines) != 32 {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		fi, err := os.Stat(filepath.Join(l.home, delivery.EnvFile))
		if err != nil || fi.Size() != delivery.MaxEnvFileBytes {
			t.Fatalf("env file: %v %v", fi, err)
		}
		got := loadUserEnvironmentD(l.home)
		if len(got) != 32 {
			t.Fatalf("loader read %d of 32", len(got))
		}
		for i, it := range items {
			if got[i] != it.Variable+"="+it.Value {
				t.Errorf("value %d not read back whole", i)
			}
		}
	})

	t.Run("one byte more", func(t *testing.T) {
		l := newLayout(t)
		items := build(delivery.MaxEnvFileBytes + 1)
		body, err := json.Marshal(delivery.Answer{Version: 1, ID: req.ID, Items: items})
		if err != nil {
			t.Fatal(err)
		}
		if _, r := delivery.ParseAnswer(body, req); r == nil || r.Rule != delivery.RuleEnvTooLarge {
			t.Fatalf("answer: %v, want env_too_large", r)
		}
		in := delivery.NewWriterInput(1, "", items)
		in.Files = []delivery.WriterFile{file("f", "x", "0600", "1")}
		res := run(t, l.home, in)
		if res.code != delivery.WriterExitFailed || res.lines[0].Result != delivery.ResultOK {
			t.Fatalf("got %d %+v", res.code, res.lines)
		}
		for _, ln := range res.lines[1:] {
			if ln.Result != delivery.ResultIO {
				t.Fatalf("env item %s: %s, want io", ln.Name, ln.Result)
			}
		}
		if _, err := os.Lstat(filepath.Join(l.home, ".config")); !os.IsNotExist(err) {
			t.Errorf("env directory created: %v", err)
		}
	})
}

// TestParseAgentFlags: the daemon takes flags only; an unknown subcommand
// or a word after the flags is refused, without echoing it.
func TestParseAgentFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		ok   bool
	}{
		{"no arguments (the VZ and Firecracker units)", nil, true},
		{"the cloud unit's flags", []string{"--listen", "unix:/run/shed-agent", "--socket-group", "shed-link", "--shed-name", "s1"}, true},
		{"an unknown subcommand", []string{"apply-item"}, false},
		{"an unknown subcommand with flags", []string{"versions", "--layout-file", "/x"}, false},
		{"a word after the flags", []string{"--shed-name", "s1", "secret-word"}, false},
		{"an unknown flag", []string{"--nope"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errb bytes.Buffer
			o, err := parseAgentFlags(tc.args, &errb)
			if (err == nil) != tc.ok {
				t.Fatalf("err %v, want ok %v", err, tc.ok)
			}
			if tc.ok && o == nil {
				t.Fatal("no flags")
			}
			if strings.Contains(errb.String(), "secret-word") || strings.Contains(errb.String(), "apply-item\n") {
				t.Errorf("stderr echoes the argument: %q", errb.String())
			}
		})
	}
	o, _ := parseAgentFlags([]string{"--shed-name", "s1", "--listen", "unix:/x"}, io.Discard)
	if o.shedName != "s1" || o.listenSpec != "unix:/x" || o.consolePort != DefaultConsolePort {
		t.Errorf("parsed %+v", o)
	}
}
