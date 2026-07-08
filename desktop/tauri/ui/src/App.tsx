/* shed desktop — the app shell (A0b). Sidebar + header + the five panes,
   rendered from static seed data so the linen shell matches the mockup; A1b/A1c
   wire live shed-core data + the dialogs. Nav is driven by clicks AND by the Rust
   `ui.navigate` op (via the `navigate` Tauri event); the rendered pane + a
   computed-style sample are reported back to Rust (useUiBridge) so the harness can
   assert them over IPC. */
import { useCallback, useEffect, useRef, useState } from "react";
import {
  Boxes, Shield, Sparkles, ScrollText, HardDrive, Box, Plus,
  Terminal, RotateCw, Square, Play, Trash2, RefreshCw, ExternalLink, Key,
  Fingerprint, Moon, Sun, Settings, X,
} from "lucide-react";
import { cn } from "@/lib/utils";
import {
  useUiBridge, shedAction, fetchSystemDf, openTerminal,
  fetchTerminalPresets, getPrefs, setTerminalPref,
  getLoginItem, setLoginItem,
  createStart, createStatus, createCancel, fetchHosts,
  fetchApprovals, decideApproval, fetchActivity, fetchGateNamespaces,
  getSshApproval, setSshApproval,
  fetchRcSessions, rcLaunch, rcKill, reportAgents,
  useCoordinatorData, useNowTick,
  type Pane, type Shed, type HostDiskUsage, type TerminalPresetInfo,
  type Modal, type CreateProgress, type Approval, type AuditEntry, type SshPrefs,
  type RcSession, type RcKind, type RcState,
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

/* ---- small building blocks ------------------------------------------------ */
function Dot({ className, style }: { className?: string; style?: React.CSSProperties }) {
  return <span style={style} className={cn("inline-block h-[9px] w-[9px] rounded-full", className)} />;
}

function Tag({ kind }: { kind: string }) {
  const vz = kind === "vz";
  return (
    <span
      className="rounded px-1.5 py-0.5 text-[10px] font-semibold leading-none"
      style={{
        background: vz ? "var(--shed-tag-vz-bg)" : "var(--shed-tag-fc-bg)",
        color: vz ? "var(--shed-tag-vz-text)" : "var(--shed-tag-fc-text)",
      }}
    >
      {kind}
    </span>
  );
}

function ImageChip({ children }: { children: React.ReactNode }) {
  return (
    <span className="rounded bg-shed-inset px-1.5 py-0.5 font-mono text-[10px] font-medium leading-none text-shed-text-secondary">
      {children}
    </span>
  );
}

function PageHead({ title, sub, right }: { title: string; sub?: string; right?: React.ReactNode }) {
  return (
    <div className="mb-4 flex items-start gap-4">
      <div className="flex-1">
        <h1 className="text-[26px] font-bold leading-tight text-shed-text">{title}</h1>
        {sub && <p className="mt-1 text-[12px] text-shed-text-muted">{sub}</p>}
      </div>
      {right}
    </div>
  );
}

// A header action — a plain text + SF-style icon accent link (Swift ShedListView's
// "New shed" Label), not a filled button.
function HeadAction({ icon: Icon, label, onClick }: { icon: typeof Box; label: string; onClick?: () => void }) {
  return (
    <button
      onClick={onClick}
      className="hlink -mr-1 inline-flex items-center gap-1.5 rounded-md px-1.5 py-1 text-[13px] font-medium text-shed-accent"
    >
      <Icon size={14} />
      {label}
    </button>
  );
}

function IconBtn({ icon: Icon, tone = "neutral", title, onClick, spin, disabled }:
  { icon: typeof Box; tone?: "neutral" | "accent" | "ok" | "attention" | "danger"; title?: string; onClick?: () => void; spin?: boolean; disabled?: boolean }) {
  const tinted = tone !== "neutral";
  const v = `var(--shed-${tone})`;
  return (
    <button
      onClick={onClick}
      title={title}
      disabled={disabled}
      className="hbtn inline-flex h-[26px] w-[26px] flex-none items-center justify-center rounded-[7px]"
      style={{
        // A CLEAN intent tint — mixed into the near-white card surface (Swift
        // IntentButton `intent.opacity(0.12)`), not into the beige `--shed-inset`,
        // which muddied every tone toward tan. `srgb` matches Swift's alpha compositing.
        border: `1px solid ${tinted ? `color-mix(in srgb, ${v} 34%, var(--shed-border))` : "var(--shed-border)"}`,
        background: tinted ? `color-mix(in srgb, ${v} 14%, var(--shed-surface))` : "var(--shed-inset)",
        color: tinted ? v : "var(--shed-text-secondary)",
        opacity: disabled ? 0.45 : 1,
      }}
    >
      <Icon size={14} className={spin ? "animate-spin" : undefined} />
    </button>
  );
}

const card = "rounded-shed border border-shed-border bg-shed-surface shadow-shed";

/* ---- live sheds (A1b) ----------------------------------------------------- */
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

/* ---- panes ---------------------------------------------------------------- */
/** The muted "nothing here yet" card shared by the empty panes. */
function EmptyCard({ children }: { children: React.ReactNode }) {
  return <div className={cn(card, "px-5 py-8 text-center text-[14px] text-shed-text-muted")}>{children}</div>;
}

function ShedsPane({ sheds, refresh, onNew }: { sheds: Shed[]; refresh: () => void; onNew: () => void }) {
  const act = (action: string, s: Shed) => void shedAction(action, s.name, s.host).then(refresh);
  return (
    <div>
      <PageHead title="Sheds" right={<HeadAction icon={Plus} label="New shed" onClick={onNew} />} />
      {sheds.length === 0 ? (
        <EmptyCard>No sheds on the configured hosts.</EmptyCard>
      ) : (
        groupByHost(sheds).map(([host, rows]) => (
          <div key={host} className="mb-4 last:mb-0">
            <div className="mb-2 pl-0.5 text-[11px] font-semibold uppercase tracking-wider text-shed-text-muted">{host}</div>
            <div className="flex flex-col gap-2">
              {rows.map((s) => {
                const running = s.status === "running";
                return (
                  <div key={`${host}/${s.name}`} className={cn(card, "flex items-center gap-3 px-3.5 py-3")} style={{ animation: "shed-in .25s ease" }}>
                    <Dot style={{ background: running ? "var(--shed-ok)" : "var(--shed-text-muted)" }} className="h-2 w-2" />
                    <div className="min-w-0 flex-1">
                      <div className="mb-0.5 flex flex-wrap items-center gap-1.5">
                        <span className="text-[14px] font-semibold text-shed-text">{s.name}</span>
                        {s.backend && <Tag kind={s.backend} />}
                        {s.image && <ImageChip>{s.image}</ImageChip>}
                      </div>
                      <div className="text-[12px] text-shed-text-muted">{metaLine(s)}</div>
                    </div>
                    <div className="flex gap-1.5">
                      {running ? (
                        <>
                          <IconBtn icon={Terminal} tone="accent" title="Open in Terminal" onClick={() => void openTerminal(s.name, s.host)} />
                          <IconBtn icon={RotateCw} tone="attention" title="Restart" onClick={() => act("reset", s)} />
                          <IconBtn icon={Square} tone="danger" title="Stop" onClick={() => act("stop", s)} />
                        </>
                      ) : (
                        <>
                          <IconBtn icon={Play} tone="ok" title="Start" onClick={() => act("start", s)} />
                          <IconBtn icon={Trash2} tone="danger" title="Delete" onClick={() => act("delete", s)} />
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
        <EmptyCard>No pending approvals.</EmptyCard>
      ) : (
        <div className="flex flex-col gap-3">
          {approvals.map((a) => {
            const t = Date.parse(a.expires_at);
            // A malformed/absent expiry parses to NaN; the backend treats it as
            // already-expired (fail-closed), so show 0s rather than "NaNs".
            const secs = Number.isFinite(t) ? Math.max(0, Math.round((t - now) / 1000)) : 0;
            const biometric = a.gate !== "none";
            return (
              <div key={a.id} className={cn(card, "p-3.5")} style={{ animation: "shed-in .25s ease" }}>
                <div className="flex items-start gap-2.5">
                  <div className="flex h-8 w-8 flex-none items-center justify-center rounded-lg" style={{ background: "var(--shed-tag-vz-bg)" }}>
                    <Key size={16} style={{ color: "var(--shed-tag-vz-text)" }} />
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="text-[14px] font-semibold text-shed-text">{a.namespace} · {a.op}</div>
                    <div className="mt-0.5 text-[12px] leading-snug text-shed-text-secondary">shed {qualifiedShed(a.server, a.shed)} · {a.detail}</div>
                  </div>
                  <span className="flex-none text-[12px] font-medium" style={{ color: "var(--shed-attention)" }}>expires in {secs}s</span>
                </div>
                <div className="mt-2.5 flex items-center">
                  <span className="flex-1 text-[11px] text-shed-text-muted">{approveHint(a.default_scope)}</span>
                  <div className="flex gap-2">
                    <button onClick={() => void decideApproval(a.id, "deny")} className="hbtn rounded-lg px-3.5 py-[7px] text-[13px] font-medium" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}>Deny</button>
                    <button
                      onClick={() => void decideApproval(a.id, "approve", { scope: a.default_scope, ttl: a.default_ttl })}
                      className="hbtn inline-flex items-center gap-1.5 rounded-lg px-3.5 py-[7px] text-[13px] font-medium"
                      style={{ background: "var(--shed-approve)", color: "var(--shed-approve-fg)" }}
                    >
                      {biometric && <Fingerprint size={14} />} Approve
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

/* ---- Agents / remote-control (B2.4) --------------------------------------- */
const RC_KINDS: { id: RcKind; label: string }[] = [
  { id: "claude-rc", label: "Claude" },
  { id: "shell", label: "Shell" },
];
const rcInput =
  "w-full rounded-[9px] border border-shed-border bg-shed-inset px-3 py-2 text-[14px] text-shed-text outline-none focus:border-shed-accent";

/** State → badge tone: green ready, red dead, amber for in-progress / needs-action. */
function rcStateTone(state: RcState): { bg: string; fg: string } {
  if (state === "ready")
    return { bg: "color-mix(in srgb, var(--shed-ok) 18%, var(--shed-surface))", fg: "var(--shed-ok)" };
  if (state === "dead") return { bg: "var(--shed-deny-bg)", fg: "var(--shed-danger)" };
  return { bg: "color-mix(in srgb, var(--shed-attention) 18%, var(--shed-surface))", fg: "var(--shed-attention)" };
}

function AgentsPane({ sheds }: { sheds: Shed[] }) {
  const [sessions, setSessions] = useState<RcSession[]>([]);
  const [showForm, setShowForm] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const gen = useRef(0);

  const refresh = useCallback(async () => {
    const mine = ++gen.current;
    const rows = await fetchRcSessions();
    if (mine === gen.current) setSessions(rows); // drop a superseded fetch
  }, []);

  useEffect(() => { void refresh(); }, [refresh]);
  // Publish the rendered sessions so the `agents.dump` op can observe them.
  useEffect(() => { reportAgents(sessions); }, [sessions]);

  return (
    <div>
      <PageHead
        title="Remote-control agents"
        sub="Drive an agent — a REPL, a shell, or a coding agent — inside a shed from here."
        right={<HeadAction icon={Plus} label="New session" onClick={() => { setError(null); setShowForm((v) => !v); }} />}
      />
      {showForm && (
        <LaunchForm sheds={sheds} onLaunched={() => { setShowForm(false); void refresh(); }} onError={setError} />
      )}
      {error && (
        <div className={cn(card, "mb-3 flex items-start gap-2 p-3.5 text-[13px]")} style={{ borderColor: "var(--shed-danger)", color: "var(--shed-danger)" }}>
          <X size={16} className="mt-px flex-none" /> <span className="min-w-0 break-words">{error}</span>
        </div>
      )}
      {sessions.length === 0 ? (
        <EmptyCard>No remote-control sessions. Start one with “New session”.</EmptyCard>
      ) : (
        <div className="flex flex-col gap-3">
          {sessions.map((s) => (
            <SessionCard key={`${s.host}/${s.shed}/${s.slug}`} session={s} onKilled={() => void refresh()} onError={setError} />
          ))}
        </div>
      )}
    </div>
  );
}

function SessionCard({ session: s, onKilled, onError }: { session: RcSession; onKilled: () => void; onError: (e: string) => void }) {
  const [busy, setBusy] = useState(false);
  const tone = rcStateTone(s.state);
  const sub = [`tmux ${s.tmux_session}`, s.workdir, s.created_by].filter(Boolean).join(" · ");
  const kill = async () => {
    setBusy(true);
    try { await rcKill(s.shed, s.slug, s.host); onKilled(); }
    catch (e) { onError(String(e)); setBusy(false); }
  };
  return (
    <div className={cn(card, "flex items-center gap-3 px-3.5 py-3")} style={{ animation: "shed-in .25s ease" }}>
      <span className="inline-flex w-[84px] flex-none items-center justify-center rounded-md px-2 py-1 text-[11px] font-medium" style={{ background: tone.bg, color: tone.fg }}>{s.state}</span>
      <div className="min-w-0 flex-1">
        <div className="mb-0.5 flex flex-wrap items-center gap-1.5">
          <span className="text-[14px] font-semibold text-shed-text">{s.display_name}</span>
          <span className="rounded bg-shed-inset px-1.5 py-0.5 font-mono text-[10px] font-medium text-shed-text-secondary">{s.kind}</span>
          {!s.managed && <span className="rounded bg-shed-inset px-1.5 py-0.5 font-mono text-[10px] font-semibold text-shed-text-muted">legacy</span>}
        </div>
        <div className="truncate text-[12px] text-shed-text-muted">{sub}</div>
      </div>
      {s.url && (
        <a href={s.url} target="_blank" rel="noreferrer" className="hbtn inline-flex flex-none items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-[12px] font-medium" style={{ background: "var(--shed-accent-subtle)", border: "1px solid var(--shed-accent-border)", color: "var(--shed-accent)" }}>
          <ExternalLink size={14} /> Open in Claude
        </a>
      )}
      <IconBtn icon={Terminal} tone="accent" title="Open in Terminal" onClick={() => void openTerminal(s.shed, s.host, s.tmux_session)} />
      <IconBtn icon={Trash2} tone="danger" title="End session" onClick={() => void kill()} disabled={busy} spin={busy} />
    </div>
  );
}

function LaunchForm({ sheds, onLaunched, onError }: { sheds: Shed[]; onLaunched: () => void; onError: (e: string) => void }) {
  const running = sheds.filter((s) => s.status === "running");
  const [target, setTarget] = useState(running[0] ? `${running[0].host}/${running[0].name}` : "");
  const [kind, setKind] = useState<RcKind>("claude-rc");
  const [displayName, setDisplayName] = useState("");
  const [prompt, setPrompt] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    const sel = running.find((s) => `${s.host}/${s.name}` === target);
    if (!sel) { onError("Pick a running shed to launch in."); return; }
    setBusy(true);
    onError("");
    try {
      await rcLaunch({
        shed: sel.name,
        host: sel.host,
        kind,
        displayName: displayName.trim() || undefined,
        initialPrompt: prompt.trim() || undefined,
      });
      setDisplayName(""); setPrompt("");
      onLaunched();
    } catch (e) {
      onError(String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={cn(card, "mb-3 flex flex-col gap-3 p-4")} style={{ animation: "shed-in .2s ease" }}>
      <div className="grid grid-cols-2 gap-3">
        <Field label="Shed">
          <select value={target} onChange={(e) => setTarget(e.target.value)} className={rcInput}>
            {running.length === 0 && <option value="">no running sheds</option>}
            {running.map((s) => (
              <option key={`${s.host}/${s.name}`} value={`${s.host}/${s.name}`}>{qualifiedShed(s.host, s.name)}</option>
            ))}
          </select>
        </Field>
        <Field label="Kind">
          <div className="flex gap-2">
            {RC_KINDS.map((k) => {
              const on = kind === k.id;
              return (
                <button key={k.id} onClick={() => setKind(k.id)} className={cn("hbtn flex-1 rounded-[9px] px-3 py-2 text-[14px] font-semibold", on ? "text-shed-accent" : "text-shed-text-muted")} style={{ background: on ? "color-mix(in srgb, var(--shed-accent) 14%, var(--shed-surface))" : "var(--shed-inset)", border: on ? "1px solid color-mix(in srgb, var(--shed-accent) 34%, var(--shed-border))" : "1px solid var(--shed-border)" }}>{k.label}</button>
              );
            })}
          </div>
        </Field>
      </div>
      <Field label="Display name (optional)">
        <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="defaults to shed/slug" className={rcInput} />
      </Field>
      <Field label={kind === "shell" ? "Initial command (optional)" : "Initial prompt (optional)"}>
        <textarea value={prompt} onChange={(e) => setPrompt(e.target.value)} rows={2} placeholder={kind === "shell" ? "npm install && npm test" : "summarize this repo"} className={cn(rcInput, "resize-none")} />
      </Field>
      <div className="flex justify-end">
        <button onClick={() => void submit()} disabled={busy || !target} className="hbtn inline-flex items-center gap-2 rounded-[10px] px-[22px] py-[11px] text-[15px] font-semibold disabled:opacity-50" style={{ background: "color-mix(in srgb, var(--shed-accent) 14%, var(--shed-surface))", border: "1px solid color-mix(in srgb, var(--shed-accent) 34%, var(--shed-border))", color: "var(--shed-accent)" }}>
          <Sparkles size={18} /> {busy ? "Launching…" : "Launch"}
        </button>
      </div>
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
        <EmptyCard>No activity yet.</EmptyCard>
      ) : (
        <div className={cn(card, "overflow-hidden")}>
          {activity.map((e, i) => {
            const ok = e.result === "ok";
            return (
              <div key={`${e.id}-${i}`} className={cn("row-hover flex items-center gap-2.5 px-3.5 py-2", i && "border-t border-shed-border")}>
                <span className="w-[66px] flex-none font-mono text-[11px] text-shed-text-muted">{shortTime(e.ts)}</span>
                {e.ns && <span className="flex-none rounded px-1.5 py-0.5 font-mono text-[10px] font-semibold" style={{ background: "var(--shed-agent-pill-bg)", color: "var(--shed-agent-pill-text)" }}>{e.ns}</span>}
                <span className="min-w-0 flex-1 truncate text-[12px] text-shed-text-secondary">{activityDetail(e)}</span>
                <span className="flex flex-none items-center gap-2">
                  <span className="text-[12px] font-medium" style={{ color: ok ? "var(--shed-ok)" : "var(--shed-danger)" }}>{e.result}</span>
                  {e.approval && e.approval !== "none" && <span className="text-[11px] text-shed-text-muted">{e.approval}</span>}
                </span>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

function SystemPane() {
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
  return (
    <div>
      <PageHead
        title="System"
        sub="Disk usage per host (images, sheds, snapshots, orphans)."
        right={<HeadAction icon={RefreshCw} label="Refresh" onClick={load} />}
      />
      <div className="flex flex-col gap-2.5">
        {rows.map((h) => {
          const t = h.usage?.totals;
          return (
            <div key={h.host} className={cn(card, "p-3.5")}>
              <div className={cn("flex items-center gap-2", t ? "mb-2.5" : "mb-2")}>
                <HardDrive size={16} className="text-shed-text-muted" />
                <span className="text-[14px] font-semibold text-shed-text">{h.host}</span>
                {h.usage?.backend && <Tag kind={h.usage.backend} />}
                <span className="flex-1" />
                {t && <span className="text-[13px] font-semibold text-shed-text">{formatBytes(t.all.physical_bytes)}</span>}
              </div>
              {t ? (
                <div className="grid grid-cols-4 gap-2.5">
                  {(
                    [
                      ["Images", t.images],
                      ["Sheds", t.sheds],
                      ["Snapshots", t.snapshots],
                      ["Orphans", t.orphans],
                    ] as const
                  ).map(([label, size]) => (
                    <div key={label}>
                      <div className="mb-1 text-[10px] text-shed-text-muted">{label}</div>
                      <div className="font-mono text-[12px] font-medium text-shed-text">{formatBytes(size.physical_bytes)}</div>
                    </div>
                  ))}
                </div>
              ) : (
                <div className="break-words font-mono text-[11px] leading-relaxed" style={{ color: "var(--shed-danger)" }}>{h.error}</div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

/* ---- preferences (in-app modal; the tray is Phase C) ---------------------- */
// How an SSH-key approval is confirmed. On Linux "Authenticate" routes through
// polkit (B6); "Prompt only" needs no gate — a plain Approve button. (The
// biometrics-only method is macOS-specific, so it's not offered here.)
const APPROVAL_METHODS: { id: SshPrefs["method"]; label: string; detail: string }[] = [
  { id: "biometrics-or-password", label: "Authenticate", detail: "Confirm with your login password." },
  { id: "prompt", label: "Prompt only", detail: "A plain Approve button — no password." },
];

// The SSH approval policies, most → least permissive (mirrors SshApprovalPolicy in
// shed-core). `prompts` gates the Method picker and `usesDuration` gates the
// Duration field — the SAME policy→behavior mapping as the Swift app
// (SSHApprovalPolicy.prompts / .usesDuration): only the two "Always" options
// decide with no prompt, and only "Time Based Allow" carries a duration.
const SSH_POLICIES: { id: string; label: string; prompts: boolean; usesDuration: boolean }[] = [
  { id: "always-allow", label: "Always Allow", prompts: false, usesDuration: false },
  { id: "per-shed-allow", label: "Per Shed Allow", prompts: true, usesDuration: false },
  { id: "time-based-allow", label: "Time Based Allow", prompts: true, usesDuration: true },
  { id: "always-ask", label: "Always Ask", prompts: true, usesDuration: false },
  { id: "always-deny", label: "Always Deny", prompts: false, usesDuration: false },
];

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

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

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
  // toggle misrepresenting the real state (mirrors applySsh's reconcile). Guarded
  // by `loginBusy`: the control is disabled while a write is in flight, so a fast
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
  // field (the coordinator composes partial updates), then reconcile ALL three
  // from the backend — so a rejected/failed write can't leave the method radio
  // misrepresenting the actual gate strength (a security-signal surface). A
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
    <div
      className="fixed inset-0 z-50 flex items-center justify-center px-6"
      style={{ background: "color-mix(in oklch, var(--shed-text) 32%, transparent)" }}
      onClick={onClose}
      data-prefs
    >
      <div
        className={cn(card, "w-full max-w-[460px] overflow-hidden p-0")}
        onClick={(e) => e.stopPropagation()}
        // A macOS grouped-Settings-form look: a recessed `--shed-bg` canvas with
        // white section cards (matches the Swift `Form { Section }` .formStyle(.grouped)).
        style={{ animation: "shed-in .18s ease", background: "var(--shed-bg)" }}
      >
        <div className="flex items-center gap-3 border-b border-shed-border bg-shed-surface px-5 py-3">
          <Settings size={16} className="text-shed-text-muted" />
          <h2 className="flex-1 text-[13px] font-semibold text-shed-text">Preferences</h2>
          <button onClick={onClose} title="Close" className="hlink flex h-6 w-6 items-center justify-center rounded-md text-shed-text-muted">
            <X size={15} />
          </button>
        </div>
        <div className="max-h-[540px] overflow-auto px-5 py-4">
          {/* SECTIONS */}

        <div className="mb-1.5 text-[11px] font-semibold uppercase tracking-wider text-shed-text-muted">General</div>
        <div className="mb-5 overflow-hidden rounded-[10px] border border-shed-border bg-shed-surface">
          <label className="flex cursor-pointer items-center gap-3 px-3.5 py-2.5">
            <span className="text-[13px] text-shed-text">Launch at login</span>
            <span className="flex-1" />
            <span className="text-[12px] text-shed-text-muted">Open Shed Desktop when you sign in.</span>
            <input
              type="checkbox"
              checked={launchAtLogin}
              disabled={loginBusy}
              onChange={(e) => toggleLaunchAtLogin(e.target.checked)}
              style={{ accentColor: "var(--shed-accent)" }}
              data-launch-at-login
            />
          </label>
        </div>

        <div className="mb-1.5 text-[11px] font-semibold uppercase tracking-wider text-shed-text-muted">Terminal</div>
        <p className="mb-2 text-[12px] text-shed-text-muted">Which terminal opens when you click “Open in Terminal” on a shed.</p>
        <div className="mb-5 overflow-hidden rounded-[10px] border border-shed-border bg-shed-surface">
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
              className="w-full rounded-[9px] border border-shed-border bg-shed-surface px-3 py-2 font-mono text-[13px] text-shed-text outline-none"
            />
            <p className="mt-1.5 text-[12px] text-shed-text-muted">
              <code className="font-mono">{"{cmd}"}</code> is the ssh command, <code className="font-mono">{"{shed}"}</code> the shed name.
            </p>
          </div>
        )}

        <div className="mb-1.5 text-[11px] font-semibold uppercase tracking-wider text-shed-text-muted">Credential approvals</div>
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
                // on blur/Enter, and only when it changed. `applySsh`→set_ssh_approval
                // resets live SSH grants + rewrites prefs.json, so a per-keystroke
                // persist would revoke active grants and churn disk mid-typing.
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
            <div className="mb-1.5 text-[11px] font-semibold uppercase tracking-wider text-shed-text-muted">Method</div>
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
      </div>
    </div>
  );
}

/* ---- new shed (create dialog with live SSE progress) ---------------------- */
const inputCls =
  "rounded-[9px] border border-shed-border bg-shed-inset px-3 py-2 text-[14px] text-shed-text outline-none";

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-1.5">
      <span className="text-[12px] font-semibold text-shed-text-secondary">{label}</span>
      {children}
    </label>
  );
}

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

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !creating) onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [creating, onClose]);

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

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center px-6"
      style={{ background: "color-mix(in oklch, var(--shed-text) 32%, transparent)" }}
      onClick={() => {
        if (!creating) onClose();
      }}
      data-create
    >
      <div className={cn(card, "w-full max-w-[540px] p-6")} onClick={(e) => e.stopPropagation()} style={{ animation: "shed-in .18s ease" }}>
        <div className="mb-5 flex items-center gap-3">
          <Plus size={20} className="text-shed-text-muted" />
          <h2 className="flex-1 text-[17px] font-semibold text-shed-text">New shed</h2>
          <button onClick={cancel} title="Close" className="hlink flex h-7 w-7 items-center justify-center rounded-md text-shed-text-muted">
            <X size={17} />
          </button>
        </div>

        {!creating ? (
          <div className="flex flex-col gap-3.5">
            <Field label="Name">
              <input autoFocus value={name} onChange={(e) => setName(e.target.value)} placeholder="my-shed" className={cn(inputCls, "font-mono")} />
            </Field>
            <div className="grid grid-cols-2 gap-3.5">
              <Field label="Host">
                <select value={host} onChange={(e) => setHost(e.target.value)} className={inputCls}>
                  {hosts.length === 0 && <option value="">(default)</option>}
                  {hosts.map((h) => (
                    <option key={h} value={h}>{h}</option>
                  ))}
                </select>
              </Field>
              <Field label="Backend">
                <select value={vmBackend} onChange={(e) => setVmBackend(e.target.value)} className={inputCls}>
                  <option value="">Default</option>
                  <option value="vz">vz</option>
                  <option value="firecracker">firecracker</option>
                </select>
              </Field>
            </div>
            <Field label="Image">
              <input value={image} onChange={(e) => setImage(e.target.value)} placeholder="e.g. base" className={cn(inputCls, "font-mono")} />
            </Field>
            <div className="grid grid-cols-2 gap-3.5">
              <Field label="CPUs">
                <input type="number" min="1" step="1" value={cpus} onChange={(e) => setCpus(e.target.value)} placeholder="2" className={inputCls} />
              </Field>
              <Field label="Memory (GB)">
                <input type="number" min="1" step="1" value={memGb} onChange={(e) => setMemGb(e.target.value)} placeholder="4" className={inputCls} />
              </Field>
            </div>
            <Field label="Repo (optional)">
              <input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="owner/repo" className={cn(inputCls, "font-mono")} />
            </Field>
            {error && (
              <div className="rounded-md px-3 py-2 font-mono text-[12px]" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}>{error}</div>
            )}
            <div className="mt-1 flex justify-end gap-2.5">
              <button onClick={onClose} className="hbtn rounded-[10px] px-4 py-2.5 text-[14px] font-semibold text-shed-text-secondary" style={{ background: "var(--shed-inset)" }}>Cancel</button>
              <button
                onClick={() => void submit()}
                disabled={!name.trim() || submitting}
                className="hbtn inline-flex items-center gap-2 rounded-[10px] px-4 py-2.5 text-[14px] font-semibold text-shed-accent-fg"
                style={{ background: "var(--shed-accent)", opacity: name.trim() && !submitting ? 1 : 0.5 }}
              >
                <Plus size={16} /> Create
              </button>
            </div>
          </div>
        ) : (
          <div>
            <div className="mb-3 flex items-center gap-2.5 text-[14px] font-semibold">
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
            <div className="mt-4 flex justify-end gap-2.5">
              {state === "complete" ? (
                <button onClick={onClose} className="hbtn rounded-[10px] px-4 py-2.5 text-[14px] font-semibold text-shed-accent-fg" style={{ background: "var(--shed-accent)" }}>Done</button>
              ) : (
                <button onClick={cancel} className="hbtn rounded-[10px] px-4 py-2.5 text-[14px] font-semibold" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}>Cancel</button>
              )}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

/* ---- shell ---------------------------------------------------------------- */
export default function App() {
  const [pane, setPane] = useState<Pane>("sheds");
  const [mode, setMode] = useState<"light" | "dark">("light");
  const [modal, setModal] = useState<Modal>(null);
  const { sheds, refresh } = useUiBridge(pane, setPane, modal);
  // Live approval queue (drives the badge + the pane) + the delegated namespaces
  // (a non-empty set = the host agent handshook, so it's connected).
  const approvals = useCoordinatorData<Approval[]>("approvals-changed", fetchApprovals, []);
  const gateNs = useCoordinatorData<string[]>("connected-changed", fetchGateNamespaces, []);
  const connected = gateNs.length > 0;

  useEffect(() => {
    document.documentElement.dataset.mode = mode;
  }, [mode]);

  // Open the modals both from the buttons and from the ui.show_preferences /
  // ui.show_create IPC ops (events), so the harness can drive + screenshot them.
  useEffect(() => {
    if (typeof window === "undefined" || !("__TAURI_INTERNALS__" in window)) return;
    const uns: Array<() => void> = [];
    let cancelled = false;
    void import("@tauri-apps/api/event").then(async ({ listen }) => {
      uns.push(await listen("show-preferences", () => setModal("prefs")));
      uns.push(await listen("show-create", () => setModal("create")));
      if (cancelled) uns.forEach((u) => u());
    });
    return () => {
      cancelled = true;
      uns.forEach((u) => u());
    };
  }, []);

  const pending = approvals.length;
  // Sidebar hosts are the distinct hosts of the live sheds (all reachable — the
  // "N unreachable" rollup rides in with the System pane at A1c).
  const hosts = [...new Set(sheds.map((s) => s.host))];

  return (
    <div className="flex h-full">
      {/* sidebar */}
      <aside className="flex w-[200px] flex-none flex-col gap-[3px] border-r border-shed-border bg-shed-bg-sidebar px-2 pb-2.5 pt-3">
        {NAV.map(([id, label, Icon]) => {
          const active = pane === id;
          const badge = id === "sheds" ? sheds.length || null : id === "approvals" && pending ? pending : null;
          const alert = id === "approvals" && pending > 0;
          return (
            <button
              key={id}
              onClick={() => setPane(id)}
              className={cn("nav-item flex w-full items-center gap-[9px] rounded-lg px-[9px] py-2 text-left text-[13px]", active ? "font-medium" : "font-normal", active && "is-active")}
              style={{
                background: active ? "var(--shed-accent-subtle)" : "transparent",
              }}
            >
              <Icon size={16} className="flex-none" style={{ color: active ? "var(--shed-accent)" : "var(--shed-text-muted)" }} />
              <span className="flex-1" style={{ color: active ? "var(--shed-accent)" : "var(--shed-text-secondary)" }}>{label}</span>
              {badge != null && (
                <span
                  className="inline-flex items-center justify-center rounded-full px-[7px] py-px text-[11px] font-medium"
                  style={{ background: alert ? "color-mix(in srgb, var(--shed-danger) 13%, var(--shed-surface))" : "var(--shed-inset)", color: alert ? "var(--shed-danger)" : "var(--shed-text-secondary)" }}
                >
                  {badge}
                </span>
              )}
            </button>
          );
        })}
        <div className="mx-1 my-3 h-px bg-shed-border" />
        <div className="px-2.5 pb-1 pt-0.5 text-[11px] font-semibold tracking-wider text-shed-text-muted">HOSTS</div>
        {hosts.map((h) => (
          <div key={h} className="flex items-center gap-2 px-2.5 py-[5px] text-[13px] text-shed-text-secondary">
            <Dot style={{ background: "var(--shed-ok)" }} />
            <span>{h}</span>
          </div>
        ))}
        <div className="flex-1" />
      </aside>

      {/* main column */}
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-[44px] flex-none items-center gap-2 border-b border-shed-border bg-shed-bg px-4">
          <Box size={16} className="text-shed-text-secondary" />
          <span className="text-[13px] font-medium text-shed-text-secondary">shed desktop</span>
          <div className="flex-1" />
          {pending > 0 && (
            <button onClick={() => setPane("approvals")} className="hlink inline-flex items-center gap-1.5 rounded-md px-2 py-1 text-[12px] font-medium" style={{ color: "var(--shed-danger)" }}>
              <Shield size={13} /> {pending} pending
            </button>
          )}
          <span className="inline-flex items-center gap-1.5 text-[12px] text-shed-text-secondary">
            <Dot className="h-[7px] w-[7px]" style={{ background: connected ? "var(--shed-ok)" : "var(--shed-text-muted)" }} /> host agent · {connected ? "connected" : "connecting…"}
          </span>
          <button onClick={() => setModal("prefs")} title="Preferences" className="hlink ml-1 flex h-7 w-7 items-center justify-center rounded-md text-shed-text-muted">
            <Settings size={15} />
          </button>
          <button onClick={() => setMode(mode === "light" ? "dark" : "light")} title="Toggle appearance" className="hlink flex h-7 w-7 items-center justify-center rounded-md text-shed-text-muted">
            {mode === "light" ? <Moon size={15} /> : <Sun size={15} />}
          </button>
        </header>
        <main className="flex-1 overflow-auto bg-shed-bg px-5 pb-4 pt-[18px]">
          <div data-pane={pane}>
            {pane === "sheds" && <ShedsPane sheds={sheds} refresh={refresh} onNew={() => setModal("create")} />}
            {pane === "approvals" && <ApprovalsPane approvals={approvals} />}
            {pane === "agents" && <AgentsPane sheds={sheds} />}
            {pane === "activity" && <ActivityPane />}
            {pane === "system" && <SystemPane />}
          </div>
        </main>
      </div>
      {modal === "prefs" && <PreferencesModal onClose={() => setModal(null)} />}
      {modal === "create" && <NewShedDialog refresh={refresh} onClose={() => setModal(null)} />}
    </div>
  );
}
