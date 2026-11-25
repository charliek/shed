/* The Credentials view's view-model (`src/lib/credentials.ts`, plan 040 §3.9), on
 * node's own test runner — the `sources.test.mjs` arrangement. The forms are drawn from
 * `fixtures/describe.json`, which a Tauri crate test holds equal to `delivery.describe`. */
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import {
  COMMAND_KEPT,
  EMPTY_CHECK,
  checkErrorText,
  configErrorText,
  checkNote,
  checkRequest,
  checkRuns,
  sameRuns,
  checkResult,
  configFromDraft,
  draftFromConfig,
  edit,
  fieldControl,
  formatSize,
  machineFields,
  missing,
  onlyOf,
  outcomeText,
  pausedErrorText,
  readDescriptors,
  recentRows,
  redactedConfig,
  refusalCode,
  refusalText,
  rowErrorText,
  saveLabel,
  scopeOf,
  scopeWords,
  serverTicks,
  switched,
  whatView,
  whenText,
  whereRowOf,
  whereRowOfMove,
  whereStatusOf,
  whereView,
} from "../dist-test/credentials.js";

const DESCS = readDescriptors(JSON.parse(readFileSync(new URL("./fixtures/describe.json", import.meta.url), "utf8")));
const D = Object.fromEntries(DESCS.map((d) => [d.name, d]));
const ME = { id: "0".repeat(32), name: "laptop" };
const DESK = { id: "f".repeat(32), name: "desk" };

/* ---- 1. Where ---------------------------------------------------------------- */

const listRow = (server, kind, extra = {}) => ({
  server, kind, machine: null, connected: false, url: null, state: null, stream: null,
  paused: false, busy: false, unreachable: false, error: null, error_message: null, ...extra,
});

test("every kind of sources.list row reads, and says who answers", () => {
  const st = whereStatusOf({
    servers: [
      listRow("here", "this_machine", { machine: ME, connected: true, stream: { state: "connected" } }),
      listRow("desk", "other_machine", { machine: { id: DESK.id, name: "desk‮\u0007x" } }),
      listRow("account", "service", { url: "https://accounts.example.test" }),
      listRow("paused", "service", { url: "https://accounts.example.test", state: "not_delivering" }),
      listRow("fresh", "nobody"),
      listRow("stopped", "nobody", { paused: true }),
      listRow("old", "unsupported"),
      listRow("locked", "unknown", { error: "forbidden", error_message: "SERVER TEXT" }),
      { kind: "nobody" },
    ],
    this_machine: ME,
    this_machine_error: null,
    paused_error: null,
  });
  assert.equal(st.rows.length, 8, "a row with no server is dropped");
  const v = Object.fromEntries(st.rows.map((r) => [r.host, whereView(r, { moving: false, canUse: !r.this_machine, refusal: null })]));
  assert.equal(v.here.who, "This machine answers here");
  assert.equal(v.here.detail, "connected");
  assert.deepEqual(v.here.actions[0], { id: "unbind", label: "Stop answering from here", enabled: true });
  assert.equal(v.desk.who, "Another machine answers here:");
  assert.equal(v.desk.name, "deskx", "a name from elsewhere is cleaned");
  assert.deepEqual(v.desk.actions[0], { id: "use", label: "Answer from here", enabled: true });
  assert.equal(v.account.who, "The account service answers here");
  assert.equal(v.account.url, "https://accounts.example.test");
  assert.deepEqual(v.account.actions, [], "no action where the account service answers");
  assert.equal(v.account.notDelivering, false);
  assert.equal(v.paused.notDelivering, true);
  assert.equal(v.paused.detail, "It is not delivering to this server.");
  assert.equal(v.fresh.who, "Nobody answers here");
  assert.equal(v.stopped.detail, "This machine stopped answering here.");
  assert.deepEqual(v.old.actions, []);
  assert.equal(v.locked.kind, "unknown");
  assert.equal(v.locked.error, "The server does not let this client see who answers.");
  assert.equal(JSON.stringify(v).includes("SERVER TEXT"), false, "never the server's words");
});

