/* The Preferences source rows' reconciliation (`src/lib/sources.ts`, plan 035
 * §3.4), on node's own test runner — the `laneSettings.test.mjs` arrangement.
 *
 * The orderings here are the ones a review reproduced against the window's real
 * callback: two servers' moves whose read-backs resolve out of order, a busy read,
 * a read-back that fails after a confirmed move, a refused move, and a second
 * press on a row whose move is in flight. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  EMPTY_SOURCES,
  SOURCE_REFRESH_MIN_MS,
  SOURCE_RETRY_MS,
  UNREACHABLE_RETRY_MS,
  applyRead,
  beginMove,
  canUse,
  createSourceReader,
  moveConfirmed,
  moveFailed,
  refreshDelay,
  restartRetries,
  shownRows,
} from "../dist-test/sources.js";

const ME = { id: "0".repeat(32), name: "laptop" };
const DESK = { id: "f".repeat(32), name: "desk" };
const OLD = { id: "e".repeat(32), name: "old-box" };

const row = (host, source, extra = {}) => ({
  host,
  supported: true,
  source,
  this_machine: !!source && source.id === ME.id,
  connected: true,
  busy: false,
  unreachable: false,
  error: null,
  ...extra,
});

/** What the backend answers for a server it could not reach or read. */
const unreachable = (host) =>
  row(host, null, { supported: false, connected: false, unreachable: true, error: `${host}: transport error` });

const status = (...rows) => ({
  rows,
  this_machine: ME,
  this_machine_error: null,
});

const shown = (state, host) => state.rows[host];

/** Start from one read naming `desk` on both servers. */
function loaded() {
  return applyRead(EMPTY_SOURCES, 1, status(row("a", DESK), row("b", DESK))).state;
}

test("a read fills every row and this machine", () => {
  const s = loaded();
  assert.deepEqual(shownRows(s).map((r) => r.host), ["a", "b"]);
  assert.equal(s.thisMachine.name, "laptop");
  assert.equal(shown(s, "a").source.name, "desk");
});

test("an older read never replaces a newer one, per server", () => {
  let s = loaded();
  // Read 3 (newer) lands first, read 2 (older) after it.
  s = applyRead(s, 3, status(row("a", ME), row("b", DESK))).state;
  s = applyRead(s, 2, status(row("a", OLD), row("b", OLD))).state;
  assert.equal(shown(s, "a").source.name, "laptop", "a keeps the newer read");
  // b's newest is read 3 too, so read 2 is older there as well.
  assert.equal(shown(s, "b").source.name, "desk");
});

test("CONTROL: two servers' moves whose read-backs resolve out of order", () => {
  // The reviewer's sequence: A's read-back captures B's old binding and waits; B
  // moves and its read-back shows B as this machine; A's older read-back arrives.
  let s = loaded();
  let seq = 1;
  s = beginMove(s, "a");
  s = beginMove(s, "b");
  assert.ok(s, "moves on two servers run side by side");
  s = moveConfirmed(s, "a", ++seq, row("a", ME));
  const readBackA = ++seq; // issued now; sees b's OLD binding
  s = moveConfirmed(s, "b", ++seq, row("b", ME));
  const readBackB = ++seq;
  s = applyRead(s, readBackB, status(row("a", ME), row("b", ME)), "b").state;
  s = applyRead(s, readBackA, status(row("a", ME), row("b", DESK)), "a").state;
  assert.equal(shown(s, "b").this_machine, true, "b's newer confirmed state stands");
  assert.equal(shown(s, "a").this_machine, true);
  assert.equal(canUse(s, "a"), false);
  assert.equal(canUse(s, "b"), false);
});

test("a read that lands while a move is in flight leaves that row to the move", () => {
  let s = loaded();
  s = beginMove(s, "a");
  assert.equal(shown(s, "a").this_machine, true, "optimistic");
  const r = applyRead(s, 5, status(row("a", DESK), row("b", OLD)));
  s = r.state;
  assert.equal(shown(s, "a").this_machine, true, "the move owns its row");
  assert.equal(shown(s, "b").source.name, "old-box", "the other row still takes it");
  // A read issued before the move's answer arrived is older than the answer.
  s = moveConfirmed(s, "a", 7, row("a", ME));
  s = applyRead(s, 6, status(row("a", DESK), row("b", OLD))).state;
  assert.equal(shown(s, "a").this_machine, true);
});

test("a second press on a row whose move is in flight does nothing", () => {
  let s = loaded();
  s = beginMove(s, "a");
  assert.equal(canUse(s, "a"), false);
  assert.equal(beginMove(s, "a"), null);
  assert.equal(canUse(s, "b"), true, "another server's row is still pressable");
});

