/* shed desktop — the roost bootstrap affordance (plan 019 §3.6, C8): the status
 * line + plan-matrix button a machine/shed card shows, the consent dialog, and
 * the settle toast. Rendering only — the data (`RoostCardState`, the copy
 * helpers) lives in `@/lib/roost`; this file is the one place that turns it
 * into DOM, so the six-row matrix is read off the wire everywhere it appears. */
import { useEffect } from "react";
import { Download, RefreshCw, X } from "lucide-react";
import { cn } from "@/lib/utils";
import { Scrim, DialogShell, dialogBtnSecondary } from "@/components/dialog";
import {
  roostButtonLabel, roostStatusLine, roostConsentCopy,
  renderedText, reportRoostConsent, reportToast,
  type RoostCardState, type RoostPreview, type RoostToast,
} from "@/lib/roost";

/** The `data-*` marks the two self-reporting components are read back through —
 *  the scrim's `mark` (`Scrim` stamps `data-<mark>`) and the toast's own attr. */
const CONSENT_MARK = "[data-roost-consent]";
const TOAST_MARK = "[data-toast]";

/** The roost sub-line under a machine/shed card: a status word, a button when
 *  the plan is actionable, nothing at all before the first probe answers.
 *
 *  `busyDetail` (non-null while THIS target's bootstrap is running) replaces
 *  both with the indeterminate phrase (plan 019 §3.6's `detail`) — the row is
 *  where a person watching that card sees progress, not a modal that could be
 *  dismissed or navigated away from. */
export function RoostLine({
  target,
  state,
  busyDetail,
  onOpenConsent,
}: {
  target: string;
  state: RoostCardState | undefined;
  busyDetail: string | undefined;
  onOpenConsent: (target: string, preview: RoostPreview) => void;
}) {
  const line = "mt-2 flex items-center gap-2 border-t border-shed-border pt-2 font-mono text-[11.5px]";
  if (busyDetail) {
    return (
      <div className={line} data-roost={target}>
        <RefreshCw size={12} className="flex-none animate-spin text-shed-text-muted" />
        <span className="text-shed-text-muted">{busyDetail}</span>
      </div>
    );
  }
  if (!state || (state.loading && !state.preview)) {
    return (
      <div className={line} data-roost={target}>
        <span className="text-shed-text-muted">roost: probing…</span>
      </div>
    );
  }
  if (state.error) {
    return (
      <div className={line} data-roost={target}>
        <span className="text-shed-text-muted">roost: {state.error}</span>
      </div>
    );
  }
  if (!state.preview) return null;
  const button = roostButtonLabel(state.preview);
  return (
    <div className={line} data-roost={target}>
      <span className="min-w-0 flex-1 truncate text-shed-text-muted">{roostStatusLine(state.preview)}</span>
      {button && (
        <button
          onClick={() => onOpenConsent(target, state.preview!)}
          className="hbtn flex-none rounded-[7px] px-2.5 py-1 text-[11.5px] font-semibold"
          style={{ background: "var(--shed-accent-subtle)", color: "var(--shed-accent)", border: "1px solid var(--shed-accent-border)" }}
        >
          {button}
        </button>
      )}
    </div>
  );
}

/** The consent dialog (plan 019 §3.5): what / where / from where / the
 *  hook-wiring sentence, and — for an Update — the backup note. Nothing is
 *  downloaded before `onConfirm` runs; the confirmation carries the probe's
 *  fingerprint (`preview.probe.fingerprint`), which `roost.bootstrap`
 *  re-checks host-side. Closing here does not cancel a run in flight — there
 *  is no cancel op on the wire (`RoostHosts::bootstrap` has none to call) — so
 *  Confirm both fires the request and dismisses the card back to its row,
 *  which is where progress shows next.
 *
 *  **It reports what it rendered** (`reportRoostConsent` → `roost_consent.dump`),
 *  the `LanePanel`/`reportLane` rule — including the card's own DOM text, so a
 *  cell asserting the pinned hook-wiring sentence is asserting about the screen.
 *  Reported from HERE and not from the state that opens it, because a report made
 *  beside this JSX would go on answering if the JSX were deleted. */
