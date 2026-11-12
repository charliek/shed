//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// TestMetadataLegacyHasNoOwnerAndAllMounts: a metadata file written before
// service clients and mount selection existed loads with no owner and no
// stored selection, which the Shed JSON reports as {"mode":"all"} and
// SelectMounts treats as every mount.
func TestMetadataLegacyHasNoOwnerAndAllMounts(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version": 3, "name": "old", "status": "stopped", "backend": "firecracker"}`
	if err := os.WriteFile(MetadataPath(dir, "old"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := LoadMetadata(dir, "old")
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.Owner != "" || meta.MountSelection != nil {
		t.Fatalf("legacy metadata: owner=%q selection=%+v, want none", meta.Owner, meta.MountSelection)
	}
	if !meta.MountSelection.Allows("anything") {
		t.Error("legacy selection does not allow every mount")
	}
	shed := metadataToShed(meta)
	if shed.Owner != "" || shed.Mounts == nil || shed.Mounts.Mode != config.MountModeAll {
		t.Errorf("legacy Shed: owner=%q mounts=%+v, want no owner and mode all", shed.Owner, shed.Mounts)
	}
}

// TestMetadataSelectedNoneRoundTrips: an owned shed with "selected" and no
// names saves and reloads as none, never as every mount.
func TestMetadataSelectedNoneRoundTrips(t *testing.T) {
	dir := t.TempDir()
	meta := &Metadata{
		Version:        MetadataVersion,
		Name:           "owned",
		Status:         config.StatusStopped,
		Backend:        "firecracker",
		Owner:          "nocturn",
		MountSelection: config.SelectedMounts(nil),
	}
	if err := meta.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadMetadata(dir, "owned")
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if got.Owner != "nocturn" {
		t.Errorf("owner = %q", got.Owner)
	}
	if got.MountSelection == nil || got.MountSelection.Mode != config.MountModeSelected || len(got.MountSelection.Names) != 0 {
		t.Fatalf("selection = %+v, want selected with no names", got.MountSelection)
	}
	if got.MountSelection.Allows("claude") {
		t.Error("a selected-none shed allows a mount after reload")
	}
	shed := metadataToShed(got)
	if shed.Owner != "nocturn" || shed.Mounts == nil || shed.Mounts.Mode != config.MountModeSelected || len(shed.Mounts.Names) != 0 {
		t.Errorf("Shed: owner=%q mounts=%+v", shed.Owner, shed.Mounts)
	}
}
