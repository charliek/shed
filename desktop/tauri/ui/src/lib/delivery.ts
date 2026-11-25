/* Provisioning by delivery (plan 039 A7): reading a shed's delivery record off its
 * row, and the badge and detail a row shows. (The provider forms are the Credentials
 * view's, `credentials.ts`.)
 *
 * Pure and dependency-free, in the `sources.ts` shape, so node's own test runner
 * pins it (`test/delivery.test.mjs`). Nothing here ever carries a delivered value:
 * a record names items, targets and fingerprints only. */

export type DeliveryState = "none" | "not_applicable" | "delivered" | "partial" | "not_delivered";

/** The tracked steps, in the one order they run. */
export const STEP_NAMES = ["stage1", "clone", "stage2", "install"] as const;

export interface DeliveryItem {
  name: string;
  provider: string;
  stage: number;
  kind: string;
  base: string | null;
  target: string;
  fingerprint: string;
  expires_at: string | null;
  written_at: string | null;
  result: string;
}

export interface DeliveryProblem {
  provider: string;
  code: string;
  message: string;
}

export interface DeliveryLast {
  at: string;
  reason: string;
  outcome: string;
  failure: { reason: string; rule: string | null; item: string | null; result: string | null } | null;
  /** The source asked: `kind` is `machine` (with its id and name) or `service` (the account
   * service, with neither). */
  source: { kind: string; machine_id: string; machine_name: string } | null;
  problems: DeliveryProblem[];
}

export interface DeliveryStep {
  name: string;
  state: string;
  reason: string | null;
}

export interface DeliveryRecord {
  /** One of [DeliveryState], or a newer server's word, kept as it came. */
  state: string;
  steps: DeliveryStep[] | null;
  last: DeliveryLast | null;
  items: DeliveryItem[];
  expired: string[];
  owed: string[];
  /** The server withheld the rest (`withheld: true`): an admin looking at another
   *  user's shed, who may not deliver to it either. */
  withheld: boolean;
}

/** One status tone, the primitives' `Tone`. */
export type DeliveryTone = "ok" | "attention" | "muted" | "danger";

const obj = (v: unknown): Record<string, unknown> | null =>
  v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
const str = (v: unknown): string | null => (typeof v === "string" && v !== "" ? v : null);
const strs = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);

function readProblem(v: unknown): DeliveryProblem | null {
  const o = obj(v);
  if (!o) return null;
  return { provider: str(o.provider) ?? "", code: str(o.code) ?? "", message: str(o.message) ?? "" };
}

function readItem(v: unknown): DeliveryItem | null {
  const o = obj(v);
  const name = o && str(o.name);
  if (!o || !name) return null;
  return {
    name,
    provider: str(o.provider) ?? "",
    stage: typeof o.stage === "number" ? o.stage : 1,
    kind: str(o.kind) ?? "",
    base: str(o.base),
    target: str(o.target) ?? "",
    fingerprint: str(o.fingerprint) ?? "",
    expires_at: str(o.expires_at),
    written_at: str(o.written_at),
    result: str(o.result) ?? "",
  };
}

function readLast(v: unknown): DeliveryLast | null {
  const o = obj(v);
  if (!o) return null;
  const f = obj(o.failure);
  const src = obj(o.source);
  return {
    at: str(o.at) ?? "",
    reason: str(o.reason) ?? "",
    outcome: str(o.outcome) ?? "",
    failure: f
      ? { reason: str(f.reason) ?? "", rule: str(f.rule), item: str(f.item), result: str(f.result) }
      : null,
    source: src
      ? { kind: str(src.kind) ?? "", machine_id: str(src.machine_id) ?? "", machine_name: str(src.machine_name) ?? "" }
      : null,
    problems: Array.isArray(o.problems)
      ? o.problems.map(readProblem).filter((p): p is DeliveryProblem => p !== null)
      : [],
  };
}

function readSteps(v: unknown): DeliveryStep[] | null {
  const o = obj(v);
  if (!o) return null;
  return STEP_NAMES.map((name) => {
    const s = obj(o[name]);
    return { name, state: (s && str(s.state)) ?? "", reason: s ? str(s.reason) : null };
  });
}

/** A shed's delivery record as its row carries it, decoded defensively: `null` for
 *  anything without a string `state` (a server older than delivery sends none). */
export function readDelivery(raw: unknown): DeliveryRecord | null {
  const o = obj(raw);
  const state = o && str(o.state);
  if (!o || !state) return null;
  const steps = readSteps(o.steps);
  const last = readLast(o.last);
  const items = Array.isArray(o.items)
    ? o.items.map(readItem).filter((i): i is DeliveryItem => i !== null)
    : [];
  const expired = strs(o.expired);
  const owed = strs(o.owed);
  // The server marks a record it stripped for a caller who may not enter the shed
  // (an admin looking at another user's): the state alone can never tell.
  const withheld = o.withheld === true;
  return { state, steps, last, items, expired, owed, withheld };
}

/** The row's badge: its words and tone, and the extras beside it (expired items,
 *  owed steps). */
export function deliveryBadge(rec: DeliveryRecord): { label: string; tone: DeliveryTone; extras: string[] } {
  const [label, tone]: [string, DeliveryTone] =
    rec.state === "delivered" ? ["delivered", "ok"]
    : rec.state === "partial" ? ["partial", "attention"]
    : rec.state === "not_delivered" ? ["not delivered", "danger"]
    : rec.state === "none" ? ["no delivery", "muted"]
    : rec.state === "not_applicable" ? ["not applicable", "muted"]
    : [rec.state.replace(/_/g, " "), "muted"];
  const extras: string[] = [];
  if (rec.expired.length) extras.push(`${rec.expired.length} expired`);
  if (rec.owed.length) extras.push(`owed: ${rec.owed.join(", ")}`);
  return { label, tone, extras };
}

/** The sentence under the badge in the detail, by state. */
export function deliverySummary(rec: DeliveryRecord): string {
  if (rec.withheld) return "Only the shed's user can see what was delivered.";
  switch (rec.state) {
    case "delivered":
      return "Everything was delivered.";
    case "partial":
      return "Some items were delivered; something failed, is owed, or was reported as a problem.";
    case "not_delivered":
      return "Nothing was delivered.";
    case "none":
      return "This shed has no delivery record.";
    case "not_applicable":
      return "Delivery does not apply to this shed.";
    default:
      return "";
  }
}

/** A step's state with its reason, as the detail prints it: `failed (no_source)`. */
export function stepText(s: DeliveryStep): string {
  if (!s.state) return "unknown";
  return s.reason ? `${s.state} (${s.reason})` : s.state;
}

/** A `{result, ok, error, progress}` provision answer (`shed.provision`), as the
 *  detail's one-line result. */
export function provisionResult(out: unknown): { ok: boolean; text: string; warnings: string[] } {
  const o = obj(out);
  if (!o) return { ok: false, text: "No answer.", warnings: [] };
  const progress = Array.isArray(o.progress) ? o.progress.map(obj).filter((p) => p !== null) : [];
  const warnings = progress
    .filter((p) => p!.warning === true)
    .map((p) => str(p!.message))
    .filter((m): m is string => m !== null);
  if (o.ok === true) {
    const rec = readDelivery(obj(o.delivery));
    const state = rec ? deliveryBadge(rec).label : "done";
    return { ok: true, text: `Delivered again: ${state}.`, warnings };
  }
  const e = obj(o.error);
  const msg = (e && str(e.message)) ?? "the server refused";
  return { ok: false, text: `Could not deliver again: ${msg}`, warnings };
}
