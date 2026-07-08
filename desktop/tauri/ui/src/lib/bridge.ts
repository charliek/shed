import { useCallback, useEffect, useRef, useState } from "react";

export type Pane = "sheds" | "approvals" | "agents" | "activity" | "system";

/** Which modal (if any) is open — reported so the harness can drive + assert it. */
export type Modal = null | "prefs" | "create";

const PANES: readonly Pane[] = ["sheds", "approvals", "agents", "activity", "system"];

/** Narrow an untrusted value (an IPC payload) to a known pane. */
export function isPane(x: unknown): x is Pane {
  return typeof x === "string" && (PANES as readonly string[]).includes(x);
}

/** A shed as shed-core serializes it — the fields the dashboard reads (more exist). */
export type Shed = {
  name: string;
  host: string;
  status: string;
  backend?: string | null;
  image?: string | null;
  cpus?: number | null;
  memory_mb?: number | null;
};

/** Running inside a Tauri webview (vs a plain browser during `vite dev`)? */
export function inTauri(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}

/** Sample the rendered theme so the harness's computed-style probe can confirm
 *  the WebView actually applied the linen CSS (a resolved color, not a fallback). */
function sampleStyle() {
  const cs = getComputedStyle(document.body);
  const main = document.querySelector("[data-pane]");
  return {
    bg: cs.backgroundColor,
    color: cs.color,
    accent: main ? getComputedStyle(main).getPropertyValue("--shed-accent").trim() : "",
  };
}

/** Invoke a Rust command, swallowing errors to `undefined` (the shell degrades to
 *  an empty list rather than throwing inside an effect). */
async function invoke<T>(cmd: string, args?: Record<string, unknown>): Promise<T | undefined> {
  const core = await import("@tauri-apps/api/core");
  return core.invoke<T>(cmd, args).catch(() => undefined);
}

/** Report the rendered snapshot Rust relays to the harness (`ui.current_pane` /
 *  `ui.computed_style` / `dashboard.dump`). One blob, so a new reader is one more
 *  key. The refresh token is echoed so `sheds.refresh` can block on it. */
function report(pane: Pane, sheds: Shed[], refreshToken: number, modal: Modal) {
  void invoke("ui_report", {
    snapshot: { pane, style: sampleStyle(), sheds, refresh_token: refreshToken, modal },
  });
}

/** Wire the shell to Rust: drive the pane from the `navigate` event, keep a live
 *  shed list (fetched on mount + on each `refresh` event — the `sheds.refresh`
 *  op), and report the rendered snapshot back so the harness can assert it.
 *  Returns the live sheds + a `refresh` callback (a lifecycle button chains it to
 *  re-fetch after its action). A no-op in a plain browser.
 *
 *  Readiness: the initial report is emitted only AFTER both the navigate + refresh
 *  listeners register, so `current_pane != null` tells the harness the listeners
 *  are live and a `ui.navigate`/`refresh` won't be lost to an attach race (which
 *  is also why `ui.navigate` fails `frontend_not_ready`, and `sheds.refresh` only
 *  blocks on the echo, until then). */
export function useUiBridge(
  pane: Pane,
  setPane: (p: Pane) => void,
  modal: Modal,
): { sheds: Shed[]; refresh: () => void } {
  const [sheds, setSheds] = useState<Shed[]>([]);
  const [refreshToken, setRefreshToken] = useState(0);
  // paneRef lets the mount effect read the latest pane for its initial report
  // without taking `pane` as a dep (which would re-subscribe the listeners).
  const paneRef = useRef(pane);
  paneRef.current = pane;
  const ready = useRef(false);
  const fetchGen = useRef(0);

  // Fetch the live shed list and store it; on a refresh, also advance the echoed
  // token so Rust's synchronous `sheds.refresh` observes completion. A generation
  // guard drops a superseded (slower, older) fetch so it can't overwrite a newer
  // one's rows OR re-report stale rows under an already-advanced token.
  const fetchSheds = useCallback(async (token: number) => {
    const gen = ++fetchGen.current;
    const rows = (await invoke<Shed[]>("list_sheds")) ?? [];
    if (gen !== fetchGen.current) return; // a newer fetch started — drop this one
    setSheds(rows);
    if (token > 0) setRefreshToken(token);
  }, []);

  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    const unlisten: Array<() => void> = [];
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      unlisten.push(
        await listen<{ pane?: unknown }>("navigate", (e) => {
          if (isPane(e.payload?.pane)) setPane(e.payload.pane);
        }),
      );
      unlisten.push(
        await listen<{ token?: unknown }>("refresh", (e) => {
          const t = e.payload?.token;
          void fetchSheds(typeof t === "number" ? t : 0);
        }),
      );
      if (cancelled) {
        unlisten.forEach((u) => u());
        return;
      }
      ready.current = true; // both listeners live → readiness signal + initial report
      await fetchSheds(0);
      // Fresh sheds arrive via the report effect on the next render; this initial
      // report just publishes the pane, so `current_pane != null` = "listeners live".
      // No modal is open at mount.
      report(paneRef.current, [], 0, null);
    })();
    return () => {
      cancelled = true;
      ready.current = false;
      unlisten.forEach((u) => u());
    };
  }, [setPane, fetchSheds]);

  // Re-report on any rendered change (pane, sheds, or the echoed token). Gated on
  // `ready` (not a first-run flag) so the listener effect owns the initial report
  // and `current_pane` stays a true "listeners live" signal under StrictMode replay.
  useEffect(() => {
    if (!inTauri() || !ready.current) return;
    report(pane, sheds, refreshToken, modal);
  }, [pane, sheds, refreshToken, modal]);

  const refresh = useCallback(() => void fetchSheds(0), [fetchSheds]);
  return { sheds, refresh };
}

