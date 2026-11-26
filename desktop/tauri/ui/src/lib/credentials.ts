/* The Credentials view's view-model (plan 040 §3.9): every decision the view makes,
 * in four parts the way the account service's page has them.
 *
 *  1. Where: who answers for you on each configured server, the words and the one
 *     action each row offers, and the fixed words for every refusal (a closed list of
 *     codes and classes: never a server's text).
 *  2. What: each provider, from its descriptor (`delivery.describe`): its state, its
 *     scope and where it lands in words, its form as a draft, what the form requires
 *     before the provider can be turned on, and the `delivery.toml` table a draft
 *     writes. A stored passphrase command never leaves the backend: the view sees the
 *     marker [COMMAND_KEPT] and sends it back to keep the command.
 *  3. Check: what a Check sends (a shed, or a server with a backend and a repo), what
 *     it says before it runs, and its result in words with the details behind them.
 *  4. Recent deliveries: the answers this machine gave (`delivery.recent`), in words.
 *
 * Pure and dependency-free (node's own test runner pins it, `test/credentials.test.mjs`),
 * so the components over it are plain and a port to another toolkit is a port of this
 * module and a redraw. Nothing here ever carries a delivered value. */

import type { SourceRow, SourceStatus } from "./sources";

/* ---- shared ----------------------------------------------------------------- */

const obj = (v: unknown): Record<string, unknown> | null =>
  v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
const str = (v: unknown): string | null => (typeof v === "string" && v !== "" ? v : null);
const strs = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);

/** Control and Unicode format characters out of text that came from elsewhere. */
export function clean(s: string): string {
  return s.replace(/[\p{Cc}\p{Cf}]/gu, "");
}

/** Bytes in words. */
export function formatSize(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "";
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

/** A contract time (`2026-11-25T10:00:00Z`) as the view prints it. */
export function whenText(at: string): string {
  const m = /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})(?:\.\d+)?Z$/.exec(at);
  return m ? `${m[1]} ${m[2]} UTC` : at;
}

/** The three backends, in the order the view ticks them. */
export const BACKENDS = ["vz", "firecracker", "remote"] as const;
export const BACKEND_LABEL: Record<string, string> = { vz: "VZ", firecracker: "Firecracker", remote: "Cloud" };

/* ---- 1. Where ---------------------------------------------------------------- */

/** Who answers on a server (`sources.list`'s `kind`). */
const WHERE_KINDS = ["this_machine", "other_machine", "service", "nobody", "unsupported", "unknown"] as const;
export type WhereKind = (typeof WHERE_KINDS)[number];
const isWhereKind = (k: unknown): k is WhereKind => (WHERE_KINDS as readonly unknown[]).includes(k);

/** One `sources.list` row as the source rows (`sources.ts`) hold it: the reconciliation
 *  fields, plus who answers (`kind`), the account service's URL and state, this
 *  machine's stream to it and whether this machine stopped answering there. `error` is
 *  the fixed words of the row's error class, never the server's. */
export type WhereRow = SourceRow;

/** The fixed words for a row's error class (`shed_app::SOURCE_ERROR_CLASSES`). */
export function rowErrorText(cls: string): string {
  switch (cls) {
    case "unreachable":
      return "The server could not be reached, or its answer could not be read.";
    case "busy":
      return "The server is busy; it is asked again later.";
    case "unauthorized":
      return "The server refused this app's credentials.";
    case "forbidden":
      return "The server does not let this client see who answers.";
    case "source_is_service":
      return "The account service answers for you on this server.";
    case "refused":
      return "The server refused to say who answers.";
    default:
      return "The server could not be asked who answers.";
  }
}

function machine(v: unknown): { id: string; name: string } | null {
  const o = obj(v);
  if (!o) return null;
  return { id: clean(typeof o.id === "string" ? o.id : ""), name: clean(typeof o.name === "string" ? o.name : "") };
}

/** A `sources.list` row as a [WhereRow]. A server older than sources is listed (with
 *  its kind), never hidden. */
export function whereRowOf(raw: unknown): WhereRow | null {
  const o = obj(raw);
  const host = o && str(o.server);
  if (!o || !host) return null;
  const kind = isWhereKind(o.kind) ? o.kind : "unknown";
  const stream = obj(o.stream);
  const cls = str(o.error);
  const m = machine(o.machine);
  return {
    host,
    supported: true,
    source: kind === "this_machine" || kind === "other_machine" ? m : null,
    this_machine: kind === "this_machine",
    connected: o.connected === true,
    busy: o.busy === true,
    unreachable: o.unreachable === true,
    error: kind === "unknown" && cls ? rowErrorText(cls) : null,
    kind,
    url: kind === "service" ? (str(o.url) ? clean(o.url as string) : null) : null,
    state: kind === "service" ? str(o.state) : null,
    paused: o.paused === true,
    stream: stream ? str(stream.state) : null,
    stream_error: stream ? str(stream.last_error) : null,
  };
}

/** `sources.list` as the source reader's status: the rows, and this machine. */
export function whereStatusOf(
  raw: unknown,
): (SourceStatus & { paused_error: string | null; config_error: string | null }) | undefined {
  const o = obj(raw);
  if (!o || !Array.isArray(o.servers)) return undefined;
  return {
    rows: o.servers.map(whereRowOf).filter((r): r is WhereRow => r !== null),
    this_machine: machine(o.this_machine),
    this_machine_error: str(o.this_machine_error),
    paused_error: str(o.paused_error),
    config_error: str(o.config_error),
  };
}

/** The words for a shed config that could not be read (`config_error`), or null: the
 *  rows are then the servers it last listed, and nothing is sent until it reads. */
export function configErrorText(e: string | null): string | null {
  return e
    ? "Your shed config cannot be read, so these are the servers it last listed. Fix it; until it reads, Answer from here and Stop answering from here are refused."
    : null;
}

