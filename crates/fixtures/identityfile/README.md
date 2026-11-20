# `identity_file` fixture (written by Go, read by Go and Rust)

A server entry's `identity_file` is the SSH key that entry uses. Set, each ssh argv run
for the entry carries `-i <path> -o IdentitiesOnly=yes -o ControlMaster=no -o
ControlPath=none` (the key first, no key that is only in the agent, and no shared
connection another key opened), and each generated ssh config stanza carries
`IdentityFile "<path>"`, `IdentitiesOnly yes`, `ControlMaster no` and `ControlPath none`.
The one exception is a transport that keeps a shared connection of its own, keyed by
the identity file too, and gives its own `ControlMaster` and `ControlPath` (the CLI's
roost provider): it carries only `-i <path> -o IdentitiesOnly=yes`. Go (the CLI) and Rust
(shed-core's config reader and the broker's discovery) both read the field from
`~/.shed/config.yaml`, so they must agree on what each value means and which values are
refused. These files are that agreement.

| File | What it is |
|---|---|
| `config.yaml` | Accepted entries, written by the CLI's own saver (`config.ClientConfig.Save`), so the quoting is what a real `config.yaml` holds (a tab, for one, is written as the escape `\t`). |
| `refused.yaml` | Refused entries, hand-written the way a person would edit the file, with YAML's escapes for characters that cannot be typed (`\t`, `\_` for U+00A0, `\N` for U+0085, `\L` for U+2028, `\U00003000`). |
| `expected.json` | For each accepted entry: `identity_file` as decoded, `identity_args`, the whole `bootstrap_argv` (scope `control`, client kind `cli`, no CSR, known_hosts `/home/pat/.shed/known_hosts`), the `ssh_config_lines` the identity file adds, and the CLI's whole `ssh_config_stanza` for a shed named `box`. For each refused entry: the value it decodes to and the reason `class`. |

**Do not edit by hand.** `cmd/shed/identityfile_fixture_test.go` writes all three files
under `UPDATE_IDENTITYFILE=1` and checks them otherwise, so a stale fixture fails
`make test`:

```bash
UPDATE_IDENTITYFILE=1 go test ./cmd/shed -run TestIdentityFileFixture
```

## The rule

One rule, for every way the value arrives (the `--identity-file` flag, a hand-edited
`config.yaml`), in every language. A value is refused when:

| Class | Why |
|---|---|
| `nul`, `cr`, `lf` | An argv element or a config line cannot carry it. |
| `double_quote` | It would end the quoted `IdentityFile "<path>"` value. |
| `backslash` | ssh unescapes `\` in `-i` and `IdentityFile`. |
| `hash` | shed-core's config reader cuts a value at `#`. |
| `percent`, `dollar` | ssh expands `%` tokens and `${...}` environment references in `-i` and `IdentityFile`. |
| `leading_whitespace`, `trailing_whitespace`, `whitespace_only` | Unicode White_Space at either end, not only ASCII (Go `unicode.IsSpace`, Rust `char::is_whitespace`, which agree on every code point): the fixture has a tab, U+00A0, U+0085, U+2028 and U+3000. |
| `not_absolute` | It does not begin with `/` (`~` is expanded only by the CLI's flag, never in the file). |

An empty value means no identity file. Whitespace in the middle of a path (a space, a
tab, U+00A0), an apostrophe, a colon and non-ASCII letters are accepted.

**UTF-8.** A value must be valid UTF-8. Go refuses one that is not (`it is not valid
UTF-8`); YAML and a Rust `String` cannot hold one at all, so no such value can reach a
Rust reader, and the fixture has none.

## The file is checked at the time of use

The value rule above is all this fixture covers. Separately, at each use and before ssh
is spawned, the file must exist, be a regular file (a link to one is followed; it is
opened without blocking and the type is checked on what was opened) and be readable,
and its public key must be learnable without a passphrase: the file is a public key, or
a private key whose format carries its public key unencrypted (the OpenSSH format,
encrypted or not) or that is not encrypted, or else a `<path>.pub` beside it holds it;
a `.pub` that names another key than the file's own is refused. The CLI compares that
public key with the key the server enrolled (the issued certificate's subject). The
paths here do not exist, so a reader's test checks the value rule and the argv and
stanza it composes, not the file.

## Readers

- Go: `cmd/shed/identityfile_fixture_test.go` (the writer and checker).
- Rust: shed-core's config reader and the broker's discovery (plan 037 A8).
