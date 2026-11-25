/* PreferencesWindow — the dedicated Preferences window (label "preferences"): General
   and Terminal, in that order. (What your sheds get and who answers for you on each
   server are the dashboard's Credentials view, plan 040 §3.9.) A fixed-size decorated
   window; its own webview + React root so it never mounts `useUiBridge`. It reports
   `{sections, values, mode}` under the `prefs` key so the harness drives + asserts it
   via `prefs.dump`. */
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { Select, Switch } from "@/components/dialog";
import { Footnote, Section } from "@/components/prefsKit";
import {
  inTauri,
  fetchTerminalPresets, getPrefs, setTerminalPref,
  getLoginItem, setLoginItem,
  getAppearanceState, reportPrefs,
  type TerminalPresetInfo, type PrefsReport,
} from "@/lib/bridge";

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
  }, []);

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
  // The mac availableTerminalPresets shape: offer the installed presets, plus the
  // persisted one even if it's (no longer) installed so the picker stays truthful.
  const presetOpts = presets
    .filter((p) => p.available || p.id === preset)
    .map((p) => ({ value: p.id, label: p.available ? p.label : `${p.label} (not installed)` }));

  // The visible section ids, in order — reported so the harness asserts them as
  // logical content, not pixels.
  const sections: string[] = ["general", "terminal"];

  // Report the rendered snapshot on mount + every value/visibility change, deduped
  // by content so a re-render without a real change doesn't re-report.
  const report: PrefsReport = {
    sections,
    values: {
      preset,
      template,
      login: launchAtLogin,
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

    </div>
  );
}
