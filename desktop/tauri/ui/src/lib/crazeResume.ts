/* The craze create sheet's Resume mode (plan 032 §3.4, C5) — pure rules: when
 * the sheet offers `New | Resume`, the saved list's lifecycle, one reopen at a
 * time and where each one's outcome lands (drop the row, keep it, offer
 * Retry), which machine INCARNATION the state belongs to (a host removed and
 * registered again under the same address is a new one: nothing the old one
 * left running carries over, and no late answer of its lands — told by every
 * status the shell reads, not only by a read the sheet makes), where a failed
 * create-options read offers its Retry, and how a saved row reads (its title,
 * its workspace with the home shortened to `~`, its age).
 *
 * Pure and dependency-free so node's own test runner pins them
 * (`test/crazeResume.test.mjs`), `crazeCreate.ts`'s arrangement: the shell
 * (`App.tsx`) holds a machine's resume state and runs its reopens, the sheet
 * (`CrazeCreateDialog.tsx`) renders it, and neither holds a rule of its own. */

/** One saved session as `craze.saved` carries it (`LaneSavedSession`). */
export type CrazeSavedSession = {
  /** What a reopen names — OPAQUE (craze's session id, or
   *  `<provider>:<providerSessionId>` for one recorded before craze ids). */
  id: string;
  provider: string;
  /** Absolute, as the machine recorded it; not checked to exist still. */
  workspace: string;
  title?: string | null;
  /** When it was last active, Unix ms (0: no time). */
  updated_unix_ms: number;
  created_unix_ms?: number | null;
};

/** `craze.saved`'s answer: the sessions newest first, `more` when craze left
 *  some out, the host's home where the app knows it, and the incarnation of
 *  the machine's craze source it came through (`source_gen`, the app's — not
 *  craze's: [`observeSource`]). */
export type CrazeSavedList = {
  sessions: CrazeSavedSession[];
  more: boolean;
  home?: string | null;
  source_gen?: number | null;
};

/** What a reopen's SUCCESS says that decides where it lands
 *  ([`reopenLanding`]) — `craze.reopen`'s answer, structurally. */
export type ReopenAnswer = {
  /** The session ended again before its row was listed: nothing to open. */
  ended?: boolean;
  /** The machine was removed — or removed and registered again, a new
   *  incarnation — while the reopen ran: its answer came through a source
   *  the machine no longer has, nothing was listed, and nothing can be opened
   *  on it here (C5 review). Not the row's own `stale`, which a reopen on a
   *  DORMANT machine answers too (its hub just started, its source not yet
   *  attached) and which opens all the same. */
  superseded?: boolean;
};

/** A refusal as both doors carry it — structurally `crazeCreate.ts`'s
 *  `CrazeRefusal` (kept here so this module stays dependency-free). */
export type ResumeRefusal = { code: string; message: string };

/** Which half of the sheet shows. */
export type CrazeSheetMode = "new" | "resume";

/** Where one saved row stands:
 *  - `resuming` — its reopen is running (minutes, at worst), as the shell's
 *    `attempt`: only THAT reopen's outcome lands on the row;
 *  - `refused` — a start failure (`start_failed`), a reopen whose answer was
 *    `superseded`, or any other definite refusal: said inline, the row kept,
 *    Resume again allowed;
 *  - `unavailable` — transient, a lost answer included ("Retry is safe": a
 *    reopen is idempotent): said inline, with Retry. */
export type SavedRowState =
  | { phase: "idle" }
  | { phase: "resuming"; attempt: number }
  | { phase: "refused"; refusal: ResumeRefusal }
  | { phase: "unavailable"; refusal: ResumeRefusal };

/** The saved list's read. `token` fences an answer to the read that asked:
 *  a sheet reopened while a read ran starts another, and only ITS answer
 *  lands. */
