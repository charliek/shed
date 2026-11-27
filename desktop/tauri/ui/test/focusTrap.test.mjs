/* The modal dialogs' keyboard focus rules (`src/lib/focusTrap.ts`), on node's own
 * test runner — the `laneVerbs.test.mjs` arrangement. `components/dialog.tsx`
 * applies them to every DialogShell. */
import test from "node:test";
import assert from "node:assert/strict";

import { DialogStack, initialFocus, shouldRestore, tabTarget } from "../dist-test/focusTrap.js";

const seen = (over) => ({ holdsFocus: false, hasMarked: false, hasBodyControl: false, hasControl: false, ...over });

test("a control that autoFocus moved focus to keeps it", () => {
  assert.equal(initialFocus(seen({ holdsFocus: true, hasMarked: true, hasBodyControl: true, hasControl: true })), "keep");
});

test("a confirmation starts on its marked safe answer, before the first control", () => {
  assert.equal(initialFocus(seen({ hasMarked: true, hasBodyControl: true, hasControl: true })), "marked");
});

test("otherwise the first control in the body, before the header's close", () => {
  assert.equal(initialFocus(seen({ hasBodyControl: true, hasControl: true })), "body");
  assert.equal(initialFocus(seen({ hasControl: true })), "first");
});

test("a dialog with no control takes focus itself", () => {
  assert.equal(initialFocus(seen({})), "dialog");
});

test("Tab past the last control wraps to the first, Shift+Tab before the first to the last", () => {
  assert.equal(tabTarget(3, 2, false), 0);
  assert.equal(tabTarget(3, 0, true), 2);
});

test("Tab between controls is left to the browser", () => {
  assert.equal(tabTarget(3, 0, false), null);
  assert.equal(tabTarget(3, 1, false), null);
  assert.equal(tabTarget(3, 1, true), null);
  assert.equal(tabTarget(3, 2, true), null);
});

test("focus on no control (the dialog itself, or lost) comes back in at the near end", () => {
  assert.equal(tabTarget(3, -1, false), 0);
  assert.equal(tabTarget(3, -1, true), 2);
  assert.equal(tabTarget(3, 7, false), 0);
});

test("one control: Tab and Shift+Tab stay on it", () => {
  assert.equal(tabTarget(1, 0, false), 0);
  assert.equal(tabTarget(1, 0, true), 0);
});

test("no control: Tab stays on the dialog itself", () => {
  assert.equal(tabTarget(0, -1, false), -1);
  assert.equal(tabTarget(0, -1, true), -1);
});

test("a closing dialog gives focus back to its opener when focus was in it or lost", () => {
  const ok = { openerConnected: true, openerIsPage: false, focusInDialogOrLost: true };
  assert.equal(shouldRestore(ok), true);
  assert.equal(shouldRestore({ ...ok, openerConnected: false }), false, "an opener gone from the document");
  assert.equal(shouldRestore({ ...ok, openerIsPage: true }), false, "nothing held focus before");
  assert.equal(shouldRestore({ ...ok, focusInDialogOrLost: false }), false, "focus moved elsewhere on purpose");
});

test("the newest open dialog is the top one; closing it hands the top back", () => {
  const s = new DialogStack();
  assert.equal(s.top(), null);
  s.push("outer");
  s.push("inner");
  assert.equal(s.top(), "inner");
  assert.equal(s.isTop("outer"), false);
  s.remove("inner");
  assert.equal(s.isTop("outer"), true);
  s.remove("outer");
  assert.equal(s.size, 0);
});

test("a dialog pushed again (a remount) is listed once, on top", () => {
  const s = new DialogStack();
  s.push("a");
  s.push("b");
  s.push("a");
  assert.equal(s.size, 2);
  assert.equal(s.top(), "a");
  s.remove("a");
  assert.equal(s.top(), "b");
});
