/* The transcript composer's `/` menu — its KEYS and its DRAFT lifecycle
 * (plan 029 §3.5; P8, P9, P28).
 *
 * Every rule about the MENU itself — which rows show, what a typed token
 * matches, what accepting a row does, when a command is refused — is
 * `shed_app::slash_menu`'s, reached through the `lane_slash_*` commands (P5);
 * nothing here filters, parses or decides a command. What is left for the
 * client is what only a keyboard and a screen have: which key does what to the
 * menu on screen, where the highlight goes, when an Esc dismissal ends, which
 * draft a keypress still owns once its round-trips answer, and what becomes
 * of the draft once an outcome is known. Those are pure and
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
 *  back to the first row on EVERY filter change (P9), so the key is the
 *  token as well as the rows — `/r` → `/re` listing the same rows is a new
 *  filter, and its first row is highlighted — and a new catalog (new rows)
 *  likewise. An activity change (the same token and rows, other rows dimmed)
 *  is neither, and keeps the highlight. */
export function highlightKey(token: string, names: string[]): string {
  return JSON.stringify([token, ...names]);
}

/** The row an Enter or Tab accepts, from the filter answer for the draft it
 *  was pressed on (`fresh`): the row highlighted on screen when that answer
 *  IS the one on screen (`shown`: its [`highlightKey`] and the highlight's
 *  index), else the fresh answer's first row — the highlight a new filter
 *  starts on (P9). `undefined` with no rows. */
export function rowOnEnter(
  fresh: { token: string; names: string[] },
  shown: { key: string; index: number },
): string | undefined {
  if (fresh.names.length === 0) return undefined;
  const same = highlightKey(fresh.token, fresh.names) === shown.key;
  return (same ? fresh.names[shown.index] : undefined) ?? fresh.names[0];
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
  /** `/model`: the settings sheet opens on the model list. */
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
 *  | `ModelPicker` | cleared | the settings sheet opens |
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