test("a row kept while the server cannot say, and a move or stop in flight", () => {
  const r = whereRowOf(listRow("a", "other_machine", { machine: DESK }));
  const gone = whereView({ ...r, unreachable: true }, { moving: false, canUse: true, refusal: null });
  assert.equal(gone.detail, "not connected · cannot reach the server");
  assert.equal(gone.name, "desk");
  const busy = whereView({ ...r, busy: true }, { moving: false, canUse: true, refusal: null });
  assert.equal(busy.retrying, true);
  const moving = whereView({ ...r, kind: "this_machine", this_machine: true }, { moving: true, canUse: false, refusal: null });
  assert.deepEqual(moving.actions[0], { id: "unbind", label: "Switching…", enabled: false });
  const stopping = whereView({ ...r, kind: "nobody", this_machine: false, source: null, paused: true },
    { moving: true, canUse: false, refusal: null });
  assert.deepEqual(stopping.actions[0], { id: "use", label: "Stopping…", enabled: false });
  const noIdentity = whereView(r, { moving: false, canUse: false, refusal: null });
  assert.equal(noIdentity.actions[0].enabled, false);
  const refused = whereView(r, { moving: false, canUse: true, refusal: "no" });
  assert.equal(refused.error, "no");
});

test("a pause shows on every kind of row, and an unreachable server has its own", () => {
  const other = whereRowOf(listRow("a", "other_machine", { machine: DESK, connected: true, paused: true }));
  const v = whereView(other, { moving: false, canUse: true, refusal: null });
  assert.equal(v.paused, true);
  assert.equal(v.detail, "connected · this machine stopped answering here");
  const svc = whereView(whereRowOf(listRow("s", "service", { url: "https://x.example.test", paused: true })),
    { moving: false, canUse: true, refusal: null });
  assert.equal(svc.detail, "This machine stopped answering here.");
  const nobody = whereView(whereRowOf(listRow("n", "nobody", { paused: true })), { moving: false, canUse: true, refusal: null });
  assert.equal(nobody.detail, "This machine stopped answering here.", "said once");
  const off = whereRowOf(listRow("o", "unknown", { unreachable: true, error: "unreachable" }));
  const u = whereView(off, { moving: false, canUse: true, refusal: null });
  assert.equal(u.who, "This server cannot be reached");
  assert.equal(u.detail, "Nothing is known of it yet.");
  assert.deepEqual(serverTicks([off], null), ["o"], "its scope tick is offered");
});

test("an uncertain stop can be undone from the view; an unreachable server offers nothing", () => {
  const bound = whereRowOf(listRow("a", "this_machine", { machine: ME, paused: true }));
  const v = whereView(bound, { moving: false, canUse: true, refusal: null });
  assert.deepEqual(v.actions.map((a) => [a.id, a.label, a.enabled]),
    [["use", "Answer from here", true], ["unbind", "Stop answering from here", true]]);
  // Negative control: answering here, not paused, offers only the stop.
  const live = whereView(whereRowOf(listRow("a", "this_machine", { machine: ME })), { moving: false, canUse: false, refusal: null });
  assert.deepEqual(live.actions.map((a) => a.id), ["unbind"]);
  const gone = whereView({ ...whereRowOf(listRow("d", "other_machine", { machine: DESK })), unreachable: true },
    { moving: false, canUse: true, refusal: null });
  assert.deepEqual(gone.actions, []);
});

test("a shed config that cannot be read keeps the rows it last listed, and says so", () => {
  const st = whereStatusOf({ servers: [listRow("a", "nobody")], this_machine: ME, config_error: "unreadable" });
  assert.equal(st.config_error, "unreadable");
  assert.equal(st.rows.length, 1, "the last servers, never dropped");
  assert.match(configErrorText(st.config_error), /cannot be read, so these are the servers it last listed/);
  assert.match(refusalText("config_unreadable", "a", "use"), /cannot be read/);
  // Negative control: a config read fine says nothing.
  assert.equal(configErrorText(whereStatusOf({ servers: [], this_machine: ME }).config_error), null);
});

