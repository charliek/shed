//go:build linux

// This file implements `orchestrator.BackendCreator` for the Firecracker
// backend. It carries the per-step platform logic that used to live
// inline in CreateShed; the orchestrator now owns the call ordering,
// the cleanup-stack scaffolding, and the success/failure return
// contract. See `internal/backend/orchestrator/create.go` for the
// contract and `internal/firecracker/client.go:CreateShed` for the
// wrapper.

package firecracker

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/backend/orchestrator"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
	"github.com/charliek/shed/internal/vmimage/clone"
	"github.com/charliek/shed/internal/vmutil"
)

// fcCreator is Firecracker's implementation of
// `orchestrator.BackendCreator`. It wraps a *Client and carries the
// per-create resolved values (cpus / memory / upper-size, image
// source) inside `fcPreFlight` so subsequent hooks don't re-derive
// them.
type fcCreator struct{ c *Client }

// fcPreFlight is the per-create resolved-input bundle handed from
// PreFlight to the rest of the lifecycle.
type fcPreFlight struct {
	snapshotUpperSource string
	lowerDigest         string
	lowerImageTag       string
	cpus                int
	memoryMB            int
	upperSizeBytes      int64
}

func (p *fcPreFlight) IsFromSnapshot() bool { return p.snapshotUpperSource != "" }

// fcUpper carries the freshly-allocated upper layer between hooks.
type fcUpper struct {
	path string
	size int64
}

// fcNet carries the per-shed network resources from AllocateNetwork
// to subsequent hooks (BuildAndPersistMetadata for Metadata embed
// and the FC-specific RegisterInstance call inside StartVM).
type fcNet struct {
	cid       uint32
	ipAddress string
	tapDevice string
}

// fcMetaHandle carries the persisted *Metadata between hooks. The
// VM-build and FinalizeStartedVM hooks type-assert back to access
// the rest of the fields.
type fcMetaHandle struct{ meta *Metadata }

func (h *fcMetaHandle) Name() string { return h.meta.Name }

// fcVMHandle carries the running VM between hooks.
type fcVMHandle struct{ vm *VM }

// ---------------------------------------------------------------------------
// BackendCreator implementation
// ---------------------------------------------------------------------------

