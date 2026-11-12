//go:build darwin

package vz

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// TestCreateRefusesNonOpenProfile: VZ offers only the open profile, and its
// CreateShed refuses anything else with the sentinel the API maps to 422,
// before it touches any state.
func TestCreateRefusesNonOpenProfile(t *testing.T) {
	c := &Client{}
	_, err := c.CreateShed(context.Background(), config.CreateShedRequest{Name: "iso", Network: config.NetworkProfileIsolated})
	if !errors.Is(err, config.ErrNetworkProfileUnsupportedSentinel) {
		t.Fatalf("err = %v, want ErrNetworkProfileUnsupportedSentinel", err)
	}
}

// TestMetadataLegacyNetworkProfileIsOpen: metadata written before network
// profiles existed reports "open".
func TestMetadataLegacyNetworkProfileIsOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version": 3, "name": "old", "status": "stopped", "backend": "vz"}`
	if err := os.WriteFile(MetadataPath(dir, "old"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := LoadMetadata(dir, "old")
	if err != nil {
		t.Fatal(err)
	}
	if got := metadataToShed(meta, "").NetworkProfile; got != config.NetworkProfileOpen {
		t.Errorf("network_profile = %q, want open", got)
	}
}
