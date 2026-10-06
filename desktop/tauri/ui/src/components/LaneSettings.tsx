/* shed desktop — a session's settings sheet (plan 025 §3.10, C11).

   One sheet, rendered generically from the session's streamed settings
   (`LaneView.settings`, what `lane.messages` answers): the model (a list),
   then the current model's options — each a segmented control when it has
   four values or fewer, else a list — then the mode, and a context meter when
   the session reports both its tokens and its window. Opened from the
   transcript header's settings chip, and ONLY on a session whose capabilities
   say `settings` (the panel decides; hidden, never disabled).

   Load-bearing rather than stylistic:

   * **It folds nothing and remembers no value.** Every row is drawn from the
     view the panel last read, so the sheet re-renders from the session's next
     `Settings` — a model change redraws the options as the NEW model offers
     them, and a change made in an attached TUI appears here live. A press shows
     no optimistic value: the row says "applying…" until craze answers, and the
     value it then shows is the session's.
   * **A change's life is per row** (`laneSettings.ts`'s `RowMark`): pending
     until the answer; a refusal inline on its row — an option refused because
     the model moved under it reads "the model changed; try again", and the
     retry is a press, which is a NEW command; an answer lost to a drop is "not
     confirmed" until the next `Settings` says what the session is at. Nothing
     is ever resent. The marks live in the PANEL (`useSettingChanges`), not
     here, so closing the sheet mid-change loses nothing.
   * **It reports what it rendered** (`reportLaneSettings` →
     `lane_settings.dump`), `null` when closed. */
import { useCallback, useEffect, useRef, useState } from "react";
import { Check, X } from "lucide-react";
import {
  changeFor, choiceName, markText, pressSends, rowNamed, settingsChip, settle, sheetRows, shownMark, usageMeter,
  type RowMark, type SessionSettings, type SettingsRow,
} from "@/lib/laneSettings";
import { laneFailure, laneSet, reportLaneSettings, type LaneSettingsReport } from "@/lib/bridge";

/** A row's key in the marks: its kind and id (an option could be named
 *  `model`; its row is not the model row). */
const rowKey = (r: SettingsRow) => `${r.kind}:${r.id}`;

/** **The settings changes of one lane**, owned by its panel: each row's mark,
 *  and the press that sends a change and settles the row from craze's answer.
 *  `settingsSeen` counts the session's `Settings` frames so far — a lost
 *  answer's "not confirmed" lasts until it moves — and `shown` is the settings
 *  the sheet DISPLAYS: an option is sent bound to its model (Amendment A13),
 *  and a lost answer whose value is already on screen is no "not confirmed". */
export function useSettingChanges(
  machine: string,
  kind: string,
  sessionId: string,
  settingsSeen: number,
  shown: SessionSettings,
) {
  const [marks, setMarks] = useState<Record<string, RowMark>>({});
  // The count and the settings as of NOW, for an answer that settles after a
  // render or two.
  const seen = useRef(settingsSeen);
  seen.current = settingsSeen;
  const onScreen = useRef(shown);
  onScreen.current = shown;
  const live = useRef(true);
  useEffect(() => () => {
    live.current = false;
  }, []);
  // The rows with a change in flight, as of NOW: the pending gate must hold
  // for a second press that lands before the first one's render.
  const inflight = useRef(new Set<string>());

  const markOf = useCallback(
    (r: SettingsRow): RowMark | null => shownMark(marks[rowKey(r)], settingsSeen),
    [marks, settingsSeen],
  );

  const press = useCallback(
    (row: SettingsRow, value: string) => {
      const key = rowKey(row);
      if (inflight.current.has(key)) return;
      if (!pressSends(row, value, shownMark(marks[key], seen.current))) return;
      inflight.current.add(key);
      setMarks((m) => ({ ...m, [key]: { state: "pending" } }));
      const put = (next: RowMark | null) => {
        inflight.current.delete(key);
        if (!live.current) return;
        setMarks((m) => {
          const copy = { ...m };
          if (next) copy[key] = next;
          else delete copy[key];
          return copy;
        });
      };
      // Bound to the model shown as the person pressed — not the one the
      // adapter's fold may have moved on to.
      laneSet(machine, kind, sessionId, changeFor(row, value, onScreen.current.model)).then(
        () => put(settle(row.kind, { ok: true }, seen.current)),
        (e: unknown) => {
          const f = laneFailure(e);
          const now = sheetRows(onScreen.current).find((r) => r.kind === row.kind && r.id === row.id);
          put(settle(row.kind, { ok: false, code: f.code, message: f.message }, seen.current, now?.current === value));
        },
      );
    },
    [machine, kind, sessionId, marks],
  );

  return { markOf, press };
}

