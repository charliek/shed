/* shed desktop — the agent-lane transcript panel (plan 015 §3.4 C6, plan 017 §3.5).

   A machine row whose roost tab reported an agent server carries an `agent_lane`
   stamp, and that stamp is the whole capability signal: the card gets a
   "Transcript" affordance, and this is what it opens. A right-hand panel over
   whatever pane is showing — the transcript, what the session is blocked on, and
   what you can do about it (send, interject, cancel, answer).

   **It renders one panel for every adapter, and takes the differences from
   `capabilities` rather than from a branch on the kind.** Plan 015 shipped this
   with `capabilities` stored and never read, which was invisible while opencode
   was the only adapter: what it can do and what the panel offered happened to
   agree. gx does not agree — it interjects, and its permissions offer whatever
   the agent asked, not a fixed three — so the panel now reads the flags (the
   Interject toggle) and the approval's own `options` (the buttons). The kind
   itself appears exactly once, as a badge in the header, so a person can see
   which agent they are talking to.

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
import { ChevronDown, ChevronRight, ScrollText, Send, Square, X, Zap } from "lucide-react";
import { cn } from "@/lib/utils";
import { newestWins } from "@/lib/newest";
import { cardCls, KindBadge, StatusChip, type Tone } from "@/components/primitives";
import {
  LANE_EVENT, laneAnswer, laneApprovals, laneCancel, laneClose, laneFailure,
  laneMessages, laneOpen, laneSend, reportLane,
  type LaneAnswer, type LaneApproval, type LaneApprovalCard, type LaneEventEnvelope,
  type LaneMessage, type LaneOpened, type LaneOption, type LaneReport, type LaneRow,
  type LaneView,
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

/** The approval kinds whose buttons are the agent's OWN offered options —
 *  rendered generically, in the order offered, answering `{choice: "<id>"}`.
 *
 *  This used to be three fixed decisions with the adapter's labels borrowed over
 *  them, which worked only because opencode happens to offer exactly three. gx
 *  does not: a real permission there offers FIVE options, **two of them of kind
 *  `allow_once`** ("Yes, proceed" and "Yes, and don't ask again for anything"),
 *  so `{permission: "allow-once"}` is genuinely ambiguous and the adapter
 *  refuses it. The only thing that can name which button a person pressed is the
 *  button's own id.
 *
 *  Nothing visible changes for opencode: its three options carry these exact
 *  labels in this exact order, so the same three buttons render — they now post
 *  `{choice: "allow_once"}` instead of `{permission: "allow-once"}`, which the
 *  adapter resolves to the same option. The `{permission: …}` IPC form is
 *  untouched and still works for scripts.
 *
 *  `plan_approval` is here too: its two options are synthesized by the adapter
 *  (`approved`/`cancelled`) but are offered the same way and answer the same
 *  way, so there is no second renderer for it. */
const OPTION_KINDS = new Set(["permission", "plan_approval"]);

/** Is this option a refusal? Off `kind` — the ACP vocabulary — and never off
 *  `id`, which is opaque. opencode calls its refusal `reject`; a live gx offers
 *  two, `reject-once` and `reject-always-command`; a third agent will call its
 *  own something else again. Matching any one of those spellings mis-styles the
 *  others, and mis-styling here means a destructive button that does not look
 *  like one. `reject_once` and `reject_always` both start `reject`, which is the
 *  same prefix rule the contract's own `Reject` mapping uses. */
