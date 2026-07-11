/* PreferencesWindow — the dedicated Preferences window (label "preferences"), the
   Tauri port of the mac PreferencesView's grouped form: General / Terminal / the
   namespace-gated approval sections (SSH, AWS, Docker) / Per-shed overrides, in
   the mac section order. A fixed-size decorated window; its own webview + React
   root so it never mounts `useUiBridge`. It reports `{sections, values, mode}`
   under the `prefs` key so the harness drives + asserts it via `prefs.dump`. */
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";
import { Segmented, Select, Switch } from "@/components/dialog";
import {
  inTauri,
  fetchTerminalPresets, getPrefs, setTerminalPref,
  getLoginItem, setLoginItem,
  getSshApproval, setSshApproval,
  fetchGateNamespaces, useCoordinatorData,
  getProviderModes, setProviderMode,
  listShedRules, removeShedRule,
  getAppearanceState, reportPrefs,
  type TerminalPresetInfo, type SshPrefs, type ProviderModes, type ShedRule,
  type PrefsReport,
} from "@/lib/bridge";

/* The credential namespaces the host agent can delegate (mac CredentialNamespace). */
const NS_SSH = "ssh-agent";
const NS_AWS = "aws-credentials";
const NS_DOCKER = "docker-credentials";

// How an SSH-key approval is confirmed. On Linux "Authenticate" routes through
// polkit; "Prompt only" needs no gate — a plain Approve button.
const APPROVAL_METHODS: { id: SshPrefs["method"]; label: string; detail: string }[] = [
  { id: "biometrics-or-password", label: "Authenticate", detail: "Confirm with your login password." },
  { id: "prompt", label: "Prompt only", detail: "A plain Approve button — no password." },
];

// The SSH approval policies, most → least permissive (mirrors SshApprovalPolicy in
// shed-core). `prompts` gates the Method radios and `usesDuration` gates the
// Duration field — the SAME policy→behavior mapping as the Swift app.
const SSH_POLICIES: { id: string; label: string; prompts: boolean; usesDuration: boolean }[] = [
  { id: "always-allow", label: "Always Allow", prompts: false, usesDuration: false },
  { id: "per-shed-allow", label: "Per Shed Allow", prompts: true, usesDuration: false },
  { id: "time-based-allow", label: "Time Based Allow", prompts: true, usesDuration: true },
  { id: "always-ask", label: "Always Ask", prompts: true, usesDuration: false },
  { id: "always-deny", label: "Always Deny", prompts: false, usesDuration: false },
];

/** A titled grouped card: the mac per-section mono label + the rounded container
    the section's rows live in. The optional Footnote is a separate sibling. */
function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <>
      <div className="mb-1.5 font-mono text-[11px] font-semibold uppercase tracking-wider text-shed-text-muted">{title}</div>
      <div className="mb-5 overflow-hidden rounded-[10px] border border-shed-border bg-shed-surface">{children}</div>
    </>
  );
}

/** The muted footnote below a grouped card (mac's per-section footer text). */
function Footnote({ children }: { children: React.ReactNode }) {
  return <p className="-mt-3.5 mb-5 px-1 text-[12px] leading-relaxed text-shed-text-muted">{children}</p>;
}

/** An AWS/Docker provider section: a Segmented Allow|Deny + the mac live-note. */
function ProviderSection({ title, mode, onChange }:
  { title: string; mode: "approve" | "deny"; onChange: (v: "approve" | "deny") => void }) {
  return (
    <>
      <Section title={title}>
        <div className="flex items-center gap-3 px-3.5 py-2.5">
          <span className="text-[13px] text-shed-text">Mode</span>
          <span className="flex-1" />
          <span className="w-[170px]">
            <Segmented
              options={[["approve", "Allow"], ["deny", "Deny"]]}
              value={mode}
              set={(v) => onChange(v as "approve" | "deny")}
            />
          </span>
        </div>
      </Section>
      <Footnote>Live — takes effect immediately. Credentials fail closed (deny) when shed-desktop isn't running.</Footnote>
    </>
  );
}

