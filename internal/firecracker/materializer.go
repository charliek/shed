//go:build linux
// +build linux

// Materializer entry point for the firecracker backend.
//
// On Linux hosts (the only platform where firecracker runs) the
// vmimage package's dispatcher takes the host-native fast path —
// mkfs.erofs runs directly via exec.Command, no VM required. The
// runtime dep is satisfied by `erofs-utils` in the deb's Depends
// list.
//
// This file exists for symmetry with internal/vz/materializer.go and
// to leave a clear hook for the future cross-arch case (a Mac host
// running firecracker for ARM Linux, or vice versa via cross-arch
// VMs). Today both are out of scope, so RunMaterializer is a
// not-implemented stub. MaterializerHook returns nil so callers can
// register it unconditionally without disabling the host-native path.

package firecracker

import (
	"context"
	"fmt"

	"github.com/charliek/shed/internal/vmimage"
)

// MaterializerOpts mirrors the vz package's shape so future code can
// share a config type across backends without reshuffling fields.
type MaterializerOpts struct {
	KernelPath    string
	InitrdPath    string
	InputBlobsDir string
	InputDigest   string
	OutputPath    string
	CPUs          int
	MemoryMiB     int
}

// RunMaterializer is not implemented. The firecracker backend only
// runs on Linux, and Linux hosts use vmimage.materializeNativeLinux
// directly — there's no scenario today where a firecracker host needs
// to launch a microVM just to run mkfs.erofs. Returns a sentinel error
// callers can treat as "use the host-native path."
func RunMaterializer(ctx context.Context, opts MaterializerOpts) error {
	return fmt.Errorf("%w: firecracker materializer VM not implemented (use the host-native mkfs.erofs path)", vmimage.ErrMaterializerUnavailable)
}

// MaterializerHook returns nil. The firecracker backend defers to the
// host-native materializer on Linux; registering a nil hook means the
// dispatcher falls straight through to materializeNativeLinux.
func MaterializerHook(imagesDir string) vmimage.MaterializerFunc {
	return nil
}
