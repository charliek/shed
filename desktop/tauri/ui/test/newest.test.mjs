/* The newest-wins read guard (`src/lib/newest.ts`), on node's own test runner.
 *
 * Plain `.mjs` against the JS `npm test` emits from the TypeScript, so this
 * needs no test framework, no DOM and no transform — nothing joins the lock for
 * it. What it drives is the exact mechanism the lane panel's pull uses; the
 * component's own wiring is a three-line call that `tsc` type-checks.
 *
 * Every ordering here is landed EXPLICITLY (deferred promises resolved by hand),
 * so nothing depends on a scheduler racing the way this run happened to. */
import test from "node:test";
import assert from "node:assert/strict";

import { newestReload, newestWins } from "../dist-test/newest.js";

/** A promise this test resolves (or rejects) when it decides to. */
function deferred() {
  let settle;
  let fail;
  const promise = new Promise((resolve, reject) => {
    settle = resolve;
    fail = reject;
  });
  return { promise, resolve: settle, reject: fail };
}

/** A recorder for what actually reached the screen. */
function sink() {
  const committed = [];
  const failed = [];
  return {
    committed,
    failed,
    commit: (value) => committed.push(value),
    fail: (error) => failed.push(String(error)),
  };
}

test("a read that lands late does not overwrite a newer one", async () => {
  const guard = newestWins();
  const out = sink();
  const older = deferred();
  const newer = deferred();

  const first = guard.run(() => older.promise, out.commit, out.fail);
  const second = guard.run(() => newer.promise, out.commit, out.fail);

  // The SECOND read answers first — the whole point.
  newer.resolve("fresh");
  await second;
  older.resolve("stale");
  await first;

  assert.deepEqual(out.committed, ["fresh"], "a stale read overwrote a fresh one");
  assert.deepEqual(out.failed, []);
});

test("reads that land in order both commit", async () => {
  const guard = newestWins();
  const out = sink();
  const first = deferred();
  const second = deferred();

  const a = guard.run(() => first.promise, out.commit, out.fail);
  const b = guard.run(() => second.promise, out.commit, out.fail);
  first.resolve("one");
  await a;
  second.resolve("two");
  await b;

  assert.deepEqual(out.committed, ["one", "two"], "in-order reads must both show");
});

test("an older FAILURE does not overwrite a newer success", async () => {
  const guard = newestWins();
  const out = sink();
  const older = deferred();
  const newer = deferred();

  const first = guard.run(() => older.promise, out.commit, out.fail);
  const second = guard.run(() => newer.promise, out.commit, out.fail);

  newer.resolve("fresh");
  await second;
  older.reject(new Error("the lane was closed a moment ago"));
  await first;

  assert.deepEqual(out.committed, ["fresh"]);
  assert.deepEqual(out.failed, [], "a stale read error replaced a fresh view");
});

test("a newer failure DOES replace an older success", async () => {
  const guard = newestWins();
  const out = sink();
  const older = deferred();
  const newer = deferred();

  const first = guard.run(() => older.promise, out.commit, out.fail);
  const second = guard.run(() => newer.promise, out.commit, out.fail);

  older.resolve("stale");
  await first;
  newer.reject(new Error("unavailable: the tunnel went away"));
  await second;

  assert.deepEqual(out.committed, ["stale"]);
  assert.deepEqual(
    out.failed,
    ["Error: unavailable: the tunnel went away"],
    "the newest answer must win even when it is a failure",
  );
});

test("cancel stops every read still in flight", async () => {
  const guard = newestWins();
  const out = sink();
  const pending = deferred();
  const failing = deferred();

  const a = guard.run(() => pending.promise, out.commit, out.fail);
  const b = guard.run(() => failing.promise, out.commit, out.fail);
  guard.cancel();
  pending.resolve("after the unmount");
  failing.reject(new Error("after the unmount"));
  await a;
  await b;

  assert.deepEqual(out.committed, [], "a read committed after the panel unmounted");
  assert.deepEqual(out.failed, []);
});

/* ---- newestReload: the shell's host list (shed#400) ---------------------
 *
 * `App.tsx`'s host list re-reads `system_df` on every `refresh` and feeds the
 * Hosts badge and the sidebar from whatever it committed. These pin the reload
 * it goes through: a bare `read().then(commit)` there is what these turn red. */

/** Two host rows, the shape `system_df` answers with (one host down). */
const TWO_HOSTS = [
  { host: "mock", usage: { totals: {} } },
  { host: "down", usage: null, error: "connection refused" },
];
const ONE_HOST = [{ host: "mock", usage: { totals: {} } }];

test("host list: an older answer landing after a newer one does not commit", async () => {
  const committed = [];
  const answers = [deferred(), deferred()];
  let calls = 0;
  const rows = newestReload(() => answers[calls++].promise, (r) => committed.push(r));

  const older = rows.reload();
  const newer = rows.reload();
  // The newer read answers first: the host set changed between the two.
  answers[1].resolve(TWO_HOSTS);
  await newer;
  answers[0].resolve(ONE_HOST);
  await older;

  assert.deepEqual(committed, [TWO_HOSTS], "a stale host list overwrote a fresh one");
});

test("host list: a failed reload keeps the last rows", async () => {
  const committed = [];
  const first = deferred();
  const second = deferred();
  const queue = [first, second];
  const rows = newestReload(() => queue.shift().promise, (r) => committed.push(r));

  const a = rows.reload();
  first.resolve(TWO_HOSTS);
  await a;
  const b = rows.reload();
  second.reject(new Error("ipc: the webview lost its channel"));
  await b; // settles quietly: a failed read is not an answer about the hosts

  assert.deepEqual(committed, [TWO_HOSTS], "a failed read replaced the host list");
});

test("host list: an older answer landing after a newer FAILURE does not commit either", async () => {
  const committed = [];
  const answers = [deferred(), deferred(), deferred()];
  let calls = 0;
  const rows = newestReload(() => answers[calls++].promise, (r) => committed.push(r));

  const seed = rows.reload();
  answers[0].resolve(TWO_HOSTS);
  await seed;
  const older = rows.reload();
  const newer = rows.reload();
  answers[2].reject(new Error("ipc: refused"));
  await newer;
  answers[1].resolve(ONE_HOST);
  await older;

  assert.deepEqual(committed, [TWO_HOSTS], "a stale answer committed past a newer read");
});

test("host list: in-order reloads both commit, and cancel stops the rest", async () => {
  const committed = [];
  const answers = [deferred(), deferred(), deferred()];
  let calls = 0;
  const rows = newestReload(() => answers[calls++].promise, (r) => committed.push(r));

  const a = rows.reload();
  answers[0].resolve(ONE_HOST);
  await a;
  const b = rows.reload();
  answers[1].resolve(TWO_HOSTS);
  await b;
  const c = rows.reload();
  rows.cancel();
  answers[2].resolve(ONE_HOST);
  await c;

  assert.deepEqual(committed, [ONE_HOST, TWO_HOSTS], "in-order reads must both show; none after cancel");
});
