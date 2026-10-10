/* The craze create sheet's pure rules (`src/lib/crazeCreate.ts`, plan 025
 * §3.8), on node's own test runner — the `crazeRows.test.mjs` arrangement.
 *
 * The cases marked CONTROL are the ones plan 025's C10 control list names: a
 * non-ready provider must never be selectable, a not-ready default never
 * preselected, the request id kept ONLY across an unknown outcome and minted
 * anew after any definite one, and the typed form never cleared. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  EMPTY_DRAFT,
  HELD_EDIT_NOTE,
  NO_PROVIDER_READY,
  OUTCOME_UNKNOWN_NOTE,
  beginSubmit,
  closesOntoTranscript,
  crazeCreateGate,
  crazeCreateOffered,
  crazeCreateUpdateNote,
  crazeSheetNote,
  directoryProblem,
  edit,
  groupMachine,
  liveSetter,
  mintRequestId,
  preselectedProvider,
  primaryLabel,
  providerSelectable,
  reconcileProvider,
  refusalView,
  replaysHeldRequest,
  settle,
  sheetOnScreen,
} from "../dist-test/crazeCreate.js";

/** craze's own createOptions on the recipe rig (cursor missing, native with no
 *  key, grok ready and the default). */
const recipe = {
  providers: [
    { id: "cursor", label: "cursor", state: "unavailable", reason: "cursor-agent not found on PATH", fix: "install cursor-agent" },
    { id: "grok", label: "grok", state: "ready" },
    { id: "native", label: "native", state: "needs_setup", reason: "no model provider has a key", fix: "craze auth login" },
  ],
  default_provider: "grok",
  recent_dirs: ["/w/a", "/w/b"],
};

test("CONTROL: only a ready provider is selectable", () => {
  assert.deepEqual(
    recipe.providers.map((p) => [p.id, providerSelectable(p)]),
    [["cursor", false], ["grok", true], ["native", false]],
  );
  assert.equal(providerSelectable({ id: "x", label: "x", state: "warming_up" }), false, "an unknown word is not ready");
});

test("CONTROL: the default is preselected only when it is listed and ready", () => {
  assert.equal(preselectedProvider(recipe), "grok");
  // A not-ready default: the first READY provider instead.
  assert.equal(preselectedProvider({ ...recipe, default_provider: "native" }), "grok");
  // A default craze does not list (a missing gx): the first ready.
  assert.equal(preselectedProvider({ ...recipe, default_provider: "gx" }), "grok");
  // None ready: nothing preselected.
  assert.equal(
    preselectedProvider({ providers: recipe.providers.filter((p) => p.id !== "grok"), default_provider: "native", recent_dirs: [] }),
    null,
  );
});

test("a carried-over selection survives only while still ready", () => {
  assert.equal(reconcileProvider("grok", recipe), "grok");
  assert.equal(reconcileProvider("cursor", recipe), "grok", "a dimmed provider is never kept");
  assert.equal(reconcileProvider("gone", recipe), "grok");
  assert.equal(reconcileProvider(null, recipe), "grok");
});

test("a directory must be absolute", () => {
  assert.ok(directoryProblem(""));
  assert.ok(directoryProblem("   "));
  assert.ok(directoryProblem("relative/path"));
  assert.ok(directoryProblem("~/w"));
  assert.equal(directoryProblem("/w/a"), null);
  assert.equal(directoryProblem("  /w/a  "), null);
});

test("a minted request id is craze's form: shed- and a v4 UUID's 32 hex digits", () => {
  const id = mintRequestId();
  assert.match(id, /^shed-[0-9a-f]{12}4[0-9a-f]{3}[89ab][0-9a-f]{15}$/);
  assert.ok(id.length <= 64);
  assert.notEqual(mintRequestId(), id);
});

const minted = (() => {
  let n = 0;
  return () => `shed-id-${++n}`;
})();

test("CONTROL: an unknown outcome keeps the id, and the retry reuses it", () => {
  let d = { ...EMPTY_DRAFT, provider: "grok", cwd: "/w", prompt: "hi" };
  d = beginSubmit(d, minted);
  const first = d.requestId;
  assert.ok(first);
  assert.equal(d.phase, "submitting");
  d = settle(d, { ok: false, refusal: { code: "outcome_unknown", message: "outcome unknown: lost twice" } });
  assert.equal(d.phase, "unknown");
  assert.equal(d.requestId, first, "kept while the outcome is unknown");
  assert.equal(primaryLabel(d), "Try again");
  d = beginSubmit(d, minted);
  assert.equal(d.requestId, first, "Try again resumes the SAME request");
  d = settle(d, { ok: true });
  assert.equal(d.phase, "created");
  assert.equal(d.requestId, null);
});

