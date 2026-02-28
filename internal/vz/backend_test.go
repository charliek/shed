//go:build darwin
// +build darwin

package vz

import (
	"testing"

	"github.com/charliek/shed/internal/backend"
)

func TestVZBackendType(t *testing.T) {
	b := &VZBackend{}
	if b.Type() != backend.TypeVZ {
		t.Errorf("Type() = %q, want %q", b.Type(), backend.TypeVZ)
	}
}

func TestVZBackendImplementsInterface(t *testing.T) {
	// This is also checked at compile time via the var _ line in backend.go,
	// but this test makes it explicit.
	var _ backend.Backend = (*VZBackend)(nil)
}
