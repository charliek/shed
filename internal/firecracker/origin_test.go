//go:build linux

package firecracker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/delivery"
)

// TestStampUserOrigins: the start-up pass gives each shed with no recorded
// user origin the current origin of its user's name, durably, once; a shed
// already stamped keeps its own.
func TestStampUserOrigins(t *testing.T) {
	dir := t.TempDir()
	c := &Client{cfg: testFirecrackerConfig(dir)}
	old := createTestInstance(t, dir, "old")
	old.User = "ada"
	if err := old.Save(dir); err != nil {
		t.Fatal(err)
	}
	stamped := createTestInstance(t, dir, "stamped")
	stamped.User, stamped.UserOrigin = "ada", delivery.OriginLocal
	if err := stamped.Save(dir); err != nil {
		t.Fatal(err)
	}
	origin := func(s backend.OriginShed) (string, bool) {
		if s.User == "ada" {
			return delivery.OriginDirectory, true
		}
		return delivery.OriginLocal, true
	}
	got, err := NewBackend(c).StampUserOrigins(context.Background(), origin)
	if err != nil || !slices.Equal(got, []string{"old"}) {
		t.Fatalf("StampUserOrigins = %v, %v", got, err)
	}
	for name, want := range map[string]string{"old": delivery.OriginDirectory, "stamped": delivery.OriginLocal} {
		m, err := LoadMetadata(dir, name)
		if err != nil || m.UserOrigin != want {
			t.Fatalf("%s: user_origin %q (%v), want %q", name, m.UserOrigin, err, want)
		}
	}
	if again, _ := c.StampUserOrigins(context.Background(), origin); len(again) != 0 {
		t.Fatalf("a second pass stamped %v", again)
	}
}

// TestStampUserOriginsFailedSave: a stamp whose save is not durable (the
// directory's sync fails) is not counted, leaves the metadata without an
// origin (the shed fails closed), and names the shed in its error; the next
// pass stamps it.
func TestStampUserOriginsFailedSave(t *testing.T) {
	dir := t.TempDir()
	c := &Client{cfg: testFirecrackerConfig(dir)}
	createTestInstance(t, dir, "old")
	prev := metadataDirSync
	metadataDirSync = func(string) error { return errors.New("sync failed") }
	got, err := c.StampUserOrigins(context.Background(), func(backend.OriginShed) (string, bool) { return delivery.OriginLocal, true })
	metadataDirSync = prev
	if err == nil || len(got) != 0 || !strings.Contains(err.Error(), "old") {
		t.Fatalf("StampUserOrigins = %v, %v", got, err)
	}
	if m, _ := LoadMetadata(dir, "old"); m.UserOrigin != "" {
		t.Fatalf("a failed stamp left user_origin %q", m.UserOrigin)
	}
	if got, err := c.StampUserOrigins(context.Background(), func(backend.OriginShed) (string, bool) { return delivery.OriginLocal, true }); err != nil || len(got) != 1 {
		t.Fatalf("the next pass = %v, %v", got, err)
	}
}
