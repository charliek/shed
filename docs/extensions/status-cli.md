# Status CLI

`shed-ext status` is an in-VM health check tool that reports the connectivity status of each credential namespace.

## Usage

```bash
shed-ext status
```

## Output

### All Connected

```text
ssh-agent:       ✓ connected (agent-forward mode, 3 keys available)
aws-credentials: ✓ connected (role: arn:aws:iam::123:role/dev, cached until 15:45 UTC)
```

### Partially Connected

```text
ssh-agent:       ✓ connected (local-keys mode, 1 key available)
aws-credentials: ✗ not connected (shed-host-agent not responding)

Hint: ensure shed-host-agent is running on the host machine.
```

### Not Connected

```text
ssh-agent:       ✗ not connected (shed-host-agent not responding)
aws-credentials: ✗ not connected (shed-host-agent not responding)

Hint: ensure shed-host-agent is running on the host machine.
```

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | All namespaces connected |
| 1 | One or more namespaces not connected |
| 2 | Usage error |

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--publish-url` | `http://127.0.0.1:498/v1/publish` | shed-agent publish endpoint |

## Status Detail

The status command queries each namespace with a `status` operation (2-second timeout per namespace):

- **SSH agent**: Reports backend mode (`agent-forward` or `local-keys`) and number of available keys
- **AWS credentials**: Reports the configured IAM role and cache expiration (assume-role mode), or `passthrough:<profile>` and the session expiry when known (passthrough mode)
