package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// egressTestRoot is a temp dir with its symlinks resolved, so paths built
// under it compare equal to EvalSymlinks output (macOS's /var is a symlink).
func egressTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// writeExec creates an executable file (and its parent dirs) at path.
func writeExec(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// symlink creates link -> target (and link's parent dirs).
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// brewKeg lays out the formula's macOS install under root, as brew does:
// Cellar/shed/1.2.3/libexec/ShedServer.app/Contents/MacOS/shed-server, a
// relative bin/shed-server symlink into it, bin/shed-egress-proxy, and
// opt/shed -> ../Cellar/shed/1.2.3. Returns the keg (Cellar) path.
func brewKeg(t *testing.T, root string) string {
	t.Helper()
	keg := filepath.Join(root, "Cellar", "shed", "1.2.3")
	writeExec(t, filepath.Join(keg, bundleExeSuffix))
	symlink(t, filepath.Join("..", bundleExeSuffix), filepath.Join(keg, "bin", "shed-server"))
	writeExec(t, filepath.Join(keg, "bin", egressProxyBinName))
	symlink(t, filepath.Join("..", "Cellar", "shed", "1.2.3"), filepath.Join(root, "opt", "shed"))
	return keg
}

func TestEgressProxyCandidateDirs(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string) (exe string, want []string)
	}{
		{
			name: "plain binary: one directory",
			setup: func(t *testing.T, root string) (string, []string) {
				exe := writeExec(t, filepath.Join(root, "usr", "local", "bin", "shed-server"))
				return exe, []string{filepath.Dir(exe)}
			},
		},
		{
			name: "brew service through opt/bin: the symlink's dir, the bundle's, then the keg's bin",
			setup: func(t *testing.T, root string) (string, []string) {
				keg := brewKeg(t, root)
				exe := filepath.Join(root, "opt", "shed", "bin", "shed-server")
				return exe, []string{
					filepath.Join(root, "opt", "shed", "bin"),
					filepath.Dir(filepath.Join(keg, bundleExeSuffix)),
					filepath.Join(keg, "bin"),
				}
			},
		},
		{
			name: "no executable: no directories",
			setup: func(t *testing.T, root string) (string, []string) {
				return "", nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exe, want := tt.setup(t, egressTestRoot(t))
			if got := egressProxyCandidateDirs(exe); !reflect.DeepEqual(got, want) {
				t.Errorf("egressProxyCandidateDirs(%q) = %q, want %q", exe, got, want)
			}
		})
	}
}