/** A move's answer (`source.use`'s `row`, the server's document after the move) as a
 *  [WhereRow]: this machine, connected or not yet. */
export function whereRowOfMove(raw: unknown, host: string): WhereRow {
  const o = obj(raw) ?? {};
  const m = machine(o.source);
  const mine = o.this_machine === true;
  return {
    host,
    supported: true,
    source: m,
    this_machine: mine,
    connected: o.connected === true,
    busy: false,
    unreachable: false,
    error: null,
    kind: mine ? "this_machine" : m ? "other_machine" : "nobody",
    url: null,
    state: null,
    paused: false,
    stream: null,
    stream_error: null,
  };
}

/** Who answers on a row, from what the row shows now (an optimistic move or stop
 *  included). */
export function whereKind(r: WhereRow): WhereKind {
  if (isWhereKind(r.kind)) return r.kind;
  if (r.this_machine) return "this_machine";
  return r.source ? "other_machine" : "nobody";
}

/** This machine's stream to a server, in words. */
export function streamText(state: string | null | undefined, lastError?: string | null): string {
  switch (state) {
    case "connected":
      return "connected";
    case "connecting":
    case null:
    case undefined:
      return "connecting";
    case "not_bound":
      return "not answering: another machine is bound";
    case "duplicate":
      return "not answering: the server holds another stream for this machine";
    case "service":
      return "not answering: the account service answers";
    case "stopped":
      return "stopped";
    case "offline":
      return lastError ? `not connected (${lastError.replace(/_/g, " ")})` : "not connected";
    default:
      return state.replace(/_/g, " ");
  }
}

/** One Where row, as the view draws it. */
export type WhereView = {
  server: string;
  kind: WhereKind;
  /** Who answers, in words; a machine's name is `name`, drawn as a text node. */
  who: string;
  name: string | null;
  url: string | null;
  /** The line under it: the stream, "not delivering", a pause, an older server. */
  detail: string | null;
  notDelivering: boolean;
  /** The last thing known, kept while the server cannot say (or, with nothing known
   *  yet, a row that says the server cannot be reached). */
  unreachable: boolean;
  retrying: boolean;
  /** This machine stopped answering on that server, whoever answers there now. */
  paused: boolean;
  /** The actions the row offers, in order (none, one, or, for a stop whose outcome is
   *  uncertain, Answer from here before Stop answering from here). */
  actions: WhereAction[];
  /** Why the row is in error: fixed words. */
  error: string | null;
};

/** One action of a Where row: "Answer from here" (`use`) or "Stop answering from here"
 *  (`unbind`). */
export type WhereAction = { id: "use" | "unbind"; label: string; enabled: boolean };

/** One row as the view draws it. `moving`: a move or a stop of this row is in flight
 *  (the row shows its optimistic result); `canUse`: this machine has an identity and
 *  is not already the source (`sources.ts`'s rule); `refusal`: the last refused action's
 *  fixed words. Where the account service answers there is no action, and none on a
 *  server older than sources. */
export function whereView(
  r: WhereRow,
  o: { moving: boolean; canUse: boolean; refusal: string | null; stopUnverified?: boolean },
): WhereView {
  const kind = whereKind(r);
  const name = r.source ? r.source.name || "an unnamed machine" : null;
  let who: string;
  let detail: string | null = null;
  switch (kind) {
    case "this_machine":
      who = "This machine answers here";
      detail = o.moving ? "switching…" : r.stream ? streamText(r.stream, r.stream_error) : r.connected ? "connected" : "not connected yet";
      break;
    case "other_machine":
      who = "Another machine answers here:";
      detail = r.connected ? "connected" : "not connected";
      break;
    case "service":
      who = "The account service answers here";
      detail = r.state === "not_delivering" ? "It is not delivering to this server." : null;
      break;
    case "nobody":
      who = "Nobody answers here";
      detail = r.paused
        ? "This machine stopped answering here."
        : "The first of your machines to connect answers.";
      break;
    case "unsupported":
      who = "This server is older than credential sources";
      break;
    default:
      who = r.unreachable ? "This server cannot be reached" : "Who answers here is not known";
  }
  // The pause is this machine's, and holds whoever answers there now.
  if (r.paused && kind !== "nobody") {
    detail = detail ? `${detail} · this machine stopped answering here` : "This machine stopped answering here.";
  }
  if (r.unreachable && kind === "unknown") detail = "Nothing is known of it yet.";
  else if (r.unreachable) detail = detail ? `${detail} · cannot reach the server` : "cannot reach the server";
  else if (r.busy) detail = detail ? `${detail} · the server is busy, retrying` : "the server is busy, retrying";
  // No action where the account service answers, on a server older than sources, or
  // on one that cannot be reached now (the server could not take it). Where this
  // machine is bound and yet stopped answering (a stop whose outcome was uncertain),
  // "Answer from here" undoes the pause, and "Stop answering from here" tries again.
  // Where another machine answers and this one has not stopped there, it can stop (a
  // pause only: that machine's binding is never cleared).
  const actions: WhereAction[] = [];
  if (r.unreachable || kind === "service" || kind === "unsupported") {
    // none
  } else if (kind === "this_machine") {
    if (r.paused && !o.moving) actions.push({ id: "use", label: "Answer from here", enabled: o.canUse });
    actions.push({ id: "unbind", label: o.moving ? "Switching…" : "Stop answering from here", enabled: !o.moving });
  } else {
    actions.push({ id: "use", label: o.moving ? "Stopping…" : "Answer from here", enabled: !o.moving && o.canUse });
    // Where another machine answers and this one has not stopped there; and on any
    // row whose last stop could not be confirmed (`stopUnverified`, `sources.ts`'s
    // `stopRetryable`: the same rule as `canStop`), to try again.
    if (((kind === "other_machine" && !r.paused) || o.stopUnverified) && !o.moving) {
      actions.push({ id: "unbind", label: "Stop answering from here", enabled: true });
    }
  }
  return {
    server: r.host,
    kind,
    who,
    name: kind === "other_machine" ? name : null,
    url: kind === "service" ? (r.url ?? null) : null,
    detail,
    notDelivering: kind === "service" && r.state === "not_delivering",
    unreachable: !!r.unreachable,
    retrying: r.busy,
    paused: !!r.paused,
    actions,
    error: o.refusal ?? r.error,
  };
}