/** Fire a lifecycle action (a shed card button). The caller re-fetches via the
 *  hook's `refresh` so the card reflects the new state. Best-effort in a browser. */
export async function shedAction(action: string, name: string, host: string): Promise<void> {
  if (!inTauri()) return;
  await invoke("shed_action", { action, name, host });
}

/** Disk-usage shapes (shed-core's df models, serialized). */
export type DiskSize = { logical_bytes: number; physical_bytes: number };
export type DiskTotals = {
  images: DiskSize;
  sheds: DiskSize;
  snapshots: DiskSize;
  orphans: DiskSize;
  all: DiskSize;
};
export type HostDiskUsage = {
  host: string;
  usage: { backend?: string | null; totals: DiskTotals } | null;
  error?: string | null;
};

/** Per-host disk usage for the System pane (the same data the `system.df` op
 *  serves the harness). An unreachable host comes back as an error row. */
export async function fetchSystemDf(): Promise<HostDiskUsage[]> {
  return (await invoke<HostDiskUsage[]>("system_df")) ?? [];
}

/* ---- terminal + prefs (the Preferences view + the shed-card button) ------- */
export type TerminalPresetInfo = { id: string; label: string; detail: string; available: boolean };
export type TerminalPrefs = { terminal_preset: string; terminal_template: string };

export async function fetchTerminalPresets(): Promise<TerminalPresetInfo[]> {
  return (await invoke<{ presets: TerminalPresetInfo[] }>("terminal_presets"))?.presets ?? [];
}

export async function getPrefs(): Promise<TerminalPrefs> {
  return (
    (await invoke<TerminalPrefs>("get_prefs")) ?? { terminal_preset: "custom", terminal_template: "" }
  );
}

export async function setTerminalPref(preset: string, template?: string): Promise<void> {
  await invoke("set_terminal_pref", { preset, template });
}

/* ---- launch-at-login (B4) -------------------------------------------------- */

/** Whether the app is registered to launch at login (the Preferences → General
 *  toggle reads this on mount + reconciles to it after a set). */
export async function getLoginItem(): Promise<boolean> {
  return (await invoke<{ enabled: boolean }>("loginitem_status"))?.enabled ?? false;
}

/** Set launch-at-login. THROWS on error (unlike the swallowing `invoke`) so the
 *  toggle surfaces a failed/guarded write and reconciles from `getLoginItem`
 *  rather than silently misrepresenting the real state. */
export async function setLoginItem(enabled: boolean): Promise<void> {
  const core = await import("@tauri-apps/api/core");
  await core.invoke("loginitem_set", { enabled });
}

/** Open a shed in the user's chosen terminal (best-effort; a no-op in a browser
 *  and disabled server-side under test mode). A non-empty `session` attaches that
 *  tmux session (the Agents console button → `tmux attach -t rc-<slug>`). */
export async function openTerminal(shed: string, host: string, session?: string): Promise<void> {
  if (!inTauri()) return;
  await invoke("open_terminal", { shed, host, session });
}

