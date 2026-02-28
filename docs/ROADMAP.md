# Shed Roadmap

This document outlines planned future enhancements for Shed.

## Firecracker Hardening (Deferred)

Follow-up improvements to consider after the initial Firecracker backend merges.

- ~~Disable root SSH login by default in the Firecracker rootfs and switch to a non-root user workflow.~~ **Done** — shed-agent now runs commands as the `shed` user (UID 1000), matching the Docker backend's non-root model.
- ~~Add deeper graceful shutdown handling for `shed-agent` connection goroutines (beyond basic timeout-driven exit).~~ **Done** — `Stop()` now uses a 5s drain timeout so hung connections don't block VM shutdown.
- ~~Consider stricter validation defaults for Firecracker configs (additional guardrails beyond `vsock_base_cid`).~~ **Done** — upper-bound validation added for CPUs, memory, disk, vsock CID/ports, and timeouts; kernel/rootfs paths checked at startup.
- ~~Add metadata JSON version field for forward/backward compatibility of the metadata format.~~ **Done** — `MetadataVersion = 1`, backward-compat for pre-version files.
- Consider reducing `MaxMessageSize` (16MB) or adding streaming for large messages in agentproto.
- ~~Document integration test strategy (requires KVM, can't be automated in CI without nested virtualization).~~ **Done** — `docs/development/testing.md` covers unit, integration, and e2e tiers.

## Firecracker Graceful Shutdown (Deferred)

Remaining improvement for stop/start resilience after most e2e gaps were resolved:

- Boot cleanup handles generic stale state (shared memory, lock files, temp files) via `network-setup.sh`; service-specific cleanup moved to startup hooks (documented in provisioning guide)
- ~~Still deferred: configurable stop timeout, pre-shutdown hooks for graceful service termination~~ **Done** — `hooks.shutdown` in `provision.yaml` runs before `shed stop`/`shed delete`, with time budget of `min(stopTimeout/2, 30s)`

## Bidirectional Credential Sync

Event-driven bidirectional sync for writable credential mounts (e.g., `gh`, `claude`, `opencode`). Changes inside a VM (token refreshes) sync back to the host, and host-side changes push to all running VMs.

**How it works:**
- Agent runs `fsnotify` watchers on writable credential target paths inside the VM
- On file change, agent sends a `MsgTypeFileChanged` notification to the host over a persistent vsock connection (port 1026)
- Host pulls just the changed files via tar-over-vsock and writes them to the host source directory
- Host runs its own `fsnotify` watcher on credential source directories
- Host-side changes push to all running VMs via the existing `transferCredential()` mechanism
- Echo suppression (2s cooldown) prevents changes from bouncing back to the originating VM

**Status:** Complete. Merged in PR #16.

**Architecture note:** The SSHFS-over-vsock approach previously considered here was superseded by this event-driven design. SSHFS would have required FUSE in the kernel, `sshfs` in the rootfs, and an sftp-server per mount. The notification approach is lighter weight and only transfers changed files.

## Notification Channel Enhancements (Deferred)

Future uses for the persistent agent↔host notification port (1026) established by credential sync:

- **Health heartbeats over notification channel** — replace the current 500ms polling during VM startup (`WaitForHealth`) with agent-pushed heartbeats. Eliminates repeated connection open/close cycles.
- **Agent-pushed resource metrics** — CPU/memory/disk usage pushed from agent at configurable intervals. Enables `shed status` to show live resource usage without exec overhead.
- **Process event notifications** — agent notifies host when provisioning hooks finish, services crash, or long-running processes exit. Enables reactive orchestration.
- **Log streaming** — structured log events from inside the VM pushed over the notification channel. Alternative to SSH-based log tailing.
- **Provisioning pipeline over persistent connection** — consolidate the sequential exec calls during provisioning into a single persistent connection to reduce vsock connection overhead.

## Other Potential Enhancements

### GPU Passthrough Support

Enable GPU access in Firecracker VMs for ML/AI workloads. This would require:
- VFIO-based GPU passthrough
- Driver installation in rootfs
- Resource allocation management

### Snapshot/Restore

Enable fast VM startup using snapshots:
- Pre-boot snapshots for instant start
- User-triggered snapshots for state preservation
- Snapshot management commands

### Resource Limits

Enhanced resource management:
- CPU quota/throttling
- Memory overcommit policies
- I/O bandwidth limits
- Network rate limiting

### Virtiofs Support

If Firecracker adds virtiofs ([issue #1180](https://github.com/firecracker-microvm/firecracker/issues/1180)), replace the tar-over-vsock credential sync with proper filesystem passthrough for live mounts. Would eliminate the need for the notification channel approach for file sync, though the channel remains valuable for other agent→host communication.

### Intel macOS VZ Support

Expand the VZ backend beyond Apple Silicon to support Intel macOS hosts.

- Add architecture-aware VZ rootfs build support (`linux/amd64` path in `scripts/build-vz-rootfs.sh`)
- Validate vfkit + kernel boot flow on Intel Macs
- Add Intel-specific setup and troubleshooting documentation

### Multi-node Sheds

Support for distributed development environments:
- Multiple VMs working together
- Shared networking
- Service discovery
- Orchestration integration

## Deferred Upgrades

(No items currently deferred.)

## General Quality

- Revisit docstring coverage thresholds and expand public API documentation if needed.
