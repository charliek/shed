/* The transcript panel's verb gates (`src/lib/laneVerbs.ts`), on node's own
 * test runner — same arrangement as `newest.test.mjs`: plain `.mjs` against the
 * JS `npm test` emits from the TypeScript, no DOM, no React.
 *
 * The case marked REGRESSION is the review finding this module exists for
 * (plan 025 C4, sol 6 / astra 4): Cancel was gated on the activity alone, so a
 * Working session whose streamed capabilities said `cancel: false` still showed
 * an enabled Cancel button — and `lane.dump` reported `can_cancel: true` — for a
 * verb the session had said it cannot do. */
import test from "node:test";
import assert from "node:assert/strict";

import { laneVerbs } from "../dist-test/laneVerbs.js";

const caps = (cancel, interject) => ({ cancel, interject });

test("REGRESSION: a working session that cannot cancel shows no Cancel", () => {
  const { cancel } = laneVerbs(caps(false, false), "working");
  assert.deepEqual(cancel, { shown: false, enabled: false });
});

test("a session that can cancel shows Cancel, enabled only while working", () => {
  assert.deepEqual(laneVerbs(caps(true, false), "working").cancel, { shown: true, enabled: true });
  for (const activity of ["needs_input", "needs_approval", "idle", "unknown", ""]) {
    assert.deepEqual(
      laneVerbs(caps(true, false), activity).cancel,
      { shown: true, enabled: false },
      activity,
    );
  }
});

test("interject follows the same rule, independently of cancel", () => {
  assert.deepEqual(laneVerbs(caps(true, false), "working").interject, { shown: false, enabled: false });
  assert.deepEqual(laneVerbs(caps(false, true), "working").interject, { shown: true, enabled: true });
  assert.deepEqual(laneVerbs(caps(false, true), "idle").interject, { shown: true, enabled: false });
});

test("before the first seed's capabilities arrive, neither button exists", () => {
  for (const missing of [null, undefined, {}]) {
    const verbs = laneVerbs(missing, "working");
    assert.deepEqual(verbs.cancel, { shown: false, enabled: false });
    assert.deepEqual(verbs.interject, { shown: false, enabled: false });
  }
});
