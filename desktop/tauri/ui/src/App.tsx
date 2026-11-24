/* shed desktop — the app shell (Plex reskin). Sidebar + header + the four panes,
   over live shed-core data. Nav is driven by clicks AND by the Rust `ui.navigate`
   op (via the `navigate` Tauri event); the rendered pane + a computed-style sample
   are reported back to Rust (useUiBridge) so the harness can assert them over IPC.
   The shared visual vocabulary lives in components/{primitives,dialog}. */
import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import {
  Boxes, Sparkles, ScrollText, HardDrive, Box, Plus,
  Terminal, RotateCw, Square, Play, Trash2, RefreshCw, ExternalLink,
  Moon, Sun, Settings, X, ChevronDown, ChevronRight,
} from "lucide-react";
import { cn } from "@/lib/utils";
import owlOrange from "@/assets/owl-orange.svg";
import owlAmber from "@/assets/owl-amber.svg";
import {
  cardCls, Dot, StatusChip, Tag, ImageChip, KindBadge, FactChip, ActBtn,
  PageHead, HeadAction, RefreshHeadButton, Empty, type Tone,
} from "@/components/primitives";
import { Scrim, DialogShell, Field, Select, dialogInput, dialogBtnSecondary, useEscClose } from "@/components/dialog";
import { LanePanel } from "@/components/LanePanel";
import { CrazeCreateDialog } from "@/components/CrazeCreateDialog";
import { canOpenTerminal, crazeDoingLine, crazeMachineNote } from "@/lib/crazeRows";
import { newestReload } from "@/lib/newest";
import { isBusy, statusText } from "@/lib/shedStatus";
import {
  EMPTY_DRAFT, beginSubmit, closesOntoTranscript, crazeCreateOffered, createTitle,
  crazeStatusFor, edit as editCrazeDraft, groupMachine, liveSetter,
  replaysHeldRequest, settle, sheetOnScreen,
  type CrazeDraft, type CrazeStatusLike,
} from "@/lib/crazeCreate";
import {
  EMPTY_RESUME, SUPERSEDED_NOTE, beginList, beginResume, canResume, chooseMode, laneOnResume, listNeeded,
  observeSource, observeSources, onSheetOpen, reopenLanding, settleList, settleResume,
  type CrazeResume, type CrazeSheetMode, type LaneTarget,
} from "@/lib/crazeResume";
import { RoostLine, RoostConsentDialog, Toast } from "@/components/RoostBootstrap";
import { DeliveryBadge, ShedDeliveryDetail, provisionRunOf, useShedDelivery, type ShedDeliveryState } from "@/components/ShedDelivery";
import { readDelivery } from "@/lib/delivery";
import { ShedEgressDetail } from "@/components/ShedEgress";
import {
  EMPTY_FEED, actorText, appendPage, deliveryText, errorText, outcomeTone, readPage,
  type ActivityFeed,
} from "@/lib/activity";
import { profileSummary } from "@/lib/egress";
import {
  roostBootstrap, roostPreview, roostProgressSteps, roostToastFor, roostDumpRow,
  reportShedRoost, renderedText, useRoostBoard,
  type RoostPreview, type RoostToast, type RoostDumpRow,
} from "@/lib/roost";
import {
  useUiBridge, shedAction, fetchSystemDf, fetchHostRows, openTerminal,
  createStart, createStatus, createCancel, fetchHosts,
  fetchEgressProfiles, reportEgress, inTauri, listServerActivity, reportActivity, reportScrolled,
  openPreferences, setAppearanceState,
  sessionKey, reportAgents, useRcSessions,
  crazeCreate, crazeOpenTerminal, crazeReopen, crazeSaved, laneFailure,
  shedsEmptyState, hostFailureFor,
  type Pane, type Shed, type HostDiskUsage, type HostFailure,
  type Modal, type CreateProgress,
  type HostEgressProfiles,
  type RcSession, type RcState,
  type MachineStatus, type AgentsLoad, type ShedCrazeEntry,
  agentsEmptyState,
} from "@/lib/bridge";

/** A timestamp as the Activity pane shows it: the date and time (the raw text on a miss). */
function eventTime(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime())
    ? ts
    : d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
}
const NAV: [Pane, string, typeof Box][] = [
  ["sheds", "Sheds", Boxes],
  ["agents", "Agents", Sparkles],
  ["activity", "Activity", ScrollText],
  ["system", "System", HardDrive],
];

/* ---- live sheds ----------------------------------------------------------- */
/** Group rows under a key, each group + its rows in first-seen order.
 *
 *  First-seen rather than sorted: the order a server reports its rows in is
 *  stable, and re-sorting would make a list reshuffle as items come and go. */
function groupBy<T>(rows: T[], key: (row: T) => string): [string, T[]][] {
  const groups: [string, T[]][] = [];
  for (const r of rows) {
    const k = key(r);
    const g = groups.find(([gk]) => gk === k);
    if (g) g[1].push(r);
    else groups.push([k, [r]]);
  }
  return groups;
}

const groupByHost = (sheds: Shed[]) => groupBy(sheds, (s) => s.host);

/** Where a session runs, as its group heading: `<server>/<shed>`. */
const sessionOrigin = (s: RcSession): string => `${s.host}/${s.shed}`;

/** A one-line spec summary ending in the status and its reason (`statusText`:
 *  `2 vCPU · 4 GB · stopped (capacity)`; just the status when specs are absent).
 *  The status part is its own node, so the dashboard report reads it back off
 *  the row (`data-shed-status`). */
