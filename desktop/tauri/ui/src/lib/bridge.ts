import { useCallback, useEffect, useRef, useState } from "react";

import type { AgentsEmptyState, AgentsLoad } from "@/lib/agentsEmpty";
import type { CrazeCreateOptions } from "@/lib/crazeCreate";
import type { CrazeSavedList } from "@/lib/crazeResume";
import { initialReport } from "@/lib/report";
import { pairRowsWithCards } from "@/lib/shedStatus";
import type { SourceMachine, SourceRow, SourceStatus } from "@/lib/sources";

export type Pane = "sheds" | "agents" | "credentials" | "activity" | "system";

/** Which modal (if any) is open — reported so the harness can drive + assert it.
 *  (Preferences is a dedicated window, not a modal — see `reportPrefs`.) */
export type Modal = null | "create" | "craze";

const PANES: readonly Pane[] = ["sheds", "agents", "credentials", "activity", "system"];

/** Narrow an untrusted value (an IPC payload) to a known pane. */
export function isPane(x: unknown): x is Pane {
  return typeof x === "string" && (PANES as readonly string[]).includes(x);
}

/** A shed as shed-core serializes it — the fields the dashboard reads (more exist). */
export type Shed = {
  name: string;
  host: string;
  status: string;
  /** Why the shed is in `status` (a remote backend code such as `capacity`), or
   *  null — shown beside the status by `statusText` (`lib/shedStatus`). */
  status_reason?: string | null;
  backend?: string | null;
  image?: string | null;
  cpus?: number | null;
  memory_mb?: number | null;
  /** The shed's delivery record (plan 039 §3.6) as its server sent it, or null from
   *  a server older than delivery. Read with `readDelivery` (`lib/delivery`). */
  delivery?: unknown;
  /** When this incarnation of the shed was created, verbatim: what names it to the
   *  egress feed (`egress.recent`). */
  created_at?: string | null;
};

/** Why a host couldn't be listed. `other` is the open set (rendered generically). */
export type HostFailureKind = "other";

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

/** The sidebar nav badge counts (sheds / active RC sessions / hosts), carried in
 *  the shell's full snapshot so the harness can assert the rendered counts as UI
 *  truth via `ui.badges` — not pixels. */
export type Badges = { sheds: number; agents: number; hosts: number };

/** What the shell knows about the sidebar's two sections before the host
 *  failures are folded in (which `report` does, since it holds them). */
export type SidebarInput = {
  servers: { name: string; reachable: boolean }[];
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
  /** The nav entries as rendered, in order: their pane and their label. */
  entries: { pane: string; label: string }[];
  servers: { name: string; reachable: boolean; detail: string }[];
};

/** The failure for one host, if any — the SINGLE lookup shared by the sidebar's
 *  rendered tooltip and the reported `detail`, so the two cannot drift into
 *  disagreeing about whether a host has a reason attached. */
export function hostFailureFor(hostErrors: HostFailure[], host: string): HostFailure | undefined {
  return hostErrors.find((e) => e.server === host);
}

/** Everything one full report is built from: what the shell rendered, before
 *  `report` derives the send-time parts (the sampled style, each server's failure
 *  `detail`, the pane-keyed empty state). `useUiBridge` keeps the latest one it
 *  built, so its initial report can re-send it (`initialReport`, shed#400). */
type ReportPayload = {
  pane: Pane;
  sheds: Shed[];
  refreshToken: number;
  modal: Modal;
  badges: Badges;
  sidebar: SidebarInput;
  mode?: string;
  hostErrors: HostFailure[];
};

/** What one shed's card on the Sheds pane rendered, read back off its own nodes
 *  — the `renderedText` rule, so `dashboard.dump` says what the row shows, not
 *  what `statusText`/`isBusy` would say. Both are null when no card for the shed
 *  is mounted (another pane is up).
 *
 *  `status_text` is the meta line's status part (`stopping`, `stopped
 *  (capacity)`); `controls` is `{spinner, buttons}` — whether the busy spinner
 *  shows, and the titles of the action buttons present (`Start`, `Delete`,
 *  `Stop`, `Restart`, `Open in Terminal`). */
export type ShedRowRendered = {
  status_text: string | null;
  controls: { spinner: boolean; buttons: string[] } | null;
  /** The delivery badge's text (plan 039 A7), or null when the row shows none. */
  delivery_badge: string | null;
};

/** The reported shed rows: each row with its [ShedRowRendered]. A card is found
 *  by its `data-shed-host` + `data-shed-name` pair (no selector is built from a
 *  server name); rows that share both are paired with their cards in order, and
 *  get null when the counts differ (`pairRowsWithCards`), so no row reports
 *  another card's evidence. Call at send time, after the commit. */
function withRenderedRows(sheds: Shed[]): (Shed & ShedRowRendered)[] {
  const cards = typeof document === "undefined" ? [] : Array.from(document.querySelectorAll("[data-shed-row]"));
  const paired = pairRowsWithCards(sheds, cards, (el) => {
    const host = el.getAttribute("data-shed-host");
    const name = el.getAttribute("data-shed-name");
    return host === null || name === null ? null : { host, name };
  });
  return sheds.map((s, i) => {
    const card = paired[i];
    const controls = card?.querySelector("[data-shed-controls]");
    return {
      ...s,
      status_text: card?.querySelector("[data-shed-status]")?.textContent ?? null,
      delivery_badge: card?.querySelector("[data-shed-delivery-badge]")?.textContent ?? null,
      controls: controls
        ? {
            spinner: !!controls.querySelector(".animate-spin"),
            buttons: Array.from(controls.querySelectorAll("button"))
              .map((b) => b.title)
              .filter((t) => t !== ""),
          }
        : null,
    };
  });
}

/** Report the rendered snapshot Rust relays to the harness (`ui.current_pane` /
 *  `ui.computed_style` / `dashboard.dump` / `ui.badges`). ALWAYS a FULL blob — every
 *  key present — so no partial report can (a) create the `main` slot before the shell
 *  is ready and false-signal readiness to `sheds.refresh`, or (b) omit `sheds` and
 *  zero the mac tray running-count (lib.rs recomputes it from each report). A new
 *  reader is one more key. The refresh token is echoed so `sheds.refresh` blocks on it. */
function report({ pane, sheds, refreshToken, modal, badges, sidebar, mode, hostErrors }: ReportPayload) {
  void invoke("ui_report", {
    snapshot: {
      pane,
      style: sampleStyle(mode),
      // Each row as shed-core sent it, plus what its card RENDERED (read off
      // the mounted row at send time): `status_text` and `controls`.
      sheds: withRenderedRows(sheds),
      refresh_token: refreshToken,
      modal,
      badges,
      // The sidebar's status foot. `detail` is the host's failure summary — the
      // hover text — so a test can prove the reason is threaded to the row and
      // not merely that the row exists.
      sidebar: {
        entries: Array.from(document.querySelectorAll<HTMLElement>("[data-nav]")).map((el) => ({
          pane: el.dataset.nav ?? "",
          label: el.querySelector("[data-nav-label]")?.textContent ?? "",
        })),
        servers: sidebar.servers.map((sv) => ({
          ...sv,
          detail: hostFailureFor(hostErrors, sv.name)?.summary ?? "",
        })),
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
 *  This owns the SINGLE report path — the shell's badges (`agentCount`/`hostCount`
 *  supplied by App, `sheds` derived here) and the active `mode` fold into
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
  // The latest complete report the report effect built — what the listener
  // effect's initial report re-sends (with the current pane) instead of a seed
  // that would undo it (shed#400). Null until the first one is built.
  const latestReport = useRef<ReportPayload | null>(null);
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
      // Fresh sheds arrive via the report effect on the next render; this initial
      // report publishes the pane, so `current_pane != null` = "listeners live".
      // It re-sends the latest report the effect built — which, while that first
      // list was in flight, may already carry the real badges and sidebar (the
      // host list answers on its own) — and seeds empty ONLY when none was built.
      // A seed sent after a real report replaced its every key: the Hosts badge
      // and the sidebar read empty until the next render (shed#400).
      report(initialReport(latestReport.current, paneRef.current));
    })();
    return () => {
      cancelled = true;
      ready.current = false;
      unlisten.forEach((u) => u());
    };
  }, [setPane, fetchSheds]);

  // Auto-refresh the shed list on a cadence so an externally-created shed (e.g.
  // `shed create` from the CLI, or another client) surfaces on the ALREADY-OPEN
  // dashboard without a manual action (every 5s). Reuses the SAME
  // `fetchSheds` path the `refresh` event uses — no second fetch mechanism.
  //
  // JS-side by design (why-only): no Rust ticker / IPC-event plumbing is needed —
  // @tauri-apps/api/window is enough to observe visibility from JS. We poll ONLY
  // while the window is actually shown — the webview is HIDDEN (not unmounted)
  // when the window is closed to the tray, so polling then would waste battery +
  // network.
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
    const badges: Badges = { sheds: sheds.length, agents: agentCount, hosts: hostCount };
    const payload: ReportPayload = { pane, sheds, refreshToken, modal, badges, sidebar, mode, hostErrors };
    latestReport.current = payload;
    report(payload);
    // `sidebar` is rebuilt by the caller each render, so it is compared by VALUE
    // (a reference dep would re-report on every render — the flap this whole
    // shape exists to avoid).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pane, sheds, hostErrors, refreshToken, modal, mode, agentCount, hostCount, JSON.stringify(sidebar)]);

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