/** The fixed words for a refused "Answer from here" or "Stop answering from here", by
 *  the refusal's code (`source.use` / `source.unbind`): a closed list, never the
 *  server's text. */
export function refusalText(code: string, server: string, what: "use" | "unbind"): string {
  const act = what === "use" ? "move" : "stop";
  switch (code) {
    case "source_is_service":
      return `The account service answers for you on ${server}; no machine does there.`;
    case "unsupported":
      return `${server} is older than credential sources.`;
    case "source_machine_not_found":
      return `${server} has not seen this machine yet: its delivery stream has not reached that server. Check the server, then try again.`;
    case "forbidden":
      return `${server} does not let this client change its credential source.`;
    case "unauthorized":
      return `${server} refused this app's credentials.`;
    case "machine_identity_unavailable":
      return "This machine has no identity, so it cannot answer.";
    case "rebound":
      return `${server} bound this machine again after the stop; it stays stopped here. Try again.`;
    case "source_moved":
      return `Another machine took over as your credential source on ${server}; nothing was cleared, and this machine stays stopped there.`;
    case "moved_after_unbind":
      return `The stop on ${server} cleared this machine, then another machine took over as your credential source; this machine stays stopped there.`;
    case "stream_not_stopped":
      return `This machine's stream to ${server} did not stop in time, so nothing was sent. Try again.`;
    case "bad_request":
      return `${server} is not in your shed config.`;
    case "config_unreadable":
      return "Your shed config cannot be read; fix it, then try again.";
    default:
      return `${server} could not confirm the ${act}; check who answers before trying again.`;
  }
}

/** What a stop's answer (`source.unbind`) says happened: `confirmed` (the binding was
 *  read back, or another machine answers and only this one paused), `unsent` (the
 *  binding could not be read, so nothing was sent: `unverified`), or `sent_unconfirmed`
 *  (the unbind was sent and the binding not read back: `verified: false`). */
export function stopOutcome(done: unknown): "confirmed" | "unsent" | "sent_unconfirmed" {
  const o = obj(done) ?? {};
  if (o.unverified === true) return "unsent";
  if (o.another_machine_answers === true) return "confirmed";
  if (o.verified === false) return "sent_unconfirmed";
  return "confirmed";
}

/** The note under a row whose stop could not be confirmed: fixed words, saying whether
 *  the unbind was sent. */
export function stopUnverifiedText(server: string, sent = false): string {
  return sent
    ? `The stop was sent to ${server}, but its binding could not be read back; this machine stays stopped there. Try again.`
    : `${server}'s answer could not be confirmed, so nothing was sent; this machine stays stopped there. Try again.`;
}

/** A refusal as the app's command throws it (`{code, message}`), reduced to its code. */
export function refusalCode(e: unknown): string {
  const o = obj(e);
  return (o && str(o.code)) ?? "action_failed";
}

/** The words for what is wrong with the list of servers this machine stopped
 *  answering on (`paused_error`), or null. */
export function pausedErrorText(e: string | null): string | null {
  if (!e) return null;
  // A change that could not be saved: the list in use still holds, so this machine
  // keeps answering wherever it has not stopped; the change may not outlive the app.
  if (e === "write_failed") {
    return "A change to the list of servers this machine stopped answering on could not be saved. The list in use still holds: this machine answers wherever it has not stopped, but the change may not survive a restart.";
  }
  // The file could not be read: every server is paused until a write.
  return "The list of servers this machine stopped answering on cannot be read, so it answers on none of them. Answer from here on a server to start again.";
}

/* ---- 2. What: the descriptors ------------------------------------------------- */

const FIELD_KINDS = ["text", "secret", "file", "path", "switch", "list"] as const;
export type FieldKind = (typeof FIELD_KINDS)[number];

export interface FieldDesc {
  key: string;
  kind: FieldKind;
  title: string;
  help: string;
  required: boolean;
  place: "machine" | "service" | "both";
  default?: boolean | string;
  choices: string[];
  write_only: boolean;
  entry: FieldDesc[];
  scoped: boolean;
}

export interface ProviderDesc {
  name: string;
  title: string;
  summary: string;
  lands: string;
  fields: FieldDesc[];
}

function readField(v: unknown): FieldDesc | null {
  const o = obj(v);
  const key = o && str(o.key);
  if (!o || !key) return null;
  const place = o.place === "service" || o.place === "both" ? o.place : "machine";
  return {
    key,
    kind: (FIELD_KINDS as readonly unknown[]).includes(o.kind) ? (o.kind as FieldKind) : "text",
    title: str(o.title) ?? key,
    help: str(o.help) ?? "",
    required: o.required === true,
    place,
    default: typeof o.default === "boolean" || typeof o.default === "string" ? o.default : undefined,
    choices: strs(o.choices),
    write_only: o.write_only === true,
    entry: Array.isArray(o.entry) ? o.entry.map(readField).filter((f): f is FieldDesc => f !== null) : [],
    scoped: o.scoped === true,
  };
}

/** `delivery.describe`'s providers, in the order they are asked. */
export function readDescriptors(raw: unknown): ProviderDesc[] {
  const o = obj(raw);
  const list = o && Array.isArray(o.providers) ? o.providers : [];
  return list
    .map((p) => {
      const d = obj(p);
      const name = d && str(d.name);
      if (!d || !name) return null;
      return {
        name,
        title: str(d.title) ?? name,
        summary: str(d.summary) ?? "",
        lands: str(d.lands) ?? "",
        fields: Array.isArray(d.fields) ? d.fields.map(readField).filter((f): f is FieldDesc => f !== null) : [],
      };
    })
    .filter((d): d is ProviderDesc => d !== null);
}

