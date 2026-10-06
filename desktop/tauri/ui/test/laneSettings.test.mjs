/* The settings sheet's pure rules (`src/lib/laneSettings.ts`, plan 025 §3.10),
 * on node's own test runner — the `crazeCreate.test.mjs` arrangement.
 *
 * The cases marked CONTROL are the ones plan 025's C11 control list names: the
 * sheet shown when the capabilities say `settings: false`, and a change lost to
 * a drop not marked "not confirmed" until the next `Settings`. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  NOT_CONFIRMED_TEXT,
  NO_SETTINGS,
  PENDING_TEXT,
  SEGMENTED_MAX,
  STALE_MODEL_TEXT,
  changeFor,
  choiceName,
  compactTokens,
  controlFor,
  effortOption,
  fastOn,
  markText,
  pressSends,
  rowNamed,
  settingsChip,
  settingsOffered,
  settle,
  sheetRows,
  shownMark,
  usageMeter,
} from "../dist-test/laneSettings.js";

const offOn = (on) => [{ id: "false", name: "Off" }, { id: "true", name: on }];

/** craze's permodel cursor on grok-4.6, as shed-craze hands it over (ordered). */
const grok46 = {
  model: "grok-4.6",
  models: [
    { id: "grok-4.6", name: "Grok 4.6" },
    { id: "composer-2.5", name: "Composer 2.5" },
    { id: "claude-opus-5", name: "Claude Opus 5" },
    { id: "glm-5.2", name: "GLM 5.2" },
  ],
  mode: "agent",
  modes: [{ id: "agent", name: "Agent" }, { id: "plan", name: "Plan" }, { id: "ask", name: "Ask" }],
  options: [
    { id: "effort", name: "Effort", category: "thought_level", current: "high", values: [
      { id: "low", name: "Low" }, { id: "medium", name: "Medium" }, { id: "high", name: "High" }, { id: "xhigh", name: "Extra High" },
    ] },
    { id: "fast", name: "Fast", category: "model_config", current: "true", values: offOn("Fast") },
  ],
};

/** …and on claude-opus-5: thinking AND effort under thought_level. */
const opus = {
  ...grok46,
  model: "claude-opus-5",
  options: [
    { id: "thinking", name: "Thinking", category: "thought_level", current: "true", values: offOn("On") },
    { id: "effort", name: "Effort", category: "thought_level", current: "max", values: [
      { id: "low", name: "Low" }, { id: "medium", name: "Medium" }, { id: "high", name: "High" },
      { id: "xhigh", name: "Extra High" }, { id: "max", name: "Max" },
    ] },
    { id: "context", name: "Context", category: "model_config", current: "300k", values: [{ id: "300k", name: "300K" }, { id: "1m", name: "1M" }] },
    { id: "fast", name: "Fast", category: "model_config", current: "false", values: offOn("Fast") },
  ],
};

test("≤ 4 values is a segmented control, more is a list", () => {
  assert.equal(SEGMENTED_MAX, 4);
  assert.equal(controlFor(2), "segmented");
  assert.equal(controlFor(4), "segmented");
  assert.equal(controlFor(5), "list");
});

test("the rows: the model (a list), the options in the order given, then the mode", () => {
  const rows = sheetRows(opus);
  assert.deepEqual(rows.map((r) => [r.id, r.kind, r.control]), [
    ["model", "model", "list"],
    ["thinking", "config", "segmented"],
    ["effort", "config", "list"],
    ["context", "config", "segmented"],
    ["fast", "config", "segmented"],
    ["mode", "mode", "segmented"],
  ]);
  assert.equal(rows[0].current, "claude-opus-5");
  assert.equal(rows.at(-1).current, "agent");
  // A model with fewer models than four is still a list: there can be hundreds.
  assert.equal(sheetRows({ ...opus, models: opus.models.slice(0, 2) })[0].control, "list");
});

test("a row with nothing to pick is not drawn", () => {
  const rows = sheetRows({ models: [], modes: [], options: [
    { id: "empty", name: "Empty", category: "x", current: "", values: [] },
  ] });
  assert.deepEqual(rows, []);
});

