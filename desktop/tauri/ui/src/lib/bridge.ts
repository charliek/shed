import { useCallback, useEffect, useRef, useState } from "react";

export type Pane = "sheds" | "machines" | "approvals" | "agents" | "activity" | "egress" | "system";

/** Which modal (if any) is open — reported so the harness can drive + assert it.
 *  (Preferences is a dedicated window, not a modal — see `reportPrefs`.) */
export type Modal = null | "create" | "launch" | "machine";

const PANES: readonly Pane[] = ["sheds", "machines", "approvals", "agents", "activity", "egress", "system"];

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

/** Why a host couldn't be listed. `agent_upgrade_required` is the one cause whose
 *  remedy is a user action; `other` is the open set (rendered generically). */
export type HostFailureKind = "agent_upgrade_required" | "other";

/** One host's failure in presentation shape (`shed_app::HostFailure`): `summary`
 *  is the one-line strip text with the REMEDY FIRST (strips truncate from the
 *  end), `detail` is the tooltip body. */
export type HostFailure = {
  server: string;
  kind: HostFailureKind;
  summary: string;
  detail: string;
};

/** The `list_sheds` / `sheds.list` payload: the sheds of every reachable host,
 *  plus the per-host failures beside them (additive — a host that can't be listed
 *  is surfaced, not silently dropped). Both keys are ALWAYS emitted (`host_errors`
 *  is `[]` when every host is healthy) — the Rust side and this bundle ship in one
 *  binary, so there is no older-payload path to defend against; the `?? []` at the
 *  fetch sites covers only a failed `invoke` (which yields no payload at all). */
export type ShedsPayload = { sheds: Shed[]; host_errors: HostFailure[] };

/** The Sheds pane's empty state — reported as UI truth so the harness can assert
 *  that a host failure (not the generic "nothing here yet" copy) is what a user
 *  with zero listable sheds actually reads. */
export type EmptyState = { title: string; body: string };

/** The Sheds empty state, deferring to a per-host failure when there is one: with
 *  no sheds AND a failed host, leading with "No sheds yet" would imply everything
 *  is fine and blame the user's config (shed#300). The single source for both the
 *  render and the reported UI truth.
 *
 *  It NAMES the unreachable servers but carries no transport error text — the
 *  reason belongs to the sidebar's SHED SERVERS section (and the System pane),
 *  which is where a person looks for "is that box up". A pane that cannot list
 *  anything should say so once and point, not reprint a stack of `connect:`
 *  strings. */
export function shedsEmptyState(sheds: Shed[], hostErrors: HostFailure[]): EmptyState | null {
  if (sheds.length > 0) return null; // the list renders instead
  if (hostErrors.length > 0) {
    const names = hostErrors.map((e) => e.server).join(", ");
    return {
      title: hostErrors.length === 1 ? `${names} is unreachable` : "Some shed servers are unreachable",
      body: `No sheds could be listed from ${names}. Check SHED SERVERS in the sidebar for status.`,
    };
  }
  return {
    title: "No sheds yet",
    body: "No sheds on the configured hosts. Create one to spin up an isolated dev environment.",
  };
}

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

/** One machine as the sidebar's status foot renders it: the WORD a person reads,
 *  never the raw transport error (which stays on hover). */
export type SidebarMachineRow = { name: string; origin: string; status: string; note: string };

/** What the shell knows about the sidebar's two sections before the host
 *  failures are folded in (which `report` does, since it holds them). */
export type SidebarInput = {
  servers: { name: string; reachable: boolean }[];
  machines: SidebarMachineRow[];
};

/** The sidebar's status foot as rendered — UI truth for the surface that took
 *  over from the Sheds pane's error strip, so "an unreachable server is still
 *  visible somewhere, with its reason reachable" stays a test rather than a hope.
 *
 *  It rides the shell's ONE full report rather than a `reportSidebar` of its own:
 *  the sidebar paints on the FIRST render, before `useUiBridge` has attached its
 *  navigate/refresh listeners, so a partial report from here would create the
 *  `main` slot early and false-signal readiness (see `report`). */
export type SidebarReport = {
  servers: { name: string; reachable: boolean; detail: string }[];
  machines: SidebarMachineRow[];
};

/** The failure for one host, if any — the SINGLE lookup shared by the sidebar's
 *  rendered tooltip and the reported `detail`, so the two cannot drift into
 *  disagreeing about whether a host has a reason attached. */
export function hostFailureFor(hostErrors: HostFailure[], host: string): HostFailure | undefined {
  return hostErrors.find((e) => e.server === host);
}

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
  sidebar: SidebarInput,
  mode?: string,
  hostErrors: HostFailure[] = [],
) {
  void invoke("ui_report", {
    snapshot: {
      pane,
      style: sampleStyle(mode),
      sheds,
      refresh_token: refreshToken,
      modal,
      badges,
      // The sidebar's status foot. `detail` is the host's failure summary — the
      // hover text — so a test can prove the reason is threaded to the row and
      // not merely that the row exists.
      sidebar: {
        servers: sidebar.servers.map((sv) => ({
          ...sv,
          detail: hostFailureFor(hostErrors, sv.name)?.summary ?? "",
        })),
        machines: sidebar.machines,
      },
      // The Sheds pane's failure surface (`dashboard.dump.host_errors` / `.empty`):
      // the strip rows, and the empty state ONLY while that pane is rendered — an
      // off-pane `empty` would report copy nobody is reading (the `agents.dump`
      // on-pane rule).
      host_errors: hostErrors,
      empty: pane === "sheds" ? shedsEmptyState(sheds, hostErrors) : null,
    },
  });
}

