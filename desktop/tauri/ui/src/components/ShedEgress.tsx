/* A shed's egress, on its detail (plan 039 PR B): its EFFECTIVE egress (`shed.delivery`'s
   `egress`, which the server resolved: the profiles in force, the server's default
   marked as such for a shed created without `--egress`) and its recent blocks — the
   `deny` decisions the app's egress feed kept for THIS incarnation of the shed
   (`egress.recent {server, shed, created_at, blocks: true}`), re-read on every
   `egress-changed` event for its server. A shed deleted and created again under its
   name is another incarnation: it shows none of the old one's blocks.

   Opened with the rest of the detail (`ui.show_shed_delivery`, or its alias
   `egress.show`) and read back with `shed_egress.dump`: what this section rendered,
   off its own nodes. */
import { useCallback, useEffect, useRef, useState } from "react";
import { ShieldAlert } from "lucide-react";
import {
  fetchEgressBlocks, getShedDelivery, inTauri, reportShedEgress,
  type EgressEvent, type Shed,
} from "@/lib/bridge";
import { blockTarget, egressText, readEgress, type EffectiveEgress } from "@/lib/egress";

/** A timestamp as the section shows it (the raw text when it does not parse). */
function when(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
}

export function ShedEgressDetail({ shed }: { shed: Shed }) {
  const root = useRef<HTMLDivElement>(null);
  // undefined: still reading; null: the shed or its egress status could not be read.
  const [profiles, setProfiles] = useState<EffectiveEgress | null | undefined>(undefined);
  const [blocks, setBlocks] = useState<EgressEvent[] | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const createdAt = shed.created_at ?? "";
  const gen = useRef(0);

  const readBlocks = useCallback(() => {
    if (!createdAt) {
      setBlocks([]);
      return;
    }
    const mine = ++gen.current;
    void fetchEgressBlocks(shed.host, shed.name, createdAt).then(
      (b) => {
        if (mine !== gen.current) return;
        setBlocks(b);
        setError(null);
      },
      (e: unknown) => {
        if (mine !== gen.current) return;
        setBlocks([]);
        setError(typeof e === "string" ? e : e instanceof Error ? e.message : String(e));
      },
    );
  }, [shed.host, shed.name, createdAt]);

  useEffect(() => {
    let live = true;
    void getShedDelivery(shed.host, shed.name).then(
      (doc) => { if (live) setProfiles(readEgress(doc?.egress)); },
      () => { if (live) setProfiles(null); },
    );
    return () => { live = false; };
  }, [shed.host, shed.name, createdAt]);

  useEffect(() => {
    readBlocks();
    if (!inTauri()) return;
    let cancelled = false;
    let un: (() => void) | undefined;
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const u = await listen<{ server?: unknown }>("egress-changed", (e) => {
        if (e.payload?.server === shed.host) readBlocks();
      });
      if (cancelled) u();
      else un = u;
    })();
    return () => {
      cancelled = true;
      un?.();
    };
  }, [readBlocks, shed.host]);

  // Report what this section rendered, read back off its own nodes after the commit.
  const key = JSON.stringify([profiles, blocks, error, createdAt]);
  useEffect(() => {
    const el = root.current;
    if (!el || profiles === undefined || blocks === undefined) return;
    reportShedEgress({
      server: shed.host,
      shed: shed.name,
      created_at: createdAt || null,
      profiles: el.querySelector("[data-egress-profiles]")?.textContent ?? null,
      blocks: Array.from(el.querySelectorAll<HTMLElement>("[data-egress-block]")).map((row) => ({
        target: row.querySelector("[data-col=target]")?.textContent ?? null,
        reason: row.querySelector("[data-col=reason]")?.textContent ?? null,
        at: row.querySelector("[data-col=at]")?.textContent ?? null,
      })),
      note: el.querySelector("[data-egress-note]")?.textContent ?? null,
      error: el.querySelector("[data-egress-error]")?.textContent ?? null,
      rendered: el.textContent ?? "",
    });
  }, [key, shed.host, shed.name]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => reportShedEgress(null), []);

  return (
    <div ref={root} className="border-t border-shed-border px-[18px] pb-4 pt-3.5" data-shed-egress-detail={`${shed.host}/${shed.name}`}>
      <div className="mb-1.5 font-mono text-[10px] font-semibold uppercase tracking-[.09em] text-shed-text-muted">Egress</div>
      <div className="text-[12.5px] text-shed-text-secondary" data-egress-profiles>
        {egressText(profiles)}
      </div>
      <div className="mb-1.5 mt-3 font-mono text-[10px] font-semibold uppercase tracking-[.09em] text-shed-text-muted">Recent blocks</div>
      {error ? (
        <div className="text-[12px] font-medium text-shed-danger" data-egress-error>{error}</div>
      ) : !createdAt ? (
        <div className="text-[12px] text-shed-text-muted" data-egress-note>
          The server did not say when this shed was created, so its blocks cannot be told apart from an earlier shed's.
        </div>
      ) : blocks === undefined ? (
        <div className="text-[12px] text-shed-text-muted">Reading…</div>
      ) : blocks.length === 0 ? (
        <div className="text-[12px] text-shed-text-muted" data-egress-note>
          No blocked connections since this app connected.
        </div>
      ) : (
        <div className="overflow-hidden rounded-[8px] border border-shed-border">
          {blocks.map((b, i) => (
            <div
              key={`${b.ts}-${i}`}
              className="flex items-center gap-3 border-t border-shed-border px-2.5 py-1.5 font-mono text-[11.5px] first:border-t-0"
              data-egress-block
            >
              <ShieldAlert size={13} className="flex-none" style={{ color: "var(--shed-danger)" }} />
              <span className="w-[120px] flex-none text-shed-text-muted" data-col="at">{when(b.ts)}</span>
              <span className="min-w-0 flex-1 truncate text-shed-text" data-col="target">{blockTarget(b)}</span>
              <span className="flex-none text-shed-text-secondary" data-col="reason">{b.reason || b.verdict}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
