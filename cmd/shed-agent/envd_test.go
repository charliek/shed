//go:build linux
// +build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadEnvironmentD(t *testing.T) {
	dir := t.TempDir()

	// Write two conf files — 10 should be read before 20
	if err := os.WriteFile(filepath.Join(dir, "10-base.conf"), []byte(
		"FOO=bar\nBAZ=qux\n",
	), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20-extensions.conf"), []byte(
		"SSH_AUTH_SOCK=/run/shed-extensions/ssh-agent.sock\nAWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:499/credentials\n",
	), 0644); err != nil {
		t.Fatal(err)
	}

	env := loadEnvironmentD(dir)

	expected := []string{
		"FOO=bar",
		"BAZ=qux",
		"SSH_AUTH_SOCK=/run/shed-extensions/ssh-agent.sock",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:499/credentials",
	}

	if len(env) != len(expected) {
		t.Fatalf("got %d vars, want %d: %v", len(env), len(expected), env)
	}
	for i, want := range expected {
		if env[i] != want {
			t.Errorf("env[%d] = %q, want %q", i, env[i], want)
		}
	}
}

func TestLoadEnvironmentDCommentsAndBlankLines(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "test.conf"), []byte(
		"# This is a comment\n\nVALID=yes\n  # indented comment\n  \nALSO_VALID=true\n",
	), 0644); err != nil {
		t.Fatal(err)
	}

	env := loadEnvironmentD(dir)

	if len(env) != 2 {
		t.Fatalf("got %d vars, want 2: %v", len(env), env)
	}
	if env[0] != "VALID=yes" {
		t.Errorf("env[0] = %q, want %q", env[0], "VALID=yes")
	}
	if env[1] != "ALSO_VALID=true" {
		t.Errorf("env[1] = %q, want %q", env[1], "ALSO_VALID=true")
	}
}

func TestLoadEnvironmentDMissingDir(t *testing.T) {
	env := loadEnvironmentD("/nonexistent/path")
	if len(env) != 0 {
		t.Fatalf("expected empty slice for missing dir, got %v", env)
	}
}

func TestLoadEnvironmentDEmptyDir(t *testing.T) {
	dir := t.TempDir()
	env := loadEnvironmentD(dir)
	if len(env) != 0 {
		t.Fatalf("expected empty slice for empty dir, got %v", env)
	}
}

func TestLoadEnvironmentDFileOrder(t *testing.T) {
	dir := t.TempDir()

	// Write files in reverse alphabetical order to verify sorting
	if err := os.WriteFile(filepath.Join(dir, "z-last.conf"), []byte("ORDER=last\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a-first.conf"), []byte("ORDER=first\n"), 0644); err != nil {
		t.Fatal(err)
	}

	env := loadEnvironmentD(dir)

	if len(env) != 2 {
		t.Fatalf("got %d vars, want 2: %v", len(env), env)
	}
	// a-first.conf should be read first
	if env[0] != "ORDER=first" {
		t.Errorf("env[0] = %q, want %q", env[0], "ORDER=first")
	}
	if env[1] != "ORDER=last" {
		t.Errorf("env[1] = %q, want %q", env[1], "ORDER=last")
	}
}