export function RoostConsentDialog({
  preview,
  onCancel,
  onConfirm,
}: {
  preview: RoostPreview;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const copy = roostConsentCopy(preview);
  // Keyed on the VALUE, so a parent re-render does not re-report; `rendered` is
  // read in the effect (after the commit) rather than during render, where the
  // document would still hold the previous frame.
  const key = JSON.stringify({ target: preview.target, ...copy });
  useEffect(() => {
    reportRoostConsent({ ...JSON.parse(key), rendered: renderedText(CONSENT_MARK) });
  }, [key]);
  useEffect(() => () => reportRoostConsent(null), []);
  return (
    <Scrim onClose={onCancel} mark="roost-consent">
      <DialogShell icon={Download} title={`${copy.action} roost-session`} sub={copy.what} onClose={onCancel} width={520}>
        <div className="flex flex-col gap-3 font-mono text-[12.5px] leading-relaxed text-shed-text-secondary">
          <div><span className="text-shed-text-muted">Where: </span>{copy.where}</div>
          <div><span className="text-shed-text-muted">From: </span>{copy.from}</div>
          <div className="rounded-md bg-shed-inset px-3 py-2 text-shed-text-secondary">{copy.hooks}</div>
          {copy.backup && (
            <div className="rounded-md bg-shed-inset px-3 py-2 text-shed-text-secondary">{copy.backup}</div>
          )}
        </div>
        <div className="flex items-center justify-between pt-1">
          <button onClick={onCancel} className={dialogBtnSecondary}>Cancel</button>
          <button
            onClick={onConfirm}
            className="hbtn inline-flex items-center gap-2 rounded-[9px] px-[22px] py-2.5 text-[14px] font-semibold"
            style={{ background: "var(--shed-accent)", color: "var(--shed-accent-fg)", border: "none" }}
          >
            <Download size={16} /> {copy.action}
          </button>
        </div>
      </DialogShell>
    </Scrim>
  );
}

/** A single settle toast (plan 019 §3.6): the PATH warning + hook results a
 *  bootstrap ends with, or a failure's own pinned message. Bottom-right,
 *  self-dismissing (the caller owns the timer); every line is backend copy,
 *  verbatim — this only picks the tone's color.
 *
 *  **It reports what it rendered** (`reportToast` → `toast.dump`), for
 *  [`RoostConsentDialog`]'s reason: a toast reported from the state that holds it
 *  would keep answering with lines nobody was shown. */
export function Toast({ toast, onDismiss }: { toast: RoostToast; onDismiss: () => void }) {
  const tone = toast.tone === "error" ? "var(--shed-danger)" : toast.tone === "warn" ? "var(--shed-warn-fg)" : "var(--shed-ok)";
  const key = JSON.stringify(toast);
  useEffect(() => {
    reportToast({ ...(JSON.parse(key) as RoostToast), rendered: renderedText(TOAST_MARK) });
  }, [key]);
  useEffect(() => () => reportToast(null), []);
  return (
    <div
      data-toast=""
      className={cn(
        "fixed bottom-5 right-5 z-[60] flex max-w-[420px] items-start gap-2.5 rounded-[12px] border px-4 py-3 shadow-shed",
      )}
      style={{ background: "var(--shed-surface)", borderColor: "var(--shed-border)", animation: "dialog-in .18s cubic-bezier(.2,.8,.2,1)" }}
    >
      <span className="mt-0.5 h-2 w-2 flex-none rounded-full" style={{ background: tone }} />
      <div className="min-w-0 flex-1 font-mono text-[12px] leading-relaxed text-shed-text-secondary">
        {toast.lines.map((l, i) => <div key={i}>{l}</div>)}
      </div>
      <button onClick={onDismiss} title="Dismiss" className="hlink flex-none text-shed-text-muted">
        <X size={14} />
      </button>
    </div>
  );
}
