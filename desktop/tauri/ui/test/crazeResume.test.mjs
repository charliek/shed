/* The craze create sheet's Resume mode (`src/lib/crazeResume.ts`, plan 032
 * §3.4, C5), on node's own test runner — the `crazeCreate.test.mjs`
 * arrangement.
 *
 * The cases marked CONTROL are the plan's own: Resume shown exactly when the
 * create options say `reopen`, the list read on the FIRST choice of Resume,
 * one reopen at a time with every other button disabled, `gone` dropping the
 * row, `start_failed` and contract errors keeping it, `unavailable` offering
 * Retry — branched on the code, never the words — and the C5 review's: the
 * state bound to the machine's INCARNATION (a host removed and added again
 * under the same name starts afresh, and the old one's late answer is
 * dropped whole), a `superseded` answer never opened — and the re-review's:
 * every STATUS names the incarnation, so a new one resets the state with no
 * create-options read at all, and a failed options read offers its Retry in
 * the Resume half too. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  EMPTY_RESUME,
  MORE_NOTE,
  RESUMING_LABEL,
  SUPERSEDED_NOTE,
  UNTITLED,
  beginList,
  beginResume,
  canResume,
  chooseMode,
  listNeeded,
  modeLocked,
  observeSource,
  observeSources,
  onSheetOpen,
  optionsRetryHalf,
  relativeAge,
  reopenLanding,
  resumeLabel,
  resumeOffered,
  resumingId,
  rowState,
  savedRowError,
  savedTitle,
  settleList,
  settleResume,
  shortenHome,
} from "../dist-test/crazeResume.js";

const A = { id: "0192f0aa-a", provider: "grok", workspace: "/home/me/proj", title: "fix the parser", updated_unix_ms: 1_000 };
const B = { id: "grok:legacy", provider: "grok", workspace: "/srv/other", title: null, updated_unix_ms: 500 };

/** The sheet in Resume mode with [A, B] listed. */
function listed(more = false) {
  const r = beginList(chooseMode(EMPTY_RESUME, "resume"), 1);
  return settleList(r, 1, { ok: true, list: { sessions: [A, B], more, home: "/home/me" } });
}

/** The same, listed through the machine's craze source of generation `gen`. */
function listedAt(gen) {
  const r = beginList(chooseMode(EMPTY_RESUME, "resume"), 1);
  return settleList(r, 1, { ok: true, list: { sessions: [A, B], more: false, home: "/home/me", source_gen: gen } });
}

test("CONTROL: New | Resume shows exactly when the options say reopen", () => {
  assert.equal(resumeOffered(true, EMPTY_RESUME), true);
  assert.equal(resumeOffered(false, EMPTY_RESUME), false, "a hub before 0.2.0");
  assert.equal(resumeOffered(undefined, EMPTY_RESUME), false, "options not read yet, or an older app's");
  // A sheet reopened while its reopen runs shows Resume before its options land.
  const running = beginResume(listed(), A.id, 1);
  assert.equal(resumeOffered(undefined, running), true);
});

test("CONTROL: the list is read on the FIRST choice of Resume, and not again until the sheet reopens", () => {
  assert.equal(listNeeded(EMPTY_RESUME), false, "the sheet opens on New");
  const chosen = chooseMode(EMPTY_RESUME, "resume");
  assert.equal(chosen.mode, "resume");
  assert.equal(listNeeded(chosen), true);
  const loading = beginList(chosen, 7);
  assert.equal(listNeeded(loading), false, "a read is running");
  const ready = settleList(loading, 7, { ok: true, list: { sessions: [A], more: false } });
  assert.equal(listNeeded(chooseMode(chooseMode(ready, "new"), "resume")), false, "kept across New and back");
  assert.deepEqual(onSheetOpen(ready), EMPTY_RESUME, "nothing cached across opens");
  assert.equal(listNeeded(chooseMode(onSheetOpen(ready), "resume")), true);
});

test("a read's answer lands only on the read that asked (the token)", () => {
  const loading = beginList(chooseMode(EMPTY_RESUME, "resume"), 2);
  const late = settleList(loading, 1, { ok: true, list: { sessions: [A], more: false } });
  assert.deepEqual(late, loading, "an older read's answer is dropped");
  const failed = settleList(loading, 2, { ok: false, refusal: { code: "unavailable", message: "busy" } });
  assert.deepEqual(failed.load, { phase: "failed", refusal: { code: "unavailable", message: "busy" } });
  assert.deepEqual(settleList(failed, 2, { ok: true, list: { sessions: [A], more: false } }), failed,
    "a settled read takes no second answer");
  // Retry after a failed read is a new read.
  assert.equal(beginList(failed, 3).load.phase, "loading");
});