const onMachine = (f: FieldDesc) => f.place === "machine" || f.place === "both";

/** The fields a machine's form shows, in order. */
export function machineFields(d: ProviderDesc): FieldDesc[] {
  return d.fields.filter(onMachine);
}

/** A list field's entries' machine fields (the files' from, to, base, mode). */
export function entryFields(f: FieldDesc): FieldDesc[] {
  return f.entry.filter(onMachine);
}

/** The modes a file may have in `delivery.toml` (0644 exists in a service only); `""`
 *  is unset, which is 0600. */
export const MACHINE_MODES = ["", "0600", "0400"] as const;
export const MODE_LABEL: Record<string, string> = {
  "": "0600 (default)",
  "0600": "0600",
  "0400": "0400 (read-only)",
};

/** How a field is edited on a machine: one line, a switch, one line per entry, a
 *  write-only command (never shown back), a list of entries, or a choice. */
export function fieldControl(f: FieldDesc): "text" | "switch" | "list" | "command" | "entries" | "choice" {
  if (f.kind === "switch") return "switch";
  if (f.kind === "list" && f.write_only) return "command";
  if (f.kind === "list" && f.entry.length) return "entries";
  if (f.kind === "list") return "list";
  if (f.choices.length || f.key === "mode") return "choice";
  return "text";
}

/** A choice field's options on a machine. */
export function choiceOptions(f: FieldDesc): { value: string; label: string }[] {
  if (f.key === "mode") return MACHINE_MODES.map((m) => ({ value: m, label: MODE_LABEL[m] }));
  return f.choices.map((c) => ({ value: c, label: c }));
}

/* ---- 2. What: scope ------------------------------------------------------------ */

/** A provider's (or a file's) scope as its form edits it. `every`: every server this
 *  machine answers on, including ones added later (writes no `only.servers`); else
 *  exactly the ticked `servers`. No backend ticked is every backend; no pattern is
 *  every repository. */
export interface ScopeDraft {
  every: boolean;
  servers: string[];
  backends: string[];
  repos: string[];
}

export const EVERY_SERVER = "Every server this machine answers on, including ones you add later";
export const ONLY_THESE = "Only these";

export function scopeOf(only: unknown): ScopeDraft {
  const o = obj(only) ?? {};
  const servers = Array.isArray(o.servers) ? strs(o.servers) : null;
  return {
    every: servers === null,
    servers: servers ?? [],
    backends: strs(o.backends),
    repos: strs(o.repos),
  };
}

/** The `only` table a scope writes, or null for none. Servers in the order they are
 *  ticked in ([serverTicks]); backends in [BACKENDS] order; patterns exactly as typed
 *  (a blank one is refused by `delivery.set`, never dropped: dropping it could turn a
 *  restriction into none). */
export function onlyOf(s: ScopeDraft, ticks?: readonly string[]): Record<string, string[]> | null {
  const out: Record<string, string[]> = {};
  if (!s.every) {
    const order = ticks ? [...ticks, ...s.servers.filter((x) => !ticks.includes(x))] : s.servers;
    out.servers = order.filter((x) => s.servers.includes(x));
  }
  const backends = BACKENDS.filter((b) => s.backends.includes(b));
  const unknown = s.backends.filter((b) => !(BACKENDS as readonly string[]).includes(b));
  if (backends.length || unknown.length) out.backends = [...backends, ...unknown];
  if (s.repos.length) out.repos = [...s.repos];
  return Object.keys(out).length ? out : null;
}

/** The servers a scope can be ticked for: each configured server where a machine can
 *  answer (not where the account service answers, nor one older than sources), then
 *  any the scope already names that is not among them, so nothing is dropped unseen. */
export function serverTicks(rows: readonly WhereRow[], scope: ScopeDraft | null): string[] {
  const out = rows
    .filter((r) => {
      const k = whereKind(r);
      return k !== "service" && k !== "unsupported";
    })
    .map((r) => r.host);
  for (const s of scope?.servers ?? []) if (!out.includes(s)) out.push(s);
  return out;
}

function joinWords(xs: string[]): string {
  if (xs.length <= 1) return xs.join("");
  return `${xs.slice(0, -1).join(", ")} and ${xs[xs.length - 1]}`;
}

/** A scope in words, a phrase of its own: `On every server · VZ and Cloud sheds only ·
 *  repos matching example/*`, `Only on a and b`. */
export function scopeWords(only: unknown): string {
  const s = scopeOf(only);
  const parts = [s.every ? "On every server" : s.servers.length ? `Only on ${joinWords(s.servers)}` : "On no server"];
  if (s.backends.length) parts.push(`${joinWords(s.backends.map((b) => BACKEND_LABEL[b] ?? b))} sheds only`);
  if (s.repos.length) parts.push(`repos matching ${joinWords(s.repos)}`);
  return parts.join(" · ");
}

/* ---- 2. What: drafts ---------------------------------------------------------- */

/** How `delivery.settings` shows a stored passphrase command, and how `delivery.set`
 *  takes it back unchanged: its arguments never leave the backend. */
export const COMMAND_KEPT = "set";

/** A passphrase command in the form: the stored one kept unseen, or a new argv. */
export type CommandDraft = { keep: true } | { keep: false; args: string[] };

/** One entry of a list of entries (a file): its fields as typed, and its own scope. */
export interface EntryDraft {
  values: Record<string, string>;
  scope: ScopeDraft;
}

/** One provider's form. Every string is exactly as typed: nothing is joined, split or
 *  trimmed, so an unchanged setting saves back as it was. */
