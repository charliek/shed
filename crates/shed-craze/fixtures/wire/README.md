# The wire fixtures (plan 027 §3.11, C9)

Each `*.ndjson` file is one scripted scenario against a fresh `fakehost.Host`
(`internal/fakehost`'s `TestWireFixtures`, in `../wire_test.go`). A line is one
JSON object, one of:

- `{"conn": N, "dir": "c2s", "msg": {...}}` — a request the runner sends
  verbatim on connection `N` (dialed lazily, the first time its number is
  named). `N` has no relation to the host's own ids (`c-1`, `s-1`, …); it is
  only how this file tells its connections apart.
- `{"conn": N, "dir": "s2c", "msg": {...}}` — a line the host must write to
  connection `N` next, checked byte for byte (after the incarnation
  placeholder substitution below). `msg` may be absent
  (`{"conn": N, "dir": "s2c"}`): a bare marker meaning "one line is expected
  here, content not yet recorded" — `-update` fills it in; a plain run
  refuses to guess and fails outright, telling you to run `-update` first.
- `{"dir": "op", "op": {"name": "...", ...}}` — not a wire message at all: a
  host-side script step (`fakehost.Host.Do`), run against the Host directly,
  never sent or received over any connection. It is how a fixture drives the
  Stub without an agent: emitting text, opening an ask, restarting the engine
  (a new incarnation), and so on. `cmd/craze-fake-host`'s stdin reads the same
  shape.
- A c2s line may also carry `"invalid": true` (fixtures 10 and 26): it is
  deliberately not a well-formed request of a method protocol 1 defines with
  today's params (an unknown method, or a field no schema allows) — sent as it
  stands, and not held to the request schema, which such a line is designed
  never to pass. Every other line, c2s and s2c alike, is schema-checked
  (`internal/control/wiretest`) as it is sent or read.

## The incarnation placeholder

A Stub's event log mints its own random UUIDv7 incarnation — the one thing a
`Host` cannot pin deterministically (see `../doc.go`). So a fixture never
names a real incarnation: it writes `INCARNATION-1`, then `INCARNATION-2` after
the first `restart` op, and so on. The runner (`wire_test.go`) tracks the
Host's real incarnation at construction and after every `restart`, and does
the substitution both ways — real to placeholder in every line it reads off
the wire before comparing or recording it, placeholder to real in every c2s
line before sending it. Exact string replacement of values the Host itself
reported, never a guess at their shape.

## Determinism

Every other source of nondeterminism a real host would have is pinned by
`fakehost.Options`'s defaults: a fixed clock (2026-01-01T00:00:00Z, moved only
by an explicit `advance_clock` op), a fixed pseudo-random token source, a
fixed host id, craze version, pid, workspace and durable session id. Fixture
4 (a `slow_consumer` reset) and fixture 12 (an `omitted` reset) are made
deterministic from the host side alone — a small subscription budget the
client asks for (fixture 4: `budget.maxBytes`, small enough that a single
ordinary-sized push can never fit, so no race against a forwarder goroutine
decides the outcome) and a single event too large for any subscription to
carry (fixture 12: the `oversized_event` op) — never by racing a stalled
write against a fixed sleep. `stall_writes`/`resume_writes` exist as ops (see
`../host.go`) but neither of those two fixtures needed them once a
size-based trigger was found to be exactly reproducible under `-race`; an
earlier design that stalled the connection's writes and then raced a
duration-bounded burst of pushes against it was measured to be flaky under
`-race -count=20` and was replaced by the size-based design here.

## Re-recording

`go test ./internal/fakehost/... -run TestWireFixtures -update` re-records
every bare s2c marker (and re-validates every already-recorded line) from
each fixture's own c2s and op lines. The committed files here pass
`TestWireFixtures` without `-update`; run it `-race -count=20` to confirm a
fixture is not flaky before committing it.

## The fixtures

What each fixture exercises is listed once, in
[`docs/reference/protocol.md`'s "Fixtures and the fake host"](../../../../docs/reference/protocol.md#fixtures-and-the-fake-host),
beside the protocol it documents, so the list cannot drift from it here.
That section also covers what this file does not: a fixture's first line
`{"dir": "host", "host": {...}}`, which says how its host was built, and the
`"sock": "hub"` lines of a two-socket fixture, which run the real hub in
front of the host.
