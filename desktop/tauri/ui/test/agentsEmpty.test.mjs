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

import { agentsEmptyState, crazeOnly } from "../dist-test/agentsEmpty.js";

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

// ---- a host listed by craze alone (plan 026 §3.2) --------------------------

/** A shed a create (or its first craze `Ready`) listed, with no roost watcher
 *  and no roost inventory. */
const crazeListedShed = (state) => ({
  roost_watched: false, reachable: false, connected_once: false, craze: { state },
});

test("CONTROL: a shed listed by craze alone is craze only, whatever its craze phase", () => {
  for (const state of ["live", "dormant", "offline"]) {
    assert.equal(crazeOnly(crazeListedShed(state)), true, state);
  }
  assert.equal(crazeOnly(crazeListedShed("absent")), false, "no craze source: not craze only");
  assert.equal(crazeOnly({ ...crazeListedShed("live"), craze: null }), false);
  assert.equal(crazeOnly({ ...crazeListedShed("live"), reachable: true }), false, "roost answers");
  assert.equal(crazeOnly({ ...crazeListedShed("live"), connected_once: true }), false,
    "roost answered once: unreachable, not craze only");
});

test("CONTROL: a host whose roost is watched is not craze only before its first inventory", () => {
  // The C2 review's P2: the shed's roost answered and a watcher runs, but its
  // first inventory has not landed (`connected_once` false) when its craze
  // reaches Ready — "no roost session here yet" would be false.
  const watched = { ...crazeListedShed("live"), roost_watched: true };
  assert.equal(crazeOnly(watched), false);
  // A machine is watched from the start (configured, or the implicit
  // localhost): "connecting" and "unreachable" stay its words.
  assert.equal(crazeOnly({ ...crazeListedShed("dormant"), roost_watched: true }), false);
  // An old payload with no flag reads as unwatched.
  const { roost_watched: _drop, ...legacy } = crazeListedShed("live");
  assert.equal(crazeOnly(legacy), true);
});

test("CONTROL: a host listed by craze alone does not make the pane unreachable", () => {
  const down = { reachable: false, roost_watched: true, connected_once: true, craze: { state: "absent" } };
  assert.equal(agentsEmptyState([down, down], "ready", null).state, "unreachable");
  const withCraze = agentsEmptyState([down, crazeListedShed("live")], "ready", null);
  assert.equal(withCraze.state, "empty", "the craze-only shed is somewhere the pane can see");
  assert.equal(agentsEmptyState([crazeListedShed("dormant")], "ready", null).state, "empty");
});
