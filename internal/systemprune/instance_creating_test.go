package systemprune

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanInstanceCreatingMarkers(t *testing.T) {
	dir := t.TempDir()
	freshDigest := "sha256:" + strings.Repeat("a", 64)
	staleDigest := "sha256:" + strings.Repeat("b", 64)

	// Fresh marker: should be protective.
	writeMarker(t, dir, "in-flight", freshDigest)

	// Stale marker (past InstanceCreatingMaxAge): should NOT protect.
	staleDir := writeMarker(t, dir, "crashed-long-ago", staleDigest)
	past := time.Now().Add(-2 * InstanceCreatingMaxAge)
	if err := os.Chtimes(filepath.Join(staleDir, InstanceCreatingMarker), past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Malformed marker (no sha256: prefix): should NOT protect.
	mDir := filepath.Join(dir, "malformed")
	if err := os.MkdirAll(mDir, 0o755); err != nil {
		t.Fatalf("mkdir malformed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mDir, InstanceCreatingMarker), []byte("not-a-digest"), 0o600); err != nil {
		t.Fatalf("write malformed: %v", err)
	}

	// Completed shed (no marker, just metadata.json): not in scan output.
	completedDir := filepath.Join(dir, "completed")
	if err := os.MkdirAll(completedDir, 0o755); err != nil {
		t.Fatalf("mkdir completed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(completedDir, "metadata.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	refs, err := ScanInstanceCreatingMarkers(dir)
	if err != nil {
		t.Fatalf("ScanInstanceCreatingMarkers: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected exactly 1 protective ref (the fresh marker), got %d: %#v", len(refs), refs)
	}
	if refs[0].ShedName != "in-flight" {
		t.Errorf("ref.ShedName = %q, want %q", refs[0].ShedName, "in-flight")
	}
	if refs[0].LowerDigest != freshDigest {
		t.Errorf("ref.LowerDigest = %q, want %q", refs[0].LowerDigest, freshDigest)
	}
}

func TestScanInstanceCreatingMarkers_MissingDir(t *testing.T) {
	refs, err := ScanInstanceCreatingMarkers(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("ScanInstanceCreatingMarkers on missing dir returned error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected no refs on missing dir; got %#v", refs)
	}
}

// writeMarker creates instanceDir/name/.creating containing digest.
// Returns the instance dir path for further mtime manipulation.
func writeMarker(t *testing.T, instanceDir, name, digest string) string {
	t.Helper()
	dir := filepath.Join(instanceDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, InstanceCreatingMarker), []byte(digest), 0o600); err != nil {
		t.Fatalf("write marker for %s: %v", name, err)
	}
	return dir
}
