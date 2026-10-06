/* The settings sheet's pure rules (plan 025 §3.10) — the header chip's words,
 * the sheet's rows and the control each one is, what a press sends, a change's
 * life on its row, and the context meter.
 *
 * Pure and dependency-free so node's own test runner pins them
 * (`test/laneSettings.test.mjs`), the `crazeCreate.ts` arrangement: the sheet
 * (`LaneSettings.tsx`) and the transcript header (`LanePanel.tsx`) both read
 * these, and a rule that lived inside a component would be the one copy no test
 * can reach.
 *
 * **The data is the adapter's, ordered already.** `shed-craze` computes craze's
 * model order (the current model, then the remembered ones by rank, then the
 * catalog's) and the options' (the current model's own, `thought_level` first,
 * then `model_config`, each in the provider's order) ONCE; this module never
 * re-sorts what it is given. It only decides how each row is drawn. */

/** One selectable value (`shed_core::lane::LaneChoice`): a model, a mode, an
 *  option's value. `id` is what a change sends back — opaque, verbatim. */
export type SettingsChoice = { id: string; name: string; rank?: number | null; description?: string | null };

/** One of the current model's options (`shed_core::lane::LaneSetting`). */
export type SettingsOption = { id: string; name: string; category: string; current: string; values: SettingsChoice[] };

/** How full the context is (`shed_core::lane::LaneUsage`). */
export type SettingsUsage = { context_tokens?: number | null; context_window?: number | null };

/** A session's settings as `lane.messages` carries them
 *  (`shed_core::lane::LaneSettings`). */
export type SessionSettings = {
  model?: string | null;
  models: SettingsChoice[];
  mode?: string | null;
  modes: SettingsChoice[];
  options: SettingsOption[];
  usage?: SettingsUsage | null;
};

/** What a row's press sends (`shed_core::lane::LaneSettingChange`, as
 *  `lane_set`'s `change`). A config change carries `for_model`, the model the
 *  sheet DISPLAYED when the person chose the option (plan 025 Amendment A13). */
export type SettingChange =
  | { kind: "model"; id: string }
  | { kind: "mode"; id: string }
  | { kind: "config"; id: string; value: string; for_model?: string };

/** **Where the settings are offered at all**: on a session whose streamed
 *  capabilities say `settings`, and NOWHERE else — hidden, never disabled
 *  (plan 025 §3.10): no chip, and no sheet even when one is asked for. The
 *  capability alone decides; a session that says `settings` before its first
 *  `Settings` has arrived shows an empty sheet ([`NO_SETTINGS`]), not none. */
export function settingsOffered(capabilities: { settings?: boolean | null } | null | undefined): boolean {
  return capabilities?.settings === true;
}

/** Settings with nothing in them — what a sheet draws before the first
 *  `Settings` of a session that offers them. */
export const NO_SETTINGS: SessionSettings = { models: [], modes: [], options: [] };

/** The most values a row shows side by side; a longer list is a list. */
export const SEGMENTED_MAX = 4;

/** `segmented` (every value one press away, side by side) or `list`. */
export type SettingControl = "segmented" | "list";

/** **≤ 4 values → a segmented control, more → a list** (plan 025 §3.10). */
export function controlFor(values: number): SettingControl {
  return values <= SEGMENTED_MAX ? "segmented" : "list";
}

/** One row of the sheet. `id` is the row's name in the sheet's doors and dump
 *  — `model`, `mode`, or the option's own id — and `kind` what a press on it
 *  changes. */
export type SettingsRow = {
  id: string;
  kind: "model" | "mode" | "config";
  name: string;
  control: SettingControl;
  /** The current value's id, as the session last said (`null` when it does
   *  not say). */
  current: string | null;
  values: SettingsChoice[];
  /** The option's category (`thought_level`, `model_config`, …); `null` for
   *  the model and mode rows. */
  category: string | null;
};

/** **The sheet's rows, in the order plan 025 §3.10 pins**: the model (always
 *  a list — there can be hundreds), then the current model's options in the
 *  order given, then the mode, when there are modes. A row with nothing to
 *  pick is not drawn (hidden, never disabled). */
