/* shed desktop — the roost bootstrap surface (plan 019 §3.6, C8): "put a
 * roost-session on this host" for a machine or a shed card.
 *
 * The wire shapes below are hand-mirrored from the Rust side
 * (`desktop/tauri/src-tauri/src/roost_hosts.rs`'s `probe_json`/`plan_json`/
 * `installed_json`/`failure_json`) rather than generated — the same rule
 * `bridge.ts`'s other DTOs follow. `roost_probe`/`roost_preview`/
 * `roost_bootstrap` are `#[tauri::command]`s that land in the SAME
 * `RoostHosts` methods the harness drives over the `roost.*` socket ops
 * (`ipc.rs`), so what a cell proves about a bootstrap is what a click
 * actually runs.
 *
 * **Do not re-derive the plan matrix here.** `Plan.kind` and `actionable` are
 * roost's own classification (plan 019 §3.4); this module renders what they
 * say rather than recomputing them from the probe. The one pinned-copy rule
 * that matters to a renderer: [`Plan.message`] (the `report` row) and
 * [`RoostSource.sentence`] when unavailable are backend sentences and must
 * reach the screen VERBATIM, never paraphrased.
 */
import { useCallback, useEffect, useRef, useState } from "react";

/** A binary's self-report (`identify`), when one answered at all. */
export type RoostIdentity = {
  app_version: string;
  session_protocol: number;
  /** Absent on a `session.identify` answer (the running-session shape omits
   *  it) — only a binary's own `identify` carries it. */
  libghostty_build?: string;
};

/** What's on disk at the best candidate path, if anything. */
export type RoostProbeOutcome =
  | { kind: "compatible"; path: string; identity: RoostIdentity | null }
  | { kind: "mismatch"; path: string; identity: RoostIdentity | null }
  | { kind: "missing" };

/** Whether anything is currently serving. */
export type RoostSessionState =
  | { state: "running"; app_version: string; session_protocol: number; session_id: string; started_at: string }
  | { state: "no-session" }
  | { state: "not-installed" };

/** `roost.probe`'s read-only look at a host. */
export type RoostProbe = {
  outcome: RoostProbeOutcome;
  arch: string;
  home: string | null;
  session: RoostSessionState;
  candidates: string[];
  dest: string | null;
  /** Hashes (target, arch, outcome, session) — re-checked by `roost_bootstrap`
   *  so a card cannot act on a host that has since changed under it. */
  fingerprint: string;
};

/** The one action a probe implies — roost's plan matrix (plan 019 §3.4),
 *  read verbatim off the wire. `kind` is the button's identity; `install` /
 *  `update` / `start` are the only ones with a button at all. */
export type RoostPlan =
  | { kind: "install"; needs_source: true; dest: string | null }
  | {
      kind: "update";
      needs_source: true;
      path: string;
      incumbent: RoostIdentity | null;
      replaces_newer: boolean;
      dest: string | null;
    }
  | { kind: "start"; needs_source: false; path: string }
  | { kind: "up-to-date"; needs_source: false; session_protocol: number; app_version: string }
  // `message` is PINNED COPY (`bootstrap::copy::protocol_report`) — render it
  // verbatim, never reworded.
  | { kind: "report"; needs_source: false; session_protocol: number; message: string };

/** Where the bytes would come from, and whether there are any at all. */
export type RoostSource = {
  rung: string;
  /** The "from where" sentence. When [`available`] is false this IS the
   *  pinned `NoSource` copy (plan 019 §3.5) — render it verbatim in place of
   *  a button. */
  sentence: string;
  available: boolean;
  skipped: string[];
};

/** `roost.preview` — the probe, the plan, and the source: everything the
 *  status line, the button and the consent card are built from. */
export type RoostPreview = {
  target: string;
  probe: RoostProbe;
  plan: RoostPlan;
  source: RoostSource;
  /** The plan-matrix overlay (plan 019 §3.4's sixth row): true only when the
   *  plan itself is actionable AND (it needs no bytes OR the source has
   *  some). A card offers a button iff this is true. */
  actionable: boolean;
  client_label: string;
};

export type RoostHooksSkip = { agent: string; reason: string };
export type RoostHooksError = { agent: string; error: string };

