/* The Preferences source rows' reconciliation (`src/lib/sources.ts`, plan 035
 * §3.4), on node's own test runner — the `laneSettings.test.mjs` arrangement.
 *
 * The orderings here are the ones a review reproduced against the window's real
 * callback: two servers' moves whose read-backs resolve out of order, a busy read,
 * a read-back that fails after a confirmed move, a refused move, and a second
 * press on a row whose move is in flight. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  EMPTY_SOURCES,
  SOURCE_REFRESH_MIN_MS,
  applyRead,
  beginMove,
  canUse,
  moveConfirmed,
  moveFailed,
  refreshDelay,
  shownRows,
} from "../dist-test/sources.js";

const ME = { id: "0".repeat(32), name: "laptop" };
const DESK = { id: "f".repeat(32), name: "desk" };
const OLD = { id: "e".repeat(32), name: "old-box" };

const row = (host, source, extra = {}) => ({
  host,
  supported: true,
  source,
  this_machine: !!source && source.id === ME.id,
  connected: true,
  busy: false,
  error: null,
  ...extra,
});

const status = (...rows) => ({
  rows,
  this_machine: ME,
  this_machine_error: null,
  broker_mode: "embedded",
});

const shown = (state, host) => state.rows[host];

/** Start from one read naming `desk` on both servers. */
function loaded() {
  return applyRead(EMPTY_SOURCES, 1, status(row("a", DESK), row("b", DESK))).state;
}

test("a read fills every row and this machine", () => {
  const s = loaded();
  assert.deepEqual(shownRows(s).map((r) => r.host), ["a", "b"]);
  assert.equal(s.thisMachine.name, "laptop");
  assert.equal(shown(s, "a").source.name, "desk");
});

test("an older read never replaces a newer one, per server", () => {
  let s = loaded();
  // Read 3 (newer) lands first, read 2 (older) after it.
  s = applyRead(s, 3, status(row("a", ME), row("b", DESK))).state;
  s = applyRead(s, 2, status(row("a", OLD), row("b", OLD))).state;
  assert.equal(shown(s, "a").source.name, "laptop", "a keeps the newer read");
  // b's newest is read 3 too, so read 2 is older there as well.
  assert.equal(shown(s, "b").source.name, "desk");
});

test("CONTROL: two servers' moves whose read-backs resolve out of order", () => {
  // The reviewer's sequence: A's read-back captures B's old binding and waits; B
  // moves and its read-back shows B as this machine; A's older read-back arrives.
  let s = loaded();
  let seq = 1;
  s = beginMove(s, "a");
  s = beginMove(s, "b");
  assert.ok(s, "moves on two servers run side by side");
  s = moveConfirmed(s, "a", ++seq, row("a", ME));
  const readBackA = ++seq; // issued now; sees b's OLD binding
  s = moveConfirmed(s, "b", ++seq, row("b", ME));
  const readBackB = ++seq;
  s = applyRead(s, readBackB, status(row("a", ME), row("b", ME)), "b").state;
  s = applyRead(s, readBackA, status(row("a", ME), row("b", DESK)), "a").state;
  assert.equal(shown(s, "b").this_machine, true, "b's newer confirmed state stands");
  assert.equal(shown(s, "a").this_machine, true);
  assert.equal(canUse(s, "a"), false);
  assert.equal(canUse(s, "b"), false);
});

test("a read that lands while a move is in flight leaves that row to the move", () => {
  let s = loaded();
  s = beginMove(s, "a");
  assert.equal(shown(s, "a").this_machine, true, "optimistic");
  const r = applyRead(s, 5, status(row("a", DESK), row("b", OLD)));
  s = r.state;
  assert.equal(shown(s, "a").this_machine, true, "the move owns its row");
  assert.equal(shown(s, "b").source.name, "old-box", "the other row still takes it");
  // A read issued before the move's answer arrived is older than the answer.
  s = moveConfirmed(s, "a", 7, row("a", ME));
  s = applyRead(s, 6, status(row("a", DESK), row("b", OLD))).state;
  assert.equal(shown(s, "a").this_machine, true);
});

