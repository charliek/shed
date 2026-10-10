/* The shell's FIRST `ui_report`, and why it is not a seed (shed#400).
 *
 * `useUiBridge` sends one report as soon as its navigate + refresh listeners
 * are live — that report is the harness's readiness signal (`current_pane !=
 * null` means "a `ui.navigate`/`refresh` will be heard"), so it cannot simply
 * be dropped. It goes out after the first shed list lands, and the list can
 * take a while. Meanwhile the report effect may already have published a real
 * report: the host list (`system_df`) answers independently, so `hosts: 2` can
 * be on the wire before the sheds are.
 *
 * The first report used to be a hard-coded seed — zero badges, no sheds, no
 * sidebar rows, refresh token 0. `ui_report` merges by key, so a seed landing
 * after a real report did not add a pane: it REPLACED every key the real one
 * had set. The Hosts badge read 0, `dashboard.dump` and `sidebar.dump` read
 * empty, and the mac tray count went to zero, until the next render's report
 * repaired them.
 *
 * So the first report re-sends the latest whole report the shell has built,
 * with the current pane. The seed is only for the case it was written for: no
 * report has been built yet, so there is nothing true to say beyond the pane.
 *
 * Pure and dependency-free, in the `newest.ts` / `agentsEmpty.ts` shape: the
 * payload type is the bridge's own, passed through structurally, so this module
 * stays free of everything `bridge.ts` pulls in. */

/** The badge counts a report carries (`bridge.ts`'s `Badges`, structurally). */
export type ReportBadges = { sheds: number; agents: number; hosts: number; pending: number };

/** The report the shell sends when it has built none yet: the pane and nothing
 *  else — no badges, no sheds, no sidebar rows, no modal, no host failures, and
 *  no echoed refresh token. Its `mode` is absent, so the style is sampled from
 *  whatever the document currently says. */
export type SeededReport<P> = {
  pane: P;
  sheds: never[];
  refreshToken: 0;
  modal: null;
  badges: ReportBadges;
  sidebar: { servers: never[]; machines: never[] };
  hostErrors: never[];
};

/** The seed for `pane`. Only [initialReport] decides when it is sent. */
export function seededReport<P>(pane: P): SeededReport<P> {
  return {
    pane,
    sheds: [],
    refreshToken: 0,
    modal: null,
    badges: { sheds: 0, agents: 0, hosts: 0, pending: 0 },
    sidebar: { servers: [], machines: [] },
    hostErrors: [],
  };
}

/** What the listener effect's initial report sends: the `latest` complete
 *  report the shell has built, re-addressed to the current `pane`, or — only
 *  when none has been built yet — the seed. Never a seed once a real report
 *  exists, so the readiness signal can no longer undo one. */
export function initialReport<T extends { pane: unknown }>(
  latest: T | null,
  pane: T["pane"],
): T | SeededReport<T["pane"]> {
  if (latest === null) return seededReport(pane);
  return { ...latest, pane };
}