/** Wire the shell to Rust: drive the pane from the `navigate` event, keep a live
 *  shed list (fetched on mount, on each `refresh` event — the `sheds.refresh`
 *  op — and on a 5s visible-only poll so a CLI-created shed surfaces on the open
 *  dashboard), and report the rendered snapshot back so the harness can assert it.
 *  Returns the live sheds + a `refresh` callback (a lifecycle button chains it to
 *  re-fetch after its action). A no-op in a plain browser.
 *
 *  This owns the SINGLE report path — the shell's badges (`agentCount`/`hostCount`/
 *  `pending` supplied by App, `sheds` derived here) and the active `mode` fold into
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
  hostCount: number,
  pending: number,
  sidebar: SidebarInput,
): { sheds: Shed[]; hostErrors: HostFailure[]; refresh: () => void } {
  const [sheds, setSheds] = useState<Shed[]>([]);
  const [hostErrors, setHostErrors] = useState<HostFailure[]>([]);
  const [refreshToken, setRefreshToken] = useState(0);
  // paneRef lets the mount effect read the latest pane for its initial report
  // without taking `pane` as a dep (which would re-subscribe the listeners).
  const paneRef = useRef(pane);
  paneRef.current = pane;
  const ready = useRef(false);
  const fetchGen = useRef(0);
  // The highest refresh token not yet echoed. A token-bearing fetch records it
  // BEFORE awaiting, so whichever fetch ends up committing rows echoes it —
  // see `fetchSheds`.
  const pendingToken = useRef(0);

  // Fetch the live shed list and store it; on a refresh, also advance the echoed
  // token so Rust's synchronous `sheds.refresh` observes completion. A generation
  // guard drops a superseded (slower, older) fetch so it can't overwrite a newer
  // one's rows.
  //
  // The token is HANDED OFF rather than echoed by whichever fetch happens to
  // finish: a token-bearing fetch claims it before awaiting, and the fetch that
  // actually COMMITS rows echoes it. So (a) `sheds.refresh` never sees its token
  // acknowledged against rows that were then dropped — the reply it builds from
  // the reported snapshot is the state that was really committed — and (b) a
  // concurrent background poll (token 0) superseding it still discharges the
  // token, so the op can't block to timeout.
  const fetchSheds = useCallback(async (token: number) => {
    const gen = ++fetchGen.current;
    if (token > pendingToken.current) pendingToken.current = token;
    const payload = await invoke<ShedsPayload>("list_sheds");
    if (gen !== fetchGen.current) return; // a newer fetch started — it owns the commit + the echo
    setSheds(payload?.sheds ?? []);
    // The failed hosts travel WITH the rows (one payload, one state update) so the
    // strip can never describe a different fetch than the list it sits above.
    setHostErrors(payload?.host_errors ?? []);
    if (pendingToken.current > 0) {
      setRefreshToken(pendingToken.current);
      pendingToken.current = 0;
    }
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
      report(
        paneRef.current, [], 0, null,
        { sheds: 0, agents: 0, hosts: 0, pending: 0 },
        { servers: [], machines: [] },
      );
    })();
    return () => {
      cancelled = true;
      ready.current = false;
      unlisten.forEach((u) => u());
    };
  }, [setPane, fetchSheds]);

  // Auto-refresh the shed list on a cadence so an externally-created shed (e.g.
  // `shed create` from the CLI, or another client) surfaces on the ALREADY-OPEN
  // dashboard without a manual action — the mac AppModel polls every 5s
  // (AppModel.swift `pollInterval`); this is the Tauri parity. Reuses the SAME
  // `fetchSheds` path the `refresh` event uses — no second fetch mechanism.
  //
  // JS-side by design (why-only): no Rust ticker / IPC-event plumbing is needed —
  // @tauri-apps/api/window is enough to observe visibility from JS. Unlike Swift
  // (which polls even when hidden), we poll ONLY while the window is actually
  // shown — the webview is HIDDEN (not unmounted) when the window is closed to the
  // tray, so polling then would waste battery + network.
  //
  // The GATE is the NATIVE window `isVisible()` (shown vs hidden-to-tray), NOT
  // `document.visibilityState`: the latter tracks WebKit *occlusion*, so a shown-
  // but-occluded window (another app on top) reads "hidden" and would wrongly
  // stall the poll — proven flaky under the harness in isolation. `isVisible()`
  // tracks show()/hide() directly (CloseRequested → w.hide(), tray-reopen →
  // show()+set_focus(); see lib.rs / ipc.rs present_main_window). We ALSO refresh
  // IMMEDIATELY on regaining foreground (window focus, or the occlusion
  // `visibilitychange` when it does fire) so a tray-reopen shows fresh state at
  // once without waiting out the interval.
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    const POLL_MS = 5000;
    // The window handle, resolved async below; until then the tick no-ops.
    let win: { isVisible: () => Promise<boolean> } | undefined;
    // `fetchSheds(0)` = refresh WITHOUT advancing the echoed token (this is a
    // background poll, not a `sheds.refresh` completion Rust blocks on).
    // `pollInFlight` serializes the timer: if a `list_sheds` hangs past the
    // interval, ticks skip rather than pile up unbounded behind the slow one.
    let pollInFlight = false;
    const tick = async () => {
      if (!win || pollInFlight) return;
      const visible = await win.isVisible().catch(() => true); // fail-open on probe error
      if (cancelled || !visible) return;
      pollInFlight = true;
      try {
        await fetchSheds(0);
      } finally {
        pollInFlight = false;
      }
    };
    const timer = setInterval(() => void tick(), POLL_MS);
    const onVisibility = () => {
      if (document.visibilityState === "visible") void fetchSheds(0);
    };
    document.addEventListener("visibilitychange", onVisibility);
    const unlistenFocus: Array<() => void> = [];
    void (async () => {
      const { getCurrentWindow } = await import("@tauri-apps/api/window");
      const w = getCurrentWindow();
      win = w;
      const un = await w.onFocusChanged(({ payload: focused }) => {
        if (focused) void fetchSheds(0); // authoritative "user reopened the window"
      });
      if (cancelled) un();
      else unlistenFocus.push(un);
    })();
    return () => {
      cancelled = true;
      clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisibility);
      unlistenFocus.forEach((u) => u());
    };
  }, [fetchSheds]);

  // Re-report on any rendered change (pane, sheds, echoed token, modal, the badge
  // inputs, or the appearance mode). Gated on `ready` (not a first-run flag) so the
  // listener effect owns the initial report and `current_pane` stays a true
  // "listeners live" signal under StrictMode replay. One FULL snapshot per report —
  // badges are derived+carried here, never published as a partial `ui_report`.
  useEffect(() => {
    if (!inTauri() || !ready.current) return;
    // `hosts` = the configured-host count (with reachability) the shell supplies —
    // covers zero-shed hosts, unlike a sheds-derived distinct-host count.
    const badges: Badges = { sheds: sheds.length, agents: agentCount, hosts: hostCount, pending };
    report(pane, sheds, refreshToken, modal, badges, sidebar, mode, hostErrors);
    // `sidebar` is rebuilt by the caller each render, so it is compared by VALUE
    // (a reference dep would re-report on every render — the flap this whole
    // shape exists to avoid).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pane, sheds, hostErrors, refreshToken, modal, mode, agentCount, hostCount, pending, JSON.stringify(sidebar)]);

  const refresh = useCallback(() => void fetchSheds(0), [fetchSheds]);
  return { sheds, hostErrors, refresh };
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

/* ---- egress (mac parity: profiles per host + the ns=="egress" activity) ---- */