test("a pause file that cannot be written is not one that cannot be read", () => {
  assert.equal(pausedErrorText(null), null);
  const write = pausedErrorText("write_failed");
  assert.match(write, /could not be saved/);
  assert.match(write, /answers wherever it has not stopped/);
  assert.doesNotMatch(write, /answers on none/);
  // Negative control: a load failure still says every server is paused.
  for (const e of ["unreadable", "unparseable", "unknown_version"]) {
    assert.match(pausedErrorText(e), /answers on none of them/, e);
  }
});

test("a move's answer is this machine's row", () => {
  const row = whereRowOfMove({ host: "a", supported: true, source: ME, this_machine: true, connected: false }, "a");
  assert.equal(row.kind, "this_machine");
  assert.equal(whereView(row, { moving: false, canUse: false, refusal: null }).detail, "not connected yet");
});

test("refusals are fixed words by code, never the server's", () => {
  assert.equal(refusalCode({ code: "forbidden", message: "SERVER TEXT" }), "forbidden");
  assert.equal(refusalCode("a string"), "action_failed");
  assert.equal(refusalText("forbidden", "mock", "use"), "mock does not let this client change its credential source.");
  assert.match(refusalText("source_is_service", "mock", "unbind"), /account service answers for you on mock/);
  assert.match(refusalText("rebound", "mock", "unbind"), /stays stopped here/);
  assert.match(refusalText("anything else", "mock", "use"), /could not confirm the move/);
  assert.match(refusalText("anything else", "mock", "unbind"), /could not confirm the stop/);
  for (const c of ["unreachable", "busy", "unauthorized", "forbidden", "source_is_service", "refused", "x"]) {
    assert.ok(rowErrorText(c).length > 0, c);
  }
});

/* ---- 2. What ----------------------------------------------------------------- */

test("the descriptors read, and each machine field has its control", () => {
  assert.deepEqual(DESCS.map((d) => d.name), ["ssh-key", "github", "api-keys", "envsecrets-login", "files"]);
  const ctl = (p) => Object.fromEntries(machineFields(D[p]).map((f) => [f.key, fieldControl(f)]));
  assert.deepEqual(ctl("ssh-key"), { key_file: "text", sign_commits: "switch" });
  assert.deepEqual(ctl("envsecrets-login"), { passphrase_file: "text", passphrase_command: "command" });
  assert.deepEqual(ctl("api-keys"), { craze_providers: "switch", env_files: "list" });
  assert.deepEqual(ctl("files"), { file: "entries" });
});

// Every config a valid delivery.toml can hold, as `delivery.settings` shows it, saved
// back unchanged.
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
    { from: "~/w,1.env", to: ".env", base: "project", mode: "0400",
      only: { servers: ["b"], backends: ["vz"], repos: ["example/w"] } },
  ], only: { backends: ["vz", "remote"] } }],
];

test("an unchanged draft saves back exactly what it was read from", () => {
  for (const [p, cfg] of ROUND_TRIPS) {
    assert.deepEqual(configFromDraft(D[p], draftFromConfig(D[p], cfg), cfg.enabled), cfg, p);
  }
});

test("scope: every server writes no servers, the ticks write exactly the ticked", () => {
  const ticks = ["mock", "mock2", "lab"];
  let d = draftFromConfig(D.github, { enabled: true, gh: "gh" });
  assert.equal(d.scope.every, true);
  assert.equal("only" in configFromDraft(D.github, d, true, ticks), false, "every server: no only");
  d = edit.server(d, null, "lab", true);
  d = edit.server(d, null, "mock", true);
  assert.deepEqual(configFromDraft(D.github, d, true, ticks).only, { servers: ["mock", "lab"] }, "in tick order");
  d = edit.server(d, null, "mock", false);
  assert.deepEqual(configFromDraft(D.github, d, true, ticks).only, { servers: ["lab"] });
  // Back to every server: the ticks are kept in the form, and nothing is written.
  d = edit.every(d, null, true);
  assert.equal("only" in configFromDraft(D.github, d, true, ticks), false);
  // Only these, with nothing ticked, cannot be saved.
  d = edit.every(edit.server(d, null, "lab", false), null, false);
  assert.deepEqual(missing(D.github, d), ["Tick at least one server, or choose every server."]);
  // Backends as three ticks, written in their fixed order; none ticked is every backend.
  d = edit.every(d, null, true);
  d = edit.backend(d, null, "remote", true);
  d = edit.backend(d, null, "vz", true);
  d = edit.backend(d, null, "docker", true);
  assert.deepEqual(configFromDraft(D.github, d, true, ticks).only, { backends: ["vz", "remote"] });
  // Repos as patterns, each exactly as typed (a blank one is the set's to refuse).
  d = edit.add(d, "only.repos");
  d = edit.entry(d, "only.repos", 0, "example/*");
  d = edit.add(d, "only.repos");
  assert.deepEqual(configFromDraft(D.github, d, true, ticks).only.repos, ["example/*", ""]);
  assert.deepEqual(onlyOf(scopeOf({ servers: [] })), { servers: [] }, "an empty list is kept, never every server");
});

