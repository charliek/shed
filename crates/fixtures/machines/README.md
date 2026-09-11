# `machines:` decode fixture (vendored, two-language)

`sample.yaml` + `expected.json` are a **two-language fixture**, the repo's existing
twin-fixture convention (see `crates/fixtures/config_sample.yaml`, `crates/fixtures/roost-vectors/`):
one YAML input, one expected-decode JSON, asserted by BOTH:

- the Rust `shed_core::config::ShedConfig::parse` machines section
  (`crates/shed-core/src/config.rs`), and
- the Go tolerant decoder `internal/config/machines.go` (`ClientConfig.DecodeMachines`).

**Update both tests together when you touch either file here.** A change to `sample.yaml`
that only one side re-derives from is exactly the drift this fixture exists to prevent.

## What it covers

Four `machines:` entries:

- `bare` — name only. `host` defaults to the entry name, `ssh_port` to 22, every optional
  field absent.
- `full` — every field Go's decoder models (`host`, `user`, `ssh_port`, `known_hosts`) set
  explicitly.
- `withrc` — carries `rc_bin` (see "The one intentional asymmetry" below) plus an unknown
  key (`color`) neither language models. Both decoders must still decode it cleanly —
  unknown-key tolerance is part of the contract, not an oversight.
- `broken` — malformed: its value is a YAML scalar, not a mapping. Both decoders must SKIP
  it (Go additionally writes a one-line stderr note) and keep decoding the entries around
  it — one bad hand edit in `machines:` must never take down `shed list` or the roost
  provider's `list` phase (plan 019 §3.1).

## Order is part of the contract

`expected.json` lists entries **sorted by name**, because both decoders sort (`config.rs`'s
`machines.sort_by`, `machines.go`'s `sort.Slice`) rather than preserving YAML document
order — a provider menu should not reshuffle because someone hand-edited an entry into a
different spot in the file.

`sample.yaml`'s document order is therefore deliberately **not** alphabetical. An
already-sorted sample would let a decoder that merely preserved document order pass by
coincidence, which is exactly what happened to the first draft of this fixture. Both tests
assert the decoded name sequence against `expected.json`'s, not just membership.

## The one intentional asymmetry: `rc_bin`

`shed-core`'s `MachineEntry` models `rc_bin` (the Rust `machine.rs`/RC-session kickoff path
reads it). Go's `internal/config/machines.go` deliberately does **not** model it — plan 019
pin P7 puts `rc_bin` and the whole machine-RC surface out of scope for Go's roost-provider
work.

`expected.json` handles that explicitly rather than by accident: every entry carries an
`rc_bin` key (`null` when absent, the path string on `withrc`), and it is:

- **asserted** by the Rust test, which reads the full `MachineEntry` shape including
  `rc_bin`;
- **deliberately ignored** by the Go test, which decodes `expected.json` into a
  Go-shaped struct with no `rc_bin` field — Go's JSON decoder silently skips a key it has no
  field for, so the asymmetry is a documented decision here, not a silent gap in either
  test.

## Confirmed Go↔Rust divergences (outside this fixture's agreed subset)

The fixture above pins the subset both decoders **agree** on. The table below is
the opposite: edge cases verified live against both `ShedConfig::parse`
(`crates/shed-core/src/config.rs`) and `DecodeMachines`
(`internal/config/machines.go`) where they genuinely disagree. None of these is
reachable from a config `shed` itself writes — they only show up from a hand
edit — so none is "fixed" here; this table exists so the next person who hits
one of these in the wild doesn't waste time re-deriving it.

| input | Rust (`ShedConfig::parse`) | Go (`DecodeMachines`) |
|---|---|---|
| `mini: {host: x}` (inline flow map) | entry skipped | decoded |
| `ssh_port: 0` | 0 | 22 |
| duplicate `host:` in one entry | last value wins | entry skipped with a note |
| `known_hosts: "/tmp/known#hosts"` | `"/tmp/known` (mangled) | `/tmp/known#hosts` |

A couple of these are worth calling out explicitly so they don't get
mistaken for open bugs in THIS change:

- **`ssh_port: 0`** is the one remaining ssh_port divergence after Go's decoder
  was brought in line with Rust's u16-range-fallback behavior for every other
  out-of-range value (70000, -1, ...) — both those now fall back to 22 in both
  languages. Zero is different: Rust's u16 parse accepts it verbatim (0), while
  Go falls back to 22 same as any other out-of-range value. Port 0 is
  meaningless for ssh regardless of which language reads it, so this is left
  as-is rather than chased into parity.
- **The `known_hosts` row is a pre-existing bug in Rust's `yaml_lite`**, not a
  Go bug: `yaml_lite` strips everything from an unescaped `#` onward before it
  unquotes a scalar, so a `#` inside a quoted value truncates the string AND
  leaves the opening quote character in the result. Go's decoder (a real YAML
  library, `gopkg.in/yaml.v3`) handles the quoted `#` correctly. Fixing
  `yaml_lite` is out of scope for this change — it carries its own Swift
  byte-parity test (a hand-rolled indentation-based reader that agrees with
  Swift's own config parsing byte-for-byte) and deserves a change of its own
  rather than a drive-by fix here.

## Refreshing

If shed-core's `MachineEntry` or Go's `MachineEntry` gains/loses a field, update
`sample.yaml` and `expected.json` together, then re-run both:

```
cd crates && cargo test -p shed-core machines
go test ./internal/config/...
```