export interface ProviderDraft {
  text: Record<string, string>;
  bools: Record<string, boolean>;
  lists: Record<string, string[]>;
  command: CommandDraft | null;
  /** A command is stored now (shown as set). */
  commandStored: boolean;
  scope: ScopeDraft;
  /** The list-of-entries field's key (`file`), and its entries. */
  entriesKey: string | null;
  entries: EntryDraft[];
}

/** A provider's draft from its `delivery.settings` config (null: not set up). */
export function draftFromConfig(d: ProviderDesc, config: Record<string, unknown> | null): ProviderDraft {
  const c = config ?? {};
  const draft: ProviderDraft = {
    text: {}, bools: {}, lists: {}, command: null, commandStored: false,
    scope: scopeOf(c.only), entriesKey: null, entries: [],
  };
  for (const f of machineFields(d)) {
    switch (fieldControl(f)) {
      case "switch":
        draft.bools[f.key] = typeof c[f.key] === "boolean" ? (c[f.key] as boolean) : f.default === true;
        break;
      case "list":
        draft.lists[f.key] = strs(c[f.key]);
        break;
      case "command":
        // A view never carries an argv; anything but the marker reads as none stored.
        draft.commandStored = c[f.key] === COMMAND_KEPT;
        draft.command = draft.commandStored ? { keep: true } : { keep: false, args: [] };
        break;
      case "entries": {
        draft.entriesKey = f.key;
        const fields = entryFields(f);
        draft.entries = (Array.isArray(c[f.key]) ? (c[f.key] as unknown[]) : [])
          .map(obj)
          .filter((e): e is Record<string, unknown> => e !== null)
          .map((e) => ({
            values: Object.fromEntries(fields.map((ef) => [ef.key, typeof e[ef.key] === "string"
              ? (e[ef.key] as string)
              : typeof ef.default === "string" ? ef.default : ""])),
            scope: scopeOf(e.only),
          }));
        break;
      }
      default:
        draft.text[f.key] = typeof c[f.key] === "string" ? (c[f.key] as string) : "";
    }
  }
  return draft;
}

/** A new entry: every field empty but its default, its scope every server. */
export function newEntry(d: ProviderDesc, key: string): EntryDraft {
  const f = d.fields.find((x) => x.key === key);
  return {
    values: Object.fromEntries((f ? entryFields(f) : []).map((ef) => [
      ef.key, typeof ef.default === "string" ? ef.default : "",
    ])),
    scope: { every: true, servers: [], backends: [], repos: [] },
  };
}

/** The `delivery.set` config a draft writes, with `enabled`. An empty text field, an
 *  empty plain-list entry and an entry with every field empty are left out (none is a
 *  valid setting); a command's arguments are sent exactly, empty ones included, and a
 *  kept command as [COMMAND_KEPT]. `ticks` orders the servers of each scope. */
export function configFromDraft(
  d: ProviderDesc,
  draft: ProviderDraft,
  enabled: boolean,
  ticks?: readonly string[],
): Record<string, unknown> {
  const out: Record<string, unknown> = { enabled };
  for (const f of machineFields(d)) {
    switch (fieldControl(f)) {
      case "switch":
        out[f.key] = draft.bools[f.key] === true;
        break;
      case "list": {
        const list = (draft.lists[f.key] ?? []).filter((v) => v !== "");
        if (list.length) out[f.key] = list;
        break;
      }
      case "command":
        if (draft.command?.keep) out[f.key] = COMMAND_KEPT;
        else if (draft.command && draft.command.args.length) out[f.key] = [...draft.command.args];
        break;
      case "entries": {
        const fields = entryFields(f);
        out[f.key] = draft.entries
          .filter((e) => fields.some((ef) => ef.required && (e.values[ef.key] ?? "") !== ""))
          .map((e) => {
            const x: Record<string, unknown> = {};
            for (const ef of fields) {
              const v = e.values[ef.key] ?? "";
              if (v !== "") x[ef.key] = v;
            }
            const only = onlyOf(e.scope, ticks);
            if (only) x.only = only;
            return x;
          });
        break;
      }
      default: {
        const v = draft.text[f.key] ?? "";
        if (v !== "") out[f.key] = v;
      }
    }
  }
  const only = onlyOf(draft.scope, ticks);
  if (only) out.only = only;
  return out;
}

/** A draft for a dump: a command's arguments are never shown, only how many. */
export function redactedConfig(d: ProviderDesc, draft: ProviderDraft, enabled: boolean): Record<string, unknown> {
  const out = configFromDraft(d, draft, enabled);
  for (const f of machineFields(d)) {
    if (fieldControl(f) === "command" && Array.isArray(out[f.key])) {
      out[f.key] = `replaced (${(out[f.key] as unknown[]).length} arguments)`;
    }
  }
  return out;
}

/** What a draft still needs before the provider can be turned on: fixed words, empty
 *  when it is complete. The descriptor's required fields, and what each provider needs
 *  to deliver anything: a passphrase from a file or a command (envsecrets), something
 *  to read (API keys), at least one file (files); a scope of "only these" needs a
 *  ticked server. */
export function missing(d: ProviderDesc, draft: ProviderDraft): string[] {
  const out: string[] = [];
  for (const f of machineFields(d)) {
    const ctl = fieldControl(f);
    if (ctl === "text" && f.required && (draft.text[f.key] ?? "") === "") out.push(`${f.title} is required.`);
    if (ctl === "entries") {
      draft.entries.forEach((e, i) => {
        for (const ef of entryFields(f)) {
          if (ef.required && (e.values[ef.key] ?? "") === "") out.push(`File ${i + 1}: ${ef.title} is required.`);
        }
        if (!e.scope.every && e.scope.servers.length === 0) {
          out.push(`File ${i + 1}: tick at least one server, or choose every server.`);
        }
      });
    }
  }
  if (d.name === "envsecrets-login") {
    const file = (draft.text.passphrase_file ?? "") !== "";
    const cmd = draft.command?.keep === true || (draft.command?.keep === false && (draft.command.args[0] ?? "") !== "");
    if (!file && !cmd) out.push("A passphrase file or a passphrase command is required.");
  }
  if (d.name === "api-keys") {
    const any = draft.bools.craze_providers === true || (draft.lists.env_files ?? []).some((v) => v !== "");
    if (!any) out.push("craze's providers.toml or an env file is required.");
  }
  if (draft.entriesKey && draft.entries.length === 0) out.push("At least one file is required.");
  if (!draft.scope.every && draft.scope.servers.length === 0) {
    out.push("Tick at least one server, or choose every server.");
  }
  return out;
}

