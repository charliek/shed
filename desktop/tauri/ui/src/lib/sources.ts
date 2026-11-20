/* The Preferences window's credential-source rows (plan 035 §3.4): how a read, a
 * move and its read-back are reconciled into what each server's row shows.
 *
 * Pure and dependency-free, in the `report.ts` / `laneSettings.ts` shape, so node's
 * own test runner pins it (`test/sources.test.mjs`): `bridge.ts` re-exports the
 * types, `PreferencesWindow.tsx` holds the state and calls these.
 *
 * **Every application is per server, and ordered.** Each read is given a sequence
 * number when it is ISSUED, and each move's answer one when it ARRIVES; a server's
 * row takes a read only if that read is newer than what the row shows. One read
 * answers for every server, so an older read that resolves last (one server's
 * move read-back overtaken by another's) can no longer put back a row a newer read
 * or a confirmed move replaced — it still lands for the servers where it IS newest.
 *
 * **A move owns its row while it is in flight.** The row flips to this machine at
 * once (optimistic); reads skip it until the move answers. A confirmed move shows
 * the server's own answer (the POST returns the document after the move); a refused
 * move goes back to the row it replaced. A read-back after a confirmed move that
 * cannot say anything for that row (the call failed, or the row is an error or
 * busy) leaves the confirmed row in place and asks for one later read.
 *
 * **Busy is not "no source".** A row the server could not read right now (`503
 * SOURCE_BUSY`, still so after the core's bounded retries) keeps the source it
 * last showed, marked busy (and no longer unreachable: the server answered), and asks
 * for one later read.
 *
 * **Unreachable is not "unsupported".** A server that could not be reached or read (a
 * restart, the network) keeps the row it last showed, marked unreachable, and asks for
 * a few later reads with growing spacing ([UNREACHABLE_RETRY_MS]); then it stops until
 * the window is shown, focused or reloaded again ([restartRetries]). With no row known
 * yet there is nothing to keep, so none is shown, and the reads are still asked for.
 *
 * **A move's read-back can come too early.** A confirmed move whose read-back shows
 * this machine "not connected" (its broker has not connected yet) asks for one later
 * read, so the row does not stay stale.
 *
 * **One read at a time.** The window's reader ([createSourceReader]) never has two
 * reads in flight: a read asked for while one is out is folded into exactly one more
 * read after it (with every move's `hold` asked for meanwhile), and a pending later
 * read is dropped whenever a read starts. */

/** A machine as a source row shows it. The name came from ANOTHER machine by way of
 *  the server: render it only as a text node, never into HTML, a title or a tooltip. */
export type SourceMachine = { id: string; name: string };

/** One configured server's credential source (`shed_app::HostSource`). */
export type SourceRow = {
  host: string;
  /** False for a server older than the feature (no row is shown for it). */
  supported: boolean;
  /** The machine that answers credential requests for you on this server. */
  source: SourceMachine | null;
  this_machine: boolean;
  connected: boolean;
  /** The server was busy: the source is unknown for now, not absent. */
  busy: boolean;
  /** The server could not be reached or read: everything is unknown for now (not
   *  "unsupported"). Absent from an older backend. */
  unreachable?: boolean;
  error: string | null;
};

/** `source_status`: every server's row plus this machine's identity (or why none). */
export type SourceStatus = {
  rows: SourceRow[];
  this_machine: SourceMachine | null;
  this_machine_error: string | null;
};

/** What the window shows, and what it needs to keep reads in order. */
export type SourcesState = {
  /** The hosts, in the order the newest read listed them. */
  order: string[];
  orderSeq: number;
  /** Each host's row as shown. */
  rows: Record<string, SourceRow>;
  /** The sequence number of what each host's row shows. */
  applied: Record<string, number>;
  thisMachine: SourceMachine | null;
  thisMachineError: string | null;
  meSeq: number;
  /** Hosts with a move in flight → the row the move replaced (its last confirmed
   *  state), restored if the move is refused. */
  moving: Record<string, SourceRow>;
  /** A refused move's words, per host, until that host's next attempt. */
  errors: Record<string, string>;
  /** Reads in a row that found a server unreachable, since the last show, focus or
   *  reload; past [UNREACHABLE_RETRY_MS]'s length no further read is asked for. */
  unreachableTries: number;
};