/** `session.set_agent_hooks`'s result, always reported and never fatal. */
export type RoostHooksResult = {
  client: string;
  mode: string;
  applied: boolean;
  lease_held: boolean;
  skipped_code: string | null;
  wired: string[];
  refreshed: string[];
  removed: string[];
  skipped: RoostHooksSkip[];
  errors: RoostHooksError[];
  error: string | null;
};

/** A successful `roost.bootstrap`. */
export type RoostInstalled = {
  ok: true;
  target: string;
  plan: RoostPlan;
  dest: string | null;
  verdict: unknown;
  session_protocol: number | null;
  session_id: string | null;
  /** Pin P5: reported, never acted on. */
  path_warning: string | null;
  backup_warning: string | null;
  hooks: RoostHooksResult | null;
};

/** A refused `roost.bootstrap` — the STAGE is the payload (plan 019 §3.6):
 *  "prepare refused" and "the host changed under you" need different copy and
 *  different next steps, which is why this is an `ok` envelope carrying
 *  `ok: false` rather than a thrown error. `message` is pinned copy from
 *  `bootstrap::copy` — render it verbatim. */
export type RoostBootstrapFailure = {
  ok: false;
  error: { stage: string; message: string; restored: boolean | null };
};

export type RoostBootstrapResult = RoostInstalled | RoostBootstrapFailure;

async function invokeRoost<T>(cmd: string, args: Record<string, unknown>): Promise<T> {
  const core = await import("@tauri-apps/api/core");
  return core.invoke<T>(cmd, args);
}

/** `roost.probe {target}` — read-only, safe before consent. THROWS
 *  `"{stage}: {message}"` on a probe failure (an unreachable host, an
 *  unsupported OS, …) — the caller decides whether that is worth a line. */
export async function roostProbe(target: string): Promise<{ target: string; probe: RoostProbe; plan: RoostPlan }> {
  return invokeRoost("roost_probe", { target });
}

/** `roost.preview {target}` — the probe, the plan and the source sentence:
 *  the consent card's content. Nothing is resolved or fetched here (plan 019
 *  §3.5). THROWS `"{stage}: {message}"` on a probe failure. */
/** The backoff schedule for the "another Roost is connected" collision below —
 *  short, then longer, capped: the colliding reach is usually a one-shot
 *  `session.identify` that lets go quickly, but the retry survives it taking
 *  a few seconds on a loaded host without turning into a poll. */
const PREVIEW_COLLISION_BACKOFF_MS = [500, 1000, 2000, 4000];

export async function roostPreview(target: string): Promise<RoostPreview> {
  for (let attempt = 0; ; attempt++) {
    try {
      return await invokeRoost("roost_preview", { target });
    } catch (e) {
      // A shed's OWN `RoostHosts::observe_sheds` probe (plan 019 §3.6) and
      // this card's preview are two independent SSH reaches to the same
      // host, and a shed that just became running is exactly when both
      // fire — the pane's `sheds.list` refresh triggers the backend's own
      // probe-then-watch pass at the same moment this preview is asked for.
      // When they land close enough together the reach layer reports
      // "another Roost is connected" even though it's this app's own
      // bookkeeping on the other end, not a real second client. Transient —
      // retried with backoff rather than surfaced as this card's error.
      const backoff = PREVIEW_COLLISION_BACKOFF_MS[attempt];
      if (backoff === undefined || !String(e).includes("another Roost is connected")) {
        throw e;
      }
      await new Promise((resolve) => setTimeout(resolve, backoff));
    }
  }
}

/** `roost.bootstrap {target, fingerprint, consent}` — install/update/start a
 *  `roost-session` on `target` and wire its agent hooks. Always sent with
 *  `consent: true` here: the card is the consent (plan 019 §3.5), so by the
 *  time this is called the dialog has already shown it. THROWS only on a
 *  caller bug (missing consent — cannot happen from this UI) or the outer
 *  10-minute budget; every HOST-side refusal (a stale fingerprint, a failed
 *  stage) answers as [`RoostBootstrapFailure`] instead. */
export async function roostBootstrap(target: string, fingerprint: string): Promise<RoostBootstrapResult> {
  return invokeRoost("roost_bootstrap", { target, fingerprint, consent: true });
}

/** The button's label for an ACTIONABLE plan — `null` for the three rows with
 *  no button (`up-to-date`, `report`, or blocked on `NoSource`). The plan
 *  matrix's own row names (plan 019 §3.4), not invented copy. */