/** One egress profile fragment, as shed-core decodes it (shed-server's
 *  `config.EgressProfile` — all fields optional). */
export type EgressProfile = {
  mode?: string | null;
  allow?: string[] | null;
  deny?: string[] | null;
  rule?: string | null;
};
/** One entry of `GET /api/egress/profiles` (`source`: "config" | "user"). */
export type EgressProfileInfo = { name: string; source: string; profile: EgressProfile };
/** One host's egress profiles, or the error that host returned — unreachable /
 *  egress-disabled hosts come back as error rows (the system_df row shape). */
export type HostEgressProfiles = {
  host: string;
  profiles: EgressProfileInfo[];
  error?: string | null;
};

/** Per-host egress profiles for the Egress pane's Profiles sub-tab. */
export async function fetchEgressProfiles(): Promise<HostEgressProfiles[]> {
  return (await invoke<HostEgressProfiles[]>("egress_profiles")) ?? [];
}

/** The Egress pane's rendered state, reported so the `egress.profiles` op can
 *  observe it (UI truth, like `reportAgents`): the active sub-tab, the flat
 *  profile rows + per-host error rows, the selected profile's detail, and how
 *  many ns=="egress" activity rows the Activity sub-tab renders. Keys are
 *  additive + stable — the harness asserts them. */
export type EgressReport = {
  tab: "activity" | "profiles";
  profiles: { host: string; name: string; source: string }[];
  errors: { host: string; error: string }[];
  selected: {
    host: string;
    name: string;
    source: string;
    allow: string[];
    deny: string[];
    mode?: string | null;
    rule?: string | null;
  } | null;
  activity_count: number;
};

/** Report the rendered Egress pane state (mounted-only, like `reportAgents` —
 *  `ui_report` merges this `egress` key with the shell's snapshot). Pass `null` on
 *  unmount to CLEAR the key (the merge overwrites `egress` with null, not skips it),
 *  so an off-pane or freshly-remounted-but-not-yet-reported `egress.profiles` dump
 *  returns null instead of the previous mount's stale snapshot. */
