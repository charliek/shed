/* The craze create sheet (plan 025 §3.8, C10) — provider + directory + an
 * optional name (plan 029 D14, on a hub that can take one) + an optional
 * first prompt, nothing else (D6: no model, no effort, no permission mode;
 * craze's own defaults).
 *
 * What it holds and what it does not:
 *
 * - **The options are read on every open** (D8: nothing cached) — providers in
 *   craze's order, the default, the recent directories — through
 *   `craze_create_options`, which on a DORMANT machine is the explicit action
 *   that starts its hub (§3.6.1).
 * - **The draft is NOT the sheet's**: the typed form, the request id and the
 *   submission's phase live in the app shell, per machine
 *   (`crazeCreate.ts`'s `CrazeDraft`), so dismissing the sheet keeps a
 *   submission running — its session simply appears as a row — and a re-open
 *   finds the same form and the same id. The shell runs the create; this
 *   component edits the draft and presses the button.
 * - **No state clears the typed form**: loading, a failed options read, the
 *   machine going offline or turning out too old, a refusal, an unknown
 *   outcome — each says its piece and leaves what was typed alone.
 * - **Resume** (plan 032 §3.4): where the options say the hub can reopen, a
 *   `New | Resume` control; Resume lists the machine's saved sessions (read
 *   on its first choice) and runs one again — one at a time, its outcome
 *   landing by its code (`crazeResume.ts`). Its state is the shell's too, so
 *   a reopen — minutes, at worst — runs on behind a dismissed sheet, bound
 *   to the machine's incarnation by every status the shell reads. A failed
 *   options read offers its Retry in either half.
 *
 * Every rule it applies is `crazeCreate.ts`'s or `crazeResume.ts`'s (pure,
 * node-tested); this file is the rendering and the drivable doors
 * (`ui.fill_craze_create` / `ui.submit_craze_create` / `ui.choose_craze_mode`
 * / `ui.resume_saved`, test mode only) and its own report
 * (`craze_create.dump`), read back off its DOM. */
import { useCallback, useEffect, useId, useRef, useState } from "react";
import { RotateCw, Sparkles } from "lucide-react";

import { DialogShell, Field, Scrim, Segmented, dialogBtnSecondary, dialogInput, useEscClose } from "@/components/dialog";
import { cn } from "@/lib/utils";
import { renderedText, renderedValues } from "@/lib/roost";
import {
  crazeCreateOptions, laneFailure, reportCrazeCreate,
  type CrazeCreateProviderRow, type CrazeSavedDump,
} from "@/lib/bridge";
import {
  crazeCreateGate, crazeSheetNote, directoryProblem, preselectedProvider,
  primaryLabel, providerSelectable, reconcileProvider, refusalView, titleFieldShown,
  type CrazeCreateOptions, type CrazeDraft, type CrazeStatusLike,
} from "@/lib/crazeCreate";
import {
  MORE_NOTE, RESUMING_LABEL, canResume, modeLocked, optionsRetryHalf, relativeAge, resumeLabel, resumeOffered,
  rowState, savedRowError, savedTitle, shortenHome,
  type CrazeResume, type CrazeSheetMode,
} from "@/lib/crazeResume";

/** The sheet's two halves (plan 032 §3.4): a new session, or a saved one
 *  running again. */
const SHEET_MODES: [CrazeSheetMode, string][] = [["new", "New"], ["resume", "Resume"]];

type OptionsLoad =
  | { phase: "loading" }
  | { phase: "failed"; code: string; message: string }
  | { phase: "ready"; options: CrazeCreateOptions };

