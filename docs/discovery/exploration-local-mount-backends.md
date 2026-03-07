# Exploration: Local Directory Mount for VZ and Docker Backends

## Problem Statement

Currently, shed creates isolated workspaces (Docker volumes or ext4 images) and clones repositories into them. For local Mac development — particularly AI coding agent workflows (Claude Code, OpenCode) — we want the option to **mount a local host directory as the workspace** with reliable bidirectional sync, so that:

1. AI agents run inside the shed but operate on files that live on the host
2. Local IDEs (VS Code, IntelliJ) see changes made by agents immediately (with filesystem events)
3. The host directory is the source of truth — no repo clone needed
4. Both the traditional repo-clone mode and local-mount mode coexist

## Requirements

| Requirement | Priority | Notes |
|---|---|---|
| Bidirectional file sync | Must | Both sides see changes |
| Filesystem event propagation (inotify/FSEvents) | Must | IDEs and file watchers must trigger |
| Sub-second consistency | Must | Consistency > raw speed |
| Support Docker Desktop on macOS | Must | Primary Docker runtime |
| Support VZ (vfkit) on macOS | Must | Apple Silicon native |
| Coexist with repo-clone mode | Must | Per-shed choice |
| Local Mac only (shed-server on same machine) | Must | No remote mount scenario |
| Leverage existing credential sync where applicable | Should | Avoid reinventing |

---

## Current State

### Docker Backend
- Workspace: Named Docker volume (`shed-{name}-workspace`) at `/home/shed/workspace`
- Credentials: Host bind mounts (read-only or read-write)
- Bind mounts are native — Docker Desktop handles the host↔VM layer transparently

### VZ Backend
- Workspace: ext4 rootfs image, no shared filesystem
- Credentials: Bidirectional sync via vsock + fsnotify + tar archives
  - Host→VM: `CredentialTransfer` (tar over agent exec)
  - VM→Host: `CredentialNotifyListener` (fsnotify → `FileChangedMessage` → tar pull)
  - 500ms debounce, 2s echo suppression, 50MB archive / 10MB file limits
- No VirtioFS or 9p support currently
- vfkit subprocess manages VM lifecycle

### Existing Sync Infrastructure
- `internal/sync/`: Client-side SSH+tar transfer (host→container only, not bidirectional)
- `internal/vmutil/credentials_watch.go`: Host-side fsnotify watcher with debounce
- `internal/vmutil/credentials_notify.go`: VM→host notification listener with security validation
- `cmd/shed-agent/notify.go`: Guest-side fsnotify watcher for credential directories
- `internal/agentproto/`: Framed message protocol over vsock (16MB max frame)

---

## Approach 1: Docker Bind Mount (Docker Backend)

### How It Works
Docker Desktop on macOS already supports bind-mounting host directories into containers. Modern Docker Desktop defaults to **VirtioFS** for file sharing, which provides:
- Near-native performance
- Proper inotify event propagation into the container
- Bidirectional — writes in container appear on host and vice versa

### Implementation
Replace the named volume with a bind mount when a local directory is specified:

```go
// In internal/docker/containers.go, during container creation:
if opts.LocalDir != "" {
    // Bind mount instead of named volume
    mounts = append(mounts, mount.Mount{
        Type:   mount.TypeBind,
        Source: opts.LocalDir,    // e.g., /Users/charlie/projects/myapp
        Target: "/home/shed/workspace",
    })
} else {
    // Existing behavior: named volume + optional repo clone
    mounts = append(mounts, mount.Mount{
        Type:   mount.TypeVolume,
        Source: volumeName,
        Target: "/home/shed/workspace",
    })
}
```

### Tradeoffs

| Pro | Con |
|---|---|
| Simplest approach — Docker handles everything | Performance depends on Docker Desktop's file sharing backend |
| Full inotify propagation built-in | macOS UID/GID mapping can cause permission issues |
| No new code for sync | `.git` and `node_modules` performance can be poor on large repos |
| Battle-tested by millions of developers | No isolation — container writes directly to host filesystem |

### Performance Notes
- Docker Desktop VirtioFS: Good general performance, inotify works
- For large repos with many small files (node_modules), performance degrades
- Can mitigate with `.dockerignore`-style excludes or selective mounting
- Docker Desktop's `synchronized file shares` (beta) uses Mutagen under the hood for better performance on large trees

### Permission Handling
- Docker Desktop on Mac maps the macOS user to the container user
- May need `--user` flag or chown in entrypoint to match the `shed` user (UID 1000)
- Alternative: use `userns-remap` or Docker's built-in user namespace mapping

### Estimated Complexity
**Low.** Mostly configuration — the Docker API already supports this. Main work is:
1. Add `LocalDir` option to shed creation
2. Conditionally use bind mount vs. volume
3. Handle UID/GID mapping
4. Skip repo clone when local dir is specified

---

## Approach 2: VirtioFS Shared Directory (VZ Backend)

### How It Works
Apple's Virtualization.framework supports **VirtioFS** for sharing host directories with VMs. vfkit (which shed already uses) supports this via:

```
--device virtio-fs,sharedDir=/path/on/host,mountTag=workspace
```

Inside the VM, the guest mounts it:
```bash
mount -t virtiofs workspace /home/shed/workspace
```

### Implementation

1. **vfkit launch args** (`internal/vz/vm.go`):
   ```go
   if opts.LocalDir != "" {
       args = append(args,
           "--device", fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=workspace", opts.LocalDir),
       )
   }
   ```

2. **Guest mount** (in shed-agent or VM init):
   ```bash
   mkdir -p /home/shed/workspace
   mount -t virtiofs workspace /home/shed/workspace
   ```

3. **Kernel requirements**: The VM kernel must have `CONFIG_VIRTIO_FS` enabled. Need to verify the current shed VM kernel supports this.

### Tradeoffs

| Pro | Con |
|---|---|
| Kernel-level sharing — best possible performance | Requires kernel support in VM image |
| Full inotify propagation (virtiofs supports it) | macOS-only (tied to Virtualization.framework) |
| No userspace sync overhead | UID/GID mapping between host and guest |
| No file size limits | Less battle-tested than Docker bind mounts |
| vfkit already supports it | Need to handle mount lifecycle in guest agent |

### Performance Notes
- VirtioFS on Apple Silicon is highly optimized — Apple uses it for their own Rosetta Linux support
- Significantly faster than 9p for metadata-heavy workloads
- inotify events propagate through virtiofs to the guest kernel
- FSEvents on the host side work natively since the files are on the host filesystem

### UID/GID Considerations
- Host files are owned by the macOS user (typically UID 501)
- Guest `shed` user is UID 1000
- Options:
  - Map UIDs in virtiofs (if supported by vfkit)
  - Run guest processes as the mapped UID
  - Use `idmapped mounts` in the guest kernel (Linux 5.12+)

### Estimated Complexity
**Medium.** The vfkit plumbing is straightforward, but:
1. Verify/rebuild VM kernel with virtiofs support
2. Add mount logic to shed-agent startup
3. Handle UID mapping
4. Ensure proper cleanup on VM stop/destroy

---

## Approach 3: Extend Credential Sync to Workspace (VZ Backend)

### How It Works
Reuse the existing bidirectional credential sync infrastructure (`fsnotify` + tar over vsock) but apply it to the entire workspace directory instead of just credentials.

### Implementation
Conceptually: add the local directory as a "credential" mount that happens to be the workspace:

```yaml
credentials:
  workspace:
    source: ~/projects/myapp
    target: /home/shed/workspace
    readonly: false
    exclude:
      - ".git/objects/**"
      - "node_modules/**"
      - "*.o"
      - "*.pyc"
```

The existing `CredentialWatcher` (host→VM) and `CredentialNotifyListener` (VM→host) would handle sync.

### What Would Need to Change