/* ---- 2. What: the form's changes ------------------------------------------------ */

/** Where a scope lives: the provider's (`null`) or an entry's (its index). */
export type ScopeAt = number | null;

function scopeAt(d: ProviderDraft, at: ScopeAt): ScopeDraft | null {
  if (at === null) return d.scope;
  return d.entries[at]?.scope ?? null;
}

function withScope(d: ProviderDraft, at: ScopeAt, s: ScopeDraft): ProviderDraft {
  if (at === null) return { ...d, scope: s };
  return { ...d, entries: d.entries.map((e, i) => (i === at ? { ...e, scope: s } : e)) };
}

/** A list by its form name: a plain list field, `passphrase_command` (while it is
 *  being replaced), `only.repos`, or `<entries>.<i>.only.repos`. */
function listOf(d: ProviderDraft, name: string): string[] | null {
  if (name === "only.repos") return d.scope.repos;
  const m = /^([a-z_]+)\.(\d+)\.only\.repos$/.exec(name);
  if (m) return m[1] === d.entriesKey ? (d.entries[Number(m[2])]?.scope.repos ?? null) : null;
  if (d.command && name === "passphrase_command") return d.command.keep ? null : d.command.args;
  return d.lists[name] ?? null;
}

function withList(d: ProviderDraft, name: string, next: string[]): ProviderDraft {
  if (name === "only.repos") return { ...d, scope: { ...d.scope, repos: next } };
  const m = /^([a-z_]+)\.(\d+)\.only\.repos$/.exec(name);
  if (m) {
    const i = Number(m[2]);
    return { ...d, entries: d.entries.map((e, j) => (j === i ? { ...e, scope: { ...e.scope, repos: next } } : e)) };
  }
  if (name === "passphrase_command") return { ...d, command: { keep: false, args: next } };
  return { ...d, lists: { ...d.lists, [name]: next } };
}

/** The form's changes, each a pure step: an unknown name changes nothing. */
export const edit = {
  text: (d: ProviderDraft, key: string, v: string): ProviderDraft =>
    key in d.text ? { ...d, text: { ...d.text, [key]: v } } : d,
  bool: (d: ProviderDraft, key: string, v: boolean): ProviderDraft =>
    key in d.bools ? { ...d, bools: { ...d.bools, [key]: v } } : d,
  entry: (d: ProviderDraft, name: string, i: number, v: string): ProviderDraft => {
    const list = listOf(d, name);
    if (!list || i < 0 || i >= list.length) return d;
    return withList(d, name, list.map((x, j) => (j === i ? v : x)));
  },
  add: (d: ProviderDraft, name: string, desc?: ProviderDesc): ProviderDraft => {
    if (name === d.entriesKey && desc) return { ...d, entries: [...d.entries, newEntry(desc, name)] };
    const list = listOf(d, name);
    return list ? withList(d, name, [...list, ""]) : d;
  },
  remove: (d: ProviderDraft, name: string, i: number): ProviderDraft => {
    if (name === d.entriesKey) return { ...d, entries: d.entries.filter((_, j) => j !== i) };
    const list = listOf(d, name);
    return list ? withList(d, name, list.filter((_, j) => j !== i)) : d;
  },
  /** One field of an entry (`from`, `to`, `base`, `mode`). */
  entryValue: (d: ProviderDraft, i: number, key: string, v: string): ProviderDraft => {
    const e = d.entries[i];
    if (!e || !(key in e.values)) return d;
    return { ...d, entries: d.entries.map((x, j) => (j === i ? { ...x, values: { ...x.values, [key]: v } } : x)) };
  },
  /** "Every server…" (true) or "Only these" (false). */
  every: (d: ProviderDraft, at: ScopeAt, every: boolean): ProviderDraft => {
    const s = scopeAt(d, at);
    return s ? withScope(d, at, { ...s, every }) : d;
  },
  /** A server's tick (it turns the scope to "Only these"). */
  server: (d: ProviderDraft, at: ScopeAt, name: string, on: boolean): ProviderDraft => {
    const s = scopeAt(d, at);
    if (!s) return d;
    const servers = on ? (s.servers.includes(name) ? s.servers : [...s.servers, name]) : s.servers.filter((x) => x !== name);
    return withScope(d, at, { ...s, every: false, servers });
  },
  /** A backend's tick. */
  backend: (d: ProviderDraft, at: ScopeAt, name: string, on: boolean): ProviderDraft => {
    const s = scopeAt(d, at);
    if (!s || !(BACKENDS as readonly string[]).includes(name)) return d;
    const backends = on ? (s.backends.includes(name) ? s.backends : [...s.backends, name]) : s.backends.filter((x) => x !== name);
    return withScope(d, at, { ...s, backends });
  },
  /** Replace the stored command with a new argv, typed from empty. */
  replaceCommand: (d: ProviderDraft): ProviderDraft => (d.command ? { ...d, command: { keep: false, args: [""] } } : d),
  /** No command at all. */
  clearCommand: (d: ProviderDraft): ProviderDraft => (d.command ? { ...d, command: { keep: false, args: [] } } : d),
  /** Back to the stored command, unchanged. */
  keepCommand: (d: ProviderDraft): ProviderDraft => (d.commandStored ? { ...d, command: { keep: true } } : d),
};

