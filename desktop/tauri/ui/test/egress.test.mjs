/* A shed's effective egress and recent blocks, and the System pane's profile rows
 * (`src/lib/egress.ts`, plan 039 PR B), on node's own test runner. */
import test from "node:test";
import assert from "node:assert/strict";

import { blockTarget, egressText, profileSummary, readEgress } from "../dist-test/egress.js";

test("a shed's effective egress", () => {
  // Created without --egress: the server's default is in force, and marked as such.
  const inherited = readEgress({ enabled: true, restricted: true, profiles: ["default", "", 3], inherited: true, mode: "audit" });
  assert.deepEqual(inherited, { enabled: true, restricted: true, profiles: ["default"], inherited: true, mode: "audit" });
  assert.equal(egressText(inherited), "Egress profile: default (audit), the server's default");
  // The shed's own selection.
  assert.equal(
    egressText(readEgress({ enabled: true, restricted: true, profiles: ["base", "github"], inherited: false, mode: "enforce" })),
    "Egress profile: base, github (enforce)",
  );
  assert.equal(egressText(readEgress({ enabled: true, restricted: true, profiles: ["base"], mode: "weird" })), "Egress profile: base");
  // "Not restricted" only when nothing applies.
  assert.equal(egressText(readEgress({ enabled: true, restricted: false, profiles: [] })), "Egress profile: none (egress is not restricted)");
  assert.equal(egressText(readEgress({ enabled: false, restricted: false })), "Egress profile: none (egress control is off on this server)");
  // A shed that is not restricted carries no profiles, whatever else the value says.
  assert.deepEqual(readEgress({ enabled: true, restricted: false, profiles: ["x"], inherited: true }).profiles, []);
  assert.match(egressText(readEgress({ enabled: true, restricted: true, profiles: [] })), /restricted/);
  // Unread, and unreadable.
  assert.equal(readEgress(null), null);
  assert.equal(readEgress(["default"]), null);
  assert.match(egressText(null), /unknown/);
  assert.match(egressText(undefined), /reading/);
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
