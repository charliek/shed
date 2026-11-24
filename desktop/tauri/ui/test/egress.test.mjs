/* A shed's egress profiles and recent blocks, and the System pane's profile rows
 * (`src/lib/egress.ts`, plan 039 PR B), on node's own test runner. */
import test from "node:test";
import assert from "node:assert/strict";

import { blockTarget, profileSummary, profilesText, readProfiles } from "../dist-test/egress.js";

test("a shed's profiles off its document", () => {
  assert.deepEqual(readProfiles(["default", "", 3, "extra"]), ["default", "extra"]);
  assert.deepEqual(readProfiles(null), []);
  assert.equal(profilesText(["default", "extra"]), "Egress profile: default, extra");
  assert.equal(profilesText([]), "Egress profile: none (egress is not restricted)");
  assert.match(profilesText(null), /unknown/);
});

test("where a block went", () => {
  assert.equal(blockTarget({ host: "evil.example.test", port: 443, resolved_ip: "" }), "evil.example.test:443");
  assert.equal(blockTarget({ host: "", port: 22, resolved_ip: "192.0.2.1" }), "192.0.2.1:22");
  assert.equal(blockTarget({ host: "", port: 0, resolved_ip: "" }), "?");
});

test("a profile's summary", () => {
  assert.equal(profileSummary({ mode: "audit", allow: ["a"], deny: ["b", "c"] }), "mode=audit allow=1 deny=2");
  assert.equal(profileSummary({}), "(empty)");
});
