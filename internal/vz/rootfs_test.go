//go:build darwin
// +build darwin

package vz

import (
	"testing"
)

func TestRootfsPath(t *testing.T) {
	path := RootfsPath("/var/lib/shed/vz/instances", "my-vm")
	want := "/var/lib/shed/vz/instances/my-vm/rootfs.ext4"
	if path != want {
		t.Errorf("RootfsPath() = %q, want %q", path, want)
	}
}
