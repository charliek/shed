/* The Activity pane (plan 039 PR B): the servers' own activity feeds
 * (`activity.server_list`, `GET /api/activity` on every configured server, merged
 * newest first by the Rust side), read into what a row shows, and the pane's paging.
 *
 * Pure and dependency-free, in the `delivery.ts` shape, so node's own test runner pins
 * it (`test/activity.test.mjs`). The server already filters what a user may see; this
 * only reads what it sent, defensively (an older kind carries fewer fields). */

/** One event as the merged page carries it (`activity.go`'s `ActivityEvent`, plus the
 *  `server` the app stamps on it). */
export interface ActivityEvent {
  server: string;
  id: string;
  at: string;
  kind: string;
  shed: { name: string; id?: string; user?: string; owner?: string };
  actor: { kind: string; user?: string; client?: string; implicit?: boolean; script?: boolean };
  result?: string;
  delivery?: { state?: string; outcome?: string; reason?: string; failure?: { reason?: string } | null } | null;
}

/** A server whose page could not be read: it keeps its place for the next page. */
export interface ActivityError {
  server: string;
  status: number | null;
  code: string;
  message: string;
}

/** The pane's state: every event read so far, newest first, the cursor for the page
 *  after (`null`: nothing older anywhere), and the last page's per-server errors. */
export interface ActivityFeed {
  events: ActivityEvent[];
  next: string | null;
  errors: ActivityError[];
}

export const EMPTY_FEED: ActivityFeed = { events: [], next: null, errors: [] };

const str = (v: unknown): string => (typeof v === "string" ? v : "");

/** One event off the wire, or null when it is not one (no id or no kind). */
export function readEvent(raw: unknown): ActivityEvent | null {
  if (!raw || typeof raw !== "object") return null;
  const o = raw as Record<string, unknown>;
  const id = str(o.id);
  const kind = str(o.kind);
  if (!id || !kind) return null;
  const shed = (o.shed && typeof o.shed === "object" ? o.shed : {}) as Record<string, unknown>;
  const actor = (o.actor && typeof o.actor === "object" ? o.actor : {}) as Record<string, unknown>;
  const delivery = o.delivery && typeof o.delivery === "object" ? (o.delivery as ActivityEvent["delivery"]) : null;
  return {
    server: str(o.server),
    id,
    at: str(o.at),
    kind,
    shed: { name: str(shed.name), id: str(shed.id) || undefined, user: str(shed.user) || undefined },
    actor: {
      kind: str(actor.kind) || "unknown",
      user: str(actor.user) || undefined,
      client: str(actor.client) || undefined,
      implicit: actor.implicit === true,
      script: actor.script === true,
    },
    result: str(o.result) || undefined,
    delivery,
  };
}

/** One merged page (`activity.server_list`'s answer), read. */
export function readPage(raw: unknown): ActivityFeed {
  const o = (raw && typeof raw === "object" ? raw : {}) as Record<string, unknown>;
  const events = (Array.isArray(o.events) ? o.events : []).map(readEvent).filter((e): e is ActivityEvent => e !== null);
  const errors = (Array.isArray(o.errors) ? o.errors : []).map((e) => {
    const r = (e && typeof e === "object" ? e : {}) as Record<string, unknown>;
    return {
      server: str(r.server),
      status: typeof r.status === "number" ? r.status : null,
      code: str(r.code),
      message: str(r.message),
    };
  });
  return { events, next: typeof o.next === "string" && o.next ? o.next : null, errors };
}

/** A sortable instant (ms since the epoch); a time that does not parse sorts oldest,
 *  as the Rust merge sorts it. */
function instant(at: string): number {
  const t = Date.parse(at);
  return Number.isNaN(t) ? Number.NEGATIVE_INFINITY : t;
}

/** The feed after the page read at its cursor: the events shown and the page's merged
 *  into one newest-first list (an event already shown, by server and id, is not shown
 *  twice; equal times keep the order they came in), the page's cursor, and its errors.
 *
 *  Merged rather than appended: a server that failed keeps its place, so when it
 *  recovers on a later page its events can be NEWER than rows already shown, and they
 *  belong above them. */
export function appendPage(feed: ActivityFeed, page: ActivityFeed): ActivityFeed {
  const seen = new Set(feed.events.map((e) => `${e.server}\u0000${e.id}`));
  const fresh = page.events.filter((e) => !seen.has(`${e.server}\u0000${e.id}`));
  const events = [...feed.events, ...fresh]
    .map((e, i) => ({ e, i, t: instant(e.at) }))
    .sort((a, b) => (b.t === a.t ? a.i - b.i : b.t > a.t ? 1 : -1))
    .map(({ e }) => e);
  return { events, next: page.next, errors: page.errors };
}

/** Who acted, in a few words. */
export function actorText(a: ActivityEvent["actor"]): string {
  switch (a.kind) {
    case "user": {
      const who = a.user || (a.implicit ? "the server's user" : "a user");
      return a.script ? `${who} (script key)` : who;
    }
    case "client":
      return a.client ? `client ${a.client}` : "a service client";
    case "system":
      return "the server";
    default:
      return "unknown";
  }
}

/** A delivery event's state, in words (`partial · accepted`, `not delivered ·
 *  timeout`), or null when the event carries none. */
export function deliveryText(d: ActivityEvent["delivery"]): string | null {
  if (!d || !d.state) return null;
  const parts = [d.state.replace(/_/g, " ")];
  if (d.outcome) parts.push(d.outcome.replace(/_/g, " "));
  const why = d.failure?.reason || d.reason;
  if (why && d.state !== "delivered") parts.push(why.replace(/_/g, " "));
  return parts.join(" · ");
}

/** Whether the event ended well (a `result` of `ok`, or a delivered delivery), badly,
 *  or says nothing either way. */
export function outcomeTone(e: ActivityEvent): "ok" | "bad" | "none" {
  const d = e.delivery?.state;
  if (d) return d === "delivered" ? "ok" : d === "none" || d === "not_applicable" ? "none" : "bad";
  if (!e.result) return "none";
  return e.result === "ok" ? "ok" : "bad";
}

/** A server's failure, as the pane says it. */
export function errorText(e: ActivityError): string {
  const status = e.status != null ? ` (${e.status})` : "";
  return `${e.server}: ${e.message || e.code || "could not be read"}${status}`;
}
