/* PreferencesWindow — the dedicated Preferences window (label "preferences"): General,
   Terminal and Provisioning (with this machine's source rows), in that order. A fixed-size decorated window; its own webview + React
   root so it never mounts `useUiBridge`. It reports `{sections, values, mode}`
   under the `prefs` key so the harness drives + asserts it via `prefs.dump`. */
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Select, Switch } from "@/components/dialog";
import { Footnote, Section } from "@/components/prefsKit";
import { ProvisioningSection, type ProvisioningReport } from "@/components/ProvisioningSection";
import {
  inTauri,
  fetchTerminalPresets, getPrefs, setTerminalPref,
  getLoginItem, setLoginItem,
  getAppearanceState, reportPrefs,
  getSourceStatus, makeThisMachineSource, cleanMachineText,
  type TerminalPresetInfo, type PrefsReport,
} from "@/lib/bridge";
import {
  EMPTY_SOURCES,
  beginMove, canUse, createSourceReader, moveConfirmed, moveFailed, shownRows,
  type SourceReader, type SourcesState,
} from "@/lib/sources";

export default function PreferencesWindow() {
  // Appearance: seeded from the Rust-held app-wide state (the dashboard's mode);
  // unset (a fresh launch) falls back to the OS scheme (popover parity). Live via
  // the same `set-appearance` broadcast the dashboard's mode effect triggers.
  const [mode, setMode] = useState<"light" | "dark">("light");
  const [presets, setPresets] = useState<TerminalPresetInfo[]>([]);
  const [preset, setPreset] = useState("custom");
  const [template, setTemplate] = useState("");
  const [launchAtLogin, setLaunchAtLogin] = useState(false);
  const [loginBusy, setLoginBusy] = useState(false);
  // Provisioning (plan 039 A7): what its section rendered, and a key that re-reads it.
  const [provisioning, setProvisioning] = useState<ProvisioningReport | null>(null);
  const [provisioningKey, setProvisioningKey] = useState(0);
  // Credential sources (plan 035 §3.4): every server's row + this machine's identity,
  // reconciled per server by `lib/sources.ts` (reads in issue order, a move owning its
  // row while in flight). The ref is the truth every async step reads and replaces;
  // the state mirrors it for rendering.
  const srcRef = useRef<SourcesState>(EMPTY_SOURCES);
  const [src, setSrc] = useState<SourcesState>(EMPTY_SOURCES);
  const srcSeq = useRef(0); // every read (at issue) and move answer (at arrival) takes one
  // The reads, their one pending later read, and their lifetimes (`createSourceReader`):
  // made per mount by the effect below (StrictMode mounts, cleans up and mounts again),
  // disposed at unmount, so a read or a timer that lands after it does nothing.
  const sourceReader = useRef<SourceReader | null>(null);
  const sourceOps = useMemo(() => {
    const commit = (next: SourcesState) => {
      srcRef.current = next;
      setSrc(next);
    };
    return {
      commit,
      // `hold`: the host whose move was just confirmed (see `applyRead`).
      read: (hold?: string) => sourceReader.current?.read(hold),
      // The window was shown again or gained focus (throttled).
      refresh: () => sourceReader.current?.refresh(),
      // A (re)load of the whole window (mount, `prefs-changed`): a new generation.
      reload: () => sourceReader.current?.reload(),
    };
  }, []);
  useEffect(() => {
    const reader = createSourceReader({
      fetch: getSourceStatus,
      get: () => srcRef.current,
      commit: sourceOps.commit,
      nextSeq: () => ++srcSeq.current,
      now: () => Date.now(),
      setTimer: (fn, ms) => setTimeout(fn, ms),
      clearTimer: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
    });
    sourceReader.current = reader;
    return () => {
      reader.dispose();
      if (sourceReader.current === reader) sourceReader.current = null;
    };
  }, [sourceOps]);
  const reloadGen = useRef(0); // bumped per reloadAll; guards every fetch's application

  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    const uns: Array<() => void> = [];
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      // Listener BEFORE the initial read: `ui.set_appearance` updates the Rust
      // cell before emitting, so read-after-attach can never be older than a
      // heard event (no attach race, no flash).
      uns.push(
        await listen<{ mode?: unknown }>("set-appearance", (e) => {
          if (e.payload?.mode === "light" || e.payload?.mode === "dark") setMode(e.payload.mode);
        }),
      );
      const initial = await getAppearanceState();
      if (!cancelled) {
        setMode(initial ?? (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light"));
      }
      if (cancelled) uns.forEach((u) => u());
    })();
    return () => {
      cancelled = true;
      uns.forEach((u) => u());
    };
  }, []);
  useLayoutEffect(() => {
    document.documentElement.dataset.mode = mode;
  }, [mode]);

  // (Re)load every value the window shows. Ran on mount and on `prefs-changed`
  // (an IPC-driven mutation changed prefs behind the window's back — its own
  // controls update local state directly).
  const reloadAll = useCallback(() => {
    // One generation captured at entry, checked before every async application:
    // two interleaved prefs-changed reloads can't let an older fetch resolving
    // last leave stale values (or a stale prefs.dump).
    const gen = ++reloadGen.current;
    const fresh = () => gen === reloadGen.current;
    void fetchTerminalPresets().then((v) => {
      if (fresh()) setPresets(v);
    });
    void getLoginItem().then((v) => {
      if (fresh()) setLaunchAtLogin(v);
    });
    void getPrefs().then((p) => {
      if (!fresh()) return;
      setPreset(p.terminal_preset);
      setTemplate(p.terminal_template);
    });
    // Ordered per server by its own sequence (`lib/sources.ts`), not by `fresh()`.
    sourceOps.reload();
    setProvisioningKey((k) => k + 1);
  }, [sourceOps]);

  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let unlisten: (() => void) | undefined;
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const un = await listen("prefs-changed", reloadAll);
      if (cancelled) un();
      else unlisten = un;
      reloadAll(); // initial load, after the listener is live
    })();
    return () => {
      cancelled = true;
      unlisten?.();
    };
  }, [reloadAll]);

  const choosePreset = (id: string) => {
    setPreset(id);
    void setTerminalPref(id, id === "custom" ? template : undefined);
  };
  const editTemplate = (t: string) => {
    setTemplate(t);
    if (preset === "custom") void setTerminalPref("custom", t);
  };
  // Launch-at-login: set optimistically, persist via the THROWING setter, then
  // reconcile from loginitem.status — so a failed/guarded write can't leave the
  // toggle misrepresenting the real state. Guarded by `loginBusy` so a fast
  // double-toggle can't race enable()/disable() into the wrong final OS state.
  const toggleLaunchAtLogin = (v: boolean) => {
    setLaunchAtLogin(v);
    setLoginBusy(true);
    void (async () => {
      try {
        await setLoginItem(v);
      } catch {
        // fall through to reconcile from the backend truth
      }
      setLaunchAtLogin(await getLoginItem()); // getLoginItem swallows → never throws
      setLoginBusy(false);
    })();
  };
  // "Use this machine" on one server: ONE click, no confirmation. A move is undone the
  // same way (one click on the other machine), and the footnote says what it does — a
  // dialog would only slow down the one thing this row is for. The row flips optimistically and owns itself until the move
  // answers (its button is gone meanwhile: no second move on that row can overlap). A
  // confirmed move shows the server's own answer, then is read back; a refused one
  // goes back to the row it replaced, keeps the refusal under it in the words the
  // backend chose, and is read back too. Moves on different servers run side by side.
  const chooseSource = (host: string) => {
    const started = beginMove(srcRef.current, host);
    if (!started) return;
    sourceOps.commit(started);
    void (async () => {
      try {
        const { row } = await makeThisMachineSource(host);
        sourceOps.commit(moveConfirmed(srcRef.current, host, ++srcSeq.current, row));
        sourceOps.read(host);
      } catch (e) {
        const why = cleanMachineText(typeof e === "string" ? e : e instanceof Error ? e.message : String(e));
        sourceOps.commit(moveFailed(srcRef.current, host, why));
        sourceOps.read();
      }
    })();
  };

  // Re-read the sources when the window comes back: closing Preferences only HIDES it
  // (a reopen does not remount), and the source can move elsewhere meanwhile. Three
  // triggers, all throttled by `sourceOps.refresh` (at most one read per
  // SOURCE_REFRESH_MIN_MS, the rest folded into one trailing read):
  //  * `prefs-shown` — emitted by the one native show path (`show_preferences_window`)
  //    when a built window is shown again (tray, popover footer, dashboard gear, the
  //    `ui.show_preferences` op): the same under WKWebView (macOS) and WebKitGTK
  //    (Linux), whatever the window manager — the trigger that does not depend on
  //    either webview;
  //  * the DOM `focus` event on `window` — WKWebView fires it when the NSWindow becomes
  //    key with the web view as first responder (also on a click back into an open,
  //    unfocused window); WebKitGTK when the GtkWindow gets focus-in, which a window
  //    manager hands over;
  //  * `visibilitychange` to `visible` — WebKitGTK follows the GtkWindow being mapped
  //    (hidden on close, visible on reopen); WKWebView follows the NSWindow's
  //    visibility and occlusion, so it also fires when the window is uncovered.
  // Each was checked on its own under the harness's Xvfb (WebKitGTK, no window
  // manager): `prefs-shown` alone, and the DOM events alone, each refresh a reopen.
  useEffect(() => {
    const onFocus = () => sourceOps.refresh();
    const onVisible = () => {
      if (document.visibilityState === "visible") sourceOps.refresh();
    };
    window.addEventListener("focus", onFocus);
    document.addEventListener("visibilitychange", onVisible);
    let cancelled = false;
    let unlisten: (() => void) | undefined;
    if (inTauri()) {
      void (async () => {
        const { listen } = await import("@tauri-apps/api/event");
        const un = await listen("prefs-shown", () => {
          sourceOps.refresh();
          setProvisioningKey((k) => k + 1);
        });
        if (cancelled) un();
        else unlisten = un;
      })();
    }
    return () => {
      cancelled = true;
      unlisten?.();
      window.removeEventListener("focus", onFocus);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [sourceOps]);

  // The harness's press (`ui.press_source_use`, test mode only): find THIS window's own
  // button for the host and click it, so the press runs the button's handler and is
  // refused by its disabled state exactly as a pointer's would be.
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let unlisten: (() => void) | undefined;
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const un = await listen<{ host?: unknown }>("press-source-use", (e) => {
        const host = e.payload?.host;
        if (typeof host !== "string") return;
        document.querySelectorAll<HTMLButtonElement>("button[data-source-use]").forEach((el) => {
          if (el.dataset.sourceUse === host && !el.disabled) el.click();
        });
      });
      if (cancelled) un();
      else unlisten = un;
    })();
    return () => {
      cancelled = true;
      unlisten?.();
    };
  }, []);

  // Rows only for servers that support sources (an older server shows nothing).
  const sourceRows = shownRows(src);
  const thisMachine = src.thisMachine;
  const thisMachineError = src.thisMachineError;

  // The mac availableTerminalPresets shape: offer the installed presets, plus the
  // persisted one even if it's (no longer) installed so the picker stays truthful.
  const presetOpts = presets
    .filter((p) => p.available || p.id === preset)
    .map((p) => ({ value: p.id, label: p.available ? p.label : `${p.label} (not installed)` }));

  // The visible section ids, in order — reported so the harness asserts them as
  // logical content, not pixels.
  const sections: string[] = [
    "general",
    "terminal",
    ...(provisioning ? ["provisioning"] : []),
  ];

  // Report the rendered snapshot on mount + every value/visibility change, deduped
  // by content so a re-render without a real change doesn't re-report.
  const report: PrefsReport = {
    sections,
    values: {
      preset,
      template,
      login: launchAtLogin,
      sources: sourceRows.map((r) => ({
        host: r.host,
        source_name: r.source ? r.source.name : null,
        this_machine: r.this_machine,
        connected: r.connected,
        busy: !!src.moving[r.host],
        retrying: r.busy,
        unreachable: !!r.unreachable,
        // `canUse` is the one rule behind the button: no button (this machine is
        // the source) is never reported as usable.
        can_use: canUse(src, r.host),
        error: src.errors[r.host] ?? r.error ?? null,
      })),
      this_machine_name: sourceRows.length ? (thisMachine?.name ?? null) : null,
      this_machine_error: sourceRows.length ? thisMachineError : null,
      provisioning,
    },
    mode,
  };
  const reportJson = JSON.stringify(report);
  useEffect(() => {
    if (!inTauri()) return;
    reportPrefs(report);
  }, [reportJson]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div className="h-full overflow-y-auto bg-shed-bg px-5 py-4 text-shed-text">
      <Section title="General">
        <label className="flex items-center gap-3 px-3.5 py-2.5">
          <span className="text-[13px] text-shed-text">Launch at login</span>
          <span className="flex-1" />
          <Switch on={launchAtLogin} set={toggleLaunchAtLogin} disabled={loginBusy} />
        </label>
      </Section>
      <Footnote>
        Open Shed Desktop when you sign in. This machine answers provisioning requests only
        while the app is running.
      </Footnote>

      <Section title="Terminal">
        <label className="flex items-center gap-3 px-3.5 py-2.5">
          <span className="flex-none text-[13px] text-shed-text">Open sheds in</span>
          <span className="flex-1" />
          <span className="w-[200px]">
            <Select value={preset} onChange={choosePreset} options={presetOpts} />
          </span>
        </label>
        {preset === "custom" && (
          <div className="border-t border-shed-border px-3.5 py-2.5">
            <input
              value={template}
              onChange={(e) => editTemplate(e.target.value)}
              placeholder="e.g. ghostty -e {cmd}"
              className="w-full rounded-[8px] border border-shed-border bg-shed-inset px-2.5 py-1.5 font-mono text-[13px] text-shed-text outline-none focus:border-shed-accent"
              data-terminal-template
            />
          </div>
        )}
      </Section>
      <Footnote>
        {preset === "custom" ? (
          <>
            <code className="font-mono">{"{cmd}"}</code> is replaced with the ssh command,{" "}
            <code className="font-mono">{"{shed}"}</code> with the shed name.
          </>
        ) : (
          presets.find((p) => p.id === preset)?.detail ||
          'Which terminal opens when you click "Open in Terminal" on a shed.'
        )}
      </Footnote>

      {/* Provisioning (plan 039 A7): the providers, the credential source rows (which
          machine answers for you on each server), and the preview. */}
      <ProvisioningSection
        reloadKey={provisioningKey}
        onReport={setProvisioning}
        sourcesKey={JSON.stringify(report.values.sources)}
        sources={
          sourceRows.length > 0 && (
            <>
              <Section title="Source machine" id="sources">
            {/* Credential sources (plan 035 §3.4): which machine answers for you on each
                server that supports it. Machine names come from other machines — text
                nodes only, never a title or tooltip. */}
            {sourceRows.length > 0 && (
              <div className="border-t border-shed-border px-3.5 py-2.5 first:border-t-0" data-source-this-machine>
                <div className="text-[12px] text-shed-text-muted">
                  This machine:{" "}
                  <span className="font-medium text-shed-text">{thisMachine ? thisMachine.name : "unknown"}</span>
                </div>
                {thisMachineError && (
                  <div className="mt-1 text-[12px] font-medium text-shed-danger" data-source-identity-error>
                    This machine has no identity, so it can't become a source: {cleanMachineText(thisMachineError)}
                  </div>
                )}
              </div>
            )}
            {sourceRows.map((r) => {
              const busy = !!src.moving[r.host];
              const live = canUse(src, r.host);
              const failure = src.errors[r.host] ?? r.error;
              return (
                <div key={r.host} className="flex items-center gap-3 border-t border-shed-border px-3.5 py-2.5 first:border-t-0" data-source-row={r.host}>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-[13px] text-shed-text">{r.host}</div>
                    <div className="truncate text-[12px] text-shed-text-muted">
                      {r.source ? (
                        <>
                          Source: <span className="text-shed-text">{r.source.name || "an unnamed machine"}</span>
                          {r.unreachable ? (
                            <span data-source-unreachable> · cannot reach the server</span>
                          ) : r.busy ? (
                            " · server busy, retrying"
                          ) : (
                            !r.connected && " · not connected"
                          )}
                        </>
                      ) : r.unreachable ? (
                        // Unreachable is unknown, never "no source".
                        <span data-source-unreachable>Cannot reach the server</span>
                      ) : r.busy ? (
                        // Busy is unknown, never "no source".
                        <span data-source-retrying>Checking the source — the server is busy, retrying…</span>
                      ) : (
                        "No source yet — the first machine to connect becomes it."
                      )}
                    </div>
                    {failure && (
                      <div className="mt-1 text-[12px] font-medium text-shed-danger" data-source-error>
                        {failure}
                      </div>
                    )}
                  </div>
                  {r.this_machine ? (
                    <span className="flex-none text-[11px] font-semibold text-shed-text-muted" data-source-this>
                      this machine
                    </span>
                  ) : (
                    <button
                      type="button"
                      onClick={() => chooseSource(r.host)}
                      disabled={!live}
                      className="hbtn flex-none rounded-[8px] border border-shed-border bg-shed-inset px-2.5 py-1 text-[12px] font-medium text-shed-text"
                      style={{ opacity: live ? 1 : 0.5, cursor: live ? "pointer" : "default" }}
                      data-source-use={r.host}
                    >
                      {busy ? "Switching…" : "Use this machine"}
                    </button>
                  )}
                </div>
              );
            })}
              </Section>
          {sourceRows.length > 0 && (
            <Footnote>
              <span data-source-footnote>
                One machine answers provisioning requests per server, and nothing fails over
                on its own: if the source is off, nothing answers until you choose another.
                Moving the source here makes this machine answer at once.
              </span>
            </Footnote>
          )}
            </>
          )
        }
      />
    </div>
  );
}
