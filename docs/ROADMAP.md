# Shed Roadmap

This document outlines planned future enhancements for Shed.

## Firecracker Live Mounts (SSHFS over vsock)

### Use Case

Some workflows need live two-way sync between host and VM:
- Editable credential files that may be refreshed during a session
- Shared project files that need host-side tooling access
- Development workflows where host IDE edits should reflect immediately in VM

### Current Behavior

Credentials are copied at create/start time via tar over vsock. This works well for:
- SSH keys (read-only)
- Git config (read-only)
- AI agent credentials (read-only)

The copy approach is the default since most credential use cases are read-only. Changes on the host after VM starts won't be reflected in the VM, and changes in the VM won't sync back to the host.

### Proposed Solution: SSHFS over vsock

Since Firecracker doesn't support virtiofs or 9p filesystem passthrough, the best alternative for live mounts is SSHFS over vsock.

**Architecture:**
```
Host: socat VSOCK-LISTEN:12345,fork EXEC:"/usr/lib/openssh/sftp-server"
Guest: sshfs -o vsock=2:12345 unused_host:/path /mount/point
```

**Requirements:**
1. Kernel FUSE support (verify CONFIG_FUSE_FS=y in kernel)
2. Add `sshfs` to rootfs image
3. Start sftp-server listener on host for each credential mount
4. Mount management in shed-agent (mount on start, unmount on stop)

**Key Research:**
- https://github.com/firecracker-microvm/firecracker/issues/889
- https://github.com/firecracker-microvm/firecracker/issues/1180
- Performance: SSHFS has moderate overhead vs local FS

**Complexity:** Medium - no Firecracker changes needed, uses existing vsock

### Priority

Low - current copy approach works for most credential use cases. This would be implemented if users request live sync functionality.

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

### Multi-node Sheds

Support for distributed development environments:
- Multiple VMs working together
- Shared networking
- Service discovery
- Orchestration integration