1. **Remove file size limits**: Current 10MB/file and 50MB/archive limits are too small for workspace files
2. **Improve throughput**: Tar-based transfer works for small credential files but would bottleneck on large source trees
3. **Reduce debounce**: 500ms + 2s echo suppression = potential 2.5s round-trip latency
4. **Initial sync**: Need efficient initial sync of potentially large directories (current credential transfer is designed for small file sets)
5. **Conflict resolution**: Credentials are mostly unidirectional in practice; workspace sync will have true concurrent writes
6. **inotify propagation**: Currently NO filesystem events propagate — only file content syncs. The guest agent would need to synthesize inotify events after writing files, which is fragile and incomplete (tools watching for directory creation events, for example, won't work reliably)

### Tradeoffs

| Pro | Con |
|---|---|
| Leverages proven, existing code | Not designed for workspace-scale data |
| No kernel changes needed | No native filesystem events — must synthesize |
| Works with current VM image | Higher latency (debounce + tar + transfer) |
| Familiar codebase | Conflict resolution is hard |
| | Poor performance on bulk operations (git checkout, npm install) |
| | Echo suppression adds unavoidable latency |

### Estimated Complexity
**High** to do well. The credential sync works because credentials are small, infrequently changing, and mostly unidirectional. Workspace sync inverts all of those assumptions:
- Large file trees (thousands of files)
- Frequent changes (AI agents + builds)
- True bidirectional concurrent access
- Need for filesystem event fidelity

---

## Approach 4: Mutagen-Based Sync (VZ Backend)

### How It Works
[Mutagen](https://mutagen.io/) is a purpose-built tool for high-performance bidirectional file sync. Docker Desktop uses it internally for their "synchronized file shares" feature.

### Implementation
1. Bundle or require mutagen binary
2. On shed creation with local dir, start a mutagen sync session:
   ```
   mutagen sync create \
     ~/projects/myapp \
     shed-vm:/home/shed/workspace \
     --sync-mode=two-way-resolved
   ```
3. Mutagen handles:
   - Efficient delta transfer (rsync-like)
   - Conflict resolution (configurable policies)
   - Filesystem watching on both ends
   - Reconnection on transient failures

### Transport for VZ
Mutagen typically uses SSH or Docker as transport. For VZ VMs:
- Option A: Expose SSH from VM (already done for shed access)
- Option B: Write a custom mutagen transport over vsock
- Option C: Use mutagen's `--transport-socket` with a vsock-to-unix-socket bridge

### Tradeoffs

| Pro | Con |
|---|---|
| Purpose-built for this exact problem | External dependency |
| Handles conflicts, reconnection, large trees | Another binary to bundle/manage |
| Efficient delta sync (doesn't re-transfer unchanged files) | Adds complexity to lifecycle management |
| Used by Docker Desktop internally | inotify propagation depends on mutagen's flush timing |
| Good performance even over high-latency links | Learning curve for configuration |

### inotify Consideration
Mutagen writes files to the destination filesystem normally, so inotify events fire on the guest side when mutagen writes. However, there's a sync interval — events are batched, not instant. Typically sub-second for small changes.

### Estimated Complexity
**Medium.** Mutagen handles the hard parts, but integration requires:
1. Bundling/detecting mutagen
2. Lifecycle management (start/stop sync sessions with shed)
3. Transport configuration for VZ
4. Error handling and status reporting

---

## Comparison Matrix

| Criterion | Docker Bind Mount | VZ VirtioFS | Extend Cred Sync | Mutagen |
|---|---|---|---|---|
| **Backend** | Docker | VZ | VZ | VZ |
| **Sync latency** | Instant | Instant | 500ms-2.5s | ~200-500ms |
| **inotify fidelity** | Full | Full | Synthesized (fragile) | Natural (on write) |
| **Large file performance** | Good (VirtioFS) | Excellent | Poor | Good |
| **Bulk operations** | Good | Excellent | Poor | Good |
| **Implementation effort** | Low | Medium | High | Medium |
| **External dependencies** | Docker Desktop | vfkit (existing) | None | mutagen binary |
| **Conflict handling** | N/A (shared FS) | N/A (shared FS) | Manual | Built-in |
| **Battle-tested** | Very | Moderate | No (credentials only) | Yes |
| **Kernel changes needed** | No | Maybe (virtiofs module) | No | No |

---

## Recommendation

### Phase 1: Docker Bind Mount + VZ VirtioFS
These two approaches give the best user experience with the least complexity:

1. **Docker**: Bind mount is trivial to implement and "just works" on Docker Desktop with VirtioFS. This could ship quickly.

2. **VZ**: VirtioFS is the right long-term answer — it gives kernel-level shared filesystem with full event fidelity and no sync latency. The main risk is whether the current VM kernel supports it.

### Phase 2 (if VirtioFS doesn't work): Mutagen for VZ
If the VM kernel lacks virtiofs support or UID mapping proves intractable, mutagen is a proven fallback that handles workspace-scale sync well.

### Not Recommended: Extending Credential Sync
The credential sync system is elegant for its purpose but fundamentally wrong for workspace-scale bidirectional sync. The absence of native filesystem events alone is a dealbreaker for IDE integration. It would require essentially rewriting the system into something that looks like mutagen anyway.

---

## UX Design Sketch

### CLI
```bash
# Create a shed with local directory mount (no repo clone)
shed create myproject --local-dir .
shed create myproject --local-dir ~/projects/myapp

# Create a shed with traditional repo clone (existing behavior)
shed create myproject --repo owner/repo

# The --local-dir flag implies:
#   - No repo clone
#   - Bidirectional mount/sync of the specified directory
#   - Backend chooses best available strategy (bind mount, virtiofs, etc.)
```

### Config
```yaml
# server.yaml additions
workspace:
  # Default mount strategy per backend
  docker:
    mount_type: bind          # bind | volume (default: volume for repo, bind for local-dir)
  vz:
    mount_type: virtiofs      # virtiofs | sync (default: virtiofs)
    # Fallback if virtiofs unavailable
    sync_fallback: mutagen    # mutagen | credential-style | none
```

### API
```json
// POST /sheds - create request
{
  "name": "myproject",
  "backend": "docker",
  "local_dir": "/Users/charlie/projects/myapp"
  // "repo" field omitted — mutually exclusive with local_dir
}
```

---

## Open Questions

1. **VM Kernel**: Does the current shed VM kernel (vmlinux) have `CONFIG_VIRTIO_FS=y`? If not, what's the rebuild process?
2. **UID Mapping**: What's the best approach for mapping macOS UID 501 → guest UID 1000 across both backends?
3. **Excludes**: Should we support `.gitignore`-style excludes for mounted directories (e.g., skip `node_modules`, `.git/objects`)?
4. **Multiple Mounts**: Should a shed support mounting multiple local directories, or is one workspace mount sufficient?
5. **Lifecycle**: When a shed is stopped/destroyed, should the local directory be left untouched (yes, obviously), but should any shed-specific files (`.shed/`, build artifacts) be cleaned up?
6. **Mixed Mode**: Could a shed mount a local directory AND have additional volume storage for build caches, dependencies, etc.?
