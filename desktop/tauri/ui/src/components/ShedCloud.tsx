/* A cloud shed on the Sheds pane (plan 041 B2): its detail (image, created, run time,
   the lease's end and cap, and Extend with the presets and a custom hours field), the
   usage line under a remote server's heading, and the confirmation Delete asks for.
   Every word and every offer comes from the row's `cloud_row` (`shed-app::cloud`); this
   file only lays them out.

   Driven over IPC by the test-mode doors `ui.press_extend` (a preset),
   `ui.fill_extend_hours` + `ui.press_extend_custom` (the custom field) and
   `ui.press_delete_confirm`, each acting on the control this file rendered, and read
   back through `dashboard.dump`'s `cloud` (what each part rendered, off its own
   nodes). */
import { useCallback, useEffect, useRef, useState } from "react";
import { Clock, Trash2 } from "lucide-react";
import { Scrim, DialogShell, dialogBtnSecondary, useEscClose } from "@/components/dialog";
import { extendShed, fetchCloudUsage, inTauri, reportShedCloud, type Shed } from "@/lib/bridge";
import {
  customHours,
  extendKey,
  failureWords,
  presetLabel,
  pruneRuns,
  readCloudRow,
  USAGE_FOLLOW_UP_MS,
  usageStep,
  type CloudRow,
  type UsageAsk,
} from "@/lib/cloud";

/** Click `el` (or the button inside it) unless it is disabled, as a pointer would. */
function press(el: Element | null | undefined) {
  const b = el instanceof HTMLButtonElement ? el : el?.querySelector<HTMLButtonElement>("button");
  if (b && !b.disabled) b.click();
}

/** Set a text input's value as typing does: its own setter, then its own event. */
function typeInto(el: HTMLInputElement | null | undefined, value: string) {
  if (!el || el.disabled) return;
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set?.call(el, value);
  el.dispatchEvent(new Event("input", { bubbles: true }));
}

/** One fact in the detail: a mono label and its value. */
function Fact({ label, fact, children }: { label: string; fact: string; children: React.ReactNode }) {
  return (
    <div className="flex min-w-0 items-baseline gap-2">
      <span className="w-[78px] flex-none font-mono text-[10.5px] font-semibold uppercase tracking-[.08em] text-shed-text-muted">{label}</span>
      <span className="min-w-0 break-words font-mono text-[12.5px] text-shed-text" data-cloud-fact={fact}>{children}</span>
    </div>
  );
}

/** How an extend ended; `unknown` when no answer said whether it was applied (the
 *  core then holds the shed until a listing settles it). */
type ExtendResult = { ok: boolean; words: string; unknown?: boolean };

/** One shed's extend: whether one is in flight, and how the last one ended (`seq`
 *  counts the results, so a detail can tell a new one from the one it has seen). */
export type ExtendRun = {
  busy: boolean;
  result: ExtendResult | null;
  seq: number;
  /** Listings read since an unknown result: one that no longer shows the extend
   *  held means it was settled. Held here, so a detail opened later knows too. */
  listedAfter?: number;
};

/** This incarnation of a shed (its creation time): a shed created again under its
 *  name is another shed, and shares none of the old one's runs. */
export function shedIncarnation(s: Shed): string {
  return s.created_at ?? "";
}

const runKey = (s: Shed) => extendKey(s.host, s.name, shedIncarnation(s));

/** Every cloud shed's extend, held by the app shell, not by the detail: closing and
 *  reopening a detail (or leaving the pane) neither loses an extend's outcome nor
 *  lets a second extend start while one is in flight. */
export type CloudExtends = {
  runOf: (s: Shed) => ExtendRun | undefined;
  /** Extend `s` by `hours` unless one is in flight for it; `onDone` once it has
   *  settled, either way (a refusal or an unknown outcome changes the row too). */
  extend: (s: Shed, hours: number, onDone: () => void) => void;
  /** Forget the finished runs of shed incarnations no longer in `sheds` (the rows
   *  just listed); a run still in flight is kept. */
  prune: (sheds: Shed[]) => void;
};

