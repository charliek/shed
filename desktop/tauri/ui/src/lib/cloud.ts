/* A cloud shed's row as the Sheds pane draws it (plan 041 B2).
 *
 * Every decision about a cloud shed (its words, its lease state, which actions it
 * offers, the extend presets and their cap) is made once in the shared Rust core
 * (`shed-app::cloud`, R13) and arrives on each shed as `cloud_row`. What is here is
 * layout only: reading that struct defensively, the tone a lease state is drawn in,
 * a preset's label, and the custom hours field's reading. Pure and dependency-free
 * so node's own test runner pins it (`test/cloud.test.mjs`). */

/** Where a lease stands (`shed_app::LeaseState`). */
export type LeaseState = "ok" | "soon" | "ended" | "unknown";

/** Which actions a cloud row offers (`shed_app::CloudActions`). Restart is never one. */
export type CloudActions = { start: boolean; stop: boolean; delete: boolean; extend: boolean };

/** A cloud shed's row as the core decided it (`shed_app::CloudRow`). */
export type CloudRow = {
  badge: string;
  size_words: string;
  image: string;
  lease_words: string;
  lease_state: LeaseState;
  run_words: string | null;
  actions: CloudActions;
  extend_presets_h: number[];
  extend_max_h: number;
  expires_words: string | null;
  cap_words: string | null;
  created_words: string | null;
};

/** A failed extend or usage read (`shed_app::CloudFailure`): `words` is what to show. */
export type CloudFailure = { code: string; reason: string; status: number | null; words: string };

const LEASE_STATES: readonly string[] = ["ok", "soon", "ended", "unknown"];

const str = (v: unknown): string => (typeof v === "string" ? v : "");
const optStr = (v: unknown): string | null => (typeof v === "string" ? v : null);
const int = (v: unknown): number => (typeof v === "number" && Number.isInteger(v) ? v : 0);

/** A shed's `cloud_row` as the pane takes it, or null when it has none (any shed of a
 *  server without cloud sheds, which shows as before) or it is not one. */
export function readCloudRow(raw: unknown): CloudRow | null {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const r = raw as Record<string, unknown>;
  const a = (r.actions && typeof r.actions === "object" ? r.actions : {}) as Record<string, unknown>;
  const state = str(r.lease_state);
  return {
    badge: str(r.badge) || "Cloud",
    size_words: str(r.size_words),
    image: str(r.image),
    lease_words: str(r.lease_words),
    lease_state: (LEASE_STATES.includes(state) ? state : "unknown") as LeaseState,
    run_words: optStr(r.run_words),
    actions: {
      start: a.start === true,
      stop: a.stop === true,
      delete: a.delete === true,
      extend: a.extend === true,
    },
    extend_presets_h: Array.isArray(r.extend_presets_h)
      ? r.extend_presets_h.filter((h): h is number => typeof h === "number" && Number.isInteger(h) && h >= 1)
      : [],
    extend_max_h: int(r.extend_max_h),
    expires_words: optStr(r.expires_words),
    cap_words: optStr(r.cap_words),
    created_words: optStr(r.created_words),
  };
}

/** The tone a lease's words are drawn in: amber within its warning time, the danger
 *  tone once it has ended, muted otherwise. */
export function leaseTone(state: LeaseState): "amber" | "danger" | "muted" {
  if (state === "soon") return "amber";
  if (state === "ended") return "danger";
  return "muted";
}

/** A preset's button label: `+6 h`. */
export function presetLabel(hours: number): string {
  return `+${hours} h`;
}

/** The custom hours field's value as an extend: whole hours from 1 to `max` (the
 *  row's `extend_max_h`), or null when the text is not one. */
export function customHours(text: string, max: number): number | null {
  const t = text.trim();
  if (!/^[0-9]{1,4}$/.test(t)) return null;
  const n = Number(t);
  return n >= 1 && n <= max ? n : null;
}

/** The key of one shed incarnation's extend: its server, its name and its creation
 *  time (a shed created again under its name is another shed). */
export function extendKey(host: string, name: string, incarnation: string): string {
  return JSON.stringify([host, name, incarnation]);
}

/** `runs` without the finished ones whose shed incarnation is no longer listed
 *  (`live`, the keys of the current rows): a run with a request still pending is kept
 *  whatever the rows say, since its gate is what stops a second extend. The same
 *  object back when nothing goes, so a caller can skip its update. */
export function pruneRuns<R>(
  runs: Record<string, R>,
  live: ReadonlySet<string>,
  pending: ReadonlySet<string>,
): Record<string, R> {
  const keep = Object.entries(runs).filter(([k]) => live.has(k) || pending.has(k));
  return keep.length === Object.keys(runs).length ? runs : Object.fromEntries(keep);
}

/** What a rejected extend or usage read says: a [`CloudFailure`]'s words, or the
 *  text of any other error. */
export function failureWords(e: unknown): string {
  if (e && typeof e === "object" && typeof (e as { words?: unknown }).words === "string") {
    const w = (e as { words: string }).words;
    if (w) return w;
  }
  if (typeof e === "string" && e) return e;
  if (e instanceof Error && e.message) return e.message;
  return "the extend failed";
}
