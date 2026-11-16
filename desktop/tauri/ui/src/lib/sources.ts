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
 * last showed, marked busy, and asks for one later read. */

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
  error: string | null;
};

/** `source_status`: every server's row plus this machine's identity (or why none). */
export type SourceStatus = {
  rows: SourceRow[];
  this_machine: SourceMachine | null;
  this_machine_error: string | null;
  broker_mode: "external" | "embedded" | "headless-coexist" | null;
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
};

/** Apply one read, issued as `seq`. `hold` names the host whose move was just
 *  confirmed: for it, a read that cannot say anything (no answer, an error row, a
 *  busy row) is NOT applied over the confirmed row. `retry` asks the caller for
 *  one later read: a row was busy, or `hold` got no usable answer. A failed read
 *  (`undefined`) changes nothing else. */
export function applyRead(
  state: SourcesState,
  seq: number,
  status: SourceStatus | undefined,
  hold?: string,
): { state: SourcesState; retry: boolean } {
  if (!status) return { state, retry: hold !== undefined };
  let retry = false;
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
  if (hold !== undefined && !status.rows.some((r) => r.host === hold)) retry = true;
  for (const r of status.rows) {
    if (state.moving[r.host]) continue; // the move in flight settles this row
    if (seq <= (state.applied[r.host] ?? 0)) continue; // older than what is shown
    if (r.host === hold && (r.error || r.busy)) {
      retry = true; // keep the confirmed move; read again later
      continue;
    }
    if (r.busy) {
      const prev = state.rows[r.host];
      next.rows[r.host] = prev ? { ...prev, busy: true } : r;
      retry = true;
    } else {
      next.rows[r.host] = r;
    }
    next.applied[r.host] = seq;
  }
  return { state: next, retry };
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
    rows: { ...state.rows, [host]: { ...prev, source: state.thisMachine, this_machine: true, busy: false, error: null } },
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
/** How long a busy row, or a confirmed move whose read-back said nothing, waits
 *  before its one later read. */
export const SOURCE_RETRY_MS = 3000;

/** How long to wait before a shown/focused refresh may read: 0 now, else the rest
 *  of [SOURCE_REFRESH_MIN_MS] since the last read started (the caller schedules one
 *  read for then, and only one). */
export function refreshDelay(now: number, lastRead: number, minMs: number = SOURCE_REFRESH_MIN_MS): number {
  const since = now - lastRead;
  return since >= minMs ? 0 : minMs - since;
}