test("a refused move goes back to its last confirmed row and keeps the refusal", () => {
  let s = loaded();
  s = beginMove(s, "a");
  s = moveFailed(s, "a", "a refused the move: a script key");
  assert.equal(shown(s, "a").source.name, "desk");
  assert.equal(shown(s, "a").this_machine, false);
  assert.equal(s.errors.a, "a refused the move: a script key");
  assert.equal(canUse(s, "a"), true);
  // The next press clears it.
  s = beginMove(s, "a");
  assert.equal(s.errors.a, undefined);
});

test("a confirmed move survives a read-back that fails, and asks for a later read", () => {
  let s = loaded();
  s = beginMove(s, "a");
  s = moveConfirmed(s, "a", 2, row("a", ME));
  // The call itself failed.
  let r = applyRead(s, 3, undefined, "a");
  assert.equal(r.retry, true);
  assert.equal(shown(r.state, "a").this_machine, true);
  // The row came back as an error.
  r = applyRead(s, 3, status(row("a", null, { error: "a: transport error" }), row("b", DESK)), "a");
  assert.equal(r.retry, true);
  assert.equal(shown(r.state, "a").this_machine, true, "the error row does not replace it");
  assert.equal(shown(r.state, "a").error, null);
  // The later read, a plain one, then reconciles as usual.
  r = applyRead(r.state, 4, status(row("a", ME, { connected: true }), row("b", DESK)));
  assert.equal(r.retry, false);
  assert.equal(shown(r.state, "a").this_machine, true);
});

test("a busy read keeps the last known source, marked busy, and asks for a later read", () => {
  let s = loaded();
  const r = applyRead(s, 2, status(row("a", null, { busy: true }), row("b", DESK)));
  assert.equal(r.retry, true);
  s = r.state;
  assert.equal(shown(s, "a").source.name, "desk", "never 'no source'");
  assert.equal(shown(s, "a").busy, true);
  // Recovery: the later read replaces it.
  const later = applyRead(s, 3, status(row("a", ME), row("b", DESK)));
  assert.equal(later.retry, false);
  assert.equal(shown(later.state, "a").busy, false);
  assert.equal(shown(later.state, "a").this_machine, true);
});

test("CONTROL: a busy answer after an unreachable one says busy, not 'cannot reach'", () => {
  // The review's sequence: healthy, then unreachable, then the server is back but busy.
  let s = loaded();
  s = applyRead(s, 2, status(unreachable("a"), row("b", DESK))).state;
  assert.equal(shown(s, "a").unreachable, true);
  const r = applyRead(s, 3, status(row("a", null, { busy: true }), row("b", DESK)));
  assert.equal(shown(r.state, "a").busy, true, "busy, retrying");
  assert.equal(shown(r.state, "a").unreachable, false, "the server answered: reachable");
  assert.equal(shown(r.state, "a").source.name, "desk", "still the last known source");
  assert.equal(r.retry, true);
  // And the other way round: unreachable after busy is not also busy.
  const u = applyRead(r.state, 4, status(unreachable("a"), row("b", DESK)));
  assert.equal(shown(u.state, "a").unreachable, true);
  assert.equal(shown(u.state, "a").busy, false);
});

test("holds for two confirmed moves are both honoured by one read", () => {
  let s = loaded();
  s = beginMove(s, "a");
  s = beginMove(s, "b");
  s = moveConfirmed(s, "a", 2, row("a", ME));
  s = moveConfirmed(s, "b", 3, row("b", ME));
  const r = applyRead(s, 4, status(row("a", null, { busy: true }), row("b", null, { error: "b: down" })), ["a", "b"]);
  assert.equal(r.retry, true);
  assert.equal(shown(r.state, "a").this_machine, true, "a's confirmed row stands");
  assert.equal(shown(r.state, "a").busy, false);
  assert.equal(shown(r.state, "b").this_machine, true, "b's confirmed row stands");
  assert.equal(shown(r.state, "b").error, null);
});

test("a busy first read shows a busy row with no source, not 'none'", () => {
  const r = applyRead(EMPTY_SOURCES, 1, status(row("a", null, { busy: true })));
  assert.equal(r.retry, true);
  assert.equal(shown(r.state, "a").busy, true);
});

