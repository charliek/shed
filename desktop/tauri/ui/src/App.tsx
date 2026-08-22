/* shed desktop — the app shell (Plex reskin). Sidebar + header + the six panes,
   over live shed-core data. Nav is driven by clicks AND by the Rust `ui.navigate`
   op (via the `navigate` Tauri event); the rendered pane + a computed-style sample
   are reported back to Rust (useUiBridge) so the harness can assert them over IPC.
   The shared visual vocabulary lives in components/{primitives,dialog}. */
import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import {
  Boxes, Shield, Sparkles, ScrollText, Globe, HardDrive, Box, Plus,
  Terminal, RotateCw, Square, Play, Trash2, RefreshCw, ExternalLink, Key,
  Fingerprint, Moon, Sun, Settings, X,
} from "lucide-react";
import { cn } from "@/lib/utils";
import owlOrange from "@/assets/owl-orange.svg";
import owlAmber from "@/assets/owl-amber.svg";
import {
  cardCls, Dot, StatusChip, Tag, ImageChip, KindBadge, ActBtn,
  PageHead, HeadAction, RefreshHeadButton, Empty, agentColor, type Tone,
} from "@/components/primitives";
import { Scrim, DialogShell, Field, Select, Segmented, dialogInput, dialogBtnSecondary, useEscClose } from "@/components/dialog";
import {
  useUiBridge, shedAction, fetchSystemDf, openTerminal,
  createStart, createStatus, createCancel, fetchHosts,
  fetchApprovals, decideApproval, fetchActivity, fetchGateNamespaces,
  fetchEgressProfiles, reportEgress, inTauri,
  openPreferences, setAppearanceState,
  rcLaunch, killSession, sessionKey, reportAgents, useRcSessions,
  useCoordinatorData, useNowTick, shedsEmptyState,
  type Pane, type Shed, type HostDiskUsage, type HostFailure,
  type Modal, type CreateProgress, type Approval, type AuditEntry,
  type EgressProfile, type EgressProfileInfo, type HostEgressProfiles, type EgressReport,
  type RcSession, type RcKind, type RcState,
  type RcCapabilities, type MachineStatus, offeredKinds, rcAuthHint,
} from "@/lib/bridge";

/** "server/shed" when multi-server, else the shed name. */
function qualifiedShed(server: string | null | undefined, shed: string | null | undefined): string {
  return server ? `${server}/${shed ?? ""}` : (shed ?? "");
}
/** HH:mm:ss of an ISO/flexible timestamp (best-effort; the raw string on a miss). */
function shortTime(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime())
    ? ts
    : d.toLocaleTimeString(undefined, { hour12: false });
}
/** The one-line activity detail: `op · server/shed · detail`. */
function activityDetail(e: AuditEntry): string {
  return [e.op, qualifiedShed(e.server, e.shed), e.detail].filter(Boolean).join(" · ");
}
/** The hint under an approval card, reflecting the grant its Approve applies. */
function approveHint(scope: Approval["default_scope"]): string {
  if (scope === "per-session") return "Approve grants for this session";
  if (scope === "per-shed") return "Approve grants for this shed";
  return "Approve allows this request only";
}
const NAV: [Pane, string, typeof Box][] = [
  ["sheds", "Sheds", Boxes],
  ["approvals", "Approvals", Shield],
  ["agents", "Agents", Sparkles],
  ["activity", "Activity", ScrollText],
  ["egress", "Egress", Globe],
  ["system", "System", HardDrive],
];

/* ---- live sheds ----------------------------------------------------------- */
/** Group sheds by host, each group + its rows in first-seen order. */
function groupByHost(sheds: Shed[]): [string, Shed[]][] {
  const groups: [string, Shed[]][] = [];
  for (const s of sheds) {
    const g = groups.find(([h]) => h === s.host);
    if (g) g[1].push(s);
    else groups.push([s.host, [s]]);
  }
  return groups;
}

/** A one-line spec summary (falls back to just the status when specs are absent). */
function metaLine(s: Shed): string {
  const bits: string[] = [];
  if (s.cpus) bits.push(`${s.cpus} vCPU`);
  if (s.memory_mb) bits.push(`${Math.round(s.memory_mb / 1024)} GB`);
  bits.push(s.status);
  return bits.join(" · ");
}