export function roostButtonLabel(preview: RoostPreview): string | null {
  if (!preview.actionable) return null;
  switch (preview.plan.kind) {
    case "install":
      return "Install";
    case "update":
      return "Update";
    case "start":
      return "Start";
    default:
      return null;
  }
}

/** The one-line status a card shows beside (or instead of) the button.
 *
 *  `report` renders `plan.message` VERBATIM (pinned copy); a blocked
 *  Install/Update renders the source's `sentence` VERBATIM (the pinned
 *  `NoSource` copy, plan 019 §3.5) — this function never rewrites either.
 *  Every other line is this client's own short summary, because roost pins
 *  no sentence for it. */
export function roostStatusLine(preview: RoostPreview): string {
  const { plan } = preview;
  if (plan.kind === "report") return plan.message;
  if (plan.kind === "up-to-date") {
    return `roost-session ${plan.app_version} running (protocol ${plan.session_protocol})`;
  }
  if (!preview.actionable) return preview.source.sentence;
  switch (plan.kind) {
    case "install":
      return "roost-session not found";
    case "update":
      return plan.replaces_newer
        ? "roost-session on this host speaks a newer protocol — replacing is a downgrade"
        : "roost-session is a build this app can't read";
    case "start":
      return "roost-session installed, not running";
    default:
      return "";
  }
}

/** The consent card's four required lines (plan 019 §3.5): what, where, from
 *  where, and the hook-wiring sentence — plus the Update-only backup note.
 *  Client-owned copy (consent is client-owned, per §3.5): roost pins no
 *  sentence for any of these, unlike the status line's `report`/`NoSource`
 *  cases. Pure and target-independent of I/O so a harness cell (and this
 *  module's own eyes) can read it straight off a `RoostPreview`. */
export function roostConsentCopy(preview: RoostPreview): {
  action: "Install" | "Update" | "Start";
  what: string;
  where: string;
  from: string;
  hooks: string;
  backup: string | null;
} {
  const { plan, probe, source, target } = preview;
  const dest =
    plan.kind === "install" || plan.kind === "update"
      ? plan.dest ?? probe.dest ?? "~/.local/bin/roost-session"
      : plan.kind === "start"
        ? plan.path
        : (probe.dest ?? "~/.local/bin/roost-session");
  const action = plan.kind === "install" ? "Install" : plan.kind === "update" ? "Update" : "Start";
  const what =
    plan.kind === "install"
      ? `Install roost-session on ${target}.`
      : plan.kind === "update"
        ? `Replace the roost-session on ${target} that this app can't talk to.`
        : `Start the roost-session already on ${target}.`;
  return {
    action,
    what,
    where: `${target}, at ${dest}`,
    from: source.sentence,
    hooks:
      "roost-session will also wire its hooks into the agents already configured there — " +
      "claude, codex, cursor, opencode, grok — and nothing else.",
    backup: plan.kind === "update" ? `The file currently at ${dest} will be backed up before it's replaced.` : null,
  };
}

/** One card's fetched roost state — `undefined` before the first answer,
 *  `null` on a probe failure (rendered as unreachable rather than thrown). */
export type RoostCardState = {
  preview: RoostPreview | undefined;
  error: string | null;
  loading: boolean;
};

/** Fetch (and re-fetch on demand) `roost.preview` for one target.
 *
 *  Fetched ONCE per mount / target change, and again only on an explicit
 *  `refresh()` — never polled. A preview is an SSH round trip; polling every
 *  card on a fast interval is exactly the connection storm C7's commit fixed
 *  on the backend's own probe-then-watch path, and this hook would recreate
 *  it on the client side. `refresh()` is what a pane's Refresh button and a
 *  just-finished bootstrap call. */
