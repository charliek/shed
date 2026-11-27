/* The modal dialogs' keyboard focus rules, kept free of the DOM so they are tested
   on their own (test/focusTrap.test.mjs). `components/dialog.tsx` applies them: on
   open focus moves into the dialog, Tab and Shift+Tab cycle inside it, and on close
   focus goes back to the control that opened it. */

/** Where a dialog puts focus when it opens, in this order:
 *  - `keep`: focus is already inside it (a control's `autoFocus` moved it there);
 *  - `marked`: the control marked `data-dialog-initial` (a confirmation's Cancel,
 *    its safe answer);
 *  - `body`: the first control in its body or footer;
 *  - `first`: the first control anywhere in it (the header's close button);
 *  - `dialog`: the dialog itself, which has no control to take it. */
export type InitialFocus = "keep" | "marked" | "body" | "first" | "dialog";

export function initialFocus(seen: {
  holdsFocus: boolean;
  hasMarked: boolean;
  hasBodyControl: boolean;
  hasControl: boolean;
}): InitialFocus {
  if (seen.holdsFocus) return "keep";
  if (seen.hasMarked) return "marked";
  if (seen.hasBodyControl) return "body";
  if (seen.hasControl) return "first";
  return "dialog";
}

/** Where a Tab press moves focus in a dialog with `count` controls in tab order,
 *  the focused one at `current` (-1 when focus is on none of them: on the dialog
 *  itself, or lost outside it). `back` is Shift+Tab. The answer is the index to
 *  focus, -1 for the dialog itself (it has no control), or null to let the press
 *  move focus as it would (to the next control, which is still inside). */
export function tabTarget(count: number, current: number, back: boolean): number | null {
  if (count <= 0) return -1;
  if (current < 0 || current >= count) return back ? count - 1 : 0;
  if (back && current === 0) return count - 1;
  if (!back && current === count - 1) return 0;
  return null;
}

/** Whether a closing dialog gives focus back to the control that opened it: that
 *  control is still in the document and is not the page itself, and focus is in the
 *  dialog or was lost with it (on the page itself, or nowhere). Focus that something
 *  else moved elsewhere on purpose stays there. */
export function shouldRestore(seen: {
  openerConnected: boolean;
  openerIsPage: boolean;
  focusInDialogOrLost: boolean;
}): boolean {
  return seen.openerConnected && !seen.openerIsPage && seen.focusInDialogOrLost;
}

/** The open dialogs. Only the one on top keeps Tab inside itself, so a dialog
 *  opened over another takes the keyboard while it is open and leaves it to the one
 *  below when it closes. "On top" is what the eye sees: `above(a, b)` says whether
 *  `a` is drawn over `b` (for dialogs at one layer, the later in the document). By
 *  default the newest is on top. */
export class DialogStack<T> {
  private open: T[] = [];

  constructor(private readonly above: (a: T, b: T) => boolean = () => true) {}

  push(d: T): void {
    this.remove(d);
    this.open.push(d);
  }

  remove(d: T): void {
    this.open = this.open.filter((x) => x !== d);
  }

  top(): T | null {
    let best: T | null = null;
    for (const d of this.open) {
      if (best === null || this.above(d, best)) best = d;
    }
    return best;
  }

  isTop(d: T): boolean {
    return this.top() === d;
  }

  get size(): number {
    return this.open.length;
  }
}