/* ---- create (the New-Shed dialog) ----------------------------------------- */
export type CreateFields = {
  name: string;
  host?: string;
  image?: string;
  vm_backend?: string;
  cpus?: number;
  memory_mb?: number;
  repo?: string;
};
export type CreateProgress = {
  id: string;
  state: string; // "progress" | "complete" | "error" | "unknown"
  messages: string[];
  shed?: Shed | null;
  error?: string | null;
};

/** The configured hosts a create can target (populates the dialog's host picker),
 *  including hosts with no sheds yet. */
export async function fetchHosts(): Promise<string[]> {
  return (await invoke<string[]>("list_hosts")) ?? [];
}

/** Start a create; returns the id to poll. Throws on error (the dialog surfaces
 *  it), unlike the error-swallowing `invoke` helper. */
export async function createStart(fields: CreateFields): Promise<string> {
  const core = await import("@tauri-apps/api/core");
  return core.invoke<string>("create_start", { form: fields });
}

// NB: Tauri v2 #[tauri::command] looks up invoke args in camelCase (the Rust
// `create_id` param → the key `createId`), so multi-word args MUST be sent camelCase.
export async function createStatus(createId: string): Promise<CreateProgress | undefined> {
  return invoke<CreateProgress>("create_status", { createId });
}

export async function createCancel(createId: string): Promise<void> {
  await invoke("create_cancel", { createId });
}

/* ---- approvals + activity (Phase B) --------------------------------------- */

/** A pending approval card, as the coordinator serializes it (metadata only —
 *  never key material). `gate` drives the fingerprint affordance. */
export type Approval = {
  id: string;
  namespace: string;
  op: string;
  shed: string;
  server?: string;
  detail: string;
  expires_at: string;
  gate: "none" | "biometrics" | "biometrics-or-password";
  default_scope: "per-request" | "per-session" | "per-shed";
  default_ttl: string;
};

/** An audit entry (a decision or a streamed host event) for the Activity feed. */
export type AuditEntry = {
  id: string;
  ts: string;
  source: string;
  server?: string | null;
  shed?: string | null;
  ns?: string | null;
  op?: string | null;
  result: string;
  detail?: string | null;
  approval?: string | null;
  policy?: string | null;
};

export async function fetchApprovals(): Promise<Approval[]> {
  return (await invoke<Approval[]>("approvals_list")) ?? [];
}

/** Approve/deny a pending request. `scope`/`ttl` apply the grant on approve;
 *  `persist` installs a per-shed always-allow/deny rule. */
export async function decideApproval(
  id: string,
  decision: "approve" | "deny",
  opts?: { scope?: string; ttl?: string; persist?: boolean },
): Promise<void> {
  await invoke("approval_decide", {
    id,
    decision,
    scope: opts?.scope,
    ttl: opts?.ttl,
    persist: opts?.persist ?? false,
  });
}

export async function fetchActivity(limit = 200): Promise<AuditEntry[]> {
  return (await invoke<AuditEntry[]>("activity_list", { limit })) ?? [];
}

export async function fetchGateNamespaces(): Promise<string[]> {
  return (await invoke<string[]>("gate_namespaces")) ?? [];
}

/** The current SSH approval prefs, as the coordinator serializes them. */
export type SshPrefs = {
  method: "biometrics-or-password" | "biometrics" | "prompt";
  policy: string;
  ttl: string;
};

export async function getSshApproval(): Promise<SshPrefs> {
  return (
    (await invoke<SshPrefs>("ssh_prefs_get")) ??
    // Dev-only fallback (non-Tauri `invoke` → null); mirror Rust SshPrefs::default().
    { method: "biometrics-or-password", policy: "time-based-allow", ttl: "2h" }
  );
}

export async function setSshApproval(method?: string, policy?: string, ttl?: string): Promise<void> {
  await invoke("set_ssh_approval", { method, policy, ttl });
}

/** Keep a coordinator-backed slice live: fetch on mount, then re-fetch whenever
 *  the coordinator emits `event` (the TauriEventSink app.emit). `fetch` is held
 *  in a ref so the subscription is set up once (a no-op in a plain browser). */
export function useCoordinatorData<T>(event: string, fetch: () => Promise<T>, initial: T): T {
  const [data, setData] = useState<T>(initial);
  const fetchRef = useRef(fetch);
  fetchRef.current = fetch;
  const gen = useRef(0);
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let unlisten: (() => void) | undefined;
    // A generation guard (as in useUiBridge): events can fire faster than fetches
    // resolve (e.g. disconnect then expiry), so a slower older fetch must not
    // overwrite a newer one's rows — drop any fetch that isn't the latest.
    const reload = () => {
      const mine = ++gen.current;
      void fetchRef.current().then((d) => {
        if (!cancelled && mine === gen.current) setData(d);
      });
    };
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const un = await listen(event, reload);
      if (cancelled) un();
      else unlisten = un;
      reload(); // initial fetch, after the listener is live
    })();
    return () => {
      cancelled = true;
      unlisten?.();
    };
  }, [event]);
  return data;
}