test("scope in words, and the servers it can be ticked for", () => {
  // Each reads as a phrase of its own, with no prefix before it.
  assert.equal(scopeWords(null), "On every server");
  assert.equal(scopeWords({ servers: ["mini3"] }), "Only on mini3");
  assert.equal(scopeWords({ servers: ["a", "b"], backends: ["vz", "remote"], repos: ["x/*"] }),
    "Only on a and b · VZ and Cloud sheds only · repos matching x/*");
  assert.equal(scopeWords({ servers: [] }), "On no server");
  assert.equal(scopeWords({ repos: ["a/*", "b/c"] }), "On every server · repos matching a/* and b/c");
  const rows = [
    whereRowOf(listRow("a", "this_machine", { machine: ME })),
    whereRowOf(listRow("acct", "service", { url: "https://x.example.test" })),
    whereRowOf(listRow("b", "nobody")),
    whereRowOf(listRow("old", "unsupported")),
  ];
  assert.deepEqual(serverTicks(rows, null), ["a", "b"]);
  assert.deepEqual(serverTicks(rows, scopeOf({ servers: ["gone"] })), ["a", "b", "gone"], "a named server is never dropped unseen");
});

test("a provider cannot be turned on with a required field empty", () => {
  const none = draftFromConfig(D["ssh-key"], null);
  assert.deepEqual(missing(D["ssh-key"], none), ["Key file is required."]);
  assert.deepEqual(missing(D["ssh-key"], edit.text(none, "key_file", "~/.ssh/id_ed25519")), []);
  assert.deepEqual(missing(D.github, draftFromConfig(D.github, null)), [], "gh has a default");
  assert.deepEqual(missing(D["envsecrets-login"], draftFromConfig(D["envsecrets-login"], null)),
    ["A passphrase file or a passphrase command is required."]);
  assert.deepEqual(missing(D["envsecrets-login"], draftFromConfig(D["envsecrets-login"], { passphrase_command: COMMAND_KEPT })), []);
  const api = edit.bool(draftFromConfig(D["api-keys"], null), "craze_providers", false);
  assert.deepEqual(missing(D["api-keys"], api), ["craze's providers.toml or an env file is required."]);
  let files = draftFromConfig(D.files, null);
  assert.deepEqual(missing(D.files, files), ["At least one file is required."]);
  files = edit.add(files, "file", D.files);
  assert.deepEqual(missing(D.files, files), ["File 1: From is required.", "File 1: To is required."]);
  files = edit.entryValue(edit.entryValue(files, 0, "from", "~/a"), 0, "to", "a");
  files = edit.every(files, 0, false);
  assert.deepEqual(missing(D.files, files), ["File 1: tick at least one server, or choose every server."]);
});

