# AGENTS.md

Project-specific guidance for AI agents. See `CLAUDE.md` for the full developer
guide (build/test/lint commands, project structure, backends, storage model).

## Cursor Cloud specific instructions

The Cursor Cloud VM is **Linux x86_64 without `/dev/kvm`** (no nested
virtualization) and is **not macOS**. This constrains what can run here:

- **Builds, unit tests, and lint all work.** Use the standard `Makefile`
  targets documented in `CLAUDE.md`: `make build`, `make test`, `make lint`.
  Go 1.25 is on the system `PATH` (no `mise` needed); `golangci-lint` 2.10.1
  (the version pinned in `.mise.toml`) is installed at `/usr/local/bin`.
- **`make test` shows exactly one expected failure here:**
  `TestAllocateNetwork` in `internal/firecracker`. It is **environmental, not a
  code defect** — this VM's `eth0` holds `172.30.0.2`, which collides with
  shed's default Firecracker bridge CIDR `172.30.0.0/16`, so the allocator
  correctly skips the IP the test hard-codes as the first allocation. Every
  other test (and the SDK module under `sdk/`) passes. To confirm the rest of
  the package is green: `go test ./internal/firecracker/ -skip TestAllocateNetwork`.
- **You cannot boot a shed (`shed create`) here** — the Firecracker backend
  needs `/dev/kvm`, and the VZ backend is macOS-only. `make test-integration`,
  `scripts/e2e-test.sh`, and `scripts/smoke-test-linux.sh`'s create-cycle all
  skip/stop at this boundary (matching the project's own Linux CI, which runs
  install-only smoke). The create path is still wired and reachable up to the
  image-resolution/boot step.
- **Running `shed-server` + `shed` for API-level testing (no KVM needed):**
  the server serves HTTP/SSH fine without KVM — KVM is only required at
  create/boot time. The repo's `configs/server.dev.yaml` (and siblings)
  reference the maintainer's personal mount/env paths and resolve the backend
  to `firecracker`, which requires a `firecracker:` block. For local API
  testing, run with a minimal throwaway config that sets `pull_policy: never`,
  writable `*_dir` paths under `/tmp`, and a **non-colliding** `bridge_cidr`
  (e.g. `172.31.250.1/24`, to avoid the `172.30.0.0/16` collision above), then:
  `./bin/shed server add localhost` and `./bin/shed list`.
