# Docker-Free Builds

Design notes for removing the Docker daemon dependency from
`shed image build`.

## 1. Current State

The image lifecycle is half daemon-free:

| Operation | Daemon needed? | Implementation |
|---|---|---|
| `shed image pull` | No | `go-containerregistry` |
| `shed image push` | No | `go-containerregistry` |
| `shed image save` / `load` | No | Direct blob I/O |
| `shed image inspect` / `ls` / `history` / `tag` / `rm` / `prune` | No | Direct blob I/O |
| `shed image build` | **Yes** | `docker buildx build` subprocess |

Build is the remaining gap. `shed image build` shells out to `docker
buildx build`, which requires:

- A Docker daemon (`docker.sock`) reachable by the user running the
  build.
- `buildx` configured (almost always already installed alongside Docker
  CE on Linux and Docker Desktop on macOS).
- Sufficient privileges to run buildx containers.

This is mildly annoying for:

- **Cloud VPSes** running `shed-server` for Firecracker that otherwise
  don't need Docker at all (the in-guest Docker is built into the
  rootfs).
- **CI workers** running shed images through `shed image build` in
  pipelines that try to avoid pulling Docker.
- **`shed-server` setup scripts** that today install Docker CE
  primarily for `shed image build` and the legacy host-side
  rootfs conversion.

The `--builder` flag was added to `shed image build` in v0.5 as
scaffolding: it accepts `docker`, `podman`, and `buildah`, but only
`docker` is wired up. The other two are placeholders for the work
described here.

## 2. Approach 1: Pluggable Builder (Recommended)

**Idea:** `shed image build` selects a builder backend at runtime
based on `--builder` or auto-detection. Each backend emits OCI output
to stdout / to a file / to a local registry, and shed lands it in the
OCI store with the same `InstallBlobs` path the daemon-free `pull`
already uses.

### Why this works

All three candidate builders (Docker BuildKit, Podman, Buildah) speak
OCI output natively. The piece shed needs is a manifest + config + layer
blobs, which any of them can produce with `--output type=oci-archive`
or equivalent.

### Backend matrix

| Builder | OCI output flag | Daemon? | Rootless? |
|---|---|---|---|
| `docker buildx` | `--output type=oci,dest=…` | Yes (Docker CE) | Optional |
| `podman build` | `--output type=oci,dest=…` | No | Yes |
| `buildah bud` | `--output type=oci-archive,dest=…` | No | Yes |
| `nerdctl build` | `--output type=oci,dest=…` | No (containerd) | Yes |

Podman is the easiest first port — feature parity with `docker buildx`
for the Dockerfiles shed cares about and a clean rootless story on
Linux.

### CLI shape

```text
shed image build [flags] [context]

  --builder string   Builder backend: docker | podman | buildah  (default: auto-detect)
  --file    string   Dockerfile path
  --name    string   Tag to advance after install
  --target  string   Build target stage
```

Auto-detection order: `docker` → `podman` → `buildah` → `nerdctl`.

### Implementation outline

1. Define a `Builder` interface in `internal/image`:
   ```go
   type Builder interface {
       Name() string
       Available(ctx context.Context) (bool, error)
       Build(ctx context.Context, opts BuildOptions) (ociArchivePath string, err error)
   }
   ```
2. Implement `dockerBuildx`, `podman`, `buildah` adapters. Each shells
   out to its binary with `--output type=oci-archive,dest=$TMPFILE`.
3. Wire `shed image build` to pick an adapter via `--builder`, then run
   the existing `LoadOCIArchive` path that `shed image load` already
   uses.

### Milestones

| Milestone | Scope |
|---|---|
| M1 | `--builder` flag accepted, only `docker` works (shipped in v0.5). |
| M2 | `podman` backend. Linux first; macOS via Podman Machine. |
| M3 | `buildah` backend. |
| M4 | Auto-detection. Drop the daemon-CE requirement from `shed-server setup`. |
| M5 | (Stretch) `nerdctl` backend for containerd-only hosts. |

## 3. Approach 2: BuildKit as a Library

**Idea:** Vendor BuildKit's frontend and run it in-process. No external
builder binary, no daemon — `shed-server` becomes self-sufficient.

### Pros

- Single binary. No `docker`, `podman`, `buildah` install requirement.
- Tight integration: progress events, caching, and OCI output all
  controlled in-process.
- Build path shares code with future shed features that might want
  programmatic image production (e.g. CI baking).

### Cons

- **Binary size.** BuildKit pulls in containerd, runc, and a chunk of
  OCI tooling. `shed-server` is currently ~30 MB; vendoring BuildKit
  pushes it into the hundreds of MB range.
- **API churn.** BuildKit doesn't promise a stable Go API. Every shed
  release would carry vendor-update risk.
- **Harder debugging.** When a build fails, users currently
  recognize `docker buildx` output. In-process BuildKit produces
  similar but not identical error surfaces.
- **Privilege requirements.** BuildKit still needs `runc` and a chroot,
  which means either rootless namespaces (Linux-only, fragile on older
  kernels) or root.

### Verdict

Not worth it for v1. Reconsider if Approach 1 fails to materialize a
viable rootless path on macOS.

## 4. Trade-Offs

| Aspect | Approach 1 (Pluggable) | Approach 2 (BuildKit lib) |
|---|---|---|
| Binary size | Unchanged | +200–400 MB |
| Setup complexity | Install one of {docker, podman, buildah} | None |
| Debugging | Familiar (subprocess output) | New (in-process logs) |
| Maintenance | Adapter per builder | Vendor lock-in on BuildKit Go API |
| Time to ship | Low (M2 in a sprint) | High (multi-release effort) |

## 5. Recommended Path

1. **Today:** `--builder docker` works; `podman` / `buildah` are
   placeholders (M1, shipped in v0.5).
2. **Next release:** Implement `--builder podman`. This gets the
   daemon-free build story on Linux with one well-tested adapter.
3. **Release after:** `--builder buildah` for sites that prefer it.
4. **Stretch:** auto-detect, then remove the Docker CE install step
   from `shed-server setup`.
5. **Defer:** in-process BuildKit. Only revisit if the adapter path
   hits a wall.

## 6. Open Questions

- **macOS without Docker Desktop:** Podman Machine works but spins up a
  Linux VM under the hood, which is essentially the same overhead as
  Docker Desktop. For VZ users we likely need to accept "Docker
  Desktop or Podman Machine" as the macOS build prerequisite even
  post-M4.
- **Build cache sharing:** `docker buildx` and `podman build` keep
  independent caches. Switching `--builder` per build will be slow
  until the cache warms in the new tool. Not a correctness problem;
  call out in docs.
- **Multi-platform builds:** `docker buildx build --platform
  linux/arm64,linux/amd64` is common today. Podman supports it but the
  UX is rougher. Document the difference; don't try to paper over it.
