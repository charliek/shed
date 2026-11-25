# Credentials

The desktop app is a person's **credential source**: when a shed is created, or `shed provision` runs, the server asks the app on the person's bound machine for the credentials the shed should hold, and the app answers from what you configured. Nothing is asked per use and nothing is stored by the server. What happens after the answer is on the [Credential delivery](../reference/delivery.md) page; the wire format is the [provision contract](../reference/provision-contract.md). On a server where the [account service](../guides/credentials-from-the-account-service.md) answers for you, the app is not asked at all.

The **Credentials** entry in the sidebar, between Agents and Activity, is where you set this up. Preferences holds General and Terminal only. The app holds one delivery stream per server and answers each request on its own, with no prompt.

!!! warning "Scope every provider to the servers you trust"
    The app opens a delivery stream to **every server in your shed config**, and the first machine that connects for you on a server becomes your source there. Of the [`only` rules](#the-only-rules), only `servers` is checked by the app against the server that actually asked. `backends` and `repos` are matched against what the server wrote in its request, so a dishonest server can satisfy them. A provider scoped to "every server this machine answers on" delivers its items to any configured server that asks. Scope each provider you turn on to the servers you trust with it.

## The Credentials view

The view has four parts. It reads and writes `~/.config/shed/delivery.toml`; a settings-file error shows its fixed text and where it is, never the file's content. Every text is chosen by a code, never taken from a server's words.

### Where

One row per configured server, saying who answers there for you:

| Row says | Meaning | Action |
|---|---|---|
| This machine | This machine is your source; the row shows the stream's state | **Stop answering from here** |
| Another machine, by name | A different machine of yours is bound | **Answer from here** |
| The account service | The account service answers; with its URL, and "not delivering to this server" when the server says so | none |
| Nobody | No source is bound there | **Answer from here** |
| Cannot be reached | The server did not answer, or its answer could not be read | none until it answers |

Each row also says whether this machine is **paused** there.

**Answer from here** is the pin `shed source use` makes. **Stop answering from here** is `shed source unbind` plus a pause that sticks: the server binds the first of your machines to connect, so an unbind alone would be undone within seconds by this app's own stream. The app keeps the servers it is paused on in `paused-servers.json` beside `machine.json` (mode `0600`), stops that server's stream first, then unbinds. A stop therefore survives restarts, and only **Answer from here** (or removing the server from your config) lifts it. If the file cannot be read, every server is treated as paused and the view says so. A server that answers for you through the account service has no action: its answer is not this machine's to give or withhold.

### What

One row per provider, titled and described from the provider's own descriptor, showing where its items land, its scope in words, and whether it is set up. Actions are **Set up**, **Edit**, **Turn off** and **Remove**. **Set up** opens the form. A provider cannot be turned on until its required fields are filled, in the form and again where the settings are written.

Scope is one of:

- **Every server this machine answers on, including ones you add later.** This writes no `only.servers`.
- **Only these**, with a tick for each such server.

Backends are three ticks (`firecracker`, `vz`, `remote`) and repos are patterns (`owner/name`, `*` within a part). The `files` form edits `mode` and a scope for each file. A stored passphrase command stays opaque: the form reports it as set and offers **Replace**, **Clear** or **Keep**.

### Check

**Check** shows what a shed would get. Pick a shed from the list, or a server with a backend and a repo. Before it runs, it says which commands it will run (`gh`, and the passphrase command, when those providers are set up). Pressing it re-reads the settings first and runs nothing if the commands it would run differ from the warning shown (a change to a path or a scope does not stop it; it is checked with the settings as they are). Items are listed in words, with details on request; an error is one of a fixed set of messages. It never shows a value.

### Recent deliveries

The last answers this machine gave: when, server, shed, items and problems. Names, targets, sizes and fingerprints only; never a value.

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

A passphrase command runs as an argv with no shell, in its own process group, with a time and output bound, and its output never reaches a message. The app never shows a stored command.

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

## A shed's detail and Deliver again

Each shed row shows its delivery state: delivered, partial, not delivered, none or not applicable, with the expired items and owed steps. Its detail lists the four steps, the last delivery and its failure, the problems and the items. **Deliver again** runs `shed provision` for that shed, one at a time per shed, even if you close the detail or switch panes. An admin looking at another user's shed sees the state alone and no Deliver again.

## What the app never shows

No pane, preview, status, log or IPC answer carries a delivered value, a passphrase or a passphrase command's arguments. Fingerprints are keyed hashes made with a key the app creates once under its state directory (`0600`). The last answers kept for status are redacted: names, targets, sizes and fingerprints only.

IPC ops for all of this (`delivery.status`, `delivery.settings`, `delivery.set`, `delivery.describe`, `delivery.preview`, `delivery.recent`, `sources.list`, `source.unbind`, `shed.delivery`, `shed.provision`, `activity.server_list`) are listed on the [IPC](ipc.md) page. `activity.server_list` reads each server's [activity feed](../reference/api.md#get-apiactivity), which the Activity pane shows merged across servers.