/** The same `system_df` read for the shell's host list (the Hosts badge and the
 *  sidebar's servers), except that a failed invoke REJECTS rather than reading
 *  as "no hosts": the list's reload keeps its last rows on a failure
 *  (`newestReload`, shed#400), which it can only do if it can tell one. */
export async function fetchHostRows(): Promise<HostDiskUsage[]> {
  const core = await import("@tauri-apps/api/core");
  return (await core.invoke<HostDiskUsage[] | null>("system_df")) ?? [];
}

/* ---- egress: server profiles (System) and a shed's recent blocks ---------- */

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

/** Every server's egress profiles, for the System pane. */
export async function fetchEgressProfiles(): Promise<HostEgressProfiles[]> {
  return (await invoke<HostEgressProfiles[]>("egress_profiles")) ?? [];
}

/** The System pane's rendered profiles, reported so the `egress.profiles` op can
 *  observe them (UI truth, like `reportAgents`): every profile row as shown, and each
 *  server that could not list its profiles. Keys are additive + stable — the harness
 *  asserts them. */
export type SystemEgressReport = {
  profiles: {
    host: string;
    name: string;
    source: string;
    mode: string | null;
    allow: string[];
    deny: string[];
    rule: string | null;
  }[];
  errors: { host: string; error: string }[];
};

/** Report the System pane's profiles (mounted-only). `null` on unmount CLEARS the key,
 *  so an off-pane `egress.profiles` answers null, never a stale snapshot. */
export function reportEgress(snapshot: SystemEgressReport | null): void {
  void invoke("ui_report", { snapshot: { egress: snapshot } });
}

/** One egress decision the app kept (`egress.recent`; every string cleaned for display
 *  by the Rust side). */
export type EgressEvent = {
  server: string;
  ts: string;
  shed: string;
  shed_created_at: string;
  host: string;
  port: number;
  resolved_ip: string;
  protocol: string;
  verdict: string;
  reason: string;
};

/** A shed incarnation's recent blocks (its kept `deny` decisions, newest first).
 *  THROWS the refusal (no creation time, the feed not started). */
export async function fetchEgressBlocks(server: string, shed: string, createdAt: string): Promise<EgressEvent[]> {
  const r = await invokeStrict<{ events?: EgressEvent[] }>("egress_recent", {
    server, shed, createdAt, blocks: true,
  });
  return r.events ?? [];
}