func TestResolveEgressProxyBinFor(t *testing.T) {
	tests := []struct {
		name string
		// setup builds the layout and returns the executable path, the proxy
		// path the resolver must pick, and the PATH to run with.
		setup func(t *testing.T, root string) (exe, want, path string)
	}{
		{
			name: "deb: the proxy beside a plain binary",
			setup: func(t *testing.T, root string) (string, string, string) {
				exe := writeExec(t, filepath.Join(root, "usr", "local", "bin", "shed-server"))
				want := writeExec(t, filepath.Join(root, "usr", "local", "bin", egressProxyBinName))
				return exe, want, filepath.Join(root, "empty")
			},
		},
		{
			name: "brew service launched through opt/bin into the bundle",
			setup: func(t *testing.T, root string) (string, string, string) {
				brewKeg(t, root)
				exe := filepath.Join(root, "opt", "shed", "bin", "shed-server")
				return exe, filepath.Join(root, "opt", "shed", "bin", egressProxyBinName), filepath.Join(root, "empty")
			},
		},
		{
			name: "bundle executable launched directly: the keg's bin",
			setup: func(t *testing.T, root string) (string, string, string) {
				keg := brewKeg(t, root)
				return filepath.Join(keg, bundleExeSuffix), filepath.Join(keg, "bin", egressProxyBinName), filepath.Join(root, "empty")
			},
		},
		{
			name: "a symlink elsewhere into the bundle: the keg's bin",
			setup: func(t *testing.T, root string) (string, string, string) {
				keg := brewKeg(t, root)
				exe := filepath.Join(root, "elsewhere", "shed-server")
				symlink(t, filepath.Join(keg, bundleExeSuffix), exe)
				return exe, filepath.Join(keg, "bin", egressProxyBinName), filepath.Join(root, "empty")
			},
		},
		{
			name: "a symlinked plain binary: the resolved directory",
			setup: func(t *testing.T, root string) (string, string, string) {
				target := writeExec(t, filepath.Join(root, "opt", "shed-dist", "shed-server"))
				want := writeExec(t, filepath.Join(root, "opt", "shed-dist", egressProxyBinName))
				exe := filepath.Join(root, "usr", "local", "bin", "shed-server")
				symlink(t, target, exe)
				return exe, want, filepath.Join(root, "empty")
			},
		},
		{
			name: "a stale proxy earlier on PATH loses to the installation's",
			setup: func(t *testing.T, root string) (string, string, string) {
				brewKeg(t, root)
				stale := filepath.Join(root, "usr", "local", "bin")
				writeExec(t, filepath.Join(stale, egressProxyBinName))
				exe := filepath.Join(root, "opt", "shed", "bin", "shed-server")
				return exe, filepath.Join(root, "opt", "shed", "bin", egressProxyBinName), stale
			},
		},
		{
			name: "a stale proxy on PATH loses to the keg's bin for a direct bundle launch",
			setup: func(t *testing.T, root string) (string, string, string) {
				keg := brewKeg(t, root)
				stale := filepath.Join(root, "usr", "local", "bin")
				writeExec(t, filepath.Join(stale, egressProxyBinName))
				return filepath.Join(keg, bundleExeSuffix), filepath.Join(keg, "bin", egressProxyBinName), stale
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := egressTestRoot(t)
			exe, want, path := tt.setup(t, root)
			t.Setenv("PATH", path)
			var logs bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(prev) })

			got, err := resolveEgressProxyBinFor(exe)
			if err != nil {
				t.Fatalf("resolveEgressProxyBinFor(%q): %v", exe, err)
			}
			if got != want {
				t.Errorf("resolveEgressProxyBinFor(%q) = %q, want %q", exe, got, want)
			}
			if strings.Contains(logs.String(), "PATH") {
				t.Errorf("an installation hit logged a PATH warning: %q", logs.String())
			}
		})
	}
}

func TestResolveEgressProxyBinFor_PathFallback(t *testing.T) {
	tests := []struct {
		name string
		exe  func(t *testing.T, root string) string
	}{
		{
			name: "the installation has no proxy",
			exe: func(t *testing.T, root string) string {
				return writeExec(t, filepath.Join(root, "bare", "shed-server"))
			},
		},
		{
			name: "the executable is unknown",
			exe:  func(t *testing.T, root string) string { return "" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := egressTestRoot(t)
			exe := tt.exe(t, root)
			onPath := writeExec(t, filepath.Join(root, "path", egressProxyBinName))
			t.Setenv("PATH", filepath.Dir(onPath))
			var logs bytes.Buffer
			prev := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(prev) })

			got, err := resolveEgressProxyBinFor(exe)
			if err != nil {
				t.Fatalf("resolveEgressProxyBinFor(%q): %v", exe, err)
			}
			if got != onPath {
				t.Errorf("got %q, want the PATH copy %q", got, onPath)
			}
			if !strings.Contains(logs.String(), "WARNING") || !strings.Contains(logs.String(), "from PATH") {
				t.Errorf("a PATH fallback must log a warning naming PATH, got %q", logs.String())
			}
		})
	}
}

func TestResolveEgressProxyBinFor_NotFound(t *testing.T) {
	root := egressTestRoot(t)
	exe := writeExec(t, filepath.Join(root, "bare", "shed-server"))
	t.Setenv("PATH", filepath.Join(root, "empty"))
	_, err := resolveEgressProxyBinFor(exe)
	if err == nil {
		t.Fatal("expected an error when the proxy is in neither the installation nor PATH")
	}
	if !strings.Contains(err.Error(), filepath.Dir(exe)) {
		t.Errorf("the error should name the searched directory %q: %v", filepath.Dir(exe), err)
	}
}