export function useCloudExtends(): CloudExtends {
  const [runs, setRuns] = useState<Record<string, ExtendRun>>({});
  // In flight, as of the last press (ahead of any render): the one gate.
  const inFlight = useRef(new Set<string>());
  const extend = useCallback((s: Shed, hours: number, onDone: () => void) => {
    const key = runKey(s);
    if (inFlight.current.has(key)) return;
    inFlight.current.add(key);
    setRuns((r) => ({ ...r, [key]: { busy: true, result: null, seq: r[key]?.seq ?? 0 } }));
    const settle = (result: ExtendResult) => {
      inFlight.current.delete(key);
      setRuns((r) => ({ ...r, [key]: { busy: false, result, seq: (r[key]?.seq ?? 0) + 1 } }));
    };
    extendShed(s.host, s.name, hours).then(
      (answer) => {
        const now = readCloudRow(answer?.cloud_row);
        const end = now?.expires_words ? `; deleted at ${now.expires_words}` : "";
        settle({ ok: true, words: `Extended by ${hours} h${end}` });
        onDone();
      },
      // An extend whose outcome is not known is kept in the core until a listing
      // settles it; the row says so (`extend_unresolved_words`) once the list is
      // read again.
      (e: unknown) => {
        const unknown = !!e && typeof e === "object" && (e as { code?: unknown }).code === "extend_unknown";
        settle({ ok: false, words: failureWords(e), unknown });
        onDone();
      },
    );
  }, []);
  const runOf = useCallback((s: Shed) => runs[runKey(s)], [runs]);
  const prune = useCallback((sheds: Shed[]) => {
    const live = new Set(sheds.map(runKey));
    setRuns((r) => {
      const kept = pruneRuns(r, live, inFlight.current);
      // A listing read after an unknown outcome: counted, so its words go once the
      // rows no longer show the extend held (not against the rows it ended on).
      let counted = kept;
      for (const [k, run] of Object.entries(kept)) {
        if (!run.busy && run.result?.unknown) {
          if (counted === kept) counted = { ...kept };
          counted[k] = { ...run, listedAfter: (run.listedAfter ?? 0) + 1 };
        }
      }
      return counted;
    });
  }, []);
  return { runOf, extend, prune };
}

/** The detail area of a cloud row. `run` is the shed's extend (held by the shell),
 *  `onExtend` starts one. */