export function LaneSettingsSheet({
  machine,
  kind,
  sessionId,
  settings,
  markOf,
  press,
  onClose,
}: {
  machine: string;
  kind: string;
  sessionId: string;
  settings: SessionSettings;
  markOf: (r: SettingsRow) => RowMark | null;
  press: (r: SettingsRow, value: string) => void;
  onClose: () => void;
}) {
  const rows = sheetRows(settings);
  const meter = usageMeter(settings.usage);

  // The press door (`ui.pick_lane_setting {row, value}`, test mode only):
  // the press a person makes, through the same gate — a value the row does
  // not render, or a row still pending, takes none. Read through a ref so
  // the listener, attached once, presses on the rows as rendered NOW.
  const latest = useRef({ rows, press });
  latest.current = { rows, press };
  useEffect(() => {
    if (typeof window === "undefined" || !("__TAURI_INTERNALS__" in window)) return;
    let un: (() => void) | null = null;
    let cancelled = false;
    void import("@tauri-apps/api/event").then(async ({ listen }) => {
      const u = await listen<{ row?: unknown; value?: unknown }>("pick-lane-setting", (e) => {
        const { row, value } = e.payload ?? {};
        if (typeof row !== "string" || typeof value !== "string") return;
        const r = rowNamed(latest.current.rows, row);
        if (r) latest.current.press(r, value);
      });
      if (cancelled) u();
      else un = u;
    });
    return () => {
      cancelled = true;
      un?.();
    };
  }, []);

  // ---- the rendered truth, and the report of it ---------------------------
  const report: LaneSettingsReport = {
    machine,
    session_id: sessionId,
    lane_kind: kind,
    chip: settingsChip(settings),
    rows: rows.map((r) => {
      const mark = markOf(r);
      return {
        id: r.id,
        kind: r.kind,
        name: r.name,
        category: r.category,
        control: r.control,
        current: r.current,
        current_name: choiceName(r.values, r.current),
        values: r.values.map((v) => ({ id: v.id, name: v.name, selected: v.id === r.current })),
        state: mark?.state ?? null,
        text: markText(mark),
        enabled: mark?.state !== "pending",
      };
    }),
    usage: meter,
  };
  const reported = JSON.stringify(report);
  const last = useRef(report);
  last.current = report;
  useEffect(() => {
    reportLaneSettings(last.current);
  }, [reported]);
  useEffect(() => () => reportLaneSettings(null), []);

  return (
    <section
      data-lane-settings={sessionId}
      className="flex max-h-[62%] flex-none flex-col border-b border-shed-border bg-shed-bg-sidebar"
      style={{ animation: "shed-in .14s ease" }}
    >
      <div className="flex flex-none items-center gap-2 border-b border-shed-border px-4 py-2">
        <div className="min-w-0 flex-1 text-[13px] font-semibold text-shed-text">Session settings</div>
        <button
          onClick={onClose}
          title="Close settings"
          className="hlink flex h-[26px] w-[26px] flex-none items-center justify-center rounded-lg text-shed-text-muted"
        >
          <X size={15} />
        </button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-2">
        {rows.map((r) => {
          const mark = markOf(r);
          const text = markText(mark);
          const pending = mark?.state === "pending";
          return (
            <div key={rowKey(r)} data-setting-row={r.id} className="py-2">
              <div className="flex items-baseline gap-2">
                <span className="text-[12px] font-semibold uppercase text-shed-text-secondary" style={{ letterSpacing: ".04em" }}>
                  {r.name}
                </span>
                {text && (
                  <span
                    data-setting-state={mark?.state}
                    className="min-w-0 flex-1 break-words font-mono text-[11.5px] leading-snug"
                    style={{
                      color: mark?.state === "refused"
                        ? "var(--shed-danger)"
                        : mark?.state === "not_confirmed"
                          ? "var(--shed-warn-fg)"
                          : "var(--shed-text-muted)",
                    }}
                  >
                    {text}
                  </span>
                )}
              </div>
              {r.control === "segmented" ? (
                <div className="mt-1.5 flex overflow-hidden rounded-[9px] border border-shed-border">
                  {r.values.map((v, i) => {
                    const on = v.id === r.current;
                    return (
                      <button
                        key={v.id}
                        disabled={pending}
                        title={v.description ?? undefined}
                        onClick={() => press(r, v.id)}
                        className="hbtn min-w-0 flex-1 truncate px-2.5 py-1.5 text-[12.5px] font-semibold"
                        style={{
                          background: on ? "var(--shed-accent)" : "var(--shed-surface)",
                          color: on ? "var(--shed-accent-fg)" : "var(--shed-text-secondary)",
                          border: "none",
                          borderLeft: i === 0 ? "none" : "1px solid var(--shed-border)",
                          opacity: pending ? 0.5 : 1,
                        }}
                      >
                        {v.name || v.id}
                      </button>
                    );
                  })}
                </div>
              ) : (
                <div className="mt-1.5 flex max-h-[180px] flex-col overflow-y-auto rounded-[9px] border border-shed-border bg-shed-surface">
                  {r.values.map((v) => {
                    const on = v.id === r.current;
                    return (
                      <button
                        key={v.id}
                        disabled={pending}
                        title={v.description ?? undefined}
                        onClick={() => press(r, v.id)}
                        className="hbtn flex items-center gap-2 border-t border-shed-border px-3 py-1.5 text-left text-[12.5px] first:border-t-0"
                        style={{
                          background: on ? "var(--shed-accent-subtle)" : "transparent",
                          color: on ? "var(--shed-accent)" : "var(--shed-text)",
                          opacity: pending ? 0.5 : 1,
                        }}
                      >
                        <span className="w-3.5 flex-none">{on && <Check size={13} />}</span>
                        <span className="min-w-0 flex-1 truncate">{v.name || v.id}</span>
                      </button>
                    );
                  })}
                </div>
              )}
            </div>
          );
        })}
        {meter && (
          <div data-context-meter className="py-2">
            <div className="text-[12px] font-semibold uppercase text-shed-text-secondary" style={{ letterSpacing: ".04em" }}>
              Context
            </div>
            <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-shed-inset">
              <div className="h-full rounded-full" style={{ width: `${meter.percent}%`, background: "var(--shed-accent)" }} />
            </div>
            <div className="mt-1 font-mono text-[11.5px] text-shed-text-muted">{meter.text}</div>
          </div>
        )}
      </div>
    </section>
  );
}
