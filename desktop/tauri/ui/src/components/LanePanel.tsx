/* shed desktop — the agent-lane transcript panel (plan 015 §3.4, C6).

   A machine row whose roost tab reported an opencode server carries an
   `agent_lane` stamp, and that stamp is the whole capability signal: the card
   gets a "Transcript" affordance, and this is what it opens. A right-hand panel
   over whatever pane is showing — the transcript, what the session is blocked
   on, and the two things you can do about it (send, cancel).

   Three things about it are load-bearing rather than stylistic:

   * **It folds nothing.** `lane.rs` already stages a reconnect between `Reset`
     and `Ready` and swaps it in atomically, so the panel re-READS that view
     instead of applying frames itself. A `lane-event` is a nudge, not data —
     which is why what this renders and what `lane.messages` answers can never
     disagree, and why a reseed can never flash an empty transcript here.
   * **Reads are batched per animation frame.** A streaming turn pushes one
     event per folded part; without the batch a long answer would mean one IPC
     round-trip per token-ish part.
   * **It reports what it rendered** (`reportLane` → `lane.dump`), so a test can
     assert the panel, not just the backend behind it. `null` on unmount, because
     "no panel" is the question a caller is actually asking.

   `lane.open` on mount, `lane.close` on unmount, and no polling anywhere. */
import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronDown, ChevronRight, ScrollText, Send, Square, X } from "lucide-react";
import { cn } from "@/lib/utils";
import { cardCls, StatusChip, type Tone } from "@/components/primitives";
import {
  LANE_EVENT, laneAnswer, laneApprovals, laneCancel, laneClose, laneFailure,
  laneMessages, laneOpen, laneSend, reportLane,
  type LaneAnswer, type LaneApproval, type LaneApprovalCard, type LaneEventEnvelope,
  type LaneMessage, type LaneOpened, type LaneReport, type LaneRow, type LaneView,
} from "@/lib/bridge";

/** The batching fallback, in ms.
 *
 *  `requestAnimationFrame` is the batch the plan pins, and it is the right one
 *  while the window is on screen. It is also the one thing that stops entirely
 *  when the window is hidden to the tray (WebKitGTK stops servicing frames for
 *  an unmapped window) — so a panel batched on rAF ALONE would freeze at
 *  whatever the transcript was when the window went away and only catch up on
 *  the next repaint. The timer runs the same read, whichever fires first
 *  cancels the other, so the frame is the batch when there are frames and the
 *  timer is the batch when there are none. */
const FRAME_FALLBACK_MS = 40;

/** The activity badge: the same vocabulary and tones the session cards use
 *  (`rcActivityLabel`), so a lane's badge and its card's badge read as one
 *  thing. Unlike the card's, this one always says something — the panel has no
 *  lifecycle state to defer to, and a badge that vanished would read as "no
 *  longer running" rather than "not known". */
function activityBadge(activity: string): { tone: Tone; label: string } {
  switch (activity) {
    case "working":
      return { tone: "ok", label: "working" };
    case "needs_input":
      return { tone: "attention", label: "needs input" };
    case "needs_approval":
      return { tone: "attention", label: "needs approval" };
    case "idle":
      return { tone: "muted", label: "idle" };
    default:
      return { tone: "muted", label: "unknown" };
  }
}

/** The three fixed decisions a permission accepts, in render order.
 *
 *  The decision spellings are the IPC answer grammar's (`lane::parse_answer`,
 *  kebab) and are deliberately NOT the option ids the adapter mints
 *  (`allow_once`, snake) — so the decision is always ours, and only the LABEL is
 *  taken from the adapter's matching option when it sent one. An adapter that
 *  renames its buttons changes the words; it cannot change what gets posted. */
const PERMISSION_CHOICES: { decision: "allow-once" | "allow-always" | "reject"; label: string }[] = [
  { decision: "allow-once", label: "Allow once" },
  { decision: "allow-always", label: "Always" },
  { decision: "reject", label: "Reject" },
];