export function ShedCloudDetail({ shed, row, run, onExtend }: {
  shed: Shed; row: CloudRow; run: ExtendRun | undefined; onExtend: (hours: number) => void;
}) {
  const root = useRef<HTMLDivElement>(null);
  const [custom, setCustom] = useState("");
  // An extend in flight, or an earlier one whose outcome is not known yet (the core
  // refuses every extend of the shed until a listing settles it): nothing to press.
  const busy = (run?.busy ?? false) || !!row.extend_unresolved_words;
  // An unknown outcome's words stand until a listing read after it no longer shows the
  // extend held: then it was settled, and the words would contradict the enabled
  // controls. Not against the rows the extend ended on (read before it was recorded).
  const unknownSettled =
    !!run?.result?.unknown && (run.listedAfter ?? 0) > 0 && !row.extend_unresolved_words;
  const result = unknownSettled ? null : (run?.result ?? null);
  const customValue = customHours(custom, row.extend_max_h);
  // A new successful extend clears the custom field; one this detail mounted after
  // does not.
  const seenSeq = useRef(run?.seq ?? 0);
  useEffect(() => {
    const seq = run?.seq ?? 0;
    if (seq !== seenSeq.current) {
      seenSeq.current = seq;
      if (run?.result?.ok) setCustom("");
    }
  }, [run?.seq, run?.result?.ok]);

  const extend = (hours: number) => {
    if (busy || !row.actions.extend) return;
    onExtend(hours);
  };

  // The harness's doors (test mode only): this detail's own controls, pressed or
  // typed into, so a hidden or disabled one takes nothing, as with a pointer.
  // `doors` is reported once every listener is attached (a press before then would
  // reach no one).
  const [doors, setDoors] = useState(false);
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    const uns: Array<() => void> = [];
    const mine = (p: { server?: unknown; shed?: unknown } | undefined) =>
      p?.server === shed.host && p?.shed === shed.name ? root.current : null;
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      uns.push(await listen<{ server?: unknown; shed?: unknown; hours?: unknown }>("press-extend", (e) => {
        const el = mine(e.payload);
        press(Array.from(el?.querySelectorAll<HTMLElement>("button[data-extend-preset]") ?? [])
          .find((b) => b.dataset.extendPreset === String(e.payload?.hours)));
      }));
      uns.push(await listen<{ server?: unknown; shed?: unknown; value?: unknown }>("fill-extend-hours", (e) => {
        const v = e.payload?.value;
        if (typeof v !== "string") return;
        typeInto(mine(e.payload)?.querySelector<HTMLInputElement>("input[data-extend-custom]"), v);
      }));
      uns.push(await listen<{ server?: unknown; shed?: unknown }>("press-extend-custom", (e) => {
        press(mine(e.payload)?.querySelector("button[data-extend-custom-press]"));
      }));
      if (cancelled) uns.forEach((u) => u());
      else setDoors(true);
    })();
    return () => {
      cancelled = true;
      uns.forEach((u) => u());
    };
  }, [shed.host, shed.name]);

  // Report what this detail rendered, read back off its own nodes after the commit.
  const key = JSON.stringify([row, custom, busy, run?.busy, result, shed.status, doors]);
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    const fact = (f: string) =>
      Array.from(el.querySelectorAll<HTMLElement>("[data-cloud-fact]")).find((x) => x.dataset.cloudFact === f)?.textContent ?? null;
    const input = el.querySelector<HTMLInputElement>("input[data-extend-custom]");
    const customBtn = el.querySelector<HTMLButtonElement>("button[data-extend-custom-press]");
    const res = el.querySelector<HTMLElement>("[data-extend-result]");
    reportShedCloud("shed_cloud", {
      server: shed.host,
      shed: shed.name,
      facts: {
        image: fact("image"),
        created: fact("created"),
        run: fact("run"),
        lease_end: fact("lease_end"),
        cap: fact("cap"),
      },
      extend: el.querySelector("[data-cloud-extend]")
        ? {
            presets: Array.from(el.querySelectorAll<HTMLButtonElement>("button[data-extend-preset]")).map((b) => ({
              hours: Number(b.dataset.extendPreset),
              label: b.textContent,
              disabled: b.disabled,
            })),
            custom: input ? { value: input.value, max: Number(input.dataset.max), disabled: !!customBtn?.disabled } : null,
            busy: !!el.querySelector("[data-extend-busy]"),
          }
        : null,
      result: res ? { ok: res.dataset.extendResult === "ok", words: res.textContent } : null,
      unresolved: el.querySelector("[data-extend-unresolved]")?.textContent ?? null,
      rendered: el.textContent ?? "",
      doors,
    });
  }, [key, shed.host, shed.name]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => reportShedCloud("shed_cloud", null), []);

  const leaseEnd = row.expires_words ? `${row.expires_words} (${row.lease_words})` : row.lease_words;
  return (
    <div ref={root} className="border-t border-shed-border px-[18px] pb-4 pt-3.5" data-shed-cloud-detail={`${shed.host}/${shed.name}`}>
      <div className="mb-2 font-mono text-[10px] font-semibold uppercase tracking-[.09em] text-shed-text-muted">Cloud machine</div>
      <div className="grid grid-cols-1 gap-x-6 gap-y-1.5 sm:grid-cols-2">
        <Fact label="Image" fact="image">{row.image || "—"}</Fact>
        <Fact label="Created" fact="created">{row.created_words ?? "—"}</Fact>
        <Fact label="Run time" fact="run">{row.run_words ?? "—"}</Fact>
        <Fact label="Lease end" fact="lease_end">{leaseEnd}</Fact>
        <Fact label="Cap" fact="cap">{row.cap_words ?? "—"}</Fact>
      </div>
      {row.actions.extend && (
        <div className="mt-3.5 flex flex-wrap items-center gap-2" data-cloud-extend>
          <span className="mr-1 inline-flex items-center gap-1.5 text-[12.5px] font-medium text-shed-text-secondary">
            <Clock size={13} className="text-shed-text-muted" /> Extend
          </span>
          {row.extend_presets_h.map((h) => (
            <button
              key={h}
              type="button"
              onClick={() => extend(h)}
              disabled={busy}
              className="hbtn rounded-[8px] border border-shed-border bg-shed-inset px-2.5 py-1 font-mono text-[12px] font-medium text-shed-text"
              style={{ opacity: busy ? 0.5 : 1 }}
              data-extend-preset={h}
            >
              {presetLabel(h)}
            </button>
          ))}
          <span className="ml-1 inline-flex items-center gap-1.5">
            <input
              type="text"
              inputMode="numeric"
              value={custom}
              onChange={(e) => setCustom(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && customValue !== null) extend(customValue);
              }}
              placeholder={`1–${row.extend_max_h}`}
              aria-label={`Hours, 1 to ${row.extend_max_h}`}
              title={`Hours, 1 to ${row.extend_max_h}`}
              disabled={busy}
              className="w-[72px] rounded-[8px] border border-shed-border bg-shed-inset px-2 py-1 font-mono text-[12px] text-shed-text outline-none focus:border-shed-accent"
              data-extend-custom
              data-max={row.extend_max_h}
            />
            <span className="text-[12px] text-shed-text-muted">h</span>
            <button
              type="button"
              onClick={() => customValue !== null && extend(customValue)}
              disabled={busy || customValue === null}
              className="hbtn rounded-[8px] px-2.5 py-1 text-[12px] font-semibold"
              style={{
                background: "var(--shed-accent)",
                color: "var(--shed-accent-fg)",
                opacity: busy || customValue === null ? 0.5 : 1,
              }}
              data-extend-custom-press
            >
              Extend
            </button>
          </span>
          {run?.busy && <span className="text-[12px] text-shed-text-muted" data-extend-busy>Extending…</span>}
        </div>
      )}
      {row.extend_unresolved_words && (
        <div className="mt-2.5 text-[12.5px] font-medium" style={{ color: "var(--shed-warn-fg)" }} data-extend-unresolved>
          {row.extend_unresolved_words}
        </div>
      )}
      {result && (
        <div
          className="mt-2.5 rounded-[8px] px-3 py-2 text-[12.5px] font-medium"
          style={{
            background: result.ok ? "var(--shed-ok-bg)" : "var(--shed-err-bg)",
            color: result.ok ? "var(--shed-ok-fg)" : "var(--shed-err-fg)",
          }}
          data-extend-result={result.ok ? "ok" : "refused"}
        >
          {result.words}
        </div>
      )}
    </div>
  );
}

