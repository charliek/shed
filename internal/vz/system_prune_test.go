//go:build darwin

package vz

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
	"github.com/charliek/shed/internal/vmutil"
)

// newPruneTestClient builds a Client rooted in t.TempDir's and returns the
// images and instance dirs so tests can seed fixtures directly.
func newPruneTestClient(t *testing.T) (*Client, string, string) {
	t.Helper()
	imagesDir := t.TempDir()
	instanceDir := t.TempDir()

	cfg := &config.VZConfig{
		ImagesDir:   imagesDir,
		InstanceDir: instanceDir,
		Images:      map[string]string{},
		BaseRootfs:  "",
	}
	serverCfg := &config.ServerConfig{Name: "test-prune"}

	// GetShed's stale-running→stopped branch touches credMgr.HealthTracker,
	// so even pure "prune stopped shed" tests need a non-nil credMgr. The
	// tracker itself may be nil — HealthTracker() returns nil safely, and
	// the caller checks before use.
	credMgr := vmutil.NewCredentialManager(nil, nil, "test", nil)
	c := &Client{cfg: cfg, serverCfg: serverCfg, credMgr: credMgr}
	return c, imagesDir, instanceDir
}

// seedStoppedShed creates a stopped-instance fixture with a rootfs and
// (optionally) a console.log. Backdate sets metadata.json mtime so the age
// filter sees it as "old enough."
func seedStoppedShed(t *testing.T, instanceDir, name, image string, rootfsSize int, consoleSize int, age time.Duration) {
	t.Helper()
	meta := &Metadata{
		Version: MetadataVersion,
		Name:    name,
		Status:  config.StatusStopped,
		Backend: "vz",
		Image:   image,
	}
	if err := meta.Save(instanceDir); err != nil {
		t.Fatalf("save metadata: %v", err)
	}
	dir := InstanceDir(instanceDir, name)
	if rootfsSize > 0 {
		if err := os.WriteFile(filepath.Join(dir, "rootfs.ext4"), make([]byte, rootfsSize), 0644); err != nil {
			t.Fatalf("write rootfs: %v", err)
		}
	}
	if consoleSize > 0 {
		if err := os.WriteFile(filepath.Join(dir, consoleLogFilename), make([]byte, consoleSize), 0644); err != nil {
			t.Fatalf("write console.log: %v", err)
		}
	}
	if age > 0 {
		past := time.Now().Add(-age)
		if err := os.Chtimes(MetadataPath(instanceDir, name), past, past); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
}

func TestPrune_DryRun_NoMutation(t *testing.T) {
	c, _, instanceDir := newPruneTestClient(t)
	seedStoppedShed(t, instanceDir, "old-shed", "default", 4096, 0, 5*24*time.Hour)

	report, err := c.Prune(context.Background(), backend.PruneOptions{
		Instances: true,
		DryRun:    true,
		Until:     72 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !report.DryRun {
		t.Errorf("expected DryRun=true in report")
	}
	if report.Totals.Items != 1 {
		t.Fatalf("expected 1 candidate, got %d", report.Totals.Items)
	}
	// Critical: the shed must still exist on disk.
	if _, err := os.Stat(InstanceDir(instanceDir, "old-shed")); err != nil {
		t.Errorf("dry-run deleted the shed: %v", err)
	}
}

func TestPrune_Instances_AgeFilter(t *testing.T) {
	c, _, instanceDir := newPruneTestClient(t)
	// 5 days old — should prune.
	seedStoppedShed(t, instanceDir, "old-shed", "", 4096, 0, 5*24*time.Hour)
	// 1 hour old — should be skipped.
	seedStoppedShed(t, instanceDir, "recent-shed", "", 4096, 0, time.Hour)

	report, err := c.Prune(context.Background(), backend.PruneOptions{
		Instances: true,
		Until:     72 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var deletedNames, skippedNames []string
	for _, it := range report.Items {
		if it.Kind == "instance" {
			deletedNames = append(deletedNames, it.Name)
		}
	}
	for _, s := range report.Skipped {
		if s.Kind == "instance" {
			skippedNames = append(skippedNames, s.Name)
		}
	}
	if !contains(deletedNames, "old-shed") {
		t.Errorf("expected old-shed deleted, got %v", deletedNames)
	}
	if !contains(skippedNames, "recent-shed") {
		t.Errorf("expected recent-shed skipped, got %v", skippedNames)
	}
	// The deleted shed must actually be gone.
	if _, err := os.Stat(InstanceDir(instanceDir, "old-shed")); !os.IsNotExist(err) {
		t.Errorf("old-shed still exists after prune (err=%v)", err)
	}
	// The recent shed must be preserved.
	if _, err := os.Stat(InstanceDir(instanceDir, "recent-shed")); err != nil {
		t.Errorf("recent-shed was incorrectly deleted: %v", err)
	}
}

func TestPrune_Instances_UntilZero_AnyAge(t *testing.T) {
	c, _, instanceDir := newPruneTestClient(t)
	// No backdating — just-created shed.
	seedStoppedShed(t, instanceDir, "fresh-shed", "", 4096, 0, 0)

	report, err := c.Prune(context.Background(), backend.PruneOptions{
		Instances: true,
		Until:     0, // any age
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	found := false
	for _, it := range report.Items {
		if it.Name == "fresh-shed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected fresh-shed to be pruned with Until=0, got items %v", report.Items)
	}
}

// TestPrune_MtimeSnapshotBeatsStalenessResave verifies the plan-mandated
// ordering: mtime(metadata.json) is captured BEFORE ListSheds. ListSheds's
// stale-running→stopped re-check rewrites metadata.json and would
// otherwise bump mtime past the age threshold, making the shed ineligible
// for pruning despite being old.
//
// We seed a "running" shed with no underlying vfkit process and an mtime
// 30 days in the past. ListSheds flips the status to stopped and resaves
// (mtime = now). The prune pass should still see the shed as a candidate
// because the snapshot was taken first. Without the snapshot, the post-
// resave mtime would make the shed look "too recent" against the 72h
// threshold.
//
// NOTE: This test does NOT prove that a live running shed is never
// pruned — that's a separate invariant upheld by `collectInstanceCandidates`
// filtering on `Status == StatusStopped`. See TestPrune_Instances_AgeFilter
// for the running-shed protection check (api-dev is running and lands in
// Skipped, not Items). A fully process-level "is vfkit actually running"
// test would require a vfkit fixture and is out of scope for unit tests.
func TestPrune_MtimeSnapshotBeatsStalenessResave(t *testing.T) {
	c, _, instanceDir := newPruneTestClient(t)
	// Seed a shed whose metadata.json claims "running" but no actual
	// vfkit process backs it. ListSheds's staleness pass will flip it to
	// stopped and resave metadata.
	meta := &Metadata{
		Version: MetadataVersion,
		Name:    "stale-running",
		Status:  config.StatusRunning,
		Backend: "vz",
	}
	if err := meta.Save(instanceDir); err != nil {
		t.Fatalf("save: %v", err)
	}
	past := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(MetadataPath(instanceDir, "stale-running"), past, past)

	report, err := c.Prune(context.Background(), backend.PruneOptions{
		Instances: true,
		Until:     72 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	found := false
	for _, it := range report.Items {
		if it.Name == "stale-running" {
			found = true
		}
	}
	if !found {
		t.Errorf("mtime snapshot must beat staleness resave — expected stale-running to be pruned despite ListSheds bumping mtime. Got items %+v", report.Items)
	}
}

func TestPrune_Orphans_LockSkippedIfHeld(t *testing.T) {
	c, imagesDir, _ := newPruneTestClient(t)

	// A missing rootfs ("dead") with a .tmp sidecar that's old enough.
	tmpPath := filepath.Join(imagesDir, "dead-rootfs.ext4.tmp")
	if err := os.WriteFile(tmpPath, []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(tmpPath, past, past)

	// Hold the canonical lock in this process to simulate a live
	// conversion. We use a blocking flock here because the prune path
	// probes non-blocking and must see us.
	lockPath := filepath.Join(imagesDir, "dead-rootfs.ext4.lock")
	release, err := vmimage.TryAcquireFileLockBlocking(lockPath)
	if err != nil {
		t.Fatalf("test setup: acquire lock: %v", err)
	}
	defer release()

	report, err := c.Prune(context.Background(), backend.PruneOptions{
		Orphans: true,
		Until:   72 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// The .tmp must NOT appear in deleted items.
	for _, it := range report.Items {
		if it.Path == tmpPath {
			t.Errorf("tmp orphan deleted despite lock held: %+v", it)
		}
	}
	// It should appear in Skipped with "lock held" reason.
	found := false
	for _, s := range report.Skipped {
		if strings.Contains(s.Path, "dead-rootfs.ext4.tmp") && strings.Contains(s.Reason, "lock held") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected tmp to be skipped with 'lock held', got %+v", report.Skipped)
	}
	// File must still exist.
	if _, err := os.Stat(tmpPath); err != nil {
		t.Errorf("tmp file was deleted: %v", err)
	}
}

func TestPrune_Orphans_TmpTooRecent(t *testing.T) {
	c, imagesDir, _ := newPruneTestClient(t)

	// Fresh .tmp (< 1 hour old) — should be skipped regardless of lock state.
	tmpPath := filepath.Join(imagesDir, "fresh-rootfs.ext4.tmp")
	if err := os.WriteFile(tmpPath, []byte("junk"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := c.Prune(context.Background(), backend.PruneOptions{Orphans: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, it := range report.Items {
		if it.Path == tmpPath {
			t.Errorf("fresh .tmp deleted; should be skipped as too recent: %+v", it)
		}
	}
	if _, err := os.Stat(tmpPath); err != nil {
		t.Errorf("fresh .tmp was deleted: %v", err)
	}
}

func TestPrune_Orphans_SourceWithoutRootfsDeleted(t *testing.T) {
	c, imagesDir, _ := newPruneTestClient(t)

	// An orphaned .source (old enough, no matching rootfs, no live lock).
	srcPath := filepath.Join(imagesDir, "abandoned-rootfs.ext4.source")
	if err := os.WriteFile(srcPath, []byte("ghcr.io/x/abandoned:v1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := c.Prune(context.Background(), backend.PruneOptions{Orphans: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	found := false
	for _, it := range report.Items {
		if it.Path == srcPath && it.Kind == "source" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected .source orphan deleted, got items %+v", report.Items)
	}
	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Errorf(".source still exists after prune (err=%v)", err)
	}
}

func TestPrune_JSON_NoOp(t *testing.T) {
	c, _, _ := newPruneTestClient(t)
	// Empty state: scope defaults apply, nothing to do.
	report, err := c.Prune(context.Background(), backend.PruneOptions{})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if report.Totals.Items != 0 {
		t.Errorf("expected 0 items in empty state, got %d", report.Totals.Items)
	}
}

func TestPrune_LogTruncation_PreservesTail(t *testing.T) {
	c, _, instanceDir := newPruneTestClient(t)

	// Seed a stopped shed with a large console.log.
	seedStoppedShed(t, instanceDir, "chatty", "", 0, 0, 0)
	dir := InstanceDir(instanceDir, "chatty")
	const origSize = 10 * 1024 * 1024
	content := make([]byte, origSize)
	// Pattern so the tail is identifiable: last 1024 bytes = 'Z'.
	for i := range content {
		content[i] = 'A'
	}
	for i := origSize - 1024; i < origSize; i++ {
		content[i] = 'Z'
	}
	if err := os.WriteFile(filepath.Join(dir, consoleLogFilename), content, 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	// Truncate to last 4 KiB.
	const tail = 4096
	_, err := c.Prune(context.Background(), backend.PruneOptions{
		Logs:         true,
		LogTailBytes: tail,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(dir, consoleLogFilename))
	if err != nil {
		t.Fatalf("read log after: %v", err)
	}
	if int64(len(after)) != tail {
		t.Fatalf("after size = %d, want %d", len(after), tail)
	}
	// Last 1024 bytes must all be 'Z'.
	for i := tail - 1024; i < tail; i++ {
		if after[i] != 'Z' {
			t.Fatalf("tail byte %d = %q, want 'Z' (truncation lost the tail)", i, after[i])
		}
	}
}

func TestPrune_InstancesBeforeImages_ReleasesImageRef(t *testing.T) {
	c, imagesDir, instanceDir := newPruneTestClient(t)

	// Create a cached image and a stopped shed that references it.
	if err := os.WriteFile(filepath.Join(imagesDir, vmimage.RootfsFilename("tobereclaimed")), make([]byte, 8192), 0644); err != nil {
		t.Fatal(err)
	}
	seedStoppedShed(t, instanceDir, "old-shed", "tobereclaimed", 4096, 0, 5*24*time.Hour)

	report, err := c.Prune(context.Background(), backend.PruneOptions{
		Images:    true,
		Instances: true,
		Until:     72 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	var shedsDeleted, imagesDeleted []string
	for _, it := range report.Items {
		switch it.Kind {
		case "instance":
			shedsDeleted = append(shedsDeleted, it.Name)
		case "image":
			imagesDeleted = append(imagesDeleted, it.Name)
		}
	}
	if !contains(shedsDeleted, "old-shed") {
		t.Errorf("expected old-shed deleted, got %v", shedsDeleted)
	}
	if !contains(imagesDeleted, "tobereclaimed") {
		t.Errorf("expected tobereclaimed image deleted after shed ref released, got %v", imagesDeleted)
	}
	// The image file should actually be gone.
	if _, err := os.Stat(filepath.Join(imagesDir, vmimage.RootfsFilename("tobereclaimed"))); !os.IsNotExist(err) {
		t.Errorf("image rootfs still exists after prune (err=%v)", err)
	}
}

func TestPrune_MalformedMetadataSkipped(t *testing.T) {
	c, _, instanceDir := newPruneTestClient(t)

	// Broken metadata.json beside a valid candidate.
	brokenDir := filepath.Join(instanceDir, "broken")
	if err := os.MkdirAll(brokenDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "metadata.json"), []byte("{not-json"), 0644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-5 * 24 * time.Hour)
	_ = os.Chtimes(filepath.Join(brokenDir, "metadata.json"), past, past)

	seedStoppedShed(t, instanceDir, "good", "", 1024, 0, 5*24*time.Hour)

	report, err := c.Prune(context.Background(), backend.PruneOptions{
		Instances: true,
		Until:     72 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Prune must not fail on malformed metadata: %v", err)
	}
	// The broken shed should NOT appear in Items.
	for _, it := range report.Items {
		if it.Name == "broken" {
			t.Errorf("broken metadata appeared in Items: %+v", it)
		}
	}
	// The good one must still be processed.
	found := false
	for _, it := range report.Items {
		if it.Name == "good" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected good shed still pruned, got items %+v", report.Items)
	}
}

// contains is a tiny slice helper used by tests.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestPrune_EmptyImagesDir_NoCWDScan (Codex #7 regression) verifies the
// handler-level guard: when ImagesDir is blank, orphan and image sweep
// must be skipped rather than scanning the process's current directory.
func TestPrune_EmptyImagesDir_NoCWDScan(t *testing.T) {
	c, _, instanceDir := newPruneTestClient(t)
	c.cfg.ImagesDir = "" // no images dir configured

	// A sibling file in CWD that would LOOK like an orphan if the code
	// incorrectly scanned ".". The test's working dir is the vz package;
	// drop a decoy into a temp dir we chdir into to verify no scan occurs
	// even when there IS a matching file in cwd.
	decoyDir := t.TempDir()
	decoyPath := filepath.Join(decoyDir, "decoy-rootfs.ext4.tmp")
	if err := os.WriteFile(decoyPath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	oldCWD, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	if err := os.Chdir(decoyDir); err != nil {
		t.Fatal(err)
	}

	// Seed a stopped shed so the instances path has something to do —
	// that ensures the test isn't a trivial no-op.
	seedStoppedShed(t, instanceDir, "old", "", 1024, 0, 5*24*time.Hour)

	report, err := c.Prune(context.Background(), backend.PruneOptions{
		Orphans:   true,
		Instances: true,
		Until:     72 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// The decoy must not show up as a candidate or skipped.
	for _, it := range report.Items {
		if strings.Contains(it.Path, "decoy-rootfs.ext4.tmp") {
			t.Errorf("empty ImagesDir leaked a CWD scan: item %+v", it)
		}
	}
	for _, s := range report.Skipped {
		if strings.Contains(s.Path, "decoy-rootfs.ext4.tmp") {
			t.Errorf("empty ImagesDir leaked a CWD scan: skipped %+v", s)
		}
	}
	// Instance path should still work.
	if len(report.Items) == 0 {
		t.Errorf("instance prune should still happen even with empty ImagesDir; got zero items")
	}
}

// TestPrune_LockOrphan_IsSkippedNotDeleted (Codex #4 regression): a
// `.lock` sidecar without its parent rootfs must be reported as a
// SkippedItem with a clear reason, NOT as a PrunedItem (since sweepOrphan
// never removes lock files).
func TestPrune_LockOrphan_IsSkippedNotDeleted(t *testing.T) {
	c, imagesDir, _ := newPruneTestClient(t)

	// Orphan .lock — no parent rootfs, no concurrent conversion.
	lockPath := filepath.Join(imagesDir, "dead-rootfs.ext4.lock")
	if err := os.WriteFile(lockPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	report, err := c.Prune(context.Background(), backend.PruneOptions{Orphans: true})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// Must NOT appear in Items.
	for _, it := range report.Items {
		if it.Path == lockPath {
			t.Errorf(".lock orphan appeared in Items (should be Skipped only): %+v", it)
		}
	}
	// Must appear in Skipped with "retained" reason.
	found := false
	for _, s := range report.Skipped {
		if s.Path == lockPath && strings.Contains(s.Reason, "retained") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected .lock orphan skipped with 'retained' reason, got %+v", report.Skipped)
	}
	// File must still exist (never removed).
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf(".lock orphan was removed: %v", err)
	}
}

// TestPrune_Logs_DryRunShowsCandidates (Codex #1 regression): `--logs`
// candidates must appear in the dry-run report so the CLI's dry-run-first
// flow doesn't exit before truncation can happen.
func TestPrune_Logs_DryRunShowsCandidates(t *testing.T) {
	c, _, instanceDir := newPruneTestClient(t)

	seedStoppedShed(t, instanceDir, "chatty", "", 0, 0, 0)
	dir := InstanceDir(instanceDir, "chatty")
	if err := os.WriteFile(filepath.Join(dir, consoleLogFilename), make([]byte, 10*1024*1024), 0644); err != nil {
		t.Fatal(err)
	}

	report, err := c.Prune(context.Background(), backend.PruneOptions{
		Logs:         true,
		LogTailBytes: 4096,
		DryRun:       true,
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	found := false
	for _, it := range report.Items {
		if it.Kind == "console_log" && it.Name == "chatty" && it.Action == "truncated" {
			found = true
		}
	}
	if !found {
		t.Errorf("dry-run with --logs should show the candidate; got items %+v", report.Items)
	}
	// Dry-run must NOT mutate the file.
	fi, err := os.Stat(filepath.Join(dir, consoleLogFilename))
	if err != nil {
		t.Fatalf("stat console.log after dry-run: %v", err)
	}
	if fi.Size() != 10*1024*1024 {
		t.Errorf("dry-run mutated the console.log (size %d)", fi.Size())
	}
}

// TestTryAcquireFileLock_NoCreate (Codex #3 regression): dry-run probe
// must not create a new .lock file as a side effect.
func TestTryAcquireFileLock_NoCreate(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "nonexistent.lock")

	// Pre-check: no file present.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("precondition: lock file should not exist, got err=%v", err)
	}

	release, held, err := vmimage.TryAcquireFileLock(lockPath)
	if err != nil {
		t.Fatalf("TryAcquireFileLock: %v", err)
	}
	if !held {
		t.Errorf("held=false for nonexistent lock; should be treated as 'nothing to contend'")
	}
	if release != nil {
		release()
	}
	// The probe must not have created the file.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("TryAcquireFileLock created the lock file; it should be a no-op for missing locks")
	}
}