test("the press door names model, mode, or an option by id", () => {
  const rows = sheetRows(grok46);
  assert.equal(rowNamed(rows, "model").kind, "model");
  assert.equal(rowNamed(rows, "mode").kind, "mode");
  assert.equal(rowNamed(rows, "effort").id, "effort");
  assert.equal(rowNamed(rows, "nope"), undefined);
});

test("what a press sends, row by row", () => {
  const rows = sheetRows(grok46);
  assert.deepEqual(changeFor(rowNamed(rows, "model"), "claude-opus-5"), { kind: "model", id: "claude-opus-5" });
  assert.deepEqual(changeFor(rowNamed(rows, "mode"), "plan"), { kind: "mode", id: "plan" });
  assert.deepEqual(changeFor(rowNamed(rows, "effort"), "low"), { kind: "config", id: "effort", value: "low" });
});

test("CONTROL (A13): an option is bound to the model the sheet DISPLAYED", () => {
  const rows = sheetRows(grok46);
  // The adapter's fold may already have moved on; the press names what was shown.
  assert.deepEqual(changeFor(rowNamed(rows, "effort"), "low", grok46.model),
                   { kind: "config", id: "effort", value: "low", for_model: "grok-4.6" });
  assert.deepEqual(changeFor(rowNamed(rows, "effort"), "low", null),
                   { kind: "config", id: "effort", value: "low" }, "no model shown, no binding");
  assert.deepEqual(changeFor(rowNamed(rows, "model"), "glm-5.2", grok46.model),
                   { kind: "model", id: "glm-5.2" }, "a model change takes none");
  assert.deepEqual(changeFor(rowNamed(rows, "mode"), "ask", grok46.model), { kind: "mode", id: "ask" });
});

test("the chip: model name · effort value · fast, from the current values", () => {
  assert.equal(settingsChip(grok46), "Grok 4.6 · High · fast");
  // fast OFF says nothing; `effort` outranks `thinking`, both thought_level.
  assert.equal(settingsChip(opus), "Claude Opus 5 · Max");
  // grok's own effort is a model_option named reasoning_effort.
  assert.equal(settingsChip({
    model: "grok-4.6", models: [{ id: "grok-4.6", name: "Grok 4.6" }], modes: [],
    options: [{ id: "reasoning_effort", name: "Effort", category: "model_option", current: "high",
                values: [{ id: "low", name: "Low" }, { id: "high", name: "High" }] }],
  }), "Grok 4.6 · High");
  // A model the list does not name reads as its id; nothing at all reads Settings.
  assert.equal(settingsChip({ model: "m-1", models: [], modes: [], options: [] }), "m-1");
  assert.equal(settingsChip({ models: [], modes: [{ id: "a", name: "A" }], options: [] }), "Settings");
});

test("the effort select and the fast toggle are craze's own readings", () => {
  assert.equal(effortOption(opus.options).id, "effort");
  assert.equal(effortOption([opus.options[0]]), undefined, "thinking is no effort");
  assert.equal(fastOn(grok46.options), true);
  assert.equal(fastOn(opus.options), false);
  // On is the value that is not off — by name or spelling, never by position.
  assert.equal(fastOn([{ id: "fast", name: "Fast", category: "model_config", current: "on",
                         values: [{ id: "on", name: "Turbo" }, { id: "off", name: "Off" }] }]), true);
  assert.equal(choiceName(grok46.models, "glm-5.2"), "GLM 5.2");
  assert.equal(choiceName(grok46.models, null), null);
});

test("CONTROL: offered only when the capabilities say settings — the capability alone decides", () => {
  assert.equal(settingsOffered({ settings: true }), true);
  assert.equal(settingsOffered({ settings: false }), false);
  assert.equal(settingsOffered({}), false);
  assert.equal(settingsOffered(null), false);
  // Offered before the first Settings: an empty sheet, a "Settings" chip.
  assert.deepEqual(sheetRows(NO_SETTINGS), []);
  assert.equal(settingsChip(NO_SETTINGS), "Settings");
});