test("a second press on a row whose move is in flight does nothing", () => {
  let s = loaded();
  s = beginMove(s, "a");
  assert.equal(canUse(s, "a"), false);
  assert.equal(beginMove(s, "a"), null);
  assert.equal(canUse(s, "b"), true, "another server's row is still pressable");
});

test("a refused move goes back to its last confirmed row and keeps the refusal", () => {
  let s = loaded();
  s = beginMove(s, "a");
  s = moveFailed(s, "a", "a refused the move: a script key");
  assert.equal(shown(s, "a").source.name, "desk");
  assert.equal(shown(s, "a").this_machine, false);
  assert.equal(s.errors.a, "a refused the move: a script key");
  assert.equal(canUse(s, "a"), true);
  // The next press clears it.
  s = beginMove(s, "a");
  assert.equal(s.errors.a, undefined);
});

test("a confirmed move survives a read-back that fails, and asks for a later read", () => {
  let s = loaded();
  s = beginMove(s, "a");
  s = moveConfirmed(s, "a", 2, row("a", ME));
  // The call itself failed.
  let r = applyRead(s, 3, undefined, "a");
  assert.equal(r.retry, true);
  assert.equal(shown(r.state, "a").this_machine, true);
  // The row came back as an error.
  r = applyRead(s, 3, status(row("a", null, { error: "a: transport error" }), row("b", DESK)), "a");
  assert.equal(r.retry, true);
  assert.equal(shown(r.state, "a").this_machine, true, "the error row does not replace it");
  assert.equal(shown(r.state, "a").error, null);
  // The later read, a plain one, then reconciles as usual.
  r = applyRead(r.state, 4, status(row("a", ME, { connected: true }), row("b", DESK)));
  assert.equal(r.retry, false);
  assert.equal(shown(r.state, "a").this_machine, true);
});

test("a busy read keeps the last known source, marked busy, and asks for a later read", () => {
  let s = loaded();
  const r = applyRead(s, 2, status(row("a", null, { busy: true }), row("b", DESK)));
  assert.equal(r.retry, true);
  s = r.state;
  assert.equal(shown(s, "a").source.name, "desk", "never 'no source'");
  assert.equal(shown(s, "a").busy, true);
  // Recovery: the later read replaces it.
  const later = applyRead(s, 3, status(row("a", ME), row("b", DESK)));
  assert.equal(later.retry, false);
  assert.equal(shown(later.state, "a").busy, false);
  assert.equal(shown(later.state, "a").this_machine, true);
});

test("a busy first read shows a busy row with no source, not 'none'", () => {
  const r = applyRead(EMPTY_SOURCES, 1, status(row("a", null, { busy: true })));
  assert.equal(r.retry, true);
  assert.equal(shown(r.state, "a").busy, true);
});

test("can_use is false where this machine is already the source", () => {
  let s = loaded();
  s = applyRead(s, 2, status(row("a", ME), row("b", DESK))).state;
  assert.equal(canUse(s, "a"), false);
  assert.equal(beginMove(s, "a"), null);
  assert.equal(canUse(s, "b"), true);
  // And with no identity nothing is pressable.
  const none = applyRead(EMPTY_SOURCES, 1, { ...status(row("b", DESK)), this_machine: null, this_machine_error: "no dir" }).state;
  assert.equal(canUse(none, "b"), false);
});

test("an older server's row is not shown", () => {
  const s = applyRead(EMPTY_SOURCES, 1, status(row("a", DESK), row("old", null, { supported: false }))).state;
  assert.deepEqual(shownRows(s).map((r) => r.host), ["a"]);
});

test("a shown/focused refresh is throttled", () => {
  assert.equal(refreshDelay(10_000, 0), 0);
  assert.equal(refreshDelay(10_000, 10_000 - SOURCE_REFRESH_MIN_MS), 0);
  assert.equal(refreshDelay(10_000, 9_000), SOURCE_REFRESH_MIN_MS - 1_000);
  assert.equal(refreshDelay(10_000, 10_000), SOURCE_REFRESH_MIN_MS);
});
