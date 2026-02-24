# Testing

This document covers Shed's testing strategy, how to run each tier of tests, and the conventions used across the codebase.

## Test Tiers

| Tier | Scope | Build Tags | Requirements | CI |
|------|-------|------------|--------------|-----|
| Unit | Pure logic, no external deps | None (or `linux` for Firecracker) | Go toolchain | Yes |
| Integration | Docker API, real containers | `integration` | Docker daemon | Yes |
| E2E (Firecracker) | Full VM lifecycle via API | `e2e` | KVM, root, Firecracker assets | No |

## Running Tests

### Unit Tests

```bash
# All unit tests (skips linux-only tests on macOS)
make test

# Single package
go test ./internal/config/...

# With race detection
go test -race ./...
```

Firecracker unit tests (metadata, rootfs, networking) use the `linux` build tag and run automatically on Linux but are skipped on macOS:

```bash
# Firecracker unit tests (Linux only)
go test ./internal/firecracker/...
```

### Integration Tests

Integration tests require a running Docker daemon and use the `integration` build tag:

```bash
make test-integration

# Or directly
go test -v -tags=integration ./integration/...
```

### E2E (Firecracker)

Firecracker e2e tests exercise the full VM lifecycle: create, start, exec, stop, delete. They require KVM access, root privileges, and pre-built Firecracker assets.

```bash
# Build prerequisites
make build
sudo scripts/build-firecracker-rootfs.sh
scripts/download-firecracker.sh

# Run e2e tests
sudo go test -v -tags=e2e ./e2e/firecracker/...
```

!!! note "Why Firecracker e2e can't run in CI"
    GitHub Actions runners do not support KVM (no nested virtualization). Firecracker requires `/dev/kvm` and root privileges to launch microVMs. These tests must be run manually on a bare-metal Linux host or a VM with nested virt enabled.

## Build Tag Conventions

| Tag | Purpose | Example |
|-----|---------|---------|
| `//go:build linux` | Code that uses Linux-only APIs (vsock, TAP, etc.) | `internal/firecracker/*.go` |
| `//go:build integration` | Tests requiring Docker | `integration/*_test.go` |
| `//go:build e2e` | Tests requiring KVM + Firecracker | `e2e/firecracker/*_test.go` |

All `internal/firecracker/` source and test files carry the `linux` build tag because they depend on Linux-specific syscalls (vsock, netlink).

## Test Patterns

### Table-Driven Tests

All test files use Go's standard table-driven pattern:

```go
tests := []struct {
    name    string
    input   string
    wantErr bool
}{
    {"valid", "my-shed", false},
    {"empty", "", true},
}

for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        err := ValidateShedName(tt.input)
        if (err != nil) != tt.wantErr {
            t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
        }
    })
}
```

### Modify Pattern for Config Validation

Config validation tests use a `modify func(*)` pattern to test individual field changes against a known-good baseline:

```go
cfg := validFirecrackerConfig()
tt.modify(cfg)
err := cfg.Validate()
```

### Test Helpers

The `internal/firecracker` package provides shared helpers in `testutil_test.go`:

| Helper | Purpose |
|--------|---------|
| `mustTempDir(t, prefix)` | Creates a temp directory with automatic cleanup |
| `testMetadata(name)` | Returns a valid `Metadata` with sensible defaults |
| `testFirecrackerConfig(tmpDir)` | Returns a valid `FirecrackerConfig` for testing |
| `createTestInstance(t, dir, name)` | Creates a complete test instance on disk |

### Conventions

- Place test files alongside the code they test (`foo.go` / `foo_test.go`)
- Use `t.Helper()` in all test helper functions
- Use `t.Cleanup()` for resource teardown instead of `defer` where possible
- Prefer `t.Fatalf` for setup failures, `t.Errorf` for assertion failures
- Use `os.MkdirTemp` with `t.Cleanup` for temporary directories

## Code Quality

```bash
# Run linter
make lint

# Format code
make fmt

# All checks (test + lint + fmt)
make check
```