test("each provider row says its state, scope, where it lands and its actions", () => {
  const set = (name, config, enabled = true) => ({ name, configured: config !== null, enabled, only: config?.only ?? null, config });
  let v = whatView(D["ssh-key"], set("ssh-key", null));
  assert.equal(v.state, "not_set_up");
  assert.deepEqual(v.actions, ["setup"]);
  assert.equal(v.scope, null);
  v = whatView(D["ssh-key"], set("ssh-key", { enabled: true, key_file: "~/k", only: { backends: ["vz"] } }));
  assert.deepEqual(v.actions, ["edit", "turn_off", "remove"]);
  assert.equal(v.scope, "On every server · VZ sheds only");
  v = whatView(D["ssh-key"], set("ssh-key", { enabled: false, key_file: "~/k" }, false));
  assert.deepEqual(v.actions, ["turn_on", "edit", "remove"]);
  v = whatView(D["ssh-key"], set("ssh-key", { enabled: false }, false));
  assert.deepEqual(v.actions, ["setup", "remove"], "an incomplete provider is set up through its form");
  assert.equal(v.stateText, "Off · not complete");
  v = whatView(D.files, set("files", { enabled: true, file: [{ from: "~/a", to: ".npmrc", base: "home" },
    { from: "~/b", to: ".env", base: "project" }] }));
  assert.equal(v.lands, "~/.npmrc, the project's .env");
  assert.equal(saveLabel("setup", set("ssh-key", null, false)), "Save and turn on");
  assert.equal(saveLabel("edit", set("ssh-key", { enabled: false }, false)), "Save");
  assert.deepEqual(switched(set("github", { enabled: true, gh: "gh" }), false), { enabled: false, gh: "gh" });
});

test("each list entry is edited alone, its string kept exactly", () => {
  let d = draftFromConfig(D["api-keys"], { enabled: true, env_files: ["~/a,b.env"] });
  d = edit.add(d, "env_files");
  d = edit.entry(d, "env_files", 1, " ~/two\nlines.env ");
  assert.deepEqual(configFromDraft(D["api-keys"], d, true).env_files, ["~/a,b.env", " ~/two\nlines.env "]);
  d = edit.remove(d, "env_files", 0);
  assert.deepEqual(d.lists.env_files, [" ~/two\nlines.env "]);
  d = edit.add(d, "env_files");
  assert.deepEqual(configFromDraft(D["api-keys"], d, true).env_files, [" ~/two\nlines.env "], "a blank entry is left out");
  assert.equal(edit.entry(d, "only.users", 0, "x"), d, "an unknown list changes nothing");
  assert.equal(edit.entry(d, "env_files", 5, "x"), d, "an entry out of range changes nothing");
});

test("the files form round-trips mode and a per-file scope", () => {
  let d = draftFromConfig(D.files, { enabled: true, file: [{ from: "~/.npmrc", to: ".npmrc", base: "home" }] });
  d = edit.add(d, "file", D.files);
  assert.deepEqual(d.entries[1].values, { from: "", to: "", base: "home", mode: "" });
  d = edit.entryValue(d, 1, "from", "~/notes, todo.md");
  d = edit.entryValue(d, 1, "to", "notes.md");
  d = edit.entryValue(d, 1, "base", "project");
  d = edit.entryValue(d, 1, "mode", "0400");
  d = edit.server(d, 1, "mock", true);
  d = edit.backend(d, 1, "vz", true);
  d = edit.add(d, "file.1.only.repos");
  d = edit.entry(d, "file.1.only.repos", 0, "example/widgets");
  const cfg = configFromDraft(D.files, d, true, ["mock"]);
  assert.deepEqual(cfg.file[1], { from: "~/notes, todo.md", to: "notes.md", base: "project", mode: "0400",
    only: { servers: ["mock"], backends: ["vz"], repos: ["example/widgets"] } });
  assert.deepEqual(cfg.file[0], { from: "~/.npmrc", to: ".npmrc", base: "home" }, "the other entry is as it was");
  // Read back, it is the same draft; the default mode is no mode.
  assert.deepEqual(configFromDraft(D.files, draftFromConfig(D.files, cfg), true, ["mock"]), cfg);
  d = edit.entryValue(d, 1, "mode", "");
  assert.equal("mode" in configFromDraft(D.files, d, true).file[1], false);
  d = edit.remove(d, "file", 0);
  assert.equal(d.entries.length, 1);
  assert.equal(edit.entryValue(d, 4, "to", "x"), d);
});