/** The usage line of each server whose rows are cloud sheds: `host -> words`. Read
 *  at once when that server's cloud rows change (a create, a delete, a start or a
 *  stop) and once more [`USAGE_FOLLOW_UP_MS`] later, since shed-server caches usage
 *  [`USAGE_KEEP_MS`] and the first read may be its cached count; otherwise at most
 *  every [`USAGE_KEEP_MS`] as its rows are listed again. A server whose usage cannot
 *  be read has no line. Every host-keyed table here is a `Map`: a server named like
 *  an Object property (`constructor`, `__proto__`) is a key like any other. */
export function useCloudUsage(sheds: Shed[]): ReadonlyMap<string, string> {
  const [usage, setUsage] = useState<ReadonlyMap<string, string>>(() => new Map());
  // host -> its cloud rows when its usage was last asked for, when, and that ask's
  // number. Numbers come from one counter that never repeats, so an ask made before a
  // server's rows went away and came back can never pass for a later one.
  const asked = useRef(new Map<string, UsageAsk>());
  const asks = useRef(0);
  // host -> its pending follow-up read.
  const followUps = useRef(new Map<string, number>());
  const drop = useCallback(
    (host: string) =>
      setUsage((u) => {
        if (!u.has(host)) return u;
        const next = new Map(u);
        next.delete(host);
        return next;
      }),
    [],
  );
  const cancelFollowUp = useCallback((host: string) => {
    const t = followUps.current.get(host);
    if (t !== undefined) {
      window.clearTimeout(t);
      followUps.current.delete(host);
    }
  }, []);
  const ask = useCallback(
    (host: string, key: string) => {
      const gen = ++asks.current;
      asked.current.set(host, { key, at: Date.now(), gen });
      const current = () => asked.current.get(host)?.gen === gen;
      void fetchCloudUsage(host).then(
        (a) => {
          if (!current()) return;
          if (typeof a?.words === "string" && a.words) {
            const words = a.words;
            setUsage((u) => new Map(u).set(host, words));
          } else drop(host);
        },
        () => {
          if (current()) drop(host);
        },
      );
    },
    [drop],
  );
  useEffect(() => {
    const byHost = new Map<string, string[]>();
    for (const s of sheds) {
      if (!readCloudRow(s.cloud_row)) continue;
      const rows = byHost.get(s.host);
      if (rows) rows.push(`${s.name}:${s.status}`);
      else byHost.set(s.host, [`${s.name}:${s.status}`]);
    }
    for (const host of [...asked.current.keys()]) {
      if (!byHost.has(host)) {
        asked.current.delete(host);
        cancelFollowUp(host);
        drop(host);
      }
    }
    const now = Date.now();
    for (const [host, rows] of byHost) {
      const key = JSON.stringify(rows);
      const step = usageStep(asked.current.get(host), key, now);
      if (!step.ask) continue;
      ask(host, key);
      if (!step.followUp) continue;
      cancelFollowUp(host);
      followUps.current.set(
        host,
        window.setTimeout(() => {
          followUps.current.delete(host);
          // Rows changed again since: that change has its own reads.
          if (asked.current.get(host)?.key === key) ask(host, key);
        }, USAGE_FOLLOW_UP_MS),
      );
    }
  }, [sheds, ask, drop, cancelFollowUp]);
  useEffect(
    () => () => {
      for (const t of followUps.current.values()) window.clearTimeout(t);
      followUps.current.clear();
    },
    [],
  );
  return usage;
}

