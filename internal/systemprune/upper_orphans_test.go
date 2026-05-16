package systemprune

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCollectUpperOrphanCandidates walks the matrix of states an
// upper directory can be in and verifies which ones land in
// candidates vs skipped.
func TestCollectUpperOrphanCandidates(t *testing.T) {
	uppersDir := t.TempDir()
	instanceDir := t.TempDir()

	// Case A: live shed (metadata.json present).
	makeUpper(t, uppersDir, "live-shed")
	makeMetadata(t, instanceDir, "live-shed")

	// Case B: in-flight create (.creating marker, fresh).
	makeUpper(t, uppersDir, "in-flight")
	makeMarker(t, instanceDir, "in-flight")

	// Case C: stale in-flight create (.creating marker, past 1h).
	makeUpper(t, uppersDir, "crashed-long-ago")
	makeMarker(t, instanceDir, "crashed-long-ago")
	past := time.Now().Add(-2 * InstanceCreatingMaxAge)
	if err := os.Chtimes(filepath.Join(instanceDir, "crashed-long-ago", InstanceCreatingMarker), past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Case D: true orphan (no metadata, no marker).
	makeUpper(t, uppersDir, "ghost")

	candidates, skipped := CollectUpperOrphanCandidates(uppersDir, instanceDir)

	gotCands := map[string]bool{}
	for _, c := range candidates {
		gotCands[filepath.Base(c.Dir)] = true
	}
	gotSkipped := map[string]string{}
	for _, s := range skipped {
		gotSkipped[filepath.Base(s.Path)] = s.Reason
	}

	if !gotCands["ghost"] {
		t.Errorf("expected `ghost` to be a candidate; got %v", gotCands)
	}
	if !gotCands["crashed-long-ago"] {
		t.Errorf("expected stale `crashed-long-ago` to be a candidate; got %v", gotCands)
	}
	if gotCands["live-shed"] {
		t.Errorf("live shed must NOT be a candidate: %v", gotCands)
	}
	if gotCands["in-flight"] {
		t.Errorf("in-flight create must NOT be a candidate: %v", gotCands)
	}
	if reason, ok := gotSkipped["in-flight"]; !ok {
		t.Errorf("expected in-flight to appear in skipped; got %#v", gotSkipped)
	} else if reason == "" {
		t.Errorf("in-flight skip reason is empty")
	}
}

func TestFindUpperOrphans(t *testing.T) {
	uppersDir := t.TempDir()
	instanceDir := t.TempDir()

	// One live, one orphan; df should see only the orphan.
	makeUpper(t, uppersDir, "live")
	makeMetadata(t, instanceDir, "live")
	makeUpper(t, uppersDir, "ghost")

	orphans, err := FindUpperOrphans(uppersDir, instanceDir)
	if err != nil {
		t.Fatalf("FindUpperOrphans: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan file entry, got %d: %#v", len(orphans), orphans)
	}
	if filepath.Dir(orphans[0].Path) != filepath.Join(uppersDir, "ghost") {
		t.Errorf("orphan path = %q, want under uppers/ghost", orphans[0].Path)
	}
}

func TestSweepUpperOrphan(t *testing.T) {
	uppersDir := t.TempDir()
	instanceDir := t.TempDir()
	makeUpper(t, uppersDir, "ghost")

	candidates, _ := CollectUpperOrphanCandidates(uppersDir, instanceDir)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	if !SweepUpperOrphan(candidates[0]) {
		t.Fatalf("SweepUpperOrphan returned false")
	}
	if _, err := os.Stat(candidates[0].Dir); !os.IsNotExist(err) {
		t.Errorf("upper dir still present after sweep: err=%v", err)
	}
}

// makeUpper creates uppersDir/<name>/upper.ext4 with a few bytes
// of content so the walker has something to enumerate.
func makeUpper(t *testing.T, uppersDir, name string) {
	t.Helper()
	dir := filepath.Join(uppersDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "upper.ext4"), []byte("dummy"), 0o644); err != nil {
		t.Fatalf("write upper for %s: %v", name, err)
	}
}

func makeMetadata(t *testing.T, instanceDir, name string) {
	t.Helper()
	dir := filepath.Join(instanceDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, instanceMetadataFilename), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write metadata.json for %s: %v", name, err)
	}
}

func makeMarker(t *testing.T, instanceDir, name string) {
	t.Helper()
	dir := filepath.Join(instanceDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, InstanceCreatingMarker), []byte("sha256:abc"), 0o600); err != nil {
		t.Fatalf("write marker for %s: %v", name, err)
	}
}