export function reportEgress(snapshot: EgressReport | null): void {
  void invoke("ui_report", { snapshot: { egress: snapshot } });
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

/* ---- credential broker (leg 3a.2) ----------------------------------------- */

/** The persisted broker-mode preference (default `auto`). */
export type BrokerMode = "auto" | "embedded" | "external";
/** The effective mode auto-detect (or the pref) resolved to for this process. */
export type BrokerEffective = "external" | "embedded" | "headless-coexist";

/** The credential-broker status the Preferences control reads: the effective mode +
 *  pref, the socket probe that produced it, the config provenance, and — in embedded
 *  mode — the resolved ssh mode, gate namespaces, per-server health, and any
 *  broker-failed error (fields that don't apply are null/empty). Mirrors
 *  `broker::BrokerRuntime::status`. */
export type BrokerStatus = {
  effective_mode: BrokerEffective;
  /** The LIVE persisted pref (tracks a `set_mode` without a relaunch). */
  pref: BrokerMode;
  /** The persisted pref drifted from the launch pref ⟹ a relaunch applies it. */
  restart_required: boolean;
  probe: { desktop_socket_live: boolean; status_socket_live: boolean };
  config: { source: "loaded" | "synthesized" | "error" | null; message: string | null };
  resolved_ssh_mode: string | null;
  gate_namespaces: string[];
  servers: unknown[];
  broker_error: string | null;
};

export async function getBrokerStatus(): Promise<BrokerStatus | undefined> {
  return invoke<BrokerStatus>("broker_status");
}

/** Set the broker mode. Does NOT hot-swap — the reply carries `restart_required:true`
 *  and the CURRENT effective mode; the control shows a restart-to-apply hint. THROWS on
 *  an unknown mode (unlike the swallowing `invoke`). */
export async function setBrokerMode(
  mode: BrokerMode,
): Promise<{ pref: BrokerMode; effective: BrokerEffective; restart_required: boolean }> {
  const core = await import("@tauri-apps/api/core");
  return core.invoke("broker_set_mode", { mode });
}

/** Open a shed in the user's chosen terminal (best-effort; a no-op in a browser
 *  and disabled server-side under test mode). A non-empty `session` attaches that
 *  tmux session (the Agents console button → `tmux attach -t rc-<slug>`). */
export async function openTerminal(shed: string, host: string, session?: string): Promise<void> {
  if (!inTauri()) return;
  await invoke("open_terminal", { shed, host, session });
}

/** The fields a new machine needs — exactly `shed_core::config::MachineEntry`.
 *  Everything but `name` is optional and falls back to the config reader's own
 *  defaults, so adding `mini3` really is one field. */
export type NewMachine = {
  name: string;
  host?: string;
  user?: string;
  ssh_port?: number;
  rc_bin?: string;
};

/** Append a machine to `~/.shed/config.yaml` and start watching it.
 *
 *  Throws with the backend's message on a duplicate name or an unwritable
 *  config — the dialog shows it rather than closing on a failure. */
export async function addMachine(m: NewMachine): Promise<void> {
  if (!inTauri()) return;
  const core = await import("@tauri-apps/api/core");
  await core.invoke("add_machine", { machine: m });
}

/** Open a MACHINE session in a terminal (`ssh -t … tmux attach`).
 *
 *  Takes the SLUG, not the tmux name the shed path passes: a machine's pane
 *  name is derived host-side from the slug, so handing over the derived name
 *  would mean two places agreeing about the `rc-` prefix instead of one. */
export async function openMachineTerminal(machine: string, slug: string): Promise<void> {
  if (!inTauri()) return;
  await invoke("open_terminal", { machine, session: slug, shed: "", host: null });
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

/** The AWS/Docker provider approval modes, keyed by the full namespace string
 *  (`aws-credentials`/`docker-credentials`) → `"approve"` | `"deny"`. A namespace
 *  absent from the map is Deny (fail-closed default). */
export type ProviderModes = Record<string, "approve" | "deny">;

export async function getProviderModes(): Promise<ProviderModes> {
  return (await invoke<ProviderModes>("provider_modes_get")) ?? {};
}

/** Set an AWS/Docker provider mode. THROWS on a rejected namespace (the coordinator
 *  only accepts the two providers), unlike the error-swallowing `invoke`. */
export async function setProviderMode(
  ns: "aws-credentials" | "docker-credentials",
  decision: "approve" | "deny",
): Promise<void> {
  const core = await import("@tauri-apps/api/core");
  await core.invoke("set_provider_mode", { ns, decision });
}

/** A per-shed override rule (the Preferences "Per-shed overrides" list). */
export type ShedRule = {
  scope: string;
  server?: string | null;
  namespace?: string | null;
  shed?: string | null;
  action: "approve" | "deny" | "prompt";
  gate?: string;
};

export async function listShedRules(): Promise<ShedRule[]> {
  return (await invoke<ShedRule[]>("policy_list_shed")) ?? [];
}

/** Remove one per-shed override rule (its row's remove button). */
export async function removeShedRule(server: string, shed: string): Promise<void> {
  await invoke("remove_shed_rule", { server, shed });
}

/** The app-wide light/dark appearance, read synchronously by the Preferences window
 *  on mount (`null` = unset → fall back to `prefers-color-scheme`). */
export async function getAppearanceState(): Promise<"light" | "dark" | null> {
  return (await invoke<{ mode: "light" | "dark" | null }>("get_appearance_state"))?.mode ?? null;
}

/** Broadcast a light/dark change app-wide (idempotent — no re-emit when unchanged).
 *  Invoked from the dashboard's mode effect so every window stays in sync. */
export async function setAppearanceState(mode: "light" | "dark"): Promise<void> {
  await invoke("set_appearance_state", { mode });
}

/** The Preferences window's rendered snapshot, reported under its own window label
 *  ("preferences") so the `prefs.dump` op can observe it (UI truth, like the egress
 *  report): the visible section ids in mac order, the rendered control values, and
 *  the active light/dark mode. Keys are additive + stable — the harness asserts them. */
export type PrefsReport = {
  sections: string[];
  values: {
    preset: string;
    template: string;
    policy: string;
    method: string;
    ttl: string;
    login: boolean;
    provider_modes: ProviderModes;
    shed_rules_count: number;
    broker_pref: BrokerMode;
    broker_effective: BrokerEffective;
    broker_error: string | null;
  };
  mode: string;
};

/** Report the Preferences window's rendered state (`ui_report` keys it under the
 *  calling window's label, so it can never clobber the dashboard's `main`). */
export function reportPrefs(snapshot: PrefsReport): void {
  void invoke("ui_report", { snapshot: { prefs: snapshot } });
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
/** The agent kinds this build names. The first six mirror the guest's tmux/RC
 *  registry; `gx` and `grok` are roost-only row kinds (plan 017 §3.2) — an agent
 *  shed can SEE in somebody's roost tab, with no guest producer. `gx` is `grok`
 *  with a live remote lane behind it, which is a promotion roost never makes and
 *  `RoostSession::agent_kind` does. */
export type RcKind =
  | "claude-rc" | "claude-broker" | "codex" | "opencode" | "cursor" | "shell"
  | "gx" | "grok"
  | (string & {});
export type RcState =
  | "starting" | "ready" | "reconnecting" | "needs-trust" | "needs-auth" | "dead";

/** One agent's install-probe result (capabilities.agents). */
export type RcAgentInfo = { installed: boolean; version?: string | null };
/** Per-kind UI hints (capabilities.kind_features). `watch`/`input` are additive
 *  hub hints; `feed`/`interrupt`/`attach` are contract-v2 additions (all optional
 *  so a v3-or-earlier payload still type-checks against this shape). Mirrors
 *  `shed_core::rc::RcKindFeatures`. */
export type RcKindFeatures = {
  post_input: boolean;
  approvals: string;
  watch?: boolean;
  input?: string;
  feed?: string;
  interrupt?: boolean;
  attach?: string;
};
/** A shed's RC capabilities — mirrors `shed_core::rc::RcCapabilities`. */
export type RcCapabilities = {
  rc_version: number;
  kinds: RcKind[];
  agents: Record<string, RcAgentInfo>;
  features: string[];
  kind_features: Record<string, RcKindFeatures>;
};

/** How a kind's terminal affordance is reached — mirrors
 *  `RcKindFeatures::attach_kind()` on the Rust side, the single discriminator a
 *  client uses to decide whether `>_ open` (tmux attach) makes sense for a row.
 *  Absent/empty `attach` (an old binary, or capabilities that predate contract
 *  v2) falls back to `"tmux"` — the pre-v2 assumption every terminal path made
 *  before roost rows existed. An unrecognized non-empty value passes through
 *  verbatim rather than being coerced, so a future kind doesn't silently regain
 *  an attach button it never earned. */
export function attachKind(caps: RcCapabilities | undefined, kind: string): "tmux" | "native-remote" | "none" | string {
  return caps?.kind_features?.[kind]?.attach || "tmux";
}

/** Look up the capabilities behind a session row, keyed the same way
 *  `rc_list_payload` keys `RcListResult.capabilities`: a shed row by
 *  `host/shed`, a machine row by its `origin` (`machine:<name>`). Falls back to
 *  the `host/shed` composite when `origin` is absent (an older payload), the
 *  same fallback `sessionKey` uses. */
export function capabilitiesFor(list: Pick<RcListResult, "capabilities">, s: RcSession): RcCapabilities | undefined {
  return list.capabilities[s.origin ?? `${s.host}/${s.shed}`];
}

/** The kinds a create form can offer (broker is URL-driven; unknown never creatable).
 *  Mirrors `RcKind::creatable`. `grok` is creatable and LANE-LESS by design — it
 *  gets a status row and no transcript; a `grok` tab that binds its remote lane
 *  is reported as `gx` on the next snapshot and gains one. */
export const RC_CREATABLE_KINDS: RcKind[] =
  ["claude-rc", "codex", "opencode", "cursor", "gx", "grok", "shell"];

/** The tool token a kind's agent maps to under capabilities.agents (undefined = no
 *  agent, e.g. shell). Mirrors `RcKind::tool`. */
const RC_KIND_TOOL: Record<string, string | undefined> = {
  "claude-rc": "claude", "claude-broker": "claude",
  codex: "codex", opencode: "opencode", cursor: "cursor", shell: undefined,
  // Both roost-only kinds carry their own tool token, and `roost_capabilities`
  // reports both installed — the same trade-off it already makes for the four.
  gx: "gx", grok: "grok",
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
    case "gx": return "run `gx` and complete login";
    case "grok": return "run `grok` and complete login";
    default: return "log in to the agent in a terminal";
  }
}

/** An approval request row (contract v2). Mirrors `shed_core::rc::RcFeedApproval`. */
export type RcFeedApproval = {
  id: string;
  status: string;
  decision?: string | null;
  decisions?: string[];
};

/** A remote-control session, as shed-app serializes it (the pane's fields). The
 *  table/wire identity is the computed `host/shed/slug`, not encoded. `lane`,
 *  the activity trio, and `pending_approvals` are contract-v2 additions
 *  (optional so a v3-or-earlier payload still type-checks). */
export type RcSession = {
  host: string;
  shed: string;
  slug: string;
  /** Optional because a roost-sourced machine row carries `""` here rather than
   *  omitting the field, but a stricter type would still be wrong to assume for
   *  every future payload — mirrors the DTO's evolution, not a real absence. */
  tmux_session?: string;
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
  lane?: string | null;
  activity?: string | null;
  activity_at?: string | null;
  last_message?: string | null;
  pending_approvals?: RcFeedApproval[] | null;
  /** Where this session was reached FROM — `machine:<name>` or `<host>/<shed>`.
   *  Injected client-side by the backend (like `host`/`shed` already are), never
   *  a wire field, so the hub contract and shed-mobile's DTOs are untouched.
   *
   *  **This is the row's identity.** Do NOT key on `shed`: a hub read directly
   *  reports `shed: ""` on every session, so two machines sharing a slug would
   *  collide (see `ActivityOverlay`'s `(shed, slug)` keying). Optional so a
   *  payload from an older backend still type-checks. */
  origin?: string | null;
  origin_kind?: "shed" | "machine" | null;
  /** The machine name for a machine session; absent for a shed session. */
  machine?: string | null;
  /** The feed behind this row is not currently live — the last known state is
   *  being shown. A machine that is asleep or off-network is NORMAL, not an
   *  error, so its rows stay visible and dimmed rather than vanishing. */
  stale?: boolean | null;
  /** roost's sticky `has_notification` bit (plan 013 §3.2/§3.4), stamped
   *  client-side like `origin`/`machine` — never present on a shed row. It is
   *  its OWN affordance (a dot), not folded into `needsYou`: roost only clears
   *  it on UI focus and shed never clears it, so treating it as "needs you"
   *  would leave a card stuck demanding attention forever. */
  attention?: boolean;
  /** The roost tab id backing a machine row (a string — roost's own ids are
   *  strings on the wire); absent for a shed session. */
  tab_id?: string;
  /** The agent lane behind this row, if it has one (plan 015 §3.4). Stamped
   *  client-side like `origin`/`machine`, from what the tab's adapter reported.
   *
   *  **Its PRESENCE is the capability signal** — a row that has it can open a
   *  live transcript (`lane.*`), a row that does not is status-only, and the
   *  Transcript affordance is gated on exactly this. Absent on every shed row
   *  and on any machine row whose agent reported no server.
   *
   *  Note the name: `agent_lane`, NOT `lane`. `lane` above is the RC hub's lane
   *  token and means something else entirely on the same object. */
  agent_lane?: AgentLane | null;
};

/** What `RcSession.agent_lane` carries: which agent, which session of it, and
 *  the loopback URL of the server that session is running on (validated
 *  loopback-only, on both paths, by `RoostSession::agent_lane`). The URL is the
 *  MACHINE's loopback, not this host's — the backend forwards to it when the
 *  machine is remote, so nothing in the UI should ever dial it directly.
 *
 *  Its PRESENCE is the capability signal: a row that has it gets a Transcript
 *  affordance, a row that does not gets none. */
export type AgentLane = {
  /** Which adapter speaks to it — `"opencode"` or `"gx"` today. The backend
   *  dispatches on this string and refuses one it has no adapter for by name
   *  (`unsupported_lane`), so a kind stamped by a newer core than the binary
   *  reading it is a visible refusal rather than a silent blank panel. */
  kind: string;
  /** The AGENT's own session id — the address every `lane.*` op takes, and not
   *  the roost tab id (`tab_id` / `slug`). */
  session_id: string;
  server_url: string;
};

/** A configured machine's health, for the sessions view's group rows. A machine
 *  with no sessions AND no reachability is still worth a row — that row IS the
 *  information ("mini3 is asleep"). */
export type MachineStatus = {
  name: string;
  origin: string;
  reachable: boolean;
  /** Whether a snapshot has EVER arrived — distinguishes "still connecting"
   *  from "connected, and genuinely has no sessions". */
  connected_once: boolean;
  sessions: number;
  /** Why it is unreachable, verbatim from the watcher: "no route to host" and
   *  "nothing is listening on 1029" are different problems. */
  detail?: string | null;
};

/** The `rc.list` result: live sessions (shed AND machine), the per-shed
 *  capabilities captured during the probe (keyed by `host/shed`), and each
 *  configured machine's health. */
export type RcListResult = {
  sessions: RcSession[];
  capabilities: Record<string, RcCapabilities>;
  machines: MachineStatus[];
};

/** A session's stable row identity. Uses `origin` where the backend supplied it
 *  and falls back to the legacy `host/shed` composite, so a mixed payload (or an
 *  older backend) still produces unique keys. */
export function sessionKey(s: RcSession): string {
  return `${s.origin ?? `${s.host}/${s.shed}`}/${s.slug}`;
}

/** The live RC sessions + capabilities across running sheds (the same data the
 *  `rc.list` op serves the harness). Best-effort — empty in a browser / on error. */
export async function fetchRcList(host?: string, shed?: string): Promise<RcListResult> {
  const r = await invoke<RcListResult>("rc_list", { host, shed });
  return {
    sessions: r?.sessions ?? [],
    capabilities: r?.capabilities ?? {},
    machines: r?.machines ?? [],
  };
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

export type MachineLaunchFields = {
  machine: string;
  kind: RcKind;
  displayName?: string;
  workdir?: string;
  permissionMode?: string;
  initialPrompt?: string;
};

/** Launch a session ON a machine. THROWS on error, like [rcLaunch].
 *
 *  Separate from `rcLaunch` for the same reason `machineKill` is separate from
 *  `rcKill`: a machine is addressed by name over its own SSH, not by
 *  `(host, shed)` through a server. Everything downstream — the flag set, the
 *  permission-mode gate, how a kickoff prompt is delivered — is the SAME shared
 *  builder, so the two creates differ in where they land and nothing else. */
export async function machineLaunch(fields: MachineLaunchFields): Promise<RcSession> {
  const core = await import("@tauri-apps/api/core");
  return core.invoke<RcSession>("machine_launch", fields);
}

/** One machine's RC capabilities, or null when its engine is too old to say.
 *  Probed on demand by the launch dialog — a machine that was asleep at startup
 *  must not be stuck with whatever the first probe found. THROWS if the machine
 *  cannot be reached, which is what the dialog reports. */
export async function machineCapabilities(machine: string): Promise<RcCapabilities | null> {
  const core = await import("@tauri-apps/api/core");
  const out = await core.invoke<{ capabilities: RcCapabilities | null }>(
    "machine_capabilities",
    { machine },
  );
  return out?.capabilities ?? null;
}

/** Kill an RC session. THROWS on error (the pane surfaces it). */
export async function rcKill(shed: string, slug: string, host?: string): Promise<void> {
  const core = await import("@tauri-apps/api/core");
  await core.invoke("rc_kill", { shed, slug, host });
}

/** Kill a session on a MACHINE. THROWS on error, like `rcKill`.
 *
 *  Separate from `rcKill` because the two reach paths genuinely differ here and
 *  nowhere else: a shed session is addressed by `(host, shed, slug)` through the
 *  server's SSH endpoint, a machine session by `(machine, slug)` over the
 *  machine's own SSH. Collapsing them behind one signature would mean passing an
 *  empty `shed` and letting the backend guess. */
export async function rcKillMachine(machine: string, slug: string): Promise<void> {
  const core = await import("@tauri-apps/api/core");
  await core.invoke("machine_kill", { machine, slug });
}

/** Kill a session, routing by its origin. The single entry point a card uses, so
 *  the machine/shed distinction lives in ONE place instead of at every call. */
export async function killSession(s: RcSession): Promise<void> {
  if (s.origin_kind === "machine" && s.machine) {
    return rcKillMachine(s.machine, s.slug);
  }
  return rcKill(s.shed, s.slug, s.host);
}

/** Report the rendered RC sessions so the `agents.dump` op can observe them — the
 *  drivable truth of the Agents pane, like `dashboard.dump` reads the sheds.
 *  (`ui_report` merges this `agents` key with the shell's snapshot.) */
export function reportAgents(sessions: RcSession[]): void {
  void invoke("ui_report", { snapshot: { agents: sessions } });
}

/** One machine as the Machines pane renders it: the health line a person reads,
 *  plus the slugs grouped beneath it. `status` is the CHIP TEXT, not the raw
 *  boolean — "connecting" and "unreachable" are different claims and a test that
 *  asserts the boolean cannot tell them apart. */
export type MachinePaneRow = {
  name: string;
  origin: string;
  reachable: boolean;
  status: string;
  detail: string;
  sessions: string[];
};

/** Report the rendered Machines pane (mounted-only, like `reportEgress` — pass
 *  `null` on unmount to CLEAR the key rather than leave a stale snapshot). Read
 *  by the `machines.dump` op, which is deliberately distinct from
 *  `machines.list`: the latter is the backend's view and can be perfect while
 *  nothing reaches the window. */
export function reportMachinesPane(rows: MachinePaneRow[] | null): void {
  void invoke("ui_report", { snapshot: { machines_pane: rows } });
}

/** The SINGLE source of truth for RC sessions + per-shed capabilities, lifted to the
 *  App so the sidebar badge (`sessions.length`), the Agents pane's list, and the
 *  launch dialog's capability gating all read ONE `rc.list` fetch — no divergence
 *  between independent fetches with different triggers. Refreshed on mount, on each
 *  lifecycle `refresh` event (sheds coming up/down), and whenever a caller (the pane
 *  Refresh button, a launch/kill, the pane/dialog on open) invokes the returned
 *  `refresh`. Empty in a plain browser / on error. */
export function useRcSessions(): {
  sessions: RcSession[];
  capabilities: Record<string, RcCapabilities>;
  machines: MachineStatus[];
  refresh: () => void;
} {
  const [state, setState] = useState<RcListResult>({
    sessions: [],
    capabilities: {},
    machines: [],
  });
  // A generation guard shared across the mount-, event-, and caller-driven reloads,
  // so a slower older fetch can't overwrite a newer one (the pane's superseded-fetch
  // guard, now owned here for the shared state).
  const gen = useRef(0);
  const refresh = useCallback(() => {
    if (!inTauri()) return;
    const mine = ++gen.current;
    void fetchRcList().then((r) => {
      if (mine === gen.current) setState(r);
    });
  }, []);
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let unlisten: (() => void) | undefined;
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const un = await listen("refresh", () => refresh());
      if (cancelled) un();
      else unlisten = un;
      refresh(); // initial fetch, after the listener is live
    })();
    return () => {
      cancelled = true;
      unlisten?.();
    };
  }, [refresh]);
  return {
    sessions: state.sessions,
    capabilities: state.capabilities,
    machines: state.machines,
    refresh,
  };
}

