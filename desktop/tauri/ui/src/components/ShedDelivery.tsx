/* A shed's delivery (plan 039 §3.10, A7): the badge on its row and the detail area
   under it — the delivered items, the last outcome and its problems, and Deliver
   again (`shed.provision`). Everything shown comes from the shed's record, which
   names items, targets and fingerprints, never a value. An admin looking at another
   user's shed is sent the state alone, and sees that.

   Driven over IPC by `ui.show_shed_delivery` / `ui.close_shed_delivery` (open and
   close a row's detail, like the card's badge does), `ui.press_deliver_again` (test
   mode: presses this detail's own button through its gate) and read back with
   `shed_delivery.dump` (what the open detail rendered, off its own nodes). */
import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronDown, ChevronRight, RotateCw } from "lucide-react";
import { cn } from "@/lib/utils";
import { inTauri, provisionShed, reportShedDelivery, type Pane, type Shed } from "@/lib/bridge";
import {
  deliveryBadge, deliverySummary, provisionResult, stepText,
  type DeliveryRecord, type DeliveryTone,
} from "@/lib/delivery";

/** Which row's detail is open: `{server, shed}`, or null. */
export type DeliveryOpen = { server: string; shed: string } | null;

const TONE: Record<DeliveryTone, [string, string]> = {
  ok: ["var(--shed-ok-bg)", "var(--shed-ok-fg)"],
  attention: ["var(--shed-warn-bg)", "var(--shed-warn-fg)"],
  muted: ["var(--shed-idle-bg)", "var(--shed-idle-fg)"],
  danger: ["var(--shed-err-bg)", "var(--shed-err-fg)"],
};

const STEP_TONE: Record<string, DeliveryTone> = { done: "ok", skipped: "muted", failed: "danger", owed: "attention" };

/** A timestamp as the detail shows it (the raw text when it does not parse). */
function when(ts: string | null): string {
  if (!ts) return "";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
}

/** A Deliver again run: in flight, or how it ended. */
export type Provision = { running: boolean; ok: boolean | null; text: string; warnings: string[]; progress: string[] };

/** What the app shell holds for every shed's delivery: which detail is open (and
 *  its `show-shed-delivery` / `close-shed-delivery` doors), and each shed's Deliver
 *  again run, keyed by server and shed. A run is held here, not by the detail, so
 *  closing the detail or leaving the pane can neither start a second request nor
 *  lose the first one's outcome. */
export type ShedDeliveryState = {
  open: DeliveryOpen;
  setOpen: (o: DeliveryOpen) => void;
  runs: Record<string, Provision>;
  /** Run `shed.provision` for the shed unless one is in flight; `onDone` after. */
  deliver: (server: string, shed: string, onDone: () => void) => void;
};

const runKey = (server: string, shed: string) => JSON.stringify([server, shed]);
export const provisionRunOf = (st: ShedDeliveryState, server: string, shed: string) => st.runs[runKey(server, shed)];

export function useShedDelivery(setPane: (p: Pane) => void): ShedDeliveryState {
  const [open, setOpen] = useState<DeliveryOpen>(null);
  const [runs, setRuns] = useState<Record<string, Provision>>({});
  // In flight, as of the last press (ahead of any render): the one gate.
  const inFlight = useRef(new Set<string>());
  const deliver = useCallback((server: string, shed: string, onDone: () => void) => {
    const key = runKey(server, shed);
    if (inFlight.current.has(key)) return;
    inFlight.current.add(key);
    setRuns((r) => ({ ...r, [key]: { running: true, ok: null, text: "", warnings: [], progress: [] } }));
    void (async () => {
      let out: unknown;
      try {
        out = await provisionShed(server, shed);
      } catch (e) {
        out = { ok: false, error: { message: typeof e === "string" ? e : e instanceof Error ? e.message : String(e) } };
      }
      const r = provisionResult(out);
      const o = (out ?? {}) as { progress?: Array<{ message?: unknown; warning?: unknown }> };
      const progress = (o.progress ?? [])
        .filter((p) => p.warning !== true && typeof p.message === "string")
        .map((p) => p.message as string);
      inFlight.current.delete(key);
      setRuns((x) => ({ ...x, [key]: { running: false, ok: r.ok, text: r.text, warnings: r.warnings, progress } }));
      onDone();
    })();
  }, []);
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    const uns: Array<() => void> = [];
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      uns.push(
        await listen<{ server?: unknown; shed?: unknown }>("show-shed-delivery", (e) => {
          const { server, shed } = e.payload ?? {};
          if (typeof server !== "string" || typeof shed !== "string") return;
          setPane("sheds");
          setOpen({ server, shed });
        }),
      );
      uns.push(await listen("close-shed-delivery", () => setOpen(null)));
      if (cancelled) uns.forEach((u) => u());
    })();
    return () => {
      cancelled = true;
      uns.forEach((u) => u());
    };
  }, [setPane]);
  return { open, setOpen, runs, deliver };
}

