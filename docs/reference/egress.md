# Egress Control

Opt-in, audit-first control over what a shed's sandbox can reach on the network.
Off by default. Works on both backends (Firecracker and VZ).

!!! warning "Level 1 is cooperative — NOT a security boundary"
    Egress control routes traffic through a host-side filtering proxy via the
    guest's `HTTP_PROXY`/`HTTPS_PROXY` environment. That is honored only by
    **cooperating** clients (curl, git, dockerd, most language HTTP stacks). A
    process that ignores the proxy environment — a raw `connect()`, `nc`, `ssh`,
    a statically-linked binary, or a sudo user who unsets the variables —
    **bypasses egress control entirely**, including the always-on deny-CIDR
    guards. Treat this as visibility + guardrails for a *trusted* team, not as a
    sandbox escape boundary. The full bypass inventory is below.

## What it does

When enabled, shed-server runs a small `shed-egress-proxy` child process. Each
egress-enabled shed gets its own listener port on that proxy, and the guest is
injected with an `HTTP(S)_PROXY` pointing at it (plus an in-guest dockerd
proxy drop-in, so `docker pull` is covered too). The proxy:

- **resolves the destination itself** and **pins** the dialed IP for the
  connection lifetime (a DNS-rebinding / cloud-metadata defense),
- **denies always-on guard CIDRs** — link-local/IMDS (`169.254.0.0/16`,
  `169.254.170.2/32`), loopback, RFC1918, CGNAT, the VM gateway, and the IPv6
  equivalents — checked against **every** resolved address, even in audit mode,
- evaluates a **per-shed policy** (allow/deny domain globs + CEL rules),
- **allows** matching connections (spliced through) and **denies** the rest
  (dropped, with an audited reason),
- records every decision to a durable audit log.

Attribution is by **per-shed listener port + a per-shed token** injected into the
proxy URL — a shed that guesses another shed's port still lacks its token.

## Enabling it

Egress is configured in the shed-server config (`server.yaml`). It is **off**
unless `egress.enabled: true`.

```yaml
egress:
  enabled: true
  port_range: "20000-30000"   # per-shed listener allocation (default)
  default: []                 # profiles applied to sheds created without --egress
                              # ([] or absent = those sheds get NO egress control)
  profiles:
    audit:    { mode: audit }                                  # allow + log everything (guards still deny)
    base:     { allow: ["*.ubuntu.com", "*.debian.org"] }
    github:   { allow: ["*.github.com", "github.com"] }
    internal: { rule: 'host.endsWith(".corp.internal") && port == 443' }   # CEL
```

A bad glob or CEL rule (or a reserved/unknown profile name) makes shed-server
**fail to start** with a clear error, rather than silently running without the
policy you intended.

### Profiles

A profile is a named policy fragment. Sheds compose one or more by name.

