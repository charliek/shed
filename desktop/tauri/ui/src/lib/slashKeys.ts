/* The transcript composer's `/` menu — its KEYS and its DRAFT lifecycle
 * (plan 029 §3.5; P8, P9, P28).
 *
 * Every rule about the MENU itself — which rows show, what a typed token
 * matches, what accepting a row does, when a command is refused — is
 * `shed_app::slash_menu`'s, reached through the `lane_slash_*` commands (P5);
 * nothing here filters, parses or decides a command. What is left for the
 * client is what only a keyboard and a screen have: which key does what to the
 * menu on screen, where the highlight goes, when an Esc dismissal ends, which
 * draft a keypress still owns once its round-trips answer, that one key's
 * operation runs at a time, and what becomes of the draft once an outcome is
 * known. Those are pure (the sequencer takes its round-trips injected) and
 * dependency-free so node's own test runner pins them (`test/slashKeys.test.mjs`,
 * the `laneVerbs.ts` arrangement) — the composer (`LanePanel.tsx`) and its
 * report both read them. */

/** One `keydown` as the composer sees it. `composing`: an IME composition is
 *  in progress (`isComposing`, or the legacy keyCode 229). */
export type ComposerKey = { key: string; shift?: boolean; composing?: boolean };

/** What a key does:
 *
 *  - `none` — not the composer's: the browser's own default happens (Shift+
 *    Enter's newline, Tab's focus move), and the event goes on as usual;
 *  - `send` — Enter with no menu up: the draft is SENT (through the shared
 *    dispatch when the session has a menu, as text when it has none);
 *  - `accept` — Enter with the menu up: the highlighted row is accepted with
 *    Enter (the shared rule then decides: a token that already spells the row
 *    sends, anything else selects it — P9);
 *  - `complete` — Tab with the menu up: the highlighted row completes;
 *  - `dismiss` — Esc with the menu up: the menu closes, and the Esc stops here
 *    (an open dialog's Esc handler must not also see it);
 *  - `up` / `down` — the highlight moves instead of the caret (P28). */
export type KeyAction = "none" | "send" | "accept" | "complete" | "dismiss" | "up" | "down";

/** **The key table** (P9). `shown` is the menu as it is ON SCREEN
 *  ([`menuUp`]). Every menu key is ignored while composing (an IME's Enter
 *  commits its composition, never a send), and so is the composer's own
 *  Enter-to-send; Shift+Enter is always a newline. With no menu up, Enter
 *  sends — zero matches included — and Tab, Esc and the arrows are the
 *  browser's. */
export function composerKey(k: ComposerKey, shown: boolean): KeyAction {
  if (k.composing) return "none";
  switch (k.key) {
    case "Enter":
      if (k.shift) return "none";
      return shown ? "accept" : "send";
    case "Tab":
      return shown && !k.shift ? "complete" : "none";
    case "Escape":
      return shown ? "dismiss" : "none";
    case "ArrowUp":
      return shown ? "up" : "none";
    case "ArrowDown":
      return shown ? "down" : "none";
    default:
      return "none";
  }
}

/** Whether a native `keydown` is part of an IME composition: `isComposing`,
 *  or keyCode 229 — what WebKit reports for a key an input method took. */
export function isComposing(e: { isComposing?: boolean; keyCode?: number }): boolean {
  return e.isComposing === true || e.keyCode === 229;
}

/** Whether the menu is ON SCREEN: the shared filter says it is open (which
 *  is "it has rows"), and it has not been dismissed with Esc for this very
 *  token. */
export function menuUp(filter: { open: boolean; token: string } | null | undefined, dismissed: string | null): boolean {
  return !!filter?.open && filter.token !== dismissed;
}

/** **An Esc dismissal lasts until the token changes** (P9): the token it was
 *  given for, kept while the token is still that one, and gone for good once
 *  it is not — so typing on reopens the menu, and typing back does not
 *  re-close it.
 *
 *  `token` is what the latest filter ANSWER says — `null` while there is none
 *  for the menu on screen (a replaced catalog awaiting its filter). No answer
 *  is no news about the token: the dismissal stands, so a catalog refresh
 *  never reopens a menu dismissed for a token nobody changed. */
export function dismissalAfter(dismissed: string | null, token: string | null): string | null {
  if (token === null) return dismissed;
  return dismissed === token ? dismissed : null;
}

/** The highlight after ↑ (`-1`) or ↓ (`+1`) over `count` rows, wrapping at
 *  either end. */
export function moveHighlight(index: number, delta: 1 | -1, count: number): number {
  if (count <= 0) return 0;
  return (((index + delta) % count) + count) % count;
}

