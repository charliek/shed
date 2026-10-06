/* The craze create sheet's pure rules (plan 025 §3.8) — which providers can be
 * picked and which one is picked first, what a directory must look like, the
 * request id's lifecycle, the states' words, and where a machine offers the
 * sheet at all.
 *
 * Pure and dependency-free so node's own test runner pins them
 * (`test/crazeCreate.test.mjs`), the `crazeRows.ts` arrangement: the sheet,
 * the Machines pane, the Agents pane's group header and the launch dialog all
 * read these, and a rule that lived inside a component would be the one copy
 * no test can reach. */

/** One provider as `craze.create_options` carries it (`LaneProvider`). */
export type CrazeProvider = {
  id: string;
  label: string;
  /** `ready` | `needs_setup` | `unavailable` — or a word this build does not
   *  know, which is NOT ready. */
  state: string;
  reason?: string | null;
  fix?: string | null;
};

/** What a create can start on one machine (`LaneCreateOptions`): providers in
 *  craze's order, the default as craze states it, recent directories newest
 *  first. */
export type CrazeCreateOptions = {
  providers: CrazeProvider[];
  default_provider?: string | null;
  recent_dirs: string[];
};

/** **Only a ready provider is selectable** (D5: dim, not hide — and a dimmed
 *  row that still created would only fail at start). */
export function providerSelectable(p: CrazeProvider): boolean {
  return p.state === "ready";
}

/** **The preselection**: craze's default provider IF it is listed AND ready,
 *  else the first ready provider, else none (the sheet then says no provider
 *  is ready, and Create is disabled). */
export function preselectedProvider(o: CrazeCreateOptions): string | null {
  const ready = o.providers.filter(providerSelectable);
  const preferred = ready.find((p) => p.id === o.default_provider);
  return (preferred ?? ready[0])?.id ?? null;
}

/** A selection carried over from an earlier open of the sheet, kept only while
 *  that provider is still listed and ready; else the preselection. */
export function reconcileProvider(current: string | null, o: CrazeCreateOptions): string | null {
  const still = o.providers.find((p) => p.id === current);
  if (still && providerSelectable(still)) return still.id;
  return preselectedProvider(o);
}

/** What the sheet says when NO provider can be picked. */
export const NO_PROVIDER_READY = "no provider is ready on this machine";

/** The directory field's refusal, or `null` when it may be sent: a session's
 *  directory must be ABSOLUTE (empty and relative are refused here; craze
 *  checks that it exists). */
export function directoryProblem(cwd: string): string | null {
  const dir = cwd.trim();
  if (dir === "") return "choose a directory: a recent one, or an absolute path";
  if (!dir.startsWith("/")) return "an absolute path, starting with /";
  return null;
}

// ---- the request id (plan 025 §3.8, the panel's Codex B) -------------------

/** Where one submission stands. */
export type CrazePhase = "idle" | "submitting" | "refused" | "unknown" | "created";

/** A refusal as both doors carry it: the backend's code, craze's words. */
export type CrazeRefusal = { code: string; message: string };

/** A machine's create draft — the typed form, the request id it holds, and
 *  where its submission stands. It lives ABOVE the sheet, so dismissing the
 *  sheet keeps a submission running and a re-open finds the same form and id. */
export type CrazeDraft = {
  provider: string | null;
  cwd: string;
  prompt: string;
  /** Held ONLY while the last submission's outcome is unknown (or while it is
   *  in flight): the retry that follows reuses it, so a lost answer never
   *  makes a second session. `null`: the next submission mints a new one. */
  requestId: string | null;
  phase: CrazePhase;
  refusal: CrazeRefusal | null;
};

export const EMPTY_DRAFT: CrazeDraft = {
  provider: null,
  cwd: "",
  prompt: "",
  requestId: null,
  phase: "idle",
  refusal: null,
};

/** A request id in craze's form: `shed-` and a UUID v4 in its simple (32 hex
 *  digit) form — the shape `shed_craze::new_request_id` mints. */
export function mintRequestId(random: (n: number) => Uint8Array = cryptoBytes): string {
  const b = random(16);
  b[6] = (b[6] & 0x0f) | 0x40; // version 4
  b[8] = (b[8] & 0x3f) | 0x80; // RFC 4122 variant
  return `shed-${Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("")}`;
}

function cryptoBytes(n: number): Uint8Array {
  const b = new Uint8Array(n);
  globalThis.crypto.getRandomValues(b);
  return b;
}

/** A submission starts: the id held from an unknown outcome is REUSED, else a
 *  new one is minted. */
export function beginSubmit(d: CrazeDraft, mint: () => string = () => mintRequestId()): CrazeDraft {
  return { ...d, requestId: d.requestId ?? mint(), phase: "submitting", refusal: null };
}

/** A submission settles. **Only an unknown outcome keeps the id** — craze
 *  stores a failure under its id and would replay that same failure for ten
 *  minutes, so after ANY definite answer (a session, or any refusal) the next
 *  submission mints a new one. The typed form is never touched. */
export function settle(d: CrazeDraft, outcome: { ok: true } | { ok: false; refusal: CrazeRefusal }): CrazeDraft {
  if (outcome.ok) return { ...d, requestId: null, phase: "created", refusal: null };
  if (outcome.refusal.code === "outcome_unknown") {
    return { ...d, phase: "unknown", refusal: outcome.refusal };
  }
  return { ...d, requestId: null, phase: "refused", refusal: outcome.refusal };
}

