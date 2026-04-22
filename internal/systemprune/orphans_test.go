package systemprune

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifySidecar(t *testing.T) {
	tests := []struct {
		in       string
		wantBase string
		wantKind string
	}{
		{"default-rootfs.ext4.lock", "default-rootfs.ext4", "lock"},
		{"default-rootfs.ext4.source", "default-rootfs.ext4", "source"},
		{"default-rootfs.ext4.tmp", "default-rootfs.ext4", "tmp"},
		{"default-rootfs.ext4.tmp.12345", "default-rootfs.ext4", "tmp"},
		{"default-rootfs.ext4", "", ""},       // the rootfs itself
		{"random.txt", "", ""},                // unrelated
		{"default-rootfs.ext4.weird", "", ""}, // unknown sidecar suffix
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			base, kind := ClassifySidecar(tt.in)
			if base != tt.wantBase || kind != tt.wantKind {
				t.Errorf("ClassifySidecar(%q) = (%q, %q), want (%q, %q)",
					tt.in, base, kind, tt.wantBase, tt.wantKind)
			}
		})
	}
}

func TestFindOrphans(t *testing.T) {
	dir := t.TempDir()

	// Live: rootfs present → lock/source/tmp are NOT orphans.
	live := "live-rootfs.ext4"
	if err := os.WriteFile(filepath.Join(dir, live), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, live+".lock"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, live+".source"), []byte("ref"), 0644); err != nil {
		t.Fatal(err)
	}

	// Dead: no rootfs → lock/tmp/source ARE orphans.
	dead := "dead-rootfs.ext4"
	if err := os.WriteFile(filepath.Join(dir, dead+".lock"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dead+".tmp"), []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dead+".tmp.99999"), []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dead+".source"), []byte("ref"), 0644); err != nil {
		t.Fatal(err)
	}

	// Noise: files that aren't sidecars must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := FindOrphans(dir)
	if err != nil {
		t.Fatalf("FindOrphans: %v", err)
	}
	if len(got) != 4 {
		names := make([]string, len(got))
		for i, f := range got {
			names[i] = filepath.Base(f.Path)
		}
		t.Fatalf("got %d orphans %v, want 4 (dead .lock, .tmp, .tmp.99999, .source)", len(got), names)
	}
	// Classification spot-check.
	for _, f := range got {
		if filepath.Base(f.Path) == dead+".lock" && f.Kind != "lock" {
			t.Errorf("dead.lock kind = %q, want lock", f.Kind)
		}
	}
}