/** The identity of a filter answer — what the highlight is reset by: it goes
 *  back to where a new filter starts (the answer's `start`) on EVERY filter
 *  change (P9), so the key is the catalog the answer was filtered from (the
 *  menu's JSON), the token and the rows — `/r` → `/re` listing the same rows
 *  is a new filter, a new catalog is one even when it lists the same names
 *  in the same order (another action behind a row, plan 030 FU7), and new
 *  rows likewise. An activity change (the same catalog, token and rows,
 *  other rows dimmed) is none of these, and keeps the highlight where it is
 *  — on a row that became dimmed too (plan 030 §3.4). */
export function highlightKey(catalog: string, token: string, names: string[]): string {
  return JSON.stringify([catalog, token, ...names]);
}

/** A filter answer as an Enter or Tab reads it: whether the menu is open,
 *  its token, its rows' names, the row its highlight starts on (`start`),
 *  and the catalog it was filtered from (the menu's JSON). */
export type KeyFilter = { open: boolean; token: string; names: string[]; start: number; catalog: string };

/** The row an Enter or Tab accepts, from the filter answer for the draft it
 *  was pressed on (`fresh`): the row highlighted on screen when that answer
 *  IS the one on screen (`shown`: its [`highlightKey`] — catalog, token and
 *  rows — and the highlight's index), else the row the fresh answer's
 *  highlight starts on (its `start`: the first row a turn does not refuse) —
 *  where a new filter's highlight starts (P9). An answer for another catalog
 *  than the one on screen is never the one on screen, however alike their
 *  names (plan 030 §3.5). `undefined` with no rows. */
export function rowOnEnter(
  fresh: Pick<KeyFilter, "catalog" | "token" | "names" | "start">,
  shown: { key: string; index: number },
): string | undefined {
  if (fresh.names.length === 0) return undefined;
  const same = highlightKey(fresh.catalog, fresh.token, fresh.names) === shown.key;
  return (same ? fresh.names[shown.index] : undefined) ?? fresh.names[fresh.start] ?? fresh.names[0];
}

// ---- who owns the draft ----------------------------------------------------

/** The composer's draft, and its REVISION: a count that moves on every change
 *  to the text — typing, a programmatic draft (a completion, a clear, the
 *  type door) — and never goes back. Each Enter, Send or pick claims the
 *  revision it was pressed on, BEFORE its first round-trip, and carries that
 *  claim through filter → accept → dispatch → the verb. */
export type DraftRev = { text: string; rev: number };

/** The draft after it is set to `text`: a new revision when the text changed,
 *  the same one when it did not (a re-set of the same text — the type door
 *  re-sent — is no new draft). */
export function revise(d: DraftRev, text: string): DraftRev {
  return text === d.text ? d : { text, rev: d.rev + 1 };
}

/** **Does a claim still own the draft?** — the revision it was pressed on is
 *  still the composer's. A keypress whose round-trips (filter, accept,
 *  dispatch) end on a moved revision was for a draft that no longer exists:
 *  it is DROPPED, silently, before any verb is sent. */
export function owns(claimed: number, current: number): boolean {
  return claimed === current;
}

// ---- one key at a time (E15) -----------------------------------------------

/** The composer's one-at-a-time guard: held from the moment an Enter, Tab,
 *  Send or pick is pressed until its whole operation — filter, accept,
 *  dispatch, the verb — has settled. */
export type Guard = { held: boolean };

/** **Run `op` only if no other key's operation is running**, holding `guard`
 *  until it settles — returned, dropped or thrown (`finally`: a dropped or
 *  failed key never strands it). The guard is taken SYNCHRONOUSLY, before
 *  `op`'s first await, so a second key pressed while the first's filter is
 *  still out is dropped then and there: two pending answers for one draft
 *  would otherwise each go on to send it (the branch review's finding 4: the
 *  first's verb is lost — `outcome_unknown`, the draft kept — and the
 *  second's filter answer, landing after, sends that draft AGAIN with no new
 *  keypress). */
export async function oneAtATime(guard: Guard, op: () => Promise<void>): Promise<void> {
  if (guard.held) return;
  guard.held = true;
  try {
    await op();
  } finally {
    guard.held = false;
  }
}

/** The round-trips an Enter or Tab makes, for the draft it claimed:
 *
 *  - `filter` — that draft's filter (the one on screen when it answered
 *    these very inputs, else a fresh one), with the catalog it answered for;
 *    `null` with no menu;
 *  - `owned` — whether the claim still owns the draft ([`owns`]), read after
 *    the filter answered;
 *  - `dismissed` — the Esc dismissal as it is once the filter answered;
 *  - `send` — the draft sent (the shared dispatch, or text);
 *  - `accept` — the row accepted with this key, against the catalog the
 *    filter answered for (`catalog`): when that is no longer the session's
 *    menu, nothing runs — the menu moved under the key, and its own
 *    re-filter shows where (plan 030 §3.5). */