/* ---- agent lanes (plan 015 §3.4) ------------------------------------------ */

/** One transcript row, as `lane.messages` serializes `shed_core::rc::RcFeedMessage`
 *  — the SAME rows the RC feed renders, so a lane transcript and an RC transcript
 *  are one widget. `type` is the wire spelling (`text` | `reasoning` | `tool_use` |
 *  `tool_result` | `status` | `approval_request`). */
export type LaneMessage = {
  seq: number;
  ts?: string;
  role: string;
  type: string;
  text?: string;
  tool?: { name?: string; detail?: string } | null;
  approval?: RcFeedApproval | null;
};

/** The staged-then-swapped view `lane.messages` answers with. `generation` is
 *  the generation of the rows being handed back (it moves when a reseed
 *  COMPLETES), and `stale` is the reason a `Down` gave — non-null means the last
 *  good generation is still on screen and the feed behind it is not live. That is
 *  the contract's "consume" posture: keep rendering, say so, don't blank. */
export type LaneView = {
  messages: LaneMessage[];
  activity: string;
  generation: number;
  stale: string | null;
};

/** One button an approval offers, exactly as the agent offered it.
 *
 *  `id` is OPAQUE — whatever the agent called it — and is what `{choice}` hands
 *  back. `kind` is the separate, semantic half (the ACP vocabulary:
 *  `allow_once` | `allow_always` | `reject_once` | `reject_always`), absent when
 *  the agent states none. The two are independent, which is the whole reason
 *  both exist: gx's own permission offers `optionId: "enable-always-approve"`
 *  with `kind: "allow_once"`, so a client that styled buttons by sniffing the id
 *  would paint that one wrong. Style by `kind`, post by `id`. */
