/* The transcript composer's `/` menu keys and draft lifecycle
 * (`src/lib/slashKeys.ts`, plan 029 §3.5; P8, P9, P28), on node's own test
 * runner — the `laneVerbs.test.mjs` arrangement.
 *
 * The cases marked CONTROL pin what the plan's panel named: IME composition
 * never sends or accepts; Shift+Enter is always a newline; with the menu
 * closed (zero matches included) Enter SENDS; Esc closes the menu and only the
 * menu; and the draft lifecycle is §3.5's table, row for row. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  MAY_HAVE_APPLIED,
  composerKey,
  dismissalAfter,
  draftAfter,
  highlightKey,
  isComposing,
  menuUp,
  moveHighlight,
  oneAtATime,
  owns,
  pressKey,
  revise,
  rowOnEnter,
  settleFor,
} from "../dist-test/slashKeys.js";

const KEYS = ["Enter", "Tab", "Escape", "ArrowUp", "ArrowDown"];

test("the key table with the menu UP", () => {
  assert.equal(composerKey({ key: "Enter" }, true), "accept");
  assert.equal(composerKey({ key: "Tab" }, true), "complete");
  assert.equal(composerKey({ key: "Escape" }, true), "dismiss");
  assert.equal(composerKey({ key: "ArrowUp" }, true), "up");
  assert.equal(composerKey({ key: "ArrowDown" }, true), "down");
  assert.equal(composerKey({ key: "a" }, true), "none", "typing is typing");
  assert.equal(composerKey({ key: "Tab", shift: true }, true), "none", "Shift+Tab moves focus back");
});

test("CONTROL: with the menu closed — zero matches included — Enter sends and the rest are the browser's", () => {
  assert.equal(composerKey({ key: "Enter" }, false), "send");
  for (const key of ["Tab", "Escape", "ArrowUp", "ArrowDown", "a"]) {
    assert.equal(composerKey({ key }, false), "none", key);
  }
});

test("CONTROL: Shift+Enter is a newline, open menu or not", () => {
  assert.equal(composerKey({ key: "Enter", shift: true }, true), "none");
  assert.equal(composerKey({ key: "Enter", shift: true }, false), "none");
});

test("CONTROL: every key is ignored while an IME composes — the menu's and Enter-to-send alike", () => {
  for (const up of [true, false]) {
    for (const key of KEYS) {
      assert.equal(composerKey({ key, composing: true }, up), "none", `${key} up=${up}`);
      assert.equal(composerKey({ key, shift: true, composing: true }, up), "none", `shift ${key} up=${up}`);
    }
  }
});

test("composing is isComposing, or WebKit's keyCode 229", () => {
  assert.equal(isComposing({ isComposing: true }), true);
  assert.equal(isComposing({ isComposing: false, keyCode: 229 }), true);
  assert.equal(isComposing({ isComposing: false, keyCode: 13 }), false);
  assert.equal(isComposing({}), false);
});

test("CONTROL: the menu is up only while the filter says open and Esc has not dismissed this token", () => {
  assert.equal(menuUp({ open: true, token: "/re" }, null), true);
  assert.equal(menuUp({ open: true, token: "/re" }, "/re"), false, "dismissed for this token");
  assert.equal(menuUp({ open: true, token: "/ren" }, "/re"), true, "another token");
  assert.equal(menuUp({ open: false, token: "/zz" }, null), false, "zero matches is a closed menu");
  assert.equal(menuUp(null, null), false, "no menu at all");
});

test("an Esc dismissal lasts exactly until the token changes", () => {
  assert.equal(dismissalAfter("/re", "/re"), "/re", "the same token: still dismissed");
  assert.equal(dismissalAfter("/re", "/ren"), null, "typed on: the menu is back");
  assert.equal(dismissalAfter(null, "/re"), null);
  // Once gone it stays gone: typing back to the old token does not re-close.
  let d = dismissalAfter("/re", "/ren");
  d = dismissalAfter(d, "/re");
  assert.equal(d, null);
});

test("CONTROL: a catalog refresh (no filter answer yet) does not end an Esc dismissal", () => {
  // The menu on screen was replaced: its filter is pending (`null`) — no news
  // about the token, so the dismissal stands…
  assert.equal(dismissalAfter("/re", null), "/re", "a pending filter changes nothing");
  // …and when the replacement's answer says the SAME token, it still stands:
  // the menu stays closed for a token nobody changed.
  let d = dismissalAfter("/re", null);
  d = dismissalAfter(d, "/re");
  assert.equal(d, "/re");
  assert.equal(menuUp({ open: true, token: "/re" }, d), false, "the refreshed menu stays dismissed");
  // An answer with ANOTHER token ends it, as typing does.
  assert.equal(dismissalAfter(d, "/ren"), null);
  assert.equal(dismissalAfter(null, null), null);
});

test("the highlight wraps at either end", () => {
  assert.equal(moveHighlight(0, 1, 3), 1);
  assert.equal(moveHighlight(2, 1, 3), 0);
  assert.equal(moveHighlight(0, -1, 3), 2);
  assert.equal(moveHighlight(1, -1, 3), 0);
  assert.equal(moveHighlight(4, 1, 0), 0, "no rows: the first");
});

test("CONTROL: the highlight resets on EVERY filter change — the token as well as the rows", () => {
  const rows = ["rename", "research"];
  assert.equal(highlightKey("/r", rows), highlightKey("/r", [...rows]), "the same answer: the highlight stays");
  // `/r` → `/re` lists the very same rows: still a new filter (P9).
  assert.notEqual(highlightKey("/r", rows), highlightKey("/re", rows), "a new token, the same rows");
  assert.notEqual(highlightKey("/r", rows), highlightKey("/r", ["research", "rename"]), "reordered rows");
  assert.notEqual(highlightKey("/r", ["rename"]), highlightKey("/r", rows), "new rows (a new catalog)");
  // No token can be mistaken for a row (or the other way round).
  assert.notEqual(highlightKey("/a", ["b"]), highlightKey("/a\nb", []));
});

test("Enter accepts the highlighted row only when the fresh answer IS the one on screen", () => {
  const rows = ["rename", "research"];
  const shown = { key: highlightKey("/r", rows), index: 1 }; // ↓ to research
  assert.equal(rowOnEnter({ token: "/r", names: rows }, shown), "research", "the highlight on screen");
  // An Enter on `/re` before its answer was on screen: the same rows, a new
  // filter — whose highlight starts on its first row.
  assert.equal(rowOnEnter({ token: "/re", names: rows }, shown), "rename");
  assert.equal(rowOnEnter({ token: "/r", names: ["research"] }, shown), "research", "new rows: the first");
  assert.equal(rowOnEnter({ token: "/zz", names: [] }, shown), undefined, "no rows: nothing to accept");
  assert.equal(rowOnEnter({ token: "/r", names: rows }, { key: shown.key, index: 9 }), "rename",
    "an index past the rows: the first");
});

test("CONTROL: the draft lifecycle is §3.5's table, row for row", () => {
  assert.deepEqual(draftAfter({ kind: "done" }), { clear: true, note: null, openSettings: false }, "success");
  assert.deepEqual(
    draftAfter({ kind: "refused", reason: "/help: not available here yet" }),
    { clear: false, note: { tone: "refusal", text: "/help: not available here yet" }, openSettings: false },
    "Refuse: kept, the reason inline",
  );
  assert.deepEqual(
    draftAfter({ kind: "failed", code: "bad_request", message: "params.title must not be empty" }),
    { clear: false, note: { tone: "refusal", text: "params.title must not be empty" }, openSettings: false },
    "a definite refusal from craze: kept, craze's words",
  );
  assert.deepEqual(
    draftAfter({ kind: "failed", code: "outcome_unknown", message: "outcome unknown: the connection dropped" }),
    { clear: false, note: { tone: "refusal", text: MAY_HAVE_APPLIED }, openSettings: false },
    "outcome unknown: kept, may have applied",
  );
  assert.equal(MAY_HAVE_APPLIED, "may have applied — check the session");
  assert.deepEqual(draftAfter({ kind: "cancelled" }), { clear: false, note: null, openSettings: false },
    "Stop's confirm cancelled: kept, nothing said");
  assert.deepEqual(draftAfter({ kind: "picker" }), { clear: true, note: null, openSettings: true },
    "ModelPicker: cleared, the sheet opens");
  const warning = "renamed, but craze could not record it: disk full";
  assert.deepEqual(
    draftAfter({ kind: "warning", text: warning }),
    { clear: true, note: { tone: "warning", text: warning }, openSettings: false },
    "SetTitle's index_write: cleared, the warning",
  );
});

test("a refusal with no words still says its code", () => {
  assert.equal(draftAfter({ kind: "failed", code: "not_accepting", message: "" }).note.text, "not_accepting");
});

// ---- draft ownership (review c4 items 1 and 3) -------------------------------

test("the draft's revision moves on every change to the text, and only then", () => {
  let d = { text: "", rev: 0 };
  d = revise(d, "/rename x");
  assert.deepEqual(d, { text: "/rename x", rev: 1 }, "typing");
  assert.equal(revise(d, "/rename x"), d, "the same text set again is no new draft");
  d = revise(d, "");
  assert.deepEqual(d, { text: "", rev: 2 }, "a clear");
  // Typed away and back: the text is the same, the DRAFT is not.
  const pressed = d.rev;
  d = revise(revise(d, "a"), "");
  assert.equal(d.text, "");
  assert.equal(owns(pressed, d.rev), false, "edited away and back still moved the revision");
});

test("CONTROL: a stale Enter is dropped — the draft moved while its round-trips were out", () => {
  // Enter pressed on `/no-such-command-029` (claimed); while its filter is
  // in flight the draft is replaced with `/plan`.
  let d = revise({ text: "", rev: 0 }, "/no-such-command-029");
  const claimed = d.rev;
  d = revise(d, "/plan");
  assert.equal(owns(claimed, d.rev), false, "the Enter was for a draft that no longer exists");
  // An Enter pressed on the draft as it is owns it.
  assert.equal(owns(d.rev, d.rev), true);
});

test("CONTROL: an outcome clears ONLY the draft it was sent from — a newer one survives", () => {
  const outcomes = [
    { kind: "done" },
    { kind: "refused", reason: "/help: not available here yet" },
    { kind: "failed", code: "bad_request", message: "params.title must not be empty" },
    { kind: "failed", code: "outcome_unknown", message: "lost" },
    { kind: "cancelled" },
    { kind: "picker" },
    { kind: "warning", text: "renamed, but craze could not record it: disk full" },
  ];
  for (const o of outcomes) {
    assert.deepEqual(settleFor(o, true), draftAfter(o), `owned ${o.kind}: §3.5's table exactly`);
    const stale = settleFor(o, false);
    assert.equal(stale.clear, false, `${o.kind}: a newer draft is never cleared`);
    assert.deepEqual(stale.note, draftAfter(o).note, `${o.kind}: what it has to say still shows`);
    assert.equal(stale.openSettings, draftAfter(o).openSettings, o.kind);
  }
  // The review's case, end to end: `/rename old` sent, a new prompt typed
  // while the rename is pending, then the rename succeeds.
  let d = revise({ text: "", rev: 0 }, "/rename old");
  const sent = d.rev;
  d = revise(d, "a newer prompt");
  const after = settleFor({ kind: "done" }, owns(sent, d.rev));
  assert.equal(after.clear, false, "the rename's success left the newer prompt alone");
});

// ---- one key at a time (E15; the branch review's finding 4) ---------------

/** A promise the test settles by hand — a round-trip held open. */
function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** Let every queued continuation run (the sequencer's awaits are on settled
 *  promises once the test has answered them). */
