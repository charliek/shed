# Shed Backlog

This file tracks follow-up ideas and improvements to consider after the current Firecracker PR merges.

## Firecracker Hardening (Deferred)

- Disable root SSH login by default in the Firecracker rootfs and switch to a non-root user workflow.
- Improve `ipToUint32` handling in `internal/firecracker/network.go` (explicit errors for invalid IPv4).
- Add deeper graceful shutdown handling for `shed-agent` connection goroutines (beyond basic timeout-driven exit).
- Evaluate shell-operator behavior for Docker/Firecracker exec and document quoting expectations.
- Consider stricter validation defaults for Firecracker configs (additional guardrails beyond `vsock_base_cid`).
- Revisit Firecracker kernel source strategy (Ignite vs Firecracker-CI/custom build) to preserve Docker support while improving maintainability.

## General Quality

- Revisit docstring coverage thresholds and expand public API documentation if needed.
