/* The craze create sheet (plan 025 §3.8, C10) — provider + directory + an
 * optional first prompt, nothing else (D6: no model, no effort, no permission
 * mode; craze's own defaults).
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
 *
 * Every rule it applies is `crazeCreate.ts`'s (pure, node-tested); this file
 * is the rendering and the drivable doors (`ui.fill_craze_create` /
 * `ui.submit_craze_create`, test mode only) and its own report
 * (`craze_create.dump`), read back off its DOM. */
import { useCallback, useEffect, useId, useRef, useState } from "react";
import { RotateCw, Sparkles } from "lucide-react";

import { DialogShell, Field, Scrim, dialogBtnSecondary, dialogInput, useEscClose } from "@/components/dialog";
import { cn } from "@/lib/utils";
import { renderedText, renderedValues } from "@/lib/roost";
import {
  crazeCreateOptions, laneFailure, reportCrazeCreate,
  type CrazeCreateProviderRow, type CrazeStatus,
} from "@/lib/bridge";
import {
  NO_PROVIDER_READY, OUTCOME_UNKNOWN_NOTE, crazeSheetNote, directoryProblem, preselectedProvider,
  primaryLabel, providerSelectable, reconcileProvider, refusalView,
  type CrazeCreateOptions, type CrazeDraft,
} from "@/lib/crazeCreate";

type OptionsLoad =
  | { phase: "loading" }
  | { phase: "failed"; code: string; message: string }
  | { phase: "ready"; options: CrazeCreateOptions };

