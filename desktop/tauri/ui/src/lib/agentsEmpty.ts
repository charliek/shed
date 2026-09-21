/* What the Agents pane says when it is showing no rows.
 *
 * **Three states wear the same blank screen, and they are not the same news.**
 * Before S6 the distinction barely mattered: `rc.list` was a probe of running
 * sheds and an empty answer meant "nothing launched yet" whichever way it got
 * there. Since S6 the pane has exactly one row source — each host's own
 * `roost-session` — so an empty list is a specific claim ("there is no session
 * here to list"), and the offer beside it points at a specific fix (bootstrap
 * one, from the shed's card). Making that offer while the list is still loading,
 * or after the load FAILED, tells the user to go fix the wrong thing — and the
 * failure path was silent, because the fetch turned an invocation error into
 * empty arrays and the initial state was empty too.
 *
 * So the three are named here, and only the last one carries the offer:
 *
 *   - `loading` — nothing has answered yet. Not a claim about anything.
 *   - `failed`  — the list could not be read; the reason travels with it,
 *                 because "the backend is not up" and "that host refused" need
 *                 different fixes and the user can see neither from a blank pane.
 *   - `ready`   — the answer arrived and it was empty. THIS is the one that
 *                 means "no roost-session here", and the only one that offers
 *                 the bootstrap.
 *
 * Pure and dependency-free, in the `newest.ts` / `roostBoard.ts` shape: the copy
 * a person reads is the thing most worth a test, and a test of it should not
 * need React, a DOM or a Tauri runtime. `bridge.ts` re-exports it, the pane
 * renders it, and `agents.dump` reports it verbatim so the harness asserts the
 * words rather than a screenshot. */

/** Where the shared `rc.list` read has got to. */
export type AgentsLoad = "loading" | "failed" | "ready";

/** The one fact this module needs about a machine row: whether the pane could
 *  see it. Structural rather than an import of `MachineStatus`, so this module
 *  stays free of everything `bridge.ts` pulls in. */
export type EmptyMachine = { reachable: boolean };

/** The Agents pane's empty state, as rendered and as `agents.dump` reports it.
 *
 *  `state` is the machine-readable half — a cell asserts the STATE and spot-
 *  checks the words, rather than pinning three sentences it would then have to
 *  keep re-pinning. `action` is a label; `null` means there is nothing useful to
 *  offer about this particular emptiness. */
export type AgentsEmptyState = {
  state: "loading" | "failed" | "unreachable" | "empty";
  title: string;
  body: string;
  action: string | null;
};

/** The empty state for one (load, machines, error) triple.
 *
 *  Order matters and is deliberate: a pane that has not finished looking, or
 *  could not look, must not make a claim about what is running — so those two
 *  answer first, before any reading of the machine list (which is itself empty
 *  in both cases and would otherwise fall through to the bootstrap offer). */
export function agentsEmptyState(
  machines: EmptyMachine[],
  load: AgentsLoad,
  error: string | null,
): AgentsEmptyState {
  if (load === "loading") {
    return {
      state: "loading",
      title: "Loading agent sessions",
      body: "Reading the roost-session on each host…",
      action: null,
    };
  }
  if (load === "failed") {
    return {
      state: "failed",
      title: "Could not load agent sessions",
      // The reason, verbatim and in full: this is the only place it is on
      // screen, and a truncated transport error is a bug report nobody can act
      // on. An empty reason still gets a sentence rather than a dangling colon.
      body: error
        ? `The session list could not be loaded: ${error}`
        : "The session list could not be loaded, and the failure gave no reason.",
      action: null,
    };
  }
  // With machines configured but none reachable, "nothing is running" is a
  // claim this pane cannot make — it has not been able to look.
  const down = machines.filter((m) => !m.reachable).length;
  if (down > 0 && down === machines.length) {
    return {
      state: "unreachable",
      title: "Nothing to show",
      body: `${down === 1 ? "The configured machine is" : `All ${down} configured machines are`} unreachable, so this pane cannot see what is running — see the Machines pane for why.`,
      action: null,
    };
  }
  return {
    state: "empty",
    title: "No agent sessions",
    body: "Agent sessions live in a roost-session — on a machine, or inside a shed. A shed without one has no sessions to list; install and start it from the Sheds pane, then launch an agent here.",
    action: "Set up a roost-session",
  };
}