// PreFlight resolves the image-source (or snapshot-source) for the
// requested create, derives cpus/memory/upper-size from the request +
// config defaults, and writes the `.creating` marker that protects
// the lower-digest blob from a concurrent `shed image prune`.
func (b *fcCreator) PreFlight(ctx context.Context, req config.CreateShedRequest, cleanup *backend.Cleanup) (orchestrator.PreFlightResult, error) {
	pre := &fcPreFlight{}

	// Resolve cpus/memory/upper-size with the same bounds checks the
	// old inline CreateShed used.
	cpus := req.CPUs
	if cpus == 0 {
		cpus = b.c.cfg.DefaultCPUs
	}
	memoryMB := req.MemoryMB
	if memoryMB == 0 {
		memoryMB = b.c.cfg.DefaultMemoryMB
	}
	upperSizeBytes := req.UpperSizeBytes
	if upperSizeBytes == 0 {
		sz := b.c.cfg.UpperSizeDefault
		if sz == "" {
			sz = config.DefaultUpperSize
		}
		parsed, err := config.ParseUpperSize(sz)
		if err != nil {
			return nil, fmt.Errorf("invalid upper_size_default: %w", err)
		}
		upperSizeBytes = parsed
	} else {
		if upperSizeBytes < config.MinUpperSizeBytes || upperSizeBytes > config.MaxUpperSizeBytes {
			return nil, fmt.Errorf("upper size out of range: must be between %dG and %dG",
				config.MinUpperSizeBytes/(1024*1024*1024), config.MaxUpperSizeBytes/(1024*1024*1024))
		}
	}
	pre.cpus = cpus
	pre.memoryMB = memoryMB
	pre.upperSizeBytes = upperSizeBytes

	// Resolve image or snapshot source.
	if req.FromSnapshot != "" {
		snap, err := loadSnapshot(b.c.cfg.SnapshotsDir, req.FromSnapshot)
		if err != nil {
			return nil, err
		}
		if snap.Backend != config.BackendFirecracker {
			return nil, fmt.Errorf("%w: snapshot %q is for backend %q, server is %q",
				config.ErrSnapshotBackendMismatchSentinel, req.FromSnapshot, snap.Backend, config.BackendFirecracker)
		}
		if snap.LowerDigest == "" {
			return nil, fmt.Errorf("snapshot %q is missing lower_digest; recreate the snapshot from a current shed", req.FromSnapshot)
		}
		if !vmimage.BlobExists(b.c.cfg.ImagesDir, snap.LowerDigest) {
			ref := snap.SourceImage
			if ref == "" {
				ref = "(unknown)"
			}
			return nil, fmt.Errorf("snapshot %q references lower digest %s which is no longer cached; pull the original image (%s) first",
				req.FromSnapshot, vmimage.ShortDigest(snap.LowerDigest), ref)
		}
		pre.snapshotUpperSource = SnapshotRootfsPath(b.c.cfg.SnapshotsDir, req.FromSnapshot)
		pre.lowerDigest = snap.LowerDigest
		pre.lowerImageTag = snap.SourceImage
		// Note: --upper-size with --from-snapshot is silently ignored
		// here (the cloned upper inherits the snapshot's size). See
		// matching note in `internal/vz/orchestrator.go`.
	} else {
		// Resolve and ensure image before allocating network resources
		// (Codex's #138 note: EnsureImage may pull from Docker which
		// can take minutes; doing it in PreFlight avoids holding the
		// network resources during the pull).
		var resolved config.ResolvedImage
		var err error
		if req.Image != "" {
			resolved, err = b.c.cfg.ResolveImage(req.Image)
			if err != nil {
				return nil, err
			}
		} else {
			if b.c.cfg.BaseRootfs == "" {
				return nil, fmt.Errorf("%w: no --image specified and no base_rootfs configured in firecracker.base_rootfs; pass --image <tag> or set base_rootfs in server.yaml", config.ErrInvalidShedRequestSentinel)
			}
			resolved = b.c.cfg.ResolveBaseRootfs()
		}
		backend.Phase(ctx, "image")
		backend.Status(ctx, "Resolving image...")
		mgr := vmimage.NewManager(b.c.cfg, b.c.refScanner())
		ensureRes, err := mgr.EnsureImage(ctx, vmimage.ResolvedRef{
			Path:      resolved.Path,
			DockerRef: resolved.DockerRef,
			Name:      resolved.Name,
			Digest:    resolved.Digest,
		}, func(stage, msg string) {
			backend.Phase(ctx, stage)
			backend.Status(ctx, msg)
		})
		if err != nil {
			return nil, fmt.Errorf("failed to ensure image: %w", err)
		}
		if ensureRes.Digest == "" {
			return nil, fmt.Errorf("image %q resolved to a path outside the blob store; the overlay model requires content-addressed images", resolved.Name)
		}
		pre.lowerDigest = ensureRes.Digest
		pre.lowerImageTag = resolved.Name
	}

	// Drop a `.creating` marker recording the lower digest so a
	// concurrent `shed image prune` can't sweep the blob between
	// here and meta.Save. Skip for from-snapshot: the snapshot
	// already pins the digest via its own LowerDigest field.
	if pre.lowerDigest != "" && req.FromSnapshot == "" {
		if err := writeCreatingMarker(b.c.cfg.InstanceDir, req.Name, pre.lowerDigest); err != nil {
			return nil, fmt.Errorf("failed to write creating marker: %w", err)
		}
		shedName := req.Name
		cleanup.Register("remove creating marker", func() error {
			removeCreatingMarker(b.c.cfg.InstanceDir, shedName)
			return nil
		})
	}

	return pre, nil
}

// AllocateNetwork acquires the per-shed CID + IP + TAP device.
// Cleanups are Registered individually so a failure at any step
// unwinds only what came before.
func (b *fcCreator) AllocateNetwork(ctx context.Context, req config.CreateShedRequest, cleanup *backend.Cleanup) (orchestrator.NetworkResources, error) {
	backend.Phase(ctx, "network")
	backend.Status(ctx, "Allocating network resources...")
	cid, err := b.c.AllocateCID(req.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate CID: %w", err)
	}
	cleanup.Register("release CID", func() error {
		b.c.ReleaseCID(cid)
		return nil
	})

	tapDevice, ipAddress, err := b.c.AllocateNetwork(req.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to allocate network: %w", err)
	}
	cleanup.Register("release IP", func() error {
		b.c.ReleaseIP(ipAddress)
		return nil
	})

	if err := b.c.netMgr.CreateTAPDevice(tapDevice); err != nil {
		return nil, fmt.Errorf("failed to create TAP device: %w", err)
	}
	cleanup.Register("delete TAP device", func() error {
		return b.c.netMgr.DeleteTAPDevice(tapDevice)
	})

	return fcNet{cid: cid, ipAddress: ipAddress, tapDevice: tapDevice}, nil
}