test("a change's life: pending, then cleared, refused inline, or not confirmed", () => {
  assert.equal(settle("config", { ok: true }, 3), null);
  assert.deepEqual(settle("config", { ok: false, code: "not_accepting", message: "the session is not accepting that right now" }, 3),
                   { state: "refused", text: STALE_MODEL_TEXT });
  assert.deepEqual(settle("model", { ok: false, code: "not_accepting", message: "the session is not accepting that right now" }, 3),
                   { state: "refused", text: "the session is not accepting that right now" });
  assert.deepEqual(settle("mode", { ok: false, code: "failed", message: "json-rpc error -32602: Invalid params" }, 3),
                   { state: "refused", text: "json-rpc error -32602: Invalid params" });
  assert.deepEqual(settle("config", { ok: false, code: "outcome_unknown", message: "outcome unknown: …" }, 3),
                   { state: "not_confirmed", since: 3 });
  assert.equal(markText({ state: "pending" }), PENDING_TEXT);
  assert.equal(markText({ state: "not_confirmed", since: 0 }), NOT_CONFIRMED_TEXT);
  assert.equal(markText(null), null);
});

test("CONTROL: a lost answer whose value is already ON SCREEN is not 'not confirmed'", () => {
  const lost = { ok: false, code: "outcome_unknown", message: "outcome unknown: dropped" };
  assert.equal(settle("config", lost, 4, true), null, "the session's Settings already said it took");
  assert.deepEqual(settle("config", lost, 4, false), { state: "not_confirmed", since: 4 });
  assert.deepEqual(settle("config", { ok: false, code: "not_accepting", message: "x" }, 4, true),
                   { state: "refused", text: STALE_MODEL_TEXT }, "a refusal is shown whatever is on screen");
});

test("CONTROL: not confirmed until the NEXT Settings, which replaces it", () => {
  const lost = settle("config", { ok: false, code: "outcome_unknown", message: "outcome unknown" }, 5);
  assert.deepEqual(shownMark(lost, 5), lost, "no Settings since: still not confirmed");
  assert.equal(shownMark(lost, 6), null, "the next Settings states the real value");
  const refused = { state: "refused", text: STALE_MODEL_TEXT };
  assert.deepEqual(shownMark(refused, 99), refused, "a refusal stays until the row is pressed again");
});

test("a press sends only a change: never while pending, never the current value", () => {
  const effort = rowNamed(sheetRows(grok46), "effort");
  assert.equal(pressSends(effort, "low", null), true);
  assert.equal(pressSends(effort, "high", null), false, "already high");
  assert.equal(pressSends(effort, "low", { state: "pending" }), false);
  assert.equal(pressSends(effort, "max", null), false, "a value the row does not offer");
  assert.equal(pressSends(effort, "low", { state: "refused", text: STALE_MODEL_TEXT }), true, "the retry");
});

test("the context meter: tokens and a known window only", () => {
  assert.deepEqual(usageMeter({ context_tokens: 68000, context_window: 200000 }),
                   { tokens: 68000, window: 200000, percent: 34, text: "68k / 200k tokens · 34%" });
  assert.equal(usageMeter({ context_tokens: 5 }), null);
  assert.equal(usageMeter({ context_tokens: 5, context_window: 0 }), null);
  assert.equal(usageMeter(null), null);
  assert.equal(usageMeter({ context_tokens: 300000, context_window: 200000 }).percent, 100);
  assert.equal(compactTokens(1500), "1.5k");
  assert.equal(compactTokens(1_000_000), "1M");
  assert.equal(compactTokens(999), "999");
});

test("CONTROL: the fixtures' labels carry no invisible characters", () => {
  // A zero-width character in a label is invisible in a diff and changes what
  // an equality reads (CodeRabbit, C11). Every name a fixture here offers is
  // plain text.
  const invisible = /[\u200B-\u200D\u2060\uFEFF]/;
  for (const s of [grok46, opus]) {
    const names = [
      ...s.models.map((m) => m.name),
      ...s.modes.map((m) => m.name),
      ...s.options.flatMap((o) => [o.name, ...o.values.map((v) => v.name)]),
    ];
    for (const n of names) assert.equal(invisible.test(n), false, JSON.stringify(n));
  }
});
