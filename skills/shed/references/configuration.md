# shed client configuration

The `shed` client uses three files under `~/.shed/`. (Server configuration — `server.yaml`, backends, credentials — is separate and out of scope for client usage.)

## `~/.shed/config.yaml` — servers and known sheds

Written and updated by `shed server add` / `shed create`; you rarely edit it by hand.

```yaml
default_server: my-server          # used when -s/--server is not passed
servers:
  my-server:
    host: my-server.tailnet.ts.net # hostname/IP on the private network (Tailscale)
    http_port: 8080                # API port (default 8080)
    ssh_port: 2222                 # auto-discovered from the server's /api/info
sheds:                             # local cache of where each shed lives
  myproj:
    server: my-server
    status: running
create_timeout: 10m                # optional; default create timeout
```

Auth is SSH-key based (keys from `~/.ssh`); there is no token/login field. The server's SSH host key is fetched and cached on first `shed server add`.

## `~/.shed/sync.yaml` — file sync profiles

Defines reusable **features** (sets of paths + optional post-sync commands) grouped into **profiles**. `shed sync` and `shed create` (default profile) consume this.

```yaml
features:
  devproxy:
    description: "Sync mkcert certificates for HTTPS development"
    paths:
      - source: ~/.local/share/mkcert/rootCA.pem
        target: /usr/local/share/ca-certificates/mkcert-ca.crt
      - source: ~/.devproxy/certs
        target: /etc/ssl/devproxy
        include: "*.pem"           # optional glob filter
    postSync:
      - run: update-ca-certificates
  dotfiles:
    description: "Shell + git config"
    paths:
      - source: ~/.gitconfig
        target: /home/shed/.gitconfig

profiles:
  default:                          # runs automatically on `shed create`
    features: [devproxy]
  full:
    features: [devproxy, dotfiles]
```

- Path fields: `source` (local, `~` expands), `target` (path in the shed), optional `include` glob.
- `shed sync <name> -p <profile>` runs a profile; `-f <feature>` runs one feature; `--dry-run` previews.

## `~/.shed/tunnels.yaml` — port-forwarding profiles

Named port sets per shed, used by `shed tunnels start <shed> -p <profile>`.

```yaml
sheds:
  myproj:
    profiles:
      webdev:
        - "3000"
        - "5173"
      database:
        - "5432"
        - "6379"
```

Each entry is a port the tunnel forwards (`local == remote`). For different local/remote ports or one-off ports, pass `-t local:remote` / `-t port` on the command line (combinable with `-p`). Background tunnels record their PIDs in `~/.shed/tunnels.state`.
