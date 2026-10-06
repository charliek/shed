/* The craze rows' pure rules (plan 025 §3.6.5) — what a craze session's card
 * and transcript header say, and where its End tab goes.
 *
 * Pure and dependency-free so node's own test runner pins them
 * (`test/crazeRows.test.mjs`), the `laneVerbs.ts` arrangement: the card, the
 * Machines pane and the transcript panel all read these, and a rule that lived
 * inside a component would be the one copy no test can reach. */

/** The fields of a session row these rules read — the shape of `RcSession` in
 *  `bridge.ts`, narrowed. */
export type CrazeRowLike = {
  source?: string | null;
  machine?: string | null;
  host: string;
  shed: string;
  slug: string;
  tab_id?: string | null;
  activity?: string | null;
  doing?: string | null;
  last_reply?: string | null;
};

/** Where a card's End/kill action goes: a roost `tab.close` on a host
 *  addressed by `machine` (`machine.kill`), or — for a payload too old to
 *  stamp an address — by `(host, shed)` (`rc.kill`). `null`: the row has no
 *  tab to end. */
export type KillTarget =
  | { via: "machine"; machine: string; slug: string }
  | { via: "shed"; host: string; shed: string; slug: string };

/** **A craze row's End tab routes by its TYPED `tab_id`, never by its slug.**
 *  `machine.kill` parses `slug` as a roost tab id, and a craze row's slug is
 *  its hostId — twelve hex digits that would either fail to parse or, worse,
 *  name somebody else's tab. So the craze row sends `String(tab_id)` and a
 *  craze row with no tab (a headless session) has nothing to end. A roost
 *  row's slug IS its tab id, as it always was. */
export function killTarget(s: CrazeRowLike): KillTarget | null {
  if (s.source === "craze") {
    if (!s.machine || s.tab_id == null || s.tab_id === "") return null;
    return { via: "machine", machine: s.machine, slug: String(s.tab_id) };
  }
  if (s.machine) return { via: "machine", machine: s.machine, slug: s.slug };
  return { via: "shed", host: s.host, shed: s.shed, slug: s.slug };
}

/** **Open in terminal is offered on a HEADLESS craze row** (plan 025
 *  §3.6.5/§3.6.6): a craze session with no roost tab attached, on a host the
 *  row addresses. A row that has a tab offers End tab instead; a roost row
 *  never offers it. */
export function canOpenTerminal(s: CrazeRowLike): boolean {
  return s.source === "craze" && !!s.machine && (s.tab_id == null || s.tab_id === "");
}

/** The card's one-line "what is it doing": craze's `doing`; else, when the
 *  session is idle, its last reply DIMMED (it is history, not activity);
 *  else nothing. */
export function crazeDoingLine(s: CrazeRowLike): { text: string; dimmed: boolean } | null {
  const doing = (s.doing ?? "").trim();
  if (doing) return { text: doing, dimmed: false };
  const reply = (s.last_reply ?? "").trim();
  if (reply && s.activity === "idle") return { text: reply, dimmed: true };
  return null;
}

/** The Machines pane's note for a host's craze (plan 025 §3.6.5): too old
 *  says so, with what to do; not installed says NOTHING (a machine without
 *  craze is not a problem to report); everything else is the status chip's
 *  business, not a note's. */
export function crazeMachineNote(
  craze: { state?: string | null; cause?: string | null } | null | undefined,
): string | null {
  if (craze?.cause === "too_old") return "craze on this machine is too old for shed; update it";
  return null;
}

/** The transcript header's line for a session's permission posture (craze's
 *  `bypass` | `prompt`, plan 025 §3.6.5) — so a sheet-created session, which
 *  runs `bypass`, says out loud that it runs tools without asking. An unknown
 *  word renders as itself; none at all renders nothing. */
export function permissionLine(mode: string | null | undefined): string | null {
  const m = (mode ?? "").trim();
  if (!m) return null;
  if (m === "bypass") return "runs tools without asking";
  if (m === "prompt") return "asks before running tools";
  return `permissions: ${m}`;
}

/** The fields of a lane's session row the transcript header reads — the
 *  shape of `LaneSessionRow` in `bridge.ts`, narrowed. */
export type LaneHeaderRowLike = {
  title?: string | null;
  cwd?: string | null;
  permission_mode?: string | null;
};

/** What the transcript header says: the session's title, its directory and
 *  its permission line.
 *
 *  **From the LIVE row — `lane.messages`' `session` — and from `lane.open`'s
 *  only until the first seed has swapped one in** (plan 025 §3.6.5). The open's
 *  row is whatever the source listed at that instant and is re-answered
 *  unchanged while the lane stays open; for the transcript the create sheet
 *  opens at once, that is the create's own row, which carries no permission
 *  mode — so a header that kept reading it never said "runs tools without
 *  asking" for as long as the panel stayed open (live leg 1). The stream's row
 *  carries the attach info document's mode. Whole rows, not field by field:
 *  the live row is the newer truth, including about what it does not say. */
export function laneHeader(
  opened: LaneHeaderRowLike | null | undefined,
  live: LaneHeaderRowLike | null | undefined,
): { title: string; cwd: string; permission: string | null } {
  const row = live ?? opened ?? null;
  return {
    title: row?.title ?? "",
    cwd: row?.cwd ?? "",
    permission: permissionLine(row?.permission_mode),
  };
}