const drain = () => new Promise((r) => setImmediate(r));

/** A composer for `pressKey`: each Enter's filter is a round-trip the test
 *  answers (`filters[i]`), the draft is owned while `owned` says so, and
 *  every send and accept is recorded. A send answers like a LOST verb
 *  (`outcome_unknown`): the verb's own catch shows the note, and the draft is
 *  KEPT — owned still. */
function composer() {
  const c = {
    owned: true,
    dismissed: null,
    filters: [],
    sends: 0,
    accepts: [],
    steps() {
      return {
        filter: () => {
          const d = deferred();
          c.filters.push(d);
          return d.promise;
        },
        owned: () => c.owned,
        dismissed: () => c.dismissed,
        send: async () => {
          c.sends += 1;
        },
        accept: async (name) => {
          c.accepts.push(name);
        },
      };
    },
  };
  return c;
}

const SHOWN = { key: "", index: 0 };
/** Every sequencer cell is bounded: a regressed guard FAILS a cell, never
 *  hangs the suite on a round-trip nobody answers. */
const SEQ = { timeout: 5000 };
/** `/research test` with the caret at its end: past the first token, so the
 *  menu is CLOSED (P28) and Enter sends. */
const CLOSED = { open: false, token: "/research", names: [] };

test("CONTROL: two rapid Enters on one draft send it ONCE — the guard is taken before the filter (finding 4)", SEQ, async () => {
  const g = { held: false };
  const c = composer();
  // Both Enters land before either's filter has answered.
  const first = pressKey(g, "enter", SHOWN, c.steps());
  const second = pressKey(g, "enter", SHOWN, c.steps());
  // The first's filter answers; its send goes out, its answer is lost
  // (`outcome_unknown`): the draft is kept, still owned, and the guard is
  // released.
  c.filters[0].resolve(CLOSED);
  await first;
  assert.equal(c.sends, 1);
  assert.equal(g.held, false, "the first key released the guard");
  // The second Enter's filter — if one was ever asked for — answers only
  // now, on the same unchanged draft. It must NOT send it again: no key was
  // pressed after the warning.
  for (const d of c.filters.slice(1)) d.resolve(CLOSED);
  await second;
  await drain();
  assert.equal(c.sends, 1, "the draft was sent once: the second Enter was dropped at the press");
  assert.equal(c.filters.length, 1, "the dropped Enter never even asked for a filter");
});