test("a fresh list keeps craze's order, more, the home — and forgets settled rows", () => {
  const r = listed(true);
  assert.deepEqual(r.load.sessions.map((s) => s.id), [A.id, B.id]);
  assert.equal(r.load.more, true);
  assert.equal(r.load.home, "/home/me");
  assert.equal(MORE_NOTE, "Showing the 50 most recent.");
  const refused = settleResume(beginResume(r, A.id, 1), A.id, 1, { ok: false, refusal: { code: "start_failed", message: "x" } });
  const again = settleList(beginList(refused, 9), 9, { ok: true, list: { sessions: [A], more: false } });
  assert.deepEqual(rowState(again, A.id), { phase: "idle" });
  assert.equal(again.load.home, null, "no home said: none known");
});

test("CONTROL: one reopen at a time — every other button is disabled while one runs", () => {
  const r = listed();
  assert.equal(canResume(r, A.id), true);
  assert.equal(canResume(r, B.id), true);
  assert.equal(canResume(r, "not-listed"), false);
  const running = beginResume(r, A.id, 1);
  assert.equal(resumingId(running), A.id);
  assert.deepEqual(rowState(running, A.id), { phase: "resuming", attempt: 1 });
  assert.equal(canResume(running, B.id), false, "the other row waits");
  assert.equal(canResume(running, A.id), false, "and its own is Resuming…");
  assert.deepEqual(beginResume(running, B.id, 2), running, "a second press is refused");
  assert.equal(resumeLabel(rowState(running, A.id)), RESUMING_LABEL);
  assert.equal(RESUMING_LABEL, "Resuming… (this can take a few minutes)");
  assert.equal(canResume(chooseMode(r, "new"), A.id), false, "only in Resume");
});

test("CONTROL: the mode is locked while a create submits or a reopen runs", () => {
  assert.equal(modeLocked(false, EMPTY_RESUME), false);
  assert.equal(modeLocked(true, EMPTY_RESUME), true);
  assert.equal(modeLocked(false, beginResume(listed(), A.id, 1)), true);
});

test("CONTROL: success takes the row off the list — it runs now", () => {
  const done = settleResume(beginResume(listed(), A.id, 1), A.id, 1, { ok: true, answer: {} });
  assert.deepEqual(done.load.sessions.map((s) => s.id), [B.id]);
  assert.equal(resumingId(done), null);
  assert.equal(done.gone, null);
  assert.equal(canResume(done, B.id), true, "the others work again");
});

test("CONTROL: gone drops the row and says why where it was", () => {
  const gone = settleResume(beginResume(listed(), A.id, 1), A.id, 1,
    { ok: false, refusal: { code: "gone", message: "the session's directory /home/me/proj is no longer a directory" } });
  assert.deepEqual(gone.load.sessions.map((s) => s.id), [B.id]);
  assert.deepEqual(gone.gone, {
    id: A.id, title: "fix the parser",
    message: "the session's directory /home/me/proj is no longer a directory",
  });
  assert.deepEqual(rowState(gone, A.id), { phase: "idle" });
  // The next reopen clears the note.
  assert.equal(beginResume(gone, B.id, 2).gone, null);
  // An untitled one is named "Untitled".
  const untitled = settleResume(beginResume(listed(), B.id, 1), B.id, 1, { ok: false, refusal: { code: "gone", message: "m" } });
  assert.equal(untitled.gone.title, UNTITLED);
});

test("CONTROL: start_failed and contract errors keep the row, said inline; Resume works again", () => {
  for (const code of ["start_failed", "failed", "bad_request", "shutting_down", "too_old"]) {
    const refusal = { code, message: `words for ${code}` };
    const r = settleResume(beginResume(listed(), A.id, 1), A.id, 1, { ok: false, refusal });
    assert.deepEqual(r.load.sessions.map((s) => s.id), [A.id, B.id], code);
    assert.deepEqual(rowState(r, A.id), { phase: "refused", refusal }, code);
    assert.equal(resumeLabel(rowState(r, A.id)), "Resume", code);
    assert.equal(canResume(r, A.id), true, code);
    assert.equal(r.gone, null, code);
  }
  const failed = settleResume(beginResume(listed(), A.id, 1), A.id, 1,
    { ok: false, refusal: { code: "start_failed", message: "could not start: grok exited" } });
  assert.deepEqual(savedRowError(rowState(failed, A.id)), { where: "cause", text: "could not start: grok exited" });
  const other = settleResume(beginResume(listed(), A.id, 1), A.id, 1, { ok: false, refusal: { code: "failed", message: "boom" } });
  assert.deepEqual(savedRowError(rowState(other, A.id)), { where: "general", text: "boom" });
});

