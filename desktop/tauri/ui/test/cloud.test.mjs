/* A cloud shed's row as the Sheds pane draws it (`src/lib/cloud.ts`, plan 041 B2), on
 * node's own test runner — the `sources.test.mjs` arrangement. */
import test from "node:test";
import assert from "node:assert/strict";

import { customHours, extendKey, failureWords, leaseTone, presetLabel, pruneRuns, readCloudRow } from "../dist-test/cloud.js";

const ROW = {
  badge: "Cloud",
  size_words: "2 vCPU · 8 GB",
  image: "shed-full-1",
  lease_words: "deleted in 5h 10m",
  lease_state: "ok",
  run_words: "running for 3h 2m",
  actions: { start: false, stop: true, delete: false, extend: true },
  extend_presets_h: [6, 12, 24, 48],
  extend_max_h: 94,
  expires_words: "14:10 UTC",
  cap_words: "2026-11-30 13:00 UTC",
  created_words: "06:00 UTC",
};

test("readCloudRow keeps the core's row as it came", () => {
  assert.deepEqual(readCloudRow(ROW), ROW);
});

test("readCloudRow: no row is null, and a malformed one offers nothing", () => {
  for (const raw of [null, undefined, "Cloud", 7, [], [ROW]]) assert.equal(readCloudRow(raw), null);
  const r = readCloudRow({ lease_state: "later", actions: { extend: "yes", delete: 1 }, extend_presets_h: [6, "12", 0, 1.5, 24] });
  assert.equal(r.badge, "Cloud");
  assert.equal(r.lease_state, "unknown");
  assert.deepEqual(r.actions, { start: false, stop: false, delete: false, extend: false });
  assert.deepEqual(r.extend_presets_h, [6, 24]);
  assert.equal(r.extend_max_h, 0);
  assert.equal(r.run_words, null);
  assert.equal(r.expires_words, null);
});

test("leaseTone: amber within the warning time, danger once ended", () => {
  assert.equal(leaseTone("soon"), "amber");
  assert.equal(leaseTone("ended"), "danger");
  assert.equal(leaseTone("ok"), "muted");
  assert.equal(leaseTone("unknown"), "muted");
});

test("presetLabel", () => {
  assert.equal(presetLabel(6), "+6 h");
  assert.equal(presetLabel(48), "+48 h");
});

test("customHours: whole hours from 1 to the row's most", () => {
  assert.equal(customHours("30", 94), 30);
  assert.equal(customHours(" 1 ", 94), 1);
  assert.equal(customHours("94", 94), 94);
  for (const bad of ["", "0", "95", "-3", "1.5", "6h", "1e2", "abc", "99999"]) {
    assert.equal(customHours(bad, 94), null, bad);
  }
  assert.equal(customHours("1", 0), null, "no extend offered: nothing is a value");
});

test("failureWords: a refusal's fixed words, else the error's text", () => {
  assert.equal(
    failureWords({ code: "LEASE_AT_CAP", reason: "", status: 409, words: "extend by fewer hours" }),
    "extend by fewer hours",
  );
  assert.equal(failureWords("no server answered"), "no server answered");
  assert.equal(failureWords(new Error("boom")), "boom");
  assert.equal(failureWords({ words: "" }), "the extend failed");
  assert.equal(failureWords(undefined), "the extend failed");
});

test("extendKey: server, name and incarnation apart", () => {
  assert.notEqual(extendKey("a", "b/c", "t"), extendKey("a/b", "c", "t"));
  assert.notEqual(extendKey("a", "x", "t1"), extendKey("a", "x", "t2"));
  assert.equal(extendKey("a", "x", "t1"), extendKey("a", "x", "t1"));
});

test("pruneRuns forgets finished runs of incarnations no longer listed, never a pending one", () => {
  const now = extendKey("mock", "ok", "t2");
  const old = extendKey("mock", "ok", "t1"); // the same name, an earlier incarnation
  const gone = extendKey("mock", "gone", "t1");
  const pending = extendKey("mock", "busy", "t1");
  const runs = { [now]: "n", [old]: "o", [gone]: "g", [pending]: "p" };
  const out = pruneRuns(runs, new Set([now]), new Set([pending]));
  assert.deepEqual(out, { [now]: "n", [pending]: "p" });
  // Nothing to forget: the same object, so the caller's update is a no-op.
  assert.equal(pruneRuns(out, new Set([now]), new Set([pending])), out);
  // A thousand finished runs of deleted sheds go at the next listing.
  const many = Object.fromEntries(Array.from({ length: 1000 }, (_, i) => [extendKey("mock", `s${i}`, "t"), i]));
  assert.deepEqual(pruneRuns(many, new Set(), new Set()), {});
});