export const EMPTY_SOURCES: SourcesState = {
  order: [],
  orderSeq: 0,
  rows: {},
  applied: {},
  thisMachine: null,
  thisMachineError: null,
  meSeq: 0,
  moving: {},
  errors: {},
  unreachableTries: 0,
};

/** How long a busy row, a confirmed move whose read-back said nothing, or one that
 *  read "not connected", waits before its one later read. */
export const SOURCE_RETRY_MS = 3000;
/** The spacing of the later reads after a read that found a server unreachable: one
 *  per entry, growing, then none until the window is shown, focused or reloaded. */
export const UNREACHABLE_RETRY_MS: readonly number[] = [2000, 4000, 8000, 16000];

/** Apply one read, issued as `seq`. `hold` names the host (or hosts) whose move was
 *  just confirmed: for it, a read that cannot say anything (no answer, an error row, a
 *  busy or unreachable row) is NOT applied over the confirmed row. `retry` asks the
 *  caller for one later read, `retryMs` from now: a row was busy, `hold` got no usable
 *  answer or read "not connected", or a server was unreachable (a bounded series of
 *  reads with growing spacing). A failed read (`undefined`) changes nothing else.
 *
 *  **Only the newest read keeps the retry accounts.** A read that is not the newest
 *  issued (`newest` false: another was issued after it) or that is older than one
 *  already applied still lands where it is newest, row by row, but asks for no later
 *  read and leaves the unreachable count alone: a late answer cannot spend a series
 *  the newer read restarted, nor start a timer of its own. */
export function applyRead(
  state: SourcesState,
  seq: number,
  status: SourceStatus | undefined,
  hold?: string | readonly string[],
  newest: boolean = true,
): { state: SourcesState; retry: boolean; retryMs: number } {
  const accounts = newest && seq > state.orderSeq;
  const holds: readonly string[] = hold === undefined ? [] : typeof hold === "string" ? [hold] : hold;
  if (!status) {
    return { state, retry: accounts && holds.length > 0, retryMs: SOURCE_RETRY_MS };
  }
  let retry = false;
  let unreachable = false;
  const next: SourcesState = {
    ...state,
    rows: { ...state.rows },
    applied: { ...state.applied },
  };
  if (seq > state.meSeq) {
    next.thisMachine = status.this_machine;
    next.thisMachineError = status.this_machine_error;
    next.meSeq = seq;
  }
  if (seq > state.orderSeq) {
    next.order = status.rows.map((r) => r.host);
    next.orderSeq = seq;
  }
  if (holds.some((h) => !status.rows.some((r) => r.host === h))) retry = true;
  for (const r of status.rows) {
    if (r.unreachable) unreachable = true;
    if (state.moving[r.host]) continue; // the move in flight settles this row
    if (seq <= (state.applied[r.host] ?? 0)) continue; // older than what is shown
    const held = holds.includes(r.host);
    if (held && (r.error || r.busy || r.unreachable)) {
      retry = true; // keep the confirmed move; read again later
      continue;
    }
    if (r.unreachable) {
      // Unknown, not unsupported: keep what was last shown, noted. Nothing known yet
      // means nothing to show.
      const prev = state.rows[r.host];
      if (!prev) continue;
      next.rows[r.host] = { ...prev, unreachable: true, busy: false };
    } else if (r.busy) {
      // The server answered (busy): it is reachable again, whatever was shown before.
      const prev = state.rows[r.host];
      next.rows[r.host] = prev ? { ...prev, busy: true, unreachable: false } : r;
      retry = true;
    } else {
      next.rows[r.host] = r;
      // The move's read-back came before its broker connected: look once more.
      if (held && r.this_machine && !r.connected) retry = true;
    }
    next.applied[r.host] = seq;
  }
  let retryMs = SOURCE_RETRY_MS;
  if (!accounts) {
    // Rows applied where this read is newest; the retry accounts are the newest read's.
    return { state: next, retry: false, retryMs };
  }
  if (unreachable) {
    const tries = state.unreachableTries + 1;
    next.unreachableTries = tries;
    if (tries <= UNREACHABLE_RETRY_MS.length) {
      const wait = UNREACHABLE_RETRY_MS[tries - 1];
      retryMs = retry ? Math.min(retryMs, wait) : wait;
      retry = true;
    }
  } else {
    next.unreachableTries = 0;
  }
  return { state: next, retry, retryMs };
}