export type SavedLoad =
  | { phase: "unloaded" }
  | { phase: "loading"; token: number }
  | { phase: "failed"; refusal: ResumeRefusal }
  | { phase: "ready"; sessions: CrazeSavedSession[]; more: boolean; home: string | null };

/** A machine's Resume state — held by the shell, ABOVE the sheet, like a
 *  create's draft: dismissing the sheet keeps a reopen running (its session
 *  then simply appears as a row), and a re-open finds it. */
export type CrazeResume = {
  /** The machine INCARNATION it belongs to — the craze source generation an
   *  answer about it came through (`source_gen`: the machine's status, its
   *  create options, its saved list), `null` until one says. The shell holds
   *  this state by machine NAME, which a host removed and registered again
   *  keeps; this is what tells the two apart ([`observeSource`]). */
  gen: number | null;
  mode: CrazeSheetMode;
  load: SavedLoad;
  /** Each row's state by saved id; an absent id is idle. */
  rows: Record<string, SavedRowState>;
  /** The last row dropped because it cannot run here any more (`gone`):
   *  what it was called and craze's words — said where the row was. */
  gone: { id: string; title: string; message: string } | null;
};

export const EMPTY_RESUME: CrazeResume = { gen: null, mode: "new", load: { phase: "unloaded" }, rows: {}, gone: null };

/** What the sheet says under a list craze cut short. */
export const MORE_NOTE = "Showing the 50 most recent.";

/** A running reopen's button. It can take minutes: a provider's load replays
 *  the session, and craze is allowed 200 s per answer, sent again once. */
export const RESUMING_LABEL = "Resuming… (this can take a few minutes)";

/** What a saved session with no title is called. */
export const UNTITLED = "Untitled";

/** What a row says when its reopen's answer was `superseded`: the session
 *  runs, but on a machine since removed and added again, so it is not opened
 *  — and a Resume of it now joins it through the machine as it is. */
export const SUPERSEDED_NOTE =
  "craze resumed the session, but this machine was removed and added again meanwhile, so it was not opened — Resume again to open it";

/** **Whether the sheet offers `New | Resume`** (PC11): when the create
 *  options it just read say the hub can reopen — and while it already shows
 *  Resume (a reopen running when the sheet was reopened, its options still
 *  loading). */
export function resumeOffered(optionsReopen: boolean | null | undefined, r: CrazeResume): boolean {
  return optionsReopen === true || r.mode === "resume";
}

/** The saved id whose reopen is running, or `null`. One at a time. */
export function resumingId(r: CrazeResume): string | null {
  return Object.keys(r.rows).find((id) => r.rows[id].phase === "resuming") ?? null;
}

/** **Whether the `New | Resume` control is locked**: while a create is
 *  submitting or a reopen is running — each finishes in the half it started
 *  in. */
export function modeLocked(createSubmitting: boolean, r: CrazeResume): boolean {
  return createSubmitting || resumingId(r) !== null;
}

/** A sheet OPENING: nothing is cached (the create options' D8), so the list
 *  is read again on the first choice of Resume and the sheet opens on New —
 *  unless a reopen is still running, which the sheet then shows as it left
 *  it (while it is still this incarnation's: the shell applies the
 *  machine's status first, [`observeSource`]). The incarnation the state is
 *  bound to is kept either way — it is not the sheet's content but what
 *  fences an older incarnation's late word out of the fresh one. */
export function onSheetOpen(r: CrazeResume): CrazeResume {
  return resumingId(r) !== null ? r : { ...EMPTY_RESUME, gen: r.gen };
}

/** Whether generation `gen` is a NEWER incarnation than the state's — `gen`
 *  said, the state bound, and `gen` the larger: generations only grow (one
 *  counter for the app's life), so a later incarnation's is always the
 *  larger, and a smaller one is an incarnation already replaced. */
function newerSource(r: CrazeResume, gen: number | null | undefined): gen is number {
  return gen !== null && gen !== undefined && r.gen !== null && gen > r.gen;
}

