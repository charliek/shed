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
 *  the WebView actually applied the Plex CSS (a resolved color, not a fallback).
 *  `mode` mirrors the active appearance so the appearance op is observable — taken
 *  from the caller (the React state) when known, else read back off the root
 *  `data-mode` (the readiness report, before a mode is threaded through). */
function sampleStyle(mode?: string) {
  const cs = getComputedStyle(document.body);
  const main = document.querySelector("[data-pane]");
  return {
    bg: cs.backgroundColor,
    color: cs.color,
    accent: main ? getComputedStyle(main).getPropertyValue("--shed-accent").trim() : "",
    mode: mode ?? document.documentElement.dataset.mode ?? "light",
  };
}

/** Invoke a Rust command, swallowing errors to `undefined` (the shell degrades to
 *  an empty list rather than throwing inside an effect). */
async function invoke<T>(cmd: string, args?: Record<string, unknown>): Promise<T | undefined> {
  const core = await import("@tauri-apps/api/core");
  return core.invoke<T>(cmd, args).catch(() => undefined);
}

/** The sidebar nav badge counts (sheds / active RC sessions / hosts / pending
 *  approvals), carried in the shell's full snapshot so the harness can assert the
 *  rendered counts as UI truth via `ui.badges` — not pixels. */
export type Badges = { sheds: number; agents: number; hosts: number; pending: number };

/** Report the rendered snapshot Rust relays to the harness (`ui.current_pane` /
 *  `ui.computed_style` / `dashboard.dump` / `ui.badges`). ALWAYS a FULL blob — every
 *  key present — so no partial report can (a) create the `main` slot before the shell
 *  is ready and false-signal readiness to `sheds.refresh`, or (b) omit `sheds` and
 *  zero the mac tray running-count (lib.rs recomputes it from each report). A new
 *  reader is one more key. The refresh token is echoed so `sheds.refresh` blocks on it. */
function report(
  pane: Pane,
  sheds: Shed[],
  refreshToken: number,
  modal: Modal,
  badges: Badges,
  mode?: string,
) {
  void invoke("ui_report", {
    snapshot: { pane, style: sampleStyle(mode), sheds, refresh_token: refreshToken, modal, badges },
  });
}

/** Wire the shell to Rust: drive the pane from the `navigate` event, keep a live
 *  shed list (fetched on mount + on each `refresh` event — the `sheds.refresh`
 *  op), and report the rendered snapshot back so the harness can assert it.
 *  Returns the live sheds + a `refresh` callback (a lifecycle button chains it to
 *  re-fetch after its action). A no-op in a plain browser.
 *
 *  This owns the SINGLE report path — the shell's badges (`agentCount`/`pending`
 *  supplied by App, `sheds`/`hosts` derived here) and the active `mode` fold into
 *  one full snapshot, so there are no partial `ui_report`s to race the `main` slot
 *  or drop keys (App must apply `data-mode` before this effect runs — a
 *  useLayoutEffect — so the sampled colors reflect the flip).
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
  mode: "light" | "dark",
  agentCount: number,
  pending: number,
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
      // Fresh sheds + real badges arrive via the report effect on the next render;
      // this initial report just publishes the pane, so `current_pane != null` =
      // "listeners live". No modal is open at mount; badges seed at zero.
      report(paneRef.current, [], 0, null, { sheds: 0, agents: 0, hosts: 0, pending: 0 });
    })();
    return () => {
      cancelled = true;
      ready.current = false;
      unlisten.forEach((u) => u());
    };
  }, [setPane, fetchSheds]);

  // Re-report on any rendered change (pane, sheds, echoed token, modal, the badge
  // inputs, or the appearance mode). Gated on `ready` (not a first-run flag) so the
  // listener effect owns the initial report and `current_pane` stays a true
  // "listeners live" signal under StrictMode replay. One FULL snapshot per report —
  // badges are derived+carried here, never published as a partial `ui_report`.
  useEffect(() => {
    if (!inTauri() || !ready.current) return;
    const hosts = new Set(sheds.map((s) => s.host)).size;
    const badges: Badges = { sheds: sheds.length, agents: agentCount, hosts, pending };
    report(pane, sheds, refreshToken, modal, badges, mode);
  }, [pane, sheds, refreshToken, modal, mode, agentCount, pending]);

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

// The known kinds, plus `(string & {})` so an UNKNOWN kind from a newer/other tool
// keeps its raw string (the unknown-kind policy) instead of failing the type — it
// renders neutrally and is never offered for creation.
export type RcKind =
  | "claude-rc" | "claude-broker" | "codex" | "opencode" | "cursor" | "shell"
  | (string & {});
export type RcState =
  | "starting" | "ready" | "reconnecting" | "needs-trust" | "needs-auth" | "dead";

/** One agent's install-probe result (capabilities.agents). */
export type RcAgentInfo = { installed: boolean; version?: string | null };
/** Per-kind UI hints (capabilities.kind_features). */
export type RcKindFeatures = { post_input: boolean; approvals: string };
/** A shed's RC capabilities — mirrors `shed_core::rc::RcCapabilities`. */
export type RcCapabilities = {
  rc_version: number;
  kinds: RcKind[];
  agents: Record<string, RcAgentInfo>;
  features: string[];
  kind_features: Record<string, RcKindFeatures>;
};