// AllocateUpper provisions the writable upper layer. Includes the
// snapshot-clone branch for --from-snapshot.
func (b *fcCreator) AllocateUpper(ctx context.Context, req config.CreateShedRequest, preRaw orchestrator.PreFlightResult, cleanup *backend.Cleanup) (orchestrator.UpperInfo, error) {
	pre := preRaw.(*fcPreFlight)

	backend.Phase(ctx, "rootfs")
	backend.Status(ctx, "Allocating writable upper layer...")
	upperPath, err := EnsureUpper(b.c.cfg.UppersDir, req.Name, pre.upperSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create upper: %w", err)
	}
	shedName := req.Name
	cleanup.Register("delete upper layer", func() error {
		return DeleteUpper(b.c.cfg.UppersDir, shedName)
	})

	if pre.snapshotUpperSource != "" {
		// Replace the freshly allocated empty upper with a reflink
		// clone of the snapshot's stored upper so the new shed
		// inherits its parent's writable contents.
		if err := os.Remove(upperPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to clear upper for snapshot clone: %w", err)
		}
		if _, err := clone.CloneFile(pre.snapshotUpperSource, upperPath); err != nil {
			return nil, fmt.Errorf("failed to clone snapshot upper: %w", err)
		}
		if err := os.Chmod(upperPath, 0o644); err != nil {
			return nil, fmt.Errorf("failed to chmod cloned upper: %w", err)
		}
		// The metadata's UpperSizeBytes must reflect the *cloned*
		// file's size; mutate pre so BuildAndPersistMetadata sees
		// the corrected value.
		if fi, statErr := os.Stat(upperPath); statErr == nil {
			pre.upperSizeBytes = fi.Size()
		}
	}
	return fcUpper{path: upperPath, size: pre.upperSizeBytes}, nil
}

// BuildAndPersistMetadata builds the FC Metadata struct and saves it.
// Registers the "delete instance dir" cleanup BEFORE Save (PR #137).
func (b *fcCreator) BuildAndPersistMetadata(ctx context.Context, req config.CreateShedRequest, preRaw orchestrator.PreFlightResult, upperRaw orchestrator.UpperInfo, netRaw orchestrator.NetworkResources, cleanup *backend.Cleanup) (orchestrator.MetadataHandle, error) {
	pre := preRaw.(*fcPreFlight)
	upper := upperRaw.(fcUpper)
	net := netRaw.(fcNet)

	meta := &Metadata{
		Name:           req.Name,
		Status:         config.StatusStopped,
		CreatedAt:      time.Now(),
		Backend:        config.BackendFirecracker,
		CID:            net.cid,
		IPAddress:      net.ipAddress,
		TAPDevice:      net.tapDevice,
		CPUs:           pre.cpus,
		MemoryMB:       pre.memoryMB,
		RootfsPath:     upper.path,
		UpperPath:      upper.path,
		UpperSizeBytes: pre.upperSizeBytes,
		Repo:           req.Repo,
		LocalDir:       req.LocalDir,
		Image:          req.Image,
		LowerDigest:    pre.lowerDigest,
		LowerImageTag:  pre.lowerImageTag,
		FromSnapshot:   req.FromSnapshot,
	}

	cleanup.Register("delete instance dir", func() error {
		return meta.Delete(b.c.cfg.InstanceDir)
	})
	if err := meta.Save(b.c.cfg.InstanceDir); err != nil {
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}
	return &fcMetaHandle{meta: meta}, nil
}

// StartVM creates and starts the per-shed VM. Also performs the FC-
// specific RegisterInstance call (a redundant re-record of the CID
// + IP into the client's maps — already populated by Allocate{CID,
// Network} — but kept for symmetry with UnregisterInstance).
func (b *fcCreator) StartVM(ctx context.Context, metaRaw orchestrator.MetadataHandle, _ orchestrator.UpperInfo, _ orchestrator.NetworkResources, cleanup *backend.Cleanup) (orchestrator.VMHandle, error) {
	meta := metaRaw.(*fcMetaHandle).meta

	b.c.RegisterInstance(meta.Name, meta.CID, meta.IPAddress)

	vm, err := CreateVM(ctx, meta, b.c.cfg, b.c.netMgr)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	backend.Phase(ctx, "vm")
	backend.Status(ctx, "Starting virtual machine...")
	if err := vm.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start VM: %w", err)
	}
	cleanup.Register("stop VM", func() error {
		return vm.Stop(context.Background())
	})
	return fcVMHandle{vm: vm}, nil
}

// FinalizeStartedVM bumps the metadata's Status, re-saves, and
// registers the c.vms map entry's removal cleanup.
func (b *fcCreator) FinalizeStartedVM(ctx context.Context, metaRaw orchestrator.MetadataHandle, vmRaw orchestrator.VMHandle, cleanup *backend.Cleanup) error {
	meta := metaRaw.(*fcMetaHandle).meta
	vm := vmRaw.(fcVMHandle).vm

	meta.Status = config.StatusRunning
	if err := meta.Save(b.c.cfg.InstanceDir); err != nil {
		return fmt.Errorf("failed to save metadata: %w", err)
	}

	b.c.mu.Lock()
	b.c.vms[meta.Name] = vm
	b.c.mu.Unlock()
	shedName := meta.Name
	cleanup.Register("remove from vms map", func() error {
		b.c.mu.Lock()
		defer b.c.mu.Unlock()
		delete(b.c.vms, shedName)
		return nil
	})
	return nil
}

