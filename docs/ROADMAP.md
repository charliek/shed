# Shed Roadmap

This document outlines planned future enhancements for Shed.

## Firecracker Hardening

- Consider reducing `MaxMessageSize` (16MB) or adding streaming for large messages in agentproto.

## Shed Hardening and Orchestration

Follow-ups to service clients and the `isolated` network profile (see [Security](reference/security.md)):

- **Proxy-only profile** — isolated, with the [egress proxy](reference/egress.md) as the only way out, so domain policy becomes enforced rather than cooperative ([#205](https://github.com/charliek/shed/issues/205)).
- **No-network profile** — a shed with no network at all (the host still reaches it over vsock), for code that needs none.
- **MSS clamping** on the host's forwarding path, for container traffic on a reduced-MTU path ([#202](https://github.com/charliek/shed/issues/202)).
- **Per-certificate revocation** for service clients, by serial, instead of removing the whole client.
- **Firecracker under its `jailer`** (chroot, namespaces, privilege drop), so a VMM escape is not root on the host.
- **9P over vsock**, so a shed's server mounts are not reachable on the bridge by other open sheds.
- **Restore the bridge and NAT at `serve` start**, so creates work after a host reboot without re-running `shed-server setup` ([#413](https://github.com/charliek/shed/issues/413)).

## Notification Channel Enhancements

Future uses for the persistent agent↔host notification port (1026):

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

### Virtiofs Support for Firecracker

If Firecracker adds virtiofs ([issue #1180](https://github.com/firecracker-microvm/firecracker/issues/1180)), credentials and local-dir mounts could use virtiofs instead of 9P for improved performance. The VZ backend already uses VirtioFS via Apple's Virtualization.framework.

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

## General Quality

- Revisit docstring coverage thresholds and expand public API documentation if needed.
