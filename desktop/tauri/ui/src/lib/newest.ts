/* Newest-wins sequencing for overlapping async reads.
 *
 * The lane panel re-READS its whole view whenever a `lane-event` says something
 * moved (it folds nothing itself — see `LanePanel.tsx`). Those reads are
 * batched, but batching only coalesces the *starts*: a burst still issues one
 * read while an earlier one is in flight, and two IPC round-trips have no
 * ordering guarantee between them. If the earlier one lands second it overwrites
 * a fresh transcript with a stale one, and — because the panel does not poll —
 * the screen stays wrong until the next unrelated frame.
 *
 * So a read commits only if nothing newer has already committed. It is a
 * counter rather than a queue on purpose: serializing would make the visible
 * state as old as the SLOWEST read in the burst, where discarding makes it as
 * new as the fastest one, and the reads are idempotent projections of one
 * locked view — there is nothing to preserve about an older answer.
 *
 * Failures obey the same rule. A read error is a property of the lane at the
 * moment it was read, so an older failure must not overwrite a newer success
 * (and a newer failure must be allowed to overwrite an older success). */

export type NewestWins = {
  /** Run one read. `commit` (or `fail`) is called only if no LATER `run` has
   *  already settled, and never after [`NewestWins.cancel`]. */
  run<T>(
    read: () => Promise<T>,
    commit: (value: T) => void,
    fail: (error: unknown) => void,
  ): Promise<void>;
  /** Stop committing anything, for good — the unmount. */
  cancel(): void;
};

export function newestWins(): NewestWins {
  /** Reads STARTED. Assigned before the read is even called, so the order is
   *  the order they were asked for rather than the order they answered in. */
  let issued = 0;
  /** The highest read that has already committed or failed. */
  let settled = 0;
  let cancelled = false;

  const guard: NewestWins = {
    cancel() {
      cancelled = true;
    },
    async run(read, commit, fail) {
      const mine = ++issued;
      let value;
      try {
        value = await read();
      } catch (error) {
        if (cancelled || mine <= settled) return;
        settled = mine;
        fail(error);
        return;
      }
      if (cancelled || mine <= settled) return;
      settled = mine;
      commit(value);
    },
  };
  return guard;
}