function MetaLine({ s }: { s: Shed }) {
  const specs: string[] = [];
  if (s.cpus) specs.push(`${s.cpus} vCPU`);
  if (s.memory_mb) specs.push(`${Math.round(s.memory_mb / 1024)} GB`);
  return (
    <div className="font-mono text-[12.5px] leading-tight text-shed-text-muted">
      {specs.map((b) => `${b} · `).join("")}
      <span data-shed-status>{statusText(s)}</span>
    </div>
  );
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

/** A sidebar section heading (SHED SERVERS). */
function SidebarSection({ label }: { label: string }) {
  return (
    <div className="flex items-center px-2.5 pb-2 pt-2.5 first-of-type:pt-0.5">
      <span className="font-mono text-[10px] font-semibold tracking-[.1em] text-shed-text-muted">{label}</span>
    </div>
  );
}

/** A sidebar dot's tone: up, still connecting, down — or `neutral`, a host
 *  listed by craze alone (plan 026 §3.2), which is neither up nor waiting. */
type SidebarTone = "ok" | "pending" | "off" | "neutral";

/** One status row: a dot, a name, and an optional right-aligned note. Shared by
 *  both sections so a server and a machine read the same way — the difference
 *  between them is how they're reached, which is not a thing to look at. */
function SidebarRow({ name, tone, note, title, onClick }:
  { name: string; tone: SidebarTone; note?: string; title?: string; onClick?: () => void }) {
  const color = tone === "ok" ? "var(--shed-ok)"
    : tone === "pending" ? "var(--shed-attention)"
    : tone === "neutral" ? "var(--shed-text-secondary)"
    : "var(--shed-text-muted)";
  const body = (
    <>
      <Dot className="h-2 w-2 flex-none" style={{ background: color }} />
      <span className="min-w-0 flex-1 truncate">{name}</span>
      {note && <span className="flex-none font-mono text-[10.5px] text-shed-text-muted">{note}</span>}
    </>
  );
  const cls = "flex w-full items-center gap-2.5 px-2.5 py-1.5 text-left text-[13px] font-medium text-shed-text";
  return onClick ? (
    <button type="button" onClick={onClick} title={title} className={cn(cls, "nav-item rounded-[7px]")}>{body}</button>
  ) : (
    <div title={title} className={cls}>{body}</div>
  );
}

/** The activity chip for a session, honouring "lifecycle trumps activity".
 *
 *  A needs-auth or dead row shows NO activity: whatever it was doing stopped
 *  being true when it stopped being able to run, and a stale `working` badge on
 *  a dead session is a lie the card would be telling on its own initiative. */
function rcActivityLabel(s: RcSession): { tone: Tone; label: string } | null {
  if (s.state !== "ready" && s.state !== "reconnecting") return null;
  switch (s.activity) {
    case "needs_input":
      return { tone: "attention", label: "needs input" };
    case "needs_approval":
      return { tone: "attention", label: "needs approval" };
    case "working":
      return { tone: "ok", label: "working" };
    case "idle":
      return { tone: "muted", label: "idle" };
    default:
      return null;
  }
}

/** The card's left edge — what most wants your attention.
 *
 *  Deliberately NOT the badges' precedence: a bad LIFECYCLE outranks any
 *  activity, because a dead session is not merely idle. Below that, asking for
 *  a person outranks merely being busy. Anything not worth saying gets no edge
 *  at all, so the coloured ones carry weight — a full column of stripes would
 *  say nothing.
 *
 *  A stale row gets none either: its machine is unreachable, so colouring it
 *  would assert something present about a box we cannot see. Same rule as
 *  shed-mobile's `sessionRailColor`, because a shed row and a machine row must
 *  read as one column. */
function sessionRail(s: RcSession): string | undefined {
  if (s.stale) return undefined;
  if (s.state === "dead") return "var(--shed-danger)";
  if (s.state === "needs-auth" || s.state === "needs-trust") return "var(--shed-attention)";
  if (s.activity === "needs_input" || s.activity === "needs_approval") return "var(--shed-attention)";
  if (s.activity === "working") return "var(--shed-ok)";
  return undefined;
}

/** Is this session WAITING ON A PERSON?
 *
 *  The two activities that mean "it stopped and wants you" — as opposed to
 *  `working` (busy, leave it alone) or `idle` (done, nothing owed). This is the
 *  one distinction worth rolling up to a machine, because it is the only one
 *  that answers "do I need to go look".
 *
 *  Unknown activity is NOT waiting: a session whose activity never arrived is
 *  a session we know nothing about, and guessing "needs you" would cry wolf on
 *  every list. */
export function needsYou(s: RcSession): boolean {
  // Lifecycle trumps activity, the same rule the badges follow. A session that
  // moved to needs-auth or dead keeps whatever activity it last had — the hub's
  // `session.updated` carries the new state, not a cleared activity — so
  // counting on activity alone reports "1 waiting" for a machine whose session
  // is actually blocked on something else entirely, and whose card correctly
  // shows no activity at all. The pane and the card would then disagree.
  if (s.state !== "ready" && s.state !== "reconnecting") return false;
  return s.activity === "needs_input" || s.activity === "needs_approval";
}

function ShedsPane({ sheds, hostErrors, refresh, onNew, roostTick, onOpenConsent, roostBusy, hosts, shedCraze, onCrazeCreate, delivery: dlv }: {
  sheds: Shed[]; hostErrors: HostFailure[]; refresh: () => void; onNew: () => void;
  /** The open delivery detail and every shed's Deliver again run (plan 039 A7). */
  delivery: ShedDeliveryState;
  roostTick: number; onOpenConsent: (target: string, preview: RoostPreview) => void; roostBusy: Record<string, string>;
  /** `rc.list`'s status rows and `shed_craze` — what each running shed's craze
   *  is read from ([`crazeStatusFor`], plan 026 §3.2). */
  hosts: MachineStatus[]; shedCraze: ShedCrazeEntry[];
  onCrazeCreate: (machine: string) => void;
}) {
  const act = (action: string, s: Shed) => void shedAction(action, s.name, s.host).then(refresh);
  // The empty state defers to a host failure when there is one — same helper the
  // bridge reports as UI truth, so the pane and `dashboard.dump.empty` agree.
  const empty = shedsEmptyState(sheds, hostErrors);
  // A shed's own roost-session is only worth asking about while it is
  // RUNNING — there is no sshd to reach on a stopped one, and probing it
  // would just be a slow way to learn what `status` already says.
  const runningTargets = sheds.filter((s) => s.status === "running").map((s) => `roost:${s.host}/${s.name}`);
  const roostBoard = useRoostBoard(runningTargets, roostTick);
  // A running shed's craze, read through the one lookup (plan 026 §3.2): its
  // status row's once it is listed, else its `shed_craze` entry's — so a shed
  // whose probe says dormant offers New craze session here without being a
  // listed host, and one whose craze is too old says so.
  const crazeOf = (target: string) => crazeStatusFor(target, hosts, shedCraze);
  // Publish what this pane rendered, mounted-only, keyed on the VALUE so an
  // unrelated re-render doesn't
  // re-report. Each row's `rendered` — and its craze offer and note — are read
  // back off its own nodes IN THE EFFECT (after the commit), so the fields
  // beside them cannot pass as a parallel data structure with nothing on screen.
  const reportedRoost = Object.fromEntries(
    runningTargets.map((t) => {
      const craze = crazeOf(t);
      return [t, {
        ...roostDumpRow(t, roostBoard[t], roostBusy[t]),
        craze_create: crazeCreateOffered(craze),
        craze_note: crazeMachineNote(craze),
      }];
    }),
  );
  const reportedRoostKey = JSON.stringify(reportedRoost);
  useEffect(() => {
    const rows: Record<string, RoostDumpRow> = JSON.parse(reportedRoostKey);
    for (const [target, row] of Object.entries(rows)) {
      row.rendered = renderedText(`[data-roost="${target}"]`);
      const craze = document.querySelector(`[data-shed-craze="${target}"]`);
      row.craze_create = !!craze?.querySelector("[data-craze-create-action]");
      row.craze_note = craze?.querySelector("[data-craze-note]")?.textContent ?? null;
    }
    reportShedRoost(rows);
  }, [reportedRoostKey]);
  useEffect(() => () => reportShedRoost(null), []);
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
      {/* No per-host error strip here by design: an unreachable server is a
          STATUS, and status lives in the sidebar's SHED SERVERS section (with the
          reason on hover) and the System pane. This pane's only duty when it
          can't list is to not claim "no sheds yet" — which `empty` handles. */}
      {empty ? (
        <Empty icon={Boxes} title={empty.title} body={empty.body} />
      ) : (
        groupByHost(sheds).map(([host, rows]) => (
          <div key={host} className="mb-[22px] last:mb-0">
            <HostLabel host={host} />
            <div className="flex flex-col gap-3">
              {rows.map((s) => {
                const running = s.status === "running";
                const busy = isBusy(s);
                const dotColor = running ? "var(--shed-ok)" : busy ? "var(--shed-attention)" : "var(--shed-text-muted)";
                const delivery = readDelivery(s.delivery);
                // The row's detail: its delivery (when it has a record) and its egress.
                const detailShown = dlv.open?.server === s.host && dlv.open?.shed === s.name;
                const toggleDetail = () => dlv.setOpen(detailShown ? null : { server: s.host, shed: s.name });
                return (
                  <div key={`${host}/${s.name}`} data-shed-row data-shed-host={s.host} data-shed-name={s.name}
                    data-scroll-anchor={`shed:${s.host}/${s.name}`}
                    className={cardCls} style={{ animation: "shed-in .25s ease" }}>
                  <div className="flex items-center gap-3.5 px-[18px] py-[15px]">
                    <Dot className="h-2.5 w-2.5" style={{ background: dotColor }} />
                    <div className="min-w-0 flex-1">
                      <div className="mb-1.5 flex flex-wrap items-center gap-2.5">
                        <span className="text-[16px] font-semibold text-shed-text">{s.name}</span>
                        {s.backend && <Tag kind={s.backend} />}
                        {s.image && <ImageChip>{s.image}</ImageChip>}
                        {delivery ? (
                          <DeliveryBadge rec={delivery} open={detailShown} onToggle={toggleDetail} />
                        ) : (
                          <button
                            type="button"
                            onClick={toggleDetail}
                            title={detailShown ? "Hide details" : "Show details"}
                            aria-expanded={detailShown}
                            className="hbtn inline-flex flex-none items-center gap-1 rounded-md px-1.5 py-[3px] font-mono text-[10.5px] font-semibold leading-none"
                            style={{ background: "var(--shed-inset)", color: "var(--shed-text-secondary)" }}
                            data-shed-detail-toggle
                          >
                            details {detailShown ? <ChevronDown size={11} /> : <ChevronRight size={11} />}
                          </button>
                        )}
                      </div>
                      <MetaLine s={s} />
                      {running && (() => {
                        const target = `roost:${s.host}/${s.name}`;
                        const craze = crazeOf(target);
                        const crazeNote = crazeMachineNote(craze);
                        return (
                          <>
                            <RoostLine
                              target={target}
                              state={roostBoard[target]}
                              busyDetail={roostBusy[target]}
                              onOpenConsent={onOpenConsent}
                            />
                            {/* The shed's craze (plan 026 §3.2): New craze
                                session where it can create one — a dormant
                                shed included, whose sheet opening starts its
                                hub — or the note when it is too old. A shed
                                without craze says nothing. */}
                            {(crazeNote || crazeCreateOffered(craze)) && (
                              <div data-shed-craze={target} className={cn("mt-2", !crazeNote && "-ml-2")}>
                                {crazeNote ? (
                                  <div className="font-mono text-[12px]" style={{ color: "var(--shed-warn-fg)" }} data-craze-note>
                                    {crazeNote}
                                  </div>
                                ) : (
                                  <CrazeCreateAction machine={{ name: target, craze }} onCrazeCreate={onCrazeCreate} compact />
                                )}
                              </div>
                            )}
                          </>
                        );
                      })()}
                    </div>
                    <div className="flex gap-2" data-shed-controls>
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
                  {/* The egress section leads: it is short, and the delivery's item
                      table below it can be long. Keyed by the incarnation: a shed
                      created again under its name is another shed, and starts with
                      none of the old one's blocks. */}
                  {detailShown && (
                    <ShedEgressDetail key={`${s.host}/${s.name}/${s.created_at ?? ""}`} shed={s} />
                  )}
                  {detailShown && delivery && (
                    <ShedDeliveryDetail
                      shed={s}
                      rec={delivery}
                      prov={provisionRunOf(dlv, s.host, s.name)}
                      onDeliver={() => dlv.deliver(s.host, s.name, refresh)}
                    />
                  )}
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

/* ---- Agents / remote-control ---------------------------------------------- */
/** State → status-chip tone: green ready, red dead, amber for in-progress /
    needs-action (the mapping the pane has always used, now on the shared vocab). */
function rcStateTone(state: RcState): Tone {
  if (state === "ready") return "ok";
  if (state === "dead") return "danger";
  return "attention";
}

function AgentsPane({ sessions, hosts, hasUnwatchedRunningShed, load, loadError, refresh, onTranscript, onSetUpRoost, onCrazeCreate }:
  { sessions: RcSession[]; hosts: MachineStatus[]; hasUnwatchedRunningShed: boolean; load: AgentsLoad; loadError: string | null; refresh: () => void; onTranscript: (s: RcSession) => void; onSetUpRoost: () => void; onCrazeCreate: (target: string) => void }) {
  const [error, setError] = useState<string | null>(null);
  // The empty state is a VALUE, not inline JSX: it is reported as UI truth so
  // `agents.dump` can assert the words — the `dashboard.dump.empty` rule, and
  // the only way to drive an empty pane, which has nothing on screen to read.
  //
  // `load`/`loadError` are what keep the three blanks apart (see
  // `agentsEmpty.ts`): still reading, could not read, and read-and-empty are one
  // screen but three different pieces of news, and only the last one may offer
  // the bootstrap.
  const empty = sessions.length === 0 ? agentsEmptyState(hosts, load, loadError, hasUnwatchedRunningShed) : null;

  // Refresh the shared RC state on mount so navigating to the pane re-lists (the
  // shared state, so the sidebar badge stays consistent — one source of truth).
  useEffect(() => { refresh(); }, [refresh]);
  // Publish the rendered sessions + empty state so the `agents.dump` op can
  // observe them. This effect stays IN the pane so reportAgents fires ONLY while
  // the pane is mounted (the backend blanks the agents snapshot off-pane).
  useEffect(() => {
    // Read AFTER the commit: the Transcript button is the DOM, and a roost
    // opencode tab must be able to prove it rendered none.
    const buttons = new Set(
      Array.from(document.querySelectorAll<HTMLElement>("[data-agent-transcript]")).map(
        (el) => el.dataset.agentTranscript ?? "",
      ),
    );
    const chips = new Map(
      Array.from(document.querySelectorAll<HTMLElement>("[data-agent-activity]")).map((el) => [
        el.dataset.agentActivity ?? "",
        el.dataset.agentActivityLabel ?? "",
      ]),
    );
    reportAgents(
      sessions.map((s) => {
        const key = `${s.target ?? ""}/${s.slug}`;
        return {
          ...s,
          transcript: buttons.has(key),
          activity_chip: chips.has(key) ? chips.get(key) : null,
        };
      }),
      empty,
    );
  }, [sessions, empty?.state, empty?.title, empty?.body, empty?.action]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <div>
      <PageHead
        title="Agents"
        accessory={<span className="font-mono text-[13px] text-shed-text-muted">{sessions.length} active</span>}
        right={
          <div className="flex items-center gap-[18px]">
            <RefreshHeadButton onClick={refresh} />
          </div>
        }
      />
      {error && (
        <div className={cn(cardCls, "mb-3 flex items-start gap-2 p-3.5 text-[13px]")} style={{ borderColor: "var(--shed-danger)", color: "var(--shed-danger)" }}>
          <X size={16} className="mt-px flex-none" /> <span className="min-w-0 break-words">{error}</span>
        </div>
      )}
      {empty ? (
        // The offer beside the sentence, when there IS one: a shed's sessions
        // come from its own roost-session, so the thing to DO about an empty
        // list is to set one up — which happens on the shed's card, one pane
        // over. A pane that is still loading, or that failed to load, offers
        // nothing: it has made no claim about what is running.
        <Empty
          icon={Sparkles}
          title={empty.title}
          body={empty.body}
          action={empty.action ? { label: empty.action, onClick: onSetUpRoost } : undefined}
        />
      ) : (
        // Grouped by where they run, the same treatment the Sheds pane gives
        // hosts. The heading carries the origin, so a row no longer has to:
        // said once per group instead of once per card.
        groupBy(sessions, sessionOrigin).map(([origin, rows]) => (
          <div key={origin} className="mb-[22px] last:mb-0">
            {/* The group's heading — and, on a machine whose craze can create
                one (live with the capability, or dormant: plan 025 §3.6.5),
                its "New craze session". */}
            <div className="flex items-start justify-between gap-3">
              <HostLabel host={origin} />
              <CrazeCreateAction machine={groupMachine(origin, rows, hosts)} onCrazeCreate={onCrazeCreate} compact />
            </div>
            <div className="flex flex-col gap-3">
              {rows.map((s) => (
                // Keyed by the shed target plus the slug, so two sheds that reuse a
                // roost tab id do not collide into one React key.
                <SessionCard key={sessionKey(s)} session={s} onKilled={refresh} onError={setError} onTranscript={onTranscript} />
              ))}
            </div>
          </div>
        ))
      )}
    </div>
  );
}

/** "New craze session" on a machine (plan 025 §3.8): offered where its craze
 *  can create one — a live hub with the capability, or a DORMANT machine,
 *  whose sheet opening is the explicit action that starts its hub (§3.6.5).
 *  Too old, offline, absent or not installed: nothing (the card's note says
 *  why when there is something to say). */
function CrazeCreateAction({ machine: m, onCrazeCreate, compact }:
  { machine: { name: string; craze?: CrazeStatusLike } | undefined; onCrazeCreate: (machine: string) => void; compact?: boolean }) {
  if (!m || !crazeCreateOffered(m.craze)) return null;
  return (
    <button
      data-craze-create-action={m.name}
      onClick={() => onCrazeCreate(m.name)}
      title={`Start a craze session on ${m.name}: pick a provider, a directory and an optional first prompt`}
      className={cn(
        "hlink inline-flex flex-none items-center gap-1.5 rounded-[8px] font-semibold",
        compact ? "-mt-1 px-2 py-1 text-[12px]" : "px-3 py-2 text-[13px]",
      )}
      style={{ color: "var(--shed-accent)" }}
    >
      <Plus size={compact ? 13 : 15} /> New craze session
    </button>
  );
}

function SessionCard({ session: s, onKilled, onError, onTranscript }:
  { session: RcSession; onKilled: () => void; onError: (e: string) => void; onTranscript: (s: RcSession) => void }) {
  const [busy, setBusy] = useState(false);
  // WORKDIR FIRST: on a narrow pane this truncates, and the working directory
  // is what tells two sessions on the same box apart. The origin is not here at
  // all — the pane is grouped by it.
  const sub = s.workdir ?? "";
  const act = rcActivityLabel(s);
  const rail = sessionRail(s);
  // A craze session's row is its hub's: the provider and model, what it is
  // doing, what it is blocked on, who is attached. A roost-detected row is
  // status only — its label and activity stay, and the desktop does not
  // launch, kill, or open a transcript for it.
  const craze = s.source === "craze";
  const doing = craze ? crazeDoingLine(s) : null;
  const asks = craze && typeof s.pending_approvals === "number" ? s.pending_approvals : 0;
  // Open in terminal: a headless craze session gets `craze attach` in a new
  // roost tab.
  const canAttachCraze = canOpenTerminal(s);
  const attachCraze = async () => {
    setBusy(true);
    try { await crazeOpenTerminal(s.target ?? "", s.slug); onKilled(); }
    catch (e) { onError(String(e)); }
    finally { setBusy(false); }
  };
  return (
    <div
      className={cn(cardCls, "px-[18px] py-4")}
      // A stale row is the last KNOWN state of a machine that is currently
      // unreachable — dimmed rather than hidden, because "mini3 is asleep and
      // these were its sessions" is more useful than an empty list.
      style={{
        animation: "shed-in .25s ease",
        opacity: s.stale ? 0.55 : 1,
        // The rail as a left border. Only the left side is coloured, so the
        // padding compensates to keep the text where it was.
        borderLeft: rail ? `4px solid ${rail}` : undefined,
        paddingLeft: rail ? 15 : undefined,
      }}
    >
      {/* Two rows, and which row a thing is on is the point. The NAME and the
          two status badges are what you scan a list for, so they own the top
          line alone — badges hard right, where the eye already goes. What the
          session IS and what it is working on (kind, directory) sits under the
          name, beside the controls that act on it. All five on one line is what
          made the shipped card read as a wall. */}
      <div className="flex items-start gap-4">
        <span className="min-w-0 flex-1 truncate text-[16px] font-semibold text-shed-text">{s.display_name}</span>
        <span className="flex flex-none items-center gap-2">
          {/* roost's `has_notification` bit, sticky until the user focuses the
              tab THERE — shed never clears it, so it is deliberately not folded
              into `needsYou`/activity: a dot, not a claim that this needs you
              right now. */}
          {s.attention && (
            <span
              title="roost notification"
              style={{ width: 8, height: 8, borderRadius: 9999, background: "var(--shed-attention)", flex: "none" }}
            />
          )}
          {/* A craze row has no roost lifecycle to show — its activity,
              its start error and its staleness say what there is to say. */}
          {!craze && <StatusChip tone={rcStateTone(s.state)} label={s.state} />}
          {act && (
            <span data-agent-activity={`${s.target ?? ""}/${s.slug}`} data-agent-activity-label={act.label}>
              <StatusChip tone={act.tone} label={act.label} />
            </span>
          )}
        </span>
      </div>
      <div className="mt-2 flex items-center gap-4">
        <div className="flex min-w-0 flex-1 items-center gap-2.5">
          <KindBadge kind={s.kind} />
          {craze && s.provider && <FactChip label={s.provider} title="provider" />}
          {craze && s.model && <FactChip label={s.model} title="model" />}
          <span className="truncate font-mono text-[12px] text-shed-text-muted">
            {s.stale || (craze && s.approximate) ? `${sub} · last known` : sub}
          </span>
        </div>
        <div className="flex flex-none items-center gap-1.5">
          {/* The agent lane (plan 015 §3.4). `agent_lane`'s PRESENCE is the whole
              capability signal — a row that has it can open a live transcript,
              one that does not is status-only — so the affordance is gated on
              exactly that and on nothing about the kind. It LEADS on the rows
              that have it: a machine row has no tmux pane to attach to, so this
              is how you read the session at all. */}
          {craze && s.agent_lane && s.target && (
            <button
              data-agent-transcript={`${s.target}/${s.slug}`}
              onClick={() => onTranscript(s)}
              title="Open the live transcript"
              className="hbtn inline-flex items-center gap-[7px] rounded-[9px] px-[14px] py-2.5 text-[13px] font-medium"
              style={{ background: "var(--shed-surface)", border: "1px solid var(--shed-border)", color: "var(--shed-text-secondary)" }}
            >
              <ScrollText size={15} /> Transcript
            </button>
          )}
          {/* Open in terminal on a HEADLESS craze session: `craze attach` in
              a new roost tab. What the two ways out of that tab mean is
              craze's, and the tooltip says it (plan 025 §3.6.6). */}
          {canAttachCraze && (
            <button
              data-craze-open-terminal={s.slug}
              onClick={() => void attachCraze()}
              disabled={busy}
              title="Attach to this session in a new roost terminal tab. Closing the tab only detaches; typing /exit in it STOPS the session."
              className="hbtn inline-flex items-center gap-[7px] rounded-[9px] px-[14px] py-2.5 text-[13px] font-medium"
              style={{ background: "var(--shed-surface)", border: "1px solid var(--shed-border)", color: "var(--shed-text-secondary)" }}
            >
              <Terminal size={15} /> Open in terminal
            </button>
          )}
          {/* claude.ai is a second way in, not a better one — a quiet square
              beside the terminal rather than a labelled pill competing with
              it. */}
          {s.url && (
            <a
              href={s.url}
              target="_blank"
              rel="noreferrer"
              title="Open in Claude"
              className="hbtn inline-flex h-9 w-9 flex-none items-center justify-center rounded-[9px]"
              style={{ background: "var(--shed-surface)", border: "1px solid var(--shed-border)", color: "var(--shed-accent)" }}
            >
              <ExternalLink size={15} />
            </a>
          )}
        </div>
      </div>
      {craze && (doing || asks > 0 || (s.attached ?? 0) > 0 || s.start_error) && (
        <div className="mt-2 flex flex-col gap-1" data-craze-facts>
          {doing && (
            <div
              className="truncate text-[13px]"
              style={{ color: doing.dimmed ? "var(--shed-text-muted)" : "var(--shed-text-secondary)" }}
            >
              {doing.text}
            </div>
          )}
          {asks > 0 && (
            <div className="truncate text-[13px]" style={{ color: "var(--shed-warn-fg)" }}>
              {asks === 1 ? "needs you" : `needs you · ${asks}`}
              {s.head_ask_summary ? ` — ${s.head_ask_summary}` : ""}
            </div>
          )}
          {(s.attached ?? 0) > 0 && (
            <div className="truncate font-mono text-[12px] text-shed-text-muted">
              {s.attached === 1 ? "1 client attached" : `${s.attached} clients attached`}
            </div>
          )}
          {s.start_error && (
            <div className="truncate font-mono text-[12px]" style={{ color: "var(--shed-danger)" }}>
              {s.start_error}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

/* ---- Activity pane -------------------------------------------------------- */
/** How many events a page asks for. */
const ACTIVITY_PAGE = 50;

/** The servers' own activity (`GET /api/activity` on every configured server, merged
 *  newest first): what happened to your sheds, who did it, and how it ended. The
 *  server decides what a user may see. A server that could not be read is named
 *  above the list and keeps its place: its events come with a later page. */
function ActivityPane() {
  const [feed, setFeed] = useState<ActivityFeed>(EMPTY_FEED);
  const [state, setState] = useState<"loading" | "ready" | "failed">("loading");
  const [older, setOlder] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const root = useRef<HTMLDivElement>(null);
  const gen = useRef(0);
  const feedNow = useRef(feed);
  feedNow.current = feed;
  const olderNow = useRef(false);
  const why = (e: unknown) => (typeof e === "string" ? e : e instanceof Error ? e.message : String(e));

  const load = useCallback(() => {
    const g = ++gen.current;
    olderNow.current = false;
    setOlder(false);
    setState("loading");
    void listServerActivity({ limit: ACTIVITY_PAGE }).then(
      (p) => {
        if (g !== gen.current) return;
        setFeed(appendPage(EMPTY_FEED, readPage(p)));
        setFailure(null);
        setState("ready");
      },
      (e: unknown) => {
        if (g !== gen.current) return;
        setFailure(why(e));
        setState("failed");
      },
    );
  }, []);
  useEffect(() => load(), [load]);

  // Older events, from the cursor the last page left. One read at a time.
  const loadOlder = useCallback(() => {
    const cursor = feedNow.current.next;
    if (!cursor || olderNow.current) return;
    const g = gen.current;
    olderNow.current = true;
    setOlder(true);
    void listServerActivity({ limit: ACTIVITY_PAGE, cursor })
      .then(
        (p) => {
          if (g === gen.current) setFeed((f) => appendPage(f, readPage(p)));
        },
        (e: unknown) => {
          if (g === gen.current) setFailure(why(e));
        },
      )
      .finally(() => {
        if (g !== gen.current) return;
        olderNow.current = false;
        setOlder(false);
      });
  }, []);

  // The harness's press (`ui.press_activity_older`, test mode only): this pane's own
  // button, clicked, so a hidden or disabled one takes no press.
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let un: (() => void) | undefined;
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const u = await listen("press-activity-older", () => {
        const btn = root.current?.querySelector<HTMLButtonElement>("button[data-activity-older]");
        if (btn && !btn.disabled) btn.click();
      });
      if (cancelled) u();
      else un = u;
    })();
    return () => {
      cancelled = true;
      un?.();
    };
  }, []);

  // Report what the pane rendered (`activity.dump`), read back off its own nodes.
  const key = JSON.stringify([feed, state, older, failure]);
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    const btn = el.querySelector<HTMLButtonElement>("button[data-activity-older]");
    reportActivity({
      state,
      events: Array.from(el.querySelectorAll<HTMLElement>("[data-activity-event]")).map((row) => {
        const cell: Record<string, string | null> = { id: row.dataset.activityEvent ?? null };
        row.querySelectorAll<HTMLElement>("[data-col]").forEach((c) => {
          cell[c.dataset.col!] = c.textContent;
        });
        return cell;
      }),
      errors: Array.from(el.querySelectorAll("[data-activity-error]")).map((e) => e.textContent),
      failure: el.querySelector("[data-activity-failure]")?.textContent ?? null,
      older: btn ? { disabled: btn.disabled, text: btn.textContent } : null,
      empty: el.querySelector("[data-activity-empty]")?.textContent ?? null,
    });
  }, [key]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => reportActivity(null), []);

  return (
    <div ref={root} data-scroll-anchor="activity">
      <PageHead
        title="Activity"
        sub="What happened to your sheds on each server, newest first."
        right={<RefreshHeadButton onClick={load} />}
      />
      {feed.errors.length > 0 && (
        <div className={cn(cardCls, "mb-3 flex flex-col gap-1 px-[18px] py-3")} style={{ borderColor: "var(--shed-danger)" }}>
          {feed.errors.map((e) => (
            <div key={e.server} className="break-words font-mono text-[12px]" style={{ color: "var(--shed-danger)" }} data-activity-error>
              {errorText(e)}
            </div>
          ))}
        </div>
      )}
      {failure && (
        <div className="mb-3 break-words font-mono text-[12px]" style={{ color: "var(--shed-danger)" }} data-activity-failure>
          {failure}
        </div>
      )}
      {state === "loading" ? (
        <div className="px-1 py-8 text-center text-[13px] text-shed-text-muted">Loading activity…</div>
      ) : (
        <>
          {/* A page can be empty and still have a continuation (a server's scan
              budget ran out among events this user may not see), so the empty
              state never hides "Load older". */}
          {feed.events.length === 0 ? (
            <div data-activity-empty>
              <Empty
                icon={ScrollText}
                title={feed.next ? "No activity on this page" : "No activity yet"}
                body={feed.next
                  ? "The servers have older events to look through: load the next page."
                  : "Creating, starting, stopping and provisioning a shed, and each delivery, appear here."}
              />
            </div>
          ) : (
          <div className={cn(cardCls, "overflow-hidden")}>
            {feed.events.map((e, i) => {
              const tone = outcomeTone(e);
              const outcome = deliveryText(e.delivery) ?? e.result ?? "";
              return (
                <div
                  key={`${e.server}/${e.id}`}
                  className={cn("row-hover flex items-center gap-3 px-[18px] py-[11px]", i > 0 && "border-t border-shed-border")}
                  data-activity-event={`${e.server}/${e.id}`}
                >
                  <span className="w-[128px] flex-none font-mono text-[12px] text-shed-text-muted" data-col="at">{eventTime(e.at)}</span>
                  <span className="flex-none rounded-md px-[7px] py-1 font-mono text-[11px] font-semibold leading-none" style={{ background: "var(--shed-inset)", color: "var(--shed-text-secondary)" }} data-col="server">{e.server}</span>
                  <span className="flex-none rounded-md px-[7px] py-1 font-mono text-[11px] font-semibold leading-none" style={{ background: "var(--shed-agent-pill-bg)", color: "var(--shed-agent-pill-text)" }} data-col="kind">{e.kind}</span>
                  <span className="min-w-0 flex-1 truncate text-[13.5px]">
                    <span className="font-semibold text-shed-text" data-col="shed">{e.shed.name}</span>
                    <span className="text-shed-text-muted"> · </span>
                    <span className="text-shed-text-secondary" data-col="actor">{actorText(e.actor)}</span>
                  </span>
                  <span
                    className="flex-none text-[12.5px] font-semibold"
                    style={{ color: tone === "ok" ? "var(--shed-ok)" : tone === "bad" ? "var(--shed-denied)" : "var(--shed-text-muted)" }}
                    data-col="outcome"
                  >
                    {outcome}
                  </span>
                </div>
              );
            })}
          </div>
          )}
          {feed.next && (
            <div className="mt-3 flex justify-center">
              <button
                type="button"
                onClick={loadOlder}
                disabled={older}
                className="hbtn rounded-[9px] border border-shed-border bg-shed-surface px-4 py-2 text-[13px] font-medium text-shed-text-secondary"
                style={{ opacity: older ? 0.6 : 1 }}
                data-activity-older
              >
                {older ? "Loading…" : "Load older"}
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

/* ---- System pane ---------------------------------------------------------- */
/** The config|user source badge of an egress profile (user = accent tint, config = neutral). */
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

/** Every server's egress profiles (config baseline and user-managed), view-only: they
 *  are managed with `shed egress profile`. A server that could not list them (egress
 *  not enabled, unreachable) says so in its own row. */
function EgressProfilesSection({ hosts, loaded }: { hosts: HostEgressProfiles[]; loaded: boolean }) {
  const root = useRef<HTMLDivElement>(null);
  const key = JSON.stringify([hosts, loaded]);
  // Report what was RENDERED, read back off the section's own nodes after the commit,
  // so a field the pane does not show cannot pass as shown.
  useEffect(() => {
    const el = root.current;
    if (!loaded || !el) return;
    const text = (n: Element, f: string) => n.querySelector(`[data-field="${f}"]`)?.textContent ?? null;
    const items = (n: Element, f: string) =>
      Array.from(n.querySelectorAll(`[data-field="${f}"] [data-item]`)).map((i) => i.textContent ?? "");
    reportEgress({
      profiles: Array.from(el.querySelectorAll<HTMLElement>("[data-egress-profile]")).map((n) => ({
        host: n.dataset.egressHost ?? "",
        name: text(n, "name") ?? "",
        source: text(n, "source") ?? "",
        mode: text(n, "mode"),
        allow: items(n, "allow"),
        deny: items(n, "deny"),
        rule: text(n, "rule"),
      })),
      errors: Array.from(el.querySelectorAll<HTMLElement>("[data-egress-host-error]")).map((n) => ({
        host: n.dataset.egressHostError ?? "",
        error: n.textContent ?? "",
      })),
    });
  }, [key]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => reportEgress(null), []);
  return (
    <div ref={root} className="mt-7" data-system-egress data-scroll-anchor="system-egress">
      <div className="mb-3 pl-0.5 font-mono text-[11px] font-semibold uppercase leading-none tracking-[.09em] text-shed-text-muted">Egress profiles</div>
      {!loaded ? (
        <div className="px-1 py-4 text-[13px] text-shed-text-muted">Loading egress profiles…</div>
      ) : hosts.length === 0 ? (
        <div className="px-1 py-4 text-[13px] text-shed-text-muted">No servers are configured.</div>
      ) : (
        <div className="flex flex-col gap-3">
          {hosts.map((h) => (
            <div key={h.host} className={cn(cardCls, "px-[19px] py-[15px]")} data-egress-host={h.host}>
              <div className="mb-2 text-[14px] font-semibold text-shed-text">{h.host}</div>
              {h.error ? (
                <div className="break-words font-mono text-[12px] leading-relaxed" style={{ color: "var(--shed-danger)" }} data-egress-host-error={h.host}>{h.error}</div>
              ) : h.profiles.length === 0 ? (
                <div className="text-[12.5px] text-shed-text-muted">No egress profiles. Define them in the server config or with `shed egress profile set`.</div>
              ) : (
                <div className="flex flex-col gap-2.5">
                  {h.profiles.map((p) => (
                    <div key={`${p.source}/${p.name}`} data-egress-profile={`${h.host}/${p.name}`} data-egress-host={h.host}>
                      <div className="flex items-center gap-2">
                        <span className="text-[13.5px] font-semibold text-shed-text" data-field="name">{p.name}</span>
                        <span data-field="source" className="contents"><SourceBadge source={p.source} /></span>
                        <span className="font-mono text-[11px] text-shed-text-muted">{profileSummary(p.profile)}</span>
                      </div>
                      {p.profile.mode && (
                        <div className="mt-0.5 font-mono text-[12px] leading-relaxed text-shed-text-secondary">
                          <span className="text-shed-text-muted">mode </span>
                          <span data-field="mode">{p.profile.mode}</span>
                        </div>
                      )}
                      {(["allow", "deny"] as const).map((k) =>
                        (p.profile[k] ?? []).length ? (
                          <div key={k} className="mt-0.5 font-mono text-[12px] leading-relaxed text-shed-text-secondary">
                            <span className="text-shed-text-muted">{k} </span>
                            <span data-field={k}>
                              {(p.profile[k] ?? []).map((item, i) => (
                                <span key={i}>{i > 0 && ", "}<span data-item>{item}</span></span>
                              ))}
                            </span>
                          </div>
                        ) : null,
                      )}
                      {p.profile.rule && (
                        <div className="mt-0.5 break-words font-mono text-[12px] leading-relaxed text-shed-text-secondary">
                          <span className="text-shed-text-muted">rule </span>
                          <span data-field="rule">{p.profile.rule}</span>
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function SystemPane({ sheds }: { sheds: Shed[] }) {
  const [rows, setRows] = useState<HostDiskUsage[]>([]);
  // Generation guard (as in useUiBridge's fetchSheds): a slower earlier Refresh
  // can't resolve last and overwrite a newer fetch with stale rows.
  const [egress, setEgress] = useState<HostEgressProfiles[]>([]);
  const [egressLoaded, setEgressLoaded] = useState(false);
  const gen = useRef(0);
  const load = useCallback(() => {
    const g = ++gen.current;
    void fetchSystemDf().then((r) => {
      if (g === gen.current) setRows(r);
    });
    void fetchEgressProfiles().then((r) => {
      if (g !== gen.current) return;
      setEgress(r);
      setEgressLoaded(true);
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
        sub="Disk usage per server (images, sheds, snapshots, orphans), and each server's egress profiles."
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
      <EgressProfilesSection hosts={egress} loaded={egressLoaded} />
    </div>
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
  // The open shed delivery detail, its `ui.show_shed_delivery` doors, and every
  // shed's Deliver again run (plan 039 A7).
  const shedDelivery = useShedDelivery(setPane);
  const [mode, setMode] = useState<"light" | "dark">("light");
  // The open modal — and its LIVE value (`modalNow`), written synchronously by
  // the one setter, which is what a craze create that resolves later decides
  // by (`crazeCreate.ts`'s `liveSetter`): a ref a passive effect copies into
  // lags a render.
  const [modal, publishModal] = useState<Modal>(null);
  const modalNow = useRef<Modal>(null);
  const setModal = useMemo(() => liveSetter(modalNow, publishModal), []);
  // The open agent-lane transcript, if any (plan 015 §3.4). Not a pane: it
  // belongs to a ROW, so it lives beside the panes rather than replacing one —
  // and it stays put when you navigate, because a transcript you opened is
  // something you are reading, not somewhere you went.
  // `settings` is whether its settings sheet is asked for (the header chip, or
  // `ui.show_lane_settings`); the panel shows it only on a session whose
  // capabilities say `settings`. Opening a lane any other way asks for none.
  // `opens` moves when a resume lands on the session the panel already shows
  // (`laneOnResume`): the panel opens its lane again in place.
  const [lane, setLane] = useState<LaneTarget | null>(null);

  // -- roost bootstrap (plan 019 §3.6, C8) ---------------------------------
  // The open consent card, if any — one at a time, App-level like `lane`
  // (opened from a card's plan-matrix button, or headlessly via
  // `ui.show_roost_consent` for the harness, which has no click).
  const [roostConsent, setRoostConsent] = useState<{ target: string; preview: RoostPreview } | null>(null);
  // Per-target indeterminate-progress phrase, non-null exactly while THAT
  // target's `roost.bootstrap` is in flight. A map (not one flag) because two
  // cards can be mid-run at once — a bootstrap is per-host, and nothing here
  // serializes a click on one card behind another's.
  const [roostBusy, setRoostBusy] = useState<Record<string, string>>({});
  // Bumped after every settled bootstrap so `useRoostBoard` re-previews the
  // host it just acted on (and every other row sharing the board) — a plain
  // re-fetch, never a poll.
  const [roostTick, setRoostTick] = useState(0);
  const [toast, setToast] = useState<RoostToast | null>(null);

  const openRoostConsent = useCallback((target: string, preview: RoostPreview) => {
    setRoostConsent({ target, preview });
  }, []);

  // The open card, mirrored into a ref. The `confirm-roost-consent` listener is
  // registered ONCE (its effect has `[]` deps), so the handler it captures must
  // not close over a particular render's `roostConsent` — and the previous
  // version read it out of a functional `setRoostConsent` updater instead, which
  // is the bug this fixes: an updater must be PURE, and React StrictMode (on in
  // `main.tsx`) calls it twice, so one Confirm click started two bootstraps.
  const openCard = useRef<{ target: string; preview: RoostPreview } | null>(null);
  useEffect(() => { openCard.current = roostConsent; }, [roostConsent]);
  // Targets whose bootstrap this shell has already started and not yet settled.
  // `roostBusy` says the same thing, but it is STATE — it lands a render later,
  // and two Confirms in one tick would both read it empty. A ref is the only
  // thing that is true immediately.
  const roostRunning = useRef<Set<string>>(new Set());

  // Confirm: close the card (there is nothing further to consent to) and run
  // the bootstrap in the BACKGROUND, cycling the confirmed plan's progress
  // phrases on the row itself — see `roost.ts`'s `roostProgressSteps` doc for
  // why this is decorative rather than server-pushed. Shared by the dialog's
  // own button and the headless `confirm-roost-consent` door.
  //
  // **One click is one bootstrap.** Every side effect happens right here, in an
  // event handler, never inside a state updater; the claim on `roostRunning` is
  // taken before the first `await`, so a doubled event (a double click, a
  // re-delivered door) is dropped rather than installed over itself.
  const confirmRoostBootstrap = useCallback(() => {
    const current = openCard.current;
    if (!current) return;
    const { target, preview } = current;
    if (roostRunning.current.has(target)) return;
    roostRunning.current.add(target);
    openCard.current = null;
    setRoostConsent(null); // close the card
    const steps = roostProgressSteps(preview.plan.kind);
    let i = 0;
    setRoostBusy((b) => ({ ...b, [target]: steps[0] }));
    const timer = window.setInterval(() => {
      i = Math.min(i + 1, steps.length - 1);
      setRoostBusy((b) => (target in b ? { ...b, [target]: steps[i] } : b));
    }, 2500);
    void roostBootstrap(target, preview.probe.fingerprint)
      .then(
        (result) => setToast(roostToastFor(result)),
        (e: unknown) => setToast({ tone: "error", lines: [String(e)] }),
      )
      .finally(() => {
        window.clearInterval(timer);
        roostRunning.current.delete(target);
        setRoostBusy((b) => {
          const { [target]: _drop, ...rest } = b;
          return rest;
        });
        setRoostTick((t) => t + 1);
      });
  }, []);

  // The consent card and the toast report THEMSELVES (`roost_consent.dump` /
  // `toast.dump`, in `components/RoostBootstrap.tsx`) — including the DOM text
  // they painted. Reporting them from here instead would keep answering about a
  // card or a toast that had been deleted from the tree.
  //
  // Auto-dismiss, like a toast should — long enough to read the hook summary.
  useEffect(() => {
    if (!toast) return;
    const t = window.setTimeout(() => setToast(null), 8000);
    return () => window.clearTimeout(t);
  }, [toast]);
  // The SINGLE source of truth for RC sessions + capabilities: the sidebar badge,
  // the Agents pane, and the launch dialog all read this one `rc.list` state, so
  // they can't diverge. `refreshRc` reloads on the pane Refresh button, a
  // launch/kill, and pane/dialog open; the hook itself reloads on mount + `refresh`.
  const {
    sessions: rcSessions, hosts: rcHosts, shedCraze: rcShedCraze,
    load: rcLoad, error: rcLoadError, refresh: refreshRc,
  } = useRcSessions();

  // -- the craze create sheet (plan 025 §3.8, C10) ---------------------------
  // Which machine's sheet is open (with `modal === "craze"`), and every
  // machine's DRAFT — the typed form, the request id, the submission's phase —
  // held HERE, above the sheet: dismissing the sheet keeps a submission
  // running (its session appears as a row), and re-opening it finds the same
  // form and the same id (`crazeCreate.ts` has the lifecycle). The ref is the
  // authoritative copy, written synchronously, so a submit right behind an
  // edit reads the edit; the state mirrors it for rendering.
  const [crazeMachine, publishCrazeMachine] = useState<string | null>(null);
  const crazeMachineNow = useRef<string | null>(null);
  const setCrazeMachine = useMemo(() => liveSetter(crazeMachineNow, publishCrazeMachine), []);
  const crazeDrafts = useRef<Record<string, CrazeDraft>>({});
  const [drafts, setDrafts] = useState<Record<string, CrazeDraft>>({});
  const updateDraft = useCallback((machine: string, f: (d: CrazeDraft) => CrazeDraft) => {
    const next = f(crazeDrafts.current[machine] ?? EMPTY_DRAFT);
    crazeDrafts.current = { ...crazeDrafts.current, [machine]: next };
    setDrafts(crazeDrafts.current);
  }, []);
  // Every machine's RESUME state (plan 032 §3.4) — the sheet's Resume half:
  // the mode shown, the saved list, each row's reopen — held here beside the
  // drafts and for their reason: a reopen can take minutes, and dismissing
  // the sheet keeps it running (its session appears as a row). The ref is
  // the authoritative copy, the state its mirror (`updateDraft`'s rule).
  // Keyed by machine NAME, which a host removed and registered again keeps,
  // so each state is bound to the INCARNATION its answers came through
  // (`observeSource`, C5 review): a new one starts afresh, and an old one's
  // late answers land nowhere.
  const crazeResumes = useRef<Record<string, CrazeResume>>({});
  const [resumes, setResumes] = useState<Record<string, CrazeResume>>({});
  const updateResume = useCallback((machine: string, f: (r: CrazeResume) => CrazeResume) => {
    const next = f(crazeResumes.current[machine] ?? EMPTY_RESUME);
    crazeResumes.current = { ...crazeResumes.current, [machine]: next };
    setResumes(crazeResumes.current);
  }, []);
  // The token each saved-list read is fenced by (`settleList`), and the
  // attempt number each reopen is (`reopenLanding`/`settleResume`).
  const savedReads = useRef(0);
  const reopenAttempts = useRef(0);
  // An answer about `machine` came through its craze source of generation
  // `gen` — the sheet's create options, read on every open.
  const observeCrazeSource = useCallback((machine: string, gen: number | null | undefined) => {
    updateResume(machine, (r) => observeSource(r, gen));
  }, [updateResume]);
  // **Every status binds every machine's Resume state to its incarnation**
  // (C5 re-review): each `rc.list` names each host's craze source generation
  // (`craze.source_gen`, read through `crazeStatusFor`, so an unlisted
  // dormant shed's comes from its `shed_craze` entry), and it is applied to
  // every state held — sheet open or not (`observeSources`). A host removed
  // and registered again under the same name thereby resets what its old
  // incarnation left, a running reopen's lock included, as soon as the new
  // one is SEEN: never waiting on a create-options read, which may fail. The
  // ref holds the latest status for the sheet's opening, below.
  const rcCrazeNow = useRef({ hosts: rcHosts, shedCraze: rcShedCraze });
  const crazeGenOf = useCallback(
    (machine: string) =>
      crazeStatusFor(machine, rcCrazeNow.current.hosts, rcCrazeNow.current.shedCraze)?.source_gen,
    [],
  );
  useEffect(() => {
    rcCrazeNow.current = { hosts: rcHosts, shedCraze: rcShedCraze };
    const next = observeSources(crazeResumes.current, crazeGenOf);
    if (next === crazeResumes.current) return;
    crazeResumes.current = next;
    setResumes(next);
  }, [rcHosts, rcShedCraze, crazeGenOf]);
  const openCrazeSheet = useCallback((machine: string) => {
    // A sheet opening reads afresh (nothing cached) — unless a reopen still
    // runs there, which it then shows as it was left: once the machine's
    // status has said that reopen is still this incarnation's
    // (`observeSource` first — a newer one starts the sheet afresh).
    updateResume(machine, (r) => onSheetOpen(observeSource(r, crazeGenOf(machine))));
    setCrazeMachine(machine);
    setModal("craze");
  }, [setCrazeMachine, setModal, updateResume, crazeGenOf]);
  // Read `machine`'s saved list: on the first choice of Resume since the
  // sheet opened, and on the list's Retry. Only THIS read's answer lands.
  const readSaved = useCallback(async (machine: string) => {
    const token = ++savedReads.current;
    updateResume(machine, (r) => beginList(r, token));
    try {
      const list = await crazeSaved(machine);
      updateResume(machine, (r) => settleList(r, token, { ok: true, list }));
    } catch (e) {
      const f = laneFailure(e);
      updateResume(machine, (r) => settleList(r, token, { ok: false, refusal: { code: f.code, message: f.message } }));
    }
  }, [updateResume]);
  const chooseCrazeMode = useCallback((machine: string, mode: CrazeSheetMode) => {
    updateResume(machine, (r) => chooseMode(r, mode));
    if (listNeeded(crazeResumes.current[machine] ?? EMPTY_RESUME)) void readSaved(machine);
  }, [updateResume, readSaved]);
  // Run one reopen of `machine`'s saved session `id` — one at a time
  // (`canResume`), as its own attempt. Success: the row leaves the list, the
  // rows refresh, and — if this sheet is still the one open — it closes onto
  // the session's transcript at once, as a create does. A refusal lands by
  // its code (`settleResume`: `gone` drops the row, `unavailable` offers
  // Retry, the rest keep it). **Every effect follows `reopenLanding`**,
  // decided on the state the answer lands ON (C5 review): an attempt a newer
  // incarnation of the machine forgot is dropped whole — no state change, no
  // navigation — and a `superseded` answer opens nothing.
  const resumeSaved = useCallback(async (machine: string, id: string) => {
    if (!canResume(crazeResumes.current[machine] ?? EMPTY_RESUME, id)) return;
    const attempt = ++reopenAttempts.current;
    updateResume(machine, (r) => beginResume(r, id, attempt));
    try {
      const reopened = await crazeReopen(machine, id);
      const landing = reopenLanding(crazeResumes.current[machine] ?? EMPTY_RESUME, id, attempt, reopened);
      if (landing === "dropped") return;
      updateResume(machine, (r) => settleResume(r, id, attempt, { ok: true, answer: reopened }));
      refreshRc();
      // Decided by the sheet on screen NOW, read live (C10 review's rule).
      const onScreen = sheetOnScreen(modalNow.current, crazeMachineNow.current);
      if (landing === "superseded") {
        // Said on its row; a sheet not showing it would lose it on its next
        // open, so the toast says it there.
        if (onScreen !== machine) setToast({ tone: "warn", lines: [SUPERSEDED_NOTE] });
        return;
      }
      if (landing === "ended") {
        setToast({ tone: "warn", lines: ["craze reopened the session, and it has already ended."] });
        if (onScreen === machine) setModal(null);
        return;
      }
      if (closesOntoTranscript(onScreen, machine)) {
        setModal(null);
        setPane("agents");
        // The session's transcript — opened AGAIN in place when it is the
        // panel already on screen (a finished lane whose host the resume
        // rejoined): its address, and so its key, would not change.
        setLane((l) => laneOnResume(l, { target: machine, sessionId: reopened.host_id }));
      }
    } catch (e) {
      const f = laneFailure(e);
      updateResume(machine, (r) => settleResume(r, id, attempt, { ok: false, refusal: { code: f.code, message: f.message } }));
    }
  }, [refreshRc, updateResume, setModal]);
  // Run one submission of `machine`'s draft. **The request id is the draft's**
  // — reused while an outcome is unknown, minted anew after any definite one
  // (`beginSubmit`/`settle`) — and a reused one is sent as a held replay
  // (`replaysHeldRequest`, read BEFORE the submission starts: plan 026 §3.1),
  // so nothing but craze's own answer about it ends it. Created: the draft is
  // spent, the rows refresh, a first prompt craze did not take is said, and —
  // if this sheet is still the one open — it closes onto that session's
  // transcript at once.
  const submitCraze = useCallback(async (machine: string) => {
    const current = crazeDrafts.current[machine] ?? EMPTY_DRAFT;
    if (current.phase === "submitting") return;
    const held = replaysHeldRequest(current);
    const started = beginSubmit(current);
    updateDraft(machine, () => started);
    try {
      const created = await crazeCreate({
        target: machine,
        cwd: started.cwd.trim(),
        provider: started.provider,
        prompt: started.prompt,
        // Off the draft this submission is for: a held retry carries the
        // title it was first sent with (`createTitle`).
        title: createTitle(started),
        requestId: started.requestId ?? "",
        held,
      });
      updateDraft(machine, () => EMPTY_DRAFT);
      refreshRc();
      if (created.ended) {
        // A replayed answer for a session that has since ended: craze's word,
        // and nothing to open (the source lists no such session).
        setToast({ tone: "warn", lines: ["craze answered with a session that has already ended."] });
        if (sheetOnScreen(modalNow.current, crazeMachineNow.current) === machine) setModal(null);
        return;
      }
      if (created.prompt !== "accepted" && created.prompt !== "none") {
        setToast({
          tone: "warn",
          lines: [
            created.prompt === "refused"
              ? "craze started the session, and the session refused its first prompt."
              : "craze started the session, and the answer to its first prompt was lost.",
            ...(created.prompt_error ? [created.prompt_error] : []),
          ],
        });
      }
      // Decided by the sheet on screen NOW, read live — not by a copy a
      // passive effect may not have made yet (C10 review).
      if (closesOntoTranscript(sheetOnScreen(modalNow.current, crazeMachineNow.current), machine)) {
        setModal(null);
        setPane("agents");
        setLane({ target: machine, sessionId: created.host_id });
      }
    } catch (e) {
      const f = laneFailure(e);
      updateDraft(machine, (d) => settle(d, { ok: false, refusal: { code: f.code, message: f.message } }));
    }
  }, [refreshRc, updateDraft, setModal]);
  // The Agents sidebar badge count is derived from the shared session list — always
  // consistent with what the Agents pane renders.
  const agentCount = rcSessions.length;
  // Configured hosts WITH reachability — the System pane's source (`system_df`),
  // lifted to the shell so the sidebar HOSTS list + the System nav badge cover the
  // configured hosts (even ones with zero sheds), each with a real reachable/
  // unreachable dot — instead of deriving from `sheds` (which hides zero-shed hosts,
  // undercounts the badge, and paints every dot green). Reuses the existing `refresh`
  // event that drives the shed re-fetch — no extra poll.
  //
  // Newest-wins (shed#400): a burst of `refresh` events overlaps these reads, and
  // an older answer landing second must not overwrite a newer one; a failed read
  // keeps the last rows rather than reading as "no hosts".
  const [hostRows, setHostRows] = useState<HostDiskUsage[]>([]);
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    let unlisten: (() => void) | undefined;
    const rows = newestReload(fetchHostRows, setHostRows);
    const reload = () => void rows.reload();
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      const un = await listen("refresh", reload);
      if (cancelled) un();
      else unlisten = un;
      reload(); // initial fetch, after the listener is live
    })();
    return () => {
      cancelled = true;
      rows.cancel();
      unlisten?.();
    };
  }, []);
  const hostCount = hostRows.length;
  // `badges.hosts` is the configured server count (`system_df`), not the
  // session-host list.
  // The bridge owns the single full `ui_report`; hand it the badge inputs (sheds
  // derived there, hosts/agents from here), the sidebar's rendered rows,
  // and the mode so the report is one complete snapshot, never a partial that
  // races the `main` slot / tray count.
  const { sheds, hostErrors, refresh } = useUiBridge(pane, setPane, modal, mode, agentCount, hostCount, {
    servers: hostRows.map((h) => ({ name: h.host, reachable: h.usage != null })),
  });

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
      // The craze create sheet's door (`ui.show_craze_create {machine}`),
      // the same pattern: it opens from a machine's "New craze session" — a
      // click the harness has none of.
      uns.push(
        await listen<{ target?: unknown }>("show-craze-create", (e) => {
          if (typeof e.payload?.target === "string") openCrazeSheet(e.payload.target);
        }),
      );
      uns.push(await listen("close-craze-create", () => setModal((m) => (m === "craze" ? null : m))));
      // The transcript panel's drivable door (`ui.show_lane` / `ui.close_lane`),
      // the show-create pattern: the panel opens from a card CLICK,
      // and the harness has no click — while `lane.dump` (the panel's own truth,
      // and the whole point of C6) is only observable once one is mounted.
      uns.push(
        await listen<{ target?: unknown; session_id?: unknown }>("show-lane", (e) => {
          const target = e.payload?.target;
          const sessionId = e.payload?.session_id;
          if (typeof target === "string" && typeof sessionId === "string") {
            setLane({ target, sessionId });
          }
        }),
      );
      uns.push(await listen("close-lane", () => setLane(null)));
      // The settings sheet's door (`ui.show_lane_settings`, plan 025 §3.10),
      // the same pattern: the sheet opens from the transcript header's chip —
      // a click — so the door mounts the panel for that session (if it is
      // not the one open) and asks for its sheet.
      uns.push(
        await listen<{ target?: unknown; session_id?: unknown }>("show-lane-settings", (e) => {
          const target = e.payload?.target;
          const sessionId = e.payload?.session_id;
          if (typeof target === "string" && typeof sessionId === "string") {
            setLane({ target, sessionId, settings: true });
          }
        }),
      );
      uns.push(await listen("close-lane-settings", () => setLane((l) => (l ? { ...l, settings: false } : l))));
      // The roost consent card's drivable doors (plan 019 §3.6/C8), same
      // pattern: it opens from a card's plan-matrix BUTTON, a click the
      // harness has none of. `show-roost-consent` re-previews `target` rather
      // than trusting whatever a card last cached, so it works even when the
      // harness drives it before any pane has mounted one.
      uns.push(
        await listen<{ target?: unknown }>("show-roost-consent", (e) => {
          const target = e.payload?.target;
          if (typeof target !== "string") return;
          void roostPreview(target).then(
            (preview) => setRoostConsent({ target, preview }),
            (error: unknown) => setToast({ tone: "error", lines: [String(error)] }),
          );
        }),
      );
      uns.push(await listen("confirm-roost-consent", () => confirmRoostBootstrap()));
      // `ui.scroll_to {anchor}` (test mode): bring a section into view, so a
      // screenshot can show what is below the fold.
      uns.push(
        await listen<{ anchor?: unknown; token?: unknown }>("scroll-to", (e) => {
          const anchor = e.payload?.anchor;
          if (typeof anchor !== "string") return;
          const el = Array.from(document.querySelectorAll<HTMLElement>("[data-scroll-anchor]"))
            .find((x) => x.dataset.scrollAnchor === anchor);
          el?.scrollIntoView({ block: "start" });
          // After a frame, so the scroll has been painted when the harness reads it.
          requestAnimationFrame(() =>
            reportScrolled({
              anchor, token: e.payload?.token ?? null, found: !!el,
              animations: document.getAnimations().length,
            }),
          );
        }),
      );
      uns.push(await listen("close-roost-consent", () => setRoostConsent(null)));
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

  // Sidebar hosts = the configured hosts with reachability (from system_df), NOT the
  // sheds-derived set — so zero-shed hosts still appear, the System badge counts
  // configured hosts, and each dot reflects real reachability.
  // …each carrying the shed-listing failure for that host (if any) as its dot
  // tooltip — the remedy plus the full detail the strip demotes to hover.
  const hosts = hostRows.map((h) => ({
    name: h.host,
    reachable: h.usage != null,
    failure: hostFailureFor(hostErrors, h.host),
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
          const badge =
            id === "sheds" ? sheds.length
            : id === "agents" ? agentCount
            : id === "system" ? hosts.length
            : null;
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
                  style={{ background: "var(--shed-inset)", color: "var(--shed-text-secondary)" }}
                >
                  {badge}
                </span>
              )}
            </button>
          );
        })}
        <div className="flex-1" />
        {/* The status foot: the two kinds of place a session can run, in the same
            vocabulary. This is where "is that box up" lives — which is why the
            Sheds pane no longer carries an error strip. */}
        <SidebarSection label="SHED SERVERS" />
        {hosts.map((h) => (
          <SidebarRow
            key={h.name}
            name={h.name}
            tone={h.reachable ? "ok" : "off"}
            // Unreachable is a word in the list; the reason is on hover.
            note={h.reachable ? undefined : "offline"}
            title={h.failure ? `${h.failure.summary}\n\n${h.failure.detail}` : undefined}
          />
        ))}
      </aside>

      {/* main column */}
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-[52px] flex-none items-center gap-3 border-b border-shed-border bg-shed-bg px-[22px]">
          <Box size={19} className="text-shed-text-secondary" />
          <span className="text-[14px] font-semibold text-shed-text">shed desktop</span>
          <div className="flex-1" />
          <button onClick={() => void openPreferences()} title="Preferences" className="hlink flex h-[34px] w-[34px] items-center justify-center rounded-[9px] text-shed-text-muted">
            <Settings size={18} />
          </button>
          <button onClick={() => setMode(mode === "light" ? "dark" : "light")} title="Toggle appearance" className="hlink flex h-[34px] w-[34px] items-center justify-center rounded-[9px] text-shed-text-muted">
            {mode === "light" ? <Moon size={18} /> : <Sun size={18} />}
          </button>
        </header>
        <main className="flex-1 overflow-auto bg-shed-bg px-[38px] pb-6 pt-7">
          <div data-pane={pane} className="mx-auto max-w-[880px]">
            {pane === "sheds" && (
              <ShedsPane
                sheds={sheds}
                hostErrors={hostErrors}
                refresh={refresh}
                onNew={() => setModal("create")}
                roostTick={roostTick}
                roostBusy={roostBusy}
                onOpenConsent={openRoostConsent}
                hosts={rcHosts}
                shedCraze={rcShedCraze}
                onCrazeCreate={openCrazeSheet}
                delivery={shedDelivery}
              />
            )}
            {pane === "agents" && (
              <AgentsPane
                sessions={rcSessions}
                hosts={rcHosts}
                hasUnwatchedRunningShed={sheds.some(
                  (s) => s.status === "running" && !rcHosts.some((h) => h.target === `roost:${s.host}/${s.name}`),
                )}
                load={rcLoad}
                loadError={rcLoadError}
                refresh={refreshRc}
                // The empty state's offer. A shed's roost-session is installed
                // and started from the shed's OWN card (the plan-matrix line
                // and its consent dialog, plan 019 §3.6) — so the offer is to
                // go there, not a second bootstrap entry point that would have
                // to re-ask which host it was about.
                onSetUpRoost={() => setPane("sheds")}
                onCrazeCreate={openCrazeSheet}
                onTranscript={(s) => {
                  if (s.source === "craze" && s.agent_lane && s.target) {
                    setLane({ target: s.target, sessionId: s.agent_lane.session_id });
                  }
                }}
              />
            )}
            {pane === "activity" && <ActivityPane />}
            {pane === "system" && <SystemPane sheds={sheds} />}
          </div>
        </main>
      </div>
      {/* Keyed by the lane's address so switching rows REMOUNTS the panel: the
          subscription, the batch and the report all hang off that mount, and
          reusing one would leave them pinned to the session you left. */}
      {lane && (
        <LanePanel
          key={`${lane.target}/${lane.sessionId}`}
          target={lane.target}
          sessionId={lane.sessionId}
          opens={lane.opens ?? 0}
          settingsOpen={!!lane.settings}
          onSettingsOpen={(open) => setLane((l) => (l ? { ...l, settings: open } : l))}
          onClose={() => setLane(null)}
        />
      )}
      {modal === "create" && <NewShedDialog refresh={refresh} onClose={() => setModal(null)} />}
      {/* Keyed by machine so another machine's sheet REMOUNTS (its options
          read afresh); the draft it edits is the shell's, per machine. */}
      {modal === "craze" && crazeMachine && (
        <CrazeCreateDialog
          key={crazeMachine}
          target={crazeMachine}
          // Through the one lookup (plan 026 §3.2): a dormant shed has no
          // status row until a create or its first craze `Ready` lists it, and
          // its `shed_craze` entry is what keeps the sheet from reading it as
          // "no craze" (and disabling Create) meanwhile.
          craze={crazeStatusFor(crazeMachine, rcHosts, rcShedCraze)}
          draft={drafts[crazeMachine] ?? EMPTY_DRAFT}
          onEdit={(patch) => updateDraft(crazeMachine, (d) => editCrazeDraft(d, patch))}
          onSubmit={() => void submitCraze(crazeMachine)}
          // Its Resume half (plan 032 §3.4): the state is this shell's, per
          // machine, like the draft.
          resume={resumes[crazeMachine] ?? EMPTY_RESUME}
          onSourceGen={(gen) => observeCrazeSource(crazeMachine, gen)}
          onChooseMode={(mode) => chooseCrazeMode(crazeMachine, mode)}
          onResume={(id) => void resumeSaved(crazeMachine, id)}
          onRetryList={() => void readSaved(crazeMachine)}
          onClose={() => setModal(null)}
        />
      )}
      {roostConsent && (
        <RoostConsentDialog
          preview={roostConsent.preview}
          onCancel={() => setRoostConsent(null)}
          onConfirm={confirmRoostBootstrap}
        />
      )}
      {toast && <Toast toast={toast} onDismiss={() => setToast(null)} />}
    </div>
  );
}
