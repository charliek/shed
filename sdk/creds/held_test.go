package creds_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/sdk/creds"
)

// TestHeldVariantsRunUnderTheCallersLock: WriteHeld and Staged.CommitHeld are
// the writes a caller makes while it already holds the server's lock (the CLI
// holds it across a config read, the write and a config save). They must not
// take it again: flock is not re-entrant, so a second take would block the
// caller on itself. With the lock held, both finish and leave the pair in
// place.
func TestHeldVariantsRunUnderTheCallersLock(t *testing.T) {
	run := func(t *testing.T, write func(s *creds.Store, certPEM, keyPEM []byte) error) {
		t.Helper()
		s := creds.NewStore(filepath.Join(t.TempDir(), "creds"))
		certPEM, keyPEM := issue(t, "held")
		unlock, err := s.Lock("srv")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()

		done := make(chan error, 1)
		go func() { done <- write(s, certPEM, keyPEM) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("held write: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the held write blocked on the lock its caller holds")
		}
		certPath, keyPath := s.Paths("srv")
		gotCert, _ := os.ReadFile(certPath)
		gotKey, _ := os.ReadFile(keyPath)
		if !bytes.Equal(gotCert, certPEM) || !bytes.Equal(gotKey, keyPEM) {
			t.Error("the held write did not leave the pair in place")
		}
	}

	t.Run("WriteHeld", func(t *testing.T) {
		run(t, func(s *creds.Store, certPEM, keyPEM []byte) error {
			_, _, err := s.WriteHeld("srv", certPEM, keyPEM)
			return err
		})
	})
	t.Run("CommitHeld", func(t *testing.T) {
		run(t, func(s *creds.Store, certPEM, keyPEM []byte) error {
			staged, err := s.Stage("srv", certPEM, keyPEM)
			if err != nil {
				return err
			}
			return staged.CommitHeld()
		})
	})
}
