/* shed desktop — the transcript composer's `/` menu panel (plan 029 §3.5,
   §3.6 D13).

   It RENDERS what `lane_slash_filter` answered — the rows in order, the
   highlighted one, the dimmed ones — and decides nothing (`slash.rs`). It sits
   above the composer, styled as the settings sheet's lists
   (`LaneSettings.tsx`), and is capped at 40% of the viewport height and
   scrolls: a catalog has no row cap (P16, P24). Memoised: a streaming turn's
   reads re-render the panel, not a menu of hundreds of rows. */
import { memo, useEffect, useRef } from "react";
import type { SlashFilterRow } from "@/lib/bridge";

export const SlashMenuPanel = memo(function SlashMenuPanel({ rows, highlight, onPick }: {
  rows: SlashFilterRow[];
  /** The index of the row Enter and Tab accept. */
  highlight: number;
  /** A click on a row — the menu's `pick`, the shared rule deciding what it
   *  does (run at once, complete, or refuse while working). */
  onPick: (name: string) => void;
}) {
  const list = useRef<HTMLDivElement | null>(null);
  // The highlighted row stays in view as ↑/↓ move it through a long menu.
  useEffect(() => {
    const el = list.current?.querySelector<HTMLElement>(`[data-slash-index="${highlight}"]`);
    el?.scrollIntoView({ block: "nearest" });
  }, [highlight, rows]);

  return (
    <div data-slash-menu className="flex-none border-t border-shed-border bg-shed-bg-sidebar px-4 pb-1.5 pt-2.5">
      <div
        ref={list}
        role="listbox"
        aria-label="Commands"
        className="flex max-h-[40vh] flex-col overflow-y-auto rounded-[9px] border border-shed-border bg-shed-surface"
      >
        {rows.map((r, i) => {
          const on = i === highlight;
          return (
            <button
              key={r.row.name}
              role="option"
              aria-selected={on}
              data-slash-row={r.row.name}
              data-slash-index={i}
              data-dimmed={String(r.dimmed)}
              // Keep the caret in the composer: the click is a choice, not a
              // move of focus.
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => onPick(r.row.name)}
              title={r.row.description ?? undefined}
              className="hbtn flex items-baseline gap-2 border-t border-shed-border px-3 py-1.5 text-left text-[12.5px] first:border-t-0"
              style={{
                background: on ? "var(--shed-accent-subtle)" : "transparent",
                color: on ? "var(--shed-accent)" : "var(--shed-text)",
                // DIMMED, not hidden (X7/P29) — a deliberate deviation from
                // this app's hidden-never-disabled rule: the while-working
                // gate is TEMPORAL, not a capability the session lacks, and
                // craze's guidance is to list such a row and refuse it
                // client-side; choosing it says why ("runs after this turn").
                opacity: r.dimmed ? 0.5 : 1,
              }}
            >
              <span className="flex-none font-mono font-semibold">/{r.row.name}</span>
              {r.row.arg_hint && (
                <span className="flex-none font-mono text-[11.5px] text-shed-text-muted">{r.row.arg_hint}</span>
              )}
              <span className="min-w-0 flex-1 truncate text-shed-text-secondary">
                {r.row.description ?? (r.row.qualified ? `/${r.row.qualified}` : "")}
              </span>
              {r.dimmed && <span className="flex-none text-[11px] text-shed-text-muted">after this turn</span>}
            </button>
          );
        })}
      </div>
    </div>
  );
});