function permissionChoices(a: LaneApproval) {
  return PERMISSION_CHOICES.map((c) => ({
    decision: c.decision,
    label: a.options.find((o) => o.id.replace(/_/g, "-") === c.decision)?.label ?? c.label,
  }));
}

/** Can this question form be answered by ONE click?
 *
 *  One question, one choice, no free text — the overwhelmingly common shape, and
 *  the only one where a staged "Send answer" step would be pure ceremony.
 *  Anything else (several questions, `multiple`, or a `custom` free-text field)
 *  stages a selection and submits it explicitly, because a click can no longer
 *  mean "this is my whole answer". */
function oneClick(a: LaneApproval): boolean {
  const q = a.questions.length === 1 ? a.questions[0] : undefined;
  // `options.length` is part of it: a lone question with nothing to pick has no
  // click to BE the answer, and treating it as one-click would leave a card with
  // no affordance at all.
  return !!q && q.options.length > 0 && !q.multiple && !q.custom;
}

/** `name · detail` — a tool row's line, as the plan spells it. */
function toolLine(m: LaneMessage): string | null {
  if (!m.tool) return null;
  const line = [m.tool.name, m.tool.detail].filter(Boolean).join(" · ");
  return line || null;
}

/** role → the colour its label is written in. Deliberately quiet: the roles are
 *  scanned down the left edge, not read. */
function roleColor(role: string): string {
  if (role === "user") return "var(--shed-accent)";
  if (role === "tool") return "var(--shed-text-muted)";
  return "var(--shed-text-secondary)";
}

