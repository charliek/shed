# Web Proxy Discovery: Accessing Web Services in Sheds

Research and analysis for exposing web servers running inside Firecracker (and VZ) VMs via a reverse proxy, using URLs like `https://shed-name.server-name.shed.charliek.io/index.html`.

## Problem

Sheds can run web services (opencode web UI, test applications, dev servers) but there's no way to access them from outside the host. We need a proxy layer that routes HTTP requests to the correct VM based on shed name.

## How Host-to-VM Networking Works

### Firecracker: Bridge Network

Firecracker VMs sit on a Linux bridge network (`shed-br0`) that the host can reach directly:

```
Host (172.30.0.1) <-- shed-br0 bridge --> VM1 (172.30.0.2)
                                     --> VM2 (172.30.0.3)
                                     --> VM3 (172.30.0.4)
```

- Bridge: `shed-br0` with IP `172.30.0.1/24`
- Each VM gets a TAP device attached to the bridge
- VMs get IPs starting from `172.30.0.2` (`gateway + instance_index + 1`)
- NAT (iptables MASQUERADE) gives VMs outbound internet access
- **The host can TCP connect directly to any VM IP** -- no tunneling needed

If a web server runs on port 8080 inside a VM at `172.30.0.3`, the host can `curl http://172.30.0.3:8080` directly.

### VZ: NAT/Localhost

VZ backend returns `127.0.0.1` from `GetNetworkEndpoint()`. Port forwarding would need to be handled differently (likely via vfkit port mapping or vsock-based proxying).

### Existing Name-to-IP Resolution

`GetNetworkEndpoint(ctx, shedName)` already resolves a shed name to its bridge IP by reading instance metadata. Used today by the SSH server's `handleDirectTCPIP` for port forwarding (`internal/sshd/server.go:218`).

The HTTP API (`GET /api/sheds/{name}`) also returns shed info including network details.

## End-to-End Architecture

```
Browser
  | HTTPS
  v
Tailscale Server (reverse proxy, e.g., Caddy)
  - Wildcard cert for *.server-name.shed.charliek.io
  - Extracts shed-name from subdomain
  | HTTP (over Tailscale)
  v
Host Machine (proxy layer)
  - Resolves shed-name -> VM bridge IP
  - Forwards request to 172.30.0.x:port
  | HTTP (over bridge)
  v
Firecracker VM (web server on :port)
```

## Proxy Layer Options

### Option A: Reverse Proxy Handler in shed-server

Add a reverse proxy to the existing HTTP API server (chi router on port 8080).

```go
// In internal/api/server.go
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
    shedName := extractShedName(r.Host) // parse subdomain
    endpoint, _ := s.backend.GetNetworkEndpoint(ctx, shedName)
    targetPort := resolvePort(r) // from config/subdomain/default

    proxy := httputil.NewSingleHostReverseProxy(
        &url.URL{Scheme: "http", Host: net.JoinHostPort(endpoint, targetPort)},
    )
    proxy.ServeHTTP(w, r)
}
```

**Pros:** Leverages existing shed name resolution, lifecycle-aware, no new service.
**Cons:** Mixes proxy concerns into shed management server, harder to add non-shed routing logic later.

Could listen on a separate port (e.g., 8081) or use host-based routing on the same port.

### Option B: SSH Tunnels (already built)

Use the existing SSH tunnel infrastructure (`internal/tunnels/`):

```
ssh -L 3000:localhost:3000 my-shed@host-server
```

The SSH server's `handleDirectTCPIP` already proxies to the VM's network endpoint.

**Pros:** Already implemented, encrypted, works with any TCP protocol.
**Cons:** Static port mappings, requires SSH client, harder to map `shed-name.domain` dynamically.

### Option C: Standalone External Proxy

A separate process (Caddy, nginx, custom Go binary) running on the host.

Shed name resolution options:
1. **Query shed-server's HTTP API**: `GET /api/sheds/{name}` to get the shed's IP
2. **Read instance metadata directly**: Parse JSON files in the instance directory
3. **DNS-based**: Custom DNS server that resolves `shed-name.local` to bridge IP