/** **An answer about the machine came through its craze source of
 *  generation `gen`** (`source_gen` — the machine's status on every `rc.list`
 *  the shell reads, the create options the sheet reads on every open, the
 *  saved list): the state is bound to that incarnation. The first one binds
 *  it; the same one changes nothing; a NEWER one means the host was removed
 *  and registered again under the same name since this state was made, so
 *  nothing in it is this incarnation's — a reopen left running included —
 *  and it starts afresh, bound to `gen`, on New. That reopen's answer then
 *  finds no attempt to land on and is dropped whole ([`reopenLanding`]). An
 *  OLDER one is a late word from an incarnation already replaced (an options
 *  read that began before the status said the new one, say): it changes
 *  nothing. No `gen` (none said — no status, no source yet) changes
 *  nothing. */
export function observeSource(r: CrazeResume, gen: number | null | undefined): CrazeResume {
  if (gen === null || gen === undefined || r.gen === gen) return r;
  if (r.gen === null) return { ...r, gen };
  return newerSource(r, gen) ? { ...EMPTY_RESUME, gen } : r;
}

/** **Every status, applied to every machine's state** (C5 re-review): the
 *  shell calls this on each `rc.list` it reads — sheet open or not — with
 *  `genOf(machine)` the `source_gen` that machine's craze status names now
 *  (none for a machine not shown: removed, or no source yet), each through
 *  [`observeSource`]. So a host removed and registered again resets what its
 *  old incarnation left — a running reopen's lock included — as soon as the
 *  new one is SEEN, never waiting on a create-options read, which may fail.
 *  The same object back when nothing changed, so the shell re-renders only
 *  on a change. */
export function observeSources(
  all: Record<string, CrazeResume>,
  genOf: (machine: string) => number | null | undefined,
): Record<string, CrazeResume> {
  let next = all;
  for (const [machine, r] of Object.entries(all)) {
    const seen = observeSource(r, genOf(machine));
    if (seen === r) continue;
    if (next === all) next = { ...all };
    next[machine] = seen;
  }
  return next;
}

/** **Where a failed create-options read is said, with its Retry**: in the
 *  half that shows — the New half's provider field, or, in Resume, above the
 *  saved list (C5 re-review). A sheet opened onto Resume (a reopen left
 *  running) must still be able to read the options again: they say what
 *  offers `New | Resume` once that reopen settles, and the providers New
 *  needs. The Retry is never locked — a re-read touches neither half's
 *  action. `null` while the read has not failed. */
export function optionsRetryHalf(optionsFailed: boolean, r: CrazeResume): CrazeSheetMode | null {
  return optionsFailed ? r.mode : null;
}

/** Choose a half. The list is NOT read here: [`listNeeded`] says when the
 *  shell reads it — the first time Resume is chosen. */
export function chooseMode(r: CrazeResume, mode: CrazeSheetMode): CrazeResume {
  return r.mode === mode ? r : { ...r, mode };
}

/** Whether the shell should read the saved list now: Resume shows, and
 *  nothing has been read (or is being read) since the sheet opened. */
export function listNeeded(r: CrazeResume): boolean {
  return r.mode === "resume" && r.load.phase === "unloaded";
}

/** A read of the list starts (the first choice, or Retry after a failed
 *  read), fenced by `token`. A dropped row's note goes with the old list. */
export function beginList(r: CrazeResume, token: number): CrazeResume {
  return { ...r, load: { phase: "loading", token }, gone: null };
}

/** A read settles — applied only when it is still THE read (`token`), so an
 *  answer for a sheet since closed and reopened never lands on the new one.
 *  A fresh list forgets every row's settled state; a running reopen's stays
 *  — unless the list came through a NEWER incarnation than the state's
 *  ([`observeSource`]'s rule): then that reopen is the old one's, and goes
 *  too. The list binds an unbound state to its incarnation, and moves a
 *  bound one only forward (an older one's list still lands, as words about
 *  the machine; it names nothing the state is bound to). */
