# Firecracker Docker Support Debug Log

## STATUS: SOLVED

The Weave Ignite kernel (`weaveworks/ignite-kernel:5.10.51`) works with Docker containers after fixing the kernel IP configuration format.

## Goal
Get Docker containers running inside Firecracker VMs without requiring kernel compilation on each target machine.

## Problem Summary
The default Firecracker kernel (`vmlinux-5.10.217.bin` from Firecracker CI) lacks:
- `CONFIG_CGROUP_BPF` - Required by modern runc for device cgroup control
- `CONFIG_BPF` / `CONFIG_BPF_SYSCALL` - BPF subsystem
- `CONFIG_NF_TABLES` - nftables for Docker networking

Error when running containers:
```
runc create failed: unable to start container process: error during container init:
error setting cgroup config for procHooks process: bpf_prog_query(BPF_CGROUP_DEVICE)
failed: invalid argument
```

## Approaches Tried

### 1. Weave Ignite Kernel (weaveworks/ignite-kernel:5.10.51)

**Status:** FAILED - VM health check times out

**Steps:**
1. Pulled Ignite kernel image: `docker pull weaveworks/ignite-kernel:5.10.51`
2. Extracted kernel: `docker cp tmp-ignite-kernel:/boot/vmlinux-5.10.51 /tmp/ignite-vmlinux-actual`
3. Extracted config: `docker cp tmp-ignite-kernel:/boot/config-5.10.51 /tmp/ignite-config`
4. Copied to firecracker dir: `sudo cp /tmp/ignite-vmlinux-actual /var/lib/shed/firecracker/vmlinux-ignite.bin`
5. Updated server.yaml to use `vmlinux-ignite.bin`
6. Increased start_timeout to 120s

**Ignite Kernel Config (relevant parts):**
```
CONFIG_CGROUP_BPF=y      # Built-in - what we need!
CONFIG_BPF=y             # Built-in
CONFIG_BPF_SYSCALL=y     # Built-in
CONFIG_BPF_JIT_ALWAYS_ON=y
CONFIG_NETFILTER=y       # Built-in
CONFIG_NF_TABLES=m       # Module (won't work without initrd)
```

**Initial Issue:** VM fails health check - the kernel was trying DHCP for IP configuration.

**Root Cause:** The Ignite kernel has `IP_PNP` (kernel IP auto-configuration) built-in. Our simple `ip=X.X.X.X` format wasn't recognized, causing the kernel to fall back to DHCP, which times out.

**Solution:** Use full kernel IP autoconfig format:
```
ip=<client-ip>::<gateway>:<netmask>::<device>:off
```

Example: `ip=172.30.0.2::172.30.0.1:255.255.255.0::eth0:off`

The `:off` at the end disables DHCP/BOOTP.

**Status:** WORKING - Docker containers run successfully with this kernel

**Modules extracted from Ignite image:**
- Location: `/tmp/ignite-modules/5.10.51/`
- Contains nftables, netfilter, and other modules
- These require either initrd loading or manual insertion

### 2. Original Firecracker Kernel (vmlinux.bin - v4.14.174)

**Status:** WORKS - but no BPF/container support

**Kernel version:** 4.14.174 (older than expected 5.10.217)
**File:** `/var/lib/shed/firecracker/vmlinux.bin` (21MB)

This kernel boots fine and shed-agent starts, but Docker containers fail with BPF error.

## Current State

- Server config: `/etc/shed/server.yaml`
  - `kernel_path: /var/lib/shed/firecracker/vmlinux-ignite.bin`
  - `start_timeout: 120s`
- Ignite kernel: `/var/lib/shed/firecracker/vmlinux-ignite.bin` (43MB)
- Original kernel: `/var/lib/shed/firecracker/vmlinux.bin` (21MB)
- Test VM: `test-fc` created but health check failed

## Next Steps to Try

1. **Debug why Ignite kernel fails health check:**
   - Try to get console output from VM boot
   - Check if kernel boots at all
   - Verify console settings match

2. **Try Amazon Ignite kernel:**
   - `weaveworks/ignite-amazon-kernel` - might have different config

3. **Build custom kernel (Option 1 from original plan):**
   - Use Firecracker devtool
   - Start from microvm-kernel config
   - Enable BPF and nftables as built-in (=y)

4. **Try older runc without BPF requirement:**
   - Research which runc versions don't need cgroup BPF

## Environment Info

- Host: Linux 6.17.9-76061709-generic (Pop!_OS)
- Firecracker: v1.6.0
- Docker in rootfs: Ubuntu 24.04 docker.io package
- daemon.json config:
  ```json
  {
      "storage-driver": "vfs",
      "iptables": false,
      "bridge": "none",
      "exec-opts": ["native.cgroupdriver=cgroupfs"],
      "log-driver": "json-file",
      "log-opts": {
          "max-size": "10m",
          "max-file": "3"
      }
  }
  ```

## References

- Ignite project (archived): https://github.com/weaveworks/ignite
- Flintlock (successor): https://github.com/weaveworks-liquidmetal/flintlock
- Firecracker kernel docs: https://github.com/firecracker-microvm/firecracker/blob/main/docs/rootfs-and-kernel-setup.md
