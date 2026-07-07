# Docker Credentials

The `docker-credentials` namespace brokers Docker registry credentials between shed microVMs and the host machine. Registry passwords and tokens are resolved on the host using the developer's existing Docker credential helpers (gcloud, osxkeychain, ecr-login, etc.).

## How It Works

`docker-credential-shed` is installed inside the VM at `/usr/local/bin/`. Docker discovers it via the `credsStore` setting in `~/.docker/config.json`. Unlike the SSH and AWS extensions, this is a **one-shot binary** — Docker execs it on demand, not a long-running daemon.

When Docker needs credentials for a registry pull or push, it runs `docker-credential-shed get` with the registry hostname on stdin. The binary sends the request through the message bus to the host agent, which resolves credentials from the host's Docker credential store.

```mermaid
sequenceDiagram
    participant Docker as Docker CLI (VM)
    participant Helper as docker-credential-shed (VM)
    participant Bus as Message Bus
    participant Host as shed-host-agent (Mac)
    participant Cred as Host credential helper<br/>(gcloud, osxkeychain, etc.)

    Docker->>Helper: exec get (stdin: "us-docker.pkg.dev")
    Helper->>Bus: publish(docker-credentials, get)
    Bus->>Host: SSE event
    Host->>Host: Check registry allowlist
    Host->>Host: Read ~/.docker/config.json
    Host->>Cred: docker-credential-gcloud get
    Cred->>Host: {Username, Secret}
    Host->>Bus: response
    Bus->>Helper: credentials
    Helper->>Docker: stdout: {ServerURL, Username, Secret}
```

## Docker Integration

The guest VM's `~/.docker/config.json` is configured with:

```json
{
  "credsStore": "shed"
}
```

This tells Docker to use `docker-credential-shed` as the default credential helper for all registries. The host-side allowlist controls which registries actually get credentials served.

## Public Images and Anonymous Pulls

Because `credsStore` applies to **every** registry, Docker invokes `docker-credential-shed get` even for public images (e.g. `docker pull postgres:16-alpine`, which resolves to `https://index.docker.io/v1/`). When the registry is **allowed** but the host has no matching credential, the binary returns Docker's standard `credentials not found in native keychain` message on stdout and exits non-zero. Docker treats this as "no credentials" and proceeds with an **anonymous pull**, which is exactly what public images need.

This means `{"credsStore": "shed"}` is safe as a blanket default: it serves credentials when the host has them, falls back to an anonymous pull for public images on allowed registries, and only aborts the pull on a genuine fault (a credential helper that exists but fails, a malformed request, or a bus error).

These anonymous pulls are recorded in the audit log with `result: anonymous` (not `error`), so a public-image pull shows as a successful anonymous request rather than a failure. A genuine fault is still recorded as `error`, and an allowlist deny as `error` (see below).

> **The allowlist is a hard wall, not a credential filter.** If `allow_all` is `false` and the requested registry is not in `registries`, the host returns `REGISTRY_NOT_ALLOWED` and the pull **fails** — even if a credential for that registry exists locally on the host. A non-allowed registry does *not* fall back to an anonymous pull: the allowlist is an explicit deny, checked before any credential lookup. To pull public images from a registry, add it to the allowlist (or set `allow_all: true`).

## Registry Configuration

The host agent controls which registries are available to VMs via an allowlist:

```yaml
docker:
  registries:
    - index.docker.io   # Docker Hub (the hostname Docker sends for docker.io images)
    - ghcr.io
    - registry.acmeco.com
```

Registry names must match the hostname Docker sends to the credential helper, after the broker strips the scheme and any trailing `/`, `/v1`, or `/v2`. The most common gotcha is **Docker Hub: list it as `index.docker.io`, not `docker.io`** — Docker requests Hub credentials for `https://index.docker.io/v1/`, which normalizes to `index.docker.io`. A bare `docker.io` entry does *not* match, and the pull fails with `REGISTRY_NOT_ALLOWED`.

Or allow all registries:

```yaml
docker:
  allow_all: true
```

If no registries are configured and `allow_all` is false (the default), the handler starts but rejects all credential requests with a clear error.

## Supported Credential Sources

The host agent reads `~/.docker/config.json` (or `$DOCKER_CONFIG/config.json`) and resolves credentials in priority order:

| Source | Config key | Example | How it works |
|--------|-----------|---------|-------------|
| Per-registry helper | `credHelpers` | `"us-docker.pkg.dev": "gcloud"` | Execs `docker-credential-gcloud get` |
| Default helper | `credsStore` | `"credsStore": "osxkeychain"` | Execs `docker-credential-osxkeychain get` |
| Inline credentials | `auths` | `"auths": {"registry.io": {"auth": "..."}}` | Decodes base64 `user:pass` |

## Message Format

### Get Request

```json
{
  "id": "0192b3a5-...",
  "namespace": "docker-credentials",
  "type": "request",
  "payload": {
    "operation": "get",
    "server_url": "us-docker.pkg.dev"
  }
}
```

### Get Response

```json
{
  "id": "0192b3a5-...",
  "namespace": "docker-credentials",
  "type": "response",
  "payload": {
    "server_url": "us-docker.pkg.dev",
    "username": "_json_key",
    "secret": "ya29.a0AfH6SM..."
  }
}
```

### Error

```json
{
  "id": "0192b3a5-...",
  "namespace": "docker-credentials",
  "type": "response",
  "payload": {
    "error": "registry \"blocked.io\" not in allowlist",
    "code": "REGISTRY_NOT_ALLOWED"
  }
}
```

Error codes: `CREDENTIALS_NOT_FOUND`, `REGISTRY_NOT_ALLOWED`, `READ_ONLY`, `HELPER_FAILED`, `INTERNAL_ERROR`.

## Read-Only Broker

`docker-credential-shed` rejects `store` and `erase` operations with an error. Credential management happens on the host only — the VM cannot modify the host's Docker credential store.

## Credential Caching

No caching on either side. Each `docker pull` triggers a fresh credential lookup on the host. Docker credential helpers are fast local calls (gcloud reads from its local cache, osxkeychain reads from keychain), and Docker typically calls `get` once per registry per operation.

## Timeouts

Credential requests use a 5-second timeout. A timeout is treated as a genuine fault, not a missing credential: the binary writes the error to stderr and exits non-zero with empty stdout, which aborts the pull. This is deliberate — a broker that is unreachable should surface loudly rather than silently degrade to an anonymous pull (which would then fail confusingly on any private image). Only an explicit "no credential to serve" response from the host (see [Public Images and Anonymous Pulls](#public-images-and-anonymous-pulls)) triggers Docker's anonymous-pull fallback.
