/* The roost board's per-target fetch bookkeeping (`src/lib/roostBoard.ts`), on
 * node's own test runner — same arrangement as `newest.test.mjs`: plain `.mjs`
 * against the JS `npm test` emits from the TypeScript, so this needs no test
 * framework, no DOM and no React.
 *
 * Every case here is a sequence that actually happened to a card on screen.
 * The two marked REGRESSION are the bugs this module was extracted to kill. */
import test from "node:test";
import assert from "node:assert/strict";

import { roostBoardGuard } from "../dist-test/roostBoard.js";

/** The targets `sync` asked to fetch, in order. */
function targets(starts) {
  return starts.map((s) => s.target);
}

test("a fresh board fetches every target once", () => {
  const g = roostBoardGuard();
  assert.deepEqual(targets(g.sync(["a", "b"], false)), ["a", "b"]);
  // The same list again is not a reason to re-reach two hosts.
  assert.deepEqual(g.sync(["a", "b"], false), []);
});

test("adding a card does not re-fetch the ones already there", () => {
  const g = roostBoardGuard();
  g.sync(["a"], false);
  assert.deepEqual(targets(g.sync(["a", "b"], false)), ["b"]);
});

test("the explicit refresh re-fetches everything", () => {
  const g = roostBoardGuard();
  const first = g.sync(["a", "b"], false);
  const again = g.sync(["a", "b"], true);
  assert.deepEqual(targets(again), ["a", "b"]);
  // And the refresh's generations supersede the first round's, so an answer
  // still in flight from before Refresh was pressed cannot land on top of it.
  for (const { target, gen } of first) assert.equal(g.accept(target, gen), false);
  for (const { target, gen } of again) assert.equal(g.accept(target, gen), true);
});

test("an answer commits while its target is on the board", () => {
  const g = roostBoardGuard();
  const [{ target, gen }] = g.sync(["a"], false);
  assert.equal(g.accept(target, gen), true);
});

test("REGRESSION: adding a second card does not strand the first one's probe", () => {
  // `[a]` → `[a, b]`, no refresh, with a's probe still in flight. React runs
  // the previous effect's cleanup on this deps change, and the old code read
  // that as "cancel a" while ALSO skipping a replacement fetch for a — so a's
  // card sat on `loading` until a remount.
  const g = roostBoardGuard();
  const [a] = g.sync(["a"], false);
  assert.deepEqual(targets(g.sync(["a", "b"], false)), ["b"]);
  assert.equal(g.accept(a.target, a.gen), true, "a's in-flight answer still commits");
});

test("REGRESSION: a target that leaves and comes back fetches again", () => {
  // A shed that stops leaves the board and a restarted one rejoins it. The old
  // code kept the departed target's generation counter, so the rejoining card
  // was skipped forever and rendered `roost: probing…` with no button.
  const g = roostBoardGuard();
  g.sync(["a"], false);
  g.sync([], false);
  assert.deepEqual(g.served(), []);
  assert.deepEqual(targets(g.sync(["a"], false)), ["a"]);
});

test("a departed target's answer does not resurrect its card", () => {
  const g = roostBoardGuard();
  const [a] = g.sync(["a"], false);
  g.sync([], false);
  assert.equal(g.accept(a.target, a.gen), false);
});

test("a target that returns before its old probe answers rejects the stale one", () => {
  // The case that rules out simply DELETING the counter on prune: a counter
  // reset to zero would hand the returning target generation 1 — the number
  // the probe from before it left is still holding — and that stale answer
  // would be accepted as the fresh one.
  const g = roostBoardGuard();
  const [first] = g.sync(["a"], false);
  g.sync([], false);
  const [second] = g.sync(["a"], false);
  assert.notEqual(second.gen, first.gen, "the returning fetch gets a fresh generation");
  assert.equal(g.accept(first.target, first.gen), false, "the stale answer is refused");
  assert.equal(g.accept(second.target, second.gen), true);
});

test("unmount stops everything, for good", () => {
  const g = roostBoardGuard();
  const [a] = g.sync(["a"], false);
  g.unmount();
  assert.equal(g.accept(a.target, a.gen), false);
  // Even a fetch started afterwards commits nothing.
  const [again] = g.sync(["a"], true);
  assert.equal(g.accept(again.target, again.gen), false);
});

test("an empty board forgets every target", () => {
  const g = roostBoardGuard();
  g.sync(["a", "b", "c"], false);
  assert.deepEqual(g.sync([], false), []);
  assert.deepEqual(g.served(), []);
  assert.deepEqual(targets(g.sync(["a", "b", "c"], false)), ["a", "b", "c"]);
});
