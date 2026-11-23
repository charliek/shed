/* Provisioning by delivery (plan 039 A7): reading a shed's delivery record off its
 * row, the badge and detail a row shows, and the Preferences section's provider
 * drafts.
 *
 * Pure and dependency-free, in the `sources.ts` shape, so node's own test runner
 * pins it (`test/delivery.test.mjs`). Nothing here ever carries a delivered value:
 * a record and a preview name items, targets, sizes and fingerprints only, and the
 * settings name files and commands, never their contents. */

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
  source: { machine_id: string; machine_name: string } | null;
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
    source: src ? { machine_id: str(src.machine_id) ?? "", machine_name: str(src.machine_name) ?? "" } : null,
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

/** Bytes as a preview shows them. */
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

/* ---- the Preferences section's providers ------------------------------------- */

/** The five providers, in the one order they are asked. */
export const PROVIDERS = ["ssh-key", "github", "api-keys", "envsecrets-login", "files"] as const;

export const PROVIDER_LABEL: Record<string, string> = {
  "ssh-key": "SSH key",
  github: "GitHub",
  "api-keys": "API keys",
  "envsecrets-login": "envsecrets login",
  files: "Files",
};

export const PROVIDER_DETAIL: Record<string, string> = {
  "ssh-key": "Your SSH key and its public half, into ~/.ssh.",
  github: "Your gh sign-in, and gh as git's credential helper for github.com.",
  "api-keys": "craze's providers.toml, and the variables of your env files.",
  "envsecrets-login": "Your envsecrets config and its passphrase.",
  files: "Files from this machine, copied as they are.",
};

/** A provider field: `text` one string, `bool` a switch, `list` one string per entry,
 *  `command` an argv that may hold the passphrase itself and is never shown back. */
export type FieldKind = "text" | "bool" | "list" | "command";

/** The fields each provider has, in order: `[key, label, kind, placeholder]`. */
export const PROVIDER_FIELDS: Record<string, [string, string, FieldKind, string][]> = {
  "ssh-key": [
    ["key_file", "Key file", "text", "~/.ssh/id_ed25519"],
    ["sign_commits", "Sign commits with it", "bool", ""],
  ],
  github: [["gh", "gh program", "text", "gh"]],
  "api-keys": [
    ["craze_providers", "craze providers.toml", "bool", ""],
    ["env_files", "Env files", "list", "~/.config/shed/keys.env"],
  ],
  "envsecrets-login": [
    ["passphrase_file", "Passphrase file", "text", "~/.config/envsecrets/passphrase"],
    ["passphrase_command", "Or a passphrase command", "command", "argument"],
  ],
  files: [],
};

/** How `delivery.settings` shows a stored passphrase command, and how `delivery.set`
 *  takes it back unchanged: its arguments never leave the backend. */
export const COMMAND_KEPT = "set";

export const ONLY_KEYS = ["servers", "backends", "repos"] as const;
export type OnlyKey = (typeof ONLY_KEYS)[number];

export interface FileDraft {
  from: string;
  to: string;
  base: "home" | "project";
  mode: string;
  /** The entry's own `only`, kept as it is (not edited here). */
  only: Record<string, unknown> | null;
}

/** A passphrase command in the editor: the stored one kept unseen, or a new argv. */
export type CommandDraft = { keep: true } | { keep: false; args: string[] };

/** One provider's editor. Every list is its entries, each its exact string: nothing
 *  is joined, split or trimmed, so an unchanged setting saves back as it was. The
 *  provider's `enabled` is not here: Save writes the switch's, as shown. */
export interface ProviderDraft {
  text: Record<string, string>;
  bools: Record<string, boolean>;
  lists: Record<string, string[]>;
  command: CommandDraft | null;
  /** A command is stored now (shown as "set"). */
  commandStored: boolean;
  only: Record<OnlyKey, string[]>;
  files: FileDraft[];
}

const fields = (provider: string, kind: FieldKind) =>
  (PROVIDER_FIELDS[provider] ?? []).filter(([, , k]) => k === kind).map(([key]) => key);

/** A provider's draft from its `delivery.settings` config (null: not configured). */
export function draftFromConfig(provider: string, config: Record<string, unknown> | null): ProviderDraft {
  const c = config ?? {};
  const text: Record<string, string> = {};
  for (const key of fields(provider, "text")) text[key] = typeof c[key] === "string" ? (c[key] as string) : "";
  const bools: Record<string, boolean> = {};
  for (const key of fields(provider, "bool")) {
    bools[key] = typeof c[key] === "boolean" ? (c[key] as boolean) : key === "craze_providers";
  }
  const lists: Record<string, string[]> = {};
  for (const key of fields(provider, "list")) lists[key] = strs(c[key]);
  let command: CommandDraft | null = null;
  let commandStored = false;
  if (fields(provider, "command").length) {
    const v = c.passphrase_command;
    commandStored = v === COMMAND_KEPT;
    // A view never carries an argv; anything but the marker reads as none stored.
    command = commandStored ? { keep: true } : { keep: false, args: [] };
  }
  const only = obj(c.only) ?? {};
  const files = Array.isArray(c.file)
    ? c.file.map(obj).filter((e): e is Record<string, unknown> => e !== null).map((e) => ({
        from: typeof e.from === "string" ? e.from : "",
        to: typeof e.to === "string" ? e.to : "",
        base: e.base === "project" ? ("project" as const) : ("home" as const),
        mode: typeof e.mode === "string" ? e.mode : "",
        only: obj(e.only),
      }))
    : [];
  return {
    text,
    bools,
    lists,
    command,
    commandStored,
    only: { servers: strs(only.servers), backends: strs(only.backends), repos: strs(only.repos) },
    files,
  };
}