/** The row's badge: the state in its tone, the extras beside it, and a chevron — a
 *  press opens or closes the detail. */
export function DeliveryBadge({ rec, open, onToggle }: { rec: DeliveryRecord; open: boolean; onToggle: () => void }) {
  const b = deliveryBadge(rec);
  const [bg, fg] = TONE[b.tone];
  const Chevron = open ? ChevronDown : ChevronRight;
  return (
    <button
      type="button"
      onClick={onToggle}
      title={open ? "Hide delivery" : "Show delivery"}
      aria-expanded={open}
      className="hbtn inline-flex flex-none items-center gap-1.5 rounded-md px-1.5 py-[3px] font-mono text-[10.5px] font-semibold leading-none"
      style={{ background: bg, color: fg }}
      data-shed-delivery-badge
    >
      <span>{b.label}</span>
      {b.extras.map((x) => (
        <span key={x} className="font-medium opacity-80">· {x}</span>
      ))}
      <Chevron size={11} />
    </button>
  );
}

/** A small mono heading inside the detail. */
function Label({ children }: { children: React.ReactNode }) {
  return <div className="mb-1.5 font-mono text-[10px] font-semibold uppercase tracking-[.09em] text-shed-text-muted">{children}</div>;
}

/** The detail area under a row. `prov` is the shed's Deliver again run (held by the
 *  shell), `onDeliver` starts one. */