test("a key pressed after the first settled goes through — the person's own resend", SEQ, async () => {
  const g = { held: false };
  const c = composer();
  const first = pressKey(g, "enter", SHOWN, c.steps());
  c.filters[0].resolve(CLOSED);
  await first;
  const again = pressKey(g, "enter", SHOWN, c.steps());
  c.filters[1].resolve(CLOSED);
  await again;
  assert.equal(c.sends, 2);
});

test("CONTROL: a stale key is dropped without stranding the guard", SEQ, async () => {
  const g = { held: false };
  const c = composer();
  const stale = pressKey(g, "enter", SHOWN, c.steps());
  // The draft moves on while its filter is out.
  c.owned = false;
  c.filters[0].resolve(CLOSED);
  await stale;
  assert.equal(c.sends, 0, "a key for a draft that no longer exists sends nothing");
  assert.equal(g.held, false, "and does not strand the guard");
  c.owned = true;
  const next = pressKey(g, "enter", SHOWN, c.steps());
  c.filters[1].resolve(CLOSED);
  await next;
  assert.equal(c.sends, 1, "the next key goes through");
});

test("a round-trip that throws releases the guard", SEQ, async () => {
  const g = { held: false };
  const c = composer();
  const k = pressKey(g, "enter", SHOWN, c.steps());
  c.filters[0].reject(new Error("invoke failed"));
  await assert.rejects(k, /invoke failed/);
  assert.equal(g.held, false);
  const boom = oneAtATime(g, async () => {
    throw new Error("verb failed");
  });
  await assert.rejects(boom, /verb failed/);
  assert.equal(g.held, false);
});