test("CONTROL: any definite answer ends the id, so Try again mints a new one", () => {
  for (const code of ["failed", "bad_request", "unavailable", "not_accepting"]) {
    let d = beginSubmit({ ...EMPTY_DRAFT, provider: "grok", cwd: "/w" }, minted);
    const first = d.requestId;
    d = settle(d, { ok: false, refusal: { code, message: "no" } });
    assert.equal(d.phase, "refused", code);
    assert.equal(d.requestId, null, `${code}: the id's life is over`);
    d = beginSubmit(d, minted);
    assert.notEqual(d.requestId, first, `${code}: a fresh id`);
  }
});

test("CONTROL: no state clears the typed form", () => {
  const typed = { ...EMPTY_DRAFT, provider: "grok", cwd: "/w/x", prompt: "line one\nline two" };
  let d = beginSubmit(typed, minted);
  for (const outcome of [
    { ok: false, refusal: { code: "outcome_unknown", message: "lost" } },
    { ok: false, refusal: { code: "failed", message: "Error: KEYCHAIN LOCKED / Run unlock and retry." } },
    { ok: true },
  ]) {
    d = settle(beginSubmit(d, minted), outcome);
    assert.deepEqual([d.provider, d.cwd, d.prompt], ["grok", "/w/x", "line one\nline two"]);
  }
});

test("an edit mints a new id and clears the settled outcome, not the form", () => {
  let d = settle(beginSubmit({ ...EMPTY_DRAFT, provider: "grok", cwd: "/w" }, minted), {
    ok: false,
    refusal: { code: "outcome_unknown", message: "lost" },
  });
  assert.ok(d.requestId);
  d = edit(d, { prompt: "changed" });
  assert.equal(d.requestId, null, "another request now");
  assert.equal(d.phase, "idle");
  assert.equal(d.cwd, "/w");
  // An edit that changes nothing is no edit.
  const same = { ...d, requestId: "kept" };
  assert.equal(edit(same, { cwd: "/w" }), same);
  // A submission in flight is not edited.
  const flying = beginSubmit(d, minted);
  assert.equal(edit(flying, { cwd: "/elsewhere" }), flying);
});

// ---- a held request, replayed (plan 026 §3.1) ------------------------------

/** The recipe re-read with every provider non-ready: grok gone `needs_setup`
 *  (dimmed), nothing ready. */
const grokDimmed = {
  ...recipe,
  providers: recipe.providers.map((p) => (p.id === "grok" ? { ...p, state: "needs_setup", reason: "no key" } : p)),
};

/** A draft whose outcome is unknown, holding its id: grok, a directory. */
const heldDraft = () =>
  settle(beginSubmit({ ...EMPTY_DRAFT, provider: "grok", cwd: "/w/held" }, minted), {
    ok: false,
    refusal: { code: "outcome_unknown", message: "outcome unknown: lost twice" },
  });

test("a held request is replayed only from an unknown outcome that holds its id", () => {
  const base = { ...EMPTY_DRAFT, provider: "grok", cwd: "/w" };
  assert.equal(replaysHeldRequest({ ...base, phase: "idle", requestId: null }), false, "idle");
  assert.equal(replaysHeldRequest({ ...base, phase: "idle", requestId: "shed-x" }), false, "idle, an id");
  assert.equal(replaysHeldRequest({ ...base, phase: "submitting", requestId: "shed-x" }), false, "submitting");
  assert.equal(replaysHeldRequest({ ...base, phase: "unknown", requestId: "shed-x" }), true, "unknown, held");
  assert.equal(replaysHeldRequest({ ...base, phase: "unknown", requestId: null }), false, "unknown, no id");
  assert.equal(replaysHeldRequest({ ...base, phase: "refused", requestId: null }), false, "refused");
  assert.equal(replaysHeldRequest({ ...base, phase: "created", requestId: null }), false, "created");
  assert.equal(replaysHeldRequest(heldDraft()), true, "settle's unknown");
});

test("CONTROL: an unknown outcome whose provider went dimmed keeps it selected, enabled, with no 'none ready'", () => {
  const d = heldDraft();
  const g = crazeCreateGate(d, grokDimmed, null);
  assert.equal(g.replaying, true);
  assert.equal(g.selected, "grok", "the held provider stays selected, dimmed");
  assert.equal(g.canCreate, true, "Try again works");
  assert.notEqual(g.note, NO_PROVIDER_READY, "readiness does not gate a replay");
  // The same draft NOT held (a fresh idle one) on the same options: nothing
  // to select, Create disabled, and the sheet says why.
  const idle = { ...d, phase: "idle", requestId: null, refusal: null };
  const fresh = crazeCreateGate(idle, grokDimmed, null);
  assert.deepEqual([fresh.selected, fresh.canCreate, fresh.note], [null, false, NO_PROVIDER_READY]);
});