export function sheetRows(s: SessionSettings): SettingsRow[] {
  const rows: SettingsRow[] = [];
  if (s.models.length > 0) {
    rows.push({ id: "model", kind: "model", name: "Model", control: "list",
                current: s.model ?? null, values: s.models, category: null });
  }
  for (const o of s.options) {
    if (o.values.length === 0) continue;
    rows.push({ id: o.id, kind: "config", name: o.name || o.id, control: controlFor(o.values.length),
                current: o.current, values: o.values, category: o.category });
  }
  if (s.modes.length > 0) {
    rows.push({ id: "mode", kind: "mode", name: "Mode", control: controlFor(s.modes.length),
                current: s.mode ?? null, values: s.modes, category: null });
  }
  return rows;
}

/** The row the sheet's press door names: `model` and `mode` are those rows,
 *  anything else an option by its id. */
export function rowNamed(rows: SettingsRow[], name: string): SettingsRow | undefined {
  if (name === "model" || name === "mode") return rows.find((r) => r.kind === name);
  return rows.find((r) => r.kind === "config" && r.id === name);
}

/** What pressing `value` on `row` sends. An option is bound to
 *  `displayedModel` — the model the sheet SHOWED when it was pressed (plan 025
 *  Amendment A13): the session's adapter may already have seen a move the
 *  sheet has not, and the change must be refused (`stale_model`) rather than
 *  applied to a model nobody chose it for. No model shown, no binding. */
export function changeFor(row: SettingsRow, value: string, displayedModel?: string | null): SettingChange {
  switch (row.kind) {
    case "model":
      return { kind: "model", id: value };
    case "mode":
      return { kind: "mode", id: value };
    default:
      return displayedModel
        ? { kind: "config", id: row.id, value, for_model: displayedModel }
        : { kind: "config", id: row.id, value };
  }
}

// ---- the chip -------------------------------------------------------------

/** Words that make an option an effort select (craze's own reading,
 *  `agent.isEffortSelect`): the id or the name says effort or reasoning. */
function isEffort(o: SettingsOption): boolean {
  const id = o.id.toLowerCase();
  const name = o.name.toLowerCase();
  return o.values.length > 0
    && (id.includes("effort") || id.includes("reasoning") || name.includes("effort") || name.includes("reasoning"));
}

/** craze's rank among effort selects (`agent.effortRank`): the exact id
 *  `effort` first — cursor files `thinking` under `thought_level` beside it, so
 *  a category is no evidence — then `model_option`, then `thought_level`. */
function effortRank(o: SettingsOption): number {
  if (o.id.toLowerCase() === "effort") return 0;
  if (o.category === "model_option") return 1;
  if (o.category === "thought_level") return 2;
  return 3;
}

/** The session's effort select, or `undefined` when it offers none. */
export function effortOption(options: SettingsOption[]): SettingsOption | undefined {
  let best: SettingsOption | undefined;
  for (const o of options) {
    if (isEffort(o) && (!best || effortRank(o) < effortRank(best))) best = o;
  }
  return best;
}

/** Whether the session's fast toggle exists AND is on (craze's `FastOn`): a
 *  `model_config` option named fast, whose current value is its ON one — the
 *  value that is not the off one, read off the value's own name or spelling
 *  (`Off` / `false`), never its position. */
export function fastOn(options: SettingsOption[]): boolean {
  const opt = options.find((o) => o.category === "model_config"
    && (o.id === "fast" || o.name.toLowerCase().includes("fast")));
  if (!opt) return false;
  const isOff = (v: SettingsChoice) => v.name.trim().toLowerCase() === "off" || v.id.trim().toLowerCase() === "false";
  const on = opt.values.find((v) => !isOff(v));
  return !!on && opt.current === on.id;
}

/** What a value is called: its own name, else its id. */
export function choiceName(values: SettingsChoice[], id: string | null | undefined): string | null {
  if (!id) return null;
  return values.find((v) => v.id === id)?.name || id;
}

/** **The transcript header's chip** (plan 025 §3.10): `<model name> · <effort
 *  value> · fast`, from the CURRENT values, each part only when the session
 *  has it — and "fast" only when the toggle is ON (off is the quiet default
 *  and says nothing, craze's own status-row rule). A session whose current
 *  values say none of the three reads "Settings". */
export function settingsChip(s: SessionSettings): string {
  const parts: string[] = [];
  const model = choiceName(s.models, s.model);
  if (model) parts.push(model);
  const effort = effortOption(s.options);
  const effortName = effort ? choiceName(effort.values, effort.current) : null;
  if (effortName) parts.push(effortName);
  if (fastOn(s.options)) parts.push("fast");
  return parts.length > 0 ? parts.join(" · ") : "Settings";
}

// ---- a change's life on its row -------------------------------------------