/** What the open shed detail's egress section rendered (`shed_egress.dump`), or null. */
export function reportShedEgress(snapshot: Record<string, unknown> | null): void {
  void invoke("ui_report", { snapshot: { shed_egress: snapshot } });
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

/* ---- credential sources (plan 035 §3.4; the Credentials view's Where) ---------- */

/* The row types and how reads, moves and stops are reconciled live in `sources.ts`, and
 * what each row says in `credentials.ts` (both pure, unit-tested); the calls live here.
 * A machine's name came from ANOTHER machine by way of the server: Rust has already
 * cleaned it, and `credentials.ts` cleans it again (a second line of defence); render
 * it only as a text node, never into HTML, a title or a tooltip. */
export type { SourceMachine, SourceRow, SourceStatus };

/** `sources.list`: who answers on each configured server (swallowing, like `invoke`:
 *  `undefined` on any failure). */
export async function listSources(): Promise<unknown> {
  return invoke<unknown>("sources_list");
}

/** "Answer from here": make this machine the one that answers on `host`, answering
 *  that host's row as the server left it after the move. THROWS the refusal as
 *  `{code, message}`; the view words it from the code. */
export async function answerFromHere(host: string): Promise<{ row: unknown; woke_source: boolean }> {
  return invokeStrict("source_use", { host });
}

/** "Stop answering from here" on `host`. THROWS the refusal as `{code, message}`. */
export async function stopAnsweringFromHere(host: string): Promise<Record<string, unknown>> {
  return invokeStrict("source_unbind", { host });
}

/* ---- provisioning by delivery (plan 039 A6) ----------------------------------- */

/* The same ops as the IPC socket's `delivery.*`, `shed.delivery`, `shed.provision` and
 * `activity.server_list` (`desktop/tauri/src-tauri/src/delivery.rs`). Nothing here
 * carries a delivered value: previews and records hold names, targets, sizes and
 * fingerprints only. Each call THROWS the refusal in plain words. */

/** One provider's settings: `config` is its `delivery.toml` table as flat JSON, the
 *  shape `setDeliveryProvider` takes back. */
export interface DeliveryProvider {
  name: string;
  configured: boolean;
  enabled: boolean;
  only: { servers?: string[]; backends?: string[]; repos?: string[] } | null;
  config: Record<string, unknown> | null;
}

export interface DeliverySettings {
  path: string;
  exists: boolean;
  /** Why the file cannot be used (fixed words, never its text), or null. */
  error: string | null;
  providers: DeliveryProvider[];
}

/** One merged activity page: events newest first, each with its `server`; `next` is the
 *  opaque cursor for the page after (null: nothing older anywhere). */
export interface ActivityPage {
  events: Array<Record<string, unknown> & { server: string; id: string; at: string; kind: string }>;
  next: string | null;
  errors: Array<{ server: string; status: number | null; code: string; message: string }>;
}

async function invokeStrict<T>(cmd: string, args?: Record<string, unknown>): Promise<T> {
  const core = await import("@tauri-apps/api/core");
  return core.invoke<T>(cmd, args);
}

/** The source runtime: this machine, each server's stream, recent answers (redacted). */
export function getDeliveryStatus(): Promise<Record<string, unknown>> {
  return invokeStrict("delivery_status");
}

export function getDeliverySettings(): Promise<DeliverySettings> {
  return invokeStrict("delivery_settings");
}

/** Replace one provider's table (`config` null removes it); answers the new settings. */
export function setDeliveryProvider(
  provider: string,
  config: Record<string, unknown> | null,
): Promise<DeliverySettings> {
  return invokeStrict("delivery_set", { provider, config });
}

/** What a shed on `server` would be delivered (the Credentials view's Check): a named
 *  shed, or a new shed on `backend` with `repo` (or none): names, targets, stages,
 *  sizes, fingerprints and problems. */
export function previewDelivery(
  server: string,
  of: { shed: string } | { backend: string; repo: string | null },
  runs: readonly string[],
): Promise<Record<string, unknown>> {
  // `runs`: the commands the person was told would run; settings that would run others
  // are refused `settings_changed`, and nothing runs. THROWS `{code, message}`.
  return "shed" in of
    ? invokeStrict("delivery_preview", { server, shed: of.shed, runs })
    : invokeStrict("delivery_preview", { server, vmBackend: of.backend, repo: of.repo, runs });
}

/** `delivery.describe`: each provider's descriptor (title, summary, where it lands,
 *  fields), in the order they are asked. */
export function describeDelivery(): Promise<unknown> {
  return invokeStrict("delivery_describe");
}

/** `delivery.recent`: the answers this machine gave, newest first, redacted. */
export function recentDeliveries(): Promise<unknown> {
  return invokeStrict("delivery_recent");
}

/** What the Credentials view rendered (`credentials.dump`), or null off the view. */
export function reportCredentials(snapshot: Record<string, unknown> | null): void {
  void invoke("ui_report", { snapshot: { credentials: snapshot } });
}

/** The delivery record of a shed's document (null for a server older than delivery),
 *  its explicit `egress_profiles` (null: none, the server's default applies), and its
 *  effective `egress` as the server resolved it (null, with `egress_error`: unread). */
export function getShedDelivery(server: string, shed: string): Promise<Record<string, unknown>> {
  return invokeStrict("shed_delivery", { server, shed });
}

/** "Deliver again": run `shed provision` on the shed and report its progress and outcome. */
export function provisionShed(server: string, shed: string): Promise<Record<string, unknown>> {
  return invokeStrict("shed_provision", { server, shed });
}

/** One page of every configured server's activity, merged newest first. */
export function listServerActivity(opts: { limit?: number; cursor?: string | null; shed?: string } = {}): Promise<ActivityPage> {
  return invokeStrict("activity_server_list", {
    limit: opts.limit ?? null,
    cursor: opts.cursor ?? null,
    shed: opts.shed ?? null,
  });
}

/** The last `ui.scroll_to` the dashboard carried out (`ui.scrolled`). */
export function reportScrolled(snapshot: { anchor: string; token: unknown; found: boolean; animations: number }): void {
  void invoke("ui_report", { snapshot: { scrolled: snapshot } });
}

/** What the Activity pane rendered (`activity.dump`), or null off the pane. */
export function reportActivity(snapshot: Record<string, unknown> | null): void {
  void invoke("ui_report", { snapshot: { activity: snapshot } });
}

/** What the open shed delivery detail rendered (`shed_delivery.dump`), or null when
 *  none is open. Built off the detail's own nodes by its reporting effect. */
export function reportShedDelivery(snapshot: Record<string, unknown> | null): void {
  void invoke("ui_report", { snapshot: { shed_delivery: snapshot } });
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
 *  the active light/dark mode. Keys are additive + stable — the harness asserts them.
 *  (Credentials, and who answers on each server, are the Credentials view's.) */
export type PrefsReport = {
  sections: string[];
  values: {
    preset: string;
    template: string;
    login: boolean;
  };
  mode: string;
};

/** Report the Preferences window's rendered state (`ui_report` keys it under the
 *  calling window's label, so it can never clobber the dashboard's `main`). */
export function reportPrefs(snapshot: PrefsReport): void {
  void invoke("ui_report", { snapshot: { prefs: snapshot } });
}

/* ---- remote-control agents (B2.4) ----------------------------------------- */

// The known kinds, plus `(string & {})` so an UNKNOWN kind from a newer/other tool
// keeps its raw string (the unknown-kind policy) instead of failing the type — it
// renders neutrally and is never offered for creation.
/** The agent kinds this build names. `claude-rc`/`opencode`/`shell` mirror the
 *  guest's tmux/RC registry. `codex`, `cursor`, `gx` and `grok` — the
 *  direct-agent and roost-only row kinds — left this build in plan 025
 *  (shed#390): a roost tab running one of them directly renders as a plain
 *  `(string & {})` row with no typed affordances. `craze` is the one
 *  addition — the lane craze provides for every other agent (plan 025, D1);
 *  it is not creatable through roost's own launch form (craze's own create
 *  sheet is that surface). */
export type RcKind =
  | "claude-rc" | "opencode" | "shell"
  | "craze"
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
 *
 *  **`"none"` when nothing says otherwise** (S6, `charliek/shed#328`). This used
 *  to fall back to `"tmux"` — the pre-v2 assumption every terminal path made
 *  before roost rows existed, when a shed row really did have a tmux pane behind
 *  it. Every row is a roost tab now, and roost's own synthesized contract says
 *  `native-remote`, so an absent entry means "this build has nothing to say about
 *  this kind", and offering an attach on the strength of that would open onto
 *  nothing. An unrecognized non-empty value still passes through verbatim, so a
 *  future kind doesn't silently gain an attach button it never earned. */
export function attachKind(caps: RcCapabilities | undefined, kind: string): "tmux" | "native-remote" | "none" | string {
  return caps?.kind_features?.[kind]?.attach || "none";
}

/** Look up the capabilities behind a session row, keyed exactly the way
 *  `rc_list_payload` keys `RcListResult.capabilities`: by the row's `origin`
 *  (`machine:<name>` or `roost:<server>/<shed>`).
 *
 *  The `host/shed` fallback went with the hub (S6): that composite was the HUB's
 *  key, and a payload has none of them any more — falling back to it would look
 *  up a key that cannot be there and answer `undefined` the slow way. */
export function capabilitiesFor(list: Pick<RcListResult, "capabilities">, s: RcSession): RcCapabilities | undefined {
  return s.origin ? list.capabilities[s.origin] : undefined;
}

/** The kinds a create form can offer (broker is URL-driven; `craze` is craze's
 *  own create sheet's job, not this launch form; unknown never creatable).
 *  Mirrors `RcKind::creatable`. */
export const RC_CREATABLE_KINDS: RcKind[] = ["claude-rc", "opencode", "shell"];

/** The tool token a kind's agent maps to under capabilities.agents (undefined = no
 *  agent, e.g. shell). `craze` has its own token (P2) — it still gates on an
 *  installed agent entry through this generic mechanism, even though craze's
 *  own protocol is what states a PROVIDER's availability (D5). Mirrors
 *  `RcKind::tool`. */
const RC_KIND_TOOL: Record<string, string | undefined> = {
  "claude-rc": "claude",
  opencode: "opencode", shell: undefined, craze: "craze",
};

/** The launch UI's gated kind list: the creatable kinds whose backing agent the
 *  capabilities say is installed.
 *
 *  **Absent capabilities offer NOTHING** (S6). They used to fall back to
 *  claude+shell, which was right for a shed whose guest RC helper was too old to
 *  advertise — it had always had both. There is no such binary any more: a host's
 *  contract is roost's synthesized one, present for every host in the payload, so
 *  absence means there is no roost-session to launch into and the fallback would
 *  offer a kind that could only fail. Mirrors `RcCapabilities::creatable_kinds`. */
export function offeredKinds(caps?: RcCapabilities): RcKind[] {
  if (!caps) return [];
  return RC_CREATABLE_KINDS.filter((k) => {
    if (!caps.kinds.includes(k)) return false;
    const tool = RC_KIND_TOOL[k];
    return tool ? (caps.agents[tool]?.installed ?? false) : true;
  });
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
  /** A roost row's open approvals, when a feed carries them (none does today);
   *  a CRAZE row's open-ask COUNT (`pendingAsks`, plan 025 §3.6.3). Read it
   *  through the row's `source` — the two are different facts. */
  pending_approvals?: RcFeedApproval[] | number | null;
  /** Which feed produced this row: `"roost"` (a roost tab) or `"craze"` (a
   *  machine's craze hub roster, plan 025 §3.6.3 — the hub row IS the row for
   *  a craze session, and a roost tab craze owns folds into it). */
  source?: "roost" | "craze" | string | null;
  /** The craze roster's own facts (plan 025 §3.6.3), on a craze row only. */
  provider?: string | null;
  model?: string | null;
  doing?: string | null;
  head_ask_summary?: string | null;
  last_reply?: string | null;
  attached?: number | null;
  start_error?: string | null;
  permission_mode?: string | null;
  provider_session_id?: string | null;
  /** The row's facts came from a cheap or stale read — rendered as "last
   *  known". Every row of a craze source that is not live has it. */
  approximate?: boolean | null;
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
  /** The shed this session runs on, `roost:<server>/<shed>`. */
  target?: string | null;
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
   *  strings on the wire); absent for a shed session. On a CRAZE row it is the
   *  roost tab the merge attached to the session (plan 025 §3.6.3) — what its
   *  End tab closes — and absent for a headless one. */
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
  /** Which adapter speaks to it. A craze row's is `"craze"`. The lane ops
   *  address the shed (`target`) and this `session_id`; they do not take a
   *  kind. */
  kind: string;
  /** The AGENT's own session id — the address every `lane.*` op takes, and not
   *  the roost tab id (`tab_id` / `slug`). A craze row's is its hostId. */
  session_id: string;
  /** The reported loopback URL — roost-stamped lanes only; a craze lane is
   *  reached through its machine's hub, not a URL. */
  server_url?: string;
};

/** A host's craze source, as `rc.list`'s `hosts[].craze` reports it (plan
 *  025 §3.6.2). `absent` is "never reached" and "not installed" alike; `cause`
 *  names an offline (or not-installed) state's class (`too_old`,
 *  `unreachable`, …). */
export type CrazeStatus = {
  state: "live" | "dormant" | "offline" | "absent" | string;
  cause?: string | null;
  create: boolean;
  create_options: boolean;
  /** The LIVE hub can name a session at create (`createTitle`, plan 029
   *  P10) — `false` while dormant, offline or too old, exactly as
   *  `create_options`. The create sheet offers "Name (optional)" on it. */
  create_title?: boolean;
  /** NOT craze's: the generation of this host's craze source — its
   *  INCARNATION, as `craze.create_options` reports it (`null`: no source).
   *  A host removed and added again under the same address is a new one with
   *  a larger generation; the shell resets the sheet's per-machine Resume
   *  state on it (plan 032 §3.4, C5 re-review). */
  source_gen?: number | null;
};

/** A configured machine's health, for the sessions view's group rows. A machine
 *  with no sessions AND no reachability is still worth a row — that row IS the
 *  information ("mini3 is asleep"). */
export type MachineStatus = {
  name: string;
  /** The shed address, `roost:<server>/<shed>`. `name` and `origin` carry the
   *  same token. */
  target?: string | null;
  origin: string;
  /** `machine` | `shed` — which kind of host this row is (a listed shed has a
   *  status row too, once something has answered on it). */
  kind?: string | null;
  reachable: boolean;
  /** Whether a snapshot has EVER arrived — distinguishes "still connecting"
   *  from "connected, and genuinely has no sessions". */
  connected_once: boolean;
  /** Whether a roost watcher runs for this host (plan 026 §3.2): a machine's
   *  from the start, a shed's once its roost answered — before its first
   *  inventory (`connected_once`) arrives. A listed host with none, and a
   *  craze source, is "craze only" (`crazeOnly`). */
  roost_watched?: boolean | null;
  sessions: number;
  /** Why it is unreachable, verbatim from the watcher: "no route to host" and
   *  "nothing is listening on 1029" are different problems. */
  detail?: string | null;
  /** This host's craze source (plan 025 §3.6.2). */
  craze?: CrazeStatus | null;
};

/** An UNLISTED host craze is offered on (plan 026 §3.2, "offer, don't
 *  list"): a running shed whose craze probe proved craze there with no hub
 *  roster to list — `dormant` (New craze session on its Sheds-pane row and in
 *  "Where"; opening the sheet starts its hub) or too old (its note). It has no
 *  status row and no sessions; once a create or its first craze `Ready` lists
 *  it, it is a `MachineStatus` instead — never both. */
export type ShedCrazeEntry = {
  name: string;
  label: string;
  origin: string;
  kind: string;
  craze: CrazeStatus;
};

/** The `rc.list` result: live sessions (shed AND machine), the per-shed
 *  capabilities captured during the probe (keyed by `host/shed`), each
 *  configured machine's health, and the unlisted hosts craze is offered on. */
export type RcListResult = {
  sessions: RcSession[];
  capabilities: Record<string, RcCapabilities>;
  hosts: MachineStatus[];
  shed_craze: ShedCrazeEntry[];
};

/** A session's stable row identity — the row's `origin` plus its slug. Every row
 *  carries an origin since S6 (there is one row source), so the legacy
 *  `host/shed` composite is only the shape of a payload old enough to predate
 *  it, kept so a mixed list still produces unique keys rather than colliding. */
export function sessionKey(s: RcSession): string {
  return `${s.origin ?? `${s.host}/${s.shed}`}/${s.slug}`;
}

/** One `rc.list` attempt: the payload, plus why it failed if it did.
 *
 *  **A failure is not an empty list**, and this type exists because it used to
 *  be one: `fetchRcList` swallowed the invocation error and answered
 *  `{sessions: [], …}`, which the Agents pane then rendered as "no agent
 *  sessions — set up a roost-session", pointing the user at the wrong fix. The
 *  reason travels with the payload rather than being logged, because the pane is
 *  the only place it can be read. */
export type RcListLoad = RcListResult & { error: string | null };

/** The live sessions + capabilities across every listed host (the same data the
 *  `rc.list` op serves the harness).
 *
 *  Does NOT use the swallowing [invoke]: the whole point is to tell an empty
 *  answer from a failed one. A browser (no Tauri runtime to import) is a
 *  failure like any other here — [useRcSessions] never calls it there. */
export async function fetchRcList(host?: string, shed?: string): Promise<RcListLoad> {
  try {
    const core = await import("@tauri-apps/api/core");
    const r = await core.invoke<RcListResult>("rc_list", { host, shed });
    return {
      sessions: r?.sessions ?? [],
      capabilities: r?.capabilities ?? {},
      hosts: r?.hosts ?? [],
      shed_craze: r?.shed_craze ?? [],
      error: null,
    };
  } catch (e) {
    return { sessions: [], capabilities: {}, hosts: [], shed_craze: [], error: String(e) };
  }
}

/** The Agents pane's empty state — the title, the sentence, and the offer beside
 *  it.
 *
 *  **Lives in `agentsEmpty.ts`** and is re-exported here so callers keep one
 *  import: the rule it encodes (which of loading / failed / genuinely-empty this
 *  blank screen is, and which of them may offer the bootstrap) is the copy a
 *  person reads, and it earns a test that needs no React, no DOM and no Tauri
 *  runtime — the `newest.ts` / `roostBoard.ts` shape. */
export type { AgentsLoad, AgentsEmptyState } from "@/lib/agentsEmpty";
export { agentsEmptyState, crazeOnly } from "@/lib/agentsEmpty";

/** Report the rendered RC sessions — and, when the list is empty, the empty
 *  state's own words — so the `agents.dump` op can observe them: the drivable
 *  truth of the Agents pane, like `dashboard.dump` reads the sheds.
 *  (`ui_report` merges these keys with the shell's snapshot.) */
export function reportAgents(
  sessions: Array<RcSession & { transcript?: boolean; activity_chip?: string | null }>,
  empty: AgentsEmptyState | null,
): void {
  void invoke("ui_report", { snapshot: { agents: sessions, agents_empty: empty } });
}

/** The SINGLE source of truth for RC sessions + per-shed capabilities, lifted to the
 *  App so the sidebar badge (`sessions.length`), the Agents pane's list, and the
 *  launch dialog's capability gating all read ONE `rc.list` fetch — no divergence
 *  between independent fetches with different triggers. Refreshed on mount, on each
 *  lifecycle `refresh` event (sheds coming up/down), and whenever a caller (the pane
 *  Refresh button, a launch/kill, the pane/dialog on open) invokes the returned
 *  `refresh`.
 *
 *  **`load` is the third thing an empty `sessions` can mean.** It starts
 *  `loading` — no answer has arrived, and the pane must not yet claim anything —
 *  becomes `ready` when one does, and `failed` when the invocation threw, with
 *  the reason in `error`. A plain browser has nothing to load and is `ready`
 *  with an empty list, which is the truth about it. See `agentsEmpty.ts`. */
export function useRcSessions(): {
  sessions: RcSession[];
  capabilities: Record<string, RcCapabilities>;
  hosts: MachineStatus[];
  /** `rc.list`'s `shed_craze` — read craze status through `crazeStatusFor`,
   *  which falls back to it for a host with no status row. */
  shedCraze: ShedCrazeEntry[];
  load: AgentsLoad;
  error: string | null;
  refresh: () => void;
} {
  const [state, setState] = useState<RcListResult & { load: AgentsLoad; error: string | null }>({
    sessions: [],
    capabilities: {},
    hosts: [],
    shed_craze: [],
    load: inTauri() ? "loading" : "ready",
    error: null,
  });
  // A generation guard shared across the mount-, event-, and caller-driven reloads,
  // so a slower older fetch can't overwrite a newer one (the pane's superseded-fetch
  // guard, now owned here for the shared state).
  const gen = useRef(0);
  const refresh = useCallback(() => {
    if (!inTauri()) return;
    const mine = ++gen.current;
    void fetchRcList().then((r) => {
      // A REFRESH does not go back to `loading`: the rows already on screen stay
      // there, which is what makes the pane Refresh button a re-read rather than
      // a blank-and-refill. Only the first load is ever `loading`.
      if (mine === gen.current) setState({ ...r, load: r.error ? "failed" : "ready" });
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
    hosts: state.hosts,
    shedCraze: state.shed_craze,
    load: state.load,
    error: state.error,
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
 *  COMPLETES), and `stale` is the banner — the reason a `Stale` or a `Down` gave;
 *  non-null means the last good generation is still on screen and the feed behind
 *  it is not live. That is the contract's "consume" posture: keep rendering, say
 *  so, don't blank.
 *
 *  `ended` is a DIFFERENT fact (plan 025 §3.2.4): `true` only after a `Down` —
 *  the subscription is over — where `stale` alone can be a lane that is
 *  reconnecting on its own and must not be reopened.
 *
 *  `capabilities` and `settings` are the LIVE generation's, staged and swapped
 *  in with its rows: what this session can do, read HERE and nowhere else (not
 *  from `lane.open`, which no longer carries them). `null` until a seed carrying
 *  them has completed; `settings` stays `null` on a session with none to show.
 *
 *  `session` is the live generation's session ROW — the stream's latest, `null`
 *  until a seed has swapped one in — and the row the panel's header reads once
 *  it is there (`laneHeader`). `lane.open`'s row is the one the lane was opened
 *  with and never changes while it stays open: for a session the create sheet
 *  opened at once, the create's own row, with no permission mode. */
export type LaneView = {
  messages: LaneMessage[];
  activity: string;
  session: LaneSessionRow | null;
  generation: number;
  stale: string | null;
  ended: boolean;
  /** The lane is over for good (plan 032 §3.1.1, PC14b) — the ENTRY's
   *  lifecycle, not `ended`: its session's final `Down` arrived, or the
   *  backend ended it (`session_gone`/`hub_gone` after its row left and the
   *  grace ran out, `host_removed`). Every verb is then refused `lane_ended`,
   *  and the panel disables its composer, Send and Cancel. `ended` alone may
   *  be a retryable `Down` the backend is already resubscribing after. */
  finished: boolean;
  capabilities: LaneCapabilities | null;
  settings: LaneSettings | null;
  /** The session's `/` menu, RESOLVED by the shared core (plan 029 §3.4):
   *  `null` when the capabilities do not say `slash` or no catalog has
   *  arrived — then the composer offers no menu and sends a `/` draft as
   *  text. Handed back to the `lane_slash_*` commands as is; never edited. */
  slash_menu?: SlashMenu | null;
  /** The id of `settings`' EFFORT option — craze's `EffortOption` rule, run
   *  by the shared core (`LaneSettings::effort_option`, plan 032 §3.2,
   *  PC14h) — or `null` when `settings` is or names none. What the chip shows
   *  as the effort: read from here, never re-derived from the options. */
  effort_option?: string | null;
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

/** The agent session behind a lane (`shed_core::lane::LaneSession`). The
 *  fields after `last_change_unix_ms` are a craze roster row's facts (plan 025)
 *  and are absent from an opencode row. */
export type LaneSessionRow = {
  id: string;
  title: string;
  cwd: string;
  activity: string;
  pending_approvals: number;
  approximate: boolean;
  parent_id?: string | null;
  last_change_unix_ms?: number | null;
  provider?: string | null;
  model?: string | null;
  doing?: string | null;
  head_ask_summary?: string | null;
  last_reply?: string | null;
  since_unix_ms?: number | null;
  attached?: number | null;
  start_error?: string | null;
  provider_session_id?: string | null;
  permission_mode?: string | null;
  tab_id?: number | null;
};

/** What THIS SESSION can do (`shed_core::lane::LaneCapabilities`) — per session,
 *  riding the lane's stream, and read from `lane.messages`' `capabilities`
 *  (plan 025 §3.2). `create` left this type: creating is a machine-level act. */
export type LaneCapabilities = {
  kind: string;
  interject: boolean;
  cancel: boolean;
  approvals: boolean;
  history_cursor: boolean;
  settings: boolean;
  stop: boolean;
  /** The session can be renamed (`lane_set_title`, plan 029 D4). */
  set_title?: boolean;
  /** The session publishes a slash catalog (plan 029 D3); `true` with no
   *  `LaneView.slash_menu` yet is a catalog still to come. */
  slash?: boolean;
};

/* ---- the `/` menu (plan 029 §3.4, `shed_app::slash_menu`) ---------------- *
 * Mirrors of the shared core's types, as the `lane_slash_*` commands answer
 * them; the panel renders these and hands them back. */

/** What running a shown row does (`SlashAction`). `effort_picker` is
 *  `/effort` (plan 032 §3.2): bare, it opens the settings sheet, whose effort
 *  row is `option`; its `levels` and `default_level` are what the shared
 *  dispatch matched a `/effort <level>` against — the panel never reads them.
 *  `set_effort` is only ever a dispatch's answer to such a level, never a
 *  row's: a `lane_set` of its `change` (a config change of the effort option
 *  bound to `for_model`), sent even when it is the current level. */
export type SlashAction =
  | { kind: "prompt" }
  | { kind: "set_title" }
  | { kind: "set_mode"; change: LaneSettingChange }
  | { kind: "stop" }
  | { kind: "model_picker" }
  | { kind: "effort_picker"; option: string; for_model: string; levels: LaneChoice[]; default_level?: string | null }
  | { kind: "set_effort"; change: LaneSettingChange };

/** One shown row (`SlashMenuRow`). `arg` is the catalog's word (`none` |
 *  `optional` | `required`), for display. */
export type SlashMenuRow = {
  name: string;
  qualified?: string | null;
  description?: string | null;
  source: string;
  arg: string;
  arg_hint?: string | null;
  while_working: boolean;
  action: SlashAction;
};

/** A hidden builtin a typed command is refused for (`SlashHidden`). */
export type SlashHidden = { name: string; reason: string };

/** A session's resolved menu (`SlashMenu`). */
export type SlashMenu = { rows: SlashMenuRow[]; hidden: SlashHidden[] };

/** One row of an open menu: `dimmed` while a turn runs and the row would be
 *  refused for it (X7). */
export type SlashFilterRow = { row: SlashMenuRow; dimmed: boolean };

/** What the menu panel shows for a draft and caret (`SlashFilter`). `open`
 *  is exactly "at least one row"; `token` is the draft's first token, what an
 *  Esc dismissal is keyed by; `start` is the row the highlight starts on —
 *  the first row that is not dimmed, else 0 (0 when closed) — wherever the
 *  panel resets it (plan 030 §3.4). */
export type SlashFilter = { open: boolean; token: string; rows: SlashFilterRow[]; start: number };

/** What sending a draft does (`SlashDispatch`). */
export type SlashDispatch =
  | { kind: "not_command" }
  | { kind: "prompt"; text: string }
  | { kind: "run"; action: SlashAction; arg?: string | null }
  | { kind: "refuse"; reason: string };

/** What choosing a row does (`SlashAccept`): complete the draft (the caret in
 *  UTF-16 code units, as `selectionStart` counts), or act on a dispatch. */
export type SlashAccept =
  | { kind: "complete"; draft: string; caret: number }
  | { kind: "dispatch"; dispatch: SlashDispatch };

/** How a row was chosen. */
export type SlashAcceptKey = "enter" | "tab" | "pick";

/** One selectable value — a model, a mode, an option's value
 *  (`shed_core::lane::LaneChoice`). `id` is what a change sends back.
 *  `effort` is a MODEL's effort facts (plan 032 §3.3), absent everywhere
 *  else and on every model whose agent states none (an ACP session's). */
export type LaneChoice = {
  id: string;
  name: string;
  rank?: number | null;
  description?: string | null;
  effort?: LaneModelEffort | null;
};

/** What a model says about its effort before it is chosen
 *  (`shed_core::lane::LaneModelEffort`, craze 0.2.0's per-model effort facts,
 *  a native session's alone): its levels, its default, and the level a switch
 *  to it lands on. The CURRENT model's effort is its effort option's
 *  `current`, never these. */
export type LaneModelEffort = {
  efforts: string[];
  default_effort?: string | null;
  switch_effort?: string | null;
};

/** One of a model's own options (`shed_core::lane::LaneSetting`): its current
 *  value and the values it offers. `category` is an open string. */
export type LaneSetting = { id: string; name: string; category: string; current: string; values: LaneChoice[] };

/** How full a session's context is (`shed_core::lane::LaneUsage`). */
export type LaneUsage = { context_tokens?: number | null; context_window?: number | null };

/** A session's settings, rendered generically (`shed_core::lane::LaneSettings`):
 *  the model and the models, the mode and the modes, the model's own options,
 *  and how full its context is. */
export type LaneSettings = {
  model?: string | null;
  models: LaneChoice[];
  mode?: string | null;
  modes: LaneChoice[];
  options: LaneSetting[];
  usage?: LaneUsage | null;
};

/** One change to a session's settings (`shed_core::lane::LaneSettingChange`,
 *  `lane_set`'s `change`): a model or a mode by its id, or one of the current
 *  model's options by its id and value — every id as `LaneSettings` offered it,
 *  verbatim — and, on an option, `for_model`: the model the client DISPLAYED it
 *  for (plan 025 Amendment A13). Strict on the backend: a shape it cannot name
 *  is `bad_request`. */
export type LaneSettingChange =
  | { kind: "model"; id: string }
  | { kind: "mode"; id: string }
  | { kind: "config"; id: string; value: string; for_model?: string };

/** `lane.open`'s answer: the session row alone — the row as the source listed
 *  it at the open, re-answered unchanged while the lane stays open. What the
 *  session can do is the view's (`LaneView.capabilities`), not the open's; and
 *  once a seed has swapped in, so is the row (`LaneView.session`). */
export type LaneOpened = { session: LaneSessionRow };

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

/** `{target, session_id, event}` — the Tauri `lane-event` payload. The
 *  panel only reads the address (it re-reads the staged view rather than
 *  folding frames itself, so what it renders is the same truth `lane.messages`
 *  answers with), which is why `event` stays `unknown`. */
export type LaneEventEnvelope = { target?: unknown; session_id?: unknown; event?: unknown };

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
  // Already recovered (a wrapper threw one): its message no longer carries the
  // code, so splitting it again would turn the code into `failed`.
  if (e instanceof LaneFailure) return e;
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
export async function laneOpen(target: string, sessionId: string): Promise<LaneOpened> {
  return laneInvoke<LaneOpened>("lane_open", { target, sessionId });
}

export async function laneMessages(target: string, sessionId: string): Promise<LaneView> {
  return laneInvoke<LaneView>("lane_messages", { target, sessionId });
}

export async function laneApprovals(target: string, sessionId: string): Promise<LaneApproval[]> {
  const r = await laneInvoke<{ approvals?: LaneApproval[] }>("lane_approvals", { target, sessionId });
  return r.approvals ?? [];
}

/** Send a prompt. `mode` defaults to `queue`; `interject` is REFUSED by the
 *  opencode adapter (`capabilities.interject` is false) rather than silently
 *  downgraded, so a caller that passes it gets `not_accepting`. */
export async function laneSend(
  target: string,
  sessionId: string,
  text: string,
  mode?: string,
): Promise<void> {
  await laneInvoke("lane_send", { target, sessionId, text, mode });
}

export async function laneCancel(target: string, sessionId: string): Promise<void> {
  await laneInvoke("lane_cancel", { target, sessionId });
}

/** End the SESSION (plan 025 §3.6.4) — craze's `session.stop`. Answered on
 *  craze's receipt; the lane ends, and the row leaves, when the session closes.
 *  The panel offers it only when the session's capabilities say `stop`. */
export async function laneStop(target: string, sessionId: string): Promise<void> {
  await laneInvoke("lane_stop", { target, sessionId });
}

/** Rename the SESSION (`lane.set_title`; plan 029 D4/D12) — the composer's
 *  `/rename <title>`. The new title arrives on the stream, in the session row
 *  the header reads. Resolves `{}` — or `{warning}` for a rename that TOOK
 *  and craze could not record (`index_write`): a success to clear the draft
 *  on, with the warning shown. THROWS a coded failure otherwise
 *  (`outcome_unknown` for a lost answer, never resent). */
export async function laneSetTitle(
  target: string,
  sessionId: string,
  title: string,
): Promise<{ warning?: string | null }> {
  return laneInvoke<{ warning?: string | null }>("lane_set_title", { target, sessionId, title });
}

/** The menu panel for `draft` with the caret at `caret` (UTF-16 code units)
 *  — `shed_app::slash_menu::filter`, the session's `activity` deciding what
 *  is dimmed. THROWS a coded failure (a malformed menu). */
export async function laneSlashFilter(
  menu: SlashMenu,
  draft: string,
  caret: number,
  activity: string,
): Promise<SlashFilter> {
  return laneInvoke<SlashFilter>("lane_slash_filter", { menu, draft, caret, activity });
}

/** What sending `draft` does — `shed_app::slash_menu::dispatch`: the command
 *  grammar and its guard (P5, P6). */
export async function laneSlashDispatch(menu: SlashMenu, draft: string, activity: string): Promise<SlashDispatch> {
  return laneInvoke<SlashDispatch>("lane_slash_dispatch", { menu, draft, activity });
}

/** What choosing the row `row` with `key` does — `shed_app::slash_menu::
 *  accept`; `null` when there is nothing to accept (the menu moved under the
 *  highlight: re-filter). */
export async function laneSlashAccept(
  menu: SlashMenu,
  draft: string,
  row: string,
  key: SlashAcceptKey,
  activity: string,
): Promise<SlashAccept | null> {
  return laneInvoke<SlashAccept | null>("lane_slash_accept", { menu, draft, row, key, activity });
}

/** The session's settings, read now (`lane.settings`; plan 025 §3.10). The
 *  sheet renders the STREAM's copy — `LaneView.settings` — and this one-shot
 *  read is the socket op's twin. THROWS a coded failure. */
export async function laneSettings(target: string, sessionId: string): Promise<LaneSettings> {
  const r = await laneInvoke<{ settings: LaneSettings }>("lane_settings", { target, sessionId });
  return r.settings;
}

/** Change one setting (`lane.set`; plan 025 §3.10). Resolves on craze's
 *  confirmation — the new value arrives on the stream, as the next
 *  `LaneView.settings`. THROWS a coded failure: `not_accepting` (on an option,
 *  craze's `stale_model`: the model moved under the choice), `outcome_unknown`
 *  (the answer was lost with the connection — the change may have run, and it
 *  is never resent), or the contract's others. */
export async function laneSet(
  target: string,
  sessionId: string,
  change: LaneSettingChange,
): Promise<void> {
  await laneInvoke("lane_set", { target, sessionId, change });
}

export async function laneAnswer(
  target: string,
  sessionId: string,
  approvalId: string,
  answer: LaneAnswer,
): Promise<void> {
  await laneInvoke("lane_answer", { target, sessionId, approvalId, answer });
}

/** End the subscription and release the transport. Idempotent, and deliberately
 *  BEST-EFFORT: this runs from the panel's unmount cleanup, where a throw would
 *  escape into React and where an unmount racing an eviction (the tab went away)
 *  is normal, not an error. */
export async function laneClose(target: string, sessionId: string): Promise<void> {
  try {
    await laneInvoke("lane_close", { target, sessionId });
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
  /** The shed, `roost:<server>/<shed>`. */
  target: string;
  session_id: string;
  /** Which adapter is behind this panel (the view's `capabilities.kind`), and
   *  what the header badge says. `""` until a seed carrying capabilities has
   *  swapped in. */
  kind: string;
  /** The header's permission line (`permissionLine` — `bypass` reads "runs
   *  tools without asking"), or `null` when the session states none. Like
   *  `title` and `cwd`, read off the LIVE session row (`LaneView.session`),
   *  and off `lane.open`'s only until a seed has swapped one in
   *  (`laneHeader`). */
  permission: string | null;
  title: string;
  cwd: string;
  activity: string;
  generation: number;
  /** The stale banner's reason, or null while the lane is live. */
  stale: string | null;
  /** The subscription ENDED (a `Down`) — not merely stale. */
  ended: boolean;
  /** The lane is over for good (`lane.messages`' `finished`, plan 032): what
   *  `composer_disabled` follows. */
  finished: boolean;
  /** The composer — its text box, Send, Interject and Cancel — is disabled
   *  because the lane is FINISHED (`laneVerbs`' `composer`). */
  composer_disabled: boolean;
  rows: LaneRow[];
  approvals: LaneApprovalCard[];
  /** The Cancel button exists AND is enabled: the session's streamed
   *  capabilities offer `cancel` and it is Working (`laneVerbs`). `false` both
   *  for a session that cannot cancel (no button at all) and for one that can
   *  but is not working (a disabled button). */
  can_cancel: boolean;
  /** The Interject toggle, or `null` when the session's capabilities do not
   *  advertise it and no toggle is rendered at all.
   *
   *  Three states rather than a bool because "absent" and "present but
   *  disabled" are different claims and both are pinned: opencode advertises no
   *  `interject`, so its panel has no toggle; a session that does (gx did; a
   *  craze session may) gets one — enabled only while the session is Working,
   *  because that is the only time the agent accepts one. */
  interject: { on: boolean; enabled: boolean } | null;
  /** The Stop button (plan 025 §3.6.5): `null` when the session's capabilities
   *  do not offer `stop` and no button is rendered at all; otherwise whether
   *  its inline confirm is open. */
  stop: { confirming: boolean } | null;
  /** The header's settings chip as it reads (`<model name> · <effort value> ·
   *  fast`, plan 025 §3.10), or `null` when there is none — the session's
   *  capabilities do not say `settings` (it is hidden, never disabled). */
  settings_chip: string | null;
  error: string | null;
};

/** The panel's COMPOSER as rendered — reported apart from the transcript
 *  (`reportLaneComposer`) so a keystroke re-sends these fields, not every
 *  message; `lane.dump` answers them inside its `lane`. */
export type LaneComposerReport = {
  /** The composer's text, as typed. */
  draft: string;
  /** The composer's `/` menu (plan 029 D13), or `null` on a session that
   *  offers none (its capabilities do not say `slash`, or no catalog yet).
   *  `open`: the menu is ON SCREEN (the shared filter's open, less an Esc
   *  dismissal); `rows`: what it lists, each `dimmed` while a turn runs and
   *  the row would wait for it, the `highlighted` one what Enter/Tab accept;
   *  `refusal`: the inline line a refused command left (the reason, craze's
   *  words, or "may have applied — check the session"); `warning`: a rename
   *  that took with a caveat. */
  slash_menu: SlashMenuReport | null;
};

/** The `/` menu as the composer rendered it (`lane.dump`'s `slash_menu`). */
export type SlashMenuReport = {
  open: boolean;
  token: string;
  rows: { name: string; dimmed: boolean; highlighted: boolean }[];
  draft: string;
  refusal: string | null;
  warning: string | null;
};

/** Report the transcript panel's rendered state (mounted-only, like
 *  `reportEgress`). Pass `null` on unmount to CLEAR the key — the merge
 *  overwrites `lane` with null rather than skipping it, so `lane.dump` answers
 *  `null` for "no panel" instead of the previous mount's stale snapshot. */
export function reportLane(snapshot: LaneReport | null): void {
  void invoke("ui_report", { snapshot: { lane: snapshot } });
}

/** Report the transcript panel's composer (`LaneComposerReport`), `null` at
 *  unmount. */
export function reportLaneComposer(snapshot: LaneComposerReport | null): void {
  void invoke("ui_report", { snapshot: { lane_composer: snapshot } });
}

/** The settings sheet as rendered — the UI truth `lane_settings.dump` reads
 *  (plan 025 §3.10). Each row is what a person sees: the control it is drawn
 *  as, its values with the selected one, and its state — `pending` while a
 *  change awaits craze, `refused` with the text shown inline, or
 *  `not_confirmed` after an answer lost to a drop, until the next `Settings`.
 *  `enabled` is whether the row takes a press now. A value's `secondary` is
 *  the dim line drawn under it — on the model list, `effort → <level>` for a
 *  model other than the current one whose effort facts name the level a
 *  switch lands on (plan 032 §3.3) — else `null`. */
export type LaneSettingsReport = {
  /** The shed, `roost:<server>/<shed>`. */
  target: string;
  session_id: string;
  chip: string;
  rows: {
    id: string;
    kind: "model" | "mode" | "config";
    name: string;
    category: string | null;
    control: "segmented" | "list";
    current: string | null;
    current_name: string | null;
    values: { id: string; name: string; selected: boolean; secondary: string | null }[];
    state: "pending" | "refused" | "not_confirmed" | null;
    text: string | null;
    enabled: boolean;
  }[];
  usage: { tokens: number; window: number; percent: number; text: string } | null;
};

/** Report the settings sheet's rendered state (mounted-only, the `reportLane`
 *  rule): `null` on close CLEARS the key, so `lane_settings.dump` answers
 *  `null` for "no sheet". */
export function reportLaneSettings(snapshot: LaneSettingsReport | null): void {
  void invoke("ui_report", { snapshot: { lane_settings: snapshot } });
}

/* ---- craze: the create sheet and Open in terminal (plan 025 §3.6.6, §3.8) -- */

/** A refusal of one of the `craze_*` commands, its code recovered — the
 *  `lane_*` commands' `"<code>: <message>"` agreement ([laneFailure] splits it
 *  the same way). `outcome_unknown` is the one the sheet keeps its request id
 *  on; `too_old` / `not_installed` / `no_craze` say what this machine's craze
 *  is; the rest are the lane contract's own codes. */
export type CrazeOpFailure = LaneFailure;

/** Invoke a craze command, THROWING a coded [LaneFailure]. */
async function crazeInvoke<T>(cmd: string, args: Record<string, unknown>): Promise<T> {
  const core = await import("@tauri-apps/api/core");
  try {
    return await core.invoke<T>(cmd, args);
  } catch (e) {
    throw laneFailure(e);
  }
}

/** What a create can start on `machine` — read afresh on every sheet open
 *  (D8: nothing is cached) — with the incarnation of its craze source the
 *  read came through (`source_gen`, the reply's, carried on the options). On
 *  a DORMANT machine this is the explicit action that starts its hub. THROWS
 *  a coded failure. */
export async function crazeCreateOptions(target: string): Promise<CrazeCreateOptions> {
  const r = await crazeInvoke<{ options: CrazeCreateOptions; source_gen?: number | null }>(
    "craze_create_options", { target },
  );
  return { ...r.options, source_gen: r.source_gen ?? null };
}

/** The fields of one create — the sheet's typed form and the request id it
 *  holds (`crazeCreate.ts`'s lifecycle). `held`: this submission REPLAYS that
 *  id, kept from an unknown outcome (`replaysHeldRequest`, plan 026 §3.1) —
 *  then only craze's own answer about the request ends it. */
export type CrazeCreateFields = {
  /** The shed, `roost:<server>/<shed>`. */
  target: string;
  cwd: string;
  provider?: string | null;
  prompt?: string | null;
  /** "Name (optional)" (plan 029 D14), already trimmed — absent when blank
   *  (`crazeCreate.ts`'s `createTitle`). */
  title?: string | null;
  requestId: string;
  held: boolean;
};

/** What `craze_create` answers: the new session's row (already in the
 *  machine's listing), its hostId, what became of the first prompt, and the
 *  request id it ran under. */
export type CrazeCreated = {
  session: RcSession;
  host_id: string;
  /** craze answered with a session that has ALREADY ENDED (a replay of an
   *  earlier create's answer, within craze's ten-minute window): nothing is
   *  listed, and there is no transcript to open. */
  ended?: boolean;
  prompt: "none" | "accepted" | "unknown" | "refused" | string;
  prompt_error?: string | null;
  request_id: string;
};

/** Create a craze session. THROWS a coded failure — `outcome_unknown` when
 *  craze's answer was lost twice, or a held replay got no answer about its
 *  request (keep the id; Try again resumes it). */
export async function crazeCreate(f: CrazeCreateFields): Promise<CrazeCreated> {
  return crazeInvoke<CrazeCreated>("craze_create", {
    target: f.target,
    cwd: f.cwd,
    provider: f.provider ?? undefined,
    prompt: f.prompt ?? undefined,
    title: f.title ?? undefined,
    requestId: f.requestId,
    held: f.held,
  });
}

/** The saved sessions the sheet's Resume mode lists on `machine` (plan 032
 *  §3.4) — read afresh on every first choice of Resume, newest first, at most
 *  50 (`more` when craze left some out), with the host's `home` where the app
 *  knows it (`crazeResume.ts`'s `shortenHome`). Like the create options, on a
 *  DORMANT machine this may start its hub. THROWS a coded failure. */
export async function crazeSaved(target: string): Promise<CrazeSavedList> {
  const r = await crazeInvoke<{ saved: CrazeSavedList; home?: string | null; source_gen?: number | null }>(
    "craze_saved", { target },
  );
  return {
    sessions: r.saved?.sessions ?? [], more: !!r.saved?.more, home: r.home ?? null,
    source_gen: r.source_gen ?? null,
  };
}

/** What `craze_reopen` answers: the running session's row (already in the
 *  machine's listing), its hostId — what the transcript opens on — and
 *  whether craze JOINED a session something already ran (`joined`) or
 *  started one to load it. */
export type CrazeReopened = {
  session: RcSession;
  host_id: string;
  /** The session had already ended again by the time its row was folded:
   *  nothing is listed, and there is no transcript to open. */
  ended?: boolean;
  /** The machine was removed (or removed and added again) while the reopen
   *  ran: its answer came through a craze source the machine no longer has,
   *  nothing was listed, and there is nothing to open on it here — whatever
   *  the row's own `stale` (`crazeResume.ts`'s `ReopenAnswer`). */
  superseded?: boolean;
  joined: boolean;
  /** The saved id this answered for. */
  id: string;
  /** The incarnation of the machine's craze source the reopen went
   *  through. */
  source_gen?: number | null;
};

/** Run the saved session `id` again — or join it where it already runs. It
 *  can take minutes. THROWS a coded failure whose code keeps the reopen
 *  error's variant: `gone` (drop the row), `start_failed` (show it, keep the
 *  row), `unavailable` (Retry — a lost answer included), or the inner lane
 *  code (`crazeResume.ts`'s `settleResume` branches on it). */
export async function crazeReopen(target: string, id: string): Promise<CrazeReopened> {
  return crazeInvoke<CrazeReopened>("craze_reopen", { target, id });
}

/** What `craze_open_terminal` answers: the roost tab it opened. */
export type CrazeTerminal = {
  origin: string;
  target: string;
  session_id: string;
  tab_id: string;
  cwd: string;
  argv: string[];
};

/** Open in terminal on a headless craze row: a roost tab running `craze
 *  attach` on the session (its hostId), in its workspace. THROWS a coded
 *  failure. */
export async function crazeOpenTerminal(target: string, sessionId: string): Promise<CrazeTerminal> {
  return crazeInvoke<CrazeTerminal>("craze_open_terminal", { target, sessionId });
}

/** One provider row AS RENDERED in the sheet — read back off its DOM
 *  (`data-provider` and its attributes), never restated from state. */
export type CrazeCreateProviderRow = {
  id: string;
  label: string;
  state: string;
  dimmed: boolean;
  selected: boolean;
  reason: string | null;
  fix: string | null;
};

/** The create sheet as `craze_create.dump` reads it. */
export type CrazeCreateDump = {
  /** The shed, `roost:<server>/<shed>`. */
  target: string;
  /** `loading` | `failed` (options) | the draft's phase: `idle` |
   *  `submitting` | `refused` | `unknown` | `created`. */
  state: string;
  providers: CrazeCreateProviderRow[];
  /** The default rule's answer (`preselectedProvider`). */
  preselected: string | null;
  default_provider: string | null;
  recent_dirs: string[];
  /** Each labelled control's current value ("Directory", "Name", "First
   *  prompt"). */
  values: Record<string, string>;
  /** Whether the "Name (optional)" field is rendered (plan 029 D14): on a
   *  live hub that can name a session at create, or while a title is typed. */
  title_field: boolean;
  /** A refusal about the title, as shown under Name. */
  title_problem: string | null;
  /** The request id the sheet holds — set only while a submission is in
   *  flight or its outcome is unknown. */
  request_id: string | null;
  /** The machine's note (offline / too old / not installed) or "no provider
   *  is ready on this machine". */
  note: string | null;
  /** The refusal as shown, by code. */
  error: { code: string; message: string; where: string; text: string } | null;
  /** The start failure's cause, verbatim, as the monospace block shows it. */
  cause: string | null;
  /** The directory field's own refusal (relative or empty). */
  cwd_problem: string | null;
  create_enabled: boolean;
  primary: string;
  /** The sheet's `New | Resume` control (plan 032 §3.4): `offered` — shown,
   *  because the create options say the hub can reopen — and `enabled` (not
   *  while a create submits or a reopen runs). */
  modes: { offered: boolean; enabled: boolean };
  /** The half on screen: `new` (the create form) or `resume`. */
  mode: "new" | "resume" | string;
  /** The Resume half as rendered, or `null` while New shows. */
  saved: CrazeSavedDump | null;
  rendered: string;
};

/** The Resume half of the create sheet as rendered (plan 032 §3.4, PC14i). */
export type CrazeSavedDump = {
  /** The list's read: `loading` | `failed` | `ready`. */
  state: string;
  /** A failed read's words, as shown (with its Retry). */
  error: string | null;
  /** "Showing the 50 most recent." when craze left some out. */
  more: string | null;
  /** The note a dropped (`gone`) row left: its title and craze's words. */
  gone: string | null;
  rows: CrazeSavedRowDump[];
};

/** One saved session's row AS RENDERED — read back off its DOM. */
export type CrazeSavedRowDump = {
  id: string;
  title: string;
  provider: string;
  /** The workspace as shown: the home shortened to `~` where known. */
  workspace: string;
  /** The relative age shown ("5 min ago"). */
  updated: string;
  /** `idle` | `resuming` | `refused` | `unavailable`. */
  state: string;
  /** The row's refusal as shown, or `null`. */
  error: string | null;
  /** The button's label (Resume, Resuming…, Retry) and whether it works. */
  button: string;
  enabled: boolean;
};

/** Report the create sheet's rendered state, or `null` on unmount — called
 *  by the sheet itself. */
export function reportCrazeCreate(dump: CrazeCreateDump | null): void {
  void invoke("ui_report", { snapshot: { craze_create: dump } });
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
/** Ask Rust to content-size the popover window to the measured height (no dead
 *  space). macOS-only in effect; a no-op on other targets. */
export async function resizePopover(height: number): Promise<void> {
  await invoke("resize_popover", { height });
}

/** Report the popover's compact rows so `tray.dump` can assert them (the hermetic
 *  content AC — OS tray clicks aren't drivable). The popover webview invokes this,
 *  so Rust keys it under the `popover` snapshot, never the dashboard's `main`. */
export function reportTray(rows: {
  running_sheds: { host: string; name: string }[];
}): void {
  void invoke("ui_report", { snapshot: { tray: rows } });
}
