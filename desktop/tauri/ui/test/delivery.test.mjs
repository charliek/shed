/* The delivery record and badge (`src/lib/delivery.ts`, plan 039
 * A7), on node's own test runner — the `sources.test.mjs` arrangement. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  deliveryBadge,
  deliverySummary,
  provisionResult,
  readDelivery,
  stepText,
} from "../dist-test/delivery.js";

const PARTIAL = {
  state: "partial",
  steps: {
    stage1: { state: "done" },
    clone: { state: "done" },
    stage2: { state: "failed", reason: "write_failed" },
    install: { state: "owed" },
  },
  last: {
    at: "2026-11-23T10:00:00Z", reason: "provision", outcome: "accepted",
    source: { kind: "machine", machine_id: "a".repeat(32), machine_name: "laptop" },
    problems: [{ provider: "ssh-key", code: "passphrase", message: "the key has a passphrase" }],
  },
  items: [
    { name: "github", provider: "github", stage: 1, kind: "file", base: "home",
      target: ".config/gh/hosts.yml", fingerprint: "fp", written_at: "2026-11-23T10:00:02Z",
      expires_at: "2026-11-24T10:00:00Z", result: "ok" },
    { name: "bad" },
    "nope",
  ],
  expired: ["github"],
  owed: ["install"],
};

test("a record decodes defensively", () => {
  assert.equal(readDelivery(null), null);
  assert.equal(readDelivery("delivered"), null);
  assert.equal(readDelivery({}), null);
  assert.equal(readDelivery({ state: "" }), null);
  const r = readDelivery(PARTIAL);
  assert.equal(r.state, "partial");
  assert.deepEqual(r.steps.map((s) => s.name), ["stage1", "clone", "stage2", "install"]);
  assert.equal(stepText(r.steps[2]), "failed (write_failed)");
  assert.equal(stepText(r.steps[3]), "owed");
  assert.deepEqual(r.items.map((i) => i.name), ["github", "bad"]);
  assert.equal(r.items[1].target, "");
  assert.equal(r.last.problems[0].code, "passphrase");
  assert.equal(r.last.source.machine_name, "laptop");
  assert.equal(r.last.source.kind, "machine");
  assert.equal(r.withheld, false);
});

test("a source's kind is read: the account service has no machine", () => {
  const r = readDelivery({ ...PARTIAL, last: { ...PARTIAL.last, source: { kind: "service" } } });
  assert.deepEqual(r.last.source, { kind: "service", machine_id: "", machine_name: "" });
});

test("only the server's marker withholds a record", () => {
  for (const state of ["delivered", "partial", "not_delivered", "none", "not_applicable"]) {
    const r = readDelivery({ state, withheld: true });
    assert.equal(r.withheld, true, state);
    assert.match(deliverySummary(r), /Only the shed's user/);
    // A state alone is the user's own record (a shed from before delivery).
    assert.equal(readDelivery({ state }).withheld, false, state);
  }
  assert.equal(readDelivery({ state: "partial", withheld: "yes" }).withheld, false);
});

test("the badge names the state, expired items and owed steps", () => {
  assert.deepEqual(deliveryBadge(readDelivery(PARTIAL)), {
    label: "partial", tone: "attention", extras: ["1 expired", "owed: install"],
  });
  const cases = [
    ["delivered", "delivered", "ok"],
    ["not_delivered", "not delivered", "danger"],
    ["none", "no delivery", "muted"],
    ["not_applicable", "not applicable", "muted"],
    ["later_word", "later word", "muted"],
  ];
  for (const [state, label, tone] of cases) {
    const b = deliveryBadge(readDelivery({ state }));
    assert.equal(b.label, label);
    assert.equal(b.tone, tone);
    assert.deepEqual(b.extras, []);
  }
});

test("a provision's result line", () => {
  const ok = provisionResult({
    ok: true, delivery: { state: "delivered" },
    progress: [{ message: "stage1: delivered" }, { message: "no source", warning: true }],
  });
  assert.deepEqual(ok, { ok: true, text: "Delivered again: delivered.", warnings: ["no source"] });
  const bad = provisionResult({ ok: false, error: { code: "SHED_NOT_RUNNING", message: "shed x is not running" } });
  assert.equal(bad.ok, false);
  assert.equal(bad.text, "Could not deliver again: shed x is not running");
  assert.equal(provisionResult(null).ok, false);
});