test("CONTROL: unavailable — a lost answer's too — offers Retry, the row kept", () => {
  const lost = "the reopen's answer was lost; the session may already be running — Retry is safe (twice: deadline)";
  const r = settleResume(beginResume(listed(), A.id, 1), A.id, 1, { ok: false, refusal: { code: "unavailable", message: lost } });
  assert.equal(rowState(r, A.id).phase, "unavailable");
  assert.equal(resumeLabel(rowState(r, A.id)), "Retry");
  assert.deepEqual(savedRowError(rowState(r, A.id)), { where: "general", text: `${lost} — Retry` });
  assert.equal(canResume(r, A.id), true);
  assert.deepEqual(r.load.sessions.map((s) => s.id), [A.id, B.id]);
});

test("a settle with no reopen running changes nothing", () => {
  const r = listed();
  assert.deepEqual(settleResume(r, A.id, 1, { ok: true, answer: {} }), r);
});

test("CONTROL: a sheet opening keeps a running reopen, else starts fresh", () => {
  const running = beginResume(listed(), A.id, 1);
  assert.deepEqual(onSheetOpen(running), running);
  const settled = settleResume(running, A.id, 1, { ok: false, refusal: { code: "failed", message: "x" } });
  assert.deepEqual(onSheetOpen(settled), EMPTY_RESUME);
  // Afresh — but still bound to its incarnation, which fences an older
  // one's late word out of the fresh sheet.
  const settledAt = settleResume(beginResume(listedAt(5), A.id, 1), A.id, 1,
    { ok: false, refusal: { code: "failed", message: "x" } });
  assert.deepEqual(onSheetOpen(settledAt), { ...EMPTY_RESUME, gen: 5 });
  assert.equal(observeSource(onSheetOpen(settledAt), 4).gen, 5, "an older incarnation's word binds nothing");
});

test("a fresh list keeps a running reopen's row", () => {
  const running = beginResume(listed(), A.id, 1);
  const relisted = settleList(beginList(running, 5), 5, { ok: true, list: { sessions: [A, B], more: false } });
  assert.equal(resumingId(relisted), A.id);
});

test("CONTROL (C5 review): an answer names the incarnation — the first binds, the same keeps, another starts afresh", () => {
  assert.equal(EMPTY_RESUME.gen, null);
  const bound = observeSource(EMPTY_RESUME, 5);
  assert.equal(bound.gen, 5, "the first binds");
  assert.equal(observeSource(bound, 5), bound, "the same incarnation changes nothing");
  assert.equal(observeSource(bound, null), bound, "none said changes nothing");
  assert.equal(observeSource(bound, undefined), bound);
  assert.equal(listedAt(5).gen, 5, "the list binds it too");
  const running = beginResume(listedAt(5), A.id, 1);
  // Dismissed while it runs, and opened again on the SAME machine: as left.
  assert.deepEqual(onSheetOpen(running), running);
  assert.equal(observeSource(onSheetOpen(running), 5), running);
  // Removed and added again under the same name meanwhile: the next open's
  // options name generation 6 — a new incarnation, a fresh sheet.
  const fresh = observeSource(onSheetOpen(running), 6);
  assert.deepEqual(fresh, { ...EMPTY_RESUME, gen: 6 }, "on New, nothing listed, nothing running");
  assert.equal(modeLocked(false, fresh), false, "not locked by the old incarnation's reopen");
  assert.equal(listNeeded(chooseMode(fresh, "resume")), true, "and it reads its own list");
});

test("CONTROL (C5 review): an older incarnation's late answer is dropped — no state change, no navigation", () => {
  const fresh = observeSource(beginResume(listedAt(5), A.id, 1), 6);
  for (const answer of [{}, { ended: true }, { superseded: true }]) {
    assert.equal(reopenLanding(fresh, A.id, 1, answer), "dropped", JSON.stringify(answer));
    assert.deepEqual(settleResume(fresh, A.id, 1, { ok: true, answer }), fresh, JSON.stringify(answer));
  }
  assert.deepEqual(settleResume(fresh, A.id, 1, { ok: false, refusal: { code: "gone", message: "m" } }), fresh,
    "its refusal too");
  // The same session resumed again on the new incarnation is ITS OWN attempt:
  // the old answer never lands on it.
  const relisted = settleList(beginList(chooseMode(fresh, "resume"), 2), 2,
    { ok: true, list: { sessions: [A, B], more: false, source_gen: 6 } });
  const again = beginResume(relisted, A.id, 2);
  assert.equal(reopenLanding(again, A.id, 1, {}), "dropped", "the old attempt's answer");
  assert.deepEqual(settleResume(again, A.id, 1, { ok: true, answer: {} }), again);
  assert.equal(reopenLanding(again, A.id, 2, {}), "open", "its own lands");
});