/** The usage line under a remote server's heading. */
export function CloudUsageLine({ host, words }: { host: string; words: string }) {
  return (
    <div className="-mt-1.5 mb-3 pl-0.5 font-mono text-[11.5px] text-shed-text-secondary" data-cloud-usage={host}>
      {words}
    </div>
  );
}

/** Report the usage lines the pane rendered (`{server, words}` in order), read back
 *  off their nodes after the commit; null once the pane is gone. */
export function useReportCloudUsage(usage: ReadonlyMap<string, string>) {
  const key = JSON.stringify([...usage]);
  useEffect(() => {
    reportShedCloud(
      "cloud_usage",
      Array.from(document.querySelectorAll<HTMLElement>("[data-cloud-usage]")).map((el) => ({
        server: el.dataset.cloudUsage ?? "",
        words: el.textContent ?? "",
      })),
    );
  }, [key]);
  useEffect(() => () => reportShedCloud("cloud_usage", null), []);
}

/** The words Delete asks with on a cloud row. */
export const CLOUD_DELETE_WORDS = "Deletes the machine and its disk.";

/** The confirmation Delete asks for on a cloud row. */
/** The words a failed Delete shows in its question: the error's own text (the
 *  app's, such as `shed-server returned HTTP 503`; never a cloud server's message). */
export function deleteFailureWords(e: unknown): string {
  const why = typeof e === "string" && e ? e : e instanceof Error && e.message ? e.message : "no answer";
  return `The delete failed: ${why}`;
}

/** A cloud row's Delete question. `busy` while its delete runs (Delete is then
 *  disabled); `error`, a failed delete's words, shown under the question, which
 *  stays open so the failure is seen. */
export function CloudDeleteConfirm({ shed, busy, error, onCancel, onConfirm }: {
  shed: Shed; busy: boolean; error: string | null; onCancel: () => void; onConfirm: () => void;
}) {
  const root = useRef<HTMLDivElement>(null);
  useEscClose(onCancel);
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let un: (() => void) | undefined;
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const u = await listen<{ button?: unknown }>("press-delete-confirm", (e) => {
        press(root.current?.querySelector(e.payload?.button === "delete" ? "button[data-delete-confirm]" : "button[data-delete-cancel]"));
      });
      if (cancelled) u();
      else un = u;
    })();
    return () => {
      cancelled = true;
      un?.();
    };
  }, []);
  useEffect(() => {
    const el = root.current;
    reportShedCloud("cloud_delete", {
      server: shed.host,
      shed: shed.name,
      title: el?.querySelector("[data-dialog-title]")?.textContent ?? null,
      words: el?.querySelector("[data-delete-words]")?.textContent ?? null,
      buttons: Array.from(el?.querySelectorAll("button") ?? []).map((b) => (b.textContent ?? "").trim()).filter((t) => t !== ""),
      busy,
      error: el?.querySelector("[data-delete-error]")?.textContent ?? null,
    });
  }, [shed.host, shed.name, busy, error]);
  useEffect(() => () => reportShedCloud("cloud_delete", null), []);
  return (
    <Scrim onClose={onCancel} mark="cloud-delete">
      <div ref={root}>
        <DialogShell icon={Trash2} title={`Delete ${shed.name}?`} onClose={onCancel} width={440}>
          <p className="text-[14px] leading-snug text-shed-text-secondary" data-delete-words>{CLOUD_DELETE_WORDS}</p>
          {error && (
            <p className="text-[13px] leading-snug" style={{ color: "var(--shed-err-fg)" }} role="alert" data-delete-error>
              {error}
            </p>
          )}
          <div className="flex items-center justify-between pt-1">
            <button onClick={onCancel} className={dialogBtnSecondary} data-delete-cancel>Cancel</button>
            <button
              onClick={onConfirm}
              disabled={busy}
              className="hbtn inline-flex items-center gap-2 rounded-[9px] px-[22px] py-2.5 text-[14px] font-semibold disabled:opacity-60"
              style={{ background: "var(--shed-err-fg)", color: "var(--shed-surface)", border: "none" }}
              data-delete-confirm
            >
              <Trash2 size={16} /> Delete
            </button>
          </div>
        </DialogShell>
      </div>
    </Scrim>
  );
}