/** Human-readable bytes for the System pane (matches the mock's "Zero KB" empties). */
function formatBytes(n: number): string {
  if (n <= 0) return "Zero KB";
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v.toFixed(v < 10 ? 2 : 0)} ${units[i]}`;
}

/** Uppercase mono host label above a group of cards. */
function HostLabel({ host }: { host: string }) {
  return (
    <div className="mb-3 pl-0.5 font-mono text-[11px] font-semibold uppercase leading-none tracking-[.09em] text-shed-text-muted">{host}</div>
  );
}

/* ---- Sheds pane ----------------------------------------------------------- */
/** The per-host failure strip above the shed list: one row per host that couldn't
 *  be listed, leading with the remedy (`summary`), the full context on hover
 *  (`detail`). Without it a failed host is silently absent from the pane and the
 *  user reads an empty dashboard as "nothing to see" (shed#300). */
function HostErrorStrip({ errors }: { errors: HostFailure[] }) {
  return (
    <div className="mb-[18px] flex flex-col gap-2">
      {errors.map((e) => (
        <div key={e.server} title={e.detail} className={cn(cardCls, "flex items-start gap-2.5 px-[18px] py-3")}>
          <Dot className="mt-[5px] h-2 w-2 flex-none" style={{ background: "var(--shed-danger)" }} />
          <div className="min-w-0 break-words font-mono text-[12px] leading-relaxed" style={{ color: "var(--shed-danger)" }}>{e.summary}</div>
        </div>
      ))}
    </div>
  );
}

function ShedsPane({ sheds, hostErrors, refresh, onNew }: { sheds: Shed[]; hostErrors: HostFailure[]; refresh: () => void; onNew: () => void }) {
  const act = (action: string, s: Shed) => void shedAction(action, s.name, s.host).then(refresh);
  // The empty state defers to a host failure when there is one — same helper the
  // bridge reports as UI truth, so the pane and `dashboard.dump.empty` agree.
  const empty = shedsEmptyState(sheds, hostErrors);
  return (
    <div>
      <PageHead
        title="Sheds"
        right={
          <div className="flex items-center gap-[18px]">
            <RefreshHeadButton onClick={refresh} />
            <HeadAction icon={Plus} label="New shed" color="var(--shed-accent)" onClick={onNew} />
          </div>
        }
      />
      {hostErrors.length > 0 && <HostErrorStrip errors={hostErrors} />}
      {empty ? (
        <Empty icon={Boxes} title={empty.title} body={empty.body} />
      ) : (
        groupByHost(sheds).map(([host, rows]) => (
          <div key={host} className="mb-[22px] last:mb-0">
            <HostLabel host={host} />
            <div className="flex flex-col gap-3">
              {rows.map((s) => {
                const running = s.status === "running";
                const busy = s.status === "restarting" || s.status === "starting" || s.status === "stopping";
                const dotColor = running ? "var(--shed-ok)" : busy ? "var(--shed-attention)" : "var(--shed-text-muted)";
                return (
                  <div key={`${host}/${s.name}`} className={cn(cardCls, "flex items-center gap-3.5 px-[18px] py-[15px]")} style={{ animation: "shed-in .25s ease" }}>
                    <Dot className="h-2.5 w-2.5" style={{ background: dotColor }} />
                    <div className="min-w-0 flex-1">
                      <div className="mb-1.5 flex flex-wrap items-center gap-2.5">
                        <span className="text-[16px] font-semibold text-shed-text">{s.name}</span>
                        {s.backend && <Tag kind={s.backend} />}
                        {s.image && <ImageChip>{s.image}</ImageChip>}
                      </div>
                      <div className="font-mono text-[12.5px] leading-tight text-shed-text-muted">{metaLine(s)}</div>
                    </div>
                    <div className="flex gap-2">
                      {running ? (
                        <>
                          <ActBtn icon={Terminal} tone="neutral" title="Open in Terminal" onClick={() => void openTerminal(s.name, s.host)} />
                          <ActBtn icon={RotateCw} tone="attention" title="Restart" onClick={() => act("reset", s)} />
                          <ActBtn icon={Square} tone="danger" title="Stop" onClick={() => act("stop", s)} />
                        </>
                      ) : busy ? (
                        <ActBtn icon={RotateCw} tone="attention" spin disabled />
                      ) : (
                        <>
                          <ActBtn icon={Play} tone="ok" title="Start" onClick={() => act("start", s)} />
                          <ActBtn icon={Trash2} tone="danger" title="Delete" onClick={() => act("delete", s)} />
                        </>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        ))
      )}
    </div>
  );
}

/* ---- Approvals pane ------------------------------------------------------- */
function ApprovalsPane({ approvals }: { approvals: Approval[] }) {
  const now = useNowTick();
  return (
    <div>
      <PageHead
        title="Credential approvals"
        sub="Requests routed from shed-host-agent when its approval mode is shed-desktop."
        right={<span className="text-[14px] text-shed-text-muted">gate: shed-desktop</span>}
      />
      {approvals.length === 0 ? (
        <Empty icon={Shield} title="No pending requests" body="When an agent asks the host to use a credential, it shows up here for you to approve or deny." />
      ) : (
        <div className="flex flex-col gap-3">
          {approvals.map((a) => {
            const t = Date.parse(a.expires_at);
            // A malformed/absent expiry parses to NaN; the backend treats it as
            // already-expired (fail-closed), so show 0s rather than "NaNs".
            const secs = Number.isFinite(t) ? Math.max(0, Math.round((t - now) / 1000)) : 0;
            const biometric = a.gate !== "none";
            return (
              <div key={a.id} className={cn(cardCls, "p-5")} style={{ animation: "shed-in .25s ease" }}>
                <div className="flex items-start gap-[15px]">
                  <div className="flex h-11 w-11 flex-none items-center justify-center rounded-[11px]" style={{ background: "var(--shed-tag-vz-bg)" }}>
                    <Key size={20} style={{ color: "var(--shed-tag-vz-text)" }} />
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="text-[18px] font-bold text-shed-text">{a.namespace} · {a.op}</div>
                    <div className="mt-1.5 text-[14px] leading-snug text-shed-text-muted">shed {qualifiedShed(a.server, a.shed)} · {a.detail}</div>
                  </div>
                  <span className="flex-none text-[14px] font-semibold" style={{ color: secs <= 10 ? "var(--shed-danger)" : "var(--shed-attention)" }}>expires in {secs}s</span>
                </div>
                <div className="mt-[18px] flex items-center">
                  <span className="flex-1 text-[13px] text-shed-text-muted">{approveHint(a.default_scope)}</span>
                  <div className="flex gap-[11px]">
                    <button onClick={() => void decideApproval(a.id, "deny")} className="hbtn rounded-[10px] px-5 py-[11px] text-[15px] font-semibold" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)", border: "none" }}>Deny</button>
                    <button
                      onClick={() => void decideApproval(a.id, "approve", { scope: a.default_scope, ttl: a.default_ttl })}
                      className="hbtn inline-flex items-center gap-[9px] rounded-[10px] px-[22px] py-[11px] text-[15px] font-semibold"
                      style={{ background: "var(--shed-approve)", color: "var(--shed-approve-fg)", border: "none" }}
                    >
                      {biometric && <Fingerprint size={18} />} Approve
                    </button>
                  </div>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

/* ---- Agents / remote-control ---------------------------------------------- */
/** Human labels for the creatable kinds (the gated set is capability-derived). */
const RC_KIND_LABELS: Record<string, string> = {
  "claude-rc": "Claude", codex: "Codex", opencode: "opencode",
  cursor: "Cursor", shell: "Shell",
};
const rcKindLabel = (k: RcKind): string => RC_KIND_LABELS[k] ?? k;

/** State → status-chip tone: green ready, red dead, amber for in-progress /
    needs-action (the mapping the pane has always used, now on the shared vocab). */
function rcStateTone(state: RcState): Tone {
  if (state === "ready") return "ok";
  if (state === "dead") return "danger";
  return "attention";
}

function AgentsPane({ sessions, machines, onLaunch, refresh }:
  { sessions: RcSession[]; machines: MachineStatus[]; onLaunch: () => void; refresh: () => void }) {
  const [error, setError] = useState<string | null>(null);

  // Refresh the shared RC state on mount so navigating to the pane re-lists (the
  // shared state, so the sidebar badge stays consistent — one source of truth).
  useEffect(() => { refresh(); }, [refresh]);
  // Publish the rendered sessions so the `agents.dump` op can observe them. This
  // effect stays IN the pane so reportAgents fires ONLY while the pane is mounted
  // (the backend blanks the agents snapshot off-pane).
  useEffect(() => { reportAgents(sessions); }, [sessions]);

  return (
    <div>
      <PageHead
        title="Agents"
        accessory={<span className="font-mono text-[13px] text-shed-text-muted">{sessions.length} active</span>}
        right={
          <div className="flex items-center gap-[18px]">
            <RefreshHeadButton onClick={refresh} />
            <HeadAction icon={Plus} label="New session" color="var(--shed-accent)" onClick={onLaunch} />
          </div>
        }
      />
      {error && (
        <div className={cn(cardCls, "mb-3 flex items-start gap-2 p-3.5 text-[13px]")} style={{ borderColor: "var(--shed-danger)", color: "var(--shed-danger)" }}>
          <X size={16} className="mt-px flex-none" /> <span className="min-w-0 break-words">{error}</span>
        </div>
      )}
      {sessions.length === 0 && machines.length === 0 ? (
        <Empty icon={Sparkles} title="No agents running" body="Launch an agent — a REPL, a shell, or a coding agent — inside a shed. Sessions keep running after you disconnect." />
      ) : (
        <div className="flex flex-col gap-3">
          {sessions.map((s) => (
            // Keyed by ORIGIN, not host/shed: a machine session's shed is empty
            // by construction, so two machines sharing a slug would otherwise
            // collide into one React key.
            <SessionCard key={sessionKey(s)} session={s} onKilled={refresh} onError={setError} />
          ))}
          {/* A configured machine that is unreachable gets a row even with no
              sessions — that row IS the information ("mini3 is asleep"), and it
              is a normal state rather than an error. */}
          {machines.filter((m) => !m.reachable).map((m) => (
            <MachineDownCard key={m.origin} machine={m} />
          ))}
        </div>
      )}
    </div>
  );
}

/** A configured machine that is not currently reachable.
 *
 *  Rendered as an informational row, NOT an error: a machine that is asleep,
 *  off-network, or simply not running a hub is the normal case. The `detail`
 *  is shown verbatim because "no route to host" and "nothing is listening on
 *  1029" are different problems with different fixes, and flattening them to
 *  "offline" would throw away the only actionable part. */
function MachineDownCard({ machine: m }: { machine: MachineStatus }) {
  return (
    <div
      className={cn(cardCls, "flex items-center gap-4 px-[18px] py-4")}
      style={{ opacity: 0.7 }}
      data-machine={m.name}
    >
      <StatusChip tone="attention" label={m.connected_once ? "unreachable" : "connecting"} />
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex flex-wrap items-center gap-2.5">
          <span className="text-[16px] font-semibold text-shed-text">{m.name}</span>
          <span className="rounded bg-shed-inset px-1.5 py-0.5 font-mono text-[10px] font-semibold text-shed-text-muted">
            {m.origin}
          </span>
        </div>
        <div className="truncate font-mono text-[12px] text-shed-text-muted">
          {m.detail ?? "waiting for the machine's activity hub"}
        </div>
      </div>
    </div>
  );
}

function SessionCard({ session: s, onKilled, onError }: { session: RcSession; onKilled: () => void; onError: (e: string) => void }) {
  const [busy, setBusy] = useState(false);
  const claude = s.kind === "claude-rc" || s.kind === "claude-broker";
  const machine = s.origin_kind === "machine";
  const sub = s.state === "needs-auth"
    ? rcAuthHint(s.kind)
    : [`tmux ${s.tmux_session}`, s.workdir, s.created_by].filter(Boolean).join(" · ");
  const kill = async () => {
    setBusy(true);
    // Routes by origin — a machine session is addressed by (machine, slug), a
    // shed session by (host, shed, slug). One entry point so the distinction
    // lives in the bridge rather than at every call site.
    try { await killSession(s); onKilled(); }
    catch (e) { onError(String(e)); setBusy(false); }
  };
  return (
    <div
      className={cn(cardCls, "flex items-center gap-4 px-[18px] py-4")}
      // A stale row is the last KNOWN state of a machine that is currently
      // unreachable — dimmed rather than hidden, because "mini3 is asleep and
      // these were its sessions" is more useful than an empty list.
      style={{ animation: "shed-in .25s ease", opacity: s.stale ? 0.55 : 1 }}
    >
      <StatusChip tone={rcStateTone(s.state)} label={s.state} />
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex flex-wrap items-center gap-2.5">
          <span className="text-[16px] font-semibold text-shed-text">{s.display_name}</span>
          <KindBadge kind={s.kind} />
          {machine && (
            <span
              className="rounded bg-shed-inset px-1.5 py-0.5 font-mono text-[10px] font-semibold text-shed-text-muted"
              title={`Reached over SSH to ${s.machine}, not through a shed server`}
            >
              {s.origin}
            </span>
          )}
          {!s.managed && <span className="rounded bg-shed-inset px-1.5 py-0.5 font-mono text-[10px] font-semibold text-shed-text-muted">legacy</span>}
        </div>
        <div className="truncate font-mono text-[12px] text-shed-text-muted">
          {s.stale ? `${sub} · last known` : sub}
        </div>
      </div>
      <div className="flex flex-none items-center gap-2">
        {s.url && (
          <a href={s.url} target="_blank" rel="noreferrer" className="hbtn inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-[12px] font-medium" style={{ background: "var(--shed-accent-subtle)", border: "1px solid var(--shed-accent-border)", color: "var(--shed-accent)" }}>
            <ExternalLink size={14} /> Open in Claude
          </a>
        )}
        {/* The terminal opener resolves a SHED's ssh endpoint through the
            server; a machine session has no shed, so it is not offered rather
            than offered-and-broken. */}
        {!machine && (
          <button
            onClick={() => void openTerminal(s.shed, s.host, s.tmux_session)}
            title={claude ? "Open the session in a terminal" : "Open in Terminal"}
            className="hbtn inline-flex items-center rounded-[9px] px-[18px] py-2.5 font-mono text-[13px] font-medium"
            style={{ background: "var(--shed-btn-dark)", color: "var(--shed-btn-dark-fg)", border: "none" }}
          >
            {">_ open"}
          </button>
        )}
        <ActBtn icon={Trash2} tone="danger" title="End session" onClick={() => void kill()} disabled={busy} spin={busy} />
      </div>
    </div>
  );
}

/* ---- Activity pane -------------------------------------------------------- */
/** One audit row (mono time + ns pill + detail + outcome) — shared by the
    Activity pane and the Egress pane's Activity sub-tab. */
function ActivityRow({ entry: e, divider }: { entry: AuditEntry; divider: boolean }) {
  const ok = e.result === "ok" || e.result === "approved";
  return (
    <div className={cn("row-hover flex items-center gap-3.5 px-[18px] py-[13px]", divider && "border-t border-shed-border")}>
      <span className="w-[70px] flex-none font-mono text-[13px] text-shed-text-muted">{shortTime(e.ts)}</span>
      {e.ns && <span className="flex-none rounded-md px-[7px] py-1 font-mono text-[11px] font-semibold leading-none" style={{ background: "var(--shed-agent-pill-bg)", color: "var(--shed-agent-pill-text)" }}>{e.ns}</span>}
      <span className="min-w-0 flex-1 truncate text-[14px] text-shed-text-secondary">{activityDetail(e)}</span>
      <span className="flex flex-none items-center gap-2">
        <span className="text-[13px] font-semibold" style={{ color: ok ? "var(--shed-ok)" : "var(--shed-denied)" }}>{e.result}</span>
        {e.approval && e.approval !== "none" && <span className="text-[12px] text-shed-text-muted">{e.approval}</span>}
      </span>
    </div>
  );
}

function ActivityPane() {
  const activity = useCoordinatorData<AuditEntry[]>("activity-changed", fetchActivity, []);
  return (
    <div>
      <PageHead
        title="Activity"
        sub="Host-agent credential audit + shed-desktop decisions, newest first."
      />
      {activity.length === 0 ? (
        <Empty icon={ScrollText} title="No activity yet" body="Credential checks and your approval decisions will appear here." />
      ) : (
        <div className={cn(cardCls, "overflow-hidden")}>
          {activity.map((e, i) => (
            <ActivityRow key={`${e.id}-${i}`} entry={e} divider={i > 0} />
          ))}
        </div>
      )}
    </div>
  );
}

/* ---- Egress pane (mac parity: Activity feed filter + Profiles list→detail) - */
type EgressTab = "activity" | "profiles";
type EgressRow = { host: string; info: EgressProfileInfo };
/** Unique even if a host ever returns a config + user profile of the same name
    (the mac EgressProfilesView id shape). */
const egressRowId = (r: EgressRow) => `${r.host}/${r.info.source}/${r.info.name}`;

/** The config|user source badge (user = accent tint, config = neutral). */
function SourceBadge({ source }: { source: string }) {
  const user = source === "user";
  return (
    <span
      className="flex-none rounded-full px-2 py-0.5 font-mono text-[10px] font-semibold leading-none"
      style={{
        background: user ? "var(--shed-accent-subtle)" : "var(--shed-inset)",
        color: user ? "var(--shed-accent)" : "var(--shed-text-secondary)",
      }}
    >
      {source}
    </span>
  );
}

/** One-line profile summary for the list rows (mirrors the mac `summary`). */
function egressSummary(p: EgressProfile): string {
  const parts: string[] = [];
  if (p.mode) parts.push(`mode=${p.mode}`);
  if (p.allow?.length) parts.push(`allow=${p.allow.length}`);
  if (p.deny?.length) parts.push(`deny=${p.deny.length}`);
  if (p.rule) parts.push("rule");
  return parts.length ? parts.join(" ") : "(empty)";
}

/** A mono uppercase label + mono rows — the detail's allow/deny/mode/rule blocks. */
function EgressRuleBlock({ label, items }: { label: string; items: string[] }) {
  if (!items.length) return null;
  return (
    <div>
      <div className="mb-1.5 font-mono text-[10.5px] uppercase leading-none tracking-[.05em] text-shed-text-muted">{label}</div>
      {items.map((it) => (
        <div key={it} className="font-mono text-[13px] leading-relaxed text-shed-text">{it}</div>
      ))}
    </div>
  );
}

function EgressPane() {
  const [tab, setTab] = useState<EgressTab>("activity");
  const [hosts, setHosts] = useState<HostEgressProfiles[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  // A profile selection requested via `egress.show` that arrived before rows loaded
  // (the listener records it here rather than resolving against an empty list). The
  // auto-select effect applies it once rows land — see the listener + effect below.
  const [pendingSelect, setPendingSelect] = useState<{ profile: string; host?: string } | null>(null);
  // Egress decisions = audit entries with ns=="egress" — the same coordinator
  // feed the Activity pane renders, filtered frontend-side (mac parity).
  const activity = useCoordinatorData<AuditEntry[]>("activity-changed", fetchActivity, []);
  const egress = activity.filter((e) => e.ns === "egress");

  // Fetch per-host profiles on mount + Refresh (generation guard as in SystemPane).
  const gen = useRef(0);
  const load = useCallback(() => {
    const g = ++gen.current;
    void fetchEgressProfiles().then((r) => {
      if (g === gen.current) {
        setHosts(r);
        setLoaded(true);
      }
    });
  }, []);
  useEffect(() => load(), [load]);

  const rows: EgressRow[] = hosts.flatMap((h) => h.profiles.map((info) => ({ host: h.host, info })));
  const errorRows = hosts.filter((h) => h.error);
  const multiHost = hosts.length > 1;
  const selectedRow = rows.find((r) => egressRowId(r) === selected) ?? null;

  // Auto-select the first profile once rows land (list→detail renders without a
  // click), and re-select if a refresh dropped the selected one. A pending
  // `egress.show` selection (recorded before rows loaded) takes precedence.
  useEffect(() => {
    if (!rows.length) return;
    if (pendingSelect) {
      const m = rows.find(
        (r) =>
          r.info.name === pendingSelect.profile &&
          (pendingSelect.host === undefined || r.host === pendingSelect.host),
      );
      setPendingSelect(null); // applied, or rows loaded without a match — consume it
      if (m) {
        setSelected(egressRowId(m));
        return;
      }
    }
    if (!rows.some((r) => egressRowId(r) === selected)) {
      setSelected(egressRowId(rows[0]));
    }
  }, [hosts, selected, pendingSelect]); // eslint-disable-line react-hooks/exhaustive-deps

  // Build the current rendered snapshot. Held in a ref so the listener effect can
  // publish the INITIAL report the instant it attaches (before any data-driven
  // re-report), which keeps the readiness invariant below true.
  const buildReport = useCallback(
    (): EgressReport => ({
      tab,
      profiles: rows.map((r) => ({ host: r.host, name: r.info.name, source: r.info.source })),
      errors: errorRows.map((h) => ({ host: h.host, error: h.error ?? "" })),
      selected: selectedRow
        ? {
            host: selectedRow.host,
            name: selectedRow.info.name,
            source: selectedRow.info.source,
            allow: selectedRow.info.profile.allow ?? [],
            deny: selectedRow.info.profile.deny ?? [],
            mode: selectedRow.info.profile.mode ?? null,
            rule: selectedRow.info.profile.rule ?? null,
          }
        : null,
      activity_count: egress.length,
    }),
    [tab, rows, errorRows, selectedRow, egress.length],
  );
  const reportRef = useRef(buildReport);
  reportRef.current = buildReport;

  // Readiness invariant (mirrors useUiBridge): the egress-show LISTENER is attached
  // BEFORE the pane publishes ANY egress snapshot, so a non-null reported snapshot
  // proves the listener is live — which `egress.show` relies on to fail-fast
  // (frontend_not_ready) rather than lose an emit to the async attach race. `ready`
  // gates the data-driven re-report below; this effect owns the initial report.
  const ready = useRef(false);
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    const uns: Array<() => void> = [];
    void import("@tauri-apps/api/event").then(async ({ listen }) => {
      uns.push(
        await listen<{ tab?: unknown; profile?: unknown; host?: unknown }>("egress-show", (e) => {
          const t = e.payload?.tab;
          if (t === "activity" || t === "profiles") setTab(t);
          const p = e.payload?.profile;
          if (typeof p === "string") {
            const h = e.payload?.host;
            // Record as pending (never resolve against a possibly-empty rows list
            // here): the auto-select effect applies it by (host, name) — or by name
            // (first match) when no host is given — once rows are present.
            setPendingSelect({ profile: p, host: typeof h === "string" ? h : undefined });
          }
        }),
      );
      if (cancelled) {
        uns.forEach((u) => u());
        return;
      }
      ready.current = true; // listener live → the snapshot may now publish
      reportEgress(reportRef.current()); // initial report AFTER the listener attaches
    });
    return () => {
      cancelled = true;
      ready.current = false;
      uns.forEach((u) => u());
      // Clear the reported snapshot on unmount so an off-pane or freshly-remounted-
      // but-not-yet-reported `egress.profiles` dump returns null, never stale data.
      reportEgress(null);
    };
  }, []);

  // Publish the rendered egress state so the `egress.profiles` op can observe it
  // (UI truth). Stays IN the pane so it fires only while mounted, like reportAgents;
  // gated on `ready` so the listener effect owns the initial report.
  useEffect(() => {
    if (!inTauri() || !ready.current) return;
    reportEgress(reportRef.current());
  }, [tab, hosts, selected, egress.length]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div>
      <PageHead
        title="Egress"
        sub={tab === "activity"
          ? "Per-shed network egress decisions (allow/deny), newest first. View-only — egress policy is configured on the server."
          : "Egress profiles on each server (config baseline + user-managed). View-only — manage them with `shed egress profile`."}
        right={
          <div className="flex items-center gap-[18px]">
            {tab === "profiles" && <HeadAction icon={RefreshCw} label="Refresh" color="var(--shed-accent)" onClick={load} />}
            <div className="w-[210px]">
              <Segmented
                options={[["activity", "Activity"], ["profiles", "Profiles"]]}
                value={tab}
                set={(v) => setTab(v as EgressTab)}
              />
            </div>
          </div>
        }
      />
      {tab === "activity" ? (
        egress.length === 0 ? (
          <Empty
            icon={Globe}
            title="No egress activity yet"
            body="Enable egress control on a server and create a shed with --egress to see allow/deny decisions here."
          />
        ) : (
          <div className={cn(cardCls, "overflow-hidden")}>
            {egress.map((e, i) => (
              <ActivityRow key={`${e.id}-${i}`} entry={e} divider={i > 0} />
            ))}
          </div>
        )
      ) : !loaded ? (
        <div className="px-1 py-8 text-center text-[13px] text-shed-text-muted">Loading egress profiles…</div>
      ) : rows.length === 0 ? (
        errorRows.length === 0 ? (
          <Empty
            icon={Globe}
            title="No egress profiles"
            body="Define them in the server config or with `shed egress profile set`. Requires egress enabled on the server."
          />
        ) : (
          // Surface host failures (egress disabled / unreachable) instead of a
          // misleading "no profiles" when every fetch actually errored.
          <div className={cn(cardCls, "flex flex-col gap-2 px-6 py-8 text-center")}>
            <div className="text-[14px] font-semibold text-shed-text">Couldn't load egress profiles</div>
            {errorRows.map((h) => (
              <div key={h.host} className="break-words font-mono text-[12px] leading-relaxed" style={{ color: "var(--shed-danger)" }}>
                {h.host}: {h.error}
              </div>
            ))}
          </div>
        )
      ) : (
        <div className="flex items-start gap-4">
          {/* list: profile name + source badge (+ host when multi-host, + error rows) */}
          <div className={cn(cardCls, "w-[280px] flex-none overflow-hidden")}>
            {rows.map((r, i) => {
              const id = egressRowId(r);
              const active = id === selected;
              return (
                <button
                  key={id}
                  onClick={() => setSelected(id)}
                  className={cn("row-hover block w-full px-3.5 py-2.5 text-left", i > 0 && "border-t border-shed-border")}
                  style={{ background: active ? "var(--shed-accent-subtle)" : "transparent" }}
                >
                  <span className="flex items-center gap-2">
                    <span className="min-w-0 truncate text-[13.5px] font-semibold text-shed-text">{r.info.name}</span>
                    <SourceBadge source={r.info.source} />
                  </span>
                  {multiHost && <span className="mt-0.5 block font-mono text-[10.5px] text-shed-text-muted">{r.host}</span>}
                  <span className="mt-0.5 block truncate font-mono text-[11px] text-shed-text-muted">{egressSummary(r.info.profile)}</span>
                </button>
              );
            })}
            {errorRows.map((h) => (
              <div key={h.host} className="break-words border-t border-shed-border px-3.5 py-2.5 font-mono text-[11px] leading-relaxed" style={{ color: "var(--shed-danger)" }}>
                {h.host}: {h.error}
              </div>
            ))}
          </div>
          {/* detail: the selected profile's rules */}
          <div className={cn(cardCls, "min-w-0 flex-1 px-[19px] py-[17px]")}>
            {selectedRow ? (
              <div className="flex flex-col gap-4">
                <div className="flex items-center gap-2.5">
                  <span className="text-[18px] font-bold text-shed-text">{selectedRow.info.name}</span>
                  <SourceBadge source={selectedRow.info.source} />
                  {multiHost && <span className="font-mono text-[12px] text-shed-text-muted">{selectedRow.host}</span>}
                </div>
                <EgressRuleBlock label="allow" items={selectedRow.info.profile.allow ?? []} />
                <EgressRuleBlock label="deny" items={selectedRow.info.profile.deny ?? []} />
                {selectedRow.info.profile.mode && <EgressRuleBlock label="mode" items={[selectedRow.info.profile.mode]} />}
                {selectedRow.info.profile.rule && <EgressRuleBlock label="rule" items={[selectedRow.info.profile.rule]} />}
              </div>
            ) : (
              <div className="py-10 text-center text-[13px] text-shed-text-muted">Select a profile</div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

/* ---- System pane ---------------------------------------------------------- */
function SystemPane({ sheds }: { sheds: Shed[] }) {
  const [rows, setRows] = useState<HostDiskUsage[]>([]);
  // Generation guard (as in useUiBridge's fetchSheds): a slower earlier Refresh
  // can't resolve last and overwrite a newer fetch with stale rows.
  const gen = useRef(0);
  const load = useCallback(() => {
    const g = ++gen.current;
    void fetchSystemDf().then((r) => {
      if (g === gen.current) setRows(r);
    });
  }, []);
  useEffect(() => load(), [load]);
  const summary = (h: HostDiskUsage): string => {
    if (!h.usage) return "unreachable";
    const list = sheds.filter((s) => s.host === h.host);
    const running = list.filter((s) => s.status === "running").length;
    return `${list.length} ${list.length === 1 ? "shed" : "sheds"}${running ? ` · ${running} running` : ""}`;
  };
  return (
    <div>
      <PageHead
        title="System"
        sub="Disk usage per host (images, sheds, snapshots, orphans)."
        right={<HeadAction icon={RefreshCw} label="Refresh" color="var(--shed-accent)" onClick={load} />}
      />
      <div className="flex flex-col gap-3">
        {rows.map((h) => {
          const t = h.usage?.totals;
          return (
            <div key={h.host} className={cn(cardCls, "px-[19px] py-[17px]")}>
              <div className={cn("flex items-center gap-2.5", t ? "mb-4" : "mb-3")}>
                <Dot className="h-2.5 w-2.5" style={{ background: h.usage ? "var(--shed-ok)" : "var(--shed-text-muted)" }} />
                <span className="text-[16px] font-semibold text-shed-text">{h.host}</span>
                {h.usage?.backend && <Tag kind={h.usage.backend} />}
                <span className="font-mono text-[12px] text-shed-text-secondary">{summary(h)}</span>
                <span className="flex-1" />
                {t && <span className="font-mono text-[17px] font-bold text-shed-text">{formatBytes(t.all.physical_bytes)}</span>}
              </div>
              {t ? (
                <div className="grid max-w-[560px] grid-cols-4 gap-2.5">
                  {(
                    [
                      ["Images", t.images],
                      ["Sheds", t.sheds],
                      ["Snapshots", t.snapshots],
                      ["Orphans", t.orphans],
                    ] as const
                  ).map(([label, size]) => (
                    <div key={label}>
                      <div className="mb-1.5 font-mono text-[10.5px] uppercase leading-none tracking-[.05em] text-shed-text-muted">{label}</div>
                      <div className="font-mono text-[13.5px] font-medium text-shed-text">{formatBytes(size.physical_bytes)}</div>
                    </div>
                  ))}
                </div>
              ) : (
                <div className="break-words font-mono text-[12px] leading-relaxed" style={{ color: "var(--shed-danger)" }}>{h.error}</div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

/* ---- New-session (launch agent) dialog ------------------------------------ */
/** Per-kind one-line help under the Kind picker. */
function kindHelp(kind: RcKind): string {
  if (kind === "shell") return "plain bash in the shed workspace";
  if (kind === "claude-rc") return "live claude REPL with /rc";
  return "a remote coding-agent session";
}

function LaunchAgentDialog({ sheds, capabilities, refresh, onClose, onLaunched }:
  { sheds: Shed[]; capabilities: Record<string, RcCapabilities>; refresh: () => void; onClose: () => void; onLaunched: () => void }) {
  const fid = useId(); // base for per-field control ids (label↔control association)
  const running = sheds.filter((s) => s.status === "running");
  const [target, setTarget] = useState(running[0] ? `${running[0].host}/${running[0].name}` : "");
  const [kind, setKind] = useState<RcKind>("claude-rc");
  const [displayName, setDisplayName] = useState("");
  const [prompt, setPrompt] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The Kind picker gates on the SHARED capabilities (the same rc.list object that
  // feeds the badge + pane). The dialog is App-level (drivable via ui.show_launch
  // from any pane), so refresh on open to gate on current capabilities.
  useEffect(() => { refresh(); }, [refresh]);

  // Close on Escape (like the other dialogs), but not while a launch is in flight
  // — mirrors NewShedDialog's `!creating` guard (here the in-flight flag is `busy`).
  useEscClose(onClose, !busy);

  const kinds = offeredKinds(capabilities[target]);
  // Keep the selection valid when the shed changes OR its offered kinds change.
  useEffect(() => { if (kinds.length && !kinds.includes(kind)) setKind(kinds[0]); }, [target, kinds.join(",")]); // eslint-disable-line react-hooks/exhaustive-deps

  const shedOpts = running.map((s) => ({ value: `${s.host}/${s.name}`, label: qualifiedShed(s.host, s.name) }));
  const shell = kind === "shell";
  const canCreate = !!target && kinds.length > 0;

  const submit = async () => {
    const sel = running.find((s) => `${s.host}/${s.name}` === target);
    if (!sel) { setError("Pick a running shed to launch in."); return; }
    setBusy(true);
    setError(null);
    try {
      await rcLaunch({
        shed: sel.name,
        host: sel.host,
        kind,
        displayName: displayName.trim() || undefined,
        initialPrompt: prompt.trim() || undefined,
      });
      onLaunched();
    } catch (e) {
      setError(String(e));
      setBusy(false);
    }
  };

  return (
    <Scrim onClose={onClose} mark="launch">
      <DialogShell
        icon={Sparkles}
        title="New session"
        sub="Start a remote-control session inside a shed."
        onClose={onClose}
        footer={
          <>
            <button onClick={onClose} className={dialogBtnSecondary}>Cancel</button>
            <button
              onClick={() => void submit()}
              disabled={!canCreate || busy}
              className="hbtn inline-flex items-center gap-2 rounded-[9px] px-[22px] py-2.5 text-[14px] font-semibold"
              style={{
                background: canCreate ? "var(--shed-accent)" : "var(--shed-inset)",
                color: canCreate ? "var(--shed-accent-fg)" : "var(--shed-text-muted)",
                border: "none",
                opacity: busy ? 0.7 : 1,
                cursor: canCreate && !busy ? "pointer" : "default",
              }}
            >
              <Sparkles size={16} /> {busy ? "Launching…" : "Create"}
            </button>
          </>
        }
      >
        <Field label="Shed" htmlFor={shedOpts.length ? `${fid}-shed` : undefined}>
          {shedOpts.length ? (
            <Select id={`${fid}-shed`} value={target} onChange={setTarget} options={shedOpts} />
          ) : (
            <div className="rounded-lg border border-shed-border bg-shed-bg px-3 py-2.5 text-[13px] leading-snug text-shed-text-muted">No running sheds. Start a shed first.</div>
          )}
        </Field>
        <Field label="Session name" hint="optional" htmlFor={`${fid}-name`}>
          <input id={`${fid}-name`} value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="defaults to shed/slug" className={dialogInput} />
        </Field>
        <Field label="Kind" help={kinds.length ? kindHelp(kind) : undefined}>
          {kinds.length === 0 ? (
            <div className="px-1 py-2 text-[13px] text-shed-text-muted">no agent kinds available in this shed</div>
          ) : (
            <Segmented options={kinds.map((k) => [k, rcKindLabel(k), agentColor(k)] as [string, string, string])} value={kind} set={(v) => setKind(v as RcKind)} />
          )}
        </Field>
        <Field
          label={shell ? "Initial command" : "Initial prompt"}
          hint="optional"
          htmlFor={`${fid}-prompt`}
          help={shell ? "Run in the shell once it's ready." : "Typed into the agent once it's ready."}
        >
          <textarea id={`${fid}-prompt`} value={prompt} onChange={(e) => setPrompt(e.target.value)} rows={2} placeholder={shell ? "npm install && npm test" : "summarize this repo"} className={cn(dialogInput, "resize-none")} />
        </Field>
        {error && (
          <div className="rounded-md px-3 py-2 font-mono text-[12px]" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}>{error}</div>
        )}
      </DialogShell>
    </Scrim>
  );
}

/* ---- New-shed (create dialog with live SSE progress) ---------------------- */
const BACKEND_OPTS = [
  { value: "", label: "Default" },
  { value: "vz", label: "vz" },
  { value: "firecracker", label: "firecracker" },
];

function NewShedDialog({ refresh, onClose }: { refresh: () => void; onClose: () => void }) {
  const fid = useId(); // base for per-field control ids (label↔control association)
  const [hosts, setHosts] = useState<string[]>([]);
  const [name, setName] = useState("");
  const [host, setHost] = useState("");
  const [image, setImage] = useState("");
  const [vmBackend, setVmBackend] = useState("");
  const [cpus, setCpus] = useState("");
  const [memGb, setMemGb] = useState("");
  const [repo, setRepo] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [createId, setCreateId] = useState<string | null>(null);
  const [progress, setProgress] = useState<CreateProgress | null>(null);
  const [error, setError] = useState<string | null>(null);

  const creating = createId !== null;
  const state = progress?.state;

  // The configured hosts a create can target (even ones with no sheds yet).
  useEffect(() => {
    void fetchHosts().then((hs) => {
      setHosts(hs);
      setHost(hs[0] ?? "");
    });
  }, []);

  // Poll progress while a create is in flight (pull-based, like the harness).
  useEffect(() => {
    if (!createId) return;
    let live = true;
    let timer: number | undefined;
    const tick = async () => {
      const p = await createStatus(createId);
      if (!live) return;
      if (p) setProgress(p);
      if (p?.state === "complete") {
        refresh();
        return;
      }
      if (p?.state === "error") {
        setError(p.error ?? "create failed");
        return;
      }
      timer = window.setTimeout(tick, 600);
    };
    void tick();
    return () => {
      live = false;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [createId, refresh]);

  useEscClose(onClose, !creating);

  const submit = async () => {
    if (!name.trim() || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      const id = await createStart({
        name: name.trim(),
        host: host || undefined,
        image: image.trim() || undefined,
        vm_backend: vmBackend || undefined,
        cpus: cpus ? Math.round(Number(cpus)) : undefined,
        memory_mb: memGb ? Math.round(Number(memGb) * 1024) : undefined,
        repo: repo.trim() || undefined,
      });
      setCreateId(id);
    } catch (e) {
      setError(String(e));
    } finally {
      setSubmitting(false);
    }
  };

  const cancel = () => {
    if (createId && state !== "complete") void createCancel(createId);
    onClose();
  };

  const hostOpts = hosts.length ? hosts.map((h) => ({ value: h, label: h })) : [{ value: "", label: "(default)" }];
  const canCreate = name.trim().length > 0 && !submitting;

  const footer = !creating ? (
    <>
      <button onClick={onClose} className={dialogBtnSecondary}>Cancel</button>
      <button
        onClick={() => void submit()}
        disabled={!canCreate}
        className="hbtn inline-flex items-center gap-2 rounded-[9px] px-[22px] py-2.5 text-[14px] font-semibold"
        style={{ background: "var(--shed-accent)", color: "var(--shed-accent-fg)", border: "none", opacity: canCreate ? 1 : 0.5, cursor: canCreate ? "pointer" : "default" }}
      >
        <Plus size={16} /> Create shed
      </button>
    </>
  ) : state === "complete" ? (
    <>
      <span />
      <button onClick={onClose} className="hbtn rounded-[9px] px-[22px] py-2.5 text-[14px] font-semibold" style={{ background: "var(--shed-accent)", color: "var(--shed-accent-fg)", border: "none" }}>Done</button>
    </>
  ) : (
    <>
      <span />
      <button onClick={cancel} className="hbtn rounded-[9px] px-[22px] py-2.5 text-[14px] font-semibold" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)", border: "none" }}>Cancel</button>
    </>
  );

  return (
    <Scrim onClose={() => { if (!creating) onClose(); }} mark="create">
      <DialogShell icon={Plus} title="New shed" sub="Spin up a dev environment on a host." onClose={cancel} width={540} footer={footer}>
        {!creating ? (
          <>
            <Field label="Name" htmlFor={`${fid}-name`}>
              <input id={`${fid}-name`} autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="my-shed" className={cn(dialogInput, "font-mono")} />
            </Field>
            <div className="flex gap-4">
              <div className="min-w-0 flex-1">
                <Field label="Host" htmlFor={`${fid}-host`}><Select id={`${fid}-host`} value={host} onChange={setHost} options={hostOpts} /></Field>
              </div>
              <div className="min-w-0 flex-1">
                <Field label="Backend" htmlFor={`${fid}-backend`}><Select id={`${fid}-backend`} value={vmBackend} onChange={setVmBackend} options={BACKEND_OPTS} /></Field>
              </div>
            </div>
            <Field label="Image" htmlFor={`${fid}-image`}>
              <input id={`${fid}-image`} value={image} onChange={(e) => setImage(e.target.value)} placeholder="e.g. base" className={cn(dialogInput, "font-mono")} />
            </Field>
            <div className="flex gap-4">
              <div className="flex-1">
                <Field label="CPUs" htmlFor={`${fid}-cpus`}><input id={`${fid}-cpus`} type="number" min="1" step="1" value={cpus} onChange={(e) => setCpus(e.target.value)} placeholder="2" className={dialogInput} /></Field>
              </div>
              <div className="flex-1">
                <Field label="Memory (GB)" htmlFor={`${fid}-mem`}><input id={`${fid}-mem`} type="number" min="1" step="1" value={memGb} onChange={(e) => setMemGb(e.target.value)} placeholder="4" className={dialogInput} /></Field>
              </div>
            </div>
            <Field label="Repo" hint="optional" htmlFor={`${fid}-repo`}>
              <input id={`${fid}-repo`} value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="owner/repo" className={cn(dialogInput, "font-mono")} />
            </Field>
            {error && (
              <div className="rounded-md px-3 py-2 font-mono text-[12px]" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}>{error}</div>
            )}
          </>
        ) : (
          <>
            <div className="flex items-center gap-2.5 text-[14px] font-semibold">
              {state === "complete" ? (
                <span style={{ color: "var(--shed-ok)" }}>✓ {name} created</span>
              ) : state === "error" ? (
                <span style={{ color: "var(--shed-danger)" }}>Create failed</span>
              ) : (
                <span className="inline-flex items-center gap-2 text-shed-text-secondary">
                  <RefreshCw size={15} className="animate-spin" /> Creating {name}…
                </span>
              )}
            </div>
            <div className="max-h-[280px] overflow-auto rounded-[10px] border border-shed-border bg-shed-inset p-3 font-mono text-[12px] leading-relaxed text-shed-text-secondary">
              {(progress?.messages ?? []).map((m, i) => (
                <div key={i}>{m}</div>
              ))}
              {error && <div style={{ color: "var(--shed-danger)" }}>{error}</div>}
              {(progress?.messages ?? []).length === 0 && !error && <div className="text-shed-text-muted">Starting…</div>}
            </div>
          </>
        )}
      </DialogShell>
    </Scrim>
  );
}

/* ---- shell ---------------------------------------------------------------- */
export default function App() {
  const [pane, setPane] = useState<Pane>("sheds");
  const [mode, setMode] = useState<"light" | "dark">("light");
  const [modal, setModal] = useState<Modal>(null);
  // The SINGLE source of truth for RC sessions + capabilities: the sidebar badge,
  // the Agents pane, and the launch dialog all read this one `rc.list` state, so
  // they can't diverge. `refreshRc` reloads on the pane Refresh button, a
  // launch/kill, and pane/dialog open; the hook itself reloads on mount + `refresh`.
  const { sessions: rcSessions, capabilities: rcCapabilities, machines: rcMachines, refresh: refreshRc } = useRcSessions();
  // Live approval queue (drives the badge + the pane) + the delegated namespaces
  // (a non-empty set = the host agent handshook, so it's connected).
  const approvals = useCoordinatorData<Approval[]>("approvals-changed", fetchApprovals, []);
  const gateNs = useCoordinatorData<string[]>("connected-changed", fetchGateNamespaces, []);
  const connected = gateNs.length > 0;
  const pending = approvals.length;
  // The Agents sidebar badge count is derived from the shared session list — always
  // consistent with what the Agents pane renders.
  const agentCount = rcSessions.length;
  // Configured hosts WITH reachability — the System pane's source (`system_df`),
  // lifted to the shell so the sidebar HOSTS list + the System nav badge cover the
  // configured hosts (even ones with zero sheds), each with a real reachable/
  // unreachable dot — instead of deriving from `sheds` (which hides zero-shed hosts,
  // undercounts the badge, and paints every dot green). Reuses the existing `refresh`
  // event that drives the shed re-fetch — no extra poll.
  const [hostRows, setHostRows] = useState<HostDiskUsage[]>([]);
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let unlisten: (() => void) | undefined;
    const reload = () => void fetchSystemDf().then((r) => { if (!cancelled) setHostRows(r); });
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
  }, []);
  const hostCount = hostRows.length;
  // The bridge owns the single full `ui_report`; hand it the badge inputs (sheds
  // derived there, hosts/agents/pending from here) and the mode so the report is one
  // complete snapshot, never a partial that races the `main` slot / tray count.
  const { sheds, hostErrors, refresh } = useUiBridge(pane, setPane, modal, mode, agentCount, hostCount, pending);

  // Apply the appearance to the root BEFORE the bridge's report effect samples it (a
  // layout effect runs ahead of that passive effect), so `ui.computed_style` reflects
  // the flip. Also mirror the mode into the Rust AppearanceState (which broadcasts
  // `set-appearance` to every window, so the Preferences window follows) — it fires
  // on both the manual header toggle and IPC-driven changes, and the Rust side
  // no-ops (no re-emit) when unchanged, so the listener→effect loop can't echo.
  useLayoutEffect(() => {
    document.documentElement.dataset.mode = mode;
    void setAppearanceState(mode);
  }, [mode]);

  // Open the modals both from the buttons and from the ui.show_create /
  // ui.show_launch IPC ops (events); drive dark/light from ui.set_appearance —
  // so the harness can drive + screenshot each deterministically. (Preferences is
  // a dedicated window now — opened via the open_preferences command, no event.)
  useEffect(() => {
    if (typeof window === "undefined" || !("__TAURI_INTERNALS__" in window)) return;
    const uns: Array<() => void> = [];
    let cancelled = false;
    void import("@tauri-apps/api/event").then(async ({ listen }) => {
      uns.push(await listen("show-create", () => setModal("create")));
      uns.push(await listen("show-launch", () => setModal("launch")));
      uns.push(
        await listen<{ mode?: unknown }>("set-appearance", (e) => {
          if (e.payload?.mode === "light" || e.payload?.mode === "dark") setMode(e.payload.mode);
        }),
      );
      if (cancelled) uns.forEach((u) => u());
    });
    return () => {
      cancelled = true;
      uns.forEach((u) => u());
    };
  }, []);

  // A successful launch: close the dialog, jump to Agents, and refresh the shared
  // RC state so the pane re-lists + the sidebar count updates together.
  const onLaunched = () => {
    setModal(null);
    refreshRc();
    setPane("agents");
  };

  // Sidebar hosts = the configured hosts with reachability (from system_df), NOT the
  // sheds-derived set — so zero-shed hosts still appear, the System badge counts
  // configured hosts, and each dot reflects real reachability.
  // …each carrying the shed-listing failure for that host (if any) as its dot
  // tooltip — the remedy plus the full detail the strip demotes to hover.
  const hosts = hostRows.map((h) => ({
    name: h.host,
    reachable: h.usage != null,
    failure: hostErrors.find((e) => e.server === h.host),
  }));

  return (
    <div className="flex h-full">
      {/* sidebar */}
      <aside className="flex w-[244px] flex-none flex-col border-r border-shed-border bg-shed-bg-sidebar px-3 pb-3.5 pt-4">
        <div className="flex items-center gap-2.5 px-2 pb-4 pt-0.5">
          <img src={mode === "dark" ? owlAmber : owlOrange} width={26} height={22} alt="" />
          <span className="text-[19px] font-bold leading-none text-shed-text" style={{ letterSpacing: "-.01em" }}>Shed</span>
        </div>
        {NAV.map(([id, label, Icon]) => {
          const active = pane === id;
          const isApprovals = id === "approvals";
          const badge =
            id === "sheds" ? sheds.length
            : id === "agents" ? agentCount
            : id === "system" ? hosts.length
            : isApprovals && pending ? pending : null;
          const alert = isApprovals && pending > 0;
          return (
            <button
              key={id}
              onClick={() => setPane(id)}
              className={cn("nav-item mb-0.5 flex w-full items-center gap-[11px] rounded-[9px] px-[11px] py-2 text-left text-[13.5px] font-semibold", active && "is-active")}
              style={{
                background: active ? "color-mix(in srgb, var(--shed-accent) 14%, transparent)" : "transparent",
                color: active ? "var(--shed-accent)" : "var(--shed-text-secondary)",
              }}
            >
              <Icon size={18} className="flex-none" style={{ color: active ? "var(--shed-accent)" : "var(--shed-text-secondary)" }} />
              <span className="flex-1">{label}</span>
              {badge != null && (
                <span
                  className="rounded-full px-2 py-0.5 font-mono text-[11px] font-semibold leading-none"
                  style={{ background: alert ? "var(--shed-deny-bg)" : "var(--shed-inset)", color: alert ? "var(--shed-danger)" : "var(--shed-text-secondary)" }}
                >
                  {badge}
                </span>
              )}
            </button>
          );
        })}
        <div className="flex-1" />
        <div className="flex items-center px-2.5 pb-2 pt-0.5">
          <span className="font-mono text-[10px] font-semibold tracking-[.1em] text-shed-text-muted">HOSTS</span>
        </div>
        {hosts.map((h) => (
          <div
            key={h.name}
            title={h.failure ? `${h.failure.summary}\n\n${h.failure.detail}` : undefined}
            className="flex items-center gap-2.5 px-2.5 py-1.5 text-[13px] font-medium text-shed-text"
          >
            <Dot className="h-2 w-2" style={{ background: h.reachable ? "var(--shed-ok)" : "var(--shed-text-muted)" }} />
            <span className="truncate">{h.name}</span>
          </div>
        ))}
      </aside>

      {/* main column */}
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-[52px] flex-none items-center gap-3 border-b border-shed-border bg-shed-bg px-[22px]">
          <Box size={19} className="text-shed-text-secondary" />
          <span className="text-[14px] font-semibold text-shed-text">shed desktop</span>
          <div className="flex-1" />
          {pending > 0 && (
            <button onClick={() => setPane("approvals")} className="hlink inline-flex items-center gap-1.5 rounded-lg px-[9px] py-1.5 text-[13px] font-semibold" style={{ color: "var(--shed-danger)" }}>
              <Shield size={15} /> {pending} pending
            </button>
          )}
          <span className="inline-flex items-center gap-2 text-[13px] font-medium text-shed-text-secondary">
            <Dot className="h-2 w-2" style={{ background: connected ? "var(--shed-ok)" : "var(--shed-text-muted)" }} /> host agent · {connected ? "connected" : "connecting…"}
          </span>
          <button onClick={() => void openPreferences()} title="Preferences" className="hlink flex h-[34px] w-[34px] items-center justify-center rounded-[9px] text-shed-text-muted">
            <Settings size={18} />
          </button>
          <button onClick={() => setMode(mode === "light" ? "dark" : "light")} title="Toggle appearance" className="hlink flex h-[34px] w-[34px] items-center justify-center rounded-[9px] text-shed-text-muted">
            {mode === "light" ? <Moon size={18} /> : <Sun size={18} />}
          </button>
        </header>
        <main className="flex-1 overflow-auto bg-shed-bg px-[38px] pb-6 pt-7">
          <div data-pane={pane} className="mx-auto max-w-[880px]">
            {pane === "sheds" && <ShedsPane sheds={sheds} hostErrors={hostErrors} refresh={refresh} onNew={() => setModal("create")} />}
            {pane === "approvals" && <ApprovalsPane approvals={approvals} />}
            {pane === "agents" && <AgentsPane sessions={rcSessions} machines={rcMachines} onLaunch={() => setModal("launch")} refresh={refreshRc} />}
            {pane === "activity" && <ActivityPane />}
            {pane === "egress" && <EgressPane />}
            {pane === "system" && <SystemPane sheds={sheds} />}
          </div>
        </main>
      </div>
      {modal === "create" && <NewShedDialog refresh={refresh} onClose={() => setModal(null)} />}
      {modal === "launch" && <LaunchAgentDialog sheds={sheds} capabilities={rcCapabilities} refresh={refreshRc} onClose={() => setModal(null)} onLaunched={onLaunched} />}
    </div>
  );
}
