# Orchestrated Sheds

An orchestrator, such as a CI system or an agent runner, can drive sheds on a
shed-server it does not administer. It authenticates as a **service client**: an
mTLS identity that can create sheds within a policy, run commands in them over
HTTPS, and delete them, and can see or touch nothing else on the server. Its
sheds get no server mounts and no credential bus by default, and on Firecracker
an `isolated` network that reaches the internet and nothing else.

This guide sets one up. The model and its limits are in [Security › Service
clients](../reference/security.md#service-clients); every key is in
[Configuration › Service clients](../reference/configuration.md#service-clients).

## Before you start

- The server runs `auth.mode: mtls` (see [Security
  Configuration](security-configuration.md#case-3b-remote-internet-facing-mtls-recommended)).
  Service clients do not exist in `open` or `token` mode.
- For the `isolated` network profile, the server uses the Firecracker backend
  and can run `nft` (the deb package's root unit can). A VZ server works too, with
  `network: open` in the client's policy.
- Every client request is written to the audit log, which is turned on
  automatically.

## 1. Declare the client

Add the client under `auth.clients` in `server.yaml` and restart `serve`:

```yaml
auth:
  mode: mtls
  ssh:
    github_users: [charliek]
  clients:
    nocturn:
      cert_max_ttl: 2160h          # longest certificate client issue signs (90 days)
      policy:
        name_prefix: "nocturn-"    # every shed it creates starts with this
        max_sheds: 4               # owned sheds at once, running or stopped
        max_age: 30h               # the server deletes an owned shed older than this
        network: isolated          # the default; must be open on VZ
        server_mounts: []          # the default: no host directories
        images: []                 # the default: only the server's default_image
        caps: {cpus: 4, memory_mb: 8192}
```

To let the client pick a different image, add an alias under the backend's
`image_aliases` and name the alias in `images`. A typo anywhere under
`auth.clients` stops the server from starting, with the key named.

Check what the server will enforce:

```bash
sudo shed-server client list
```

## 2. Issue a bundle

On the shed host, as the user that runs `serve` and with the same config:

```bash
sudo shed-server client issue nocturn --out nocturn-bundle.json
```

The file (mode `0600`) holds one JSON object: the client name, `auth_mode`,
`https_port`, the server's `tls_cert_fingerprint`, the PEM `client_cert` and
`client_key`, the `cert_serial` and `expires_at`. `--ttl 720h` issues a
shorter-lived certificate; it cannot exceed `cert_max_ttl`.

## 3. Store it

The bundle holds a private key: treat it as a secret. Move it to the
orchestrator's secret store (an environment variable holding the JSON, a secret
manager) and delete the file from the shed host. Whoever holds it has root in
that client's sheds (see [What a leaked bundle gives](#what-a-leaked-bundle-gives)).

## 4. Call the API

The client talks to the server's HTTPS port with its certificate, and pins the
server's self-signed certificate by `tls_cert_fingerprint`. With `curl` and
`jq`:

```bash
jq -r .client_cert nocturn-bundle.json > client.crt
jq -r .client_key  nocturn-bundle.json > client.key
chmod 600 client.key

# Fetch the server certificate and stop unless it matches the bundle's pin.
openssl s_client -connect shed-host:8443 -cert client.crt -key client.key </dev/null 2>/dev/null \
  | openssl x509 > server.pem
got="sha256:$(openssl x509 -in server.pem -noout -fingerprint -sha256 | cut -d= -f2 | tr -d : | tr A-F a-f)"
want="$(jq -r .tls_cert_fingerprint nocturn-bundle.json)"
[ "$got" = "$want" ] || { echo "server certificate does not match the pin" >&2; exit 1; }

# Pin the verified certificate's public key for every later call.
PIN="sha256//$(openssl x509 -in server.pem -noout -pubkey \
  | openssl pkey -pubin -outform der | openssl dgst -sha256 -binary | base64)"
```

Every call then passes `--pinnedpubkey "$PIN"`, so curl refuses any server
whose key is not the one just checked against the bundle. `--cacert server.pem`
alone is not enough: curl built with OpenSSL can keep trusting its system CA
directory alongside the file, so a certificate from a system-trusted CA (an
intercepting proxy's, say) would also pass. curl also checks the host name, so
call the server by a name in the certificate: `localhost`, `127.0.0.1`, or one
listed in the server's `tls_names`. A client written in a programming language
should instead compare the SHA-256 of the presented leaf certificate (DER) with
the pin on every connection, as the `shed` CLI does.

```bash
API="curl -sS --cacert server.pem --pinnedpubkey $PIN --cert client.crt --key client.key -H Content-Type:application/json"

# What the server supports: expect service_clients and http_exec.
$API https://shed-host:8443/api/info | jq .features

# Create a shed (the name must carry the prefix).
$API -d '{"name":"nocturn-demo"}' https://shed-host:8443/api/sheds | jq '{name,status,owner,mounts,network_profile}'

# Run a command. argv is not wrapped in a shell: ask for one when you need it.
$API -d '{"argv":["bash","-lc","uname -a; id -un"],"timeout_seconds":30}' \
  https://shed-host:8443/api/sheds/nocturn-demo/exec \
  | jq -j 'if .t == "exit" then "\nexit \(.code)\n" else (.d | @base64d) end'

# Delete it.
$API -X DELETE https://shed-host:8443/api/sheds/nocturn-demo
```

The exec response is one JSON frame per line: `stdout` and `stderr` chunks in
base64, then an `exit` frame. A stream that ends without an `exit` frame means
the result was lost. Pass `stdin` as base64, set `timeout_seconds` (default 60)
and `max_output_bytes` (default 4 MiB) to bound each call, and expect
`409 SHED_NOT_RUNNING` for a stopped shed: exec never starts one. The contract
is in [API › POST /api/sheds/{name}/exec](../reference/api.md#post-apishedsnameexec).

## What the client can and cannot do

| Can | Cannot |
|-----|--------|
| Read `/api/info` and `/api/images` | Reach any other route (`403 FORBIDDEN`) |
| List, create, get, start, stop, delete and exec in its own sheds | See or touch an admin's or another client's shed (`404 SHED_NOT_FOUND`, the same as a shed that does not exist) |
| Create within its policy: prefix, image list, caps, `max_sheds` | Use `local_dir`, `add_dirs`, `from_snapshot`, another network profile, or mounts outside `server_mounts` (`403 POLICY_VIOLATION`) |
| Run anything as root inside its sheds (`sudo`) | Use the credential bus, SSH, Connect tunnels, snapshots or egress settings |

`GET /api/info` needs no shed, so call it first: its `features` say what the
server supports (`service_clients`, `http_exec`, `mount_selection`, and
`network_profiles` on Firecracker). `GET /api/images` returns the server's
whole image list, the same one an admin sees; the client can create only from
the server's `default_image` and the aliases its policy's `images` names.

Admins keep the full API and see every shed, with its `owner`, in
`shed list -v`.

### Repositories and credentials

A create's `repo` is cloned inside the shed with only the egress proxy
settings in its environment, never the server's `env_file` variables, so the
server's git credentials do not reach an owned shed. Use a public URL, or
create the shed without `repo` and clone it yourself with a short-lived token
the orchestrator passes in the exec request's `env`:

```bash
$API -d '{"argv":["bash","-lc","git clone \"https://x-access-token:${TOKEN}@github.com/org/repo.git\" ~/repo"],
          "env":{"TOKEN":"<short-lived token>"},"timeout_seconds":300}' \
  https://shed-host:8443/api/sheds/nocturn-demo/exec | jq -c 'select(.t == "exit")'
```

The token is then in the shed (the clone's `.git/config` keeps the URL), so
scope it to the one repository and keep its lifetime short.

The [credential bus](../reference/security.md#credential-bus) is always off for
an owned shed, so SSH-agent, AWS and Docker credential forwarding do not work
there: `ssh-add -l`, the Docker and AWS credential helpers, and any publish to
the in-guest agent at `127.0.0.1:498` get no answer: each request waits out the
agent's 30 s timeout, then fails. An owned shed has no server mounts unless the policy's
`server_mounts` names some, so no host credential directory is there either.

## Lifetime

The orchestrator should delete its sheds when it is done. As a backstop, the
server deletes any owned shed older than the client's `max_age`: a reaper runs
at `serve` start and every 60 s, stops the shed and deletes it, and writes a
`reap` line to the audit log. Sheds whose client was removed from the config
are left alone for an admin to clean up.

## The audit log

`audit.jsonl` lives next to `egress-audit.jsonl`, in the parent of the backend's
`instance_dir` (`/var/lib/shed/firecracker/audit.jsonl` with the Firecracker
defaults). Each client request writes one `api` line (identity and certificate
serial, route, path, shed, status), each HTTPS exec an `exec.end` line (argv[0]
and the argument count, exit code, duration), and each reap a `reap` line. To
log the full argv set `audit.exec_argv: full`; it can carry secrets. See
[Configuration › Audit log](../reference/configuration.md#audit-log).

```bash
sudo jq -c 'select(.identity.name == "nocturn")' /var/lib/shed/firecracker/audit.jsonl | tail
```

## Revoking a client

There is no per-certificate revocation yet. To revoke a client, remove it from
`auth.clients` and restart `serve`: every certificate issued for it is refused
from then on, at the TLS handshake. Its sheds stay, admin-only, until an admin
deletes them. Re-adding the same name later would trust the old certificates
again until they expire, so give a replacement client a new name.

## What a leaked bundle gives

Whoever holds the bundle can do everything the client can, within its policy:
create sheds, run commands as root in its live sheds, read what the
orchestrator put in them, and delete them. Through the API it cannot touch an
admin's sheds or another client's, and it gives no admin route and no SSH.
What those commands can reach on the network and the host is set by the
shed's profile and mounts, not by the certificate: an `isolated` shed reaches
only the internet (and its own egress proxy), while with `policy.network:
open` (required on VZ) a command reaches whatever the host's NAT reaches,
including the host, the LAN and other open sheds; the client's
`server_mounts` are host directories it can read and, unless read-only, write.
See [Security › Service clients](../reference/security.md#residual-risks).
Keep `cert_max_ttl` short, keep the
bundle in a secret store, and revoke the client as above if it leaks.