/** The `delivery.set` config for a draft, with the switch's `enabled`. Strings go
 *  as they are. An empty text field, an empty path or `only` entry and a files entry
 *  with neither side are left out (none is a valid setting); a command's arguments
 *  are sent exactly, empty ones included, and a kept command as [COMMAND_KEPT]. */
export function configFromDraft(provider: string, d: ProviderDraft, enabled: boolean): Record<string, unknown> {
  const out: Record<string, unknown> = { enabled };
  for (const [key, , kind] of PROVIDER_FIELDS[provider] ?? []) {
    if (kind === "text") {
      const v = d.text[key] ?? "";
      if (v !== "") out[key] = v;
    } else if (kind === "bool") {
      out[key] = d.bools[key] === true;
    } else if (kind === "list") {
      const list = (d.lists[key] ?? []).filter((v) => v !== "");
      if (list.length) out[key] = list;
    } else if (kind === "command" && d.command) {
      if (d.command.keep) out[key] = COMMAND_KEPT;
      else if (d.command.args.length) out[key] = [...d.command.args];
    }
  }
  const only: Record<string, string[]> = {};
  for (const k of ONLY_KEYS) {
    const list = d.only[k].filter((v) => v !== "");
    if (list.length) only[k] = list;
  }
  if (Object.keys(only).length) out.only = only;
  if (provider === "files") {
    out.file = d.files
      .filter((f) => f.from !== "" || f.to !== "")
      .map((f) => {
        const e: Record<string, unknown> = { from: f.from, to: f.to, base: f.base };
        if (f.mode !== "") e.mode = f.mode;
        if (f.only) e.only = f.only;
        return e;
      });
  }
  return out;
}

/** A draft for a dump: a command's arguments are never shown, only how many. */
export function redactedConfig(provider: string, d: ProviderDraft, enabled: boolean): Record<string, unknown> {
  const out = configFromDraft(provider, d, enabled);
  if (Array.isArray(out.passphrase_command)) out.passphrase_command = `replaced (${out.passphrase_command.length} arguments)`;
  return out;
}

/** One list of a draft by its editor name: a provider list field, `passphrase_command`
 *  (while being replaced), or `only.<key>`. */
function listOf(d: ProviderDraft, name: string): string[] | null {
  if (name.startsWith("only.")) {
    const k = name.slice(5) as OnlyKey;
    return (ONLY_KEYS as readonly string[]).includes(k) ? d.only[k] : null;
  }
  if (name === "passphrase_command") return d.command && !d.command.keep ? d.command.args : null;
  return d.lists[name] ?? null;
}

function withList(d: ProviderDraft, name: string, next: string[]): ProviderDraft {
  if (name.startsWith("only.")) return { ...d, only: { ...d.only, [name.slice(5)]: next } };
  if (name === "passphrase_command") return { ...d, command: { keep: false, args: next } };
  return { ...d, lists: { ...d.lists, [name]: next } };
}

/** The editor's changes, each a pure step: unknown names change nothing. */
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
  add: (d: ProviderDraft, name: string): ProviderDraft => {
    if (name === "files") return { ...d, files: [...d.files, { from: "", to: "", base: "home", mode: "", only: null }] };
    const list = listOf(d, name);
    return list ? withList(d, name, [...list, ""]) : d;
  },
  remove: (d: ProviderDraft, name: string, i: number): ProviderDraft => {
    if (name === "files") return { ...d, files: d.files.filter((_, j) => j !== i) };
    const list = listOf(d, name);
    return list ? withList(d, name, list.filter((_, j) => j !== i)) : d;
  },
  file: (d: ProviderDraft, i: number, key: "from" | "to" | "base" | "mode", v: string): ProviderDraft => {
    if (i < 0 || i >= d.files.length) return d;
    const value = key === "base" ? (v === "project" ? "project" : "home") : v;
    return { ...d, files: d.files.map((f, j) => (j === i ? { ...f, [key]: value } : f)) };
  },
  /** Replace the stored command with a new argv, typed from empty. */
  replaceCommand: (d: ProviderDraft): ProviderDraft => (d.command ? { ...d, command: { keep: false, args: [""] } } : d),
  /** No command at all. */
  clearCommand: (d: ProviderDraft): ProviderDraft => (d.command ? { ...d, command: { keep: false, args: [] } } : d),
  /** Back to the stored command, unchanged. */
  keepCommand: (d: ProviderDraft): ProviderDraft => (d.commandStored ? { ...d, command: { keep: true } } : d),
};

/** The `only` rule in one line for a provider's row: `servers: a, b · repos: x/*`,
 *  or `""` when it has none. */
export function onlyText(only: unknown): string {
  const o = obj(only);
  if (!o) return "";
  return ONLY_KEYS.map((k) => [k, strs(o[k])] as const)
    .filter(([, v]) => v.length)
    .map(([k, v]) => `${k}: ${v.join(", ")}`)
    .join(" · ");
}
