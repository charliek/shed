/* The delivery record, badge and provider drafts (`src/lib/delivery.ts`, plan 039
 * A7), on node's own test runner — the `sources.test.mjs` arrangement. */
import test from "node:test";
import assert from "node:assert/strict";

import {
  COMMAND_KEPT,
  configFromDraft,
  deliveryBadge,
  deliverySummary,
  draftFromConfig,
  edit,
  formatSize,
  onlyText,
  provisionResult,
  readDelivery,
  redactedConfig,
  stepText,
} from "../dist-test/delivery.js";

const PARTIAL = {
  state: "partial",
  steps: {
    stage1: { state: "done" },
    clone: { state: "done" },
    stage2: { state: "failed", reason: "write_failed" },
    install: { state: "owed" },
  },
  last: {
    at: "2026-11-23T10:00:00Z", reason: "provision", outcome: "accepted",
    source: { machine_id: "a".repeat(32), machine_name: "laptop" },
    problems: [{ provider: "ssh-key", code: "passphrase", message: "the key has a passphrase" }],
  },
  items: [
    { name: "github", provider: "github", stage: 1, kind: "file", base: "home",
      target: ".config/gh/hosts.yml", fingerprint: "fp", written_at: "2026-11-23T10:00:02Z",
      expires_at: "2026-11-24T10:00:00Z", result: "ok" },
    { name: "bad" },
    "nope",
  ],
  expired: ["github"],
  owed: ["install"],
};

test("a record decodes defensively", () => {
  assert.equal(readDelivery(null), null);
  assert.equal(readDelivery("delivered"), null);
  assert.equal(readDelivery({}), null);
  assert.equal(readDelivery({ state: "" }), null);
  const r = readDelivery(PARTIAL);
  assert.equal(r.state, "partial");
  assert.deepEqual(r.steps.map((s) => s.name), ["stage1", "clone", "stage2", "install"]);
  assert.equal(stepText(r.steps[2]), "failed (write_failed)");
  assert.equal(stepText(r.steps[3]), "owed");
  assert.deepEqual(r.items.map((i) => i.name), ["github", "bad"]);
  assert.equal(r.items[1].target, "");
  assert.equal(r.last.problems[0].code, "passphrase");
  assert.equal(r.last.source.machine_name, "laptop");
  assert.equal(r.withheld, false);
});

