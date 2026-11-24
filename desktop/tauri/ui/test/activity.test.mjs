/* The Activity pane's reading and paging (`src/lib/activity.ts`, plan 039 PR B), on
 * node's own test runner — the `delivery.test.mjs` arrangement. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  EMPTY_FEED,
  actorText,
  appendPage,
  deliveryText,
  errorText,
  outcomeTone,
  readEvent,
  readPage,
} from "../dist-test/activity.js";

const EV = (server, id, at, extra = {}) => ({
  server, id, at, kind: "shed.create", shed: { name: "w" }, actor: { kind: "user", user: "ada" }, result: "ok", ...extra,
});

test("an event without an id or a kind is not one", () => {
  assert.equal(readEvent({ kind: "x" }), null);
  assert.equal(readEvent({ id: "1" }), null);
  assert.equal(readEvent("nope"), null);
  const e = readEvent({ id: "1", kind: "shed.stop", shed: { name: "w" }, actor: {} });
  assert.equal(e.actor.kind, "unknown");
  assert.equal(e.server, "");
});

test("a page reads its events, its cursor and its errors", () => {
  const p = readPage({
    events: [EV("a", "1", "2026-11-23T10:00:00Z"), { bad: true }],
    next: "",
    errors: [{ server: "b", status: 503, code: "unavailable", message: "down" }],
  });
  assert.equal(p.events.length, 1);
  assert.equal(p.next, null, "an empty cursor is no cursor");
  assert.deepEqual(p.errors, [{ server: "b", status: 503, code: "unavailable", message: "down" }]);
  assert.equal(errorText(p.errors[0]), "b: down (503)");
  assert.equal(errorText({ server: "c", status: null, code: "", message: "" }), "c: could not be read");
});

test("a later page follows what is shown and never shows an event twice", () => {
  const T = (h) => `2026-11-23T${h}:00:00Z`;
  const first = appendPage(EMPTY_FEED, readPage({ events: [EV("a", "2", T("12")), EV("b", "2", T("11"))], next: "c1", errors: [] }));
  assert.equal(first.next, "c1");
  const second = appendPage(first, readPage({ events: [EV("a", "2", T("12")), EV("a", "1", T("10"))], next: null, errors: [{ server: "b", message: "x" }] }));
  assert.deepEqual(second.events.map((e) => `${e.server}/${e.id}`), ["a/2", "b/2", "a/1"]);
  assert.equal(second.next, null);
  assert.equal(second.errors.length, 1, "the last page's errors are the ones shown");
});

test("a server that recovers on a later page has its newer events merged above older rows", () => {
  // Page 1: `a` answers (a 10:00 event), `b` fails and keeps its place.
  const first = appendPage(EMPTY_FEED, readPage({
    events: [EV("a", "1", "2026-11-23T10:00:00Z")],
    next: "c1",
    errors: [{ server: "b", status: null, code: "transport", message: "down" }],
  }));
  // Page 2: `b` recovers with an 11:00 event and a 09:00 one; `a` sends an older one.
  const second = appendPage(first, readPage({
    events: [EV("b", "9", "2026-11-23T11:00:00Z"), EV("b", "8", "2026-11-23T09:00:00Z"),
      EV("a", "0", "2026-11-23T08:00:00Z"), EV("a", "1", "2026-11-23T10:00:00Z")],
    next: null,
    errors: [],
  }));
  assert.deepEqual(second.events.map((e) => `${e.server}/${e.id}`), ["b/9", "a/1", "b/8", "a/0"]);
  assert.deepEqual(second.errors, []);
  // Equal times keep the order they came in; an unparseable time sorts oldest.
  const tied = appendPage(EMPTY_FEED, readPage({
    events: [EV("a", "x", "nope"), EV("a", "2", "t"), EV("b", "3", "2026-11-23T10:00:00Z"),
      EV("a", "4", "2026-11-23T10:00:00Z")],
    next: null,
    errors: [],
  }));
  assert.deepEqual(tied.events.map((e) => `${e.server}/${e.id}`), ["b/3", "a/4", "a/x", "a/2"]);
});

test("who acted", () => {
  assert.equal(actorText({ kind: "user", user: "ada" }), "ada");
  assert.equal(actorText({ kind: "user", implicit: true }), "the server's user");
  assert.equal(actorText({ kind: "user", user: "ada", script: true }), "ada (script key)");
  assert.equal(actorText({ kind: "client", client: "ci" }), "client ci");
  assert.equal(actorText({ kind: "system" }), "the server");
  assert.equal(actorText({ kind: "unknown" }), "unknown");
});

test("a delivery's state in words, and each event's tone", () => {
  assert.equal(deliveryText(null), null);
  assert.equal(deliveryText({ state: "delivered", outcome: "accepted" }), "delivered · accepted");
  assert.equal(deliveryText({ state: "not_delivered", failure: { reason: "no_source" } }), "not delivered · no source");
  assert.equal(outcomeTone(readEvent(EV("a", "1", "t"))), "ok");
  assert.equal(outcomeTone(readEvent(EV("a", "1", "t", { result: "error" }))), "bad");
  assert.equal(outcomeTone(readEvent(EV("a", "1", "t", { result: undefined }))), "none");
  assert.equal(outcomeTone(readEvent(EV("a", "1", "t", { result: undefined, delivery: { state: "partial" } }))), "bad");
});
