/* The Sheds pane's status text and busy rule (`src/lib/shedStatus.ts`, plan 037
 * §3.14), on node's own test runner — the `sources.test.mjs` arrangement. */
import test from "node:test";
import assert from "node:assert/strict";

import { isBusy, pairRowsWithCards, statusText } from "../dist-test/shedStatus.js";

test("statusText is the word, with the reason in parentheses as the CLI shows it", () => {
  assert.equal(statusText({ status: "stopping" }), "stopping");
  assert.equal(statusText({ status: "starting", status_reason: null }), "starting");
  assert.equal(statusText({ status: "running", status_reason: "" }), "running");
  assert.equal(statusText({ status: "stopped", status_reason: "capacity" }), "stopped (capacity)");
  assert.equal(statusText({ status: "starting", status_reason: "no_pin" }), "starting (no_pin)");
  assert.equal(statusText({ status: "stopped", status_reason: "run_limit" }), "stopped (run_limit)");
  // A status shed-core does not know arrives as `unknown`, and reads so.
  assert.equal(statusText({ status: "unknown" }), "unknown");
  // Verbatim, as the CLI: a reason of only whitespace is not empty, and a long
  // one is neither trimmed nor cut.
  assert.equal(statusText({ status: "stopped", status_reason: "   " }), "stopped (   )");
  const long = "x".repeat(4096);
  assert.equal(statusText({ status: "stopped", status_reason: long }), `stopped (${long})`);
});

/* A card as the sampler sees it: its identity attributes and what it shows. */
const card = (host, name, shows) => ({ host, name, shows });
const idOf = (c) => (c.host === null || c.name === null ? null : { host: c.host, name: c.name });
const shown = (rows, cards) => pairRowsWithCards(rows, cards, idOf).map((c) => (c ? c.shows : null));

test("pairRowsWithCards gives each row its own card", () => {
  const rows = [{ host: "a", name: "x" }, { host: "b", name: "x" }, { host: "a", name: "y" }];
  // Cards in the pane's order (grouped by host), not the list's.
  const cards = [card("a", "x", "ax"), card("a", "y", "ay"), card("b", "x", "bx")];
  assert.deepEqual(shown(rows, cards), ["ax", "bx", "ay"]);
  // Host and name are compared apart: "a"+"b/c" is not "a/b"+"c".
  assert.deepEqual(shown([{ host: "a", name: "b/c" }], [card("a/b", "c", "wrong")]), [null]);
  // No card mounted (another pane is up), or a node with no identity: null.
  assert.deepEqual(shown(rows, []), [null, null, null]);
  assert.deepEqual(shown([{ host: "a", name: "x" }], [card(null, "x", "nohost")]), [null]);
});

test("pairRowsWithCards pairs rows that share host and name in order, never borrowing", () => {
  // The review's case: one server lists two sheds named `dup`, the first
  // stopping, the second stopped. Each row reads its own card.
  const rows = [{ host: "h", name: "dup" }, { host: "h", name: "other" }, { host: "h", name: "dup" }];
  const cards = [card("h", "dup", "stopping"), card("h", "other", "running"), card("h", "dup", "stopped")];
  assert.deepEqual(shown(rows, cards), ["stopping", "running", "stopped"]);
  // Counts differ (two rows, one card): which card is whose cannot be known,
  // so both rows of that identity are null; other identities are unaffected.
  assert.deepEqual(shown(rows, [card("h", "dup", "stopping"), card("h", "other", "running")]), [null, "running", null]);
  // More cards than rows (a card left from an older render): null too.
  assert.deepEqual(
    shown([{ host: "h", name: "dup" }], [card("h", "dup", "old"), card("h", "dup", "new")]),
    [null],
  );
});

test("isBusy holds for a shed changing on its own, and nothing else", () => {
  for (const status of ["starting", "stopping", "restarting"]) {
    assert.equal(isBusy({ status }), true, status);
    assert.equal(isBusy({ status, status_reason: "external" }), true, `${status} with a reason`);
  }
  for (const status of ["running", "stopped", "error", "unknown", ""]) {
    assert.equal(isBusy({ status }), false, status);
  }
  // The reason never makes a settled shed busy.
  assert.equal(isBusy({ status: "stopped", status_reason: "capacity" }), false);
});