test("an unknown outcome whose provider is no longer listed, with none ready, is still enabled", () => {
  const unlisted = { ...grokDimmed, providers: grokDimmed.providers.filter((p) => p.id !== "grok") };
  const g = crazeCreateGate(heldDraft(), unlisted, null);
  assert.deepEqual([g.selected, g.replaying, g.canCreate], ["grok", true, true]);
});

test("an unknown outcome on a machine with a note is disabled: the retry needs a hub", () => {
  const offline = "craze on m is offline (unreachable) — your form is kept";
  const g = crazeCreateGate(heldDraft(), recipe, offline);
  assert.equal(g.selected, "grok");
  assert.equal(g.canCreate, false);
  assert.equal(g.note, offline, "the machine's note is the one said");
  // A replay still needs a valid directory, and none runs twice at once.
  assert.equal(crazeCreateGate({ ...heldDraft(), cwd: "relative" }, recipe, null).canCreate, false);
  assert.equal(crazeCreateGate(beginSubmit(heldDraft(), minted), recipe, null).canCreate, false);
  assert.equal(crazeCreateGate(heldDraft(), null, null).canCreate, false, "options not read yet");
});

test("while replaying, the note says the outcome is unknown and what an edit does", () => {
  const g = crazeCreateGate(heldDraft(), recipe, null);
  assert.equal(g.note, `${OUTCOME_UNKNOWN_NOTE} ${HELD_EDIT_NOTE}`);
  assert.match(g.note, /check the session list/);
  assert.match(g.note, /Changing the form starts a new request instead\./);
});

test("a refused draft whose provider went dimmed is disabled, and keeps its refusal", () => {
  const refused = settle(beginSubmit({ ...EMPTY_DRAFT, provider: "grok", cwd: "/w" }, minted), {
    ok: false,
    refusal: { code: "failed", message: "Error: KEYCHAIN LOCKED" },
  });
  assert.equal(refused.requestId, null, "a refused draft holds no id");
  const g = crazeCreateGate(refused, grokDimmed, null);
  assert.deepEqual([g.selected, g.replaying, g.canCreate], [null, false, false]);
  assert.equal(g.note, NO_PROVIDER_READY);
  assert.deepEqual(refused.refusal, { code: "failed", message: "Error: KEYCHAIN LOCKED" }, "the refusal is kept");
  assert.equal(refused.provider, "grok", "and so is its provider");
});

test("a refused draft with a ready provider picked is idle again, with no id", () => {
  const nativeReady = {
    ...grokDimmed,
    providers: grokDimmed.providers.map((p) => (p.id === "native" ? { ...p, state: "ready" } : p)),
  };
  const refused = settle(beginSubmit({ ...EMPTY_DRAFT, provider: "grok", cwd: "/w" }, minted), {
    ok: false,
    refusal: { code: "failed", message: "no" },
  });
  const picked = edit(refused, { provider: "native" });
  assert.deepEqual([picked.phase, picked.requestId, picked.refusal], ["idle", null, null]);
  const g = crazeCreateGate(picked, nativeReady, null);
  assert.deepEqual([g.selected, g.canCreate], ["native", true]);
  assert.notEqual(beginSubmit(picked, minted).requestId, null, "the next submit mints a fresh id");
});

test("an outcome_unknown answered to a held submit keeps the id again", () => {
  const d = heldDraft();
  const held = d.requestId;
  assert.equal(replaysHeldRequest(d), true);
  const retried = beginSubmit(d, minted);
  assert.equal(retried.requestId, held, "Try again re-sends the held id");
  const again = settle(retried, {
    ok: false,
    refusal: { code: "outcome_unknown", message: "outcome unknown: the retry did not reach craze" },
  });
  assert.deepEqual([again.phase, again.requestId], ["unknown", held]);
  assert.equal(replaysHeldRequest(again), true, "and the next press replays it too");
});

test("refusals are shown by code", () => {
  assert.deepEqual(refusalView({ code: "bad_request", message: "cwd /nope is no directory" }), {
    where: "cwd",
    text: "cwd /nope is no directory",
  });
  assert.equal(
    refusalView({ code: "bad_request", message: "requestId shed-1 was used already for a create with other params" }).where,
    "general",
  );
  const cause = "acp: agent exited: exit status 1: Error: KEYCHAIN LOCKED / Run unlock and retry.";
  assert.deepEqual(refusalView({ code: "failed", message: cause }), { where: "cause", text: cause }, "verbatim");
  assert.match(refusalView({ code: "unavailable", message: "busy" }).text, /try again/);
});

