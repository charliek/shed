# CLI-written `config.yaml` fixture (written by Go, read by Go and Rust)

The desktop app and its credential broker read `~/.shed/config.yaml` with readers of their
own: shed-core's config reader (hand-written, not a YAML library) and the broker's
discovery (over `saphyr-parser`). What they promise is narrow: they read the file **as the
shed CLI writes it**. Every entry the CLI's own save path writes with ordinary values is
read with the same host, ssh port, user and `identity_file` the CLI reads; anything else is
refused by name with a reason, never read with defaults. These files are that promise.

| File | What it is |
|---|---|
| `config.yaml` | Written by the CLI's own load and save (`config.LoadClientConfigFromPath`, then `config.ClientConfig.Save`) from real structs: the widest file it writes for ordinary values. One entry sets every `ServerEntry` field; others have a name with a colon (`prod:blue`), an address and a port (`127.0.0.1:8443`), names the saver quotes (`team: blue`, `true`, `1234`, `build #1`), values with a `#`, a quote and non-ASCII letters, and the fewest fields the saver writes. The other top-level sections are there too: `default_server`, `sheds`, `create_timeout` and a `machines:` section the CLI kept. |
| `expected.json` | What the CLI holds for each server entry (`host`, `ssh_port`, `user`, `identity_file`) and `default_server`. The `machines:` block in `config.yaml` is not decoded; it is there to prove the CLI's save preserves it. |

**Do not edit by hand.** `internal/config/clientconfig_fixture_test.go` writes both files
under `UPDATE_CLIENTCONFIG=1` and checks them otherwise, so a stale fixture fails
`make test`; it also reads `config.yaml` back through the CLI's loader and requires every
field it set:

```bash
UPDATE_CLIENTCONFIG=1 go test ./internal/config -run TestClientConfigFixture
```

## Readers

- Go: `internal/config/clientconfig_fixture_test.go` (the writer and checker).
- Rust: `the_cli_written_config_is_read_as_the_cli_reads_it` in
  `crates/shed-core/src/config.rs` (servers, default server; no entry refused,
  the file not refused) and in `crates/shed-broker/src/discovery.rs` (one target per entry).

What the readers refuse instead of reading (a block scalar, a flow mapping, a tag, an
anchor, an explicit `? key`, a raw line break other than LF and CRLF), and that they read
only the first YAML document, is pinned by `both_readers_on_every_shape_of_the_review` in
`crates/shed-broker/src/discovery.rs`, beside what Go reads for each.
