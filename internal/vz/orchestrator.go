//go:build darwin

// This file implements `orchestrator.BackendCreator` for the VZ
// backend. It carries the per-step platform logic that used to live
// inline in CreateShed; the orchestrator now owns the call ordering,
// the cleanup-stack scaffolding, and the success/failure return
// contract. See `internal/backend/orchestrator/create.go` for the
// contract and `internal/vz/client.go:CreateShed` for the wrapper.

package vz

import (
	"context"
	"errors"
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

// vzCreator is VZ's implementation of `orchestrator.BackendCreator`.
// It wraps a *Client and carries the per-create resolved values
// (cpus / memory / upper-size) inside `vzPreFlight` so subsequent
// hooks don't re-derive them.
// vzEgressGateway / vzEgressSubnet are the macOS vmnet shared-mode defaults the
// guest reaches the host on. vmnet does not expose these per-shed, so we assume
// the default subnet; a non-default vmnet config would need a config override
// (the proxy bind fails loudly if the gateway IP is wrong — never silently).
const (
	vzEgressGateway = "192.168.64.1"
	vzEgressSubnet  = "192.168.64.0/24"
)

// egressGatewaySubnet returns the gateway IP + subnet CIDR the guest reaches the
// host (proxy) on. For VZ these are the vmnet shared-mode defaults.
func (c *Client) egressGatewaySubnet() (gateway, subnet string) {
	return vzEgressGateway, vzEgressSubnet
}

type vzCreator struct {
	c *Client
	// egressEnv is the proxy env injected by ConfigureEgressProxy, threaded
	// into the subsequent CloneRepo exec so the clone is audited. nil ⇒ egress
	// off for this shed.
	egressEnv []string
}

// vzPreFlight is the per-create resolved-input bundle handed from
// PreFlight to the rest of the lifecycle. Tracks `snapshotUpperSource`
// (set when --from-snapshot is in play) and the resolved cpus /
// memory / upper-size — values the old inline CreateShed computed
// once and used in three places.
type vzPreFlight struct {
	snapshotUpperSource string
	lowerDigest         string
	lowerImageTag       string
	cpus                int
	memoryMB            int
	upperSizeBytes      int64
	hasCreatingMarker   bool
}

func (p *vzPreFlight) IsFromSnapshot() bool { return p.snapshotUpperSource != "" }

type vzUpper struct {
	path string
	size int64
}

// vzNet is a placeholder satisfying NetworkResources. VZ does not
// allocate per-shed network state (Apple's vmnet is shared NAT).
type vzNet struct{}

// vzMetaHandle carries the persisted *Metadata between hooks. Only
// Name() is exported via the orchestrator contract; the VM-build
// hook type-asserts back to access the rest of the fields.
type vzMetaHandle struct{ meta *Metadata }

func (h *vzMetaHandle) Name() string { return h.meta.Name }

// vzVMHandle carries the running VM between hooks.
type vzVMHandle struct{ vm *VM }

// ---------------------------------------------------------------------------
// BackendCreator implementation
// ---------------------------------------------------------------------------

// PreFlight resolves the image-source (or snapshot-source) for the
// requested create, derives cpus/memory/upper-size from the request +
// config defaults, and writes the `.creating` marker that protects
// the lower-digest blob from a concurrent `shed image prune`.
//
// The marker-removal cleanup is registered on the orchestrator's
// `cleanup` stack so a subsequent failure unwinds it.
func (b *vzCreator) PreFlight(ctx context.Context, req config.CreateShedRequest, cleanup *backend.Cleanup) (orchestrator.PreFlightResult, error) {
	pre := &vzPreFlight{}

	// Resolve cpus / memory / upper-size with the same bounds checks
	// the old inline CreateShed used.
	cpus := req.CPUs
	if cpus == 0 {
		cpus = b.c.cfg.DefaultCPUs
	}
	if cpus < 1 || cpus > config.MaxVZCPUs {
		return nil, fmt.Errorf("invalid cpus %d: must be between 1 and %d", cpus, config.MaxVZCPUs)
	}
	memoryMB := req.MemoryMB
	if memoryMB == 0 {
		memoryMB = b.c.cfg.DefaultMemoryMB
	}
	if memoryMB < 128 || memoryMB > config.MaxVZMemoryMB {
		return nil, fmt.Errorf("invalid memory_mb %d: must be between 128 and %d", memoryMB, config.MaxVZMemoryMB)
	}
	upperSizeBytes := req.UpperSizeBytes
	if upperSizeBytes == 0 {
		sz := b.c.cfg.UpperSizeDefault
		if sz == "" {
			sz = config.DefaultUpperSize
		}
		parsed, perr := config.ParseUpperSize(sz)
		if perr != nil {
			return nil, fmt.Errorf("invalid upper_size_default: %w", perr)
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
		if snap.Backend != config.BackendVZ {
			return nil, fmt.Errorf("%w: snapshot %q is for backend %q, server is %q",
				config.ErrSnapshotBackendMismatchSentinel, req.FromSnapshot, snap.Backend, config.BackendVZ)
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
	} else {
		var resolved config.ResolvedImage
		if req.Image != "" {
			var err error
			resolved, err = b.c.cfg.ResolveImage(req.Image)
			if err != nil {
				return nil, err
			}
		} else {
			if b.c.cfg.DefaultImage == "" {
				return nil, fmt.Errorf("%w: no --image specified and no default_image configured in vz.default_image; pass --image <ref> or set default_image in server.yaml", config.ErrInvalidShedRequestSentinel)
			}
			var err error
			resolved, err = b.c.cfg.ResolveBaseRootfs()
			if err != nil {
				return nil, err
			}
		}
		backend.Phase(ctx, "image")
		backend.Status(ctx, "Resolving image...")
		_, ldigest, err := b.c.EnsureImage(ctx, resolved)
		if err != nil {
			return nil, err
		}
		if ldigest == "" {
			return nil, fmt.Errorf("image %q resolved to a path outside the blob store; the overlay model requires content-addressed images", resolved.Name)
		}
		pre.lowerDigest = ldigest
		pre.lowerImageTag = resolved.Name
	}

	// Drop a `.creating` marker so a concurrent `shed image prune`
	// can't sweep the blob between here and meta.Save. The marker
	// counts as a Pending protective ref in the refscanner. Skip
	// for from-snapshot: the snapshot already pins the digest via
	// its own LowerDigest field.
	if pre.lowerDigest != "" && req.FromSnapshot == "" {
		if err := writeCreatingMarker(b.c.cfg.InstanceDir, req.Name, pre.lowerDigest); err != nil {
			return nil, fmt.Errorf("failed to write creating marker: %w", err)
		}
		pre.hasCreatingMarker = true
		shedName := req.Name
		// AddDeferred (not Register) so the marker is also removed
		// on the success path — the marker's lifetime is the create
		// operation itself, not the resulting shed. The shed's
		// Metadata.LowerDigest takes over blob protection from here.
		// PR #140's Codex review caught this regression in both
		// backends.
		cleanup.AddDeferred("remove creating marker", func() error {
			removeCreatingMarker(b.c.cfg.InstanceDir, shedName)
			return nil
		})
	}

	return pre, nil
}

// AllocateNetwork is a no-op for VZ — Apple's vmnet provides shared
// NAT with no per-shed allocation.
func (b *vzCreator) AllocateNetwork(ctx context.Context, req config.CreateShedRequest, cleanup *backend.Cleanup) (orchestrator.NetworkResources, error) {
	return vzNet{}, nil
}

// AllocateUpper provisions the writable upper layer. Includes the
// snapshot-clone branch (when spawning from a snapshot) and the
// best-effort template-mint branch (otherwise) — both used to live
// inline in CreateShed.
func (b *vzCreator) AllocateUpper(ctx context.Context, req config.CreateShedRequest, preRaw orchestrator.PreFlightResult, cleanup *backend.Cleanup) (orchestrator.UpperInfo, error) {
	pre := preRaw.(*vzPreFlight)

	upperPath, err := EnsureUpper(b.c.cfg.UppersDir, req.Name, pre.upperSizeBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create upper: %w", err)
	}
	shedName := req.Name
	cleanup.Register("delete upper layer", func() error {
		return DeleteUpper(b.c.cfg.UppersDir, shedName)
	})

	if pre.snapshotUpperSource != "" {
		// Replace the freshly allocated empty upper with a clone of
		// the snapshot's stored upper so the new shed inherits its
		// parent's writable contents.
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
		// file's actual size; mutate pre so BuildAndPersistMetadata
		// sees the corrected value.
		if fi, statErr := os.Stat(upperPath); statErr == nil {
			pre.upperSizeBytes = fi.Size()
		}
	} else {
		// Clone a pre-formatted ext4 template into the upper so the
		// guest mounts it directly and skips the multi-second
		// in-guest mkfs on first boot. Best-effort: any failure
		// leaves the freshly-allocated signature upper in place
		// (formatted in-guest), so create never regresses.
		if tmpl, terr := EnsureUpperTemplate(ctx, b.c.templatesDir(), resolveBuildToolsRef(), pre.upperSizeBytes, ""); terr != nil {
			if errors.Is(terr, context.Canceled) || errors.Is(terr, context.DeadlineExceeded) {
				return nil, fmt.Errorf("upper template provisioning canceled: %w", terr)
			}
			log.Printf("[%s] upper template unavailable (%v); formatting in guest", req.Name, terr)
		} else if perr := provisionUpperFromTemplate(upperPath, tmpl); perr != nil {
			if errors.Is(perr, context.Canceled) || errors.Is(perr, context.DeadlineExceeded) {
				return nil, fmt.Errorf("upper template provisioning canceled: %w", perr)
			}
			log.Printf("[%s] upper template clone failed (%v); formatting in guest", req.Name, perr)
		} else {
			backend.Phase(ctx, "rootfs")
			backend.Status(ctx, "Provisioned upper from template (skips in-guest mkfs)")
		}
	}
	return vzUpper{path: upperPath, size: pre.upperSizeBytes}, nil
}

// BuildAndPersistMetadata builds the VZ Metadata struct and saves it.
// Registers the "delete instance dir" cleanup BEFORE Save (see
// PR #137 review for the partial-MkdirAll regression history).
func (b *vzCreator) BuildAndPersistMetadata(ctx context.Context, req config.CreateShedRequest, preRaw orchestrator.PreFlightResult, upperRaw orchestrator.UpperInfo, _ orchestrator.NetworkResources, cleanup *backend.Cleanup) (orchestrator.MetadataHandle, error) {
	pre := preRaw.(*vzPreFlight)
	upper := upperRaw.(vzUpper)

	projectMounts, landingDir, err := config.ResolveCreateLayout(req)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve workspace layout: %w", err)
	}

	meta := &Metadata{
		Name:           req.Name,
		Status:         config.StatusStopped,
		CreatedAt:      time.Now(),
		Backend:        config.BackendVZ,
		CPUs:           pre.cpus,
		MemoryMB:       pre.memoryMB,
		RootfsPath:     upper.path,
		UpperPath:      upper.path,
		UpperSizeBytes: pre.upperSizeBytes,
		Repo:           req.Repo,
		ProjectMounts:  projectMounts,
		LandingDir:     landingDir,
		Image:          req.Image,
		LowerDigest:    pre.lowerDigest,
		LowerImageTag:  pre.lowerImageTag,
		FromSnapshot:   req.FromSnapshot,
	}

	// Register BEFORE Save: Save's `os.MkdirAll` runs first, so a
	// partial-write failure could leave the dir behind.
	// `preserveConsoleLog` is safe pre-Start (no console.log yet —
	// see vz/metadata.go:PreserveConsoleLog).
	cleanup.Register("preserve console log + delete instance dir", func() error {
		b.c.preserveConsoleLog(meta)
		return meta.Delete(b.c.cfg.InstanceDir)
	})
	if err := meta.Save(b.c.cfg.InstanceDir); err != nil {
		return nil, fmt.Errorf("failed to save metadata: %w", err)
	}
	return &vzMetaHandle{meta: meta}, nil
}

// StartVM constructs and starts the VM (including the per-shed
// VirtioFS credential-share pre-computation that VZ requires before
// the VM boots — the existing inline CreateShed did this between
// meta.Save and CreateVM).
func (b *vzCreator) StartVM(ctx context.Context, metaRaw orchestrator.MetadataHandle, _ orchestrator.UpperInfo, _ orchestrator.NetworkResources, cleanup *backend.Cleanup) (orchestrator.VMHandle, error) {
	meta := metaRaw.(*vzMetaHandle).meta

	// vfkit needs every VirtioFS share declared up front before the
	// VM boots; SetupCredentials post-Start only handles the guest-
	// side mount. Filter to creds whose source exists to avoid vfkit
	// rejecting the share.
	dirCreds := vmutil.FilterExistingCredentials(b.c.serverCfg)
	vm := CreateVM(meta, b.c.cfg)
	vm.credentialShares = buildCredentialShares(dirCreds)

	if err := vm.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start VM: %w", err)
	}
	cleanup.Register("stop VM", func() error {
		return vm.Stop(context.Background())
	})
	return vzVMHandle{vm: vm}, nil
}

// FinalizeStartedVM bumps the metadata's Status/PID, re-saves, and
// registers the c.vms map entry's removal cleanup.
func (b *vzCreator) FinalizeStartedVM(ctx context.Context, metaRaw orchestrator.MetadataHandle, vmRaw orchestrator.VMHandle, cleanup *backend.Cleanup) error {
	meta := metaRaw.(*vzMetaHandle).meta
	vm := vmRaw.(vzVMHandle).vm

	meta.Status = config.StatusRunning
	meta.PID = vm.meta.PID
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

// MountLocalDir mounts each project directory (--local-dir / --add-dir) via
// VirtioFS when set; no-op otherwise.
func (b *vzCreator) MountLocalDir(ctx context.Context, req config.CreateShedRequest, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) error {
	meta := metaRaw.(*vzMetaHandle).meta
	if len(meta.ProjectMounts) == 0 {
		return nil
	}
	agent := b.c.newAgentClient(meta.Name)
	backend.Phase(ctx, "mount")
	backend.Status(ctx, "Mounting project directories via VirtioFS...")
	for _, m := range meta.ProjectMounts {
		tag := config.ProjectMountTagForTarget(m.Target)
		if err := b.c.mountVirtioFSShareWithRetry(ctx, agent, tag, m.Target, m.ReadOnly); err != nil {
			return fmt.Errorf("VirtioFS mount failed for %s: %w", m.Source, err)
		}
	}
	return nil
}

// SetupCredentials mounts configured credentials post-Start. Best-effort.
func (b *vzCreator) SetupCredentials(ctx context.Context, req config.CreateShedRequest, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) {
	meta := metaRaw.(*vzMetaHandle).meta
	dirCreds := vmutil.FilterExistingCredentials(b.c.serverCfg)
	agent := b.c.newAgentClient(meta.Name)
	b.c.credMgr.SetupCredentials(ctx, agent, req.Name, dirCreds, b.c.mountVirtioFSCredential)
}

// ConfigureEgressProxy opens this shed's egress proxy listener and injects the
// proxy env when egress control is enabled and profiles resolve to a non-empty
// policy. Failable: a configure/inject error aborts the create (the listener
// teardown is registered on cleanup). No-op when egress is disabled or off for
// this shed.
func (b *vzCreator) ConfigureEgressProxy(ctx context.Context, req config.CreateShedRequest, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle, cleanup *backend.Cleanup) error {
	if b.c.egressMgr == nil {
		return nil
	}
	specs, err := b.c.serverCfg.Egress.ResolveProfiles(req.Egress)
	if err != nil {
		return fmt.Errorf("egress: resolve profiles: %w", err)
	}
	if len(specs) == 0 {
		return nil // egress off for this shed
	}
	meta := metaRaw.(*vzMetaHandle).meta
	agent := b.c.newAgentClient(meta.Name)
	gateway, subnet := b.c.egressGatewaySubnet()
	port, token, env, err := vmutil.SetupEgress(ctx, b.c.egressMgr, agent, meta.Name, meta.EgressPort, meta.EgressToken, gateway, subnet, specs, cleanup)
	if err != nil {
		return fmt.Errorf("egress: %w", err)
	}
	meta.EgressProfiles = req.Egress
	meta.EgressPort = port
	meta.EgressToken = token
	if err := meta.Save(b.c.cfg.InstanceDir); err != nil {
		return fmt.Errorf("egress: save metadata: %w", err)
	}
	b.egressEnv = env
	return nil
}

// CloneRepo runs `git clone` inside the guest when --repo is set
// (and not overridden by --local-dir / --from-snapshot). Best-effort:
// failures emit a sanitized StatusWarning and the create still
// completes — matching today's behavior.
func (b *vzCreator) CloneRepo(ctx context.Context, req config.CreateShedRequest, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) {
	if req.Repo == "" || req.LocalDir != "" || req.FromSnapshot != "" {
		return
	}
	meta := metaRaw.(*vzMetaHandle).meta
	agent := b.c.newAgentClient(meta.Name)
	backend.Phase(ctx, "repo")
	backend.Status(ctx, "Cloning repository...")
	if err := vmutil.CloneRepo(ctx, agent, b.c.serverCfg, req.Repo, b.egressEnv); err != nil {
		log.Printf("Warning: failed to clone repo %s: %v", config.SanitizeRepoURL(req.Repo), err)
		// Status attaches to the current ("clone") phase — see
		// PR #135's opportunistic cleanup that removed the
		// `repo=N clone=M repo=K` triple shape.
		backend.StatusWarning(ctx, "Failed to clone repository (see server logs for details)")
	} else {
		backend.Status(ctx, "Repository cloned")
	}
}

// RunProvisioning loads the per-shed provisioning config and runs
// any hooks it declares (unless the request opted out via
// NoProvision). Best-effort.
func (b *vzCreator) RunProvisioning(ctx context.Context, req config.CreateShedRequest, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) {
	if req.NoProvision {
		return
	}
	meta := metaRaw.(*vzMetaHandle).meta
	agent := b.c.newAgentClient(meta.Name)
	provisioner := vmutil.NewProvisioner(agent, req.Name)
	provisioner.SetWorkDir(meta.LandingDir)
	provisioner.SetAddDirs(config.ProjectAddDirTargets(meta.ProjectMounts, meta.LandingDir))
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
func (b *vzCreator) ToShedResult(metaRaw orchestrator.MetadataHandle) *config.Shed {
	return metadataToShed(metaRaw.(*vzMetaHandle).meta, "127.0.0.1")
}

// ---------------------------------------------------------------------------
// BackendStarter implementation (orchestrator.StartShed lifecycle)
//
// vzStarter wraps the same *Client as vzCreator. Hooks that overlap
// with vzCreator (mount, credentials, ToShedResult) reuse the
// underlying Client helpers directly rather than delegating to the
// vzCreator methods — the contracts diverge on input shape
// (CreateShedRequest vs persisted metadata) and keeping the codepaths
// distinct makes the start-only behaviors easier to follow.
// ---------------------------------------------------------------------------

type vzStarter struct{ c *Client }

// LoadMetadata reads the per-shed metadata and wraps the canonical
// NotFound error with the API-level sentinel.
func (b *vzStarter) LoadMetadata(_ context.Context, req orchestrator.StartRequest) (orchestrator.MetadataHandle, error) {
	meta, err := LoadMetadata(b.c.cfg.InstanceDir, req.Name)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, req.Name)
		}
		return nil, err
	}
	return &vzMetaHandle{meta: meta}, nil
}

