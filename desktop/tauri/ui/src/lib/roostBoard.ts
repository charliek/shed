/* Per-target fetch bookkeeping for a board of roost cards.
 *
 * `useRoostBoard` fetches `roost.preview` for every target a pane shows, and
 * has to answer two questions per target on every render: does this one need a
 * fetch, and may an answer that just arrived reach the screen. Both were
 * inlined in the hook, and both were wrong — in two different ways, found by
 * two different reviewers on the same twenty lines:
 *
 *   1. The "needs a fetch" question was answered by "does this target have a
 *      generation counter", and counters were never pruned. A shed that
 *      stopped and started again therefore kept its counter, was skipped
 *      forever, and its card sat on `roost: probing…` with no button.
 *
 *   2. The "may it commit" question was answered by a `cancelled` flag local
 *      to the EFFECT RUN, which React sets on every deps change and not only
 *      on unmount. So merely ADDING a second card — `[A]` → `[A, B]`, no
 *      nonce bump — cancelled A's in-flight probe, while question 1 said A
 *      still had one and skipped starting a replacement. A's card was then
 *      stuck on `loading` permanently.
 *
 * The shared cause is granularity: supersession here is per TARGET, and both
 * answers were being given by per-render or per-target-lifetime state that
 * could not express that. So this module keeps the two apart on purpose:
 *
 *   - `gens` is the supersession token and is MONOTONIC — never pruned, never
 *     reset. Resetting a returning target's counter to zero would hand it the
 *     same number a probe still in flight from before it left is holding, and
 *     that stale answer would then be accepted as the fresh one.
 *   - `served` is "answered or in flight" and IS pruned with the board, which
 *     is what lets a returning target fetch again.
 *   - `unmounted` is the only thing that stops everything, and only the
 *     component's real unmount sets it.
 *
 * Same shape and same reason as `newest.ts`: pure, dependency-free and
 * therefore testable on node's own runner, with the hook's wiring reduced to a
 * few lines `tsc` type-checks. */

/** One fetch to start: the target, and the generation to quote back to
 *  [`RoostBoardGuard.accept`] when its answer lands. */
export type BoardFetch = { target: string; gen: number };

export type RoostBoardGuard = {
  /** Reconcile against the targets the pane shows now, and answer with the
   *  ones that need a `roost.preview`.
   *
   *  `forceAll` is the explicit refresh (a pane Refresh, or a just-settled
   *  bootstrap): it re-fetches even targets that already have an answer. A
   *  target dropped from `wanted` is forgotten, so if it comes back it fetches
   *  again. */
  sync(wanted: Iterable<string>, forceAll: boolean): BoardFetch[];
  /** Whether an answer for `target`, taken at `gen`, should reach the screen.
   *
   *  False once the component is gone, once the target has left the board, and
   *  once a LATER fetch for the same target has been started. */
  accept(target: string, gen: number): boolean;
  /** The component went away. Nothing commits after this. */
  unmount(): void;
  /** The targets currently considered answered-or-in-flight. For tests. */
  served(): string[];
};

export function roostBoardGuard(): RoostBoardGuard {
  /** Fetches STARTED per target, monotonic for the guard's whole life. */
  const gens: Record<string, number> = {};
  /** Targets with an answer or an in-flight fetch, pruned with the board. */
  const served = new Set<string>();
  let unmounted = false;

  return {
    sync(wanted, forceAll) {
      const live = new Set(wanted);
      for (const target of [...served]) if (!live.has(target)) served.delete(target);
      const starts: BoardFetch[] = [];
      for (const target of live) {
        // Re-fetching an unchanged target on every board-size change would
        // race it against its OWN prior in-flight probe (a real `ssh` reach to
        // the same host), and roost's reach layer refuses a second live
        // connection to itself. The explicit refresh accepts that cost.
        if (!forceAll && served.has(target)) continue;
        const gen = (gens[target] = (gens[target] ?? 0) + 1);
        served.add(target);
        starts.push({ target, gen });
      }
      return starts;
    },

    accept(target, gen) {
      if (unmounted) return false;
      // Left the board: its answer must not resurrect a card the pane no
      // longer shows.
      if (!served.has(target)) return false;
      // Superseded by a later fetch for the same target.
      return gens[target] === gen;
    },

    unmount() {
      unmounted = true;
    },

    served() {
      return [...served];
    },
  };
}