export type LaneOption = {
  id: string;
  label: string;
  description?: string | null;
  kind?: string | null;
};

/** One structured question inside a `question` approval: its own options, plus
 *  `multiple` (several ids in one answer) and `custom` (free text alongside). */
export type LaneQuestion = {
  header: string;
  question: string;
  options: LaneOption[];
  multiple: boolean;
  custom: boolean;
};

/** One thing waiting on the human.
 *
 *  **`kind` selects which field renders, and the two are never both populated**
 *  (pinned in `shed_core::lane::LaneApproval`'s doc): a `permission` and a
 *  `plan_approval` fill `options` and leave `questions` empty; a `question`
 *  fills `questions` and leaves `options` empty. Branch on `kind`, never on
 *  which list happens to be non-empty — rendering the wrong one yields an
 *  approval card with no buttons.
 *
 *  `options` is the AGENT's own menu, in its own order, and its length is not
 *  fixed: opencode offers three, gx offers whatever the request carried (five,
 *  live, two of them `allow_once`). A kind this build cannot name — including
 *  gx's `pending_interaction` placeholder, whose `method` and `request` are both
 *  null and which therefore offers nothing — renders `request_json` instead. */
export type LaneApproval = {
  id: string;
  /** May be a DESCENDANT of the subscribed session — a child's approval still
   *  blocks the same agent, so it surfaces on the root's panel. */
  session_id: string;
  kind: string;
  status: string;
  title: string;
  detail?: string | null;
  options: LaneOption[];
  questions: LaneQuestion[];
  request_json: string;
  created_at_unix_ms?: number | null;
};