export function CrazeCreateDialog({ machine, craze, draft, onEdit, onSubmit, onClose }: {
  machine: string;
  /** This machine's craze status as the shell last read it — what turns the
   *  sheet's note on (offline, too old, not installed) while it is open. */
  craze: CrazeStatus | null | undefined;
  draft: CrazeDraft;
  onEdit: (patch: Partial<Pick<CrazeDraft, "provider" | "cwd" | "prompt">>) => void;
  onSubmit: () => void;
  onClose: () => void;
}) {
  const fid = useId();
  const [load, setLoad] = useState<OptionsLoad>({ phase: "loading" });
  const [attempt, setAttempt] = useState(0);

  // Read on every open (and every Retry): what a create can start here NOW.
  useEffect(() => {
    let live = true;
    setLoad({ phase: "loading" });
    crazeCreateOptions(machine).then(
      (options) => { if (live) setLoad({ phase: "ready", options }); },
      (e: unknown) => {
        const f = laneFailure(e);
        if (live) setLoad({ phase: "failed", code: f.code, message: f.message });
      },
    );
    return () => { live = false; };
  }, [machine, attempt]);

  // The provider follows the options: a selection carried over from an
  // earlier open survives only while still ready, else the default rule. A
  // submission in flight is left alone (its controls are disabled).
  const options = load.phase === "ready" ? load.options : null;
  useEffect(() => {
    if (!options || draft.phase === "submitting") return;
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
  const noneReady = !!options && !providers.some(providerSelectable);
  const note = machineNote ?? (noneReady ? NO_PROVIDER_READY : null);
  const selected = providers.find((p) => p.id === draft.provider && providerSelectable(p)) ?? null;
  const cwdProblem = draft.cwd === "" && draft.phase === "idle" ? null : directoryProblem(draft.cwd);
  const refusal = draft.refusal ? refusalView(draft.refusal) : null;
  const canCreate = !!options && !!selected && directoryProblem(draft.cwd) === null && !note && !submitting;

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
  const doors = useRef({ pick, onEdit, recent: [] as string[], press: () => {} });
  useEffect(() => {
    doors.current = {
      pick,
      onEdit,
      recent: options?.recent_dirs ?? [],
      press: () => { if (canCreate) onSubmit(); },
    };
  });
  useEffect(() => {
    if (typeof window === "undefined" || !("__TAURI_INTERNALS__" in window)) return;
    const uns: Array<() => void> = [];
    let cancelled = false;
    void import("@tauri-apps/api/event").then(async ({ listen }) => {
      uns.push(
        await listen<{ provider?: unknown; cwd?: unknown; prompt?: unknown; recent?: unknown }>("fill-craze-create", (e) => {
          const fill = e.payload ?? {};
          if (typeof fill.provider === "string") doors.current.pick(fill.provider);
          if (typeof fill.recent === "number") {
            const dir = doors.current.recent[fill.recent];
            if (dir !== undefined) doors.current.onEdit({ cwd: dir });
          }
          if (typeof fill.cwd === "string") doors.current.onEdit({ cwd: fill.cwd });
          if (typeof fill.prompt === "string") doors.current.onEdit({ prompt: fill.prompt });
        }),
      );
      uns.push(await listen("submit-craze-create", () => doors.current.press()));
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
  const shape = JSON.stringify([load, draft, machineNote, canCreate]);
  useEffect(() => {
    const root = document.querySelector("[data-craze-create]");
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
      request_id: draft.requestId,
      note: root?.querySelector("[data-craze-note]")?.textContent ?? null,
      error: draft.refusal && refusal ? { ...draft.refusal, where: refusal.where, text: refusal.text } : null,
      cause: root?.querySelector("[data-craze-cause]")?.textContent ?? null,
      cwd_problem: root?.querySelector("[data-cwd-problem]")?.textContent ?? null,
      create_enabled: primary?.disabled === false,
      primary: primary?.textContent?.trim() ?? "",
      rendered: renderedText("[data-craze-create]"),
    });
  }, [shape]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => reportCrazeCreate(null), []);

  return (
    <Scrim onClose={onClose} mark="craze-create">
      <DialogShell
        icon={Sparkles}
        title="New craze session"
        sub={`on ${machine} — a provider, a directory, and an optional first prompt`}
        onClose={onClose}
        width={540}
        footer={
          <>
            <button onClick={onClose} className={dialogBtnSecondary} title={submitting ? "The create keeps running; its session appears as a row" : undefined}>
              {submitting ? "Close" : "Cancel"}
            </button>
            <button
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
            </button>
          </>
        }
      >
        {note && (
          <div
            data-craze-note
            className="rounded-md px-3 py-2 text-[13px]"
            style={{ background: "var(--shed-inset)", color: "var(--shed-warn-fg)" }}
          >
            {note}
          </div>
        )}
        <Field label="Provider">
          {load.phase === "loading" ? (
            <div className="flex items-center gap-2 px-1 py-2 text-[13px] text-shed-text-muted">
              <RotateCw size={14} className="animate-spin" /> reading what {machine} can start…
            </div>
          ) : load.phase === "failed" ? (
            <div className="flex items-center gap-3 px-1 py-2 text-[13px]" style={{ color: "var(--shed-danger)" }} data-options-error>
              <span className="min-w-0 flex-1 break-words">craze could not list providers: {load.message}</span>
              <button
                onClick={() => setAttempt((a) => a + 1)}
                className="hlink rounded-[8px] border border-shed-border px-3 py-1.5 text-[13px] font-semibold text-shed-text-secondary"
              >
                Retry
              </button>
            </div>
          ) : (
            <div className="flex flex-col gap-1.5" role="radiogroup" aria-label="Provider">
              {providers.map((p) => {
                const ok = providerSelectable(p);
                const on = ok && p.id === draft.provider;
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
                      opacity: ok ? 1 : 0.5,
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
                    {!ok && p.reason && (
                      <div data-reason className="mt-0.5 text-[12px] text-shed-text-secondary">{p.reason}</div>
                    )}
                    {!ok && p.fix && (
                      <div data-fix className="mt-0.5 font-mono text-[12px] text-shed-text-muted">{p.fix}</div>
                    )}
                  </div>
                );
              })}
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
        {draft.phase === "unknown" && (
          <div data-craze-unknown className="rounded-md px-3 py-2 text-[13px]" style={{ background: "var(--shed-inset)", color: "var(--shed-warn-fg)" }}>
            {OUTCOME_UNKNOWN_NOTE}
          </div>
        )}
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
      </DialogShell>
    </Scrim>
  );
}