export function settleList(
  r: CrazeResume,
  token: number,
  outcome: { ok: true; list: CrazeSavedList } | { ok: false; refusal: ResumeRefusal },
): CrazeResume {
  if (r.load.phase !== "loading" || r.load.token !== token) return r;
  if (!outcome.ok) return { ...r, load: { phase: "failed", refusal: outcome.refusal } };
  const gen = outcome.list.source_gen ?? null;
  const moved = newerSource(r, gen);
  const running = moved ? null : resumingId(r);
  return {
    ...r,
    gen: r.gen === null || moved ? gen : r.gen,
    load: {
      phase: "ready",
      sessions: outcome.list.sessions ?? [],
      more: !!outcome.list.more,
      home: outcome.list.home ?? null,
    },
    rows: running !== null ? { [running]: r.rows[running] } : {},
  };
}

/** The listed saved session `id`, if the list holds it. */
export function savedSession(r: CrazeResume, id: string): CrazeSavedSession | undefined {
  return r.load.phase === "ready" ? r.load.sessions.find((s) => s.id === id) : undefined;
}

/** A row's state (an absent one is idle). */
export function rowState(r: CrazeResume, id: string): SavedRowState {
  return r.rows[id] ?? { phase: "idle" };
}

/** **Whether `id`'s button works**: Resume shows, the list holds it, and no
 *  reopen runs — one at a time, so every other button is disabled while one
 *  does (its own reads Resuming…). */
export function canResume(r: CrazeResume, id: string): boolean {
  return r.mode === "resume" && savedSession(r, id) !== undefined && resumingId(r) === null;
}

/** A reopen starts as the shell's `attempt` (a number no other reopen
 *  carries) — refused (unchanged) by [`canResume`]'s rule. */
export function beginResume(r: CrazeResume, id: string, attempt: number): CrazeResume {
  if (!canResume(r, id)) return r;
  return { ...r, rows: { ...r.rows, [id]: { phase: "resuming", attempt } }, gone: null };
}

/** Whether `attempt` is still the reopen running on `id` in `r`. */
function stillRunning(r: CrazeResume, id: string, attempt: number): boolean {
  const s = r.rows[id];
  return s !== undefined && s.phase === "resuming" && s.attempt === attempt;
}

/** Where a reopen's SUCCESS lands — the shell's effects follow it:
 *  - `dropped` — `attempt` is not the reopen running on `id` any more: the
 *    state it began in was forgotten for a NEWER incarnation of the machine
 *    ([`observeSource`]), so this answer is the old one's. No state change,
 *    no navigation, nothing said;
 *  - `superseded` — the backend says the machine was removed (or removed and
 *    registered again) while it ran: NOT openable — said on the row
 *    ([`SUPERSEDED_NOTE`]), the sheet left where it is;
 *  - `ended` — the session ended again before its row was listed: said, and
 *    nothing opened;
 *  - `open` — the sheet (when it is still the one on screen) closes onto the
 *    session's transcript. */
export type ReopenLanding = "dropped" | "superseded" | "ended" | "open";

export function reopenLanding(r: CrazeResume, id: string, attempt: number, answer: ReopenAnswer): ReopenLanding {
  if (!stillRunning(r, id, attempt)) return "dropped";
  if (answer.superseded) return "superseded";
  return answer.ended ? "ended" : "open";
}

/** A reopen settles, by its code — never by its words — and only while
 *  `attempt` is still the one running on `id` (else nothing changes:
 *  [`reopenLanding`]'s `dropped`):
 *  - **success** — the session runs now, so it is no longer saved: its row
 *    leaves the list (the sheet closes onto its transcript, the shell's
 *    part) — unless the answer was `superseded`, which keeps the row and says
 *    so ([`SUPERSEDED_NOTE`]): Resume again joins the session through the
 *    machine as it is now;
 *  - **`gone`** — it cannot run here any more (no such id, its directory
 *    gone): the row is DROPPED and craze's words said where it was;
 *  - **`unavailable`** — transient, a lost answer included: said inline,
 *    with Retry;
 *  - **anything else** (`start_failed`, a contract error, the app quitting):
 *    said inline, the row KEPT. */
