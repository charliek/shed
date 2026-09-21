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
- `withrc` — carries a leftover `rc_bin` (see "The retired `rc_bin` field" below) plus an
  unknown key (`color`) — as of C8 (plan 022 S6), NEITHER language models either key. Both
  decoders must still decode it cleanly — unknown-key tolerance is part of the contract, not
  an oversight, and this entry is the negative control that pins it.
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

## The retired `rc_bin` field

Through plan 019, `shed-core`'s `MachineEntry` modeled `rc_bin` — where `sx`'s one-shot RC
binary lived on the remote — while Go's `internal/config/machines.go` deliberately did
**not** (pin P7 put `rc_bin` and the whole machine-RC surface out of scope for Go's
roost-provider work). `expected.json` used to carry an explicit `rc_bin` key on every entry
for exactly that reason: asserted by the Rust test, silently ignored by the Go one (whose
JSON decoder skips a key its struct has no field for).

`sx` was sunset, unreleased, in plan 016, and C8 (plan 022 S6) deleted the rest of the
plumbing built for it: `machine::rc_prefix`, `machine::DEFAULT_MACHINE_BIN`, and the
`rc_bin` field itself — `MachineEntry` no longer carries it in **either** language.
`expected.json` no longer has an `rc_bin` key at all.

The `withrc` entry stays, unchanged in `sample.yaml` (still `rc_bin:
/opt/homebrew/bin/shed-machine-rc`), because it is now doing a more important job: it is
the **tolerance negative control**. Both `MachineEntry`s dropped the field, but a config a
user wrote before that could still carry the key — and a decoder must not choke on a key
it no longer models any more than it chokes on one it never modeled. Comment out either
decoder's unknown-key tolerance and this entry (and its Go twin,
`TestDecodeMachines_UnknownKeysAndRcBinIgnored` in `internal/config/machines_test.go`) stops
decoding.

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