// userEnvDir makes home/.config/environment.d and returns it.
func userEnvDir(t *testing.T, home string) string {
	t.Helper()
	d := filepath.Join(home, ".config", "environment.d")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// loadUserWithin runs loadUserEnvironmentD, failing if it blocks.
func loadUserWithin(t *testing.T, home string) []string {
	t.Helper()
	done := make(chan []string, 1)
	go func() { done <- loadUserEnvironmentD(home) }()
	select {
	case env := <-done:
		return env
	case <-time.After(10 * time.Second):
		t.Fatal("loadUserEnvironmentD blocked")
		return nil
	}
}

func TestLoadUserEnvironmentD(t *testing.T) {
	exact := `a "double" 'single' $HOME ${X} inner  spaces x==y=z`
	cases := []struct {
		name  string
		setup func(t *testing.T, home, dir, outside string)
		want  []string
	}{
		{
			name: "files in lexical order, comments and blanks ignored",
			setup: func(t *testing.T, home, dir, outside string) {
				write(t, filepath.Join(dir, "50-shed-delivery.conf"), "B=2\n")
				write(t, filepath.Join(dir, "10-mine.conf"), "# c\n\nA=1\n")
				write(t, filepath.Join(dir, "notes.txt"), "C=3\n")
			},
			want: []string{"A=1", "B=2"},
		},
		{
			name: "value byte for byte after the first equals sign",
			setup: func(t *testing.T, home, dir, outside string) {
				write(t, filepath.Join(dir, "a.conf"), "  TOKEN="+exact+"  \r\n")
			},
			want: []string{"TOKEN=" + exact},
		},
		{
			name: "refused variables skipped in any case",
			setup: func(t *testing.T, home, dir, outside string) {
				write(t, filepath.Join(dir, "a.conf"), strings.Join([]string{
					"PATH=/evil", "path=/evil", "Home=/x", "user=x", "LOGNAME=x", "shell=/x", "IFS=x", "env=x",
					"Bash_Env=x", "http_proxy=x", "HTTPS_PROXY=x", "no_proxy=x", "All_Proxy=x",
					"LD_PRELOAD=/x.so", "ld_library_path=/x", "SHED_TOKEN=x", "shed_x=x", "PATH =x",
					"KEPT=yes", "LDX=allowed", "SHEDX=allowed",
				}, "\n")+"\n")
			},
			want: []string{"KEPT=yes", "LDX=allowed", "SHEDX=allowed"},
		},
		{
			name: "FIFO in the directory does not block and is skipped",
			setup: func(t *testing.T, home, dir, outside string) {
				if err := syscall.Mkfifo(filepath.Join(dir, "00-pipe.conf"), 0o644); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(dir, "10-ok.conf"), "OK=1\n")
			},
			want: []string{"OK=1"},
		},
		{
			name: "symlinked .conf skipped, pointing outside or inside",
			setup: func(t *testing.T, home, dir, outside string) {
				write(t, filepath.Join(outside, "evil"), "EVIL=1\n")
				write(t, filepath.Join(dir, "real.txt"), "INSIDE=1\n")
				link(t, filepath.Join(outside, "evil"), filepath.Join(dir, "00-out.conf"))
				link(t, "real.txt", filepath.Join(dir, "01-in.conf"))
				write(t, filepath.Join(dir, "10-ok.conf"), "OK=1\n")
			},
			want: []string{"OK=1"},
		},
		{
			name: "directory named .conf skipped",
			setup: func(t *testing.T, home, dir, outside string) {
				if err := os.Mkdir(filepath.Join(dir, "d.conf"), 0o700); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(dir, "e.conf"), "OK=1\n")
			},
			want: []string{"OK=1"},
		},
		{
			name: "symlinked environment.d skipped",
			setup: func(t *testing.T, home, dir, outside string) {
				write(t, filepath.Join(outside, "envd", "a.conf"), "EVIL=1\n")
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				link(t, filepath.Join(outside, "envd"), dir)
			},
			want: nil,
		},
		{
			name: "symlinked .config skipped",
			setup: func(t *testing.T, home, dir, outside string) {
				write(t, filepath.Join(outside, "cfg", "environment.d", "a.conf"), "EVIL=1\n")
				if err := os.RemoveAll(filepath.Join(home, ".config")); err != nil {
					t.Fatal(err)
				}
				link(t, filepath.Join(outside, "cfg"), filepath.Join(home, ".config"))
			},
			want: nil,
		},
		{
			name: "missing directory is nothing",
			setup: func(t *testing.T, home, dir, outside string) {
				if err := os.RemoveAll(filepath.Join(home, ".config")); err != nil {
					t.Fatal(err)
				}
			},
			want: nil,
		},
		{
			name: "20 KiB line skipped and later lines still load",
			setup: func(t *testing.T, home, dir, outside string) {
				write(t, filepath.Join(dir, "a.conf"), "BEFORE=1\nLONG="+strings.Repeat("x", 20<<10)+"\nAFTER=2\n")
			},
			want: []string{"BEFORE=1", "AFTER=2"},
		},
		{
			name: "a line of exactly 16 KiB is taken",
			setup: func(t *testing.T, home, dir, outside string) {
				write(t, filepath.Join(dir, "a.conf"), "EDGE="+strings.Repeat("x", (16<<10)-len("EDGE="))+"\n")
			},
			want: []string{"EDGE=" + strings.Repeat("x", (16<<10)-len("EDGE="))},
		},
		{
			name: "300 KiB file cut at 256 KiB, the line the cut falls in dropped",
			setup: func(t *testing.T, home, dir, outside string) {
				var b strings.Builder
				b.WriteString("FIRST=1\n")
				filler := "# " + strings.Repeat("f", 1000) + "\n"
				for b.Len()+len(filler) <= (256<<10)-50 {
					b.WriteString(filler)
				}
				// Pad to 50 bytes short of the cut, so the next line crosses it.
				b.WriteString("#" + strings.Repeat("p", (256<<10)-50-b.Len()-2) + "\n")
				b.WriteString("STRADDLE=" + strings.Repeat("s", 100) + "\n")
				for b.Len() < 300<<10 {
					b.WriteString(filler)
				}
				b.WriteString("BEYOND=1\n")
				write(t, filepath.Join(dir, "a.conf"), b.String())
				write(t, filepath.Join(dir, "b.conf"), "NEXT=1\n")
			},
			want: []string{"FIRST=1", "NEXT=1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			home, outside := filepath.Join(base, "home"), filepath.Join(base, "outside")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			dir := userEnvDir(t, home)
			tc.setup(t, home, dir, outside)
			got := loadUserWithin(t, home)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestSystemEnvironmentDLongLine: an over-long line in /etc/environment.d
// is skipped; it used to end the file's parsing silently.
func TestSystemEnvironmentDLongLine(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.conf"), "BEFORE=1\nLONG="+strings.Repeat("x", 70<<10)+"\nAFTER=2\n")
	got := loadEnvironmentD(dir)
	if want := []string{"BEFORE=1", "AFTER=2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestEnvironmentDEntriesUserWins: in a started command, a user value beats
// the system default for the same key, and a refused variable in the user
// directory does not reach it.
func TestEnvironmentDEntriesUserWins(t *testing.T) {
	sys := t.TempDir()
	home := t.TempDir()
	write(t, filepath.Join(sys, "10-shed.conf"), "SHARED=system\nSYSONLY=s\n")
	write(t, filepath.Join(userEnvDir(t, home), "50-shed-delivery.conf"), "SHARED=user value\nPATH=/evil\nLD_PRELOAD=/evil.so\n")

	env := environmentDEntries(sys, &userInfo{homeDir: home, name: "shed"})
	cmd := exec.Command("/bin/sh", "-c", `printf '%s|%s|%s|%s' "$SHARED" "$SYSONLY" "$PATH" "${LD_PRELOAD-unset}"`)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "user value|s|/usr/bin:/bin|unset"; string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}

	if got := environmentDEntries(sys, nil); !reflect.DeepEqual(got, []string{"SHARED=system", "SYSONLY=s"}) {
		t.Errorf("no user: got %q", got)
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, target, p string) {
	t.Helper()
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

// TestLoadUserEnvironmentDBounds: each invocation is bounded whatever the
// directory holds; past a cap the rest is skipped and loading goes on.
func TestLoadUserEnvironmentDBounds(t *testing.T) {
	newHome := func(t *testing.T) (string, string) {
		home := t.TempDir()
		return home, userEnvDir(t, home)
	}

	t.Run("at most 64 .conf files, the first in lexical order", func(t *testing.T) {
		home, dir := newHome(t)
		for i := 69; i >= 0; i-- {
			write(t, filepath.Join(dir, fmt.Sprintf("%03d.conf", i)), fmt.Sprintf("V%03d=1\n", i))
		}
		got := loadUserWithin(t, home)
		if len(got) != 64 || got[0] != "V000=1" || got[63] != "V063=1" {
			t.Errorf("got %d: first %q last %q", len(got), got[0], got[len(got)-1])
		}
	})

	t.Run("many entries are looked at in bounded chunks", func(t *testing.T) {
		home, dir := newHome(t)
		for i := 0; i < userEnvMaxEntries+500; i++ {
			write(t, filepath.Join(dir, fmt.Sprintf("n%05d.txt", i)), "")
		}
		if got := loadUserWithin(t, home); got != nil {
			t.Errorf("got %q", got)
		}
	})

	t.Run("at most 1 MiB read across files", func(t *testing.T) {
		home, dir := newHome(t)
		filler := "# " + strings.Repeat("f", 1021) + "\n" // 1 KiB
		for i := 0; i < 5; i++ {
			var b strings.Builder
			fmt.Fprintf(&b, "FIRST%d=1\n", i)
			for b.Len() < 250<<10 {
				b.WriteString(filler)
			}
			fmt.Fprintf(&b, "LAST%d=1\n", i)
			write(t, filepath.Join(dir, fmt.Sprintf("%d.conf", i)), b.String())
		}
		got := loadUserWithin(t, home)
		want := []string{"FIRST0=1", "LAST0=1", "FIRST1=1", "LAST1=1", "FIRST2=1", "LAST2=1", "FIRST3=1", "LAST3=1", "FIRST4=1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %q\nwant %q", got, want)
		}
	})

	t.Run("at most 512 KiB of environment", func(t *testing.T) {
		home, dir := newHome(t)
		for f := 0; f < 3; f++ {
			var b strings.Builder
			for i := 0; i < 20; i++ {
				fmt.Fprintf(&b, "F%dV%02d=%s\n", f, i, strings.Repeat("v", 10<<10))
			}
			write(t, filepath.Join(dir, fmt.Sprintf("%d.conf", f)), b.String())
		}
		got := loadUserWithin(t, home)
		size := 0
		for _, e := range got {
			size += len(e) + 1
		}
		if size > userEnvMaxEnv || size < userEnvMaxEnv-(11<<10) {
			t.Errorf("environment of %d bytes, want just under %d", size, userEnvMaxEnv)
		}
		if got[0] != "F0V00="+strings.Repeat("v", 10<<10) || len(got) < 40 {
			t.Errorf("got %d entries", len(got))
		}
	})
}