| Field | Meaning |
|-------|---------|
| `mode` | `audit` (allow non-guard traffic and log it) or omitted (enforce: deny anything not explicitly allowed). If **any** composed profile is `audit`, the shed is in audit mode. |
| `allow` | Domain globs that allow a connection. `*.example.com` matches any subdomain (not `example.com` itself); `example.com` matches exactly. |
| `deny` | Domain globs that deny (checked before allows within a profile). |
| `rule` | A [CEL](https://github.com/google/cel-go) expression — the power path (see below). |

The names `off`, `none`, and `default` are reserved.

The **effective policy** for a shed is: always-on guard CIDRs (highest
precedence, deny even in audit mode) → the composed profiles' rules in order
(first match wins: deny globs, then allow globs, then the CEL rule, per profile)
→ a fall-through (enforce = deny; audit = allow + log).

### The policy language

`allow:`/`deny:` globs are sugar. The `rule:` field is a CEL expression that
must evaluate to a bool; a connection is allowed when it returns `true`. The
following variables are available:

| Variable | Type | Value |
|----------|------|-------|
| `host` | string | the canonicalized destination host (IDNA/punycode, lowercased) |
| `port` | int | destination port |
| `resolved_ip` | string | the pinned upstream IP |
| `protocol` | string | `"https"` (CONNECT) or `"http"` (plain) |
| `shed` | string | the shed name |

```cel
host.endsWith(".github.com") && port == 443
host == "registry.internal" && protocol == "https"
```

A CEL evaluation error **fails closed** (the connection is denied). For the full
language see the [cel-go spec](https://github.com/google/cel-spec/blob/master/doc/langdef.md).

## CLI

```bash
# Create a shed with egress profiles (comma-separated). Absent ⇒ server default.
shed create web --egress base,github
shed create web --egress off            # explicitly no egress for this shed

# Inspect a shed's egress: active profiles, listener port, resolved rules, and
# recent allow/deny decisions.
shed egress show web

# Change a running shed's profiles live (re-pushes policy + re-injects the env);
# on a stopped shed this persists and applies on next start.
shed egress set web --profile base,github
shed egress set web --profile off       # disable

# Turn egress off for a shed.
shed egress off web
```

Example `shed egress show`:

```text
Shed:     web
Profiles: github
Port:     20001

Rules:
  github: allow=*.github.com,github.com

Recent decisions (most recent last):
  TIME      VERDICT  HOST            PORT  REASON
  14:22:31  allow    api.github.com  443   allow:*.github.com
  14:22:32  deny     pypi.org        443   default-deny
```

## Auditing

Every decision is appended as one JSON line to `{state}/egress-audit.jsonl`
(next to the backend's instance dir) and kept in an in-memory ring for
`shed egress show`. Records carry host / port / resolved IP / protocol / verdict
/ reason — **never full URLs or paths** (which can carry tokens). The records
are also streamed over `GET /api/egress/stream` (Server-Sent Events) for the
shed-host-agent → shed-desktop activity feed.

## Bypass inventory (read this)

Level 1 is cooperative. Each of these reaches the network **without** going
through the proxy, and is therefore **not filtered and not audited**:

- A raw `connect()` / any non-proxy-aware tool (`nc`, `ssh`, a compiled binary)
  — reaches everything directly, **including IMDS and private ranges**, since
  the deny-CIDR guards only apply to proxy-routed traffic.
- A sudo user who unsets `HTTP_PROXY`/`HTTPS_PROXY` or overrides the docker
  proxy drop-in.
- Direct connections to an IP literal (the proxy filters by hostname).
- UDP / QUIC / HTTP3 — the proxy env only captures TCP HTTP(S).
- DoH / DoT (DNS over HTTPS/TLS) to an allowed host.
- A cooperating client that lies about the destination (SNI / Host is
  client-asserted) — though resolve-and-pin still controls which IP is dialed.
- Containers other than dockerd's own pulls (only dockerd's egress is routed at
  Level 1).
- VM-to-VM traffic on a shared host bridge.

Making egress a real boundary (forcing all traffic through the proxy via
netns/pf or a userspace netstack) is tracked as future work
([#203](https://github.com/charliek/shed/issues/203)).

## Other caveats

- **Live policy tightening** does not interrupt connections already in flight —
  a long-lived tunnel opened before you denied a host keeps running until it
  closes. New connections get the new policy immediately.
- **Audit is best-effort under load** — records are dropped (not blocked) at
  each buffer if a consumer can't keep up. The data path is never stalled by
  auditing.
- **Plain HTTP** is deny-by-default (allow `protocol == "http"` explicitly if
  you need it); only the first request on a kept-alive plain-HTTP proxy
  connection is policy-checked.
- **shed-server restart**: a running shed's egress listener is re-established
  when the shed itself is next started; after a shed-server restart, restart an
  egress-enabled shed (`shed restart <name>`) to restore its egress routing.
- **Connecting a VPN mid-session** (VZ): the gateway/MTU are detected at VM
  start; restart the shed to pick up a changed host network.

## How it fits together

```
 guest (HTTP_PROXY=http://<token>@<gateway>:<port>)
   │
   ▼
 shed-egress-proxy (child of shed-server)  ── resolve + pin + guard-check + CEL ──▶ upstream (allow)
   │  per-shed listener + token                                                    └▶ dropped (deny)
   │  audit (host/port/ip/verdict/reason)
   ▼
 shed-server  ── durable JSONL + recent ring + SSE stream ──▶ shed-host-agent ──▶ shed-desktop (view)
```

The proxy is a child of shed-server: it starts only when egress is enabled and
exits with shed-server. It ships in the brew and deb packages alongside
`shed-server`.