function isReject(o: LaneOption): boolean {
  return (o.kind ?? "").startsWith("reject");
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
  /** Does the next Send interrupt the turn in flight, or queue behind it?
   *
   *  Sticky across sends on purpose — a reader steering a long turn interjects
   *  repeatedly — but only ever ARMED, never acted on by itself: the mode is
   *  recomputed at send time against what the session is doing right now, so a
   *  toggle left on while the agent goes idle sends a queue rather than a
   *  refusal. */
  const [interject, setInterject] = useState(false);
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

    // Newest-wins across OVERLAPPING pulls. `schedule()` below coalesces the
    // callbacks that ask for a read; it does nothing about a read already in
    // flight when the next one starts, and two IPC round-trips can answer in
    // either order — so an earlier pull landing second used to overwrite a fresh
    // transcript with a stale one, on a panel that never polls and would
    // therefore stay wrong until the next unrelated frame.
    const reads = newestWins();
    const pull = () =>
      reads.run(
        // ONE round-trip pair, in parallel: the two reads are independent
        // projections of the same locked view, so there is nothing to order
        // BETWEEN them — only between one pull and the next.
        () => Promise.all([laneMessages(machine, sessionId), laneApprovals(machine, sessionId)]),
        ([v, a]) => {
          setView(v);
          setApprovals(a);
          setReadError(null);
        },
        (e) => setReadError(laneFailure(e).message),
      );
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
      reads.cancel();
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
  /** The capability half of the Interject affordance: is there a toggle at all?
   *
   *  `lane.open`'s `capabilities` were stored and never read until now — the
   *  panel offered whatever it felt like and let the adapter refuse. That is the
   *  wrong way round for an affordance: an adapter that cannot interject (every
   *  opencode lane) should not show a button whose only outcome is an error. */
  const canInterject = opened?.capabilities?.interject === true;
  /** …and the state half: armed only while the agent would accept one. */
  const interjecting = canInterject && working && interject;
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
      // `undefined` rather than `"queue"` for the ordinary case: queue is the
      // op's own default, and a client that always spelled the mode out would
      // make every adapter's default this component's business.
      await laneSend(machine, sessionId, text, interjecting ? "interject" : undefined);
      setPrompt("");
    });
  };

  const cancel = () => act1(() => laneCancel(machine, sessionId));

  const pickedFor = (a: LaneApproval): string[][] =>
    picks[a.id] ?? a.questions.map(() => []);
  const typedFor = (a: LaneApproval): string[] => typed[a.id] ?? a.questions.map(() => "");

  /** Submit a question form: one inner list of picked ids per question, and the
   *  free text BESIDE it in `custom_text` — never appended to the ids.
   *
   *  The panel used to smuggle the typed string into the vec-of-vecs as one
   *  more "option id", which left the adapter unable to tell a chosen label
   *  from something a human wrote — so gx, whose answer is a label map with a
   *  separate annotations channel, could not carry it at all. The smuggle now
   *  lives in the opencode adapter, where appending IS the agent's own shape.
   *
   *  `custom_text` is omitted entirely when nothing was typed, so the ordinary
   *  answer is the same payload this panel sent before the field existed. */
  const submitQuestion = (a: LaneApproval) => {
    const chosen = pickedFor(a);
    const free = typedFor(a);
    const answers = a.questions.map((_, i) => [...(chosen[i] ?? [])]);
    // `null` where nothing was typed AND on any question that does not take
    // free text — the backend refuses text aimed at one of those, and the box
    // is not rendered there either.
    const texts = a.questions.map((q, i) => (q.custom ? (free[i] ?? "").trim() || null : null));
    void answer(
      a.id,
      texts.some((t) => t !== null) ? { question: answers, custom_text: texts } : { question: answers },
    );
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
  const cards: LaneApprovalCard[] = approvals.map((a) => {
    // `kind` selects — never which list happens to be non-empty (§11.4). The
    // one addition gx forces: a kind that DOES take options can still arrive
    // with none (the `pending_interaction` placeholder, whose `method` and
    // `request` are both null), and rendering that as "a permission with no
    // buttons" is the exact thing a human cannot act on. It falls through to
    // the raw-request card, which at least offers Reject.
    const generic = OPTION_KINDS.has(a.kind) && a.options.length > 0;
    return {
      id: a.id,
      session_id: a.session_id,
      kind: a.kind,
      title: a.title,
      detail: a.detail ?? "",
      buttons: generic
        ? a.options.map((o) => o.label)
        : a.kind === "question"
          ? (oneClick(a) ? [] : ["Send answer"])
          : ["Reject"],
      options: generic
        ? a.options.map((o) => ({ id: o.id, label: o.label, kind: o.kind ?? null }))
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
    };
  });
  const report: LaneReport = {
    machine,
    session_id: sessionId,
    kind: opened?.capabilities?.kind ?? "",
    title: opened?.session.title ?? "",
    cwd: opened?.session.cwd ?? "",
    activity: view?.activity ?? "unknown",
    generation: view?.generation ?? 0,
    stale: view?.stale ?? null,
    rows,
    approvals: cards,
    can_cancel: working,
    interject: canInterject ? { on: interjecting, enabled: working } : null,
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
        {/* WHICH agent this transcript belongs to. Two adapters in one app made
            it worth saying out loud: the panels differ in what they offer
            (interject, the shape of a permission's buttons), and the kind is the
            reason. Empty until `lane.open` answers, and no placeholder for it —
            a badge that said "…" would be noise on every mount. */}
        {opened?.capabilities?.kind && <KindBadge kind={opened.capabilities.kind} />}
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
          {approvals.map((a, i) => {
            // BY INDEX. `cards` is `approvals.map(…)`, so `cards[i]` is this
            // approval's card by construction, while a `find` on the id would
            // take the first match — and the list can hold a descendant
            // sub-session's approval beside the root's (see the `sub-session`
            // line below), so ids are not this component's to assume unique.
            // The clicked option is posted off `a` either way, so the id lookup
            // was never a wrong-answer hazard; it could only have read one
            // card's BRANCH off another's, which is still a rendering nobody
            // could explain.
            const card = cards[i];
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
                {card && card.options.length > 0 ? (
                  // The agent's own menu, in the agent's own order, posting the
                  // agent's own ids. Keyed by id (they are unique within a
                  // request) rather than by index, so a card refreshed in place
                  // — which is exactly what gx's placeholder-then-request pair
                  // does — re-uses the buttons that did not change.
                  //
                  // Gated on the CARD's options and rendered from the
                  // APPROVAL's: the same list, but the card's went through the
                  // kind rule above (so the branch cannot disagree with what
                  // `lane.dump` reports) while the approval's still carry the
                  // `description` this button hangs its tooltip on.
                  <div className="mt-2.5 flex flex-wrap gap-2">
                    {a.options.map((o) => (
                      <button
                        key={o.id}
                        disabled={busy}
                        title={o.description ?? undefined}
                        onClick={() => void answer(a.id, { choice: o.id })}
                        className="hbtn rounded-[9px] px-3 py-2 text-[13px] font-semibold"
                        style={{
                          background: isReject(o) ? "var(--shed-deny-bg)" : "var(--shed-accent-subtle)",
                          color: isReject(o) ? "var(--shed-danger)" : "var(--shed-accent)",
                          border: "none",
                          opacity: busy ? 0.5 : 1,
                        }}
                      >
                        {o.label}
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
                  // An approval this build cannot render buttons FOR — an
                  // `mcp_elicitation` whose schema it cannot read, a kind it has
                  // never heard of, or gx's placeholder before the real request
                  // lands. Show the agent's own request rather than inventing
                  // buttons for it, and offer the one answer that is always
                  // meaningful: declining. Without it these cards were a dead
                  // end — the agent stays blocked and the panel offers nothing.
                  <>
                    <pre className="mt-2 max-h-32 overflow-auto rounded-[9px] bg-shed-inset p-2 font-mono text-[11px] text-shed-text-secondary">
                      {a.request_json}
                    </pre>
                    <div className="mt-2 flex flex-wrap gap-2">
                      <button
                        disabled={busy}
                        onClick={() => void answer(a.id, { reject: true })}
                        className="hbtn rounded-[9px] px-3 py-2 text-[13px] font-semibold"
                        style={{
                          background: "var(--shed-deny-bg)",
                          color: "var(--shed-danger)",
                          border: "none",
                          opacity: busy ? 0.5 : 1,
                        }}
                      >
                        Reject
                      </button>
                    </div>
                  </>
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
          {/* Present only when the adapter says it can interject, enabled only
              while the agent would accept one. Both halves are the capability
              read the panel used to skip: opencode advertises no `interject`,
              so its composer has no toggle at all, and gx refuses one outside a
              working turn — so an always-on button would be an affordance whose
              only outcome is `not_accepting` in the banner above. */}
          {canInterject && (
            <button
              onClick={() => setInterject(!interject)}
              disabled={busy || !working}
              title={
                working
                  ? interjecting
                    ? "Interject: the next prompt interrupts the turn in flight"
                    : "Queue: the next prompt waits for the turn in flight"
                  : "Interject — only while the agent is working"
              }
              aria-pressed={interjecting}
              className="hbtn inline-flex h-9 w-9 items-center justify-center rounded-[9px]"
              style={{
                background: interjecting ? "var(--shed-accent)" : "var(--shed-surface)",
                color: interjecting ? "var(--shed-accent-fg)" : "var(--shed-text-secondary)",
                border: "1px solid var(--shed-border)",
                opacity: busy || !working ? 0.5 : 1,
              }}
            >
              <Zap size={15} />
            </button>
          )}
          <button
            onClick={() => void send()}
            disabled={busy || !prompt.trim()}
            title={interjecting ? "Send (interject)" : "Send"}
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