export function LanePanel({ machine, sessionId, onClose }: {
  machine: string;
  sessionId: string;
  onClose: () => void;
}) {
  const [opened, setOpened] = useState<LaneOpened | null>(null);
  const [view, setView] = useState<LaneView | null>(null);
  const [approvals, setApprovals] = useState<LaneApproval[]>([]);
  /** Two error slots, deliberately. A READ error is a property of the lane and
   *  clears itself the moment a read succeeds. An ACTION's refusal —
   *  `already_resolved`, `not_accepting` — is a property of what the reader just
   *  tried, and clearing it on the next frame would make it a flash nobody sees:
   *  this panel re-reads on every event, so "clear on success" would erase the
   *  answer to "why did nothing happen" within one frame. */
  const [readError, setReadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [prompt, setPrompt] = useState("");
  const [busy, setBusy] = useState(false);
  /** Which reasoning rows the reader opened, by seq. Reasoning is collapsed by
   *  default — it is the agent thinking out loud, and it buries the answer. */
  const [shown, setShown] = useState<Record<number, boolean>>({});
  /** Per-approval question state: the option ids picked for each question, and
   *  the free text typed beside them. Keyed by approval id so two open asks
   *  cannot share a selection. */
  const [picks, setPicks] = useState<Record<string, string[][]>>({});
  const [typed, setTyped] = useState<Record<string, string[]>>({});

  const list = useRef<HTMLDivElement | null>(null);
  /** Stick to the bottom only when the reader already IS at the bottom — a
   *  transcript that yanks itself down while you are reading history is worse
   *  than one that does not follow. */
  const atBottom = useRef(true);

  // One live subscription for as long as the panel is mounted. Everything —
  // the listener, the batch, the open — belongs to this one effect so the
  // cleanup is total: an unmount takes the lane down with it.
  useEffect(() => {
    let cancelled = false;
    let frame: number | null = null;
    let timer: number | null = null;
    const unlisten: Array<() => void> = [];

    const pull = async () => {
      try {
        // ONE round-trip pair, in parallel: the two reads are independent
        // projections of the same locked view, so there is nothing to order.
        const [v, a] = await Promise.all([
          laneMessages(machine, sessionId),
          laneApprovals(machine, sessionId),
        ]);
        if (cancelled) return;
        setView(v);
        setApprovals(a);
        setReadError(null);
      } catch (e) {
        if (!cancelled) setReadError(laneFailure(e).message);
      }
    };
    const run = () => {
      if (frame !== null) cancelAnimationFrame(frame);
      if (timer !== null) clearTimeout(timer);
      frame = null;
      timer = null;
      void pull();
    };
    const schedule = () => {
      if (frame !== null || timer !== null) return;
      frame = requestAnimationFrame(run);
      timer = window.setTimeout(run, FRAME_FALLBACK_MS);
    };

    void (async () => {
      // SUBSCRIBE FIRST, exactly as the adapter does under us: `lane.open`
      // returns as soon as the subscription is started, so a listener attached
      // after it would miss the `Reset` … `Ready` of the very seed it is waiting
      // for and the panel would sit empty until the next unrelated frame.
      const { listen } = await import("@tauri-apps/api/event");
      const un = await listen<LaneEventEnvelope>(LANE_EVENT, (e) => {
        if (e.payload?.machine === machine && e.payload?.session_id === sessionId) schedule();
      });
      if (cancelled) {
        un();
        return;
      }
      unlisten.push(un);
      try {
        const o = await laneOpen(machine, sessionId);
        if (cancelled) return;
        setOpened(o);
      } catch (e) {
        if (!cancelled) setReadError(laneFailure(e).message);
        return;
      }
      // The seed may have completed before the first frame-event was scheduled
      // (an already-open lane re-answers instantly), so read once outright.
      await pull();
    })();

    return () => {
      cancelled = true;
      if (frame !== null) cancelAnimationFrame(frame);
      if (timer !== null) clearTimeout(timer);
      unlisten.forEach((u) => u());
      void laneClose(machine, sessionId);
    };
  }, [machine, sessionId]);

  const messages = view?.messages ?? [];
  useEffect(() => {
    if (atBottom.current && list.current) list.current.scrollTop = list.current.scrollHeight;
  }, [messages.length]);

  const act = activityBadge(view?.activity ?? "unknown");
  const working = (view?.activity ?? "") === "working";
  // What the reader just tried outranks what the lane is doing: a refusal is
  // about them, a read failure is about the machine.
  const error = actionError ?? readError;

  /** Run one user action: clear the last refusal, and keep this one if it fails. */
  const act1 = useCallback(async (run: () => Promise<void>) => {
    setBusy(true);
    setActionError(null);
    try {
      await run();
    } catch (e) {
      setActionError(laneFailure(e).message);
    } finally {
      setBusy(false);
    }
  }, []);

  const answer = useCallback(
    (id: string, a: LaneAnswer) => act1(() => laneAnswer(machine, sessionId, id, a)),
    [act1, machine, sessionId],
  );

  const send = async () => {
    const text = prompt.trim();
    if (!text) return;
    await act1(async () => {
      await laneSend(machine, sessionId, text);
      setPrompt("");
    });
  };

  const cancel = () => act1(() => laneCancel(machine, sessionId));

  const pickedFor = (a: LaneApproval): string[][] =>
    picks[a.id] ?? a.questions.map(() => []);
  const typedFor = (a: LaneApproval): string[] => typed[a.id] ?? a.questions.map(() => "");

  /** Submit a question form: one inner list per question, its picked ids plus
   *  the free text when the reader typed some (the contract's vec-of-vecs). */
  const submitQuestion = (a: LaneApproval) => {
    const chosen = pickedFor(a);
    const free = typedFor(a);
    const answers = a.questions.map((q, i) => {
      const ids = chosen[i] ?? [];
      const text = q.custom ? (free[i] ?? "").trim() : "";
      return text ? [...ids, text] : [...ids];
    });
    void answer(a.id, { question: answers });
  };

  const pick = (a: LaneApproval, index: number, id: string) => {
    const q = a.questions[index];
    const current = pickedFor(a);
    const next = current.map((ids, i) => {
      if (i !== index) return ids;
      if (!q.multiple) return [id];
      return ids.includes(id) ? ids.filter((x) => x !== id) : [...ids, id];
    });
    setPicks({ ...picks, [a.id]: next });
    if (oneClick(a)) void answer(a.id, { question: [[id]] });
  };

  // ---- the rendered truth, and the report of it --------------------------
  // Built ONCE and both rendered and reported from, so `lane.dump` cannot
  // describe a panel different from the one on screen.
  const rows: LaneRow[] = messages.map((m) => ({
    seq: m.seq,
    role: m.role,
    type: m.type,
    text: m.text ?? "",
    tool: toolLine(m),
    muted: m.type === "status",
    collapsed: m.type === "reasoning" && !shown[m.seq],
  }));
  const cards: LaneApprovalCard[] = approvals.map((a) => ({
    id: a.id,
    session_id: a.session_id,
    kind: a.kind,
    title: a.title,
    detail: a.detail ?? "",
    // `kind` selects — never which list happens to be non-empty (§11.4).
    buttons:
      a.kind === "permission"
        ? permissionChoices(a).map((c) => c.label)
        : a.kind === "question" && !oneClick(a)
          ? ["Send answer"]
          : [],
    questions:
      a.kind === "question"
        ? a.questions.map((q) => ({
            header: q.header,
            question: q.question,
            options: q.options.map((o) => o.label),
            custom: q.custom,
          }))
        : [],
  }));
  const report: LaneReport = {
    machine,
    session_id: sessionId,
    title: opened?.session.title ?? "",
    cwd: opened?.session.cwd ?? "",
    activity: view?.activity ?? "unknown",
    generation: view?.generation ?? 0,
    stale: view?.stale ?? null,
    rows,
    approvals: cards,
    can_cancel: working,
    error,
  };
  // Compared BY VALUE, not by reference: the report is rebuilt every render, so
  // a reference dep would re-report on every keystroke in the prompt box.
  const reported = JSON.stringify(report);
  const latest = useRef(report);
  latest.current = report;
  useEffect(() => {
    reportLane(latest.current);
  }, [reported]);
  // Clearing is its own effect so it runs exactly once, at unmount: `lane.dump`
  // answering `null` is how a caller learns there is no panel, and a report left
  // behind would answer with a transcript nobody is looking at.
  useEffect(() => () => reportLane(null), []);

  return (
    // A flex SIBLING of the main column, not a fixed overlay: the panel takes
    // its own space and the panes reflow beside it, so the card you opened it
    // from — and its Transcript affordance — stay visible and clickable. (A
    // `position: fixed` overlay also leaves stale tiles under WebKitGTK when the
    // content behind it repaints, which the render gate's screenshots showed.)
    <aside
      data-lane={sessionId}
      className="flex h-full w-[460px] max-w-[50%] flex-none flex-col border-l border-shed-border bg-shed-bg"
      style={{ animation: "shed-in .18s ease" }}
    >
      <header className="flex flex-none items-start gap-3 border-b border-shed-border bg-shed-bg-sidebar px-4 py-3">
        <ScrollText size={18} className="mt-0.5 flex-none text-shed-text-secondary" />
        <div className="min-w-0 flex-1">
          <div className="truncate text-[15px] font-semibold text-shed-text">
            {opened?.session.title || "Transcript"}
          </div>
          <div className="mt-0.5 truncate font-mono text-[11.5px] text-shed-text-muted">
            {machine} · {sessionId}
          </div>
        </div>
        <StatusChip tone={act.tone} label={act.label} />
        <button
          onClick={onClose}
          title="Close transcript"
          className="hlink flex h-[30px] w-[30px] flex-none items-center justify-center rounded-lg text-shed-text-muted"
        >
          <X size={16} />
        </button>
      </header>

      {/* The `Down` posture: the last good generation stays on screen, and the
          banner says why it is not moving. Not an error dialog — a machine that
          went to sleep is normal. */}
      {view?.stale && (
        <div
          className="flex-none border-b border-shed-border px-4 py-2 font-mono text-[12px]"
          style={{ background: "var(--shed-warn-bg)", color: "var(--shed-warn-fg)" }}
        >
          not live · {view.stale} — showing the last known transcript
        </div>
      )}
      {error && (
        <div
          className="flex-none border-b border-shed-border px-4 py-2 font-mono text-[12px]"
          style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}
        >
          {error}
        </div>
      )}

      <div
        ref={list}
        onScroll={(e) => {
          const el = e.currentTarget;
          atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
        }}
        className="min-h-0 flex-1 overflow-y-auto"
      >
        {rows.length === 0 ? (
          <div className="px-4 py-8 text-center text-[13px] text-shed-text-muted">
            No messages yet.
          </div>
        ) : (
          rows.map((r) => (
            <div key={r.seq} className="border-t border-shed-border px-4 py-2.5 first:border-t-0">
              <div className="flex items-center gap-2">
                <span
                  className="font-mono text-[11px] font-semibold uppercase leading-none"
                  style={{ color: roleColor(r.role), letterSpacing: ".04em" }}
                >
                  {r.role}
                </span>
                {r.type !== "text" && (
                  <span className="font-mono text-[11px] leading-none text-shed-text-muted">{r.type}</span>
                )}
                {r.type === "reasoning" && (
                  <button
                    onClick={() => setShown({ ...shown, [r.seq]: r.collapsed })}
                    title={r.collapsed ? "Show reasoning" : "Hide reasoning"}
                    className="hlink inline-flex items-center rounded px-1 py-0.5 text-shed-text-muted"
                  >
                    {r.collapsed ? <ChevronRight size={13} /> : <ChevronDown size={13} />}
                  </button>
                )}
              </div>
              {r.tool && (
                <div className="mt-1 break-words font-mono text-[12px] text-shed-text-secondary">{r.tool}</div>
              )}
              {r.text && !r.collapsed && (
                <div
                  className={cn(
                    "mt-1 whitespace-pre-wrap break-words text-[13px] leading-relaxed",
                    r.muted ? "font-mono text-[12px] text-shed-text-muted" : "text-shed-text",
                  )}
                >
                  {r.text}
                </div>
              )}
            </div>
          ))
        )}
      </div>

      {approvals.length > 0 && (
        <div className="flex max-h-[45%] flex-none flex-col gap-2.5 overflow-y-auto border-t border-shed-border bg-shed-bg-sidebar px-4 py-3">
          {approvals.map((a) => {
            const card = cards.find((c) => c.id === a.id);
            const chosen = pickedFor(a);
            const free = typedFor(a);
            return (
              <div key={a.id} className={cn(cardCls, "px-3.5 py-3")}>
                <div className="text-[13.5px] font-semibold text-shed-text">{a.title}</div>
                {a.detail && (
                  <div className="mt-1 whitespace-pre-wrap break-words font-mono text-[12px] text-shed-text-secondary">
                    {a.detail}
                  </div>
                )}
                {/* A descendant's approval blocks the same agent, so it belongs
                    here — but it is not this session's, and saying so is the
                    difference between "the agent is blocked" and "your session
                    did this". */}
                {a.session_id !== sessionId && (
                  <div className="mt-1 font-mono text-[11px] text-shed-text-muted">
                    sub-session {a.session_id}
                  </div>
                )}
                {a.kind === "permission" ? (
                  <div className="mt-2.5 flex flex-wrap gap-2">
                    {permissionChoices(a).map((c) => (
                      <button
                        key={c.decision}
                        disabled={busy}
                        onClick={() => void answer(a.id, { permission: c.decision })}
                        className="hbtn rounded-[9px] px-3 py-2 text-[13px] font-semibold"
                        style={{
                          background:
                            c.decision === "reject" ? "var(--shed-deny-bg)" : "var(--shed-accent-subtle)",
                          color: c.decision === "reject" ? "var(--shed-danger)" : "var(--shed-accent)",
                          border: "none",
                          opacity: busy ? 0.5 : 1,
                        }}
                      >
                        {c.label}
                      </button>
                    ))}
                  </div>
                ) : a.kind === "question" ? (
                  <div className="mt-2 flex flex-col gap-2.5">
                    {a.questions.map((q, i) => (
                      <div key={`${a.id}/${i}`} className="flex flex-col gap-1.5">
                        {q.header && (
                          <div className="text-[12px] font-semibold text-shed-text-secondary">{q.header}</div>
                        )}
                        {q.question && (
                          <div className="text-[13px] leading-snug text-shed-text">{q.question}</div>
                        )}
                        <div className="flex flex-wrap gap-2">
                          {q.options.map((o) => {
                            const on = chosen[i]?.includes(o.id);
                            return (
                              <button
                                key={o.id}
                                disabled={busy}
                                title={o.description ?? undefined}
                                onClick={() => pick(a, i, o.id)}
                                className="hbtn rounded-[9px] px-3 py-2 text-[13px] font-semibold"
                                style={{
                                  background: on ? "var(--shed-accent)" : "var(--shed-accent-subtle)",
                                  color: on ? "var(--shed-accent-fg)" : "var(--shed-accent)",
                                  border: "none",
                                  opacity: busy ? 0.5 : 1,
                                }}
                              >
                                {o.label}
                              </button>
                            );
                          })}
                        </div>
                        {/* Only when the adapter said free text is accepted —
                            `custom` defaults to false precisely so a panel never
                            invites typing the agent will reject. */}
                        {q.custom && (
                          <input
                            value={free[i] ?? ""}
                            onChange={(e) => {
                              const next = a.questions.map((_, j) => (j === i ? e.target.value : free[j] ?? ""));
                              setTyped({ ...typed, [a.id]: next });
                            }}
                            placeholder="or type an answer"
                            className="w-full rounded-[9px] border border-shed-border bg-shed-inset px-3 py-2 font-mono text-[13px] text-shed-text outline-none focus:border-shed-accent"
                          />
                        )}
                      </div>
                    ))}
                    {card?.buttons.length ? (
                      <div className="flex justify-end">
                        <button
                          disabled={busy}
                          onClick={() => submitQuestion(a)}
                          className="hbtn rounded-[9px] px-3 py-2 text-[13px] font-semibold"
                          style={{
                            background: "var(--shed-accent)",
                            color: "var(--shed-accent-fg)",
                            border: "none",
                            opacity: busy ? 0.5 : 1,
                          }}
                        >
                          {card.buttons[0]}
                        </button>
                      </div>
                    ) : null}
                  </div>
                ) : (
                  // An approval kind this build cannot name: show the agent's own
                  // request rather than inventing buttons for it.
                  <pre className="mt-2 max-h-32 overflow-auto rounded-[9px] bg-shed-inset p-2 font-mono text-[11px] text-shed-text-secondary">
                    {a.request_json}
                  </pre>
                )}
              </div>
            );
          })}
        </div>
      )}

      <div className="flex flex-none items-end gap-2 border-t border-shed-border bg-shed-surface px-4 py-3">
        <textarea
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              void send();
            }
          }}
          rows={2}
          placeholder="Send a prompt…"
          className="min-w-0 flex-1 resize-none rounded-[9px] border border-shed-border bg-shed-inset px-3 py-2 text-[13px] text-shed-text outline-none focus:border-shed-accent"
        />
        <div className="flex flex-none flex-col gap-2">
          <button
            onClick={() => void send()}
            disabled={busy || !prompt.trim()}
            title="Send"
            className="hbtn inline-flex h-9 w-9 items-center justify-center rounded-[9px]"
            style={{
              background: "var(--shed-accent)",
              color: "var(--shed-accent-fg)",
              border: "none",
              opacity: busy || !prompt.trim() ? 0.5 : 1,
            }}
          >
            <Send size={15} />
          </button>
          {/* Enabled only while the session is Working — cancelling a session
              that is already waiting on you is a no-op the agent would refuse. */}
          <button
            onClick={() => void cancel()}
            disabled={busy || !working}
            title="Cancel the turn in flight"
            className="hbtn inline-flex h-9 w-9 items-center justify-center rounded-[9px]"
            style={{
              background: "var(--shed-deny-bg)",
              color: "var(--shed-danger)",
              border: "none",
              opacity: busy || !working ? 0.5 : 1,
            }}
          >
            <Square size={14} />
          </button>
        </div>
      </div>
    </aside>
  );
}