/** The kinds a create form can offer (broker is URL-driven; unknown never creatable). */
export const RC_CREATABLE_KINDS: RcKind[] = ["claude-rc", "codex", "opencode", "cursor", "shell"];

/** The tool token a kind's agent maps to under capabilities.agents (undefined = no
 *  agent, e.g. shell). Mirrors `RcKind::tool`. */
const RC_KIND_TOOL: Record<string, string | undefined> = {
  "claude-rc": "claude", "claude-broker": "claude",
  codex: "codex", opencode: "opencode", cursor: "cursor", shell: undefined,
};

/** The launch UI's gated kind list: with capabilities, the creatable kinds whose
 *  backing agent is installed. Only ABSENT capabilities (old binary / not yet
 *  probed) fall back to claude+shell; present-but-empty capabilities yield an
 *  EMPTY offer (the shed advertises no usable kinds — the form must not invent
 *  claude). Mirrors `RcCapabilities::creatable_kinds` / the Swift `availableKinds`. */
export function offeredKinds(caps?: RcCapabilities): RcKind[] {
  if (!caps) return ["claude-rc", "shell"];
  return RC_CREATABLE_KINDS.filter((k) => {
    if (!caps.kinds.includes(k)) return false;
    const tool = RC_KIND_TOOL[k];
    return tool ? (caps.agents[tool]?.installed ?? false) : true;
  });
}

/** Per-kind login remediation for a needs-auth session. Mirrors `RcKind::auth_hint`. */
export function rcAuthHint(kind: RcKind): string {
  switch (kind) {
    case "claude-rc": case "claude-broker": return "run `claude` → /login";
    case "codex": return "run `codex` and complete login (`codex login`)";
    case "opencode": return "run `opencode auth login`";
    case "cursor": return "run `cursor-agent login`";
    default: return "log in to the agent in a terminal";
  }
}

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

/** The `rc.list` result: live sessions plus the per-shed capabilities captured
 *  during the probe (keyed by `host/shed`). */
export type RcListResult = { sessions: RcSession[]; capabilities: Record<string, RcCapabilities> };

/** The live RC sessions + capabilities across running sheds (the same data the
 *  `rc.list` op serves the harness). Best-effort — empty in a browser / on error. */
export async function fetchRcList(host?: string, shed?: string): Promise<RcListResult> {
  const r = await invoke<RcListResult>("rc_list", { host, shed });
  return { sessions: r?.sessions ?? [], capabilities: r?.capabilities ?? {} };
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

/** The active RC-session COUNT for the Agents sidebar badge, kept live at the App
 *  level. Derived from a dedicated `rc.list` fetch (NOT `reportAgents`, which the
 *  backend blanks off-pane) — refreshed on mount, on each lifecycle `refresh` event
 *  (sheds coming up/down), and whenever `bump` advances (the Agents pane signals a
 *  launch/kill, which emits no `refresh` event). A no-op count of 0 in a plain
 *  browser / on error. */
export function useAgentCount(bump: number): number {
  const [count, setCount] = useState(0);
  // A generation guard shared across the event- and bump-driven reloads, so a
  // slower older fetch can't overwrite a newer one's count.
  const gen = useRef(0);
  const reload = useCallback(() => {
    if (!inTauri()) return;
    const mine = ++gen.current;
    void fetchRcList().then((r) => {
      if (mine === gen.current) setCount(r.sessions.length);
    });
  }, []);
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let unlisten: (() => void) | undefined;
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const un = await listen("refresh", reload);
      if (cancelled) un();
      else unlisten = un;
      reload(); // initial fetch, after the listener is live
    })();
    return () => {
      cancelled = true;
      unlisten?.();
    };
  }, [reload]);
  // Re-fetch when the Agents pane signals a session change (launch/kill); skip the
  // initial mount value (bump===0) — the effect above already did the first fetch.
  useEffect(() => {
    if (bump > 0) reload();
  }, [bump, reload]);
  return count;
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