// CheckNotRunning enforces the same two-sentinel contract documented
// on BackendStarter: ErrShedAlreadyRunningSentinel for a live VMM,
// ErrZombiePresentSentinel for a stale-PID alive process. Falls
// through clearing meta.Status / meta.PID when the recorded state
// turns out to be stale (status=Running but no live vfkit).
func (b *vzStarter) CheckNotRunning(_ context.Context, metaRaw orchestrator.MetadataHandle) error {
	meta := metaRaw.(*vzMetaHandle).meta

	if meta.Status == config.StatusRunning {
		vm := &VM{meta: meta, cfg: b.c.cfg}
		if vm.IsRunning() {
			return fmt.Errorf("%w: %s", config.ErrShedAlreadyRunningSentinel, meta.Name)
		}
		meta.Status = config.StatusStopped
		meta.PID = 0
	}

	// Defensive zombie-pid check — same shape as client.go:StartShed
	// before the orchestrator migration. Refuse to double-spawn even
	// if status reads "stopped" but the recorded PID is still a live
	// vfkit (server crash mid-Save, hand-edited metadata, etc).
	if meta.PID > 0 && vmutil.IsProcessAlive(meta.PID) && isVfkitProcess(meta.PID) {
		return fmt.Errorf("%w: %s (pid %d)", config.ErrZombiePresentSentinel, meta.Name, meta.PID)
	}
	meta.PID = 0

	return nil
}

