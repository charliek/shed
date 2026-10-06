# craze 0.1.0+gx — recording provenance

Written by `crates/shed-craze/tests/live.rs` on each re-record. It is GENERATED:
edit `../README.md` (hand-maintained) for anything a run cannot know.

Recorded with:

```bash
SHED_CRAZE_LIVE=1 SHED_CRAZE_RECORD=1 SHED_CRAZE_BIN_DIR=<make craze-binaries> \
  cargo test -p shed-craze --features test-support --test live record_a_gx_session -- --nocapture
```

- craze: the pin's `craze` (`craze-8d5676c34a34`), a real hub under a scratch HOME/CRAZE_HOME/CRAZE_RUNTIME_DIR.
- provider: `gx` — gx `gx-v1.0.16-gx.12` on model `glm-5.3-flash` (the scratch gx config).
- `lane.ndjson`: every line of the lane's own connection from its `session.attach` on
  (`{dir, msg}`, craze's fixture shape), 62 lines — the host `hello` before it is never recorded.
- event kinds seen: done, meta, text, thought, tool, turn