export type KeySteps = {
  filter: () => Promise<KeyFilter | null>;
  owned: () => boolean;
  dismissed: () => string | null;
  send: () => Promise<void>;
  accept: (name: string, catalog: string) => Promise<void>;
};

/** **An Enter or Tab, start to finish** (P9, E15), inside the guard
 *  ([`oneAtATime`]) for the WHOLE operation: the claimed draft's filter; a
 *  key whose draft moved on meanwhile is dropped; with no menu up an Enter
 *  sends and a Tab does nothing; with the menu up the row [`rowOnEnter`]
 *  names is accepted, against the catalog that filter answered for. */
export function pressKey(guard: Guard, key: "enter" | "tab", shown: { key: string; index: number }, s: KeySteps): Promise<void> {
  return oneAtATime(guard, async () => {
    const f = await s.filter();
    if (!s.owned()) return;
    if (!f || !menuUp(f, s.dismissed())) {
      if (key === "enter") await s.send();
      return;
    }
    const name = rowOnEnter(f, shown);
    if (name) await s.accept(name, f.catalog);
  });
}

// ---- the draft lifecycle (§3.5's table, P8) --------------------------------

/** What became of a command the composer ran. */
export type SlashOutcome =
  /** The shared guard refused it, nothing sent (`Refuse`). */
  | { kind: "refused"; reason: string }
  /** The verb succeeded. */
  | { kind: "done" }
  /** A rename that TOOK and craze could not record (`index_write`): the
   *  backend answers it as a success with a warning (plan 029 E4). */
  | { kind: "warning"; text: string }
  /** The verb failed — craze said no (`code`, its words), or its answer was
   *  lost (`outcome_unknown`). */
  | { kind: "failed"; code: string; message: string }
  /** `/exit`'s confirm was answered Keep: nothing was sent. */
  | { kind: "cancelled" }
  /** `/model`, or a bare `/effort` (plan 032 §3.2): the settings sheet
   *  opens — its model list, its effort row. */
  | { kind: "picker" };

/** The line a composer note shows: a refusal (the draft kept to fix) or a
 *  warning (something that took, with a caveat). */
export type ComposerNote = { tone: "refusal" | "warning"; text: string };

/** What the composer says when a command's answer was lost: it may have run,
 *  it is never resent, and the person checks before sending again. */
export const MAY_HAVE_APPLIED = "may have applied — check the session";

/** **The draft after an outcome** (§3.5's table, exactly):
 *
 *  | outcome | draft | shown |
 *  |---|---|---|
 *  | success | cleared | — |
 *  | `Refuse` | kept | the reason, inline |
 *  | a definite refusal from craze | kept | craze's words |
 *  | outcome unknown | kept | "may have applied — check the session" |
 *  | Stop's confirm cancelled | kept | — (nothing sent) |
 *  | `ModelPicker`, `EffortPicker` | cleared | the settings sheet opens |
 *  | `SetTitle` with `index_write` | cleared | the warning |
 */
export function draftAfter(o: SlashOutcome): { clear: boolean; note: ComposerNote | null; openSettings: boolean } {
  switch (o.kind) {
    case "done":
      return { clear: true, note: null, openSettings: false };
    case "refused":
      return { clear: false, note: { tone: "refusal", text: o.reason }, openSettings: false };
    case "failed":
      return {
        clear: false,
        note: { tone: "refusal", text: o.code === "outcome_unknown" ? MAY_HAVE_APPLIED : o.message || o.code },
        openSettings: false,
      };
    case "cancelled":
      return { clear: false, note: null, openSettings: false };
    case "picker":
      return { clear: true, note: null, openSettings: true };
    case "warning":
      return { clear: true, note: { tone: "warning", text: o.text }, openSettings: false };
  }
}

/** **An outcome, landing on the draft it was sent from** (§3.5's table, and
 *  draft ownership): a verb, once sent, always says what became of it — its
 *  note, warning or refusal shows — but it CLEARS the draft only while
 *  `owned` ([`owns`]): a newer draft typed while it was pending is never
 *  erased by an earlier command's answer. */
export function settleFor(
  o: SlashOutcome,
  owned: boolean,
): { clear: boolean; note: ComposerNote | null; openSettings: boolean } {
  const after = draftAfter(o);
  return { ...after, clear: after.clear && owned };
}
