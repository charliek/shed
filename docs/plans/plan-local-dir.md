# Implementation Plan: `--local-dir` Flag

## Overview

Add a `--local-dir` flag to `shed create` that mounts a host directory as the workspace instead of creating a volume/cloning a repo. Supports Docker (bind mount), VZ (VirtioFS), and Firecracker (not implemented error for now).

---

## Step 1: Add `LocalDir` to `CreateShedRequest`

**File:** `internal/config/types.go`

Add `LocalDir` field to the request struct:

```go
type CreateShedRequest struct {
    Name        string `json:"name"`
    Repo        string `json:"repo,omitempty"`
    Image       string `json:"image,omitempty"`
    NoProvision bool   `json:"no_provision,omitempty"`
    Backend     string `json:"backend,omitempty"`
    CPUs        int    `json:"cpus,omitempty"`
    MemoryMB    int    `json:"memory_mb,omitempty"`

    // LocalDir mounts a host directory as the workspace instead of
    // creating a volume. Mutually exclusive with Repo.
    LocalDir string `json:"local_dir,omitempty"`
}
```

Also add `LocalDir` to the `Shed` response struct so clients can see whether a shed uses a local dir:

```go
type Shed struct {
    // ... existing fields ...
    LocalDir string `json:"local_dir,omitempty" yaml:"local_dir,omitempty"`
}
```

---

## Step 2: Add `--local-dir` CLI Flag

**File:** `cmd/shed/shed.go`

Add flag variable and registration:

```go
var createLocalDir string

// In init():
createCmd.Flags().StringVar(&createLocalDir, "local-dir", "", "Mount a local directory as the workspace (mutually exclusive with --repo)")
```

Add validation in `runCreate()`:
- `--local-dir` and `--repo` are mutually exclusive — error if both set
- Resolve `.` and relative paths to absolute paths using `filepath.Abs()`
- Verify the directory exists on the host

Wire it into the `CreateShedRequest`:
```go
req := &config.CreateShedRequest{
    // ... existing fields ...
    LocalDir: createLocalDir,
}
```

---

## Step 3: Server-Side Validation

**File:** `internal/api/handlers.go`

In `handleCreateShed()`, add validation:
- If both `req.Repo` and `req.LocalDir` are set, return 400 error
- If `req.LocalDir` is set, validate it's an absolute path
- If `req.LocalDir` is set, verify the directory exists on the server (call `os.Stat`)

---

## Step 4: Docker Backend — Bind Mount

**File:** `internal/docker/client.go`

Modify `buildMounts()` to accept `localDir string` parameter:

```go
func (c *Client) buildMounts(shedName string, localDir string) []mount.Mount {
    mounts := make([]mount.Mount, 0, len(c.config.Credentials)+1)

    if localDir != "" {
        // Bind mount host directory as workspace
        mounts = append(mounts, mount.Mount{
            Type:   mount.TypeBind,
            Source: localDir,
            Target: config.WorkspacePath,
        })
    } else {
        // Existing: named volume
        mounts = append(mounts, mount.Mount{
            Type:   mount.TypeVolume,
            Source: config.VolumeName(shedName),
            Target: config.WorkspacePath,
        })
    }

    // ... credential mounts unchanged ...
    return mounts
}
```

**File:** `internal/docker/containers.go`

In `CreateShed()`:
1. Pass `req.LocalDir` to `buildMounts(req.Name, req.LocalDir)`
2. Skip volume creation when `localDir != ""` (no `CreateVolume` call)
3. Skip repo clone when `localDir != ""` (the directory IS the workspace)
4. Still run `fixWorkspaceOwnership` — bind-mounted dirs may need chown
5. Store `localDir` in container labels for later retrieval: `labels[config.LabelShedLocalDir] = req.LocalDir`

In `DeleteShed()`:
- Skip volume deletion when the shed used a local dir (check the label)
- The local directory itself is NEVER deleted

In `containerToShed()` / `inspectToShed()`:
- Read `LabelShedLocalDir` label and populate `shed.LocalDir`

Add new label constant in `internal/config/types.go`:
```go
LabelShedLocalDir = "shed.local_dir"
```

---

## Step 5: VZ Backend — VirtioFS

**File:** `internal/vz/vm.go`

In `buildVfkitArgs()`, accept `localDir` from metadata and add VirtioFS device:

```go
if vm.meta.LocalDir != "" {
    args = append(args,
        "--device", fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=workspace", vm.meta.LocalDir),
    )
}
```

**File:** `internal/vz/metadata.go`

Add `LocalDir` field to `Metadata`:
```go
type Metadata struct {
    // ... existing fields ...
    LocalDir string `json:"local_dir,omitempty"`
}
```

**File:** `internal/vz/client.go`

