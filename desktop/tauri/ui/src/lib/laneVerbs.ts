/* Which of the transcript panel's verb buttons exist, and which are live.
 *
 * Both answers come from ONE place: the session's capabilities as the lane's
 * stream last stated them (`lane.messages`' `capabilities`, never `lane.open`'s
 * answer — plan 025 §3.2), plus what the session is doing right now. A verb the
 * capabilities do not offer has no button at all (an affordance whose only
 * outcome is a refusal is worse than none); a verb they do offer is enabled
 * only while the session is Working, because that is the only time either verb
 * means anything — cancelling a session already waiting on you is a no-op the
 * agent refuses, and an interject outside a turn is refused the same way.
 *
 * Pure and dependency-free so it can be pinned on node's own test runner
 * (`test/laneVerbs.test.mjs`) — the panel's `lane.dump` report and its rendered
 * buttons both read it, so the two cannot disagree. */

/** The two capability flags this decides on — the shape of
 *  `LaneCapabilities` in `bridge.ts`, narrowed to what is read. */
export type VerbCapabilities = { cancel?: boolean; interject?: boolean };

/** One verb's affordance: is there a button, and can it be pressed. */
export type VerbGate = { shown: boolean; enabled: boolean };

/** Cancel and Interject, gated on the streamed capabilities and the activity.
 *  `capabilities` is `null` until the first seed carrying them has swapped
 *  into the view — and until then neither button exists. */
export function laneVerbs(
  capabilities: VerbCapabilities | null | undefined,
  activity: string,
): { cancel: VerbGate; interject: VerbGate } {
  const working = activity === "working";
  const gate = (offered: boolean | undefined): VerbGate => {
    const shown = offered === true;
    return { shown, enabled: shown && working };
  };
  return { cancel: gate(capabilities?.cancel), interject: gate(capabilities?.interject) };
}

/** What the panel says when a SEND's answer was lost (`outcome_unknown`, plan
 *  025 §3.3.4): the prompt may or may not have started a turn, it is never
 *  resent, and the typed text stays in the box — so the panel says plainly
 *  what to do before pressing Send again, and the person decides. */
export const SEND_OUTCOME_UNKNOWN =
  "outcome unknown: the connection to craze dropped; check the transcript before sending again";

/** The line a failed Send shows: the lost answer's own words for an
 *  `outcome_unknown`, the refusal's message for anything else. */
export function sendFailureText(failure: { code: string; message: string }): string {
  return failure.code === "outcome_unknown" ? SEND_OUTCOME_UNKNOWN : failure.message;
}