test("a stored command is kept unseen, replaced exactly, or cleared", () => {
  const P = D["envsecrets-login"];
  const d = draftFromConfig(P, { enabled: true, passphrase_command: COMMAND_KEPT });
  assert.deepEqual(d.command, { keep: true });
  const kept = edit.text(d, "passphrase_file", "~/p");
  assert.equal(configFromDraft(P, kept, true).passphrase_command, COMMAND_KEPT);
  let r = edit.replaceCommand(d);
  r = edit.entry(r, "passphrase_command", 0, "printf");
  r = edit.add(r, "passphrase_command");
  r = edit.add(r, "passphrase_command");
  r = edit.entry(r, "passphrase_command", 2, "SENTINEL a,b\nc");
  assert.deepEqual(configFromDraft(P, r, true).passphrase_command, ["printf", "", "SENTINEL a,b\nc"]);
  assert.equal(redactedConfig(P, r, true).passphrase_command, "replaced (3 arguments)");
  assert.equal(JSON.stringify(redactedConfig(P, r, true)).includes("SENTINEL"), false);
  assert.equal(configFromDraft(P, edit.keepCommand(r), true).passphrase_command, COMMAND_KEPT);
  assert.equal("passphrase_command" in configFromDraft(P, edit.clearCommand(d), true), false);
  const none = draftFromConfig(P, { enabled: true });
  assert.deepEqual(none.command, { keep: false, args: [] });
  assert.equal(edit.keepCommand(none), none);
  assert.deepEqual(draftFromConfig(P, { passphrase_command: ["SENTINEL"] }).command, { keep: false, args: [] },
    "anything but the marker is never shown back");
});

/* ---- 3. Check ---------------------------------------------------------------- */

test("what a Check sends, or why it cannot run", () => {
  assert.deepEqual(checkRequest({ ...EMPTY_CHECK, shed: "mock/widgets" }), { server: "mock", shed: "widgets" });
  assert.deepEqual(checkRequest(EMPTY_CHECK), { error: "Pick a shed." });
  const t = { ...EMPTY_CHECK, by: "target", server: "mock", backend: "remote", repo: " example/widgets " };
  assert.deepEqual(checkRequest(t), { server: "mock", backend: "remote", repo: "example/widgets" });
  assert.deepEqual(checkRequest({ ...t, repo: "" }), { server: "mock", backend: "remote", repo: null });
  assert.deepEqual(checkRequest({ ...t, repo: "widgets" }), { error: "A repo is owner/name." });
  assert.deepEqual(checkRequest({ ...t, backend: "docker" }), { error: "Pick a backend." });
  assert.deepEqual(checkRequest({ ...t, server: "" }), { error: "Pick a server." });
});

test("Check says before it runs which commands it runs", () => {
  const p = (name, config, enabled = true) => ({ name, configured: true, enabled, only: null, config });
  const both = [p("github", { gh: "gh" }), p("envsecrets-login", { passphrase_command: COMMAND_KEPT })];
  // The same names `delivery.preview` holds a Check to (`settings_file::commands_run`).
  assert.deepEqual(checkRuns(both), ["gh", "passphrase_command"]);
  assert.match(checkNote(checkRuns(both)), /^Check runs gh and your passphrase command on this machine/);
  const gh = [p("github", { gh: "gh" }), p("envsecrets-login", { passphrase_file: "~/p" })];
  assert.deepEqual(checkRuns(gh), ["gh"]);
  assert.match(checkNote(checkRuns(gh)), /^Check runs gh on/);
  assert.deepEqual(checkRuns([p("github", { gh: "gh" }, false)]), []);
  assert.match(checkNote([]), /runs no command/);
  assert.equal(sameRuns(["gh"], ["gh"]), true);
  assert.equal(sameRuns([], ["gh"]), false, "turned on since the warning: not the same");
});

