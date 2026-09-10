# gx 1.0.16+gx.12 — recording provenance

Written by `crates/shed-gx/tests/live.rs` on each re-record. It is
GENERATED: edit `README.md` (hand-maintained, beside this file) for anything
a run cannot know, and `../README.md` for the regeneration recipe.

Recorded on 2026-09-09 with:

```bash
SHED_GX_LIVE=1 SHED_GX_RECORD=1 \
  cargo test -p shed-gx --features test-support --test live -- --nocapture
```

- gx version (from the leader's own `healthz`/record): `1.0.16+gx.12`
- `history.json`: the body of `GET /v1/sessions/{id}/history?offset=-200&limit=200`, verbatim.
- `event-frames.jsonl`: 70 `event: update` payloads, verbatim, in the order a
  RESUMING client receives them — captured by reconnecting with `Last-Event-ID` set to
  the first id in the history page above, not by teeing the live stream.
- `approvals.jsonl`: one approval resource per line, verbatim, from `GET …/approvals`.

## What the run saw

- `live session`
- `live message/text`
- `live message/reasoning`
- `live message/tool_use`
- `live message/approval_request`
- `replayed update/hook_execution`
- `replayed update/user_message_chunk`
- `replayed update/agent_message_chunk`
- `replayed update/turn_completed`
- `replayed update/agent_thought_chunk`
- `replayed update/tool_call`
- `replayed update/tool_call_update`
- `replayed update/pending_interaction`
- `replayed update/interaction_resolved`
- `approvals.jsonl: 3 resource(s) captured while pending`

Re-record on a gx upgrade into a directory named for the new version; do not
overwrite this one.
