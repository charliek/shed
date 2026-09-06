# Epic: Roost Pivot — shed's part

> Not a docs page. This directory is deliberately outside `docs/` so
> Zensical never publishes it. It is a pointer plus the rules that apply
> in this repo — never a copy of the roadmap. It supersedes the roadmap
> section of `docs/discovery/remote-agents.md` from R4 onward; that file
> stays as the history of R0–R4.

**Why this exists — read first:**
https://claude.ai/code/artifact/add27f67-3d15-4541-bd3f-eda3f34fcc48
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
| S4 | [#326](https://github.com/charliek/shed/issues/326) | RP/M5 | the `shed` roost provider script — the `sx` replacement for kickoff |
| S5 | [#327](https://github.com/charliek/shed/issues/327) | RP/M5 | `roost-session` inside sheds and on machines, via roost's bootstrap ladder |
| S6 | [#328](https://github.com/charliek/shed/issues/328) | RP/M6 | retire the RC hub, tmux driver, `shed-ext-rc`, Go engine, rc-parity oracle |
| S7 | [#329](https://github.com/charliek/shed/issues/329) | RP/M6 | strip `sx` to an unreleased hello-world stub; keep the release wiring |

S3's mobile twin is **S3m** in `shed-mobile` (#15) — mobile is the
priority client; both change.

**Order:** S1 → S3 is the only hard chain to M1 (opencode-only, both
clients). S2 only after S3 reads real status. S6 last, once nothing
consumes the hub. Ready now with no blockers: S7.

## Rules that apply in this repo

- **Never reintroduce pane scraping.** No new anchor regexes, no
  stability heuristics, no classifier fallbacks. Status is read from
  roost, never derived. One status authority per session.
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
- **Release held to M6.** `sx` stays untagged. Do not cut a tag inside
  this epic; the trigger is demolition done, not features done.
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
- **S6 waits on S3m, A4, A5, A6** — everything off the hub first.
- Until S5 lands, `prox-test` and mini3 still run the old hub/tmux stack;
  do not break them mid-epic.
