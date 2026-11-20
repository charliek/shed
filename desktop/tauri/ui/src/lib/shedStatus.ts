/* A shed row's status, as the Sheds pane shows it (plan 037 §3.14).
 *
 * Pure (no React, no DOM, no Tauri) so node's own test runner can load it — the
 * `sources.ts` arrangement. The row renders `statusText`, decides its controls
 * with `isBusy`, and the dashboard report reads both back off the rendered row
 * (the card `pairRowsWithCards` gives it), so `dashboard.dump` says what the row
 * shows rather than what these helpers say. */

/** The fields of a shed (as shed-core serializes it) these helpers read. */
export type ShedStatusLike = {
  /** The lowercase status word: `running`, `stopped`, `starting`, `stopping`,
   *  `error`, or `unknown` for a status shed-core does not know. */
  status: string;
  /** Why the shed is in that status — a code such as `capacity`, `run_limit`,
   *  `external` or `no_pin` (only the remote backend sends one); null or absent
   *  when it is the plain case. */
  status_reason?: string | null;
};

/** The status as a row shows it: the word, and the reason in parentheses when
 *  there is one — the same text as the CLI's `shed list` (`statusWithReason`):
 *  `stopping`, `stopped (capacity)`, `starting (no_pin)`. The reason is the
 *  server's code, never reworded. */
export function statusText(s: ShedStatusLike): string {
  return s.status_reason ? `${s.status} (${s.status_reason})` : s.status;
}

/** The statuses in which a shed is changing on its own: its row shows a
 *  spinner and offers no Start, Stop or Delete until it settles. */
const BUSY_STATUSES: readonly string[] = ["restarting", "starting", "stopping"];

/** Whether the shed is between states (see `BUSY_STATUSES`). */
export function isBusy(s: ShedStatusLike): boolean {
  return BUSY_STATUSES.includes(s.status);
}

/** A shed's identity on the Sheds pane: its server and its name. */
export type ShedIdentity = { host: string; name: string };

/** Pair each reported row with the mounted card that renders it, so the
 *  dashboard report reads a row's evidence off its own card and never off
 *  another's. A card's identity comes from `identityOf` (null: not a shed card);
 *  host and name are compared as plain strings.
 *
 *  A server can list two sheds under one name. The pane renders rows in list
 *  order, so when the rows and the cards of one identity are equal in number,
 *  the n-th such row takes the n-th such card. When they differ (a card not
 *  mounted, or one left from an older render), which card shows which row cannot
 *  be known, so every row of that identity gets null — never a guess. A row with
 *  no card at all (another pane is up) gets null too. */
export function pairRowsWithCards<C>(
  rows: readonly ShedIdentity[],
  cards: readonly C[],
  identityOf: (card: C) => ShedIdentity | null,
): (C | null)[] {
  // A key no host or name can forge: the two parts as a JSON pair.
  const keyOf = (id: ShedIdentity) => JSON.stringify([id.host, id.name]);
  const cardsBy = new Map<string, C[]>();
  for (const card of cards) {
    const id = identityOf(card);
    if (!id) continue;
    const k = keyOf(id);
    const list = cardsBy.get(k);
    if (list) list.push(card);
    else cardsBy.set(k, [card]);
  }
  const rowCount = new Map<string, number>();
  for (const r of rows) rowCount.set(keyOf(r), (rowCount.get(keyOf(r)) ?? 0) + 1);
  const taken = new Map<string, number>();
  return rows.map((r) => {
    const k = keyOf(r);
    const matching = cardsBy.get(k) ?? [];
    if (matching.length !== rowCount.get(k)) return null;
    const n = taken.get(k) ?? 0;
    taken.set(k, n + 1);
    return matching[n];
  });
}
