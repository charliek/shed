/* shed desktop — the app shell (Plex reskin). Sidebar + header + the five panes,
   over live shed-core data. Nav is driven by clicks AND by the Rust `ui.navigate`
   op (via the `navigate` Tauri event); the rendered pane + a computed-style sample
   are reported back to Rust (useUiBridge) so the harness can assert them over IPC.
   The shared visual vocabulary lives in components/{primitives,dialog}. */
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import {
  Boxes, Shield, Sparkles, ScrollText, HardDrive, Box, Plus,
  Terminal, RotateCw, Square, Play, Trash2, RefreshCw, ExternalLink, Key,
  Fingerprint, Moon, Sun, Settings, X,
} from "lucide-react";
import { cn } from "@/lib/utils";
import owlOrange from "@/assets/owl-orange.svg";
import owlAmber from "@/assets/owl-amber.svg";
import {
  cardCls, Dot, StatusChip, Tag, ImageChip, KindBadge, ActBtn,
  PageHead, HeadAction, Empty, agentColor, type Tone,
} from "@/components/primitives";
import { Scrim, DialogShell, Field, Select, Segmented, Switch, dialogInput, dialogBtnSecondary, useEscClose } from "@/components/dialog";
import {
  useUiBridge, shedAction, fetchSystemDf, openTerminal,
  fetchTerminalPresets, getPrefs, setTerminalPref,
  getLoginItem, setLoginItem,
  createStart, createStatus, createCancel, fetchHosts,
  fetchApprovals, decideApproval, fetchActivity, fetchGateNamespaces,
  getSshApproval, setSshApproval,
  rcLaunch, rcKill, reportAgents, useRcSessions,
  useCoordinatorData, useNowTick,
  type Pane, type Shed, type HostDiskUsage, type TerminalPresetInfo,
  type Modal, type CreateProgress, type Approval, type AuditEntry, type SshPrefs,
  type RcSession, type RcKind, type RcState,
  type RcCapabilities, offeredKinds, rcAuthHint,
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
function ShedsPane({ sheds, refresh, onNew }: { sheds: Shed[]; refresh: () => void; onNew: () => void }) {
  const act = (action: string, s: Shed) => void shedAction(action, s.name, s.host).then(refresh);
  return (
    <div>
      <PageHead title="Sheds" right={<HeadAction icon={Plus} label="New shed" color="var(--shed-accent)" onClick={onNew} />} />
      {sheds.length === 0 ? (
        <Empty icon={Boxes} title="No sheds yet" body="No sheds on the configured hosts. Create one to spin up an isolated dev environment." />
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

function AgentsPane({ sessions, onLaunch, refresh }:
  { sessions: RcSession[]; onLaunch: () => void; refresh: () => void }) {
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
            <button onClick={() => refresh()} title="Refresh" className="hlink flex items-center rounded-lg p-[7px] text-shed-text-secondary">
              <RefreshCw size={18} />
            </button>
            <HeadAction icon={Plus} label="New session" color="var(--shed-accent)" onClick={onLaunch} />
          </div>
        }
      />
      {error && (
        <div className={cn(cardCls, "mb-3 flex items-start gap-2 p-3.5 text-[13px]")} style={{ borderColor: "var(--shed-danger)", color: "var(--shed-danger)" }}>
          <X size={16} className="mt-px flex-none" /> <span className="min-w-0 break-words">{error}</span>
        </div>
      )}
      {sessions.length === 0 ? (
        <Empty icon={Sparkles} title="No agents running" body="Launch an agent — a REPL, a shell, or a coding agent — inside a shed. Sessions keep running after you disconnect." />
      ) : (
        <div className="flex flex-col gap-3">
          {sessions.map((s) => (
            <SessionCard key={`${s.host}/${s.shed}/${s.slug}`} session={s} onKilled={refresh} onError={setError} />
          ))}
        </div>
      )}
    </div>
  );
}

function SessionCard({ session: s, onKilled, onError }: { session: RcSession; onKilled: () => void; onError: (e: string) => void }) {
  const [busy, setBusy] = useState(false);
  const claude = s.kind === "claude-rc" || s.kind === "claude-broker";
  const sub = s.state === "needs-auth"
    ? rcAuthHint(s.kind)
    : [`tmux ${s.tmux_session}`, s.workdir, s.created_by].filter(Boolean).join(" · ");
  const kill = async () => {
    setBusy(true);
    try { await rcKill(s.shed, s.slug, s.host); onKilled(); }
    catch (e) { onError(String(e)); setBusy(false); }
  };
  return (
    <div className={cn(cardCls, "flex items-center gap-4 px-[18px] py-4")} style={{ animation: "shed-in .25s ease" }}>
      <StatusChip tone={rcStateTone(s.state)} label={s.state} />
      <div className="min-w-0 flex-1">
        <div className="mb-1 flex flex-wrap items-center gap-2.5">
          <span className="text-[16px] font-semibold text-shed-text">{s.display_name}</span>
          <KindBadge kind={s.kind} />
          {!s.managed && <span className="rounded bg-shed-inset px-1.5 py-0.5 font-mono text-[10px] font-semibold text-shed-text-muted">legacy</span>}
        </div>
        <div className="truncate font-mono text-[12px] text-shed-text-muted">{sub}</div>
      </div>
      <div className="flex flex-none items-center gap-2">
        {s.url && (
          <a href={s.url} target="_blank" rel="noreferrer" className="hbtn inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-[12px] font-medium" style={{ background: "var(--shed-accent-subtle)", border: "1px solid var(--shed-accent-border)", color: "var(--shed-accent)" }}>
            <ExternalLink size={14} /> Open in Claude
          </a>
        )}
        <button
          onClick={() => void openTerminal(s.shed, s.host, s.tmux_session)}
          title={claude ? "Open the session in a terminal" : "Open in Terminal"}
          className="hbtn inline-flex items-center rounded-[9px] px-[18px] py-2.5 font-mono text-[13px] font-medium"
          style={{ background: "var(--shed-btn-dark)", color: "var(--shed-btn-dark-fg)", border: "none" }}
        >
          {">_ open"}
        </button>
        <ActBtn icon={Trash2} tone="danger" title="End session" onClick={() => void kill()} disabled={busy} spin={busy} />
      </div>
    </div>
  );
}

/* ---- Activity pane -------------------------------------------------------- */
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
          {activity.map((e, i) => {
            const ok = e.result === "ok" || e.result === "approved";
            return (
              <div key={`${e.id}-${i}`} className={cn("row-hover flex items-center gap-3.5 px-[18px] py-[13px]", i && "border-t border-shed-border")}>
                <span className="w-[70px] flex-none font-mono text-[13px] text-shed-text-muted">{shortTime(e.ts)}</span>
                {e.ns && <span className="flex-none rounded-md px-[7px] py-1 font-mono text-[11px] font-semibold leading-none" style={{ background: "var(--shed-agent-pill-bg)", color: "var(--shed-agent-pill-text)" }}>{e.ns}</span>}
                <span className="min-w-0 flex-1 truncate text-[14px] text-shed-text-secondary">{activityDetail(e)}</span>
                <span className="flex flex-none items-center gap-2">
                  <span className="text-[13px] font-semibold" style={{ color: ok ? "var(--shed-ok)" : "var(--shed-denied)" }}>{e.result}</span>
                  {e.approval && e.approval !== "none" && <span className="text-[12px] text-shed-text-muted">{e.approval}</span>}
                </span>
              </div>
            );
          })}
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
        <Field label="Shed">
          {shedOpts.length ? (
            <Select value={target} onChange={setTarget} options={shedOpts} />
          ) : (
            <div className="rounded-lg border border-shed-border bg-shed-bg px-3 py-2.5 text-[13px] leading-snug text-shed-text-muted">No running sheds. Start a shed first.</div>
          )}
        </Field>
        <Field label="Session name" hint="optional">
          <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="defaults to shed/slug" className={dialogInput} />
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
          help={shell ? "Run in the shell once it's ready." : "Typed into the agent once it's ready."}
        >
          <textarea value={prompt} onChange={(e) => setPrompt(e.target.value)} rows={2} placeholder={shell ? "npm install && npm test" : "summarize this repo"} className={cn(dialogInput, "resize-none")} />
        </Field>
        {error && (
          <div className="rounded-md px-3 py-2 font-mono text-[12px]" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}>{error}</div>
        )}
      </DialogShell>
    </Scrim>
  );
}