/** A row's mark (plan 025 §3.10's command lifecycle):
 *
 *  - `pending` — the change is sent and craze has not answered; the row takes
 *    no other press until it does.
 *  - `refused` — craze said no; shown INLINE on the row, until the next press
 *    on it (which is a NEW command, never a resend).
 *  - `not_confirmed` — the answer was lost with the connection: the change may
 *    have run, and it is never resent. Shown until the next `Settings` the
 *    session sends (`since` is how many had arrived when the answer was
 *    lost), which states the real value and replaces it. */
export type RowMark =
  | { state: "pending" }
  | { state: "refused"; text: string }
  | { state: "not_confirmed"; since: number };

/** The text a `stale_model` refusal shows: craze's `stale_model` — the session
 *  left the model an option was chosen for — reaches a client as the table's
 *  `not_accepting`, which is what an option row reads it as. */
export const STALE_MODEL_TEXT = "the model changed; try again";

/** What a "not confirmed" row says. */
export const NOT_CONFIRMED_TEXT = "not confirmed — the connection dropped before craze answered";

/** What a pending row says. */
export const PENDING_TEXT = "applying…";

/** **A row's mark once craze has answered** — `null` when the change took (the
 *  row re-renders from the next `Settings`, which craze sends ahead of its
 *  answer), `not_confirmed` when the answer was lost (`outcome_unknown`), and
 *  otherwise the refusal, inline: on an option row `not_accepting` is the
 *  model having moved under the choice.
 *
 *  `confirmedOnScreen` — the row already SHOWS the value that was asked for —
 *  makes a lost answer nothing to mark: the session's own `Settings` said the
 *  change took before the loss was known, and "not confirmed" would contradict
 *  the value on screen (and could outlive a lane that never reconnects). */
export function settle(
  kind: SettingsRow["kind"],
  outcome: { ok: true } | { ok: false; code: string; message: string },
  settingsSeen: number,
  confirmedOnScreen = false,
): RowMark | null {
  if (outcome.ok) return null;
  if (outcome.code === "outcome_unknown") {
    return confirmedOnScreen ? null : { state: "not_confirmed", since: settingsSeen };
  }
  if (outcome.code === "not_accepting" && kind === "config") return { state: "refused", text: STALE_MODEL_TEXT };
  return { state: "refused", text: outcome.message || outcome.code };
}

/** The mark a row SHOWS now: a `not_confirmed` one only until a `Settings`
 *  has arrived since the answer was lost. */
export function shownMark(mark: RowMark | null | undefined, settingsSeen: number): RowMark | null {
  if (!mark) return null;
  if (mark.state === "not_confirmed" && settingsSeen > mark.since) return null;
  return mark;
}

/** The line a mark shows under its row. */
export function markText(mark: RowMark | null): string | null {
  if (!mark) return null;
  switch (mark.state) {
    case "pending":
      return PENDING_TEXT;
    case "refused":
      return mark.text;
    default:
      return NOT_CONFIRMED_TEXT;
  }
}

/** Whether a press on a row is a change to send: not while its last change is
 *  pending, and not on the value the row already shows (nothing changes). */
export function pressSends(row: SettingsRow, value: string, mark: RowMark | null): boolean {
  if (mark?.state === "pending") return false;
  if (!row.values.some((v) => v.id === value)) return false;
  return value !== row.current;
}

// ---- the context meter ----------------------------------------------------

/** `68000` → `68k`, `1000000` → `1M`, `1500` → `1.5k`. */
export function compactTokens(n: number): string {
  const fmt = (v: number, unit: string) => `${Number.isInteger(v) ? v : v.toFixed(1).replace(/\.0$/, "")}${unit}`;
  if (n >= 1_000_000) return fmt(Math.round(n / 100_000) / 10, "M");
  if (n >= 1000) return fmt(Math.round(n / 100) / 10, "k");
  return String(n);
}

/** **The context meter** (plan 025 §3.10): only when the usage has BOTH the
 *  tokens and a known window (a native session's); `percent` capped at 100. */
export function usageMeter(u: SettingsUsage | null | undefined):
  { tokens: number; window: number; percent: number; text: string } | null {
  const tokens = u?.context_tokens;
  const window = u?.context_window;
  if (tokens == null || window == null || window <= 0) return null;
  const percent = Math.min(100, Math.round((tokens / window) * 100));
  return { tokens, window, percent, text: `${compactTokens(tokens)} / ${compactTokens(window)} tokens · ${percent}%` };
}