/** The agent session behind a lane (`shed_core::lane::LaneSession`). */
export type LaneSessionRow = {
  id: string;
  title: string;
  cwd: string;
  activity: string;
  pending_approvals: number;
  approximate: boolean;
  parent_id?: string | null;
  last_change_unix_ms?: number | null;
};

/** What the adapter behind a lane can do (`lane.open`'s second half). */
export type LaneCapabilities = {
  kind: string;
  interject: boolean;
  create: boolean;
  cancel: boolean;
  approvals: boolean;
  history_cursor: boolean;
};

export type LaneOpened = { session: LaneSessionRow; capabilities: LaneCapabilities };

/** The four forms `lane.answer` accepts (`lane::parse_answer`). Exactly one key
 *  per answer — naming two is a `bad_request`.
 *
 *  `choice` names the offered option by its OWN id, verbatim: ids are opaque and
 *  the backend neither trims nor interprets them. It is the only form that can
 *  express a real menu — gx offers five permission options with two of kind
 *  `allow_once`, so `permission` cannot say which one was pressed. `permission`
 *  resolves by SEMANTIC kind instead, and note its KEBAB spellings — the IPC
 *  grammar's, not the option ids' (`allow_once`).
 *
 *  `custom_text` is a MODIFIER of `question`, not a fifth form: one entry per
 *  question, positional like `question`, `null` where nothing was typed. Beside
 *  any other form it is a `bad_request` — which is why the union spells it only
 *  on that member, so a panel cannot type-check its way into a silent drop. The
 *  backend trims it and refuses text aimed at a question whose `custom` is
 *  false. */
export type LaneAnswer =
  | { choice: string }
  | { permission: "allow-once" | "allow-always" | "reject" }
  | { question: string[][]; custom_text?: (string | null)[] }
  | { reject: true };

/** `{machine, session_id, event}` — the Tauri `lane-event` payload. The panel
 *  only reads the address (it re-reads the staged view rather than folding
 *  frames itself, so what it renders is the same truth `lane.messages` answers
 *  with), which is why `event` stays `unknown`. */
export type LaneEventEnvelope = { machine?: unknown; session_id?: unknown; event?: unknown };

/** The Tauri event every lane frame arrives on (`lane::LANE_EVENT`). */
export const LANE_EVENT = "lane-event";

/** A `lane.*` failure with the contract's snake_case code recovered.
 *
 *  A `#[tauri::command]`'s error channel is a bare string, so `lib.rs` sends
 *  `"<code>: <message>"` and this is the other half of that agreement (the IPC
 *  socket keeps the structured envelope; both doors land in the same `Lanes`). */
export class LaneFailure extends Error {
  readonly code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = "LaneFailure";
    this.code = code;
  }
}

/** A lane error code as `LaneFailure::code` spells them — snake_case, no spaces.
 *  Anything else means the string is not a coded failure (a failed `invoke`, a
 *  browser with no Tauri) and the whole text is the message. */
const LANE_CODE = /^[a-z][a-z_]*$/;

export function laneFailure(e: unknown): LaneFailure {
  const raw = typeof e === "string" ? e : e instanceof Error ? e.message : String(e);
  const cut = raw.indexOf(": ");
  const code = cut > 0 ? raw.slice(0, cut) : "";
  return LANE_CODE.test(code)
    ? new LaneFailure(code, raw.slice(cut + 2))
    : new LaneFailure("failed", raw);
}

/** Invoke a lane command, THROWING a coded [LaneFailure] (unlike the swallowing
 *  `invoke` the shell uses): every lane verb is a user action whose refusal —
 *  `already_resolved`, `not_accepting`, `unauthorized` — is exactly what the
 *  panel has to show. */
async function laneInvoke<T>(cmd: string, args: Record<string, unknown>): Promise<T> {
  const core = await import("@tauri-apps/api/core");
  try {
    return await core.invoke<T>(cmd, args);
  } catch (e) {
    throw laneFailure(e);
  }
}

// NB Tauri v2 looks invoke args up in camelCase (the Rust `session_id` param →
// the key `sessionId`), so these must be sent camelCase — the `createId` rule.

/** Start (or re-answer) a live transcript. Idempotent: a second call for an
 *  already-open lane re-answers from the entry, it does not open a second
 *  subscription. */
export async function laneOpen(machine: string, sessionId: string): Promise<LaneOpened> {
  return laneInvoke<LaneOpened>("lane_open", { machine, sessionId });
}

export async function laneMessages(machine: string, sessionId: string): Promise<LaneView> {
  return laneInvoke<LaneView>("lane_messages", { machine, sessionId });
}