/** An edit of the form. **Any change mints a new id** for the next
 *  submission (it is another request now — reusing the id with other params
 *  is craze's `request_conflict`), and a settled refusal or unknown outcome no
 *  longer describes the form. Nothing typed is cleared. A submission in flight
 *  is not edited (its controls are disabled). */
export function edit(d: CrazeDraft, patch: Partial<Pick<CrazeDraft, "provider" | "cwd" | "prompt">>): CrazeDraft {
  if (d.phase === "submitting") return d;
  const next = { ...d, ...patch };
  const changed = next.provider !== d.provider || next.cwd !== d.cwd || next.prompt !== d.prompt;
  if (!changed) return d;
  return { ...next, requestId: null, phase: "idle", refusal: null };
}

/** The primary button's label. */
export function primaryLabel(d: CrazeDraft): string {
  if (d.phase === "submitting") return "Creating…";
  if (d.phase === "refused" || d.phase === "unknown") return "Try again";
  return "Create";
}

/** What the sheet says about an UNKNOWN outcome (§3.8). */
export const OUTCOME_UNKNOWN_NOTE =
  "craze did not answer, so the session may have been created: check the session list. Try again resumes the same request.";

/** Where and how a refusal is shown (§3.8, by code): `bad_request` beside the
 *  directory — except craze's `request_conflict` (an id reused with other
 *  params, which this sheet never does on purpose), an internal error; a
 *  start failure (`failed`) as craze's own cause, VERBATIM and monospace;
 *  `unavailable` as "try again"; anything else as said. */
export function refusalView(r: CrazeRefusal): { where: "cwd" | "cause" | "general"; text: string } {
  if (r.code === "bad_request") {
    if (/requestId .* was used already/.test(r.message)) {
      return { where: "general", text: `internal error (a new request id is used next time): ${r.message}` };
    }
    return { where: "cwd", text: r.message };
  }
  if (r.code === "failed") return { where: "cause", text: r.message };
  if (r.code === "unavailable") return { where: "general", text: `${r.message} — try again` };
  return { where: "general", text: r.message };
}

// ---- where the sheet is offered (plan 025 §3.6.5) ---------------------------

/** A host's craze status, as `rc.list`'s `machines[].craze` carries it. */
export type CrazeStatusLike = {
  state?: string | null;
  cause?: string | null;
  create?: boolean | null;
  create_options?: boolean | null;
} | null | undefined;

/** **Whether a machine offers "New craze session"** — its craze is LIVE and
 *  its hub can list providers and create, or it is DORMANT (craze is there and
 *  no hub runs yet: opening the sheet is the explicit action that starts
 *  one). A live hub without those capabilities, or a too-old craze, offers
 *  nothing; neither does an offline, absent or not-installed one. */
export function crazeCreateOffered(craze: CrazeStatusLike): boolean {
  if (craze?.state === "dormant") return true;
  return craze?.state === "live" && !!craze.create && !!craze.create_options;
}

/** The machine's line when its LIVE hub cannot create — listing still works,
 *  creating does not (§3.8's "update craze on this machine"). `null`
 *  otherwise (a too-old craze has the machines pane's own note). */
export function crazeCreateUpdateNote(craze: CrazeStatusLike): string | null {
  if (craze?.state === "live" && !(craze.create && craze.create_options)) {
    return "update craze on this machine to create sessions here";
  }
  return null;
}

/** The note an OPEN sheet shows when its machine's craze stops being usable
 *  under it (Create disabled, the form kept): offline, too old, not installed,
 *  none at all. `null` while live or dormant. */
export function crazeSheetNote(craze: CrazeStatusLike, machine: string): string | null {
  const state = craze?.state ?? "absent";
  if (state === "live") {
    return crazeCreateUpdateNote(craze);
  }
  if (state === "dormant") return null;
  if (craze?.cause === "too_old") return "craze on this machine is too old for shed; update it";
  if (craze?.cause === "not_installed") return `craze is not installed on ${machine}`;
  if (state === "offline") {
    return `craze on ${machine} is offline${craze?.cause ? ` (${craze.cause.replace(/_/g, " ")})` : ""} — your form is kept`;
  }
  return `${machine} has no craze`;
}

// ---- the sheet on screen, read when a create resolves (C10 review) ----------

/** A state setter that keeps `cell.current` in step with what it publishes,
 *  SYNCHRONOUSLY — the value an async continuation reads when it resolves.
 *
 *  A passive effect that copies state into a ref lags a render: a create
 *  resolving in that gap would decide by a sheet that is no longer (or not
 *  yet) the one on screen — reopening a transcript after the sheet was
 *  dismissed, or leaving a reopened sheet up without opening the session's
 *  transcript. A functional update applies to the live value, not to a stale
 *  render's. */
export function liveSetter<T>(
  cell: { current: T },
  publish: (value: T) => void,
): (next: T | ((prev: T) => T)) => void {
  return (next) => {
    cell.current = typeof next === "function" ? (next as (prev: T) => T)(cell.current) : next;
    publish(cell.current);
  };
}

/** The craze sheet on screen: its machine, or `null` when the open modal (if
 *  any) is not the craze sheet. */
export function sheetOnScreen(modal: string | null, machine: string | null): string | null {
  return modal === "craze" ? machine : null;
}

/** Whether a create that just succeeded on `machine` closes the sheet and
 *  opens the session's transcript (plan 025 §3.8): only when the sheet on
 *  screen AT THAT MOMENT is that machine's. A sheet dismissed while the create
 *  ran is left closed — the session simply appears as a row — and another
 *  machine's sheet is left alone. */
export function closesOntoTranscript(onScreen: string | null, machine: string): boolean {
  return onScreen === machine;
}