test("can_use is false where this machine is already the source", () => {
  let s = loaded();
  s = applyRead(s, 2, status(row("a", ME), row("b", DESK))).state;
  assert.equal(canUse(s, "a"), false);
  assert.equal(beginMove(s, "a"), null);
  assert.equal(canUse(s, "b"), true);
  // And with no identity nothing is pressable.
  const none = applyRead(EMPTY_SOURCES, 1, { ...status(row("b", DESK)), this_machine: null, this_machine_error: "no dir" }).state;
  assert.equal(canUse(none, "b"), false);
});

test("an older server's row is not shown", () => {
  const s = applyRead(EMPTY_SOURCES, 1, status(row("a", DESK), row("old", null, { supported: false }))).state;
  assert.deepEqual(shownRows(s).map((r) => r.host), ["a"]);
});

test("a shown/focused refresh is throttled", () => {
  assert.equal(refreshDelay(10_000, 0), 0);
  assert.equal(refreshDelay(10_000, 10_000 - SOURCE_REFRESH_MIN_MS), 0);
  assert.equal(refreshDelay(10_000, 9_000), SOURCE_REFRESH_MIN_MS - 1_000);
  assert.equal(refreshDelay(10_000, 10_000), SOURCE_REFRESH_MIN_MS);
});

test("CONTROL: a read during a server restart keeps the row, marked, and reads again", () => {
  // The review's sequence against the old reducer: before 1 row, after 0, no retry.
  let s = loaded();
  assert.equal(shownRows(s).length, 2);
  const r = applyRead(s, 2, status(unreachable("a"), row("b", DESK)));
  s = r.state;
  assert.deepEqual(shownRows(s).map((x) => x.host), ["a", "b"], "the row survives");
  assert.equal(shown(s, "a").source.name, "desk", "the last known source");
  assert.equal(shown(s, "a").unreachable, true, "marked: cannot reach the server");
  assert.equal(shown(s, "a").error, null, "a note, not an error row");
  assert.equal(r.retry, true);
  assert.equal(r.retryMs, UNREACHABLE_RETRY_MS[0]);
  // The server is back: the next read replaces it and clears the mark.
  const back = applyRead(s, 3, status(row("a", ME), row("b", DESK)));
  assert.equal(shown(back.state, "a").unreachable, false);
  assert.equal(shown(back.state, "a").this_machine, true);
  assert.equal(back.retry, false);
  assert.equal(back.state.unreachableTries, 0);
});

test("unreachable reads retry with growing spacing, then stop until a show, focus or reload", () => {
  let s = loaded();
  const waits = [];
  let seq = 1;
  for (let i = 0; i < UNREACHABLE_RETRY_MS.length + 2; i++) {
    const r = applyRead(s, ++seq, status(unreachable("a"), row("b", DESK)));
    s = r.state;
    waits.push(r.retry ? r.retryMs : null);
  }
  assert.deepEqual(waits, [...UNREACHABLE_RETRY_MS, null, null]);
  for (let i = 1; i < UNREACHABLE_RETRY_MS.length; i++) {
    assert.ok(UNREACHABLE_RETRY_MS[i] > UNREACHABLE_RETRY_MS[i - 1], "growing");
  }
  assert.equal(shown(s, "a").source.name, "desk", "still the last known row");
  // Shown or focused again: a fresh series.
  s = restartRetries(s);
  const r = applyRead(s, ++seq, status(unreachable("a"), row("b", DESK)));
  assert.equal(r.retry, true);
  assert.equal(r.retryMs, UNREACHABLE_RETRY_MS[0]);
});

test("an unreachable server with no row known yet shows nothing, and is read again", () => {
  const r = applyRead(EMPTY_SOURCES, 1, status(unreachable("a"), row("b", DESK)));
  assert.deepEqual(shownRows(r.state).map((x) => x.host), ["b"]);
  assert.equal(r.retry, true);
  assert.equal(r.retryMs, UNREACHABLE_RETRY_MS[0]);
  const later = applyRead(r.state, 2, status(row("a", DESK), row("b", DESK)));
  assert.deepEqual(shownRows(later.state).map((x) => x.host), ["a", "b"]);
});

test("a confirmed move survives an unreachable read-back", () => {
  let s = loaded();
  s = beginMove(s, "a");
  s = moveConfirmed(s, "a", 2, row("a", ME));
  const r = applyRead(s, 3, status(unreachable("a"), row("b", DESK)), "a");
  assert.equal(r.retry, true);
  assert.equal(shown(r.state, "a").this_machine, true);
  assert.equal(!!shown(r.state, "a").unreachable, false);
});

