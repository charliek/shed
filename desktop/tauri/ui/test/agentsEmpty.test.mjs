/* The Agents pane's empty state (`src/lib/agentsEmpty.ts`), on node's own test
 * runner — same arrangement as `newest.test.mjs` / `roostBoard.test.mjs`: plain
 * `.mjs` against the JS `npm test` emits from the TypeScript, so this needs no
 * test framework, no DOM and no React.
 *
 * The case marked REGRESSION is the bug this module was extracted to kill: a
 * failed or still-pending `rc.list` rendered as "No agent sessions — set up a
 * roost-session", which tells the user to go fix the wrong thing. */
import test from "node:test";
import assert from "node:assert/strict";

import { agentsEmptyState } from "../dist-test/agentsEmpty.js";

/** The bootstrap offer, which exactly one of the four states may make. */
const OFFER = "Set up a roost-session";

test("a read that came back empty offers the bootstrap", () => {
  const empty = agentsEmptyState([], "ready", null);
  assert.equal(empty.state, "empty");
  assert.equal(empty.action, OFFER);
  assert.match(empty.body, /roost-session/);
});

test("REGRESSION: a pane that is still loading claims nothing and offers nothing", () => {
  const loading = agentsEmptyState([], "loading", null);
  assert.equal(loading.state, "loading");
  assert.equal(loading.action, null);
  assert.notEqual(loading.title, "No agent sessions");
});

test("REGRESSION: a failed load says so, and says why", () => {
  const failed = agentsEmptyState([], "failed", "rc_list: no such command");
  assert.equal(failed.state, "failed");
  assert.equal(failed.action, null, "a failed load must not offer the bootstrap");
  assert.match(failed.title, /Could not load/);
  // The reason is the only thing the user can act on, so it is on screen whole.
  assert.match(failed.body, /rc_list: no such command/);
});

test("a failure with no reason still gets a sentence", () => {
  const failed = agentsEmptyState([], "failed", "");
  assert.equal(failed.state, "failed");
  assert.doesNotMatch(failed.body, /: *$/);
});

test("all-unreachable machines are their own state, not an empty one", () => {
  const down = agentsEmptyState(
    [{ reachable: false }, { reachable: false }],
    "ready",
    null,
  );
  assert.equal(down.state, "unreachable");
  assert.equal(down.action, null);
  assert.match(down.body, /All 2 configured machines are unreachable/);

  // One reachable machine means the pane HAS looked, so an empty list is a
  // genuine "nothing here" again.
  const mixed = agentsEmptyState([{ reachable: false }, { reachable: true }], "ready", null);
  assert.equal(mixed.state, "empty");
  assert.equal(mixed.action, OFFER);
});

test("loading and failed win over the machine list", () => {
  // Both states arrive with an empty machine list too (nothing has been read),
  // so a rule that consulted the machines first would fall through to the
  // bootstrap offer in exactly the two cases that must not make it.
  const machines = [{ reachable: false }];
  assert.equal(agentsEmptyState(machines, "loading", null).state, "loading");
  assert.equal(agentsEmptyState(machines, "failed", "boom").state, "failed");
});