test("the list binds the state to its incarnation — an older one's rows go", () => {
  // Removed and added again between the options read and the list read.
  const reading = beginList(chooseMode(observeSource(EMPTY_RESUME, 5), "resume"), 3);
  const moved = settleList(reading, 3, { ok: true, list: { sessions: [A], more: false, source_gen: 6 } });
  assert.equal(moved.gen, 6);
  assert.deepEqual(moved.load.sessions.map((s) => s.id), [A.id]);
  // A running reopen never survives a list of another incarnation (no Resume
  // re-reads while one runs; the rule holds regardless).
  const running = beginResume(listedAt(5), A.id, 1);
  const rereading = { ...running, load: { phase: "loading", token: 9 } };
  const other = settleList(rereading, 9, { ok: true, list: { sessions: [A], more: false, source_gen: 6 } });
  assert.equal(resumingId(other), null);
  assert.equal(reopenLanding(other, A.id, 1, {}), "dropped");
  const same = settleList(rereading, 9, { ok: true, list: { sessions: [A], more: false, source_gen: 5 } });
  assert.deepEqual(rowState(same, A.id), { phase: "resuming", attempt: 1 }, "its own incarnation's keeps it");
});

test("CONTROL (C5 re-review): a status naming a newer incarnation resets a running reopen's lock — no options read", () => {
  // A reopen left running on generation 5; the sheet dismissed. The host is
  // removed and registered again, and the replacement's create-options read
  // FAILS — so only its status (`rc.list`'s `craze.source_gen`) names it.
  const running = beginResume(listedAt(5), A.id, 1);
  assert.equal(modeLocked(false, running), true, "locked while it runs");
  const other = beginResume(listedAt(3), B.id, 2);
  const all = { mini3: running, "roost:srv/s1": other };
  const seen = observeSources(all, (m) => (m === "mini3" ? 6 : 3));
  assert.deepEqual(seen.mini3, { ...EMPTY_RESUME, gen: 6 }, "afresh, on New, bound to the new incarnation");
  assert.equal(modeLocked(false, seen.mini3), false, "the old incarnation's reopen no longer locks it");
  assert.equal(resumeOffered(undefined, seen.mini3), false, "nor shows Resume before ITS options say reopen");
  assert.equal(seen["roost:srv/s1"], other, "a machine whose incarnation is unchanged keeps its state, as is");
  // The sheet opened next shows the fresh state; the old answer lands nowhere.
  assert.deepEqual(onSheetOpen(seen.mini3), seen.mini3);
  assert.equal(reopenLanding(seen.mini3, A.id, 1, {}), "dropped");
  assert.deepEqual(settleResume(seen.mini3, A.id, 1, { ok: true, answer: {} }), seen.mini3);
  // The shell's opening applies the status first, too.
  assert.deepEqual(onSheetOpen(observeSource(running, 6)), { ...EMPTY_RESUME, gen: 6 });
});

test("observeSources hands back the SAME object when no status changes anything", () => {
  const running = beginResume(listedAt(5), A.id, 1);
  const all = { mini3: running };
  assert.equal(observeSources(all, () => 5), all, "the same incarnation");
  assert.equal(observeSources(all, () => undefined), all, "not shown (removed, or no status yet)");
  assert.equal(observeSources(all, () => null), all, "no craze source yet");
  const none = {};
  assert.equal(observeSources(none, () => 9), none, "no state held: nothing to bind");
  // An unbound state is bound by the first status, nothing else changed.
  const bound = observeSources({ m: EMPTY_RESUME }, () => 4);
  assert.deepEqual(bound.m, { ...EMPTY_RESUME, gen: 4 });
});

test("CONTROL (C5 re-review): an OLDER incarnation's word changes nothing — generations only grow", () => {
  // The status said 7 first; an options read that began on 5 lands after.
  const bound = observeSource(EMPTY_RESUME, 7);
  assert.equal(observeSource(bound, 5), bound, "a late word from an incarnation already replaced");
  const running = beginResume(listedAt(7), A.id, 1);
  assert.equal(observeSource(running, 5), running, "a running reopen of the CURRENT one is kept");
  assert.equal(observeSources({ m: running }, () => 5).m, running);
  // A list of an older incarnation lands as words, the binding and the
  // running reopen kept.
  const rereading = { ...running, load: { phase: "loading", token: 4 } };
  const old = settleList(rereading, 4, { ok: true, list: { sessions: [A], more: false, source_gen: 5 } });
  assert.equal(old.gen, 7);
  assert.deepEqual(rowState(old, A.id), { phase: "resuming", attempt: 1 });
});