test("the sheet is offered on a live hub that can create, or a dormant machine", () => {
  assert.equal(crazeCreateOffered({ state: "live", create: true, create_options: true }), true);
  assert.equal(crazeCreateOffered({ state: "dormant", create: false, create_options: false }), true);
  assert.equal(crazeCreateOffered({ state: "live", create: false, create_options: true }), false);
  assert.equal(crazeCreateOffered({ state: "offline", cause: "too_old" }), false);
  assert.equal(crazeCreateOffered({ state: "offline", cause: "unreachable" }), false);
  assert.equal(crazeCreateOffered({ state: "absent", cause: "not_installed" }), false);
  assert.equal(crazeCreateOffered(null), false);
  assert.match(crazeCreateUpdateNote({ state: "live", create: true, create_options: false }), /update craze on this machine/);
  assert.equal(crazeCreateUpdateNote({ state: "live", create: true, create_options: true }), null);
});

test("an open sheet's note follows its machine's craze", () => {
  assert.equal(crazeSheetNote({ state: "live", create: true, create_options: true }, "m"), null);
  assert.equal(crazeSheetNote({ state: "dormant" }, "m"), null);
  assert.match(crazeSheetNote({ state: "offline", cause: "unreachable" }, "m"), /offline/);
  assert.match(crazeSheetNote({ state: "offline", cause: "too_old" }, "m"), /too old/);
  assert.match(crazeSheetNote({ state: "absent", cause: "not_installed" }, "m"), /not installed/);
  assert.match(crazeSheetNote(null, "m"), /no craze/);
});

test("CONTROL: the live setter updates its cell before anything renders", () => {
  const cell = { current: null };
  const published = [];
  const set = liveSetter(cell, (v) => published.push(v));
  set("craze");
  assert.equal(cell.current, "craze", "readable at once, not a render later");
  set((m) => (m === "craze" ? null : m));
  set((m) => (m === null ? "launch" : m));
  assert.equal(cell.current, "launch", "functional updates compose on the live value");
  assert.deepEqual(published, ["craze", null, "launch"]);
});

test("CONTROL: a create resolving decides by the sheet on screen THEN", () => {
  const modal = { current: null };
  const machine = { current: null };
  const setModal = liveSetter(modal, () => {});
  const setMachine = liveSetter(machine, () => {});
  const decide = (m) => closesOntoTranscript(sheetOnScreen(modal.current, machine.current), m);

  setMachine("m");
  setModal("craze"); // the sheet opens; a create starts
  assert.equal(decide("m"), true);
  setModal((cur) => (cur === "craze" ? null : cur)); // dismissed while it runs
  assert.equal(decide("m"), false, "a dismissed sheet's create only appears as a row");
  setModal("craze"); // reopened before it resolves
  assert.equal(decide("m"), true, "the reopened sheet closes onto the transcript");
  setModal("launch"); // another modal took its place
  assert.equal(decide("m"), false);
  setMachine("other");
  setModal("craze"); // another machine's sheet
  assert.equal(decide("m"), false, "another machine's sheet is left alone");
});

test("CONTROL: a group header's machine is found by the ROWS' real origin, not the group key", () => {
  const machines = [
    { origin: "machine:mini3", name: "mini3" },
    { origin: "roost:mini3-dev/p025r-leg3", name: "p025r-leg3" },
  ];

  // A shed group: the group key is `${host}/${shed}` (App.tsx's sessionOrigin),
  // a display string — never the shed's grammar token. Only the rows carry that.
  assert.equal(
    groupMachine("mini3-dev/p025r-leg3", [{ origin: "roost:mini3-dev/p025r-leg3" }], machines),
    machines[1],
  );

  // A machine group: the group key already IS the row's origin, so this keeps
  // working the same way it always did.
  assert.equal(
    groupMachine("machine:mini3", [{ origin: "machine:mini3" }], machines),
    machines[0],
  );

  // No row has an origin (an old payload): falls back to the group key, same
  // as the pre-fix behavior for the case it actually handled.
  assert.equal(
    groupMachine("machine:mini3", [{ origin: null }, {}], machines),
    machines[0],
  );

  // An unknown origin finds nothing.
  assert.equal(groupMachine("machine:ghost", [{ origin: "machine:ghost" }], machines), undefined);
});