/* ---- Preferences (in-app modal; the tray is Phase C) ---------------------- */
// How an SSH-key approval is confirmed. On Linux "Authenticate" routes through
// polkit; "Prompt only" needs no gate — a plain Approve button.
const APPROVAL_METHODS: { id: SshPrefs["method"]; label: string; detail: string }[] = [
  { id: "biometrics-or-password", label: "Authenticate", detail: "Confirm with your login password." },
  { id: "prompt", label: "Prompt only", detail: "A plain Approve button — no password." },
];

// The SSH approval policies, most → least permissive (mirrors SshApprovalPolicy in
// shed-core). `prompts` gates the Method picker and `usesDuration` gates the
// Duration field — the SAME policy→behavior mapping as the Swift app.
const SSH_POLICIES: { id: string; label: string; prompts: boolean; usesDuration: boolean }[] = [
  { id: "always-allow", label: "Always Allow", prompts: false, usesDuration: false },
  { id: "per-shed-allow", label: "Per Shed Allow", prompts: true, usesDuration: false },
  { id: "time-based-allow", label: "Time Based Allow", prompts: true, usesDuration: true },
  { id: "always-ask", label: "Always Ask", prompts: true, usesDuration: false },
  { id: "always-deny", label: "Always Deny", prompts: false, usesDuration: false },
];