test("a Check that could not run is fixed words by code, never the server's", () => {
  assert.equal(checkErrorText("not_found", "mock"), "mock has no shed by that name.");
  assert.match(checkErrorText("settings_changed", "mock"), /settings changed/);
  for (const code of ["server_refused", "action_failed", "bad_shed", "settings_unreadable", "key_unavailable",
    "bad_request", "SERVER SENTINEL"]) {
    const t = checkErrorText(code, "mock");
    assert.ok(t.length > 0 && !t.includes("SENTINEL"), code);
  }
  assert.equal(checkErrorText(refusalCode({ code: "not_found", message: "SERVER SENTINEL" }), "mock"),
    "mock has no shed by that name.");
});

test("a Check's result in words, details behind it", () => {
  const r = checkResult({
    server: "mock", shed: null, request: { backend: "vz", repo: "example/widgets" },
    preview: {
      items: [
        { name: "ssh-key", provider: "ssh-key", kind: "file", stage: 1, base: "home", target: ".ssh/id_ed25519",
          mode: "0600", size: 411, fingerprint: "f".repeat(24) },
        { name: "files..env", provider: "files", kind: "file", stage: 2, base: "project", target: ".env", size: 2048, fingerprint: "a" },
        { name: "k", provider: "api-keys", kind: "env", stage: 1, target: "EXAMPLE_API_KEY", size: 20, fingerprint: "b" },
      ],
      problems: [{ provider: "github", code: "unreadable", message: "gh is not installed" }],
      providers: [{ provider: "envsecrets-login", ran: false }, { provider: "ssh-key", ran: true }],
    },
  }, DESCS);
  assert.equal(r.about, "A new VZ shed on mock with example/widgets");
  assert.deepEqual(r.items.map((i) => i.words), ["SSH key: ~/.ssh/id_ed25519", "Files: the project's .env", "API keys: $EXAMPLE_API_KEY"]);
  assert.deepEqual(r.items[0].details, { target: ".ssh/id_ed25519", kind: "file", stage: "1", mode: "0600", size: "411 B", fingerprint: "f".repeat(24) });
  assert.deepEqual(r.problems, ["GitHub: gh is not installed (unreadable)"]);
  assert.deepEqual(r.leftOut, ["envsecrets login"]);
  assert.equal(checkResult({ server: "mock", shed: "widgets", request: { backend: "firecracker", repo: null }, preview: {} }, DESCS).about,
    "widgets on mock · Firecracker");
});

test("sizes", () => {
  assert.equal(formatSize(0), "0 B");
  assert.equal(formatSize(1023), "1023 B");
  assert.equal(formatSize(1536), "1.5 KB");
  assert.equal(formatSize(20 * 1024 * 1024), "20 MB");
  assert.equal(formatSize(-1), "");
});

/* ---- 4. Recent deliveries ----------------------------------------------------- */

test("recent deliveries in words, with no value", () => {
  const rows = recentRows({ recent: [
    { request: "r2", server: "mock", shed: "second", reason: "create", at: "2026-11-25T10:00:01Z",
      items: [{ name: "ssh-key", target: ".ssh/id_ed25519", size: 1, fingerprint: "x" }],
      problems: [{ provider: "github", code: "unreadable", message: "SOME TEXT" }],
      outcome: { outcome: "accepted" } },
    { request: "r1", server: "mock", shed: "first", reason: "provision", at: "bad", items: [], problems: [],
      outcome: { outcome: "refused", status: 409, code: "answer_refused", rule: "x" } },
  ] });
  assert.deepEqual(rows[0], { key: "r2", when: "2026-11-25 10:00:01 UTC", server: "mock", shed: "second",
    reason: "create", items: "1 item: ssh-key", problems: ["github: unreadable"], outcome: "delivered" });
  assert.equal(rows[1].when, "bad");
  assert.equal(rows[1].items, "no items");
  assert.equal(rows[1].outcome, "refused (answer_refused)");
  assert.equal(JSON.stringify(rows).includes("SOME TEXT"), false, "a problem's message is not listed");
  assert.equal(outcomeText({ outcome: "failed", class: "transport" }), "not sent (transport)");
  assert.equal(outcomeText({ outcome: "pending" }), "waiting for the server");
  assert.equal(outcomeText(null), "unknown");
  assert.equal(whenText("2026-11-25T10:00:01.5Z"), "2026-11-25 10:00:01 UTC");
  assert.deepEqual(recentRows(null), []);
});