test("a move's read-back that shows 'not connected' reads once more, a few seconds later", () => {
  let s = loaded();
  s = beginMove(s, "a");
  s = moveConfirmed(s, "a", 2, row("a", ME, { connected: false }));
  let r = applyRead(s, 3, status(row("a", ME, { connected: false }), row("b", DESK)), "a");
  assert.equal(r.retry, true, "the broker has not connected yet");
  assert.equal(r.retryMs, SOURCE_RETRY_MS);
  // The one later read is plain: still not connected asks for nothing more.
  const again = applyRead(r.state, 4, status(row("a", ME, { connected: false }), row("b", DESK)));
  assert.equal(again.retry, false);
  // Connected by then: shown so.
  r = applyRead(r.state, 4, status(row("a", ME, { connected: true }), row("b", DESK)));
  assert.equal(shown(r.state, "a").connected, true);
  assert.equal(r.retry, false);
  // A read-back that is already connected asks for nothing.
  s = loaded();
  s = beginMove(s, "a");
  s = moveConfirmed(s, "a", 2, row("a", ME));
  assert.equal(applyRead(s, 3, status(row("a", ME), row("b", DESK)), "a").retry, false);
});

// ---- the retry accounts belong to the newest read (review: a stale answer spent them) --

test("CONTROL: a stale unreachable read neither spends the series nor asks for a read", () => {
  // The review's sequence: read 10 lands healthy, then a delayed read 6 lands unreachable.
  let s = loaded();
  s = applyRead(s, 10, status(row("a", DESK), row("b", DESK))).state;
  const late = applyRead(s, 6, status(unreachable("a"), row("b", DESK)));
  assert.equal(late.retry, false, "no later read for a stale answer");
  assert.equal(late.state.unreachableTries, 0, "the series is untouched");
  assert.equal(shown(late.state, "a").unreachable, false, "the newer row stands");
  // Mid-series: a stale one does not advance it either.
  s = applyRead(s, 11, status(unreachable("a"), row("b", DESK))).state;
  assert.equal(s.unreachableTries, 1);
  const stale = applyRead(s, 7, status(unreachable("a"), row("b", DESK)));
  assert.equal(stale.retry, false);
  assert.equal(stale.state.unreachableTries, 1);
});

test("a read that is not the newest issued lands its rows but keeps no accounts", () => {
  let s = loaded();
  // Reads 2 and 3 overlap; 2 answers first, while 3 is still out.
  const r = applyRead(s, 2, status(row("a", OLD), row("b", DESK, { busy: true })), undefined, false);
  assert.equal(shown(r.state, "a").source.name, "old-box", "its rows still land");
  assert.equal(r.retry, false, "the newest read decides whether to read again");
  const u = applyRead(s, 2, status(unreachable("a"), row("b", DESK)), undefined, false);
  assert.equal(u.retry, false);
  assert.equal(u.state.unreachableTries, 0);
});

// ---- the window's reads and their lifetimes (createSourceReader) ---------------------

/** A reader on fake reads (answered by hand, in any order) and fake timers. */
function harness() {
  let state = loaded();
  let seq = 1;
  let out = 0; // reads in flight now
  let mostOut = 0; // the most ever in flight at once
  const reads = []; // { resolve } per fetch, in issue order
  const timers = new Map(); // id -> fn
  let nextTimer = 0;
  const cleared = [];
  let commits = 0;
  const reader = createSourceReader({
    fetch: () => {
      out++;
      mostOut = Math.max(mostOut, out);
      return new Promise((resolve) =>
        reads.push({
          resolve: (v) => {
            out--;
            resolve(v);
          },
        }),
      );
    },
    get: () => state,
    commit: (next) => {
      state = next;
      commits++;
    },
    nextSeq: () => ++seq,
    // A clock that stands still: every refresh is inside the throttle of the last read.
    now: () => 1_000_000,
    setTimer: (fn, ms) => {
      const id = ++nextTimer;
      timers.set(id, { fn, ms });
      return id;
    },
    clearTimer: (id) => {
      cleared.push(id);
      timers.delete(id);
    },
  });
  const settle = () => new Promise((r) => setTimeout(r, 0));
  return {
    reader,
    reads,
    timers,
    cleared,
    get state() {
      return state;
    },
    set state(next) {
      state = next;
    },
    get mostOut() {
      return mostOut;
    },
    get commits() {
      return commits;
    },
    /** Answer read `i` with `st`, and let its `.then` run. */
    async answer(i, st) {
      reads[i].resolve(st);
      await settle();
    },
    /** Fire every pending timer (as the clock would). */
    fireAll() {
      for (const [id, t] of [...timers]) {
        timers.delete(id);
        t.fn();
      }
    },
  };
}