export function CrazeCreateDialog({
  machine, craze, draft, onEdit, onSubmit, resume, onSourceGen, onChooseMode, onResume, onRetryList, onClose,
}: {
  machine: string;
  /** This machine's craze status as the shell last read it — what turns the
   *  sheet's note on (offline, too old, not installed) while it is open. Read
   *  through `crazeStatusFor`, so an unlisted dormant shed's comes from its
   *  `shed_craze` entry (plan 026 §3.2). */
  craze: CrazeStatusLike;
  draft: CrazeDraft;
  onEdit: (patch: Partial<Pick<CrazeDraft, "provider" | "cwd" | "title" | "prompt">>) => void;
  onSubmit: () => void;
  /** The machine's Resume state (plan 032 §3.4) — the shell's, like the
   *  draft, so a reopen keeps running behind a dismissed sheet. */
  resume: CrazeResume;
  /** The create options this open read came through the machine's craze
   *  source of generation `gen` — its incarnation, which the shell binds the
   *  Resume state to (`crazeResume.ts`'s `observeSource`, C5 review). One
   *  of the answers that bind it, not the only one: every status the shell
   *  reads binds it too (`observeSources`), so a failed read here never
   *  leaves an older incarnation's state in place. */
  onSourceGen: (gen: number | null | undefined) => void;
  /** A half chosen (the shell reads the saved list the first time Resume is). */
  onChooseMode: (mode: CrazeSheetMode) => void;
  /** A saved row's Resume (or Retry) pressed. */
  onResume: (id: string) => void;
  /** The saved list's Retry, after a failed read. */
  onRetryList: () => void;
  onClose: () => void;
}) {
  const fid = useId();
  const [load, setLoad] = useState<OptionsLoad>({ phase: "loading" });
  const [attempt, setAttempt] = useState(0);

  // Read on every open (and every Retry): what a create can start here NOW —
  // and through which incarnation of the machine, which the shell binds the
  // Resume state to (a host removed and added again under this name is a new
  // one). The handler is the one rendered with THIS machine (`live` fences
  // the rest).
  useEffect(() => {
    let live = true;
    setLoad({ phase: "loading" });
    crazeCreateOptions(machine).then(
      (options) => {
        if (!live) return;
        setLoad({ phase: "ready", options });
        onSourceGen(options.source_gen);
      },
      (e: unknown) => {
        const f = laneFailure(e);
        if (live) setLoad({ phase: "failed", code: f.code, message: f.message });
      },
    );
    return () => { live = false; };
  }, [machine, attempt]); // eslint-disable-line react-hooks/exhaustive-deps

  // The provider follows the options — for an IDLE draft only: a selection
  // carried over from an earlier open survives only while still ready, else
  // the default rule. Any other phase is left alone (plan 026 §3.1): a
  // submission in flight (its controls are disabled), an unknown outcome
  // (reconciling would be an edit, and an edit drops the id it holds — the
  // retry would then start a second session) and a refusal (it keeps its
  // words and its provider until the person picks another).
  const options = load.phase === "ready" ? load.options : null;
  useEffect(() => {
    if (!options || draft.phase !== "idle") return;
    const want = reconcileProvider(draft.provider, options);
    if (want !== draft.provider) onEdit({ provider: want });
  }, [options, draft.provider, draft.phase]); // eslint-disable-line react-hooks/exhaustive-deps

  const submitting = draft.phase === "submitting";
  // A settled outcome is said BELOW the form: bring it into view when it
  // lands, so a long provider list cannot hide "the session failed to start".
  const outcome = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (draft.phase === "refused" || draft.phase === "unknown") {
      outcome.current?.scrollIntoView({ block: "nearest" });
    }
  }, [draft.phase, draft.refusal]);
  // Dismissing while a submission runs is allowed, and keeps it running.
  useEscClose(onClose);

  const machineNote = crazeSheetNote(craze, machine);
  const providers = options?.providers ?? [];
  // Every gating rule is `crazeCreateGate`'s (node-tested): the selection,
  // the note, and whether the button works — a held request replayed
  // included.
  const { selected, replaying, note, canCreate } = crazeCreateGate(draft, options, machineNote);
  // A held provider the re-read no longer lists still names the request
  // being replayed: it is shown as a held row of its own.
  const heldAbsent = replaying && selected !== null && !!options && !providers.some((p) => p.id === selected);
  const cwdProblem = draft.cwd === "" && draft.phase === "idle" ? null : directoryProblem(draft.cwd);
  const refusal = draft.refusal ? refusalView(draft.refusal) : null;
  // "Name (optional)" where the live hub can take a title at create (P10) —
  // and while a typed one would be sent (`titleFieldShown`).
  const titleField = titleFieldShown(craze, draft);
  // `New | Resume` (plan 032 §3.4, PC11): shown where the options just read
  // say the hub can reopen, locked while either half's action runs. Every
  // rule is `crazeResume.ts`'s.
  const modesOffered = resumeOffered(options?.reopen, resume);
  const locked = modeLocked(submitting, resume);
  const resuming = resume.mode === "resume";
  // The Resume half says only the machine's own note: the rest of the
  // create gate's (no provider ready, a held request) is the form's.
  const sheetNote = resuming ? machineNote : note;
  // A failed options read says so, with its Retry, in WHICHEVER half shows
  // (`optionsRetryHalf`, C5 re-review): a sheet opened onto Resume — a
  // reopen left running — can still read them again. Never locked.
  const optionsFailure = load.phase === "failed" ? load : null;
  const retryHalf = optionsRetryHalf(optionsFailure !== null, resume);
  const retryOptions = useCallback(() => setAttempt((a) => a + 1), []);
  const choose = useCallback((mode: CrazeSheetMode) => {
    if (!modesOffered || locked) return;
    onChooseMode(mode);
  }, [modesOffered, locked, onChooseMode]);
  const pressSaved = useCallback((id: string) => {
    if (canResume(resume, id)) onResume(id);
  }, [resume, onResume]);

  // A click on a provider row: a dimmed one refuses it, as it refuses the
  // harness's door — the rule, not just the look.
  const pick = useCallback((id: string) => {
    const p = providers.find((x) => x.id === id);
    if (!p || !providerSelectable(p) || submitting) return;
    onEdit({ provider: id });
  }, [providers, submitting, onEdit]);

  // The drivable doors (test mode only — `ipc.rs`'s `craze_create_door`):
  // fill sets the named controls as a person would; submit presses the
  // primary button through its own gate. Refs keep the handlers current —
  // the listeners are registered once.
  const doors = useRef({
    pick, onEdit, recent: [] as string[], press: () => {},
    choose: (_mode: CrazeSheetMode) => {}, resume: (_id: string) => {},
  });
  useEffect(() => {
    doors.current = {
      pick,
      onEdit,
      recent: options?.recent_dirs ?? [],
      // The primary button exists only in the New half.
      press: () => { if (canCreate && !resuming) onSubmit(); },
      choose,
      resume: pressSaved,
    };
  });
  useEffect(() => {
    if (typeof window === "undefined" || !("__TAURI_INTERNALS__" in window)) return;
    const uns: Array<() => void> = [];
    let cancelled = false;
    void import("@tauri-apps/api/event").then(async ({ listen }) => {
      uns.push(
        await listen<{ provider?: unknown; cwd?: unknown; title?: unknown; prompt?: unknown; recent?: unknown }>("fill-craze-create", (e) => {
          const fill = e.payload ?? {};
          if (typeof fill.provider === "string") doors.current.pick(fill.provider);
          if (typeof fill.recent === "number") {
            const dir = doors.current.recent[fill.recent];
            if (dir !== undefined) doors.current.onEdit({ cwd: dir });
          }
          if (typeof fill.cwd === "string") doors.current.onEdit({ cwd: fill.cwd });
          if (typeof fill.title === "string") doors.current.onEdit({ title: fill.title });
          if (typeof fill.prompt === "string") doors.current.onEdit({ prompt: fill.prompt });
        }),
      );
      uns.push(await listen("submit-craze-create", () => doors.current.press()));
      // The Resume half's doors (plan 032 PC14i): each press goes through
      // its control's own gate, as a pointer's does.
      uns.push(
        await listen<{ mode?: unknown }>("choose-craze-mode", (e) => {
          const mode = e.payload?.mode;
          if (mode === "new" || mode === "resume") doors.current.choose(mode);
        }),
      );
      uns.push(
        await listen<{ id?: unknown }>("resume-saved", (e) => {
          if (typeof e.payload?.id === "string") doors.current.resume(e.payload.id);
        }),
      );
      if (cancelled) uns.forEach((u) => u());
    });
    return () => {
      cancelled = true;
      uns.forEach((u) => u());
    };
  }, []);

  // Report what the sheet RENDERED (`craze_create.dump`), read back off its
  // DOM after the commit — the `launch.dump` rule — so the dump cannot claim a
  // provider row, a dimming or a value the sheet does not show.
  const shape = JSON.stringify([load, draft, machineNote, canCreate, titleField, resume, modesOffered, locked]);
  useEffect(() => {
    const root = document.querySelector("[data-craze-create]");
    const modeButtons = Array.from(root?.querySelectorAll<HTMLButtonElement>("[data-craze-mode]") ?? []);
    const savedRoot = root?.querySelector<HTMLElement>("[data-saved]");
    const saved: CrazeSavedDump | null = savedRoot
      ? {
        state: savedRoot.dataset.savedState ?? "",
        error: savedRoot.querySelector("[data-saved-list-error]")?.textContent ?? null,
        more: savedRoot.querySelector("[data-saved-more]")?.textContent ?? null,
        gone: savedRoot.querySelector("[data-saved-gone]")?.textContent ?? null,
        rows: Array.from(savedRoot.querySelectorAll<HTMLElement>("[data-saved-row]")).map((el) => {
          const button = el.querySelector<HTMLButtonElement>("[data-saved-resume]");
          return {
            id: el.dataset.savedRow ?? "",
            title: el.querySelector("[data-saved-title]")?.textContent ?? "",
            provider: el.querySelector("[data-saved-provider]")?.textContent ?? "",
            workspace: el.querySelector("[data-saved-workspace]")?.textContent ?? "",
            updated: el.querySelector("[data-saved-updated]")?.textContent ?? "",
            state: el.dataset.state ?? "",
            error: el.querySelector("[data-saved-error]")?.textContent ?? null,
            button: button?.textContent?.trim() ?? "",
            enabled: button?.disabled === false,
          };
        }),
      }
      : null;
    const rows: CrazeCreateProviderRow[] = Array.from(
      root?.querySelectorAll<HTMLElement>("[data-provider]") ?? [],
    ).map((el) => ({
      id: el.dataset.provider ?? "",
      label: el.querySelector("[data-provider-label]")?.textContent ?? "",
      state: el.dataset.state ?? "",
      dimmed: el.dataset.dimmed === "true",
      selected: el.dataset.selected === "true",
      reason: el.querySelector("[data-reason]")?.textContent ?? null,
      fix: el.querySelector("[data-fix]")?.textContent ?? null,
    }));
    const primary = root?.querySelector<HTMLButtonElement>("[data-craze-create-primary]");
    reportCrazeCreate({
      machine,
      state: load.phase === "ready" ? draft.phase : load.phase,
      providers: rows,
      preselected: options ? preselectedProvider(options) : null,
      default_provider: options?.default_provider ?? null,
      recent_dirs: Array.from(root?.querySelectorAll<HTMLElement>("[data-recent-dir]") ?? []).map(
        (el) => el.dataset.recentDir ?? "",
      ),
      values: renderedValues("[data-craze-create]"),
      title_field: !!root?.querySelector("[data-craze-title]"),
      title_problem: root?.querySelector("[data-title-problem]")?.textContent ?? null,
      request_id: draft.requestId,
      note: root?.querySelector("[data-craze-note]")?.textContent ?? null,
      error: draft.refusal && refusal ? { ...draft.refusal, where: refusal.where, text: refusal.text } : null,
      cause: root?.querySelector("[data-craze-cause]")?.textContent ?? null,
      cwd_problem: root?.querySelector("[data-cwd-problem]")?.textContent ?? null,
      create_enabled: primary?.disabled === false,
      primary: primary?.textContent?.trim() ?? "",
      modes: { offered: modeButtons.length > 0, enabled: modeButtons.some((b) => !b.disabled) },
      mode: root?.querySelector<HTMLElement>("[data-craze-half]")?.dataset.crazeHalf ?? "",
      saved,
      rendered: renderedText("[data-craze-create]"),
    });
  }, [shape]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => reportCrazeCreate(null), []);

  return (
    <Scrim onClose={onClose} mark="craze-create">
      <DialogShell
        icon={Sparkles}
        title={resuming ? "Resume a craze session" : "New craze session"}
        sub={resuming
          ? `on ${machine} — a saved session, running again`
          : `on ${machine} — a provider, a directory, and an optional first prompt`}
        onClose={onClose}
        width={540}
        footer={
          <>
            <button
              onClick={onClose}
              className={dialogBtnSecondary}
              title={submitting
                ? "The create keeps running; its session appears as a row"
                : locked ? "The resume keeps running; its session appears as a row" : undefined}
            >
              {locked ? "Close" : "Cancel"}
            </button>
            {!resuming && <button
              data-craze-create-primary
              onClick={() => onSubmit()}
              disabled={!canCreate}
              className="hbtn inline-flex items-center gap-2 rounded-[9px] px-[22px] py-2.5 text-[14px] font-semibold"
              style={{
                background: canCreate ? "var(--shed-accent)" : "var(--shed-inset)",
                color: canCreate ? "var(--shed-accent-fg)" : "var(--shed-text-muted)",
                border: "none",
                cursor: canCreate ? "pointer" : "default",
              }}
            >
              <Sparkles size={16} /> {primaryLabel(draft)}
            </button>}
          </>
        }
      >
        {modesOffered && (
          <div data-craze-modes>
            <Segmented options={SHEET_MODES} value={resume.mode} set={(v) => choose(v as CrazeSheetMode)} disabled={locked} mark="craze-mode" />
          </div>
        )}
        {sheetNote && (
          <div
            data-craze-note
            className="rounded-md px-3 py-2 text-[13px]"
            style={{ background: "var(--shed-inset)", color: "var(--shed-warn-fg)" }}
          >
            {sheetNote}
          </div>
        )}
        {resuming ? (
          <div data-craze-half="resume" className="contents">
            {retryHalf === "resume" && optionsFailure && (
              <OptionsFailed what="craze could not read this machine's options" message={optionsFailure.message} onRetry={retryOptions} />
            )}
            <SavedList machine={machine} resume={resume} locked={locked} onResume={pressSaved} onRetryList={onRetryList} />
          </div>
        ) : (
        <div data-craze-half="new" className="contents">
        <Field label="Provider">
          {load.phase === "loading" ? (
            <div className="flex items-center gap-2 px-1 py-2 text-[13px] text-shed-text-muted">
              <RotateCw size={14} className="animate-spin" /> reading what {machine} can start…
            </div>
          ) : retryHalf === "new" && optionsFailure ? (
            <OptionsFailed what="craze could not list providers" message={optionsFailure.message} onRetry={retryOptions} />
          ) : (
            <div className="flex flex-col gap-1.5" role="radiogroup" aria-label="Provider">
              {providers.map((p) => {
                const ok = providerSelectable(p);
                const on = p.id === selected;
                return (
                  <div
                    key={p.id}
                    role="radio"
                    aria-checked={on}
                    aria-disabled={!ok || submitting}
                    data-provider={p.id}
                    data-state={p.state}
                    data-dimmed={String(!ok)}
                    data-selected={String(on)}
                    onClick={() => pick(p.id)}
                    className={cn("rounded-[9px] border px-3 py-2", ok && !submitting && "hbtn")}
                    style={{
                      opacity: ok ? 1 : on ? 0.8 : 0.5,
                      cursor: ok && !submitting ? "pointer" : "default",
                      borderColor: on ? "var(--shed-accent)" : "var(--shed-border)",
                      background: on ? "var(--shed-accent-subtle)" : "var(--shed-surface)",
                    }}
                  >
                    <div className="flex items-center gap-2">
                      <span data-provider-label className="text-[14px] font-semibold text-shed-text">{p.label}</span>
                      {!ok && (
                        <span className="font-mono text-[11px] text-shed-text-muted">{p.state.replace(/_/g, " ")}</span>
                      )}
                    </div>
                    {!ok && on && <HeldLine />}
                    {!ok && p.reason && (
                      <div data-reason className="mt-0.5 text-[12px] text-shed-text-secondary">{p.reason}</div>
                    )}
                    {!ok && p.fix && (
                      <div data-fix className="mt-0.5 font-mono text-[12px] text-shed-text-muted">{p.fix}</div>
                    )}
                  </div>
                );
              })}
              {heldAbsent && (
                <div
                  role="radio"
                  aria-checked
                  aria-disabled
                  data-provider={selected}
                  data-state="absent"
                  data-dimmed="true"
                  data-selected="true"
                  className="rounded-[9px] border px-3 py-2"
                  style={{ opacity: 0.8, borderColor: "var(--shed-accent)", background: "var(--shed-accent-subtle)" }}
                >
                  <div className="flex items-center gap-2">
                    <span data-provider-label className="text-[14px] font-semibold text-shed-text">{selected}</span>
                    <span className="font-mono text-[11px] text-shed-text-muted">not listed now</span>
                  </div>
                  <HeldLine />
                </div>
              )}
            </div>
          )}
        </Field>
        <Field label="Directory" htmlFor={`${fid}-cwd`}>
          {(options?.recent_dirs.length ?? 0) > 0 && (
            <div className="flex flex-wrap gap-1.5">
              {options?.recent_dirs.map((dir) => (
                <button
                  key={dir}
                  data-recent-dir={dir}
                  disabled={submitting}
                  onClick={() => onEdit({ cwd: dir })}
                  className="hlink max-w-full truncate rounded-[7px] border px-2 py-1 font-mono text-[12px]"
                  style={{
                    borderColor: draft.cwd === dir ? "var(--shed-accent)" : "var(--shed-border)",
                    color: draft.cwd === dir ? "var(--shed-accent)" : "var(--shed-text-secondary)",
                  }}
                  title={dir}
                >
                  {dir}
                </button>
              ))}
            </div>
          )}
          <input
            id={`${fid}-cwd`}
            value={draft.cwd}
            disabled={submitting}
            onChange={(e) => onEdit({ cwd: e.target.value })}
            placeholder="/absolute/path on that machine"
            spellCheck={false}
            className={cn(dialogInput, "font-mono text-[13px]")}
          />
          {(cwdProblem || refusal?.where === "cwd") && (
            <div data-cwd-problem className="text-[12px]" style={{ color: "var(--shed-danger)" }}>
              {refusal?.where === "cwd" ? refusal.text : cwdProblem}
            </div>
          )}
        </Field>
        {titleField && (
          <Field label="Name" hint="optional" htmlFor={`${fid}-title`}>
            <input
              id={`${fid}-title`}
              data-craze-title
              value={draft.title}
              disabled={submitting}
              onChange={(e) => onEdit({ title: e.target.value })}
              placeholder="what to call the session — craze names it otherwise"
              className={cn(dialogInput, "text-[13px]")}
            />
            {refusal?.where === "title" && (
              <div data-title-problem className="text-[12px]" style={{ color: "var(--shed-danger)" }}>
                {refusal.text}
              </div>
            )}
          </Field>
        )}
        <Field label="First prompt" hint="optional" htmlFor={`${fid}-prompt`}>
          <textarea
            id={`${fid}-prompt`}
            value={draft.prompt}
            disabled={submitting}
            onChange={(e) => onEdit({ prompt: e.target.value })}
            rows={4}
            placeholder="what to start on — or leave it empty for an idle session"
            className={cn(dialogInput, "resize-y text-[13px] leading-snug")}
          />
        </Field>
        <div ref={outcome} className="flex flex-col gap-3 empty:hidden">
        {refusal?.where === "cause" && (
          <div className="flex flex-col gap-1">
            <div className="text-[12px] font-semibold" style={{ color: "var(--shed-danger)" }}>The session failed to start:</div>
            <pre
              data-craze-cause
              className="max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md px-3 py-2 font-mono text-[12px]"
              style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}
            >
              {refusal.text}
            </pre>
          </div>
        )}
        {refusal?.where === "general" && (
          <div data-craze-error className="rounded-md px-3 py-2 font-mono text-[12px]" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}>
            {refusal.text}
          </div>
        )}
        </div>
        </div>
        )}
      </DialogShell>
    </Scrim>
  );
}