/** The window was shown, focused or reloaded: an unreachable server gets a fresh
 *  series of later reads. */
export function restartRetries(state: SourcesState): SourcesState {
  return state.unreachableTries === 0 ? state : { ...state, unreachableTries: 0 };
}

/** Whether `host`'s "Use this machine" can be pressed: an identity to send, no move
 *  of its own in flight, and this machine not already the source there — the one
 *  rule behind the button's presence, its `disabled`, and the reported `can_use`. */
export function canUse(state: SourcesState, host: string): boolean {
  return !!state.thisMachine && !state.moving[host] && !state.rows[host]?.this_machine;
}

/** A press: `null` when it cannot start ([canUse]), else the state with the row
 *  flipped to this machine and the row it replaced kept for a refusal. */
export function beginMove(state: SourcesState, host: string): SourcesState | null {
  const prev = state.rows[host];
  if (!prev || !canUse(state, host)) return null;
  const errors = { ...state.errors };
  delete errors[host];
  return {
    ...state,
    rows: { ...state.rows, [host]: { ...prev, source: state.thisMachine, this_machine: true, busy: false, unreachable: false, error: null } },
    moving: { ...state.moving, [host]: prev },
    errors,
  };
}

/** The server confirmed the move and answered `row` (its document after the move);
 *  `seq` is allocated as the answer arrives, so every read issued before it is
 *  older. */
export function moveConfirmed(state: SourcesState, host: string, seq: number, row: SourceRow): SourcesState {
  const moving = { ...state.moving };
  delete moving[host];
  return {
    ...state,
    rows: { ...state.rows, [host]: row },
    applied: { ...state.applied, [host]: Math.max(seq, state.applied[host] ?? 0) },
    moving,
  };
}

/** The move was refused: the row goes back to its last confirmed state, and the
 *  refusal is kept under it. */
export function moveFailed(state: SourcesState, host: string, message: string): SourcesState {
  const moving = { ...state.moving };
  const prev = moving[host];
  delete moving[host];
  return {
    ...state,
    rows: prev ? { ...state.rows, [host]: prev } : state.rows,
    moving,
    errors: { ...state.errors, [host]: message },
  };
}

/** The rows to show, in order: supporting servers only (an older server shows
 *  nothing). */
export function shownRows(state: SourcesState): SourceRow[] {
  return state.order.map((h) => state.rows[h]).filter((r): r is SourceRow => !!r && r.supported);
}

/** The least time between two reads the window starts on its own when it is shown
 *  or focused: focus churn (alt-tabbing, a window manager that refocuses) costs one
 *  read per server per this long, at most. */
export const SOURCE_REFRESH_MIN_MS = 5000;

/** How long to wait before a shown/focused refresh may read: 0 now, else the rest
 *  of [SOURCE_REFRESH_MIN_MS] since the last read started (the caller schedules one
 *  read for then, and only one). */
export function refreshDelay(now: number, lastRead: number, minMs: number = SOURCE_REFRESH_MIN_MS): number {
  const since = now - lastRead;
  return since >= minMs ? 0 : minMs - since;
}

/** What a [createSourceReader] needs from its window: the read itself, the state it
 *  reconciles into, the clock, one timer, and the sequence numbers it shares with the
 *  window's moves. */