export async function laneApprovals(machine: string, sessionId: string): Promise<LaneApproval[]> {
  const r = await laneInvoke<{ approvals?: LaneApproval[] }>("lane_approvals", { machine, sessionId });
  return r.approvals ?? [];
}

/** Send a prompt. `mode` defaults to `queue`; `interject` is REFUSED by the
 *  opencode adapter (`capabilities.interject` is false) rather than silently
 *  downgraded, so a caller that passes it gets `not_accepting`. */
export async function laneSend(machine: string, sessionId: string, text: string, mode?: string): Promise<void> {
  await laneInvoke("lane_send", { machine, sessionId, text, mode });
}

export async function laneCancel(machine: string, sessionId: string): Promise<void> {
  await laneInvoke("lane_cancel", { machine, sessionId });
}

export async function laneAnswer(
  machine: string,
  sessionId: string,
  approvalId: string,
  answer: LaneAnswer,
): Promise<void> {
  await laneInvoke("lane_answer", { machine, sessionId, approvalId, answer });
}

/** End the subscription and release the transport. Idempotent, and deliberately
 *  BEST-EFFORT: this runs from the panel's unmount cleanup, where a throw would
 *  escape into React and where an unmount racing an eviction (the tab went away)
 *  is normal, not an error. */
export async function laneClose(machine: string, sessionId: string): Promise<void> {
  try {
    await laneInvoke("lane_close", { machine, sessionId });
  } catch {
    /* the lane is already gone — which is what close asked for */
  }
}

/** One transcript row AS RENDERED — not the wire row. `text` is the string on
 *  screen, `tool` the `name · detail` line a tool row shows, and the two flags
 *  are the treatments the plan pins (a `status` row is muted, reasoning is
 *  collapsed). */
export type LaneRow = {
  seq: number;
  role: string;
  type: string;
  text: string;
  tool: string | null;
  muted: boolean;
  collapsed: boolean;
};

/** One approval card as rendered: `buttons` are the decision buttons' LABELS in
 *  render order (the agent's own options, however many it offered), `options`
 *  the same buttons with the id each one POSTS and the kind each is styled from,
 *  and `questions` the structured form a question renders instead — its
 *  `options` being the option buttons' labels and `custom` whether a free-text
 *  field sits beside them. Which is populated follows `kind`, exactly as the
 *  render does.
 *
 *  `options` beside `buttons` is not redundant. A label is what a person reads;
 *  an id is what the click sends, and gx proves they can disagree (two options
 *  of kind `allow_once`, distinguishable only by id). A dump that reported only
 *  labels could not tell a right button from a wrong one. */
export type LaneApprovalCard = {
  id: string;
  session_id: string;
  kind: string;
  title: string;
  detail: string;
  buttons: string[];
  options: { id: string; label: string; kind: string | null }[];
  questions: { header: string; question: string; options: string[]; custom: boolean }[];
};

/** The transcript panel's rendered state — the UI truth `lane.dump` reads.
 *
 *  `lane.messages` is the BACKEND's staged view; this is what a person is
 *  actually looking at, which is a different claim and the one a screenshot
 *  would otherwise be the only evidence for. */
export type LaneReport = {
  machine: string;
  session_id: string;
  /** Which adapter is behind this panel (`opened.capabilities.kind`), and what
   *  the header badge says. `""` until `lane.open` answers. */
  kind: string;
  title: string;
  cwd: string;
  activity: string;
  generation: number;
  /** The stale banner's reason, or null while the lane is live. */
  stale: string | null;
  rows: LaneRow[];
  approvals: LaneApprovalCard[];
  /** The Cancel button is enabled — i.e. the session is Working. */
  can_cancel: boolean;
  /** The Interject toggle, or `null` when the adapter does not advertise the
   *  capability and no toggle is rendered at all.
   *
   *  Three states rather than a bool because "absent" and "present but
   *  disabled" are different claims and both are pinned: opencode advertises no
   *  `interject`, so its panel has no toggle; gx advertises it, so its panel
   *  has one — enabled only while the session is Working, because that is the
   *  only time the agent accepts one. */
  interject: { on: boolean; enabled: boolean } | null;
  error: string | null;
};

/** Report the transcript panel's rendered state (mounted-only, like
 *  `reportEgress`). Pass `null` on unmount to CLEAR the key — the merge
 *  overwrites `lane` with null rather than skipping it, so `lane.dump` answers
 *  `null` for "no panel" instead of the previous mount's stale snapshot. */
export function reportLane(snapshot: LaneReport | null): void {
  void invoke("ui_report", { snapshot: { lane: snapshot } });
}

/* ---- menu-bar popover (B1b) ------------------------------------------------ */

/** The live sheds + per-host failures (the same `list_sheds` the dashboard +
 *  `sheds.list` use). */
export async function fetchShedsPayload(): Promise<ShedsPayload> {
  const p = await invoke<ShedsPayload>("list_sheds");
  return { sheds: p?.sheds ?? [], host_errors: p?.host_errors ?? [] };
}

/** Just the rows, for callers that render no failure surface (the tray popover's
 *  running-shed list — the dashboard is where a failed host is explained). */
export async function fetchSheds(): Promise<Shed[]> {
  return (await fetchShedsPayload()).sheds;
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

/** The Sparkle updater's status the popover "Check for Updates…" row reads on
 *  render: `{ os, enabled, reason }` (enabled == reason=="ok"). Disabled reasons
 *  drive a truthful tooltip (test_mode / no_bundle / linux_apt). Swallows errors to
 *  undefined (the row then degrades to disabled). */
export type UpdaterStatus = {
  os: "macos" | "linux";
  enabled: boolean;
  reason: "ok" | "test_mode" | "no_bundle" | "linux_apt";
  /** Whether a real Sparkle updater was instantiated (macOS bundle only; always false
   *  off macOS AND under the harness — the drivable proof test mode never made one). */
  instantiated: boolean;
};
export async function updaterStatus(): Promise<UpdaterStatus | undefined> {
  return invoke<UpdaterStatus>("updater_status");
}
/** A user-invoked update check (the enabled row's click). Fire-and-forget — on the
 *  enabled path Sparkle presents its own dialog; a disabled/failed call is logged,
 *  not surfaced (the row is only clickable when status.enabled). */
export async function updaterCheck(): Promise<void> {
  const core = await import("@tauri-apps/api/core");
  await core.invoke("updater_check").catch((e) => console.error("updater_check failed", e));
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
