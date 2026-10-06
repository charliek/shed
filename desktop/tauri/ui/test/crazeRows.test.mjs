/* The craze rows' pure rules (`src/lib/crazeRows.ts`, plan 025 §3.6.5), on
 * node's own test runner — the `laneVerbs.test.mjs` arrangement: plain `.mjs`
 * against the JS `npm test` emits from the TypeScript, no DOM, no React.
 *
 * The case marked CONTROL is the one plan 025's C9 control list names: a craze
 * row's End tab must close its TAB (`machine.kill {slug: String(tab_id)}`),
 * never send the row's slug — which is a craze hostId, not a roost tab id.
 *
 * The case marked REGRESSION is plan 025 live leg 1's finding: the transcript
 * the create sheet opens at once read its permission line off `lane.open`'s
 * row — the create's own, with no permission mode — and so never said "runs
 * tools without asking" for as long as it stayed open. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  canOpenTerminal,
  crazeDoingLine,
  crazeMachineNote,
  killTarget,
  laneHeader,
  permissionLine,
} from "../dist-test/crazeRows.js";

const crazeRow = (extra) => ({
  source: "craze",
  machine: "localhost",
  host: "machine:localhost",
  shed: "",
  slug: "a1b2c3d4e5f6",
  ...extra,
});

test("CONTROL: a craze row's End tab sends its tab id, never its hostId slug", () => {
  assert.deepEqual(killTarget(crazeRow({ tab_id: "7" })), {
    via: "machine",
    machine: "localhost",
    slug: "7",
  });
});

test("a craze row with no tab has nothing to end", () => {
  assert.equal(killTarget(crazeRow({})), null);
  assert.equal(killTarget(crazeRow({ tab_id: null })), null);
  assert.equal(killTarget(crazeRow({ tab_id: "7", machine: null })), null);
});

test("a roost row still ends by its slug, which IS its tab id", () => {
  assert.deepEqual(
    killTarget({ source: "roost", machine: "mini3", host: "machine:mini3", shed: "", slug: "5", tab_id: "5" }),
    { via: "machine", machine: "mini3", slug: "5" },
  );
  assert.deepEqual(
    killTarget({ source: "roost", machine: null, host: "srv", shed: "s1", slug: "5" }),
    { via: "shed", host: "srv", shed: "s1", slug: "5" },
  );
});

test("the doing line: doing, else the idle session's last reply, dimmed", () => {
  assert.deepEqual(crazeDoingLine(crazeRow({ doing: "editing main.rs", activity: "working" })), {
    text: "editing main.rs",
    dimmed: false,
  });
  assert.deepEqual(crazeDoingLine(crazeRow({ last_reply: "done.", activity: "idle" })), {
    text: "done.",
    dimmed: true,
  });
  assert.equal(crazeDoingLine(crazeRow({ last_reply: "done.", activity: "working" })), null);
  assert.equal(crazeDoingLine(crazeRow({})), null);
});

test("the machines-pane note: too old speaks, not installed says nothing", () => {
  assert.equal(
    crazeMachineNote({ state: "offline", cause: "too_old" }),
    "craze on this machine is too old for shed; update it",
  );
  assert.equal(crazeMachineNote({ state: "absent", cause: "not_installed" }), null);
  assert.equal(crazeMachineNote({ state: "live" }), null);
  assert.equal(crazeMachineNote(null), null);
});

test("the permission line says what bypass means", () => {
  assert.equal(permissionLine("bypass"), "runs tools without asking");
  assert.equal(permissionLine("prompt"), "asks before running tools");
  assert.equal(permissionLine("careful"), "permissions: careful");
  assert.equal(permissionLine(""), null);
  assert.equal(permissionLine(null), null);
});

// The create-time row a just-created craze session is opened on (live leg 1's
// `lane.open` answer): no permission mode, no model, approximate.
const createdRow = { id: "a998d6431654", title: "w-new", cwd: "/tmp/p025h1/w-new", activity: "unknown", approximate: true };
// The same session's row as the lane's stream states it once the attach's info
// document is in.
const liveRow = { ...createdRow, activity: "idle", approximate: false, permission_mode: "bypass" };

test("REGRESSION: the header reads the LIVE row's permission mode, not the row the lane was opened on", () => {
  assert.deepEqual(laneHeader(createdRow, liveRow), {
    title: "w-new",
    cwd: "/tmp/p025h1/w-new",
    permission: "runs tools without asking",
  });
});

test("the header falls back to lane.open's row only until a seed has swapped one in", () => {
  const opened = { ...createdRow, permission_mode: "prompt" };
  for (const live of [null, undefined]) {
    assert.deepEqual(laneHeader(opened, live), {
      title: "w-new",
      cwd: "/tmp/p025h1/w-new",
      permission: "asks before running tools",
    });
  }
  // Once there is a live row it wins WHOLE — including about what it does not
  // say, and about a title the stream renamed.
  assert.deepEqual(laneHeader(opened, { ...createdRow, title: "renamed" }), {
    title: "renamed",
    cwd: "/tmp/p025h1/w-new",
    permission: null,
  });
  assert.deepEqual(laneHeader(null, null), { title: "", cwd: "", permission: null });
});

test("Open in terminal: a headless craze row only", () => {
  assert.equal(canOpenTerminal(crazeRow({})), true);
  assert.equal(canOpenTerminal(crazeRow({ tab_id: "" })), true);
  assert.equal(canOpenTerminal(crazeRow({ tab_id: "7" })), false, "a tab: End tab instead");
  assert.equal(canOpenTerminal(crazeRow({ machine: null })), false);
  assert.equal(canOpenTerminal({ source: "roost", machine: "m", host: "h", shed: "", slug: "5" }), false);
});