export function ShedDeliveryDetail({ shed, rec, prov, onDeliver }: {
  shed: Shed; rec: DeliveryRecord; prov: Provision | undefined; onDeliver: () => void;
}) {
  const root = useRef<HTMLDivElement>(null);
  const running = shed.status === "running";
  const canDeliver = running && !prov?.running;

  const deliverAgain = () => {
    if (canDeliver) onDeliver();
  };

  // The harness's press (test mode only): this detail's OWN button, clicked, so the
  // press runs its handler and is refused by its disabled state as a pointer's is.
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let un: (() => void) | undefined;
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const u = await listen<{ server?: unknown; shed?: unknown }>("press-deliver-again", (e) => {
        root.current?.querySelectorAll<HTMLButtonElement>("button[data-deliver-again]").forEach((el) => {
          if (el.dataset.server === e.payload?.server && el.dataset.shed === e.payload?.shed && !el.disabled) el.click();
        });
      });
      if (cancelled) u();
      else un = u;
    })();
    return () => {
      cancelled = true;
      un?.();
    };
  }, []);

  // Report what this detail rendered, read back off its own nodes after the commit.
  const recKey = JSON.stringify([rec, prov, shed.status]);
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    const text = (sel: string) => el.querySelector(sel)?.textContent ?? null;
    const btn = el.querySelector<HTMLButtonElement>("button[data-deliver-again]");
    reportShedDelivery({
      server: shed.host,
      shed: shed.name,
      state: rec.state,
      withheld: rec.withheld,
      summary: text("[data-delivery-summary]"),
      steps: Array.from(el.querySelectorAll<HTMLElement>("[data-delivery-step]")).map((s) => ({
        name: s.dataset.deliveryStep,
        text: s.textContent,
      })),
      last: text("[data-delivery-last]"),
      failure: text("[data-delivery-failure]"),
      problems: Array.from(el.querySelectorAll("[data-delivery-problem]")).map((p) => p.textContent),
      items: Array.from(el.querySelectorAll<HTMLElement>("[data-delivery-item]")).map((row) => {
        const cell: Record<string, string | null> = {};
        row.querySelectorAll<HTMLElement>("[data-col]").forEach((c) => {
          cell[c.dataset.col!] = c.textContent;
        });
        return cell;
      }),
      deliver_again: btn ? { disabled: btn.disabled, text: btn.textContent } : null,
      result: text("[data-provision-result]"),
      warnings: Array.from(el.querySelectorAll("[data-provision-warning]")).map((w) => w.textContent),
      progress: Array.from(el.querySelectorAll("[data-provision-progress]")).map((w) => w.textContent),
      rendered: el.textContent ?? "",
    });
  }, [recKey, shed.host, shed.name]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => reportShedDelivery(null), []);

  const last = rec.last;
  const problems = last?.problems ?? [];
  const expired = new Set(rec.expired);
  return (
    <div ref={root} className="border-t border-shed-border px-[18px] pb-4 pt-3.5" data-shed-delivery-detail={`${shed.host}/${shed.name}`}>
      <div className="flex items-start gap-3">
        <p className="min-w-0 flex-1 text-[13px] leading-snug text-shed-text-secondary" data-delivery-summary>
          {deliverySummary(rec)}
        </p>
        {!rec.withheld && (
          <button
            type="button"
            onClick={deliverAgain}
            disabled={!canDeliver}
            title={running ? "Ask this shed's source for its items again" : "Start the shed to deliver again"}
            className="hbtn inline-flex flex-none items-center gap-1.5 rounded-[8px] border border-shed-border bg-shed-inset px-2.5 py-1 text-[12px] font-medium text-shed-text"
            style={{ opacity: canDeliver ? 1 : 0.5, cursor: canDeliver ? "pointer" : "default" }}
            data-deliver-again
            data-server={shed.host}
            data-shed={shed.name}
          >
            <RotateCw size={12} className={prov?.running ? "animate-spin" : undefined} />
            {prov?.running ? "Delivering…" : "Deliver again"}
          </button>
        )}
      </div>

      {prov && !prov.running && (
        <div className="mt-2.5 rounded-[8px] bg-shed-inset px-3 py-2">
          <div
            className="text-[12.5px] font-medium"
            style={{ color: prov.ok ? "var(--shed-ok-fg)" : "var(--shed-err-fg)" }}
            data-provision-result
          >
            {prov.text}
          </div>
          {prov.warnings.map((w, i) => (
            <div key={`w${i}`} className="mt-1 text-[12px]" style={{ color: "var(--shed-warn-fg)" }} data-provision-warning>
              {w}
            </div>
          ))}
          {prov.progress.length > 0 && (
            <div className="mt-1 font-mono text-[11.5px] leading-relaxed text-shed-text-muted">
              {prov.progress.map((p, i) => (
                <div key={`p${i}`} data-provision-progress>{p}</div>
              ))}
            </div>
          )}
        </div>
      )}

      {rec.steps && (
        <div className="mt-3">
          <Label>Steps</Label>
          <div className="flex flex-wrap gap-1.5">
            {rec.steps.map((s) => {
              const [bg, fg] = TONE[STEP_TONE[s.state] ?? "muted"];
              return (
                <span
                  key={s.name}
                  className="inline-flex items-center gap-1 rounded-md px-2 py-1 font-mono text-[11px] leading-none"
                  style={{ background: bg, color: fg }}
                  data-delivery-step={s.name}
                >
                  <span className="font-semibold">{s.name}</span> {stepText(s)}
                </span>
              );
            })}
          </div>
        </div>
      )}

      {last && (
        <div className="mt-3">
          <Label>Last delivery</Label>
          <div className="text-[12.5px] text-shed-text" data-delivery-last>
            {[when(last.at), last.reason, last.outcome].filter(Boolean).join(" · ")}
            {last.source?.machine_name ? ` · from ${last.source.machine_name}` : ""}
          </div>
          {last.failure && (
            <div className="mt-1 text-[12px] font-medium text-shed-danger" data-delivery-failure>
              Failed: {[last.failure.reason, last.failure.rule, last.failure.item, last.failure.result].filter(Boolean).join(" · ")}
            </div>
          )}
          {problems.length > 0 && (
            <ul className="mt-1.5 flex flex-col gap-1">
              {problems.map((p, i) => (
                <li key={i} className="text-[12px] leading-snug" style={{ color: "var(--shed-warn-fg)" }} data-delivery-problem>
                  <span className="font-mono font-semibold">{p.provider}</span>: {p.message}
                  {p.code && <span className="font-mono text-shed-text-muted"> ({p.code})</span>}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      {rec.items.length > 0 && (
        <div className="mt-3">
          <Label>Items</Label>
          <div className="overflow-hidden rounded-[8px] border border-shed-border">
            <table className="w-full border-collapse text-left font-mono text-[11.5px]">
              <thead>
                <tr className="bg-shed-inset text-shed-text-muted">
                  {["Name", "Provider", "Stage", "Target", "Result", "Written", "Expires"].map((h) => (
                    <th key={h} className="px-2 py-1.5 font-semibold">{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {rec.items.map((it) => (
                  <tr key={it.name} className="border-t border-shed-border text-shed-text" data-delivery-item={it.name}>
                    <td className="px-2 py-1.5" data-col="name">{it.name}</td>
                    <td className="px-2 py-1.5 text-shed-text-secondary" data-col="provider">{it.provider}</td>
                    <td className="px-2 py-1.5 text-shed-text-secondary" data-col="stage">{it.stage}</td>
                    <td className="max-w-[220px] truncate px-2 py-1.5" data-col="target">{it.target}</td>
                    <td
                      className="px-2 py-1.5"
                      style={{ color: it.result === "ok" ? "var(--shed-ok-fg)" : "var(--shed-err-fg)" }}
                      data-col="result"
                    >
                      {it.result}
                    </td>
                    <td className="px-2 py-1.5 text-shed-text-secondary" data-col="written">{when(it.written_at)}</td>
                    <td
                      className={cn("px-2 py-1.5", !expired.has(it.name) && "text-shed-text-secondary")}
                      style={expired.has(it.name) ? { color: "var(--shed-warn-fg)" } : undefined}
                      data-col="expires"
                    >
                      {expired.has(it.name) ? `expired ${when(it.expires_at)}`.trim() : when(it.expires_at)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
