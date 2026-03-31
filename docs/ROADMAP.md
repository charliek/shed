# Shed Roadmap

This document outlines planned future enhancements for Shed.

## Firecracker Hardening

- Consider reducing `MaxMessageSize` (16MB) or adding streaming for large messages in agentproto.

## Notification Channel Enhancements

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

## Image Publishing

- **CI workflow for image publishing** ([#36](https://github.com/charliek/shed/issues/36)) — Automate `scripts/publish-vz-images.sh` and `scripts/publish-fc-images.sh` via GitHub Actions on release tags.

## General Quality

- Revisit docstring coverage thresholds and expand public API documentation if needed.
