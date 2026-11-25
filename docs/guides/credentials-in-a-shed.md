# Credentials in a shed

A shed can hold the credentials your work needs: a key to clone a private repository, a GitHub token, the API keys your coding agents use, a login for your secrets store. They get there by **delivery**: when a shed is created, or when you run `shed provision`, the shed desktop app on your own machine answers the server with files and environment variables, and the shed's agent writes them in. Nothing is asked per use, and nothing is brokered while you work.

This guide says what each provider delivers, how to sign in to AWS and to a container registry inside a shed, what a shed can reach once it holds these, and what you are trading for the convenience. The mechanics are on the [Credential delivery](../reference/delivery.md) page; the app's **Credentials** view, where you set the providers up, is described under [Desktop credentials](../desktop/credentials.md). On a server whose users come from an [account service](credentials-from-the-account-service.md), the service can answer instead of your machine.

## The trade

A usable credential is **inside the shed while it runs.** Anything in the shed can read it: your own commands, the repository's install hook, and any coding agent you run there. There is no signing check and no approval step any more, so nothing stops a process in the shed from using, copying or printing what you delivered. Files are written `0600` (or `0400`) for the shed user, and they are plain files: they are in snapshots, and on a stopped cloud shed's disk.

Decide what a shed holds the way you would decide what to put on a shared build machine:

- Deliver the least that does the job, and prefer a key made for sheds over your personal one.
- Prefer credentials the provider can scope or revoke (a fine-grained token, a read-only key).
- Put an [`only` rule](../desktop/credentials.md#the-only-rules) on every provider, and above all `only.servers`, so a server you do not trust cannot draw it.
- A shed assigned to a person with `shed assign` may already hold what someone else put there. A `shed provision` of such a shed writes your credentials into a filesystem you did not build.

The server sees the values in transit but never records, logs or returns them. See [What "no values" covers](../reference/delivery.md#what-no-values-covers).

## The five providers

You set each up in the desktop app's **Credentials** view (or in `~/.config/shed/delivery.toml`); a provider cannot be turned on until its required fields are filled. A provider that is absent or off delivers nothing.

### Who answers

On a server whose users are your own pinned machines, the desktop app answers: the first of your machines to connect becomes your source, and the Credentials view's **Answer from here** and **Stop answering from here** move or stop it. On a server whose users come from an account service (a directory server), the account service answers for those users, other than the server's local admins, with the values you stored there, and a pinned machine is not asked: the view shows the service as the source and offers no action. Your `delivery.toml` is then not used for those users on that server. Which one answers is shown per server in the Credentials view.

| Provider | What a shed gets | Settings |
|---|---|---|
| `ssh-key` | One key pair, `~/.ssh/id_<type>` (`0600`) and `.pub` (`0644`). A key with a passphrase is refused, and so is a missing public half. | `key_file`; `sign_commits` makes git in the shed sign commits with the key |
| `github` | Your `gh` sign-in as `~/.config/gh/hosts.yml`, and a git config that makes `gh` the credential helper for github.com | `gh`, the program to run (default `gh` on the PATH) |
| `api-keys` | craze's `~/.craze/native/providers.toml`, and one environment variable per `KEY=VALUE` line of each env file you name. The ChatGPT plan's tokens are never read. | `craze_providers` (default on); `env_files`, each `0600` or stricter |
| `envsecrets-login` | `~/.envsecrets/config.yaml` byte for byte, and its passphrase as the environment variable the config names (`passphrase_env`), so the shed pulls secrets for itself. A config with no `passphrase_env` or no `gcs_credentials` is a problem and delivers nothing. | `passphrase_file` or `passphrase_command` (one of them) |
| `files` | Files copied as they are, to a target under the home directory or, after the clone, the project directory, with mode `0600` or `0400` | a list of `from`, `to`, `base`, `mode`, `only` |

Each provider and each `files` entry accepts an `only` rule with `servers`, `backends` and `repos`; every list given must match. Only `servers` holds against a dishonest server. See [the `only` rules](../desktop/credentials.md#the-only-rules).

A value delivered at stage 1 reaches the environment of commands and sessions started after the delivery. A process already running (a tmux server, a roost session, a craze hub) keeps the environment it started with.

### The envsecrets login: use a read-only key

The envsecrets login lets the shed read every project's secrets, and, if the Google credentials the config names can write to the bucket, change them. Make a **read-only** bucket key for sheds, point a separate envsecrets config at it, and deliver that config instead of the one you use at your desk. A shed that only needs to pull secrets then cannot alter or delete them.

## AWS and registries: sign in inside the shed

Shed no longer brokers AWS credentials or Docker registry logins. There is no credential endpoint, no `AWS_CONTAINER_CREDENTIALS_FULL_URI`, and no Docker `credsStore` helper in a new shed. AWS and a container registry are ordinary sign-ins, done where the work is:

- **AWS.** Sign in inside the shed the way you would on any machine (`aws sso login`, or a profile in `~/.aws/config`), or give the shed short-lived keys through the `api-keys` provider's `env_files` (`AWS_ACCESS_KEY_ID` and the rest as `KEY=VALUE` lines). The SDK and CLI then find them through their usual lookup. Prefer a role with a short session and the least access the work needs.
- **A container registry.** Run `docker login <registry>` inside the shed (Docker stores it in `~/.docker/config.json`), or deliver a login file with the `files` provider, for example `from = "~/.config/shed/registry-docker-config.json"`, `to = ".docker/config.json"`, with a **read-only** pull key.
- **Another CLI's sign-in.** Anything that keeps its login in a file can be delivered the same way with the `files` provider; anything that takes a token in the environment can come through `env_files`.

Delivered files and values are static. Nothing refreshes them: an expired item stays where it is, `shed start` prints one warning per expired item, and `shed provision <name>` fetches new ones.

## What a shed can reach

With the providers above on and a connected source, a new shed can:

- clone a private repository over SSH with the delivered key, or over HTTPS through `gh`, before the install hook runs, and push to it;
- call GitHub's API with your `gh` token, and sign commits if you asked for it;
- run a coding agent that finds its provider keys in craze's `providers.toml` and in the environment;
- pull secrets from your envsecrets bucket with your envsecrets login;
- use any other credential you named in `files` or an env file.

Network reach is still set by the shed's [egress profile](../reference/egress.md): a credential in the shed is useful only to the hosts the profile lets it talk to, and it is also readable by anything the profile lets out. An isolated shed with a tight profile is the strongest limit on where a delivered credential can go.

## Where this fails

| You see | Meaning | Do |
|---|---|---|
| `Credentials were not delivered (source_offline)` | Your source machine is not connected | Open the app on that machine, then `shed provision <name>` |
| The private clone failed | No key or token was delivered before the clone | `shed provision <name>` with the source connected |
| A provider shows a problem in the Preview | For example a key with a passphrase, or an env file not `0600` | Fix the file; the Preview names the provider and the reason, never a value |
| A delivered value is missing in a long-running process | It keeps the environment it started with | Restart the process, or run the command through `shed exec` |

See [Credential delivery](../reference/delivery.md#failure-and-the-warning) for every reason, and [Credential Source](../extensions/source-binding.md) for which machine is asked.
