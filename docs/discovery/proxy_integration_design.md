# Proxy Integration Design: Prox + Shed

Design document for exposing services running inside shed VMs via a reverse proxy on the host.

> **Status (as of v0.6.2).** Phases 1–2 have **shipped** (PR #62, v0.3.1):
> the vsock TCP proxy, `DialService`, the Connect API, the VZ SSH-tunnel
> fix, and the tunnel rewrite onto the Connect API are all in the tree and
> documented in [Tunnels](../reference/tunnels.md) and
> [HTTP API](../reference/api.md). **Phases 3–5 are not built** — the
> external `shed-ext-proxy` repo (reverse proxy, prox watcher, hostname
> routing) does not exist yet. The phase headings below are kept in their
> original planning form; read §8 with this status in mind. Historical
> note: `Backend.GetNetworkEndpoint`, referenced in §1, was later removed
> (PR #155) — `DialService` is now the sole contract for reaching a VM.

## 1. Current State Summary

### What Prox Writes

Prox has a **shared proxy daemon** (`~/.prox/proxy.sock`) that accepts registrations from multiple `prox up` instances. When a project starts, it sends a `RegisterRequest` over the Unix socket:

```go
type RegisterRequest struct {
    ProjectDir     string                   `json:"project_dir"`
    PID            int                      `json:"pid"`
    Version        string                   `json:"version"`
    Domain         string                   `json:"domain"`          // e.g., "local.stridelabs.ai"
    Services       map[string]ServiceTarget `json:"services"`        // e.g., {"app": {Host: "localhost", Port: 3000}}
    HTTPPort       int                      `json:"http_port"`       // e.g., 80
    HTTPSPort      int                      `json:"https_port"`      // e.g., 443
    CaptureEnabled bool                     `json:"capture_enabled"`
}
```

The daemon builds FQDNs by combining service names with the domain (e.g., `app` + `local.stridelabs.ai` = `app.local.stridelabs.ai`), **dynamically creates HTTP/HTTPS listeners on the requested ports**, and reverse-proxies matching requests to the service targets. Ports are fully user-defined — 443, 80, 6789, anything. Multiple projects can share the same port via hostname routing. TLS uses `mkcert`-generated wildcard certificates stored in `~/.prox/certs/`.

The daemon exposes an HTTP API on its Unix socket:

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/health` | GET | Liveness check + version |
| `/api/v1/register` | POST | Register a project's routes |
| `/api/v1/deregister` | POST | Remove a project's routes |
| `/api/v1/status` | GET | Full daemon status (routes, listeners, uptime) |
| `/api/v1/routes` | GET | All currently registered routes |
| `/api/v1/shutdown` | POST | Graceful daemon shutdown |

### How VM Networking Works

| | VZ (macOS) | Firecracker (Linux) |
|---|---|---|
| **Network model** | NAT via vfkit `virtio-net,nat` | Bridge + TAP (`shed-br0`, `172.30.0.1/24`) |
| **VM IP from host** | Not routable — `GetNetworkEndpoint()` returns `127.0.0.1` | Routable on bridge — returns e.g. `172.30.0.2` |
| **Direct TCP to VM** | Not possible | Yes — `curl http://172.30.0.2:3000` works |
| **Vsock** | Per-port Unix sockets (`<name>-<port>.sock`) | Single UDS with CONNECT handshake |
| **SSH tunnel port forwarding** | **Broken** — dials `127.0.0.1:<port>` on the host, not the VM | Works — dials bridge IP inside the VM |

**Critical finding:** SSH tunnels for VZ don't reach services inside the VM. `handleDirectTCPIP` calls `GetNetworkEndpoint()` returning `127.0.0.1`, then dials that on the host. `DialService` with a vsock TCP proxy fixes this.

### Shed Extension System

Namespaced message bus over vsock:

- **Guest publishes:** `POST http://127.0.0.1:498/v1/publish` (shed-agent HTTP API)
- **Agent forwards:** vsock port 1026 -> shed-server
- **Host subscribes:** SSE at `GET /api/plugins/listeners/{namespace}/messages`
- **Host responds:** `POST /api/plugins/listeners/{namespace}/respond`

Messages use `sdk.Envelope` with `namespace`, `type` (request/response/event), `payload`, and `shed` metadata.

---

## 2. Architecture Overview

### Primitive Layering

```
DialService (internal, Backend method)
  │  The foundational primitive. Opens TCP connections into VMs.
  │  VZ: vsock CONNECT protocol. Firecracker: bridge TCP.
  │
  ├── Connect API (HTTP endpoint on shed-server)
  │     Exposes DialService to external processes via HTTP upgrade.
  │     Used by: shed tunnels CLI, shed-ext-proxy-host, any future tool.
  │
  ├── handleDirectTCPIP (SSH server, same process)
  │     Uses DialService directly (internal call). Fixes VZ SSH tunnels.
  │     Used by: shed exec, shed attach (interactive sessions stay on SSH).
  │
  └── shed-agent TCP proxy (vsock port 1028)
        The in-VM side of DialService for VZ backend.
        CONNECT protocol: "CONNECT <port>\n" / "OK\n" / raw TCP.
```

**Two primitives for two different jobs:**
- **TCP tunneling** (ports, services, proxy): Connect API -> `DialService`
- **Interactive sessions** (exec, attach, shell): SSH -> vsock binary framed protocol

Exec/attach need structured, multiplexed communication (commands, resize events, signals, exit codes). A raw TCP stream can't carry resize events alongside data without framing. SSH already handles all of this. The connect API is intentionally raw TCP — the right primitive for port forwarding and reverse proxying, not for interactive terminals.

### Three Repos, Clear Boundaries

```
┌─────────────────────────────────────────────────────────────────────────┐
│ shed (this repo) — core plumbing, always available                      │
│                                                                         │
│  shed-agent:     vsock TCP proxy on port 1028 (CONNECT protocol)        │
│  backend:        DialService(ctx, shedName, port) on Backend interface  │
│  shed-server:    Connect API endpoint (HTTP upgrade -> DialService)     │
│  sshd:           VZ tunnel fix (handleDirectTCPIP uses DialService)     │
│  tunnels:        Rewrite to use Connect API (replaces SSH tunnels)      │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│ shed-ext-proxy (new repo) — prox integration, optional                  │
│                                                                         │
│  Guest binary (shed-ext-proxy):                                         │
│    Polls ~/.prox/proxy.sock for routes                                  │
│    Publishes register/deregister/health events on "proxy" namespace     │
│                                                                         │
│  Host binary (shed-ext-proxy-host):                                     │
│    Subscribes to "proxy" namespace via shed-server SSE                  │
│    Runs reverse proxy (httputil.ReverseProxy)                           │
│    Routes traffic via shed-server Connect API                           │
│    Registers hostnames with host prox daemon (TLS/ports frontend)       │
│    Manages route table, error pages, health tracking                    │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│ prox (existing repo) — small change                                     │
│                                                                         │
│  Relax version check: skip if version field is empty                    │
└─────────────────────────────────────────────────────────────────────────┘
```

shed-extensions (credentials) is **unchanged**.

### Traffic Flow

```
Browser / Mobile
  |  HTTPS (:443) or HTTP (:80) or any user-defined port
  v
Host prox daemon (TLS termination, mkcert certs, dynamic port listeners)
  |  HTTP (preserves Host header, routes by hostname)
  v
shed-ext-proxy-host (:9080, reverse proxy with route table)
  |  HTTP upgrade to Connect API
  v
shed-server (:8080, Connect API endpoint)
  |  DialService (vsock CONNECT through the guest tcpproxy on both VZ and Firecracker)
  v
Service inside VM (:3000, :8080, etc.)
```

### Control Flow

```
Inside VM:
  prox daemon (started by user via "prox up")
    |
  shed-ext-proxy guest binary (polls prox daemon every 5s)
    |  POST http://127.0.0.1:498/v1/publish
  shed-agent (forwards over vsock port 1026)
    |  vsock
On Host:
  shed-server (receives on plugin bridge, delivers via SSE)
    |
  shed-ext-proxy-host binary (subscribed to "proxy" namespace)
    |
    +---> own route table (hostname -> shed + port)
    +---> own reverse proxy listener (:9080, routes via Connect API)
    +---> host prox daemon registration (TLS/port frontend, routes to :9080)
```

### Why This Split

**shed-server exposes DialService as a Connect API** — a general-purpose "tunnel me into this VM" primitive. It doesn't know about hostnames, HTTP routing, or proxy domains. Any tool can use it.

**shed-ext-proxy-host IS the reverse proxy.** It owns the route table, hostname matching, error pages, health tracking, and prox daemon registration. All proxy-specific logic lives here, not in shed core.

**Host prox is the TLS frontend** — handles certs, dynamic port listeners, hostname routing to the extension's reverse proxy.

**Ports are user-defined.** `https_port: 443` in the VM's `prox.yaml` passes through to host prox. No restrictions.

### Routing Model: Flat Subdomains

`{service}.{domain}` — e.g., `https://app.local.stridelabs.ai -> my-project VM, port 3000`.

Conflicts rejected on second registration.

---

## 3. Design Decisions

### Vsock TCP Proxy Wire Protocol

**Text CONNECT protocol on vsock port 1028.**

```
Client sends:  "CONNECT <port>\n"       (decimal, 1-65535)
Server sends:  "OK\n"  or  "ERR <message>\n" (then close)
After OK:      raw bidirectional TCP, no framing.
```

Matches Firecracker's existing vsock dialer pattern. Agent-side: shed-agent listener on 1028, on accept reads port, dials `127.0.0.1:<port>`, responds, bidirectional `io.Copy`.

### Connect API

shed-server exposes a single HTTP endpoint that upgrades to a raw TCP tunnel:

```
GET /api/sheds/{name}/connect/{port}
Connection: Upgrade
Upgrade: shed-tcp

Success: 101 Switching Protocols -> raw bidirectional TCP
Failure: 404 (shed not found), 502 (port unreachable), 503 (shed not running)
```

Implementation uses `http.Hijacker` to take over the connection after the upgrade handshake. ~40 lines of code:

```go
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
    shedName := chi.URLParam(r, "name")
    port, _ := strconv.ParseUint(chi.URLParam(r, "port"), 10, 16)

    // Dial into the VM via DialService
    vmConn, err := s.backend.DialService(r.Context(), shedName, uint16(port))
    if err != nil {
        // return appropriate error (404, 502, 503)
        return
    }

    // Upgrade the HTTP connection
    hj, ok := w.(http.Hijacker)
    if !ok {
        vmConn.Close()
        http.Error(w, "hijack not supported", 500)
        return
    }

    w.Header().Set("Connection", "Upgrade")
    w.Header().Set("Upgrade", "shed-tcp")
    w.WriteHeader(http.StatusSwitchingProtocols)

    clientConn, _, _ := hj.Hijack()

    // Bidirectional proxy
    go io.Copy(vmConn, clientConn)
    io.Copy(clientConn, vmConn)
    clientConn.Close()
    vmConn.Close()
}
```

**Consumers of the Connect API:**

1. **shed-ext-proxy-host** — uses it as `Transport.DialContext` in its reverse proxy
2. **shed tunnels CLI** (rewritten) — opens local port, bridges connections through Connect API
3. **Any future tool** — debug probes, monitoring, third-party integrations

### `DialService` Interface

```go
// In internal/backend/backend.go
type Backend interface {
    // ... existing methods ...

    // DialService opens a TCP connection to a port inside a running shed's VM.
    // Firecracker: dials VM's bridge IP directly.
    // VZ: dials via vsock TCP proxy (port 1028) with CONNECT handshake.
    DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error)
}
```

Returns `net.Conn`. `uint16` port. Context for timeouts. VZ wraps in `bufferedConn` for `bufio.Reader` buffering.

### Tunnel Rewrite

Current `shed tunnels` spawns SSH subprocesses with `-L` flag, manages PIDs, state files, reconnection. With the Connect API:

```
shed tunnels start myproj -t 3000
  -> opens local TCP listener on :3000
  -> each connection: HTTP upgrade to shed-server /api/sheds/myproj/connect/3000
  -> bidirectional copy
```

No SSH process, no PID file, no state management, no SSH keys. The tunnel CLI becomes a thin Connect API client. Works for VZ (unlike current SSH tunnels).

| | Current SSH tunnels | Connect API tunnels |
|---|---|---|
| VZ support | Broken | Works |
| Dependencies | SSH client, keys, known_hosts | Just HTTP to shed-server |
| Code | `internal/tunnels/` manager + config + sshd handler | Small Connect API client |
| Lifecycle | SSH subprocess PID tracking | Local goroutine, no state file |

SSH stays for interactive sessions (`shed attach`, `shed exec`). Port forwarding moves to Connect API.

### Exec/Attach: Why They Stay on SSH

The connect API provides raw TCP streams. Exec needs structured, multiplexed messages:

```
[0x01] ExecRequest  — command, env, TTY settings
[0x05] Data         — stdout/stderr (bidirectional)
[0x02] Resize       — terminal rows/cols (out-of-band)
[0x03] Signal       — SIGTERM, SIGINT
[0x06] StdinEOF     — close stdin pipe
[0x04] ExitCode     — process result (final)
```

SSH already handles terminal emulation, resize, signal forwarding, multiplexing. Replacing it with WebSocket + custom protocol would be reimplementing SSH poorly. Clean separation: TCP tunneling uses Connect API; interactive sessions use SSH.

### Guest-Side Integration

Separate guest binary (`shed-ext-proxy`) polls prox daemon via Unix socket API. Prox stays completely unmodified:

1. Systemd service inside VM
2. Polls `GET /api/v1/routes` on `~/.prox/proxy.sock` every 5s
3. Diffs against last state, publishes register/deregister/health events via `BusClient`

Same pattern as `shed-ext-ssh-agent` and `shed-ext-aws-credentials`.

### Prox Daemon Lifecycle Inside a Shed

Starts on-demand via `prox up`. Cleanup: guest deregister on normal exit, host detects SSE close on crash, stale TTL (90s unhealthy, 5min removal).

### DNS Setup

Wildcard DNS `*.local.stridelabs.ai` -> shed-server host's Tailscale IP. One-time. For local-only: dnsmasq or `/etc/hosts`.

### HTTPS Certificates

Prox handles all TLS. Certs in `~/.prox/certs/`. User generates with mkcert. Host prox terminates TLS, forwards HTTP to extension's reverse proxy. No certs in shed-server.

### Route Registration

Prox-only for phase 1. AI agent use case: write a `prox.yaml` with the process + proxy config, run `prox up`.

### Health and Error UX

shed-ext-proxy-host serves branded HTML error pages (502/503/504/404) with shed name, port, troubleshooting. Mobile-friendly. `X-Shed-Error` header for programmatic clients.

---

## 4. Proxy Namespace Event Format

Published by guest binary. Fire-and-forget events.

### Register

```json
{
  "namespace": "proxy",
  "type": "event",
  "shed": {"name": "my-project", "backend": "vz", "server": "macbook"},
  "payload": {
    "action": "register",
    "routes": [
      {"hostname": "app.local.stridelabs.ai", "port": 443, "protocol": "https", "target_port": 3000},
      {"hostname": "api.local.stridelabs.ai", "port": 80, "protocol": "http", "target_port": 3001}
    ]
  }
}
```

Replace-all semantics per shed. Routes carry `port` + `protocol` (for host prox) and `target_port` (for Connect API).

### Deregister

```json
{"namespace": "proxy", "type": "event", "payload": {"action": "deregister"}}
```

### Health (every 30s)

```json
{"namespace": "proxy", "type": "event", "payload": {"action": "health", "route_count": 2}}
```

---

## 5. Prox Changes Required

```go
// handleRegister — make version optional:
if req.Version != "" && req.Version != s.version {
```

Backwards-compatible. Existing `prox up` always sets version. External clients omit it.

---

## 6. shed-ext-proxy Repo Specification

### Repo Structure

```
shed-ext-proxy/
  cmd/
    shed-ext-proxy/                  # Guest binary (Linux, in-VM)
      main.go                        # Prox watcher + bus publisher
      watcher.go                     # Poll loop, diff, event publishing
    shed-ext-proxy-host/             # Host binary (macOS/Linux)
      main.go                        # Entry point, config loading
      handler.go                     # Bus event handler
      proxy.go                       # Reverse proxy (httputil.ReverseProxy)
      routes.go                      # Route table (hostname -> shed + port)
      prox_client.go                 # Host prox daemon registration
      connect.go                     # shed-server Connect API client
      errors.go                      # Branded HTML error pages
      config.go                      # YAML config loading
  internal/
    protocol/
      proxy.go                       # Shared payload types (register/deregister/health)
  systemd/
    shed-ext-proxy.service           # Guest systemd unit
  manifests/
    proxy.yaml                       # Extension manifest
  Dockerfile                         # Multi-arch guest binary image
  Makefile
  README.md
```

### Guest Binary (`shed-ext-proxy`)

Polls prox daemon's `GET /api/v1/routes` on `~/.prox/proxy.sock`. Publishes events via shed SDK `BusClient`. ~200 lines, pure Go.

```go
func main() {
    bus := sdk.NewBusClient("http://127.0.0.1:498/v1/publish", 3*time.Second)
    watcher := NewProxWatcher(bus, proxSocketPath)
    watcher.Run(ctx) // poll every 5s, diff, publish
}
```

### Host Binary (`shed-ext-proxy-host`)

The host binary does three things:

**1. Subscribes to `proxy` namespace** via shed-server SSE. Maintains a route table from bus events.

**2. Runs a reverse proxy** on a configurable port (default `:9080`). For each request:
- Match `Host` header against route table -> shed name + target port
- Open connection via Connect API: `GET /api/sheds/{name}/connect/{port}` with HTTP upgrade
- `httputil.ReverseProxy` forwards request over the tunneled connection
- On error, serve branded HTML error page

**3. Registers routes with host prox daemon** so prox handles TLS/ports and routes to the extension's proxy port.

```go
// Connect API as Transport.DialContext
func (h *Handler) dialService(shedName string, port uint16) func(ctx context.Context, _, _ string) (net.Conn, error) {
    return func(ctx context.Context, _, _ string) (net.Conn, error) {
        return h.connectClient.Dial(ctx, shedName, port)
    }
}
```

**Connect API client** (`connect.go`):

```go
// Dial opens a TCP tunnel to a shed VM port via the Connect API.
func (c *ConnectClient) Dial(ctx context.Context, shed string, port uint16) (net.Conn, error) {
    url := fmt.Sprintf("http://%s/api/sheds/%s/connect/%d", c.shedServer, shed, port)
    req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
    req.Header.Set("Connection", "Upgrade")
    req.Header.Set("Upgrade", "shed-tcp")

    // Raw TCP dial to shed-server, send HTTP upgrade by hand
    conn, err := net.Dial("tcp", c.shedServer)
    if err != nil {
        return nil, err
    }
    req.Write(conn)

    // Read upgrade response
    resp, err := http.ReadResponse(bufio.NewReader(conn), req)
    if err != nil || resp.StatusCode != 101 {
        conn.Close()
        return nil, fmt.Errorf("connect failed: %d", resp.StatusCode)
    }
    return conn, nil
}
```

### Host Binary Config

```yaml
# ~/.config/shed-ext-proxy/config.yaml
shed_server: "http://localhost:8080"
listen: ":9080"                           # reverse proxy listen port
prox_socket: "~/.prox/proxy.sock"         # host prox daemon socket
```

### Extension Manifest

```yaml
# /etc/shed-extensions.d/proxy.yaml
namespace: proxy
systemd_unit: shed-ext-proxy.service
description: "Proxy route discovery via prox daemon"
```

### Docker Image

```dockerfile
FROM golang:1.24 AS builder
COPY . .
RUN CGO_ENABLED=0 go build -o /shed-ext-proxy ./cmd/shed-ext-proxy

FROM scratch
COPY --from=builder /shed-ext-proxy /usr/local/bin/shed-ext-proxy
COPY systemd/shed-ext-proxy.service /etc/systemd/system/
COPY manifests/proxy.yaml /etc/shed-extensions.d/
```

Published as `ghcr.io/charliek/shed-ext-proxy:<version>`. shed's Dockerfile:

```dockerfile
ARG SHED_EXT_PROXY_VERSION=v0.1.0
FROM ghcr.io/charliek/shed-ext-proxy:${SHED_EXT_PROXY_VERSION} AS shed-ext-proxy
# in experimental stage:
COPY --from=shed-ext-proxy /usr/local/bin/shed-ext-proxy /usr/local/bin/
COPY --from=shed-ext-proxy /etc/systemd/system/shed-ext-proxy.service /etc/systemd/system/
COPY --from=shed-ext-proxy /etc/shed-extensions.d/proxy.yaml /etc/shed-extensions.d/
```

---

## 7. End-to-End Flow

### Startup

1. User runs `prox up` inside shed `my-project`
2. In-VM prox daemon registers routes internally
3. `shed-ext-proxy` guest binary detects routes via `GET /api/v1/routes`
4. Publishes `register` event on `proxy` namespace
5. Event flows: shed-agent -> vsock -> shed-server -> SSE
6. `shed-ext-proxy-host` receives event, updates route table
7. Registers with host prox: `app.local.stridelabs.ai -> localhost:9080`
8. Host prox opens HTTPS on `:443`, loads mkcert cert
9. `https://app.local.stridelabs.ai` is live

### Request

1. Browser -> `https://app.local.stridelabs.ai`
2. Host prox -> `localhost:9080` (extension reverse proxy)
3. Extension matches hostname, opens Connect API tunnel to shed-server
4. shed-server `DialService("my-project", 3000)` -> VM
5. Response flows back through the chain

### Shutdown

1. **Normal:** prox down -> guest detects -> deregister event -> host deregisters from prox
2. **Crash:** SSE closes -> host detects, deregisters
3. **Stale:** TTL-based removal (90s unhealthy, 5min remove)

---

## 8. Implementation Phases

### Phase 1: Vsock TCP Proxy + DialService + Connect API (shed repo)

**Goal:** Foundational primitives for TCP access into VMs.

1. `VsockTCPProxyPort = 1028` constant
2. shed-agent: vsock listener on 1028 (CONNECT protocol)
3. VZ: add port 1028 to vfkit device args
4. `DialService` on Backend interface, VZ + Firecracker implementations
5. Connect API endpoint: `GET /api/sheds/{name}/connect/{port}` (HTTP upgrade)
6. Fix VZ SSH tunnels: `handleDirectTCPIP` uses `DialService` directly
7. Tests + manual test plan

**Deliverable:** Connect API works. VZ SSH tunnels fixed. `curl` to Connect API with upgrade reaches VM service.

### Phase 2: Tunnel Rewrite (shed repo)

**Goal:** `shed tunnels` uses Connect API instead of SSH.

1. New tunnel client using Connect API
2. Local TCP listener per port mapping
3. Simplified tunnel manager (no SSH subprocess, no PID files)
4. Deprecate/remove SSH-based tunnel code
5. Tests

**Deliverable:** `shed tunnels start my-vz-shed -t 3000` works via Connect API.

### Phase 3: Prox Version Check (prox repo)

1. Make version field optional in register API

### Phase 4: Proxy Extension (shed-ext-proxy repo, new)

1. Guest binary: prox watcher + bus publisher
2. Host binary: bus subscriber + reverse proxy + Connect API client + host prox registration
3. Extension manifest + systemd + Docker image
4. shed Dockerfile integration
5. Documentation: setup guide, DNS, certs, walkthrough

**Deliverable:** `prox up` inside VM -> browser hits `https://app.local.stridelabs.ai` -> service responds.

### Phase 5: Polish

1. Health tracking + status CLI
2. Stale route cleanup
3. Error page refinement
4. Remove legacy SSH tunnel code if Connect API tunnels prove solid

### Future

- Remote agent launch via orchestration API
- `shed proxy expose` for direct registration
- Request capture forwarding
- Multi-server routing
- Auto-discovery (port scanning without prox)
- Merge extension into shed-extensions once proven

---

## 9. Key Files Reference

### shed (this repo)

| Area | File | Change |
|------|------|--------|
| Backend interface | `internal/backend/backend.go` | Add `DialService` |
| VZ dialer | `internal/vz/dialer.go` | VZ `DialService` (vsock CONNECT) |
| VZ VM startup | `internal/vz/vm.go` | Add port 1028 to vfkit args |
| VZ client | `internal/vz/client.go` | `DialService` implementation |
| FC client | `internal/firecracker/client.go` | `DialService` implementation |
| Shed agent | `cmd/shed-agent/server.go` | TCP proxy listener on port 1028 |
| API server | `internal/api/server.go` | Connect API endpoint |
| SSH forwarding | `internal/sshd/server.go:194` | Use `DialService` in `handleDirectTCPIP` |
| Tunnel manager | `internal/tunnels/manager.go` | Rewrite to use Connect API |
| Tunnel CLI | `cmd/shed/tunnels.go` | Simplified for Connect API |

### prox

| Area | File | Change |
|------|------|--------|
| Register handler | `internal/proxyd/server.go:125` | Skip version check if empty |

### shed-ext-proxy (new repo)

| Area | File | Purpose |
|------|------|---------|
| Guest binary | `cmd/shed-ext-proxy/main.go` | Prox watcher + bus publisher |
| Host binary | `cmd/shed-ext-proxy-host/main.go` | Reverse proxy + route manager |
| Connect client | `cmd/shed-ext-proxy-host/connect.go` | shed-server Connect API client |
| Protocol types | `internal/protocol/proxy.go` | Event payload structs |
| Systemd unit | `systemd/shed-ext-proxy.service` | Guest service definition |
| Manifest | `manifests/proxy.yaml` | Extension manifest |

---

## Appendix: Prior alternatives considered (from the retired Web Proxy Discovery)

This appendix absorbs the earlier `web_proxy_discovery.md` exploration —
the option-survey that preceded this design. The design above is the chosen
realization, so these are recorded as "considered / superseded," not open
questions.

### Proxy-placement options

| Option | Sketch | Verdict |
|---|---|---|
| A. Reverse proxy *inside* shed-server | Add an `httputil` reverse proxy to the chi router; resolve shed→IP internally | Rejected — mixes proxy concerns into VM management; hard to extend later |
| B. SSH tunnels (`ssh -L`) | Reuse `handleDirectTCPIP` | Superseded — static mappings, needs an SSH client; VZ SSH tunnels were broken until the `DialService` fix |
| C. Standalone external proxy | Separate process resolves routes via a shed-server API | **Chosen (as a variant)** — became `shed-ext-proxy-host` |
| Hybrid A+C | shed-server exposes a resolution primitive; a standalone proxy owns routing | **Chosen** — the primitive is the Connect API (not a name→IP lookup); routing lives in the extension |

Key refinement over the original survey: instead of the proxy querying
`GET /api/sheds/{name}` for a bridge IP (Firecracker-only, and dependent on
the now-removed `GetNetworkEndpoint`), the proxy tunnels through the
**Connect API**, which works uniformly on VZ and Firecracker.

### Port-resolution schemes considered

- **Subdomain encoding** (`my-shed-3000.server…`) — simple, stateless, but ugly subdomains.
- **Path prefix** (`…/port/3000/path`) — clean subdomain, mangles the path.
- **Default-port convention** (always `:8080`) — simplest, one service per shed.
- **User-declared `exposed_ports`** in shed metadata — explicit, needs a declaration step.
- **Agent auto-discovery** — shed-agent scans `/proc/net/tcp` (or watches a `~/.shed/exposed-ports.json` the shed user can write) and reports listening ports over vsock.

The design above takes routes from the **prox daemon** instead of any of
these. Agent auto-discovery survives as the "Future: auto-discovery (port
scanning without prox)" item in §8.

### Networking substrate (unchanged finding)

The survey's one durable fact still holds and underpins this design: the
host can reach Firecracker VMs directly on the bridge (`172.30.0.x`), but
VZ VMs are not host-routable (`127.0.0.1`). That asymmetry is exactly why
the uniform primitive is `DialService` (vsock CONNECT on VZ, bridge TCP on
FC) rather than a raw IP.