/** The sheet's Resume half (plan 032 §3.4): craze's saved sessions, newest
 *  first — each its title (or "Untitled"), provider, workspace (the home
 *  shortened to `~` where the app knows it) and age, with a Resume button.
 *  Rendered from the shell's state by `crazeResume.ts`'s rules and read back
 *  off this DOM for `craze_create.dump`; it holds no rule of its own. */
function SavedList({ machine, resume, locked, onResume, onRetryList }: {
  machine: string;
  resume: CrazeResume;
  /** A create submitting or a reopen running: the list's Retry waits. */
  locked: boolean;
  /** A row's button pressed — through `canResume`'s gate (the caller's). */
  onResume: (id: string) => void;
  onRetryList: () => void;
}) {
  const load = resume.load;
  if (load.phase === "unloaded" || load.phase === "loading") {
    return (
      <div data-saved data-saved-state="loading" className="flex items-center gap-2 px-1 py-2 text-[13px] text-shed-text-muted">
        <RotateCw size={14} className="animate-spin" /> reading the saved sessions on {machine}…
      </div>
    );
  }
  if (load.phase === "failed") {
    return (
      <div data-saved data-saved-state="failed" className="flex items-center gap-3 px-1 py-2 text-[13px]" style={{ color: "var(--shed-danger)" }}>
        <span data-saved-list-error className="min-w-0 flex-1 break-words">craze could not list saved sessions: {load.refusal.message}</span>
        <button
          onClick={onRetryList}
          disabled={locked}
          className="hlink rounded-[8px] border border-shed-border px-3 py-1.5 text-[13px] font-semibold text-shed-text-secondary"
        >
          Retry
        </button>
      </div>
    );
  }
  const now = Date.now();
  return (
    <div data-saved data-saved-state="ready" className="flex flex-col gap-1.5">
      {resume.gone && (
        <div data-saved-gone className="rounded-md px-3 py-2 text-[12px]" style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}>
          “{resume.gone.title}” cannot run here any more, so it was removed: {resume.gone.message}
        </div>
      )}
      {load.sessions.length === 0 && (
        <div data-saved-empty className="px-1 py-2 text-[13px] text-shed-text-muted">No saved sessions on {machine}.</div>
      )}
      {load.sessions.map((s) => {
        const state = rowState(resume, s.id);
        const running = state.phase === "resuming";
        const enabled = canResume(resume, s.id);
        const error = savedRowError(state);
        return (
          <div
            key={s.id}
            data-saved-row={s.id}
            data-state={state.phase}
            className="rounded-[9px] border px-3 py-2"
            style={{
              borderColor: running ? "var(--shed-accent)" : "var(--shed-border)",
              background: running ? "var(--shed-accent-subtle)" : "var(--shed-surface)",
            }}
          >
            <div className="flex items-center gap-3">
              <div className="min-w-0 flex-1">
                <div data-saved-title className="truncate text-[14px] font-semibold text-shed-text">{savedTitle(s)}</div>
                <div className="mt-0.5 flex min-w-0 items-center gap-2 text-[12px] text-shed-text-muted">
                  <span data-saved-provider className="flex-none">{s.provider}</span>
                  <span data-saved-workspace className="min-w-0 truncate font-mono" title={s.workspace}>
                    {shortenHome(s.workspace, load.home)}
                  </span>
                  <span data-saved-updated className="flex-none">{relativeAge(s.updated_unix_ms, now)}</span>
                </div>
              </div>
              <button
                data-saved-resume
                disabled={!enabled}
                onClick={() => onResume(s.id)}
                title={running ? RESUMING_LABEL : undefined}
                className={cn("inline-flex max-w-[45%] flex-none items-center gap-1.5 rounded-[8px] px-3 py-1.5 text-left text-[13px] font-semibold", enabled && "hbtn")}
                style={{
                  background: enabled ? "var(--shed-accent)" : "var(--shed-inset)",
                  color: enabled ? "var(--shed-accent-fg)" : "var(--shed-text-muted)",
                  border: "none",
                  cursor: enabled ? "pointer" : "default",
                }}
              >
                {running && <RotateCw size={13} className="flex-none animate-spin" />}
                {resumeLabel(state)}
              </button>
            </div>
            {error && (error.where === "cause" ? (
              <pre
                data-saved-error
                className="mt-1.5 max-h-32 overflow-auto whitespace-pre-wrap break-words rounded-md px-3 py-2 font-mono text-[12px]"
                style={{ background: "var(--shed-deny-bg)", color: "var(--shed-danger)" }}
              >
                {error.text}
              </pre>
            ) : (
              <div data-saved-error className="mt-1.5 text-[12px]" style={{ color: "var(--shed-danger)" }}>
                {error.text}
              </div>
            ))}
          </div>
        );
      })}
      {load.more && <div data-saved-more className="px-1 text-[12px] text-shed-text-muted">{MORE_NOTE}</div>}
    </div>
  );
}

/** A failed create-options read, said with its Retry — in the half that
 *  shows (`optionsRetryHalf`). The Retry is never disabled: a re-read touches
 *  neither half's action, a reopen left running included. */
function OptionsFailed({ what, message, onRetry }: { what: string; message: string; onRetry: () => void }) {
  return (
    <div className="flex items-center gap-3 px-1 py-2 text-[13px]" style={{ color: "var(--shed-danger)" }} data-options-error>
      <span className="min-w-0 flex-1 break-words">{what}: {message}</span>
      <button
        data-options-retry
        onClick={onRetry}
        className="hlink rounded-[8px] border border-shed-border px-3 py-1.5 text-[13px] font-semibold text-shed-text-secondary"
      >
        Retry
      </button>
    </div>
  );
}

/** The line a held provider's row carries while its request is replayed: it
 *  is selected for the retry, not for a new request. */
function HeldLine() {
  return <div data-held className="mt-0.5 text-[12px] text-shed-text-secondary">held for the retry</div>;
}