export function useRoostPreview(target: string | null): RoostCardState & { refresh: () => void } {
  const [state, setState] = useState<RoostCardState>({ preview: undefined, error: null, loading: false });
  const gen = useRef(0);

  const load = useCallback(() => {
    if (!target) return;
    const mine = ++gen.current;
    setState((s) => ({ ...s, loading: true }));
    roostPreview(target).then(
      (preview) => {
        if (mine !== gen.current) return;
        setState({ preview, error: null, loading: false });
      },
      (e: unknown) => {
        if (mine !== gen.current) return;
        setState({ preview: undefined, error: String(e), loading: false });
      },
    );
  }, [target]);

  useEffect(() => {
    setState({ preview: undefined, error: null, loading: false });
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [target]);

  return { ...state, refresh: load };
}

/** Fetch `roost.preview` for a whole board of targets (a pane's cards) at
 *  once — re-fetched wholesale when the target list changes or `nonce`
 *  bumps (a pane Refresh, or a just-settled bootstrap), never polled, for
 *  the same reason [`useRoostPreview`] doesn't poll a single one. Superseded
 *  answers are dropped per-target, so a slow probe for one host can't
 *  overwrite a faster one that started after it. */
export function useRoostBoard(targets: string[], nonce: number): Record<string, RoostCardState> {
  const [board, setBoard] = useState<Record<string, RoostCardState>>({});
  const gens = useRef<Record<string, number>>({});
  const lastNonce = useRef(nonce);
  const key = targets.join("\u0001");

  useEffect(() => {
    let cancelled = false;
    const wanted = new Set(key ? key.split("\u0001") : []);
    const forceAll = lastNonce.current !== nonce;
    lastNonce.current = nonce;
    // Drop rows for targets that fell off the board (a machine/shed removed,
    // or a shed that stopped) — a stale entry would otherwise report a plan
    // for a host the pane no longer shows.
    setBoard((b) => {
      let changed = false;
      const next: Record<string, RoostCardState> = {};
      for (const [k, v] of Object.entries(b)) {
        if (wanted.has(k)) next[k] = v;
        else changed = true;
      }
      return changed ? next : b;
    });
    for (const target of wanted) {
      // Skip a target this board already has an answer (or an in-flight fetch)
      // for, unless this render is the explicit `nonce`-driven refresh —
      // re-fetching an unchanged target on every board-size change would race
      // it against its OWN prior in-flight probe (a real `ssh` reach to the
      // same host), and roost's reach layer refuses a second live connection
      // to itself.
      if (!forceAll && gens.current[target] !== undefined) continue;
      const mine = (gens.current[target] = (gens.current[target] ?? 0) + 1);
      setBoard((b) => ({ ...b, [target]: { preview: b[target]?.preview, error: null, loading: true } }));
      roostPreview(target).then(
        (preview) => {
          if (cancelled || gens.current[target] !== mine) return;
          setBoard((b) => ({ ...b, [target]: { preview, error: null, loading: false } }));
        },
        (e: unknown) => {
          if (cancelled || gens.current[target] !== mine) return;
          setBoard((b) => ({ ...b, [target]: { preview: undefined, error: String(e), loading: false } }));
        },
      );
    }
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, nonce]);

  return board;
}

/** The text a rendered node actually contains, or `""` when no such node is in
 *  the document.
 *
 *  **The anchor that keeps a `*.dump` an assertion about the UI.** Every field
 *  beside it is derived from React state, so on its own a dump proves only that
 *  the state was right: delete the component from the tree and every copy
 *  assertion still passes while nothing is on screen. `rendered` is read back off
 *  the DOM after the commit, so a cell that checks its copy appears in here is
 *  checking the screen. Call it from an EFFECT, never during render — during
 *  render the document still holds the previous commit. */
export function renderedText(selector: string): string {
  if (typeof document === "undefined") return "";
  return document.querySelector(selector)?.textContent ?? "";
}

/** Report the Sheds pane's per-shed roost rows (mounted-only, the
 *  `machines.dump`/`reportMachinesPane` rule — pass `null` on unmount). Read
 *  by `shed_roost.dump`. */
export function reportShedRoost(rows: Record<string, RoostDumpRow> | null): void {
  void invokeFireAndForget("ui_report", { snapshot: { shed_roost: rows } });
}

/** One row's roost state as a dump reads it — every field the harness needs
 *  to assert the rendered affordance without a screenshot. */
export type RoostDumpRow = {
  target: string;
  status: string | null;
  button: string | null;
  busy: string | null;
  error: string | null;
  /** The row's own DOM text ([`renderedText`]) — `""` when no `RoostLine` for
   *  this target is on screen, which is what stops the four fields above from
   *  being a parallel data structure. Filled by the reporting effect. */
  rendered?: string;
};

/** Build a [`RoostDumpRow`] from a board entry — the SAME data [`RoostLine`]
 *  renders from, so a dump can never claim a word the card doesn't show. */
export function roostDumpRow(target: string, state: RoostCardState | undefined, busy: string | undefined): RoostDumpRow {
  return {
    target,
    status: state?.preview ? roostStatusLine(state.preview) : null,
    button: state?.preview ? roostButtonLabel(state.preview) : null,
    busy: busy ?? null,
    error: state?.error ?? null,
  };
}

/** The consent dialog's rendered copy, as `roost_consent.dump` reads it.
 *
 *  `rendered` is the card's own DOM text ([`renderedText`]), which is what makes
 *  this a report about a card a person can see rather than about `App`'s state. */
export type RoostConsentDump = { target: string; rendered: string } & ReturnType<
  typeof roostConsentCopy
>;

/** Report the consent card's rendered copy, or `null` to clear it.
 *
 *  **Called by [`RoostConsentDialog`] itself** (the `LanePanel`/`reportLane`
 *  rule), not by whatever owns the open-card state: a report made beside the JSX
 *  rather than inside it would keep answering after the JSX was deleted. */
export function reportRoostConsent(dump: RoostConsentDump | null): void {
  void invokeFireAndForget("ui_report", { snapshot: { roost_consent: dump } });
}

/** The toast as `toast.dump` reads it — its tone and lines, plus the DOM text
 *  they were actually painted into. */
export type RoostToastDump = RoostToast & { rendered: string };

/** Report the toast, or `null` to clear it. **Called by [`Toast`] itself**, for
 *  the reason [`reportRoostConsent`] gives. */
export function reportToast(toast: RoostToastDump | null): void {
  void invokeFireAndForget("ui_report", { snapshot: { toast } });
}

async function invokeFireAndForget(cmd: string, args: Record<string, unknown>): Promise<void> {
  if (typeof window === "undefined" || !("__TAURI_INTERNALS__" in window)) return;
  const core = await import("@tauri-apps/api/core");
  await core.invoke(cmd, args).catch(() => undefined);
}

/** The indeterminate progress phrases a running bootstrap cycles through
 *  (plan 019 §3.6's `detail` examples), in order for the CONFIRMED plan.
 *  There is no server-pushed progress — `roost.bootstrap` is one blocking
 *  call (plan 019 §3.6: "synchronous request/reply") — so this is honestly
 *  decorative: it tells a waiting user what stage the confirmed plan implies
 *  is happening, advancing on a client-owned timer rather than claiming to
 *  know the host's real state mid-call. */
export function roostProgressSteps(kind: RoostPlan["kind"]): string[] {
  if (kind === "install" || kind === "update") {
    return ["installing roost-session…", "starting…", "wiring agent hooks…"];
  }
  if (kind === "start") return ["starting…", "wiring agent hooks…"];
  return ["probing…"];
}

/** A toast's rendered content after a bootstrap settles — the PATH warning
 *  and the hook results (plan 019 §3.6), or the failure's own pinned message.
 *  Every string here comes from the backend answer verbatim; this only picks
 *  which ones to show and in what tone. */
export type RoostToast = { tone: "ok" | "warn" | "error"; lines: string[] };

export function roostToastFor(result: RoostBootstrapResult): RoostToast {
  if (!result.ok) {
    return { tone: "error", lines: [result.error.message] };
  }
  const lines: string[] = [`roost-session started on ${result.target}.`];
  if (result.path_warning) lines.push(result.path_warning);
  if (result.backup_warning) lines.push(result.backup_warning);
  const hooks = result.hooks;
  let tone: RoostToast["tone"] = "ok";
  if (hooks) {
    if (hooks.skipped_code) {
      lines.push(`Hooks not wired: ${hooks.skipped_code}.`);
      tone = "warn";
    } else {
      if (hooks.wired.length) lines.push(`Hooks wired: ${hooks.wired.join(", ")}.`);
      if (hooks.refreshed.length) lines.push(`Hooks refreshed: ${hooks.refreshed.join(", ")}.`);
      if (hooks.skipped.length) {
        lines.push(`Skipped: ${hooks.skipped.map((s) => `${s.agent} (${s.reason})`).join(", ")}.`);
      }
      if (hooks.errors.length) {
        lines.push(`Hook errors: ${hooks.errors.map((e) => `${e.agent}: ${e.error}`).join(", ")}.`);
        tone = "warn";
      }
      if (hooks.error) {
        lines.push(hooks.error);
        tone = "warn";
      }
    }
  }
  return { tone: result.path_warning ? "warn" : tone, lines };
}