test("only the server's marker withholds a record", () => {
  for (const state of ["delivered", "partial", "not_delivered", "none", "not_applicable"]) {
    const r = readDelivery({ state, withheld: true });
    assert.equal(r.withheld, true, state);
    assert.match(deliverySummary(r), /Only the shed's user/);
    // A state alone is the user's own record (a shed from before delivery).
    assert.equal(readDelivery({ state }).withheld, false, state);
  }
  assert.equal(readDelivery({ state: "partial", withheld: "yes" }).withheld, false);
});

test("the badge names the state, expired items and owed steps", () => {
  assert.deepEqual(deliveryBadge(readDelivery(PARTIAL)), {
    label: "partial", tone: "attention", extras: ["1 expired", "owed: install"],
  });
  const cases = [
    ["delivered", "delivered", "ok"],
    ["not_delivered", "not delivered", "danger"],
    ["none", "no delivery", "muted"],
    ["not_applicable", "not applicable", "muted"],
    ["later_word", "later word", "muted"],
  ];
  for (const [state, label, tone] of cases) {
    const b = deliveryBadge(readDelivery({ state }));
    assert.equal(b.label, label);
    assert.equal(b.tone, tone);
    assert.deepEqual(b.extras, []);
  }
});

test("sizes", () => {
  assert.equal(formatSize(0), "0 B");
  assert.equal(formatSize(1023), "1023 B");
  assert.equal(formatSize(1536), "1.5 KB");
  assert.equal(formatSize(20 * 1024 * 1024), "20 MB");
  assert.equal(formatSize(-1), "");
});

test("a provision's result line", () => {
  const ok = provisionResult({
    ok: true, delivery: { state: "delivered" },
    progress: [{ message: "stage1: delivered" }, { message: "no source", warning: true }],
  });
  assert.deepEqual(ok, { ok: true, text: "Delivered again: delivered.", warnings: ["no source"] });
  const bad = provisionResult({ ok: false, error: { code: "SHED_NOT_RUNNING", message: "shed x is not running" } });
  assert.equal(bad.ok, false);
  assert.equal(bad.text, "Could not deliver again: shed x is not running");
  assert.equal(provisionResult(null).ok, false);
});

// Every config a valid delivery.toml can hold, as `delivery.settings` shows it, saved
// back unchanged with the switch's enabled.
const ROUND_TRIPS = [
  ["ssh-key", { enabled: true, key_file: "~/.ssh/id_ed25519", sign_commits: false,
    only: { servers: ["mock"], repos: ["example/*"] } }],
  ["ssh-key", { enabled: false, key_file: " ~/.ssh/with space ", sign_commits: true }],
  ["github", { enabled: true, gh: "/opt/gh, the program" }],
  ["api-keys", { enabled: false, craze_providers: true,
    env_files: ["~/keys,work.env", "~/a b.env", "~/x\ny.env"] }],
  ["envsecrets-login", { enabled: true, passphrase_file: "~/p", passphrase_command: COMMAND_KEPT }],
  ["files", { enabled: true, file: [
    { from: "~/.npmrc", to: ".npmrc", base: "home" },
    { from: "~/w,1.env", to: ".env", base: "project", mode: "0400", only: { repos: ["example/w"] } },
  ], only: { backends: ["vz", "remote"] } }],
];

test("an unchanged draft saves back exactly what it was read from", () => {
  for (const [provider, cfg] of ROUND_TRIPS) {
    assert.deepEqual(configFromDraft(provider, draftFromConfig(provider, cfg), cfg.enabled), cfg, provider);
  }
});

test("each list entry is edited alone, its string kept exactly", () => {
  let d = draftFromConfig("api-keys", { enabled: true, env_files: ["~/a,b.env"] });
  assert.deepEqual(d.lists.env_files, ["~/a,b.env"]);
  d = edit.add(d, "env_files");
  d = edit.entry(d, "env_files", 1, " ~/two\nlines.env ");
  assert.deepEqual(configFromDraft("api-keys", d, true).env_files, ["~/a,b.env", " ~/two\nlines.env "]);
  d = edit.remove(d, "env_files", 0);
  assert.deepEqual(d.lists.env_files, [" ~/two\nlines.env "]);
  // A blank new entry is not a setting, and is left out.
  d = edit.add(d, "env_files");
  assert.deepEqual(configFromDraft("api-keys", d, true).env_files, [" ~/two\nlines.env "]);
  let o = edit.add(draftFromConfig("github", null), "only.repos");
  o = edit.entry(o, "only.repos", 0, "a,b/*");
  assert.deepEqual(configFromDraft("github", o, false).only, { repos: ["a,b/*"] });
  assert.equal(edit.entry(o, "only.users", 0, "x"), o, "an unknown list changes nothing");
  assert.equal(edit.entry(o, "only.repos", 5, "x"), o, "an entry out of range changes nothing");
});

test("a stored command is kept unseen, replaced exactly, or cleared", () => {
  const d = draftFromConfig("envsecrets-login", { enabled: true, passphrase_command: COMMAND_KEPT });
  assert.deepEqual(d.command, { keep: true });
  assert.equal(JSON.stringify(d).includes("printf"), false);
  // An unrelated change keeps it.
  const kept = edit.text(d, "passphrase_file", "~/p");
  assert.equal(configFromDraft("envsecrets-login", kept, true).passphrase_command, COMMAND_KEPT);
  // Replaced: every argument as typed, an empty one and a comma included.
  let r = edit.replaceCommand(d);
  r = edit.entry(r, "passphrase_command", 0, "printf");
  r = edit.add(r, "passphrase_command");
  r = edit.add(r, "passphrase_command");
  r = edit.entry(r, "passphrase_command", 2, "SENTINEL a,b\nc");
  assert.deepEqual(configFromDraft("envsecrets-login", r, true).passphrase_command, ["printf", "", "SENTINEL a,b\nc"]);
  assert.equal(redactedConfig("envsecrets-login", r, true).passphrase_command, "replaced (3 arguments)");
  assert.equal(JSON.stringify(redactedConfig("envsecrets-login", r, true)).includes("SENTINEL"), false);
  // Back to the stored one, or none at all.
  assert.equal(configFromDraft("envsecrets-login", edit.keepCommand(r), true).passphrase_command, COMMAND_KEPT);
  assert.equal("passphrase_command" in configFromDraft("envsecrets-login", edit.clearCommand(d), true), false);
  // With none stored there is nothing to keep.
  const none = draftFromConfig("envsecrets-login", { enabled: true });
  assert.deepEqual(none.command, { keep: false, args: [] });
  assert.equal(edit.keepCommand(none), none);
  // Anything but the marker (a view never sends an argv) is never shown back.
  assert.deepEqual(draftFromConfig("envsecrets-login", { passphrase_command: ["SENTINEL"] }).command,
    { keep: false, args: [] });
});

test("files entries are edited field by field", () => {
  let d = edit.add(draftFromConfig("files", { enabled: true }), "files");
  d = edit.file(d, 0, "from", "~/a, b");
  d = edit.file(d, 0, "to", "a");
  d = edit.file(d, 0, "base", "project");
  assert.deepEqual(configFromDraft("files", d, true).file, [{ from: "~/a, b", to: "a", base: "project" }]);
  assert.equal(edit.file(d, 3, "to", "x"), d);
});

test("not configured: craze's providers on by default", () => {
  const none = draftFromConfig("api-keys", null);
  assert.equal(none.bools.craze_providers, true);
  assert.deepEqual(configFromDraft("api-keys", none, false), { enabled: false, craze_providers: true });
});

test("an only rule in one line", () => {
  assert.equal(onlyText(null), "");
  assert.equal(onlyText({ servers: ["a", "b"], repos: ["x/*"] }), "servers: a, b · repos: x/*");
});
