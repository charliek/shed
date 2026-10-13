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
   agree. gx did not agree (plan 017) — it interjected, and its permissions
   offered whatever the agent asked, not a fixed three — so the panel reads the
   flags (the Interject toggle) and the approval's own `options` (the buttons).
   The kind itself appears exactly once, as a badge in the header, so a person
   can see which agent they are talking to.

   **The capabilities come from the VIEW, not from `lane.open`** (plan 025
   §3.2). They are per session and ride the lane's stream — a craze session's
   change with its incarnation — so `lane.open` answers the session row alone
   and the panel reads `capabilities` off the same `lane.messages` poll as
   everything else it renders. A copy taken at open would be stale by design.
   So, once a seed has swapped one in, is the session ROW the header reads
   (`lane.messages`' `session`): `lane.open`'s row is re-answered unchanged
   while the lane stays open, and the one the create sheet's transcript is
   opened on is the create's own, with no permission mode (live leg 1).

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

   `lane.open` on mount, `lane.close` on unmount, and no polling anywhere.

   **The lane's kind is part of its address** (plan 025 §3.6.4): the panel is
   opened with the row's `agent_lane.kind`, and every `lane.*` call carries it —
   a craze hostId and an opencode session id are different namespaces. For a
   craze session the panel also shows the session's permission posture (a
   sheet-created session runs `bypass` — "runs tools without asking") and,
   when its capabilities offer `stop`, a Stop behind an inline confirm: Stop
   ends the SESSION, not just this transcript.

   **The settings chip** (plan 025 §3.10): on a session whose streamed
   capabilities say `settings`, the header carries `<model name> · <effort
   value> · fast` from the current values; pressing it opens the settings
   sheet (`LaneSettings.tsx`) under the header. No chip, and no sheet even
   when asked for (`ui.show_lane_settings`), on a session that says no —
   hidden, never disabled. The panel counts the session's `Settings` frames
   (the `lane-event` nudges that carry one) and moves the count only with the
   read that shows them: a change whose answer was lost is "not confirmed"
   until that count moves.

   **The `/` menu** (plan 029 §3.5): on a session whose view carries a
   resolved `slash_menu`, a `/` draft opens a menu above the composer. The
   panel only RENDERS it and handles keys (`slashKeys.ts`); every rule is the
   shared core's, through the `lane_slash_*` commands (`slash.rs`, P5). Send
   goes through the same dispatch, so a typed `/help` is refused inline
   rather than sent as text. Each command runs on the lane's own verb — a rename, a mode (`lane.set`, its errors in the composer,
   P7), Stop behind its confirm, the model picker (the settings sheet), a
   prompt row as a prompt. No menu on the view → the composer is as it always
   was. */
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { ChevronDown, ChevronRight, OctagonX, ScrollText, Send, SlidersHorizontal, Square, X, Zap } from "lucide-react";
import { cn } from "@/lib/utils";
import { laneVerbs, sendFailureText } from "@/lib/laneVerbs";
import { laneHeader } from "@/lib/crazeRows";
import { newestWins } from "@/lib/newest";
import { NO_SETTINGS, settingsChip, settingsOffered } from "@/lib/laneSettings";
import { LaneSettingsSheet, useSettingChanges } from "@/components/LaneSettings";
import { SlashMenuPanel } from "@/components/SlashMenu";
import { cardCls, KindBadge, StatusChip, type Tone } from "@/components/primitives";
import {
  composerKey, dismissalAfter, highlightKey, isComposing, menuUp, moveHighlight, oneAtATime, owns, pressKey, revise,
  settleFor, type ComposerNote, type DraftRev, type Guard, type SlashOutcome,
} from "@/lib/slashKeys";
import {
  LANE_EVENT, laneAnswer, laneApprovals, laneCancel, laneClose, laneFailure,
  laneMessages, laneOpen, laneSend, laneSet, laneSetTitle, laneSlashAccept, laneSlashDispatch,
  laneSlashFilter, laneStop, reportLane, reportLaneComposer,
  type LaneAnswer, type LaneApproval, type LaneApprovalCard, type LaneEventEnvelope,
  type LaneMessage, type LaneOpened, type LaneOption, type LaneReport, type LaneRow,
  type LaneComposerReport, type LaneView, type SlashAcceptKey, type SlashDispatch, type SlashFilter,
  type SlashMenu,
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

/** One filter answer and the inputs it answered for — so a key that lands
 *  before the next answer can tell it is reading a stale one. */
type Filtered = { f: SlashFilter; menu: SlashMenu; draft: string; caret: number; activity: string };

/** What an Enter, a Send or a pick was pressed ON — the draft, its caret and
 *  its revision (`slashKeys.ts`' `DraftRev`), claimed before the first
 *  round-trip and carried through every one after it. */
type Claim = { draft: string; caret: number; rev: number };

/** Report `value` through `send` whenever it changes BY VALUE — it is rebuilt
 *  every render, so a reference dep would re-report on every keystroke — and
 *  `null` exactly once, at unmount: a dump answering `null` is how a caller
 *  learns there is no panel, and a report left behind would answer with a
 *  transcript nobody is looking at. */
function useReport<T>(value: T, send: (v: T | null) => void): void {
  const json = JSON.stringify(value);
  const latest = useRef(value);
  latest.current = value;
  useEffect(() => send(latest.current), [json]);
  useEffect(() => () => send(null), []);
}

/** role → the colour its label is written in. Deliberately quiet: the roles are
 *  scanned down the left edge, not read. */
function roleColor(role: string): string {
  if (role === "user") return "var(--shed-accent)";
  if (role === "tool") return "var(--shed-text-muted)";
  return "var(--shed-text-secondary)";
}

export function LanePanel({ machine, kind, sessionId, settingsOpen, onSettingsOpen, onClose }: {
  machine: string;
  /** The row's `agent_lane.kind` — half of the lane's address. */
  kind: string;
  sessionId: string;
  /** The settings sheet is ASKED for (the chip, or `ui.show_lane_settings`);
   *  it shows only while the session's capabilities say `settings`. */
  settingsOpen: boolean;
  onSettingsOpen: (open: boolean) => void;
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
  /** The draft's text and REVISION, moved synchronously with every change
   *  (`setDraft`) — what a keypress claims, and what its round-trips' answers
   *  are checked against (`owns`): an answer for a draft that has since
   *  changed never acts on, clears or overwrites the newer one. */
  const draftRev = useRef<DraftRev>({ text: "", rev: 0 });
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
  /** The Stop button's inline confirm is open, and what opened it: the
   *  header's button, or `/exit` — whose Keep keeps the draft and whose stop
   *  clears it (§3.5). Stop ends the SESSION, so it is never one click. */
  const [confirmStop, setConfirmStop] = useState<"header" | "slash" | null>(null);
  /** The draft revision a `/exit` confirm was opened on: its stop clears the
   *  draft only while that is still the composer's. */
  const stopRev = useRef(0);
  /** How many `Settings` frames the session has sent that a committed read
   *  reflects — what ends a lost change's "not confirmed". `arrived` counts
   *  them as the nudges come in; a read takes the count as it STARTS, and
   *  commits it with the view it read, so the mark never clears a frame
   *  before the value that replaces it is on screen. */
  const arrived = useRef(0);
  const [settingsSeen, setSettingsSeen] = useState(0);
  /** The view is HELD (`ui.hold_lane_view`, test mode only): reads go on, and
   *  nothing they return is committed until it is released — what a person
   *  sees in the moment between the adapter folding a change and this panel
   *  rendering it, held open so a harness can act inside it. */
  const held = useRef(false);
  const changes = useSettingChanges(machine, kind, sessionId, settingsSeen, view?.settings ?? NO_SETTINGS);

  // ---- the composer's `/` menu (plan 029 §3.5) ---------------------------
  /** The session's resolved menu, as the view last carried it — replaced
   *  only when it CHANGES (compared by value), so a streaming turn's reads do
   *  not re-filter on every frame. `null`: no menu. */
  const [menu, setMenu] = useState<SlashMenu | null>(null);
  const menuJson = useRef("null");
  /** The caret, in UTF-16 code units (`selectionStart`) — what the shared
   *  filter's open rule reads (P28). */
  const [caret, setCaret] = useState(0);
  /** …and the same caret, moved synchronously (`placeCaret`), for a claim
   *  made before the render that shows it. */
  const caretNow = useRef(0);
  const [filtered, setFiltered] = useState<Filtered | null>(null);
  /** The highlighted row's index, and the filter answer it is an index into
   *  (`highlightKey`: its token and rows). */
  const [hl, setHl] = useState({ key: "", index: 0 });
  /** The token the menu was dismissed for with Esc, until the token changes. */
  const [dismissed, setDismissed] = useState<string | null>(null);
  /** The composer's inline line: a refused command (the draft kept), or a
   *  rename that took with a warning. */
  const [note, setNote] = useState<ComposerNote | null>(null);
  const composer = useRef<HTMLTextAreaElement | null>(null);
  /** Where the caret goes once a programmatic draft (a completion, the type
   *  door) has rendered. */
  const nextCaret = useRef<number | null>(null);
  const filterReads = useRef(newestWins());

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
    const pull = () => {
      const upto = arrived.current;
      return reads.run(
        // ONE round-trip pair, in parallel: the two reads are independent
        // projections of the same locked view, so there is nothing to order
        // BETWEEN them — only between one pull and the next.
        () => Promise.all([laneMessages(machine, kind, sessionId), laneApprovals(machine, kind, sessionId)]),
        ([v, a]) => {
          if (held.current) return;
          const json = JSON.stringify(v.slash_menu ?? null);
          if (json !== menuJson.current) {
            menuJson.current = json;
            setMenu(v.slash_menu ?? null);
          }
          setView(v);
          setApprovals(a);
          setReadError(null);
          setSettingsSeen(upto);
        },
        (e) => setReadError(laneFailure(e).message),
      );
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
        if (e.payload?.machine === machine && e.payload?.kind === kind && e.payload?.session_id === sessionId) {
          if ((e.payload.event as { kind?: unknown } | null | undefined)?.kind === "settings") {
            arrived.current += 1;
          }
          schedule();
        }
      });
      if (cancelled) {
        un();
        return;
      }
      unlisten.push(un);
      // The view hold's door (test mode only; the backend refuses it otherwise).
      const unHold = await listen<{ hold?: unknown }>("hold-lane-view", (e) => {
        held.current = e.payload?.hold === true;
        if (!held.current) schedule();
      });
      if (cancelled) {
        unHold();
        return;
      }
      unlisten.push(unHold);
      try {
        const o = await laneOpen(machine, kind, sessionId);
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
      void laneClose(machine, kind, sessionId);
    };
  }, [machine, kind, sessionId]);

  const messages = view?.messages ?? [];
  useEffect(() => {
    if (atBottom.current && list.current) list.current.scrollTop = list.current.scrollHeight;
  }, [messages.length]);

  const act = activityBadge(view?.activity ?? "unknown");
  /** The capability half of the verb affordances: is there a button at all,
   *  and can it be pressed?
   *
   *  Read off the VIEW's capabilities — the live generation's, from the
   *  `lane.messages` poll — never off `lane.open`, which no longer carries any —
   *  through `laneVerbs`, the ONE rule both the buttons and `lane.dump` read: a
   *  verb the session's capabilities do not offer has no button (every opencode
   *  lane has no Interject; a session whose capabilities say `cancel: false`
   *  has no Cancel), and an offered verb is enabled only while it is Working. */
  const capabilities = view?.capabilities ?? null;
  const verbs = laneVerbs(capabilities, view?.activity ?? "");
  const canInterject = verbs.interject.shown;
  /** …and the state half: armed only while the agent would accept one. */
  const interjecting = verbs.interject.enabled && interject;
  // What the reader just tried outranks what the lane is doing: a refusal is
  // about them, a read failure is about the machine.
  const error = actionError ?? readError;

  /** Run one user action: clear the last refusal, and keep this one if it
   *  fails — said as `describe` words it (the failure's own message by
   *  default). */
  const act1 = useCallback(
    async (run: () => Promise<void>, describe: (f: { code: string; message: string }) => string = (f) => f.message) => {
      setBusy(true);
      setActionError(null);
      try {
        await run();
      } catch (e) {
        setActionError(describe(laneFailure(e)));
      } finally {
        setBusy(false);
      }
    },
    [],
  );

  const answer = useCallback(
    (id: string, a: LaneAnswer) => act1(() => laneAnswer(machine, kind, sessionId, id, a)),
    [act1, machine, kind, sessionId],
  );

  /** Every change to the draft's text goes through here: the text, and its
   *  revision moved with it (`revise`) — synchronously, so a keypress that
   *  lands next claims the new one. */
  const setDraft = (text: string) => {
    draftRev.current = revise(draftRev.current, text);
    setPrompt(text);
  };
  const placeCaret = (at: number) => {
    caretNow.current = at;
    setCaret(at);
  };
  /** Whether `rev` — what a keypress claimed — is still the draft's. */
  const ownsDraft = (rev: number) => owns(rev, draftRev.current.rev);

  /** Send `text` as a prompt — a draft that is not a command, or any draft on
   *  a session with no menu. The text is cleared only once the agent took it,
   *  and only while it is still the draft that was sent (`rev`): one typed
   *  while it was on its way stays. On a refusal it stays to be edited, and
   *  on a LOST answer (`outcome_unknown` — the prompt may or may not have
   *  started a turn, and it is never resent) it stays too, with the panel
   *  saying to check the transcript before sending again
   *  (`sendFailureText`): the person decides. */
  const sendText = async (text: string, rev: number) => {
    if (!text) return;
    await act1(async () => {
      await postPrompt(text);
      if (ownsDraft(rev)) {
        setDraft("");
        placeCaret(0);
      }
    }, sendFailureText);
  };
  /** `lane.send`, interjecting when the toggle says so. `undefined` rather
   *  than `"queue"` for the ordinary case: queue is the op's own default, and
   *  a client that always spelled the mode out would make every adapter's
   *  default this component's business. */
  const postPrompt = (text: string) =>
    laneSend(machine, kind, sessionId, text, live.current.interjecting ? "interject" : undefined);

  const activity = view?.activity ?? "unknown";
  // Re-filter whenever the draft, the caret, the menu or the activity moves —
  // newest answer wins (`newest.ts`): two invokes can answer in either order.
  useEffect(() => {
    if (!menu) {
      setFiltered(null);
      return;
    }
    const want = { menu, draft: prompt, caret, activity };
    void filterReads.current.run(
      () => laneSlashFilter(menu, prompt, caret, activity),
      (f) => setFiltered({ ...want, f }),
      () => setFiltered(null),
    );
  }, [menu, prompt, caret, activity]);
  useEffect(() => () => filterReads.current.cancel(), []);

  /** The last answer — for THIS menu only: one that answered a menu since
   *  replaced (or gone) is no menu on screen. */
  const filter = filtered?.menu === menu ? filtered.f : null;
  const token = filter?.token ?? "";
  const menuRows = filter?.rows ?? [];
  /** The menu is ON SCREEN: open, and not dismissed for this token. */
  const menuShown = menuUp(filter, dismissed);
  // An Esc dismissal ends when an ANSWER says the token changed (P9) — not
  // while a replaced catalog awaits its filter (`null`: no news).
  const answered = filter ? filter.token : null;
  useEffect(() => setDismissed((d) => dismissalAfter(d, answered)), [answered]);
  // The highlight goes back to the first row on every filter change — a new
  // token or new rows, a new catalog included (P9) — adjusted WHILE
  // rendering, so no render (and no report) shows the new filter under the
  // old highlight.
  const hlKey = useMemo(() => highlightKey(token, menuRows.map((r) => r.row.name)), [filter]);
  if (hl.key !== hlKey) setHl({ key: hlKey, index: 0 });
  const highlight = hl.key === hlKey ? hl.index : 0;
  /** What the latest render knows, for the async continuations and the doors
   *  (a door's listener is registered once). */
  const rendered = { prompt, caret, menu, activity, filtered, filter, highlight, dismissed, interjecting, busy,
                     confirming: confirmStop };
  const live = useRef(rendered);
  live.current = rendered;
  // A programmatic draft (a completion, the type door) puts the caret where
  // it was asked for once the textarea shows it.
  useLayoutEffect(() => {
    const at = nextCaret.current;
    const ta = composer.current;
    if (at === null || !ta) return;
    nextCaret.current = null;
    ta.focus();
    ta.setSelectionRange(at, at);
  });

  /** Set the draft as if typed — the caret at `at` (its end by default), and
   *  the composer's note gone, as a keystroke clears it. A new revision when
   *  the text changed (`setDraft`). */
  const putDraft = (text: string, at = text.length) => {
    setDraft(text);
    placeCaret(at);
    setNote(null);
    nextCaret.current = at;
  };

  /** What a key, Send or a pick is pressed ON — the draft, its caret and its
   *  revision, read synchronously — claimed at once, BEFORE any round-trip,
   *  and carried through all of them. */
  const claim = (): Claim => ({ draft: draftRev.current.text, caret: caretNow.current, rev: draftRev.current.rev });

  /** The filter for the draft a key was pressed on: the last answer when it
   *  answered these very inputs, else a fresh one — so a key pressed before
   *  the last keystroke's answer arrived decides by the draft it was pressed
   *  on. */
  const freshFilter = async (at: Claim): Promise<SlashFilter | null> => {
    const now = live.current;
    if (!now.menu) return null;
    const f = now.filtered;
    if (f && f.menu === now.menu && f.draft === at.draft && f.caret === at.caret && f.activity === now.activity) {
      return f.f;
    }
    try {
      return await laneSlashFilter(now.menu, at.draft, at.caret, now.activity);
    } catch {
      return null;
    }
  };

  /** A command's outcome, applied by §3.5's table to the draft it was sent
   *  from (`rev`, `settleFor`): what it has to say shows either way, but it
   *  clears the draft only while that is still the composer's — never a
   *  newer one typed while it was pending. */
  const settleSlash = (o: SlashOutcome, rev: number) => {
    const after = settleFor(o, ownsDraft(rev));
    if (after.clear) putDraft("");
    setNote(after.note);
    if (after.openSettings) onSettingsOpen(true);
  };

  /** Run one command's verb — its outcome, or its failure (craze's words; an
   *  answer lost is "may have applied"), settled on the draft it was sent
   *  from (`rev`). */
  const slashVerb = (run: () => Promise<SlashOutcome>, rev: number) =>
    act1(async () => {
      setNote(null);
      let outcome: SlashOutcome;
      try {
        outcome = await run();
      } catch (e) {
        const f = laneFailure(e);
        outcome = { kind: "failed", code: f.code, message: f.message };
      }
      settleSlash(outcome, rev);
    });

  /** Act on what the shared dispatch answered — Send's, Enter's, or a
   *  chosen row's — for the draft `at` it judged, which the caller has JUST
   *  checked is still the composer's: a draft that is not a command goes out
   *  as THAT text, and every outcome settles on THAT revision. */
  const runDispatch = async (d: SlashDispatch, at: Claim) => {
    switch (d.kind) {
      case "not_command":
        return sendText(at.draft.trim(), at.rev);
      case "refuse":
        return settleSlash({ kind: "refused", reason: d.reason }, at.rev);
      case "prompt":
        // The row's own spelling and the argument (E8) — it queues during a
        // turn like any prompt, and interjects when the toggle says so.
        return slashVerb(async () => {
          await postPrompt(d.text);
          return { kind: "done" };
        }, at.rev);
      case "run": {
        const action = d.action;
        switch (action.kind) {
          case "set_title":
            return slashVerb(async () => {
              const r = await laneSetTitle(machine, kind, sessionId, d.arg ?? "");
              return r?.warning ? { kind: "warning", text: r.warning } : { kind: "done" };
            }, at.rev);
          case "set_mode":
            // Through the composer's verb, its errors here — not the
            // settings sheet's row marks (P7). Re-selecting the current
            // mode still sends.
            return slashVerb(async () => {
              await laneSet(machine, kind, sessionId, action.change);
              return { kind: "done" };
            }, at.rev);
          case "stop":
            // The existing confirm: Stop ends the SESSION, never in one key.
            setNote(null);
            stopRev.current = at.rev;
            setConfirmStop("slash");
            return;
          case "model_picker":
            return settleSlash({ kind: "picker" }, at.rev);
          default:
            return;
        }
      }
    }
  };

  /** One key's operation at a time (`slashKeys.ts`' `oneAtATime`, E15) —
   *  Enter, Tab, Send and a pick alike, each holding it from the press until
   *  its whole filter → accept/dispatch → verb has settled. `busy` rises only
   *  once a verb starts, so without it a second Enter during the round-trips
   *  would send the same draft twice. */
  const keyGuard = useRef<Guard>({ held: false });

  /** Send the draft `at` claimed: through the shared dispatch when the
   *  session has a menu (the guard serves Send as it serves Enter, P5), as
   *  text when it has none. A dispatch answered once the draft has moved on
   *  was for a draft that no longer exists: dropped, silently, nothing sent. */
  const submitAt = async (at: Claim) => {
    const now = live.current;
    if (now.busy || !at.draft.trim()) return;
    if (!now.menu) return sendText(at.draft.trim(), at.rev);
    let d: SlashDispatch;
    try {
      d = await laneSlashDispatch(now.menu, at.draft, now.activity);
    } catch (e) {
      if (ownsDraft(at.rev)) setActionError(laneFailure(e).message);
      return;
    }
    if (!ownsDraft(at.rev)) return;
    await runDispatch(d, at);
  };

  /** Accept the row `name` with `key` on the draft `at` claimed — the shared
   *  rule decides whether that completes the draft, runs the row, or refuses
   *  it. An answer for a draft that has since moved on is dropped: no
   *  completion overwrites the newer text, and nothing runs. */
  const acceptAt = async (name: string, key: SlashAcceptKey, at: Claim) => {
    const now = live.current;
    if (!now.menu || now.busy) return;
    let a;
    try {
      a = await laneSlashAccept(now.menu, at.draft, name, key, now.activity);
    } catch (e) {
      if (ownsDraft(at.rev)) setActionError(laneFailure(e).message);
      return;
    }
    // No answer: the menu moved under the highlight (the re-filter shows it).
    if (!a || !ownsDraft(at.rev)) return;
    if (a.kind === "complete") {
      putDraft(a.draft, a.caret);
      return;
    }
    await runDispatch(a.dispatch, at);
  };

  /** Send, the button: the draft as it is now. */
  const submit = () => {
    const at = claim();
    return oneAtATime(keyGuard.current, () => submitAt(at));
  };

  /** Enter or Tab — decided by the draft it was pressed on (`at`) and THAT
   *  draft's filter (`slashKeys.ts`' `pressKey`): the row highlighted on
   *  screen when the fresh answer is the one on screen (`shown`), else its
   *  first; a plain Send when the fresh menu turned out closed. The guard is
   *  taken at the press, BEFORE the filter is asked for: a second key while
   *  this one's round-trips are out is dropped, never queued behind it. A
   *  key whose draft moved on while its filter was out is dropped too. */
  const keyedAccept = (key: "enter" | "tab", at: Claim, shown: { key: string; index: number }) =>
    pressKey(keyGuard.current, key, shown, {
      filter: async () => {
        const f = await freshFilter(at);
        return f && { open: f.open, token: f.token, names: f.rows.map((r) => r.row.name) };
      },
      owned: () => ownsDraft(at.rev),
      dismissed: () => live.current.dismissed,
      send: () => submitAt(at),
      accept: (name) => acceptAt(name, key, at),
    });

  /** The composer's keys (`slashKeys.ts`'s table, P9). */
  const onComposerKey = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    const action = composerKey({ key: e.key, shift: e.shiftKey, composing: isComposing(e.nativeEvent) }, menuShown);
    if (action === "none") return;
    e.preventDefault();
    switch (action) {
      case "dismiss":
        // One Esc closes one thing: the menu, and nothing an Esc handler
        // further out would close (`useEscClose` ignores a handled Esc too).
        e.stopPropagation();
        setDismissed(token);
        return;
      case "up":
      case "down":
        setHl((h) => ({ key: h.key, index: moveHighlight(h.index, action === "up" ? -1 : 1, menuRows.length) }));
        return;
      case "accept":
      case "send":
      case "complete":
        // The draft is claimed NOW, as pressed — before the round-trips. All
        // three read the menu FRESH: one about to open (a `/` typed a moment
        // ago, its answer not in yet) still takes this Enter, and with none a
        // send is a send.
        void keyedAccept(action === "complete" ? "tab" : "enter", claim(), { key: hlKey, index: highlight });
        return;
    }
  };

  /** A click on a menu row — and the `ui.pick_slash_row` door: only a row the
   *  menu on screen lists takes it, on the draft as it is when picked. */
  const pickRow = (name: string) => {
    const at = claim();
    const { filter: f, dismissed: d } = live.current;
    if (!menuUp(f, d) || !f?.rows.some((r) => r.row.name === name)) return;
    void oneAtATime(keyGuard.current, () => acceptAt(name, "pick", at));
  };

  // The composer's driver doors (`ui.type_lane_composer`,
  // `ui.key_lane_composer`, `ui.pick_slash_row`, `ui.answer_lane_stop`; test
  // mode only — the backend refuses them otherwise, plan 029 D13). Registered
  // once; they act through `doors`, refreshed every render.
  const doorActs = {
    putDraft,
    pickRow,
    // The Stop confirm's two buttons (`ui.answer_lane_stop`): only while it
    // is open, as only then are they on screen.
    answerStop: (stopIt: boolean) => {
      if (!live.current.confirming || live.current.busy) return;
      if (stopIt) void stop();
      else keep();
    },
  };
  const doors = useRef(doorActs);
  doors.current = doorActs;
  /** A menu row's click — stable, so the memoised menu skips the panel's
   *  other renders. */
  const onPickRow = useCallback((name: string) => doors.current.pickRow(name), []);
  useEffect(() => {
    if (typeof window === "undefined" || !("__TAURI_INTERNALS__" in window)) return;
    const uns: Array<() => void> = [];
    let cancelled = false;
    void import("@tauri-apps/api/event").then(async ({ listen }) => {
      uns.push(
        await listen<{ key?: unknown; shift?: unknown; composing?: unknown }>("key-lane-composer", (e) => {
          const ta = composer.current;
          const key = e.payload?.key;
          if (!ta || typeof key !== "string") return;
          const composing = e.payload?.composing === true;
          // A REAL keydown on the textarea (P27): the composer's own handler
          // runs, exactly as for a key a person pressed.
          const ev = new KeyboardEvent("keydown", {
            key,
            shiftKey: e.payload?.shift === true,
            isComposing: composing,
            bubbles: true,
            cancelable: true,
          });
          ta.dispatchEvent(ev);
          // A synthetic event carries no default action, so the browser's
          // own for an Enter nobody took — a newline at the caret — is
          // applied here (an IME's Enter commits its composition instead).
          if (key === "Enter" && !ev.defaultPrevented && !composing) {
            const text = ta.value;
            const at = ta.selectionStart ?? text.length;
            const end = ta.selectionEnd ?? at;
            doors.current.putDraft(`${text.slice(0, at)}\n${text.slice(end)}`, at + 1);
          }
        }),
      );
      uns.push(
        await listen<{ name?: unknown }>("pick-slash-row", (e) => {
          if (typeof e.payload?.name === "string") doors.current.pickRow(e.payload.name);
        }),
      );
      uns.push(
        await listen<{ stop?: unknown }>("answer-lane-stop", (e) => {
          if (typeof e.payload?.stop === "boolean") doors.current.answerStop(e.payload.stop);
        }),
      );
      // The type door LAST: a harness re-sends it until the draft shows
      // (a listener registered after mount can miss the first), and once it
      // has landed every door registered before it is live too.
      uns.push(
        await listen<{ text?: unknown }>("type-lane-composer", (e) => {
          if (typeof e.payload?.text === "string") doors.current.putDraft(e.payload.text);
        }),
      );
      if (cancelled) uns.forEach((u) => u());
    });
    return () => {
      cancelled = true;
      uns.forEach((u) => u());
    };
  }, []);

  const cancel = () => act1(() => laneCancel(machine, kind, sessionId));
  /** Stop the SESSION, from the confirm's own button. The lane ends when the
   *  session's close arrives (the banner says `ended`), and the row leaves.
   *  A confirm `/exit` opened settles the draft by the composer's table: the
   *  stop that goes through clears it, a refusal is said inline. */
  const stop = () => {
    const run = async () => {
      await laneStop(machine, kind, sessionId);
      setConfirmStop(null);
    };
    return confirmStop === "slash"
      ? slashVerb(async () => {
          await run();
          return { kind: "done" };
        }, stopRev.current)
      : act1(run);
  };
  /** Keep: the confirm closes and nothing is sent — and a `/exit` draft stays
   *  as it was (§3.5). */
  const keep = () => {
    setConfirmStop(null);
    if (confirmStop === "slash") settleSlash({ kind: "cancelled" }, stopRev.current);
  };
  /** Stop exists only when the session's streamed capabilities offer it — a
   *  TUI-hosted craze session, and every opencode one, has none. */
  const canStop = capabilities?.stop === true;
  /** The header — title, directory, permission line — from the LIVE session
   *  row (`lane.messages`' `session`), and from `lane.open`'s only until the
   *  first seed has swapped one in: the open's row is re-answered unchanged
   *  while the lane stays open, and for the transcript the create sheet opens
   *  at once it is the create's own row, with no permission mode
   *  (`laneHeader`). */
  const header = laneHeader(opened?.session, view?.session);
  const permission = header.permission;
  /** The settings chip and sheet exist only when the session's streamed
   *  capabilities say `settings` (plan 025 §3.10): hidden, never disabled —
   *  and that capability alone decides. */
  const offered = settingsOffered(capabilities);
  const settings = view?.settings ?? NO_SETTINGS;
  const chip = offered ? settingsChip(settings) : null;

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
    lane_kind: kind,
    kind: capabilities?.kind ?? "",
    permission,
    title: header.title,
    cwd: header.cwd,
    activity: view?.activity ?? "unknown",
    generation: view?.generation ?? 0,
    stale: view?.stale ?? null,
    ended: view?.ended ?? false,
    rows,
    approvals: cards,
    can_cancel: verbs.cancel.enabled,
    interject: canInterject ? { on: interjecting, enabled: verbs.interject.enabled } : null,
    stop: canStop ? { confirming: confirmStop !== null } : null,
    settings_chip: chip,
    error,
  };
  useReport(report, reportLane);
  // The composer apart (`lane.dump` answers both as one `lane`): typing
  // re-sends these few fields, never the transcript.
  const composerReport: LaneComposerReport = {
    draft: prompt,
    slash_menu: menu
      ? {
          open: menuShown,
          token,
          rows: menuShown
            ? menuRows.map((r, i) => ({ name: r.row.name, dimmed: r.dimmed, highlighted: i === highlight }))
            : [],
          draft: prompt,
          refusal: note?.tone === "refusal" ? note.text : null,
          warning: note?.tone === "warning" ? note.text : null,
        }
      : null,
  };
  useReport(composerReport, reportLaneComposer);

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
      <header className="flex-none border-b border-shed-border bg-shed-bg-sidebar px-4 py-3">
        <div className="flex items-start gap-3">
          <ScrollText size={18} className="mt-0.5 flex-none text-shed-text-secondary" />
          <div className="min-w-0 flex-1">
            <div className="truncate text-[15px] font-semibold text-shed-text">
              {header.title || "Transcript"}
            </div>
            <div className="mt-0.5 truncate font-mono text-[11.5px] text-shed-text-muted">
              {machine} · {sessionId}
            </div>
            {/* The session's permission posture, when it states one: a session
                the create sheet started runs `bypass`, and this is where that
                consequence is visible (plan 025 §3.6.5). */}
            {permission && (
              <div className="mt-0.5 truncate text-[12px] text-shed-text-secondary" data-permission>
                {permission}
              </div>
            )}
          </div>
          {/* WHICH agent this transcript belongs to. Two adapters in one app made
              it worth saying out loud: the panels differ in what they offer
              (interject, the shape of a permission's buttons), and the kind is the
              reason. Empty until the first seed's capabilities have swapped into
              the view, and no placeholder for it — a badge that said "…" would be
              noise on every mount. */}
          {capabilities?.kind && <KindBadge kind={capabilities.kind} />}
          <StatusChip tone={act.tone} label={act.label} />
          {/* Stop ends the SESSION — offered only when its capabilities say
              `stop`, and never in one click: the first press opens an inline
              confirm. */}
          {canStop && !confirmStop && (
            <button
              onClick={() => setConfirmStop("header")}
              disabled={busy || !!view?.ended}
              title="Stop the session (craze ends it; the transcript stays until you close it)"
              className="hbtn inline-flex flex-none items-center gap-1.5 rounded-[9px] px-2.5 py-1.5 text-[12.5px] font-semibold"
              style={{
                background: "var(--shed-deny-bg)",
                color: "var(--shed-danger)",
                border: "none",
                opacity: busy || view?.ended ? 0.5 : 1,
              }}
            >
              <OctagonX size={14} /> Stop
            </button>
          )}
          <button
            onClick={onClose}
            title="Close transcript"
            className="hlink flex h-[30px] w-[30px] flex-none items-center justify-center rounded-lg text-shed-text-muted"
          >
            <X size={16} />
          </button>
        </div>
        {/* The settings chip (plan 025 §3.10): the current model, effort
            and fast, one press from the sheet that changes them. */}
        {chip && (
          <button
            data-settings-chip
            onClick={() => onSettingsOpen(!settingsOpen)}
            title={settingsOpen ? "Close session settings" : "Session settings: model, options, mode"}
            aria-pressed={settingsOpen}
            className="hbtn ml-[30px] mt-2 inline-flex max-w-[calc(100%-30px)] items-center gap-1.5 rounded-full px-2.5 py-1 text-[12px] font-semibold"
            style={{
              background: settingsOpen ? "var(--shed-accent)" : "var(--shed-accent-subtle)",
              color: settingsOpen ? "var(--shed-accent-fg)" : "var(--shed-accent)",
              border: "none",
            }}
          >
            <SlidersHorizontal size={12} className="flex-none" />
            <span className="truncate">{chip}</span>
          </button>
        )}
      </header>

      {canStop && confirmStop !== null && (
        <div
          data-stop-confirm
          className="flex flex-none items-center gap-2 border-b border-shed-border px-4 py-2 text-[12.5px]"
          style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}
        >
          <span className="min-w-0 flex-1">Stop this session? The agent ends; the transcript stays.</span>
          <button
            onClick={() => void stop()}
            disabled={busy}
            className="hbtn rounded-[9px] px-2.5 py-1.5 text-[12.5px] font-semibold"
            style={{ background: "var(--shed-danger)", color: "var(--shed-accent-fg)", border: "none", opacity: busy ? 0.5 : 1 }}
          >
            Stop session
          </button>
          <button
            onClick={keep}
            disabled={busy}
            className="hbtn rounded-[9px] px-2.5 py-1.5 text-[12.5px] font-semibold"
            style={{ background: "var(--shed-surface)", color: "var(--shed-text-secondary)", border: "1px solid var(--shed-border)" }}
          >
            Keep
          </button>
        </div>
      )}

      {offered && settingsOpen && (
        <LaneSettingsSheet
          machine={machine}
          kind={kind}
          sessionId={sessionId}
          settings={settings}
          markOf={changes.markOf}
          press={changes.press}
          onClose={() => onSettingsOpen(false)}
        />
      )}

      {/* The `Stale`/`Down` posture: the last good generation stays on screen,
          and the banner says why it is not moving — and whether the lane is
          reconnecting on its own (stale) or over (ended). Not an error dialog — a
          machine that went to sleep is normal. */}
      {view?.stale && (
        <div
          className="flex-none border-b border-shed-border px-4 py-2 font-mono text-[12px]"
          style={{ background: "var(--shed-warn-bg)", color: "var(--shed-warn-fg)" }}
        >
          {view.ended ? "ended" : "not live"} · {view.stale} — showing the last known transcript
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

      {/* The `/` menu, above the composer — only on a session whose view
          carries one, only while the shared filter says it is open, and not
          once Esc dismissed it for this token. */}
      {menuShown && <SlashMenuPanel rows={menuRows} highlight={highlight} onPick={onPickRow} />}
      {/* What a command left to say: a refusal (the draft kept to fix) or a
          rename that took with a caveat. */}
      {note && (
        <div
          data-composer-note={note.tone}
          className="flex-none border-t border-shed-border px-4 py-2 font-mono text-[12px]"
          style={
            note.tone === "refusal"
              ? { background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }
              : { background: "var(--shed-warn-bg)", color: "var(--shed-warn-fg)" }
          }
        >
          {note.text}
        </div>
      )}

      <div className="flex flex-none items-end gap-2 border-t border-shed-border bg-shed-surface px-4 py-3">
        <textarea
          ref={composer}
          value={prompt}
          onChange={(e) => {
            setDraft(e.target.value);
            placeCaret(e.target.selectionStart ?? e.target.value.length);
            setNote(null);
          }}
          onSelect={(e) => placeCaret(e.currentTarget.selectionStart ?? 0)}
          onKeyDown={onComposerKey}
          rows={2}
          placeholder={menu ? "Send a prompt, or / for commands…" : "Send a prompt…"}
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
              disabled={busy || !verbs.interject.enabled}
              title={
                verbs.interject.enabled
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
                opacity: busy || !verbs.interject.enabled ? 0.5 : 1,
              }}
            >
              <Zap size={15} />
            </button>
          )}
          <button
            onClick={() => void submit()}
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
          {/* Present only when the session's capabilities offer `cancel`, and
              enabled only while it is Working — cancelling a session that is
              already waiting on you is a no-op the agent would refuse, and a
              session that cannot cancel at all gets no button whose only
              outcome is a refusal (`laneVerbs`). */}
          {verbs.cancel.shown && (
            <button
              onClick={() => void cancel()}
              disabled={busy || !verbs.cancel.enabled}
              title="Cancel the turn in flight"
              className="hbtn inline-flex h-9 w-9 items-center justify-center rounded-[9px]"
              style={{
                background: "var(--shed-deny-bg)",
                color: "var(--shed-danger)",
                border: "none",
                opacity: busy || !verbs.cancel.enabled ? 0.5 : 1,
              }}
            >
              <Square size={14} />
            </button>
          )}
        </div>
      </div>
    </aside>
  );
}