test("Send, a pick and Enter/Tab share the one guard: none starts while another is out", SEQ, async () => {
  const g = { held: false };
  const c = composer();
  const verb = deferred();
  let ran = 0;
  // A Send whose verb is still out.
  const send = oneAtATime(g, () => verb.promise);
  // An Enter, a Tab and a pick pressed meanwhile are dropped at the press.
  const pressed = [
    pressKey(g, "enter", SHOWN, c.steps()),
    pressKey(g, "tab", SHOWN, c.steps()),
    oneAtATime(g, async () => {
      ran += 1;
    }),
  ];
  await drain();
  assert.equal(c.filters.length, 0, "neither key asked for a filter");
  assert.equal(ran, 0, "the pick did not run");
  // Settle whatever a regressed guard let through, so the cell ends either way.
  for (const d of c.filters) d.resolve(CLOSED);
  verb.resolve();
  await Promise.all([send, ...pressed]);
  assert.equal(c.sends, 0);
  await oneAtATime(g, async () => {
    ran += 1;
  });
  assert.equal(ran, 1, "once the Send settled, the next key runs");
});

test("Enter or Tab on the fresh filter: a closed menu sends on Enter only; an open one accepts the row on screen", SEQ, async () => {
  const run = async (key, filter, dismissed, shown = SHOWN) => {
    const c = composer();
    c.dismissed = dismissed;
    const k = pressKey({ held: false }, key, shown, c.steps());
    c.filters[0].resolve(filter);
    await k;
    return { sends: c.sends, accepts: c.accepts };
  };
  assert.deepEqual(await run("enter", null, null), { sends: 1, accepts: [] }, "no menu: Enter sends");
  assert.deepEqual(await run("tab", null, null), { sends: 0, accepts: [] }, "no menu: Tab does nothing");
  assert.deepEqual(await run("enter", CLOSED, null), { sends: 1, accepts: [] });
  const open = { open: true, token: "/re", names: ["/rename", "/research"] };
  const onScreen = { key: highlightKey("/re", ["/rename", "/research"]), index: 1 };
  assert.deepEqual(await run("enter", open, null, onScreen), { sends: 0, accepts: ["/research"] },
    "the highlighted row, when the fresh answer is the one on screen");
  assert.deepEqual(await run("tab", open, null), { sends: 0, accepts: ["/rename"] }, "else the first row");
  assert.deepEqual(await run("enter", open, "/re"), { sends: 1, accepts: [] },
    "a menu dismissed for this token is not up: Enter sends");
});
