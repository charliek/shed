# Epic: Roost Pivot — shed's part

> Not a docs page. This directory is deliberately outside `docs/` so
> Zensical never publishes it. It is a pointer plus the rules that apply
> in this repo — never a copy of the roadmap. It supersedes the roadmap
> section of `docs/discovery/remote-agents.md` from R4 onward; that file
> stays as the history of R0–R4.

**Why this exists — read first:**
https://claude.ai/code/artifact/add27f67-3d15-4541-bd3f-eda3f34fcc48
(Private — opens with the owner's claude.ai login. A 404 from anywhere
else is expected, not a broken link.)
Sections that matter here: §01 (what discovery measured — the coupling is
narrower than it looks), §03 (the layering rule), §05 (what dies, what
survives), §06 Tracks A and S, §08 (the release call).

**Tracking:** https://github.com/users/charliek/projects/4 —
`Epic: Roost Pivot`. Your PR body must contain `Closes charliek/shed#<n>`.
Status moves by itself when that merges. Never edit board status by hand.

## This repo's items

The issue is authoritative; this table is a map.

```bash
gh issue list -R charliek/shed --state open --search "in:title [S"
gh issue list -R charliek/shed --state open --search "in:title [A"
```

| ID | issue | phase | one line |
|---|---|---|---|
| S1 | [#323](https://github.com/charliek/shed/issues/323) | RP/M1 | adopt `roost-ipc` in `shed-core` (git dep until roost R2); one Rust client for every shed client |
| S3 | [#325](https://github.com/charliek/shed/issues/325) | RP/M1 | the Tauri app reads inventory + status from a `roost-session` |
| S2 | [#324](https://github.com/charliek/shed/issues/324) | RP/M2 | delete the pane anchors, stability engine, fixture corpora and their tests — after S3, never before |
| A5 | [#321](https://github.com/charliek/shed/issues/321) | RP/M2 | claude → status from roost; delete the transcript tail; claude.ai keeps control |
| A6 | [#322](https://github.com/charliek/shed/issues/322) | RP/M2 | codex + cursor → status from roost; delete the lanes, ingest, and gated input |
| A4 | [#320](https://github.com/charliek/shed/issues/320) | RP/M3 | the opencode lane as a standalone crate: transcript, prompt, interrupt, permission |
| S4 | [#326](https://github.com/charliek/shed/issues/326) | RP/M5 | the `shed` roost provider script — the kickoff path that replaced `sx` |
| S5 | [#327](https://github.com/charliek/shed/issues/327) | RP/M5 | `roost-session` inside sheds and on machines, via roost's bootstrap ladder |
| S6 | [#328](https://github.com/charliek/shed/issues/328) | RP/M6 | retire the RC hub, tmux driver, `shed-ext-rc`, Go engine, rc-parity oracle — **after S5** |
| S7 | [#329](https://github.com/charliek/shed/issues/329) | RP/M6 | ✅ **done (plan 016)** — `sx` sunset entirely: crate, release wiring and the rc-parity one-shot family deleted |

S3's mobile twin is **S3m** in `shed-mobile`
([charliek/shed-mobile#15](https://github.com/charliek/shed-mobile/issues/15))
— mobile is the priority client; both change.

**Order:** S1 → S3 is the only hard chain to M1 (opencode-only, both
clients). S2 only after S3 reads real status. S6 last, once nothing
consumes the hub. S7 is done (plan 016) — it had no blockers.

## Rules that apply in this repo

- **Never reintroduce pane scraping.** No new anchor regexes, no
  stability heuristics, no classifier fallbacks. Status is read from
  roost, never derived. One status authority per session.
- **`kind_features.feed` must say what that producer can actually
  stream, and the two producers differ.** Settled in M2 (plan 014),
  against #322's own wording, because accuracy beats the ticket text:
  - the **guest hub** says `feed: "none"` for claude-rc, codex and
    cursor. A5/A6 deleted their activity producers, so it has no signal
    at all for them. `""` would be wrong too — that means "field absent,
    producer predates v2" and sends a client back to `watch`.
  - shed's **roost-backed machine capabilities** say `feed: "activity"`
    for the same kinds, because roost reports lifecycle for them and the
    client folds it into a live activity dimension.

  So the same kind gets different answers depending on **where the
  session lives**. `feed` describes the *message* feed; no client gates
  its activity chip on it. The practical consequence, until S5 puts a
  roost-session inside sheds: **a codex or cursor session on a machine
  shows roost-sourced activity; the same kind on a shed shows liveness
  only, with no activity at all.** That is accepted, it is visible in
  the payload rather than hidden, and S5 is what closes it.
- **The hub is retired, not adapted.** Do not add roost as a provider
  *behind* the `/v1` hub; clients read roost directly through
  `shed-core`. Every hub route already has a home (§04 Q6).
- **Drive the session's own server, never a sidecar.** For opencode that
  means the `--port` the TUI was launched with — a separate server does
  not update a running TUI. Use legacy `GET /event`, not `/api/event`
  (which drops `session.idle`); `?after=` for gap-fill.
- **Keep what survives.** `kind_features` (its `attach` enum already has
  `native-remote`), the watcher stack (becomes the lanes), the transport
  and machine registry, the broker outside `rc_hub`, all client UI. Do not
  rewrite these; re-point them.
- **`tests/machine-transport`'s README overstates coverage** — the Dart
  leg it describes does not exist. Adopting `roost-ipc` dissolves the need;
  correct the README in S1 either way.
- **Release held to M6.** Do not cut a tag inside this epic; the trigger
  is demolition done, not features done. `sx` — the component this rule
  was originally written to hold back — was sunset unreleased in plan 016
  rather than shipped, so there is nothing left to keep untagged.
- **Mobile first.** Where a change lands in both clients, `shed-mobile`
  leads.

## Cross-repo edges

- **S1 ← roost R2.** Git dependency until roost publishes; the
  stable-local-port invariant from plan 012 must hold whichever SSH path
  S1 chooses.
- **S3m in shed-mobile ← S1.** Same FRB bridge; ships in M1 alongside S3.
- **A4 → shed-mobile.** The opencode crate is FRB-exposed for the phone.
- **S5 ↔ roost HS-3 bootstrap.** The ladder installs `roost-session` over
  SSH with verify-before-commit; decide rootfs-baked vs provisioned in S5.
- **S6 waits on S3m, A4, A5, A6, and S5** — everything off the hub first,
  *including* the live consumers: until S5 moves `prox-test` and mini3
  onto `roost-session`, they still run the hub/tmux stack, and retiring
  it would break them. Do not break them mid-epic.