const PREF_LABEL = "mb-1.5 font-mono text-[11px] font-semibold uppercase tracking-wider text-shed-text-muted";
const PREF_GROUP = "mb-5 overflow-hidden rounded-[10px] border border-shed-border bg-shed-surface";

function PreferencesModal({ onClose }: { onClose: () => void }) {
  const [presets, setPresets] = useState<TerminalPresetInfo[]>([]);
  const [preset, setPreset] = useState("custom");
  const [template, setTemplate] = useState("");
  const [method, setMethod] = useState<SshPrefs["method"]>("biometrics-or-password");
  const [policy, setPolicy] = useState("time-based-allow");
  const [ttl, setTtl] = useState("2h");
  const [launchAtLogin, setLaunchAtLogin] = useState(false);
  const [loginBusy, setLoginBusy] = useState(false);
  const sshGen = useRef(0);
  const ttlAtFocus = useRef(""); // the Duration value when the field gained focus

  useEffect(() => {
    void fetchTerminalPresets().then(setPresets);
    void getLoginItem().then(setLaunchAtLogin);
    void getPrefs().then((p) => {
      setPreset(p.terminal_preset);
      setTemplate(p.terminal_template);
    });
    // Guard the initial load with the same generation ref as applySsh: if the user
    // edits a pref before this slow first read resolves, the stale load must not
    // clobber their change (its optimistic set already bumped sshGen).
    const mine = ++sshGen.current;
    void getSshApproval().then((p) => {
      if (mine !== sshGen.current) return;
      setMethod(p.method);
      setPolicy(p.policy);
      setTtl(p.ttl);
    });
  }, []);

  useEscClose(onClose);

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
  const policyMeta = SSH_POLICIES.find((p) => p.id === policy);

  return (
    <Scrim onClose={onClose} mark="prefs">
      <DialogShell icon={Settings} title="Preferences" sub="Terminal, credential approvals, and launch behavior." onClose={onClose} width={480}>
        <div>
          <div className={PREF_LABEL}>General</div>
          <div className={PREF_GROUP}>
            <label className="flex items-center gap-3 px-3.5 py-2.5">
              <span className="text-[13px] text-shed-text">Launch at login</span>
              <span className="flex-1" />
              <span className="text-[12px] text-shed-text-muted">Open Shed Desktop when you sign in.</span>
              <Switch on={launchAtLogin} set={toggleLaunchAtLogin} disabled={loginBusy} />
            </label>
          </div>

          <div className={PREF_LABEL}>Terminal</div>
          <p className="mb-2 text-[12px] text-shed-text-muted">Which terminal opens when you click "Open in Terminal" on a shed.</p>
          <div className={PREF_GROUP}>
            {presets.map((p, i) => {
              const active = preset === p.id;
              return (
                <label
                  key={p.id}
                  className={cn("flex cursor-pointer items-center gap-3 px-3.5 py-2.5", i && "border-t border-shed-border", !p.available && "opacity-50")}
                  style={{ background: active ? "var(--shed-accent-subtle)" : undefined }}
                >
                  <input
                    type="radio"
                    name="terminal-preset"
                    checked={active}
                    disabled={!p.available}
                    onChange={() => choosePreset(p.id)}
                    style={{ accentColor: "var(--shed-accent)" }}
                  />
                  <span className="text-[13px] text-shed-text">{p.label}</span>
                  {!p.available && <span className="text-[12px] text-shed-text-muted">not installed</span>}
                  <span className="flex-1" />
                  {p.detail && <span className="truncate text-[12px] text-shed-text-muted">{p.detail}</span>}
                </label>
              );
            })}
          </div>
          {preset === "custom" && (
            <div className="-mt-3 mb-5">
              <input
                value={template}
                onChange={(e) => editTemplate(e.target.value)}
                placeholder="e.g. kitty -e {cmd}"
                className="w-full rounded-[9px] border border-shed-border bg-shed-surface px-3 py-2 font-mono text-[13px] text-shed-text outline-none focus:border-shed-accent"
              />
              <p className="mt-1.5 text-[12px] text-shed-text-muted">
                <code className="font-mono">{"{cmd}"}</code> is the ssh command, <code className="font-mono">{"{shed}"}</code> the shed name.
              </p>
            </div>
          )}

          <div className={PREF_LABEL}>Credential approvals</div>
          <p className="mb-2 text-[12px] text-shed-text-muted">What happens when the host agent routes an SSH-key approval here.</p>
          <div className="mb-2 overflow-hidden rounded-[10px] border border-shed-border bg-shed-surface">
            <label className="flex items-center gap-3 px-3.5 py-2.5">
              <span className="text-[13px] text-shed-text">Approval policy</span>
              <span className="flex-1" />
              <select
                value={policy}
                onChange={(e) => applySsh({ policy: e.target.value })}
                className="rounded-md border border-shed-border bg-shed-inset px-2 py-1 text-[13px] text-shed-text outline-none"
                data-ssh-policy
              >
                {SSH_POLICIES.map((p) => (
                  <option key={p.id} value={p.id}>{p.label}</option>
                ))}
              </select>
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
          </div>

          {/* Method — only the prompting policies confirm an approval (policy.prompts). */}
          {policyMeta?.prompts && (
            <>
              <div className={PREF_LABEL}>Method</div>
              <div className="mb-2 overflow-hidden rounded-[10px] border border-shed-border bg-shed-surface">
                {APPROVAL_METHODS.map((m, i) => {
                  const active = method === m.id;
                  return (
                    <label
                      key={m.id}
                      className={cn("flex cursor-pointer items-center gap-3 px-3.5 py-2.5", i && "border-t border-shed-border")}
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
              </div>
            </>
          )}
          <p className="text-[12px] leading-relaxed text-shed-text-muted">
            Always Allow / Always Deny decide every SSH sign with no prompt. The others prompt, then remember your approval per the policy. Changing the policy clears live grants. Method is how each approval is confirmed.
          </p>
        </div>
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
            <Field label="Name">
              <input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="my-shed" className={cn(dialogInput, "font-mono")} />
            </Field>
            <div className="flex gap-4">
              <div className="min-w-0 flex-1">
                <Field label="Host"><Select value={host} onChange={setHost} options={hostOpts} /></Field>
              </div>
              <div className="min-w-0 flex-1">
                <Field label="Backend"><Select value={vmBackend} onChange={setVmBackend} options={BACKEND_OPTS} /></Field>
              </div>
            </div>
            <Field label="Image">
              <input value={image} onChange={(e) => setImage(e.target.value)} placeholder="e.g. base" className={cn(dialogInput, "font-mono")} />
            </Field>
            <div className="flex gap-4">
              <div className="flex-1">
                <Field label="CPUs"><input type="number" min="1" step="1" value={cpus} onChange={(e) => setCpus(e.target.value)} placeholder="2" className={dialogInput} /></Field>
              </div>
              <div className="flex-1">
                <Field label="Memory (GB)"><input type="number" min="1" step="1" value={memGb} onChange={(e) => setMemGb(e.target.value)} placeholder="4" className={dialogInput} /></Field>
              </div>
            </div>
            <Field label="Repo" hint="optional">
              <input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="owner/repo" className={cn(dialogInput, "font-mono")} />
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
  const { sessions: rcSessions, capabilities: rcCapabilities, refresh: refreshRc } = useRcSessions();
  // Live approval queue (drives the badge + the pane) + the delegated namespaces
  // (a non-empty set = the host agent handshook, so it's connected).
  const approvals = useCoordinatorData<Approval[]>("approvals-changed", fetchApprovals, []);
  const gateNs = useCoordinatorData<string[]>("connected-changed", fetchGateNamespaces, []);
  const connected = gateNs.length > 0;
  const pending = approvals.length;
  // The Agents sidebar badge count is derived from the shared session list — always
  // consistent with what the Agents pane renders.
  const agentCount = rcSessions.length;
  // The bridge owns the single full `ui_report`; hand it the badge inputs (sheds +
  // hosts it derives itself, agents/pending from here) and the mode so the report is
  // one complete snapshot, never a partial that races the `main` slot / tray count.
  const { sheds, refresh } = useUiBridge(pane, setPane, modal, mode, agentCount, pending);

  // Apply the appearance to the root BEFORE the bridge's report effect samples it (a
  // layout effect runs ahead of that passive effect), so `ui.computed_style` reflects
  // the flip.
  useLayoutEffect(() => {
    document.documentElement.dataset.mode = mode;
  }, [mode]);

  // Open the modals both from the buttons and from the ui.show_preferences /
  // ui.show_create / ui.show_launch IPC ops (events); drive dark/light from
  // ui.set_appearance — so the harness can drive + screenshot each deterministically.
  useEffect(() => {
    if (typeof window === "undefined" || !("__TAURI_INTERNALS__" in window)) return;
    const uns: Array<() => void> = [];
    let cancelled = false;
    void import("@tauri-apps/api/event").then(async ({ listen }) => {
      uns.push(await listen("show-preferences", () => setModal("prefs")));
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

  // Sidebar hosts are the distinct hosts of the live sheds.
  const hosts = [...new Set(sheds.map((s) => s.host))];

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
        <div className="flex items-center justify-between px-2.5 pb-2 pt-0.5">
          <span className="font-mono text-[10px] font-semibold tracking-[.1em] text-shed-text-muted">HOSTS</span>
          <button
            onClick={() => setPane("system")}
            className="hlink inline-flex items-center gap-1 rounded-md px-1 py-0.5 font-mono text-[11px] font-semibold text-shed-accent"
          >
            <Plus size={13} /> Add
          </button>
        </div>
        {hosts.map((h) => (
          <div key={h} className="flex items-center gap-2.5 px-2.5 py-1.5 text-[13px] font-medium text-shed-text">
            <Dot className="h-2 w-2" style={{ background: "var(--shed-ok)" }} />
            <span className="truncate">{h}</span>
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
          <button onClick={() => setModal("prefs")} title="Preferences" className="hlink flex h-[34px] w-[34px] items-center justify-center rounded-[9px] text-shed-text-muted">
            <Settings size={18} />
          </button>
          <button onClick={() => setMode(mode === "light" ? "dark" : "light")} title="Toggle appearance" className="hlink flex h-[34px] w-[34px] items-center justify-center rounded-[9px] text-shed-text-muted">
            {mode === "light" ? <Moon size={18} /> : <Sun size={18} />}
          </button>
        </header>
        <main className="flex-1 overflow-auto bg-shed-bg px-[38px] pb-6 pt-7">
          <div data-pane={pane} className="mx-auto max-w-[880px]">
            {pane === "sheds" && <ShedsPane sheds={sheds} refresh={refresh} onNew={() => setModal("create")} />}
            {pane === "approvals" && <ApprovalsPane approvals={approvals} />}
            {pane === "agents" && <AgentsPane sessions={rcSessions} onLaunch={() => setModal("launch")} refresh={refreshRc} />}
            {pane === "activity" && <ActivityPane />}
            {pane === "system" && <SystemPane sheds={sheds} />}
          </div>
        </main>
      </div>
      {modal === "prefs" && <PreferencesModal onClose={() => setModal(null)} />}
      {modal === "create" && <NewShedDialog refresh={refresh} onClose={() => setModal(null)} />}
      {modal === "launch" && <LaunchAgentDialog sheds={sheds} capabilities={rcCapabilities} refresh={refreshRc} onClose={() => setModal(null)} onLaunched={onLaunched} />}
    </div>
  );
}