/** A 1s wall-clock tick for the "expires in Ns" countdown (display only — the
 *  backend's own tick decides the actual expiry). */
export function useNowTick(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}

/* ---- remote-control agents (B2.4) ----------------------------------------- */

export type RcKind = "claude-rc" | "claude-broker" | "shell";
export type RcState =
  | "starting" | "ready" | "reconnecting" | "needs-trust" | "needs-auth" | "dead";

/** A remote-control session, as shed-app serializes it (the pane's fields). The
 *  table/wire identity is the computed `host/shed/slug`, not encoded. */
export type RcSession = {
  host: string;
  shed: string;
  slug: string;
  tmux_session: string;
  display_name: string;
  workdir: string;
  kind: RcKind;
  state: RcState;
  url?: string | null;
  rc_id?: string | null;
  created_by?: string | null;
  created_at?: string | null;
  target_label?: string | null;
  managed: boolean;
};

/** The live RC sessions across running sheds (the same data the `rc.list` op
 *  serves the harness). Best-effort — [] in a browser / on error. */
export async function fetchRcSessions(host?: string, shed?: string): Promise<RcSession[]> {
  return (await invoke<{ sessions: RcSession[] }>("rc_list", { host, shed }))?.sessions ?? [];
}

export type RcLaunchFields = {
  shed: string;
  kind: RcKind;
  host?: string;
  // camelCase: Tauri looks up the Rust `display_name`/`initial_prompt` params here.
  displayName?: string;
  workdir?: string;
  initialPrompt?: string;
};

/** Launch an RC session. THROWS on error (the pane surfaces it) — unlike the
 *  swallowing `invoke`, because a validation / SSH failure must be shown. */
export async function rcLaunch(fields: RcLaunchFields): Promise<RcSession> {
  const core = await import("@tauri-apps/api/core");
  return core.invoke<RcSession>("rc_launch", fields);
}

/** Kill an RC session. THROWS on error (the pane surfaces it). */
export async function rcKill(shed: string, slug: string, host?: string): Promise<void> {
  const core = await import("@tauri-apps/api/core");
  await core.invoke("rc_kill", { shed, slug, host });
}

/** Report the rendered RC sessions so the `agents.dump` op can observe them — the
 *  drivable truth of the Agents pane, like `dashboard.dump` reads the sheds.
 *  (`ui_report` merges this `agents` key with the shell's snapshot.) */
export function reportAgents(sessions: RcSession[]): void {
  void invoke("ui_report", { snapshot: { agents: sessions } });
}

/* ---- menu-bar popover (B1b) ------------------------------------------------ */

/** The live shed list (the same `list_sheds` the dashboard + `sheds.list` use). */
export async function fetchSheds(): Promise<Shed[]> {
  return (await invoke<Shed[]>("list_sheds")) ?? [];
}

/** Footer actions the popover invokes. A 2nd webview can't call the IPC ops or emit
 *  the main-window events, so these are dedicated Tauri commands (test-mode-safe:
 *  they only show/emit/exit, never spawn or write). */
export async function openDashboard(): Promise<void> {
  await invoke("open_dashboard");
}
export async function openPreferences(): Promise<void> {
  await invoke("open_preferences");
}
export async function quitApp(): Promise<void> {
  await invoke("app_exit");
}
/** Ask Rust to content-size the popover window to the measured height (Swift NSPopover
 *  parity — no dead space). macOS-only in effect; a no-op on other targets. */
export async function resizePopover(height: number): Promise<void> {
  await invoke("resize_popover", { height });
}

/** Report the popover's compact rows so `tray.dump` can assert them (the hermetic
 *  content AC — OS tray clicks aren't drivable). The popover webview invokes this,
 *  so Rust keys it under the `popover` snapshot, never the dashboard's `main`. */
export function reportTray(rows: {
  connected: boolean;
  running_sheds: { host: string; name: string }[];
  pending_approvals: { namespace: string; op: string; shed: string }[];
}): void {
  void invoke("ui_report", { snapshot: { tray: rows } });
}
