/* The shell's initial report (`src/lib/report.ts`, shed#400), on node's own
 * test runner — the newest.test.mjs shape: plain `.mjs` over what `npm test`
 * emits, no framework, no DOM.
 *
 * The rule under test: the listener effect's initial report re-sends the latest
 * complete report the shell built, with the current pane, and sends the seed
 * ONLY when none was built. The call site is one line in `bridge.ts`; the
 * shedtest cell `test_a_late_first_shed_list_never_zeroes_the_badges` is what
 * catches that line, and these cells pin the helper it calls. */
import test from "node:test";
import assert from "node:assert/strict";

import { initialReport, seededReport } from "../dist-test/report.js";

/** A report the shell might have built while the first shed list was in
 *  flight: the host list had answered (two hosts, one of them down), so the
 *  badge and the sidebar are real. */
function built(over = {}) {
  return {
    pane: "sheds",
    sheds: [{ name: "hello-world", host: "mock", status: "running" }],
    refreshToken: 3,
    modal: null,
    badges: { sheds: 1, agents: 0, hosts: 2, pending: 0 },
    sidebar: {
      servers: [
        { name: "mock", reachable: true },
        { name: "down", reachable: false },
      ],
      machines: [],
    },
    mode: "dark",
    hostErrors: [{ server: "down", summary: "down: connection refused" }],
    ...over,
  };
}

test("with no report built yet, the initial report is the seed for the current pane", () => {
  const sent = initialReport(null, "agents");
  assert.deepEqual(sent, {
    pane: "agents",
    sheds: [],
    refreshToken: 0,
    modal: null,
    badges: { sheds: 0, agents: 0, hosts: 0, pending: 0 },
    sidebar: { servers: [], machines: [] },
    hostErrors: [],
  });
  assert.deepEqual(sent, seededReport("agents"));
  assert.equal("mode" in sent, false, "the seed must sample the document's mode, not claim one");
});

test("once a report is built, the initial report re-sends it whole, never a seed", () => {
  const latest = built();
  const sent = initialReport(latest, "sheds");
  assert.deepEqual(sent, latest, "the initial report re-seeded over a built report");
  assert.equal(sent.badges.hosts, 2, "the Hosts badge went back to the seed's 0");
  assert.equal(sent.sidebar.servers.length, 2, "the sidebar went back to empty");
  assert.equal(sent.sheds.length, 1, "the dashboard rows went back to empty");
  assert.equal(sent.refreshToken, 3, "the echoed refresh token went back to 0");
});

test("the re-sent report carries the CURRENT pane, and the built one is untouched", () => {
  const latest = built({ pane: "sheds" });
  const sent = initialReport(latest, "system");
  assert.deepEqual(sent, { ...latest, pane: "system" });
  assert.equal(latest.pane, "sheds", "initialReport mutated the report it was handed");
});

test("a built report that really is empty is still re-sent, not replaced by the seed", () => {
  // Zero hosts and zero sheds is a real answer (no servers configured). What
  // must not happen is the SEED standing in for it: the seed also drops the
  // modal, the mode and the echoed token.
  const latest = built({
    sheds: [],
    badges: { sheds: 0, agents: 0, hosts: 0, pending: 0 },
    sidebar: { servers: [], machines: [] },
    hostErrors: [],
    modal: "create",
    refreshToken: 7,
  });
  const sent = initialReport(latest, "sheds");
  assert.deepEqual(sent, latest);
  assert.notDeepEqual(sent, seededReport("sheds"));
});