**Pros:** Clean separation of concerns, can add arbitrary routing/auth/TLS logic without touching shed code.
**Cons:** Separate service to manage, needs config sync or API calls for shed resolution.

### Hybrid A+C (likely best long-term)

- **shed-server** provides the API endpoint for shed resolution (already does via `GET /api/sheds/{name}`)
- **Standalone proxy** queries that API to resolve routing, handles all HTTP proxy logic

This keeps shed-server focused on VM management while the proxy handles routing concerns.

Note: Caddy doesn't natively support "call an API to resolve the upstream." You'd need either:
- A sidecar that generates Caddy config and reloads via Caddy's admin API when sheds start/stop
- A custom Caddy module
- A small custom Go proxy (essentially option A as a standalone binary)

## Port Resolution: How to Know Which Port

### 1. Subdomain Encoding
URL: `my-shed-3000.server.shed.charliek.io` -> shed `my-shed`, port `3000`.
Simple, no state needed, but makes subdomain naming less clean.

### 2. Path Prefix
URL: `my-shed.server.shed.charliek.io/port/3000/path` -> strip `/port/3000`, forward to `:3000/path`.
Clean subdomain but mangles the URL path.

### 3. Default Port Convention
Always forward to port 8080 (or configurable default). User configures their service to use that port. Simplest, but limited to one service per shed.

### 4. Per-Shed Config / Agent-Reported Ports

**4a. User-declared (at shed creation or via API):**
```yaml
exposed_ports:
  - port: 3000
    label: "opencode-web"
  - port: 8080
    label: "test-app"
```
Stored in shed metadata. Proxy reads this to know valid ports.

**4b. Agent-reported (automatic discovery):**
The shed-agent periodically scans `/proc/net/tcp` for listening ports and reports them to the host via vsock. Similar to VS Code devcontainer auto-detection.

Could use the existing notify vsock channel (port 1026) or a new one. The agent runs as root and can read `/proc/net/tcp`.

**Security for non-root shed user:** The shed user can listen on any port >1024. For port declaration, a well-known file the shed user can write to:
```json
// ~/.shed/exposed-ports.json (writable by shed user)
[{"port": 3000, "label": "web"}, {"port": 8080, "label": "api"}]
```
The agent (running as root) watches this file and reports changes to the host via vsock.

**Practical security:** The bridge network is host-local -- VMs aren't directly reachable from the internet. Security is controlled at the proxy layer (which ports it forwards to).

## Suggested Phased Approach

**Phase 1 (minimal viable proxy):**
- Small standalone Go proxy or Caddy with config generation
- Subdomain-based shed name resolution via shed-server API
- Default port convention (e.g., always 8080)

**Phase 2 (port flexibility):**
- Port-in-subdomain support (`my-shed-3000.server...`)
- Or per-shed port config via API

**Phase 3 (automatic discovery):**
- Agent reports listening ports via vsock
- Registration/discovery API

## Summary

| Approach | Complexity | Flexibility | Notes |
|----------|-----------|-------------|-------|
| Direct bridge IP from host | Already works | Full TCP access | `curl http://172.30.0.x:port` works today |
| SSH tunnel (-L) | Already built | Any TCP port | `internal/tunnels/` works now |
| Reverse proxy in shed-server | Low | HTTP/WS | ~50 lines of Go, uses existing infra |
| Standalone proxy | Medium | Full control | Separate binary, queries shed-server API |
| Caddy + config sync | Medium | HTTP/WS + TLS | Need sidecar to update Caddy config |
| Agent port discovery | Medium | Automatic | Extend vsock notify protocol |

The foundational piece -- host can reach VMs via bridge IPs -- is already there. Everything else is routing and discovery.

## Key Files

| File | Relevance |
|------|-----------|
| `internal/firecracker/network.go` | Bridge/TAP/NAT setup |
| `internal/firecracker/client.go:621` | `GetNetworkEndpoint()` - name to IP resolution |
| `internal/sshd/server.go:189` | `handleDirectTCPIP` - existing port forwarding |
| `internal/api/server.go` | HTTP API server, shed resolution endpoints |
| `internal/tunnels/manager.go` | Existing SSH tunnel infrastructure |
| `cmd/shed-agent/server.go` | Vsock listener, notify channel |