test("CONTROL (C5 re-review): a failed options read offers its Retry in the half that shows — Resume included", () => {
  const running = beginResume(listedAt(5), A.id, 1);
  assert.equal(running.mode, "resume");
  assert.equal(modeLocked(false, running), true, "the mode control is locked…");
  assert.equal(optionsRetryHalf(true, running), "resume", "…and the options' Retry still shows, in Resume");
  assert.equal(optionsRetryHalf(true, EMPTY_RESUME), "new", "in New, in the provider field");
  assert.equal(optionsRetryHalf(true, chooseMode(listed(), "new")), "new");
  assert.equal(optionsRetryHalf(false, running), null, "no failure, no Retry");
  assert.equal(optionsRetryHalf(false, EMPTY_RESUME), null);
});

test("CONTROL (C5 review): a superseded answer is not openable — said on the row, the row kept", () => {
  const running = beginResume(listedAt(5), A.id, 1);
  assert.equal(reopenLanding(running, A.id, 1, { superseded: true }), "superseded");
  assert.equal(reopenLanding(running, A.id, 1, { superseded: true, ended: false }), "superseded",
    "a session still running, on a source the machine no longer has");
  const r = settleResume(running, A.id, 1, { ok: true, answer: { superseded: true, ended: false } });
  assert.deepEqual(r.load.sessions.map((s) => s.id), [A.id, B.id], "kept: Resume again joins it");
  assert.deepEqual(rowState(r, A.id), { phase: "refused", refusal: { code: "superseded", message: SUPERSEDED_NOTE } });
  assert.deepEqual(savedRowError(rowState(r, A.id)), { where: "general", text: SUPERSEDED_NOTE });
  assert.equal(resumeLabel(rowState(r, A.id)), "Resume");
  assert.equal(canResume(r, A.id), true);
  assert.equal(modeLocked(false, r), false);
});

test("CONTROL: a current answer opens its transcript — or says it ended", () => {
  const running = beginResume(listedAt(5), A.id, 1);
  assert.equal(reopenLanding(running, A.id, 1, {}), "open");
  assert.equal(reopenLanding(running, A.id, 1, { ended: false, superseded: false }), "open");
  assert.equal(reopenLanding(running, A.id, 1, { ended: true }), "ended");
  const ended = settleResume(running, A.id, 1, { ok: true, answer: { ended: true } });
  assert.deepEqual(ended.load.sessions.map((s) => s.id), [B.id], "as any success: its row leaves the list");
});

test("the title, or Untitled", () => {
  assert.equal(savedTitle(A), "fix the parser");
  assert.equal(savedTitle(B), "Untitled");
  assert.equal(savedTitle({ title: "   " }), "Untitled");
  assert.equal(savedTitle({}), "Untitled");
});

test("CONTROL: the home shortens to ~ only when known, and only under it", () => {
  assert.equal(shortenHome("/home/me/proj", "/home/me"), "~/proj");
  assert.equal(shortenHome("/home/me", "/home/me"), "~");
  assert.equal(shortenHome("/home/me/proj", "/home/me/"), "~/proj", "a trailing slash on the home");
  assert.equal(shortenHome("/home/meadow/x", "/home/me"), "/home/meadow/x", "a prefix of letters is not under it");
  assert.equal(shortenHome("/home/me/proj", null), "/home/me/proj", "no home known: the path whole");
  assert.equal(shortenHome("/home/me/proj", ""), "/home/me/proj");
  assert.equal(shortenHome("/x", "/"), "/x", "a root home shortens nothing");
  assert.equal(shortenHome("/home/shed/app", "/home/shed"), "~/app");
});

test("the age reads relative, then as a date", () => {
  const now = Date.UTC(2026, 9, 16, 12, 0, 0);
  const ago = (ms) => relativeAge(now - ms, now);
  assert.equal(ago(5_000), "just now");
  assert.equal(relativeAge(now + 60_000, now), "just now", "a clock ahead of ours");
  assert.equal(ago(5 * 60_000), "5 min ago");
  assert.equal(ago(3 * 3_600_000), "3 h ago");
  assert.equal(ago(30 * 3_600_000), "yesterday");
  assert.equal(ago(4 * 86_400_000), "4 days ago");
  assert.equal(relativeAge(Date.UTC(2026, 7, 1), now), "2026-08-01");
  assert.equal(relativeAge(0, now), "", "no time");
});
