# opencode 1.18.29 wire fixtures

Recorded by `crates/shed-opencode/tests/live.rs` on 2026-09-08 with:

```bash
SHED_OPENCODE_LIVE=1 SHED_OPENCODE_RECORD=1 \
  cargo test -p shed-opencode --test live -- --nocapture
```

- `opencode --version`: `1.18.29`
- server: `opencode serve --port 0 --hostname 127.0.0.1` in a scratch project whose
  `opencode.json` sets `permission.bash = "ask"` and `model = "opencode/muse-spark-1.3-contributor-free"`.
- `event-frames.jsonl`: one line per `/event` SSE `data:` payload, in arrival order,
  verbatim.

## The keep-alive

opencode's `/event` has **no `server.heartbeat` variant**. The stream opens with
`server.connected` and its only idle traffic is whatever the effect encoder emits —
comment lines (`: …`), which carry no event and therefore appear NOWHERE in this
file. That is why `shed-opencode`'s stall timer counts BYTES received rather than
events decoded: a healthy but quiet stream produces zero lines here.

Re-record on an opencode upgrade into a directory named for the new version; do not
overwrite this one.