// StartVM builds the credential-share list (vfkit needs every share
// declared before boot) and starts the VM. Registers the "stop VM"
// cleanup. Mirrors vzCreator.StartVM but takes no UpperInfo/
// NetworkResources (StartShed loads everything from metadata).
func (b *vzStarter) StartVM(ctx context.Context, metaRaw orchestrator.MetadataHandle, cleanup *backend.Cleanup) (orchestrator.VMHandle, error) {
	meta := metaRaw.(*vzMetaHandle).meta

	dirCreds := vmutil.FilterExistingCredentials(b.c.serverCfg)
	vm := CreateVM(meta, b.c.cfg)
	vm.credentialShares = buildCredentialShares(dirCreds)

	if err := vm.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start VM: %w", err)
	}
	cleanup.Register("stop VM", func() error {
		return vm.Stop(context.Background())
	})
	return vzVMHandle{vm: vm}, nil
}

// PersistRunningState flips the metadata to Running, re-saves, and
// registers the c.vms map entry's removal cleanup. Mirrors
// vzCreator.FinalizeStartedVM.
func (b *vzStarter) PersistRunningState(_ context.Context, metaRaw orchestrator.MetadataHandle, vmRaw orchestrator.VMHandle, cleanup *backend.Cleanup) error {
	meta := metaRaw.(*vzMetaHandle).meta
	vm := vmRaw.(vzVMHandle).vm

	// vm.meta aliases the same *Metadata as meta (CreateVM stores the
	// pointer), so vm.Start's `vm.meta.PID = cmd.Process.Pid` write
	// already lands here. The explicit assignment mirrors
	// vzCreator.FinalizeStartedVM — keeps the PID transfer visible at
	// the persistence boundary in case the aliasing changes later.
	meta.Status = config.StatusRunning
	meta.PID = vm.meta.PID
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

// RestoreStoppedMetadata rewrites the persisted metadata back to
// Stopped/PID=0 so a post-PersistRunningState failure doesn't leave
// disk lying. The orchestrator wires this in as a cleanup BEFORE
// StartVM (so LIFO runs it AFTER "stop VM" has unwound); see
// orchestrator/start.go for the ordering rationale.
//
// Defensive check mirrors StopShed's pre-flip guard at client.go's
// IsProcessAlive call: if the "stop VM" cleanup couldn't actually
// kill vfkit (uninterruptible I/O, kernel hang, etc.), refuse to
// clear the PID — the GetShed lazy-staleness check is a better
// backstop than the lying-Stopped state a force-clear would write.
func (b *vzStarter) RestoreStoppedMetadata(metaRaw orchestrator.MetadataHandle) error {
	meta := metaRaw.(*vzMetaHandle).meta
	if meta.PID > 0 && vmutil.IsProcessAlive(meta.PID) && isVfkitProcess(meta.PID) {
		return fmt.Errorf("vfkit still alive (pid %d) after stop-VM cleanup; leaving metadata as-is for GetShed staleness check", meta.PID)
	}
	meta.Status = config.StatusStopped
	meta.PID = 0
	return meta.Save(b.c.cfg.InstanceDir)
}

// MountLocalDir re-mounts each project VirtioFS share when
// meta.ProjectMounts is set. VirtioFS mounts don't persist across vfkit
// restarts; this is the start-time refresh.
func (b *vzStarter) MountLocalDir(ctx context.Context, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) error {
	meta := metaRaw.(*vzMetaHandle).meta
	if len(meta.ProjectMounts) == 0 {
		return nil
	}
	agent := b.c.newAgentClient(meta.Name)
	for _, m := range meta.ProjectMounts {
		tag := config.ProjectMountTagForTarget(m.Target)
		if err := b.c.mountVirtioFSShareWithRetry(ctx, agent, tag, m.Target, m.ReadOnly); err != nil {
			return fmt.Errorf("VirtioFS mount failed on start for %s: %w", m.Source, err)
		}
	}
	return nil
}

// SetupCredentials re-mounts configured credentials. Best-effort
// (the credentials manager itself log-and-continues per credential).
func (b *vzStarter) SetupCredentials(ctx context.Context, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) {
	meta := metaRaw.(*vzMetaHandle).meta
	dirCreds := vmutil.FilterExistingCredentials(b.c.serverCfg)
	agent := b.c.newAgentClient(meta.Name)
	b.c.credMgr.SetupCredentials(ctx, agent, meta.Name, dirCreds, b.c.mountVirtioFSCredential)
}

// ConfigureEgressProxy re-opens this shed's egress proxy listener on the
// per-shed port persisted at create time and re-injects the proxy env.
// Failable + unwinds via cleanup. No-op when egress is disabled or was off for
// this shed.
func (b *vzStarter) ConfigureEgressProxy(ctx context.Context, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle, cleanup *backend.Cleanup) error {
	if b.c.egressMgr == nil {
		return nil
	}
	meta := metaRaw.(*vzMetaHandle).meta
	if meta.EgressPort == 0 && len(meta.EgressProfiles) == 0 {
		return nil // egress was off for this shed
	}
	specs, err := b.c.serverCfg.Egress.ResolveProfiles(meta.EgressProfiles)
	if err != nil {
		return fmt.Errorf("egress: resolve profiles: %w", err)
	}
	if len(specs) == 0 {
		return nil
	}
	agent := b.c.newAgentClient(meta.Name)
	gateway, subnet := b.c.egressGatewaySubnet()
	if _, _, _, err := vmutil.SetupEgress(ctx, b.c.egressMgr, agent, meta.Name, meta.EgressPort, meta.EgressToken, gateway, subnet, specs, cleanup); err != nil {
		return fmt.Errorf("egress: %w", err)
	}
	return nil
}

// RunStartupHook runs ONLY the `startup` hook from provision.yaml
// (runInstall=false). Best-effort: a failure logs but doesn't fail
// the start (matches the pre-migration inline behavior).
func (b *vzStarter) RunStartupHook(ctx context.Context, metaRaw orchestrator.MetadataHandle, _ orchestrator.VMHandle) {
	meta := metaRaw.(*vzMetaHandle).meta
	agent := b.c.newAgentClient(meta.Name)
	provisioner := vmutil.NewProvisioner(agent, meta.Name)
	provisioner.SetWorkDir(meta.LandingDir)
	provisioner.SetAddDirs(config.ProjectAddDirTargets(meta.ProjectMounts, meta.LandingDir))
	provisioner.SetOutput(os.Stdout, os.Stderr)
	cfg, err := provisioner.LoadConfig(ctx)
	if err != nil {
		log.Printf("Warning: failed to load provisioning config: %v", err)
		return
	}
	if err := provisioner.RunProvisioning(ctx, cfg, false); err != nil {
		log.Printf("Warning: startup hook failed: %v", err)
	}
}

// ToShedResult mirrors vzCreator.ToShedResult — VZ's endpoint is the
// shared-NAT localhost.
func (b *vzStarter) ToShedResult(metaRaw orchestrator.MetadataHandle) *config.Shed {
	return metadataToShed(metaRaw.(*vzMetaHandle).meta, "127.0.0.1")
}