export default function PreferencesWindow() {
  // Appearance: seeded from the Rust-held app-wide state (the dashboard's mode);
  // unset (a fresh launch) falls back to the OS scheme (popover parity). Live via
  // the same `set-appearance` broadcast the dashboard's mode effect triggers.
  const [mode, setMode] = useState<"light" | "dark">("light");
  const [presets, setPresets] = useState<TerminalPresetInfo[]>([]);
  const [preset, setPreset] = useState("custom");
  const [template, setTemplate] = useState("");
  const [method, setMethod] = useState<SshPrefs["method"]>("biometrics-or-password");
  const [policy, setPolicy] = useState("time-based-allow");
  const [ttl, setTtl] = useState("2h");
  const [launchAtLogin, setLaunchAtLogin] = useState(false);
  const [loginBusy, setLoginBusy] = useState(false);
  const [providerModes, setModes] = useState<ProviderModes>({});
  const [shedRules, setShedRules] = useState<ShedRule[]>([]);
  const sshGen = useRef(0);
  const reloadGen = useRef(0); // bumped per reloadAll; guards every fetch's application
  const ttlAtFocus = useRef(""); // the Duration value when the field gained focus
  // Which namespaces the host agent delegates here — gates the approval sections.
  // Own subscription (separate React root ⇒ separate hook instance from the shell's).
  const gateNs = useCoordinatorData<string[]>("connected-changed", fetchGateNamespaces, []);

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
    void getProviderModes().then((v) => {
      if (fresh()) setModes(v);
    });
    void listShedRules().then((v) => {
      if (fresh()) setShedRules(v);
    });
    // SSH keeps its own guard on top: applySsh bumps sshGen on an optimistic
    // edit, so a slow load must not clobber a newer edit (not just a newer reload).
    const mine = ++sshGen.current;
    void getSshApproval().then((p) => {
      if (mine !== sshGen.current || !fresh()) return;
      setMethod(p.method);
      setPolicy(p.policy);
      setTtl(p.ttl);
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
  // Apply one SSH-pref delta: set it optimistically, persist only the changed
  // field, then reconcile ALL three from the backend — so a rejected/failed write
  // can't leave the method radio misrepresenting the actual gate strength. A
  // generation guard drops a superseded reload so fast Duration typing can't be
  // clobbered by an out-of-order confirm.
  const applySsh = (delta: { method?: SshPrefs["method"]; policy?: string; ttl?: string }) => {
    if (delta.method !== undefined) setMethod(delta.method);
    if (delta.policy !== undefined) setPolicy(delta.policy);
    if (delta.ttl !== undefined) setTtl(delta.ttl);
    const mine = ++sshGen.current;
    void (async () => {
      await setSshApproval(delta.method, delta.policy, delta.ttl);
      const p = await getSshApproval();
      if (mine !== sshGen.current) return; // superseded by a newer change
      setMethod(p.method);
      setPolicy(p.policy);
      setTtl(p.ttl);
    })();
  };
  // A provider mode: optimistic, then reconcile from the coordinator (the setter
  // throws on a rejected namespace; the read-back is the truth either way).
  const chooseProviderMode = (ns: typeof NS_AWS | typeof NS_DOCKER, decision: "approve" | "deny") => {
    setModes((m) => ({ ...m, [ns]: decision }));
    void (async () => {
      try {
        await setProviderMode(ns, decision);
      } catch {
        // fall through to reconcile from the backend truth
      }
      setModes(await getProviderModes());
    })();
  };
  const removeRule = (r: ShedRule) => {
    void (async () => {
      await removeShedRule(r.server ?? "", r.shed ?? "");
      setShedRules(await listShedRules());
    })();
  };

  const policyMeta = SSH_POLICIES.find((p) => p.id === policy);
  const sshGated = gateNs.includes(NS_SSH);
  const awsGated = gateNs.includes(NS_AWS);
  const dockerGated = gateNs.includes(NS_DOCKER);
  const anyGated = sshGated || awsGated || dockerGated;

  // The mac availableTerminalPresets shape: offer the installed presets, plus the
  // persisted one even if it's (no longer) installed so the picker stays truthful.
  const presetOpts = presets
    .filter((p) => p.available || p.id === preset)
    .map((p) => ({ value: p.id, label: p.available ? p.label : `${p.label} (not installed)` }));

  // The visible section ids, in mac order — reported so the harness asserts
  // gated visibility as logical content, not pixels.
  const sections: string[] = [
    "general",
    "terminal",
    ...(anyGated ? [] : ["approvals-empty"]),
    ...(sshGated ? ["ssh"] : []),
    ...(awsGated ? ["aws"] : []),
    ...(dockerGated ? ["docker"] : []),
    ...(shedRules.length ? ["shed-overrides"] : []),
  ];

  // Report the rendered snapshot on mount + every value/visibility change, deduped
  // by content so a re-render without a real change doesn't re-report.
  const report: PrefsReport = {
    sections,
    values: {
      preset,
      template,
      policy,
      method,
      ttl,
      login: launchAtLogin,
      provider_modes: providerModes,
      shed_rules_count: shedRules.length,
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
      <Footnote>Open Shed Desktop when you sign in.</Footnote>

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

      {!anyGated && (
        <Section title="Approvals">
          <p className="px-3.5 py-2.5 text-[12px] leading-relaxed text-shed-text-muted">
            No extensions are delegated to shed-desktop. Set an extension's{" "}
            <code className="font-mono">approval.policy</code> to{" "}
            <code className="font-mono">shed-desktop</code> in extensions.yaml to manage it here.
          </p>
        </Section>
      )}

      {sshGated && (
        <>
          <Section title="SSH approvals">
            <label className="flex items-center gap-3 px-3.5 py-2.5">
              <span className="flex-none text-[13px] text-shed-text">Approval policy</span>
              <span className="flex-1" />
              {/* The dialog Select (appearance-none + own caret): WebKitGTK renders a
                  raw native <select>'s button text illegibly in dark mode. */}
              <span className="w-[190px]" data-ssh-policy>
                <Select
                  value={policy}
                  onChange={(v) => applySsh({ policy: v })}
                  options={SSH_POLICIES.map((p) => ({ value: p.id, label: p.label }))}
                />
              </span>
            </label>

            {/* Duration — only the time-based policy carries one (policy.usesDuration). */}
            {policyMeta?.usesDuration && (
              <label className="flex items-center gap-3 border-t border-shed-border px-3.5 py-2.5">
                <span className="text-[13px] text-shed-text">Duration</span>
                <span className="flex-1" />
                <input
                  value={ttl}
                  // Free text → keep each keystroke LOCAL (optimistic) and persist only
                  // on blur/Enter, and only when it changed. applySsh resets live SSH
                  // grants + rewrites prefs.json, so a per-keystroke persist would revoke
                  // active grants and churn disk mid-typing.
                  onChange={(e) => setTtl(e.target.value)}
                  onFocus={() => {
                    ttlAtFocus.current = ttl;
                  }}
                  onBlur={() => {
                    if (ttl !== ttlAtFocus.current) applySsh({ ttl });
                  }}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") e.currentTarget.blur();
                  }}
                  placeholder="2h"
                  className="w-[72px] rounded-md border border-shed-border bg-shed-inset px-2 py-1 text-right font-mono text-[13px] text-shed-text outline-none"
                  data-ssh-ttl
                />
              </label>
            )}

            {/* Method — only the prompting policies confirm an approval (policy.prompts). */}
            {policyMeta?.prompts &&
              APPROVAL_METHODS.map((m) => {
                const active = method === m.id;
                return (
                  <label
                    key={m.id}
                    className="flex cursor-pointer items-center gap-3 border-t border-shed-border px-3.5 py-2.5"
                    style={{ background: active ? "var(--shed-accent-subtle)" : undefined }}
                  >
                    <input
                      type="radio"
                      name="approval-method"
                      checked={active}
                      onChange={() => applySsh({ method: m.id })}
                      style={{ accentColor: "var(--shed-accent)" }}
                    />
                    <span className="text-[13px] text-shed-text">{m.label}</span>
                    <span className="flex-1" />
                    <span className="truncate text-[12px] text-shed-text-muted">{m.detail}</span>
                  </label>
                );
              })}
          </Section>
          <Footnote>
            Always Allow / Always Deny decide every SSH sign with no prompt. The others prompt,
            then remember your approval per the policy. Changing the policy clears live grants.
            “Method” is the confirmation prompt shown when approving.
          </Footnote>
        </>
      )}

      {awsGated && (
        <ProviderSection
          title="AWS credentials"
          mode={providerModes[NS_AWS] ?? "deny"}
          onChange={(v) => chooseProviderMode(NS_AWS, v)}
        />
      )}
      {dockerGated && (
        <ProviderSection
          title="Docker credentials"
          mode={providerModes[NS_DOCKER] ?? "deny"}
          onChange={(v) => chooseProviderMode(NS_DOCKER, v)}
        />
      )}

      {shedRules.length > 0 && (
        <Section title="Per-shed overrides">
          {shedRules.map((r, i) => (
              <div
                key={`${r.server ?? ""}/${r.shed ?? ""}`}
                className={cn("flex items-center gap-2 px-3.5 py-2", i > 0 && "border-t border-shed-border")}
              >
                <span className="text-[13px] text-shed-text">{r.shed}</span>
                {r.server ? <span className="text-[11px] text-shed-text-muted">{r.server}</span> : null}
                <span className="flex-1" />
                <span
                  className="text-[11px] font-semibold"
                  style={{ color: r.action === "deny" ? "var(--shed-danger)" : "var(--shed-text-muted)" }}
                >
                  {r.action === "deny" ? "auto-deny" : "auto-approve"}
                </span>
                <button
                  onClick={() => removeRule(r)}
                  title="Remove this override"
                  className="hlink flex h-6 w-6 flex-none items-center justify-center rounded-md text-shed-text-muted"
                >
                  <X size={14} />
                </button>
              </div>
            ))}
        </Section>
      )}
    </div>
  );
}