/* ---- 2. What: the rows -------------------------------------------------------- */

/** A provider as `delivery.settings` lists it. */
export interface ProviderSetting {
  name: string;
  configured: boolean;
  enabled: boolean;
  only: unknown;
  config: Record<string, unknown> | null;
}

export type WhatAction = "setup" | "edit" | "turn_on" | "turn_off" | "remove";

export const ACTION_LABEL: Record<WhatAction, string> = {
  setup: "Set up",
  edit: "Edit",
  turn_on: "Turn on",
  turn_off: "Turn off",
  remove: "Remove",
};

/** One provider row, as the view draws it. */
export type WhatView = {
  name: string;
  title: string;
  summary: string;
  state: "on" | "off" | "not_set_up";
  stateText: string;
  lands: string;
  scope: string | null;
  missing: string[];
  actions: WhatAction[];
};

/** Where a provider's items land, in words: the files' own targets, else the
 *  descriptor's line. */
export function landsText(d: ProviderDesc, config: Record<string, unknown> | null): string {
  const entries = machineFields(d).find((f) => fieldControl(f) === "entries");
  if (entries && config && Array.isArray(config[entries.key])) {
    const targets = (config[entries.key] as unknown[])
      .map(obj)
      .filter((e): e is Record<string, unknown> => e !== null && typeof e.to === "string")
      .map((e) => (e.base === "project" ? `the project's ${e.to}` : `~/${e.to}`));
    if (targets.length) return targets.join(", ");
  }
  return d.lands;
}

/** A provider's row: set up or not, on or off, its scope and where it lands, and its
 *  actions. A provider that is off and complete can be turned on as it is; one that
 *  is incomplete is set up through its form, which turns it on when it saves. */
export function whatView(d: ProviderDesc, p: ProviderSetting | null): WhatView {
  const configured = !!p?.configured;
  const config = configured ? (p?.config ?? {}) : null;
  const need = missing(d, draftFromConfig(d, config));
  const state = !configured ? "not_set_up" : p?.enabled ? "on" : "off";
  const actions: WhatAction[] =
    state === "not_set_up" ? ["setup"]
    : state === "on" ? ["edit", "turn_off", "remove"]
    : need.length ? ["setup", "remove"]
    : ["turn_on", "edit", "remove"];
  return {
    name: d.name,
    title: d.title,
    summary: d.summary,
    state,
    stateText: state === "on" ? "On" : state === "off" ? (need.length ? "Off · not complete" : "Off") : "Not set up",
    lands: landsText(d, config),
    scope: configured ? scopeWords(p?.only) : null,
    missing: configured ? need : [],
    actions,
  };
}

/** What a form's Save writes: a Set up turns the provider on; an Edit keeps it as it
 *  is. */
export function saveEnabled(mode: "setup" | "edit", current: ProviderSetting | null): boolean {
  return mode === "setup" ? true : !!current?.enabled;
}

/** The form's Save label. */
export function saveLabel(mode: "setup" | "edit", current: ProviderSetting | null): string {
  return saveEnabled(mode, current) && !current?.enabled ? "Save and turn on" : "Save";
}

/** What Turn on and Turn off write: the provider's table as it is, `enabled` set. */
export function switched(p: ProviderSetting, enabled: boolean): Record<string, unknown> {
  return { ...(p.config ?? {}), enabled };
}

/* ---- 3. Check ---------------------------------------------------------------- */

/** The Check form: a shed picked from the list (`server/shed`), or a server with a
 *  backend and a repo (optional). */
export interface CheckForm {
  by: "shed" | "target";
  shed: string;
  server: string;
  backend: string;
  repo: string;
}

export const EMPTY_CHECK: CheckForm = { by: "shed", shed: "", server: "", backend: "vz", repo: "" };

/** A shed in the picker's value: `server/shed`. */
export const shedPick = (server: string, shed: string) => `${server}/${shed}`;

/** What a Check sends (`delivery.preview`), or why it cannot run. */
export function checkRequest(f: CheckForm):
  | { server: string; shed: string; backend?: undefined; repo?: undefined }
  | { server: string; shed?: undefined; backend: string; repo: string | null }
  | { error: string } {
  if (f.by === "shed") {
    const i = f.shed.indexOf("/");
    if (i <= 0 || i === f.shed.length - 1) return { error: "Pick a shed." };
    return { server: f.shed.slice(0, i), shed: f.shed.slice(i + 1) };
  }
  if (!f.server) return { error: "Pick a server." };
  if (!(BACKENDS as readonly string[]).includes(f.backend)) return { error: "Pick a backend." };
  const repo = f.repo.trim();
  if (repo && !/^[^/\s]+\/[^/\s]+$/.test(repo)) return { error: "A repo is owner/name." };
  return { server: f.server, backend: f.backend, repo: repo || null };
}

/** The commands a Check runs on this machine, from the providers that are on: `gh`
 *  (GitHub) and `passphrase_command` (envsecrets, when one is set). `delivery.preview`
 *  computes the same list from the settings it reads, and refuses to run when it
 *  differs from the one the view showed (`settings_changed`). */
export function checkRuns(providers: readonly ProviderSetting[]): string[] {
  const on = (n: string) => providers.find((p) => p.name === n && p.configured && p.enabled);
  const runs: string[] = [];
  if (on("github")) runs.push("gh");
  const env = on("envsecrets-login");
  if (env && env.config?.passphrase_command === COMMAND_KEPT) runs.push("passphrase_command");
  return runs;
}

const RUN_WORDS: Record<string, string> = { gh: "gh", passphrase_command: "your passphrase command" };

/** What Check says before it runs: the commands it runs on this machine. */
export function checkNote(runs: readonly string[]): string {
  return runs.length
    ? `Check runs ${joinWords(runs.map((r) => RUN_WORDS[r] ?? r))} on this machine, as a delivery would, and reads the files the providers name. It sends nothing and shows no value.`
    : "Check reads the files the providers name on this machine, as a delivery would. It runs no command, sends nothing and shows no value.";
}

