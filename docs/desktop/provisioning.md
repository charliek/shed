# Provisioning

The desktop app is a person's **credential source**: when a shed is created, or `shed provision` runs, the server asks the app on the person's bound machine for the credentials the shed should hold, and the app answers from what you configured. Nothing is asked per use and nothing is stored by the server. What happens after the answer is on the [Credential delivery](../reference/delivery.md) page; the wire format is the [provision contract](../reference/provision-contract.md).

The app answers only for a server whose credential source is this machine. **Preferences → Provisioning** shows each configured server's source row and **Use this machine** (the same pin `shed source use` makes; there is no unbind, a pin is only moved). The app holds one delivery stream per server and answers each request on its own, with no prompt.

!!! warning "Set `only.servers` on every provider"
    You do not have to choose a server for it to ask: the first machine that connects for you on a server becomes your source there, and the app opens a delivery stream to **every server in your shed config**. Of the [`only` rules](#the-only-rules), only `servers` is checked by the app against the server that actually asked. `backends` and `repos` are matched against what the server wrote in its request, so a dishonest server can satisfy them. A provider that is on with no `only.servers` delivers its items to any configured server that asks. Give each provider you enable an `only.servers` list of the servers you trust with it.

## The Provisioning section

Preferences gains a **Provisioning** section with:

- the five providers, each with an on/off switch and an editor for its settings and its `only` rule (lists are edited entry by entry, exactly as typed);
- the source rows ("Use this machine") for each server;
- a **preview** of what a named shed on a server would get.

The section reads and writes `~/.config/shed/delivery.toml`. A settings-file error shows its fixed text and where it is, never the file's content. A draft whose provider changed underneath is discarded on reload.

## The five providers

| Provider | Delivers |
|---|---|
| `ssh-key` | One key pair: `~/.ssh/id_<type>` (`0600`) and its `.pub` (`0644`). A key with a passphrase is refused. With `sign_commits`, git in the shed signs commits with it. |
| `github` | The token your own `gh` holds, as `~/.config/gh/hosts.yml`, with a git config that makes `gh` git's credential helper for github.com. |
| `api-keys` | craze's `~/.craze/native/providers.toml` (API keys only; never the ChatGPT-plan token file) and one environment variable per `KEY=VALUE` line of each env file you list (each file `0600` or stricter). |
| `envsecrets-login` | `~/.envsecrets/config.yaml` and its passphrase, so the shed pulls secrets for itself. The passphrase comes from a file or from a command. |
| `files` | Files copied as they are, each with a target, a base (`home`, or `project` for stage 2 after the clone) and a mode (`0600` or `0400`). For static credentials. |

Other sign-ins (a cloud CLI's, a registry's) are not delivered by a provider of their own: sign in inside the shed, or deliver the login file with `files`. See [Credentials in a shed](../guides/credentials-in-a-shed.md), which also states the trade: a usable credential is inside the shed while it runs, and anything there can read it.

## delivery.toml

Every table is optional, and a provider whose table is absent does not deliver. An unknown key is refused, so a misspelt key fails loudly. A setting that names a file takes an absolute path or one starting `~/`.

```toml
[ssh-key]
enabled = true
key_file = "~/.ssh/id_ed25519"      # its public half is key_file + ".pub"
sign_commits = true                 # git signs commits with this key

[github]
enabled = true
gh = "/opt/homebrew/bin/gh"         # optional; default "gh" on the PATH

[api-keys]
enabled = true
craze_providers = true              # optional; default true
env_files = ["~/.config/shed/keys.env"]

[envsecrets-login]
enabled = true
passphrase_file = "~/.config/envsecrets/passphrase"
# or: passphrase_command = ["security", "find-generic-password", "-s", "envsecrets", "-w"]

[files]
enabled = true
[[files.file]]
from = "~/.npmrc"
to = ".npmrc"
[[files.file]]
from = "~/work/widgets.env"
to = ".env"
base = "project"                    # "home" (default) or "project"
mode = "0400"                       # "0600" (default) or "0400"
only = { repos = ["example/widgets"] }
```

A passphrase command runs as an argv with no shell, in its own process group, with a time and output bound, and its output never reaches a message. The app never shows a stored command: the editor reports it as set and offers **Replace**, **Clear** or **Keep**.

### The `only` rules

Any provider, and any `files` entry, may say where it delivers:

```toml
[github.only]
servers = ["work"]                  # the server's name in ~/.shed/config.yaml
backends = ["remote"]               # "firecracker", "vz", "remote"
repos = ["example/*"]               # owner/name, `*` within a part
```

Every list given must match, an absent list does not restrict, and a list is never empty. A `files` entry is held to its provider's rules and its own. The file is read again for each request, so a change applies to the next create.

Only `servers` holds against a dishonest server: the app knows which server's stream a request came in on. `backends` and `repos` are read from the request, which the server writes. Use `backends` and `repos` to keep an honest server's sheds tidy, and `servers` to decide who can draw a provider's items at all.

## Preview

The preview shows what a named shed on a server would get: each item's name, provider, kind, stage, base, target, mode, size and fingerprint, and the problems a provider reported. It never shows a value. With no shed named, it shows what the providers have at all.

## A shed's detail and Deliver again

Each shed row shows its delivery state: delivered, partial, not delivered, none or not applicable, with the expired items and owed steps. Its detail lists the four steps, the last delivery and its failure, the problems and the items. **Deliver again** runs `shed provision` for that shed, one at a time per shed, even if you close the detail or switch panes. An admin looking at another user's shed sees the state alone and no Deliver again.

## What the app never shows

No pane, preview, status, log or IPC answer carries a delivered value, a passphrase or a passphrase command's arguments. Fingerprints are keyed hashes made with a key the app creates once under its state directory (`0600`). The last answers kept for status are redacted: names, targets, sizes and fingerprints only.

IPC ops for all of this (`delivery.status`, `delivery.settings`, `delivery.set`, `delivery.preview`, `shed.delivery`, `shed.provision`, `activity.server_list`) are listed on the [IPC](ipc.md) page. `activity.server_list` reads each server's [activity feed](../reference/api.md#get-apiactivity), which the Activity pane shows merged across servers.
