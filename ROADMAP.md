# Shed Backlog

This file tracks follow-up ideas and improvements to consider after the current Firecracker PR merges.

## Firecracker Hardening (Deferred)

- Disable root SSH login by default in the Firecracker rootfs and switch to a non-root user workflow.
- Improve `ipToUint32` handling in `internal/firecracker/network.go` (explicit errors for invalid IPv4).
- Add deeper graceful shutdown handling for `shed-agent` connection goroutines (beyond basic timeout-driven exit).
- Evaluate shell-operator behavior for Docker/Firecracker exec and document quoting expectations.
- Consider stricter validation defaults for Firecracker configs (additional guardrails beyond `vsock_base_cid`).
- Revisit Firecracker kernel source strategy:
  - Compare Ignite (current), Firecracker-CI kernel, and custom build options for Docker compatibility (overlayfs, cgroups, iptables, etc.).
  - Verify current Docker use cases against Firecracker-CI kernel and document any missing configs.
  - Decide on default kernel source and provide an explicit fallback path (e.g., config/flag to select kernel).
  - Define an update cadence and security review notes for kernel artifacts.
  - If switching defaults, update docs and add a brief migration note.

## General Quality

- Revisit docstring coverage thresholds and expand public API documentation if needed.