/** Whether two lists of commands a Check runs are the same. */
export function sameRuns(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}

/** The fixed words for a Check that could not run, by its code (`delivery.preview`'s
 *  refusal): a closed list, never the server's or the system's text. */
export function checkErrorText(code: string, server: string): string {
  switch (code) {
    case "settings_changed":
      return "The settings changed since this view read them. Read what Check runs now, then press Check again.";
    case "not_found":
      return `${server} has no shed by that name.`;
    case "server_refused":
      return `${server} refused to send the shed.`;
    case "action_failed":
      return `${server} could not be reached, or its answer could not be read.`;
    case "bad_shed":
      return "The shed names a backend delivery does not know.";
    case "settings_unreadable":
      return "The settings file cannot be read; fix it first.";
    case "key_unavailable":
      return "This machine's fingerprint key cannot be read.";
    case "bad_request":
      return "Check cannot run with what is filled in.";
    default:
      return "Check could not run.";
  }
}

export type CheckItem = {
  name: string;
  provider: string;
  /** The item in words: the provider's title and where it lands. */
  words: string;
  details: { target: string; kind: string; stage: string; mode: string | null; size: string; fingerprint: string };
};

export type CheckResult = {
  /** What was checked, in words. */
  about: string;
  items: CheckItem[];
  problems: string[];
  /** Providers whose scope left them out. */
  leftOut: string[];
};

/** A `delivery.preview` answer in words, with the details behind each item. */
export function checkResult(raw: unknown, descs: readonly ProviderDesc[]): CheckResult {
  const o = obj(raw) ?? {};
  const pv = obj(o.preview) ?? {};
  const req = obj(o.request);
  const title = (n: string) => descs.find((d) => d.name === n)?.title ?? n;
  const server = str(o.server) ?? "";
  const shed = str(o.shed);
  const backend = req && str(req.backend);
  const repo = req && str(req.repo);
  const about = shed
    ? `${shed} on ${server}${backend ? ` · ${BACKEND_LABEL[backend] ?? backend}` : ""}${repo ? ` · ${repo}` : ""}`
    : `A new ${backend ? `${BACKEND_LABEL[backend] ?? backend} ` : ""}shed on ${server}${repo ? ` with ${repo}` : " with no repo"}`;
  const items = (Array.isArray(pv.items) ? pv.items : []).map(obj).filter((i): i is Record<string, unknown> => i !== null).map((i) => {
    const kind = str(i.kind) ?? "";
    const target = str(i.target) ?? "";
    const where = kind === "env" ? `$${target}` : i.base === "project" ? `the project's ${target}` : `~/${target}`;
    return {
      name: str(i.name) ?? "",
      provider: str(i.provider) ?? "",
      words: `${title(str(i.provider) ?? "")}: ${where}`,
      details: {
        target,
        kind,
        stage: typeof i.stage === "number" ? String(i.stage) : "",
        mode: str(i.mode),
        size: typeof i.size === "number" ? formatSize(i.size) : "",
        fingerprint: str(i.fingerprint) ?? "",
      },
    };
  });
  const problems = (Array.isArray(pv.problems) ? pv.problems : []).map(obj).filter((p): p is Record<string, unknown> => p !== null)
    .map((p) => `${title(str(p.provider) ?? "")}: ${str(p.message) ?? ""} (${str(p.code) ?? ""})`);
  const leftOut = (Array.isArray(pv.providers) ? pv.providers : []).map(obj)
    .filter((p): p is Record<string, unknown> => p !== null && p.ran === false)
    .map((p) => title(str(p.provider) ?? ""));
  return { about, items, problems, leftOut };
}

/* ---- 4. Recent deliveries ----------------------------------------------------- */

export type RecentView = {
  key: string;
  when: string;
  server: string;
  shed: string;
  reason: string;
  items: string;
  problems: string[];
  outcome: string;
};

/** What became of an answer, in words. */
export function outcomeText(v: unknown): string {
  const o = obj(v) ?? {};
  switch (o.outcome) {
    case "accepted":
      return "delivered";
    case "pending":
      return "waiting for the server";
    case "already_answered":
      return "already answered";
    case "refused":
      return `refused${str(o.code) ? ` (${o.code})` : ""}`;
    case "failed":
      return `not sent${str(o.class) ? ` (${String(o.class).replace(/_/g, " ")})` : ""}`;
    case "dropped_stream_ended":
      return "not sent: the stream ended";
    case "dropped_deadline":
      return "not sent: too late";
    default:
      return "unknown";
  }
}

/** `delivery.recent`, newest first, in words: when, the server, the shed, the reason,
 *  the items by name, the problems by provider and code, and what became of it. No
 *  value, and no free text from the server. */
export function recentRows(raw: unknown): RecentView[] {
  const o = obj(raw);
  const list = o && Array.isArray(o.recent) ? o.recent : [];
  return list.map(obj).filter((r): r is Record<string, unknown> => r !== null).map((r, i) => {
    const items = (Array.isArray(r.items) ? r.items : []).map(obj).filter((x): x is Record<string, unknown> => x !== null)
      .map((x) => str(x.name) ?? "");
    const problems = (Array.isArray(r.problems) ? r.problems : []).map(obj).filter((p): p is Record<string, unknown> => p !== null)
      .map((p) => `${str(p.provider) ?? ""}: ${str(p.code) ?? ""}`);
    return {
      key: str(r.request) ?? String(i),
      when: whenText(str(r.at) ?? ""),
      server: str(r.server) ?? "",
      shed: str(r.shed) ?? "",
      reason: str(r.reason) ?? "",
      items: items.length ? `${items.length} ${items.length === 1 ? "item" : "items"}: ${items.join(", ")}` : "no items",
      problems,
      outcome: outcomeText(r.outcome),
    };
  });
}