// MountLocalDir mounts --local-dir via 9P when set; no-op otherwise.
// The 9P server runs in-process on the host; we start it after VM
// boot, then ask the guest to mount it.
func (b *fcCreator) MountLocalDir(ctx context.Context, req config.CreateShedRequest, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) error {
	if req.LocalDir == "" {
		return nil
	}
	meta := metaRaw.(*fcMetaHandle).meta
	agent := b.c.newAgentClient(meta.Name)
	backend.Phase(ctx, "9p")
	backend.Status(ctx, "Mounting local directory via 9P...")
	bridgeIP := b.c.netMgr.Gateway()
	srv, err := b.c.startP9Server(req.Name, bridgeIP, req.LocalDir, config.WorkspacePath, false)
	if err != nil {
		return fmt.Errorf("failed to start 9P server for local dir: %w", err)
	}
	// Register the 9P teardown only AFTER startP9Server succeeds.
	// The orchestrator's cleanup is the only path that runs this hook,
	// so we can't access it directly from MountLocalDir's signature —
	// the cleanup must be wired through the orchestrator.
	//
	// In practice, the 9P server is per-shed and lives until shed
	// deletion, so we don't register a cleanup here; the wider shed
	// cleanup (StopShed/DeleteShed) calls stopP9Servers.
	if err := b.c.mount9PInGuest(ctx, agent, srv.Addr(), config.WorkspacePath, false, "workspace"); err != nil {
		// On mount failure we need to stop the just-started 9P
		// server explicitly, since we couldn't register a cleanup.
		b.c.stopP9Servers(req.Name)
		return fmt.Errorf("failed to mount 9P in guest: %w", err)
	}
	return nil
}

// SetupCredentials mounts configured credentials via 9P post-Start.
// Best-effort.
func (b *fcCreator) SetupCredentials(ctx context.Context, req config.CreateShedRequest, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) {
	meta := metaRaw.(*fcMetaHandle).meta
	dirCreds := vmutil.FilterExistingCredentials(b.c.serverCfg)
	if len(dirCreds) > 0 {
		backend.Phase(ctx, "credentials")
		backend.Status(ctx, "Setting up credentials...")
	}
	agent := b.c.newAgentClient(meta.Name)
	b.c.credMgr.SetupCredentials(ctx, agent, req.Name, dirCreds, b.c.mount9PCredentialFunc(req.Name))
}

// CloneRepo runs `git clone` inside the guest when --repo is set.
// Best-effort; mirrors VZ's hook.
func (b *fcCreator) CloneRepo(ctx context.Context, req config.CreateShedRequest, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) {
	if req.Repo == "" || req.LocalDir != "" || req.FromSnapshot != "" {
		return
	}
	meta := metaRaw.(*fcMetaHandle).meta
	agent := b.c.newAgentClient(meta.Name)
	backend.Phase(ctx, "repo")
	backend.Status(ctx, "Cloning repository...")
	if err := vmutil.CloneRepo(ctx, agent, b.c.serverCfg, req.Repo); err != nil {
		log.Printf("Warning: failed to clone repo %s: %v", config.SanitizeRepoURL(req.Repo), err)
		backend.StatusWarning(ctx, "Failed to clone repository (see server logs for details)")
	} else {
		backend.Status(ctx, "Repository cloned")
	}
}

// RunProvisioning loads the per-shed provisioning config and runs
// any hooks it declares. Best-effort.
func (b *fcCreator) RunProvisioning(ctx context.Context, req config.CreateShedRequest, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) {
	if req.NoProvision {
		return
	}
	meta := metaRaw.(*fcMetaHandle).meta
	agent := b.c.newAgentClient(meta.Name)
	provisioner := vmutil.NewProvisioner(agent, req.Name)
	provisioner.SetOutput(os.Stdout, os.Stderr)
	cfg, err := provisioner.LoadConfig(ctx)
	if err != nil {
		log.Printf("Warning: failed to load provisioning config: %v", err)
		return
	}
	runInstall := req.FromSnapshot == ""
	if err := provisioner.RunProvisioning(ctx, cfg, runInstall); err != nil {
		log.Printf("Warning: provisioning failed: %v", err)
	}
}

// ToShedResult returns the *config.Shed value the orchestrator hands
// back to the per-backend CreateShed wrapper's caller.
func (b *fcCreator) ToShedResult(metaRaw orchestrator.MetadataHandle) *config.Shed {
	return metadataToShed(metaRaw.(*fcMetaHandle).meta)
}
