/* TrayPopover — the macOS menu-bar popover (B1b): ≤6 running sheds and a footer (Open dashboard ·
   Preferences… · Check for Updates… [disabled] · Quit). A SEPARATE webview/entry
   from the dashboard shell, so it fetches its own data and reports its rows under
   the `popover` window key (never the dashboard's `main`, which
   `dashboard.dump`/`current_pane` read). */
import { useEffect, useRef, useState } from "react";
import { AppWindow, Settings, ArrowDownCircle, Power } from "lucide-react";
import {
  inTauri, fetchSheds,
  openDashboard, openPreferences, quitApp, reportTray, resizePopover,
  updaterStatus, updaterCheck,
  type Shed, type UpdaterStatus,
} from "@/lib/bridge";

/** The row tooltip for each updater reason — `ok` is the enabled prompt; the rest are
 *  truthful about WHY the check is unavailable (test mode / installed app / apt). */
const UPDATER_TOOLTIP: Record<UpdaterStatus["reason"], string> = {
  ok: "Check for updates",
  test_mode: "Updates are disabled in test mode",
  no_bundle: "Updates require the installed app",
  linux_apt: "Updates arrive via apt",
};

export default function TrayPopover() {
  const [sheds, setSheds] = useState<Shed[]>([]);
  // The updater row's status (fetched once on mount — it's fixed for the process
  // lifetime). Null until fetched → the row renders disabled (the safe default).
  const [updater, setUpdater] = useState<UpdaterStatus | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    void updaterStatus().then((s) => { if (!cancelled && s) setUpdater(s); });
    return () => { cancelled = true; };
  }, []);

  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    // A generation guard (as in useUiBridge) so a slower older fetch can't overwrite
    // a newer one's rows — events can outrun fetches. `cancelled` drops any late
    // resolve + unlistens if cleanup raced the async listen() setup (StrictMode
    // double-mount).
    let gen = 0;
    const loadSheds = () => { const m = ++gen; void fetchSheds().then((v) => { if (!cancelled && m === gen) setSheds(v); }); };

    loadSheds();
    const un: Array<() => void> = [];
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      // Sheds re-fetch on a lifecycle `refresh`, and on the Rust show path's
      // `popover-refresh` so the popover freshens each time it opens.
      un.push(await listen("refresh", loadSheds));
      un.push(await listen("popover-refresh", loadSheds));
      if (cancelled) un.forEach((u) => u()); // cleanup already ran before listen resolved
    })();
    return () => { cancelled = true; un.forEach((u) => u()); };
  }, []);

  const running = sheds.filter((s) => s.status === "running");
  const runningTop = running.slice(0, 6);

  // Report the rendered rows so `tray.dump` can assert the popover content (the
  // drivable truth, since OS tray clicks + a real screenshot aren't hermetic).
  useEffect(() => {
    if (!inTauri()) return;
    reportTray({
      running_sheds: runningTop.map((s) => ({ host: s.host, name: s.name })),
    });
  }, [sheds]); // eslint-disable-line react-hooks/exhaustive-deps

  // Content-size the window: measure the rendered root height and ask Rust to resize
  // the popover to hug it (no dead space). The root is
  // content-height (no min-h-screen), so its box height IS the content; a
  // ResizeObserver re-measures when the sheds change. rAF-debounced + only
  // invoked when the height actually changed (>1px), so the window resize (which
  // reflows the root back to the same content height) can't feed a measure→resize loop.
  useEffect(() => {
    if (!inTauri()) return;
    const el = rootRef.current;
    if (!el) return;
    let raf = 0;
    let last = 0;
    const measure = () => {
      raf = 0;
      const h = Math.ceil(el.getBoundingClientRect().height);
      if (h > 0 && Math.abs(h - last) > 1) {
        last = h;
        void resizePopover(h);
      }
    };
    const schedule = () => { if (!raf) raf = requestAnimationFrame(measure); };
    const ro = new ResizeObserver(schedule);
    ro.observe(el);
    schedule(); // initial (rAF is throttled while hidden — see below)

    // The window builds at MAX height and WebKit throttles rAF for a HIDDEN window,
    // so the ResizeObserver's initial measure can stall arbitrarily on the first open
    // (the popover stays 640 tall until it eventually fires). Re-measure explicitly
    // whenever the Rust show path fires `popover-refresh` — the window is visible by
    // then — via TIMERS (not rAF), so a still-throttled rAF can't leave it un-sized.
    // Two shots cover layout settling after the show + the data re-fetch.
    const timers: number[] = [];
    let unlisten = () => {};
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      unlisten = await listen("popover-refresh", () => {
        timers.push(window.setTimeout(measure, 0), window.setTimeout(measure, 120));
      });
    })();
    return () => {
      ro.disconnect();
      if (raf) cancelAnimationFrame(raf);
      timers.forEach(clearTimeout);
      unlisten();
    };
  }, []);

  return (
    <div ref={rootRef} className="flex w-full flex-col overflow-hidden rounded-[12px] bg-shed-surface text-shed-text" data-tray-popover>
      <div className="flex items-center px-3.5 py-2.5">
        <span className="text-[13px] font-medium">shed desktop</span>
      </div>
      <div className="h-px bg-shed-border" />

      {/* running sheds (≤6) */}
      <div className="px-3.5 py-2">
        <div className="mb-1 flex items-center justify-between">
          <span className="text-[13px] font-semibold">Sheds</span>
          <span className="text-[11px] text-shed-text-muted">{running.length} running</span>
        </div>
        {runningTop.length === 0 ? (
          <div className="py-1 text-[12px] text-shed-text-muted">no running sheds</div>
        ) : (
          runningTop.map((s) => (
            <div key={`${s.host}/${s.name}`} className="flex items-center gap-2 py-1 text-[12px]">
              <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: "var(--shed-ok)" }} />
              <span className="truncate">{s.host}/{s.name}</span>
            </div>
          ))
        )}
      </div>
      <div className="h-px bg-shed-border" />

      {/* footer — icon'd rows with an accent-blue hover */}
      <div className="flex flex-col p-1.5">
        <FooterRow icon={AppWindow} label="Open dashboard" onClick={() => void openDashboard()} />
        <FooterRow icon={Settings} label="Preferences…" onClick={() => void openPreferences()} />
        <FooterRow
          icon={ArrowDownCircle}
          label="Check for Updates…"
          disabled={!updater?.enabled}
          // While the status fetch is unresolved OR after a failed fetch (updater ===
          // null in both cases), show a neutral tooltip — never the no_bundle reason,
          // which would misattribute the state. The row stays disabled meanwhile.
          title={updater ? UPDATER_TOOLTIP[updater.reason] : "Checking updater status…"}
          onClick={updater?.enabled ? () => void updaterCheck() : undefined}
        />
        <FooterRow icon={Power} label="Quit" onClick={() => void quitApp()} />
      </div>
    </div>
  );
}

function FooterRow(
  { icon: Icon, label, onClick, disabled, title }:
  { icon: typeof Settings; label: string; onClick?: () => void; disabled?: boolean; title?: string },
) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      title={title}
      className="menu-row flex items-center gap-2.5 rounded-md px-2 py-1.5 text-left text-[13px] disabled:opacity-40"
    >
      <span className="flex w-[18px] shrink-0 justify-center"><Icon size={15} /></span>
      {label}
    </button>
  );
}