test("CONTROL: a read that answers after the window is gone does nothing", async () => {
  const h = harness();
  h.reader.read();
  h.reader.dispose();
  const before = h.commits;
  await h.answer(0, status(unreachable("a"), row("b", DESK)));
  assert.equal(h.commits, before, "no rows applied");
  assert.equal(h.timers.size, 0, "no later read scheduled after cleanup");
  h.reader.read();
  h.reader.refresh();
  h.reader.reload();
  assert.equal(h.reads.length, 1, "nothing starts after dispose");
});

test("CONTROL: a later read pending at unmount is cancelled, and never reads", async () => {
  const h = harness();
  h.reader.read();
  await h.answer(0, status(unreachable("a"), row("b", DESK)));
  assert.equal(h.timers.size, 1, "the unreachable series asked for a later read");
  const [id] = [...h.timers.keys()];
  h.reader.dispose();
  assert.deepEqual(h.cleared, [id], "cancelled at cleanup");
  h.fireAll(); // a timer that fires anyway (already queued) does nothing
  assert.equal(h.reads.length, 1);
});

test("CONTROL: a reload cancels the old later read and ignores a read from before it", async () => {
  const h = harness();
  h.reader.read(); // read 0
  await h.answer(0, status(unreachable("a"), row("b", DESK)));
  assert.equal(h.timers.size, 1, "the unreachable series asked for a later read");
  const [old] = [...h.timers.keys()];
  h.reader.reload(); // read 1
  assert.ok(h.cleared.includes(old), "the old later read is cancelled");
  assert.equal(h.reads.length, 2);
  // A second reload while read 1 is out: folded into one read after it.
  h.reader.reload();
  assert.equal(h.reads.length, 2, "no read overlaps the one in flight");
  const before = h.commits;
  await h.answer(1, status(unreachable("a"), row("b", DESK)));
  assert.equal(h.commits, before, "a read from before the reload is ignored");
  assert.equal(h.timers.size, 0, "and schedules nothing");
  assert.equal(h.state.unreachableTries, 0, "nor spends the fresh series");
  assert.equal(h.reads.length, 3, "the reload's own read starts once that one is in");
  await h.answer(2, status(row("a", ME), row("b", DESK)));
  assert.equal(shown(h.state, "a").this_machine, true, "the reload's own read lands");
  assert.equal(h.mostOut, 1);
});

test("CONTROL: reads never overlap: the review's three reads are one, then one more", async () => {
  // The review's reproduction: a read, a reload and a move's read-back in a row.
  const h = harness();
  h.reader.read();
  h.reader.reload();
  h.reader.read("a");
  assert.equal(h.reads.length, 1, "one read in flight, not three");
  await h.answer(0, status(row("a", DESK), row("b", DESK)));
  assert.equal(h.reads.length, 2, "exactly one follow-up");
  await h.answer(1, status(row("a", DESK), row("b", DESK)));
  assert.equal(h.reads.length, 2, "and no more");
  assert.equal(h.mostOut, 1, "never two at once");
});

test("a pending later read is cancelled when a read starts", async () => {
  const h = harness();
  h.reader.read(); // 0
  await h.answer(0, status(unreachable("a"), row("b", DESK)));
  assert.equal(h.timers.size, 1);
  const [id] = [...h.timers.keys()];
  h.reader.read(); // 1: the read the timer was waiting to make
  assert.ok(h.cleared.includes(id), "cancelled");
  assert.equal(h.timers.size, 0);
  // A timer that fires anyway (already queued) while read 1 is out makes no second read.
  assert.equal(h.reads.length, 2);
});

test("a move's hold asked for during a read rides on the follow-up", async () => {
  const h = harness();
  h.reader.read(); // read 0 (seq 2), out
  // The move on `a` is confirmed meanwhile (its answer's sequence is after read 0's).
  h.state = moveConfirmed(beginMove(h.state, "a"), "a", 2.5, row("a", ME));
  h.reader.read("a"); // the move's read-back: folded into the follow-up
  assert.equal(h.reads.length, 1);
  await h.answer(0, status(row("a", DESK), row("b", DESK)));
  assert.equal(shown(h.state, "a").this_machine, true, "read 0 is older than the move");
  assert.equal(h.reads.length, 2, "the follow-up started");
  await h.answer(1, status(row("a", null, { busy: true }), row("b", DESK)));
  assert.equal(shown(h.state, "a").this_machine, true, "the confirmed row stands");
  assert.equal(shown(h.state, "a").busy, false, "held: a busy read-back is not applied over it");
  assert.equal(h.timers.size, 1, "and one later read is asked for");
  assert.equal([...h.timers.values()][0].ms, SOURCE_RETRY_MS);
});