export function settleResume(
  r: CrazeResume,
  id: string,
  attempt: number,
  outcome: { ok: true; answer: ReopenAnswer } | { ok: false; refusal: ResumeRefusal },
): CrazeResume {
  if (!stillRunning(r, id, attempt)) return r;
  const { [id]: _settled, ...rows } = r.rows;
  const drop = (load: SavedLoad): SavedLoad =>
    load.phase === "ready" ? { ...load, sessions: load.sessions.filter((s) => s.id !== id) } : load;
  if (outcome.ok) {
    if (reopenLanding(r, id, attempt, outcome.answer) === "superseded") {
      const refusal = { code: "superseded", message: SUPERSEDED_NOTE };
      return { ...r, rows: { ...rows, [id]: { phase: "refused", refusal } } };
    }
    return { ...r, load: drop(r.load), rows };
  }
  const { refusal } = outcome;
  if (refusal.code === "gone") {
    const s = savedSession(r, id);
    return {
      ...r,
      load: drop(r.load),
      rows,
      gone: { id, title: s ? savedTitle(s) : id, message: refusal.message },
    };
  }
  const phase = refusal.code === "unavailable" ? "unavailable" : "refused";
  return { ...r, rows: { ...rows, [id]: { phase, refusal } } };
}

/** A row's button label: Resume; Resuming… while its reopen runs; Retry
 *  after `unavailable`. */
export function resumeLabel(state: SavedRowState): string {
  if (state.phase === "resuming") return RESUMING_LABEL;
  if (state.phase === "unavailable") return "Retry";
  return "Resume";
}

/** Where a row's refusal is said: a start failure's cause verbatim
 *  (monospace, as the create sheet says one), anything else as said —
 *  `unavailable` adding that Retry is how to try again. `null` for none. */
export function savedRowError(state: SavedRowState): { where: "cause" | "general"; text: string } | null {
  if (state.phase === "refused") {
    return { where: state.refusal.code === "start_failed" ? "cause" : "general", text: state.refusal.message };
  }
  if (state.phase === "unavailable") return { where: "general", text: `${state.refusal.message} — Retry` };
  return null;
}

/** A saved session's title, or "Untitled". */
export function savedTitle(s: Pick<CrazeSavedSession, "title">): string {
  const t = s.title?.trim();
  return t ? t : UNTITLED;
}

/** **`path` with `home` shortened to `~`** — only a home the app KNOWS
 *  (`craze.saved`'s `home`: this machine's own, a shed's `/home/shed`; none
 *  for a machine reached over ssh), never one guessed from the path, so
 *  another user's home is never shown as yours. A path merely beginning with
 *  the home's letters (`/home/al` vs `/home/alice`) is not under it. */
export function shortenHome(path: string, home: string | null | undefined): string {
  const h = (home ?? "").replace(/\/+$/, "");
  if (!h.startsWith("/")) return path;
  if (path === h) return "~";
  return path.startsWith(`${h}/`) ? `~${path.slice(h.length)}` : path;
}

/** **When a saved session was last active, relative to `now`** (both Unix
 *  ms): "just now", "N min ago", "N h ago", "yesterday", "N days ago", then
 *  the date (UTC). A time in the future — a clock ahead of this one — is
 *  "just now"; no time at all (0) is "". */
export function relativeAge(ms: number, now: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  const secs = Math.max(0, Math.floor((now - ms) / 1000));
  if (secs < 60) return "just now";
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins} min ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours} h ago`;
  const days = Math.floor(hours / 24);
  if (days === 1) return "yesterday";
  if (days < 30) return `${days} days ago`;
  return new Date(ms).toISOString().slice(0, 10);
}