In `CreateShed()`:
1. Set `meta.LocalDir = req.LocalDir`
2. Skip repo clone when `localDir != ""`
3. After VM starts, mount VirtioFS inside the guest via agent exec:
   ```bash
   sudo mkdir -p /home/shed/workspace && sudo mount -t virtiofs workspace /home/shed/workspace && sudo chown shed:shed /home/shed/workspace
   ```
4. Populate `LocalDir` in the returned `config.Shed`

In `StartShed()`:
- The VirtioFS device is part of the VM config (via metadata), so it's re-added automatically on start
- Re-run the guest-side mount command after VM start (VirtioFS mounts don't persist across reboots)

In `GetShed()` / `ListSheds()`:
- Populate `LocalDir` from metadata

**Guest kernel requirement**: The VM kernel must have `CONFIG_VIRTIO_FS=y`. If the mount command fails, log a clear error explaining that the kernel may lack VirtioFS support.

---

## Step 6: Firecracker Backend — Not Implemented

**File:** `internal/firecracker/client.go`

In `CreateShed()`, add an early check:

```go
if req.LocalDir != "" {
    return nil, fmt.Errorf("--local-dir is not supported on the firecracker backend (planned for future release)")
}
```

This gives a clear error message without blocking the feature on other backends.

---

## Step 7: API Client (CLI side)

**File:** `internal/config/client.go` (or wherever the API client lives)

Ensure `CreateShedRequest` with `LocalDir` is properly serialized/deserialized over the HTTP API. Since we're adding a JSON field to an existing struct, this should work automatically.

---

## Step 8: Shed Response Display

**File:** `cmd/shed/shed.go`

In `runList()`, show the local dir in verbose output:
- Tier 2 (`-v`): Add `LOCAL_DIR` column
- Tier 3 (`-vv`): Show `Local Dir: /path/to/dir` in key-value output
- JSON output: Already included via the struct field

---

## Step 9: Tests

### Unit Tests

1. **`internal/config/types_test.go`**: Validate `LocalDir` and `Repo` mutual exclusion
2. **`internal/docker/client_test.go`**: Test `buildMounts()` returns bind mount when `localDir` is set, volume mount when empty
3. **`internal/vz/vm_test.go`**: Test `buildVfkitArgs()` includes `virtio-fs` device when `LocalDir` is set in metadata
4. **`internal/api/handlers_test.go`**: Test 400 error when both `repo` and `local_dir` are provided

### Integration Tests

5. **Docker**: Create shed with `--local-dir /tmp/test-workspace`, verify file written on host appears in container and vice versa
6. **VZ**: Create shed with `--local-dir`, verify VirtioFS mount and bidirectional file visibility
7. **Firecracker**: Verify `--local-dir` returns "not implemented" error

---

## File Change Summary

| File | Change |
|---|---|
| `internal/config/types.go` | Add `LocalDir` to `CreateShedRequest` and `Shed`, add `LabelShedLocalDir` |
| `cmd/shed/shed.go` | Add `--local-dir` flag, validation, mutual exclusion with `--repo` |
| `internal/api/handlers.go` | Validate `local_dir` in create handler |
| `internal/docker/client.go` | Modify `buildMounts()` to accept and use `localDir` |
| `internal/docker/containers.go` | Conditional volume/clone skip, label storage, read label in converters |
| `internal/vz/vm.go` | Add VirtioFS device in `buildVfkitArgs()` |
| `internal/vz/metadata.go` | Add `LocalDir` field |
| `internal/vz/client.go` | Guest-side mount, skip clone, populate response |
| `internal/firecracker/client.go` | Return "not implemented" error |

---

## Open Questions / Risks

1. **VZ kernel VirtioFS support**: Need to verify the current shed VM kernel has `CONFIG_VIRTIO_FS=y`. If not, we need to rebuild the kernel with it enabled. This is the biggest risk for the VZ path.

2. **UID mapping (VZ)**: macOS files are owned by UID 501, guest `shed` user is UID 1000. VirtioFS may expose files as the wrong user. Options:
   - `chown` on mount (fragile, modifies host files)
   - Linux `idmapped mounts` (requires kernel 5.12+)
   - Run guest processes with matching UID
   - Investigate if vfkit supports UID mapping options

3. **UID mapping (Docker)**: Docker Desktop on Mac typically handles this transparently, but worth testing with the `shed` user (UID 1000).

4. **Path resolution**: `--local-dir .` must resolve to an absolute path on the **client side** before sending to the server. If client and server are on different machines, the path must exist on the server. Consider whether to support relative paths at all, or require absolute.

5. **`--keep-volume` on delete**: When a shed uses `--local-dir`, the `--keep-volume` flag is irrelevant (there is no volume). Should we warn, or silently ignore?