export type SourceReaderDeps = {
  fetch: () => Promise<SourceStatus | undefined>;
  get: () => SourcesState;
  commit: (next: SourcesState) => void;
  nextSeq: () => number;
  now: () => number;
  setTimer: (fn: () => void, ms: number) => unknown;
  clearTimer: (handle: unknown) => void;
};

/** The window's reads, and the one later read at most that is ever pending. */
export type SourceReader = {
  /** Read now (`hold`: the host whose move was just confirmed, see [applyRead]) — or,
   *  with a read in flight, once right after it. */
  read: (hold?: string) => void;
  /** The window was shown or focused: an unreachable server gets a fresh series, and
   *  a read now — or, within [SOURCE_REFRESH_MIN_MS] of the last one, when that ends. */
  refresh: () => void;
  /** The whole window (re)loads: a new generation — the pending later read is
   *  cancelled, a read still in flight is ignored when it answers — a fresh series, and
   *  a read now (or right after the one in flight). */
  reload: () => void;
  /** The window is gone: the pending later read is cancelled, and nothing in flight
   *  (a read, a timer) does anything when it lands. */
  dispose: () => void;
};

/** The window's source reads, with their lifetimes kept straight: every read and every
 *  timer belongs to a GENERATION, and one from an older generation (before a reload)
 *  or after [SourceReader.dispose] does nothing — applies no rows, schedules no read.
 *
 *  Reads never overlap: at most one is in flight. A read asked for while one is out
 *  (a refresh, a reload, a move's read-back, a later read) is folded into exactly ONE
 *  follow-up read, started when the one in flight has answered, carrying every `hold`
 *  asked for meanwhile; a pending later read is cancelled whenever a read is asked
 *  for (the read it was waiting to make is being made). At most one later read is
 *  pending at a time, and none while a follow-up is due. */
export function createSourceReader(d: SourceReaderDeps): SourceReader {
  let gen = 0;
  let disposed = false;
  let timer: unknown = undefined;
  let lastRead = 0;
  let newestIssued = 0;
  let inFlight = false;
  /** The follow-up asked for while a read was in flight, with its holds (`null`: none). */
  let followUp: Set<string> | null = null;

  const cancel = () => {
    if (timer !== undefined) {
      d.clearTimer(timer);
      timer = undefined;
    }
  };
  const schedule = (ms: number) => {
    if (disposed || timer !== undefined || followUp !== null) return;
    const mine = gen;
    timer = d.setTimer(() => {
      timer = undefined;
      if (disposed || mine !== gen) return;
      read();
    }, ms);
  };
  const start = (holds: readonly string[]) => {
    inFlight = true;
    const mine = gen;
    const seq = d.nextSeq();
    newestIssued = seq;
    lastRead = d.now();
    void d.fetch().then(
      (status) => landed(mine, seq, holds, status),
      () => landed(mine, seq, holds, undefined),
    );
  };
  const landed = (mine: number, seq: number, holds: readonly string[], status: SourceStatus | undefined) => {
    inFlight = false;
    if (disposed) return;
    if (mine === gen) {
      const { state, retry, retryMs } = applyRead(d.get(), seq, status, holds, seq === newestIssued);
      d.commit(state);
      if (retry) schedule(retryMs); // not while a follow-up is due: it is the newer read
    }
    if (followUp !== null) {
      const next = [...followUp];
      followUp = null;
      start(next);
    }
  };
  const read = (hold?: string) => {
    if (disposed) return;
    cancel();
    if (inFlight) {
      followUp ??= new Set();
      if (hold !== undefined) followUp.add(hold);
      return;
    }
    start(hold === undefined ? [] : [hold]);
  };
  const restart = () => d.commit(restartRetries(d.get()));
  return {
    read,
    refresh: () => {
      if (disposed) return;
      restart();
      const wait = refreshDelay(d.now(), lastRead);
      if (wait === 0) read();
      else schedule(wait);
    },
    reload: () => {
      if (disposed) return;
      gen++;
      cancel();
      restart();
      read();
    },
    dispose: () => {
      disposed = true;
      gen++;
      followUp = null;
      cancel();
    },
  };
}
