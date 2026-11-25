/* The Credentials view (plan 040 §3.9): the fifth sidebar entry, in the four parts
   of the account service's page. Every decision is `lib/credentials.ts`'s (what a row
   says, its actions, what a form requires and writes, what Check sends); the
   reconciliation of the Where rows is `lib/sources.ts`'s. These components only draw
   them and call the app.

   1. Where (`sources.list`, `source.use`, `source.unbind`): who answers for you on each
      configured server. Refusals are worded from their code, never the server's text.
   2. What (`delivery.describe`, `delivery.settings`, `delivery.set`): each provider's
      row and its form. A stored passphrase command never reaches the page: the view
      sees "set" and sends it back to keep it.
   3. Check (`delivery.preview`): a shed from the list, or a server with a backend and a
      repo; names, places, sizes and fingerprints, never a value.
   4. Recent deliveries (`delivery.recent`).

   Driven over IPC by `ui.navigate {pane: "credentials"}` and, in test mode, the doors
   that act on its real controls: `ui.press_source_use` / `ui.press_source_unbind` (a
   Where row's button), `ui.press_delivery_button` (a provider's actions and its form's
   buttons, ticks and switches), `ui.type_delivery_field` (types into the real input),
   `ui.fill_check` (fills Check's own controls) and `ui.press_check` (presses Check or
   Details). Read back through `credentials.dump`, built off the rendered
   nodes (a command's arguments redacted). */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ChevronDown, ChevronRight, Plus, X } from "lucide-react";
import { PageHead, RefreshHeadButton } from "@/components/primitives";
import { Select, Segmented, Switch } from "@/components/dialog";
import { Footnote, Section, prefsButton, prefsInput } from "@/components/prefsKit";
import {
  inTauri, answerFromHere, describeDelivery, fetchHosts, getDeliverySettings, listSources,
  previewDelivery, recentDeliveries, reportCredentials, setDeliveryProvider, stopAnsweringFromHere,
  type DeliverySettings, type Shed,
} from "@/lib/bridge";
import {
  EMPTY_SOURCES, beginMove, beginStop, canUse, createSourceReader, moveConfirmed, moveFailed,
  shownRows, stopConfirmed,
  type SourceReader, type SourcesState,
} from "@/lib/sources";
import {
  ACTION_LABEL, BACKENDS, BACKEND_LABEL, EMPTY_CHECK, EVERY_SERVER, ONLY_THESE,
  checkErrorText, checkNote, checkRequest, checkResult, checkRuns, sameRuns, choiceOptions, clean, configFromDraft, draftFromConfig, edit,
  configErrorText, entryFields, fieldControl, machineFields, missing, pausedErrorText, readDescriptors, recentRows,
  redactedConfig, refusalCode, refusalText, saveEnabled, saveLabel, serverTicks, shedPick, switched,
  whatView, whereRowOfMove, whereStatusOf, whereView,
  type CheckForm, type CheckResult, type ProviderDesc, type ProviderDraft, type ProviderSetting,
  type RecentView, type ScopeAt, type ScopeDraft, type WhatAction, type WhereRow,
} from "@/lib/credentials";

const errText = (e: unknown): string =>
  typeof e === "string" ? e : e instanceof Error ? e.message : String(e);

/** The text a person reads in `el`, without what form controls hold: a textarea's
 *  value is also its text node, and a passphrase command's arguments must never be
 *  read back. */
function readText(el: Element): string {
  const copy = el.cloneNode(true) as Element;
  copy.querySelectorAll("textarea, input, select").forEach((x) => x.remove());
  return copy.textContent ?? "";
}

/* ---- the Where rows' state: held by the shell, so it outlives the view -------- */

export type CredentialSources = {
  src: SourcesState;
  /** A read of the servers has landed (answered or failed). */
  settled: boolean;
  pausedError: string | null;
  /** The shed config could not be read at the last read (`config_error`). */
  configError: string | null;
  use: (host: string) => void;
  stop: (host: string) => void;
};

/** Who answers on each server, reconciled by `sources.ts` (reads in issue order, a move
 *  or a stop owning its row while in flight). Held by the shell so the last known rows
 *  survive leaving the view: `active` is whether the view is shown, and `shown` moves
 *  each time it is navigated to, which reads every server again (a new generation). */
export function useCredentialSources(active: boolean, shown: number): CredentialSources {
  const srcRef = useRef<SourcesState>(EMPTY_SOURCES);
  const [src, setSrc] = useState<SourcesState>(EMPTY_SOURCES);
  const [pausedError, setPausedError] = useState<string | null>(null);
  const [configError, setConfigError] = useState<string | null>(null);
  const [settled, setSettled] = useState(false);
  const seq = useRef(0);
  const reader = useRef<SourceReader | null>(null);
  const commit = useCallback((next: SourcesState) => {
    srcRef.current = next;
    setSrc(next);
  }, []);
  useEffect(() => {
    const r = createSourceReader({
      fetch: async () => {
        const st = whereStatusOf(await listSources());
        if (st) {
          setPausedError(st.paused_error);
          setConfigError(st.config_error);
        }
        setSettled(true);
        return st;
      },
      get: () => srcRef.current,
      commit,
      nextSeq: () => ++seq.current,
      now: () => Date.now(),
      setTimer: (fn, ms) => setTimeout(fn, ms),
      clearTimer: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
    });
    reader.current = r;
    return () => {
      r.dispose();
      if (reader.current === r) reader.current = null;
    };
  }, [commit]);
  useEffect(() => {
    if (active && inTauri()) reader.current?.reload();
  }, [active, shown]);
  // Shown again by the window (focus, visibility): read again, throttled.
  useEffect(() => {
    if (!active) return;
    const onFocus = () => reader.current?.refresh();
    const onVisible = () => {
      if (document.visibilityState === "visible") reader.current?.refresh();
    };
    window.addEventListener("focus", onFocus);
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      window.removeEventListener("focus", onFocus);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [active]);

  // "Answer from here": ONE click, no confirmation (undone as easily, from the other
  // machine or by "Stop answering from here"). The row flips at once and owns itself
  // until the move answers; a refusal puts it back with fixed words under it.
  const use = useCallback((host: string) => {
    const started = beginMove(srcRef.current, host);
    if (!started) return;
    commit(started);
    void answerFromHere(host).then(
      ({ row }) => {
        commit(moveConfirmed(srcRef.current, host, ++seq.current, whereRowOfMove(row, host)));
        reader.current?.read(host);
      },
      (e: unknown) => {
        commit(moveFailed(srcRef.current, host, refusalText(refusalCode(e), host, "use")));
        reader.current?.read();
      },
    );
  }, [commit]);
  // "Stop answering from here": the same shape.
  const stop = useCallback((host: string) => {
    const started = beginStop(srcRef.current, host);
    if (!started) return;
    commit(started);
    void stopAnsweringFromHere(host).then(
      () => {
        commit(stopConfirmed(srcRef.current, host, ++seq.current));
        reader.current?.read(host);
      },
      (e: unknown) => {
        commit(moveFailed(srcRef.current, host, refusalText(refusalCode(e), host, "unbind")));
        reader.current?.read();
      },
    );
  }, [commit]);
  return { src, settled, pausedError, configError, use, stop };
}

/* ---- the view ---------------------------------------------------------------- */

type Form = { provider: string; mode: "setup" | "edit"; draft: ProviderDraft; base: string };

export function CredentialsView({ sources, sheds, shown }: {
  sources: CredentialSources;
  /** The dashboard's sheds: Check's picker. */
  sheds: Shed[];
  /** Moves each time the view is navigated to: everything is read again. */
  shown: number;
}) {
  const root = useRef<HTMLDivElement>(null);
  const { src, settled, pausedError, configError } = sources;
  const [descs, setDescs] = useState<ProviderDesc[]>([]);
  const [settings, setSettings] = useState<DeliverySettings | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [notices, setNotices] = useState<Record<string, string>>({});
  const [form, setForm] = useState<Form | null>(null);
  const [hosts, setHosts] = useState<string[]>([]);
  const [check, setCheck] = useState<CheckForm>(EMPTY_CHECK);
  const [checkOut, setCheckOut] = useState<CheckResult | null>(null);
  const [checkError, setCheckError] = useState<string | null>(null);
  const [checkBusy, setCheckBusy] = useState(false);
  const [details, setDetails] = useState(false);
  const [recent, setRecent] = useState<RecentView[] | null>(null);
  const [recentError, setRecentError] = useState<string | null>(null);
  const loadGen = useRef(0);
  // The doors' listeners are live (a door sent before then would land nowhere).
  const [doorsReady, setDoorsReady] = useState(false);
  // How many Check doors (`ui.fill_check`, `ui.press_check`) this view has taken: the
  // harness's proof that one landed.
  const [checksTaken, setChecksTaken] = useState(0);
  // How many Checks have started (a press on a disabled button starts none).
  const [checksStarted, setChecksStarted] = useState(0);

  const load = useCallback(() => {
    const gen = ++loadGen.current;
    setNotices({});
    void describeDelivery().then((d) => {
      if (gen === loadGen.current) setDescs(readDescriptors(d));
    }, () => {});
    void getDeliverySettings().then(
      (s) => {
        if (gen !== loadGen.current) return;
        setSettings(s);
        setLoadError(null);
      },
      (e) => {
        if (gen === loadGen.current) setLoadError(errText(e));
      },
    );
    void recentDeliveries().then(
      (r) => {
        if (gen !== loadGen.current) return;
        setRecent(recentRows(r));
        setRecentError(null);
      },
      (e) => {
        if (gen === loadGen.current) setRecentError(errText(e));
      },
    );
    void fetchHosts().then((h) => {
      if (gen !== loadGen.current) return;
      setHosts(h);
      setCheck((c) => ({ ...c, server: c.server && h.includes(c.server) ? c.server : (h[0] ?? "") }));
    });
  }, []);
  useEffect(() => {
    if (inTauri()) load();
  }, [load, shown]);
  // Check's shed picker starts on the first shed listed.
  useEffect(() => {
    if (check.shed || !sheds.length) return;
    setCheck((c) => (c.shed ? c : { ...c, shed: shedPick(sheds[0].host, sheds[0].name) }));
  }, [sheds, check.shed]);

  const providers = useMemo<ProviderSetting[]>(() => (settings?.providers ?? []) as ProviderSetting[], [settings]);
  const providerOf = (name: string) => providers.find((p) => p.name === name) ?? null;
  const rows = shownRows(src);
  const descOf = (name: string) => descs.find((d) => d.name === name) ?? null;

  // A provider's table written: the answer is the file as written.
  const write = (name: string, config: Record<string, unknown> | null, then?: () => void) => {
    if (busy[name]) return;
    setBusy((b) => ({ ...b, [name]: true }));
    setErrors((e) => ({ ...e, [name]: "" }));
    void setDeliveryProvider(name, config)
      .then((s) => {
        setSettings(s);
        then?.();
      })
      .catch((e) => setErrors((x) => ({ ...x, [name]: errText(e) })))
      .finally(() => setBusy((b) => ({ ...b, [name]: false })));
  };
  const act = (name: string, a: WhatAction) => {
    const d = descOf(name);
    const p = providerOf(name);
    if (!d) return;
    if (a === "setup" || a === "edit") {
      const config = p?.configured ? (p.config ?? {}) : null;
      setForm({ provider: name, mode: a, draft: draftFromConfig(d, config), base: JSON.stringify(p?.config ?? null) });
      setErrors((e) => ({ ...e, [name]: "" }));
      setNotices((n) => ({ ...n, [name]: "" }));
    } else if (a === "turn_on" || a === "turn_off") {
      if (p) write(name, switched(p, a === "turn_on"));
    } else if (a === "remove") {
      if (form?.provider === name) setForm(null);
      write(name, null);
    }
  };
  // A reload that finds the provider changed elsewhere discards the open form (and
  // says so): Save never writes over a change made elsewhere.
  useEffect(() => {
    if (!form || !settings) return;
    if (JSON.stringify(providerOf(form.provider)?.config ?? null) === form.base) return;
    const name = form.provider;
    setForm(null);
    setNotices((n) => ({ ...n, [name]: "The settings changed while you were editing; your unsaved changes were discarded." }));
  }, [settings]); // eslint-disable-line react-hooks/exhaustive-deps
  const ticksFor = (scope: ScopeDraft | null) => serverTicks(rows, scope);
  const save = () => {
    if (!form) return;
    const d = descOf(form.provider);
    if (!d || missing(d, form.draft).length) return;
    const name = form.provider;
    const config = configFromDraft(d, form.draft, saveEnabled(form.mode, providerOf(name)), ticksFor(null));
    write(name, config, () => setForm(null));
  };
  const setDraft = (f: (d: ProviderDraft) => ProviderDraft) => {
    setForm((x) => (x ? { ...x, draft: f(x.draft) } : x));
    if (form) setErrors((e) => (e[form.provider] ? { ...e, [form.provider]: "" } : e));
  };

  // Check runs under the warning the person read: the settings are read again first,
  // and a change to what it would run shows the new warning instead of running; the
  // preview itself is held to the same commands (`settings_changed`). Errors are fixed
  // words by code.
  const runCheck = (f: CheckForm) => {
    setChecksStarted((n) => n + 1);
    const req = checkRequest(f);
    setDetails(false);
    if ("error" in req) {
      setCheckOut(null);
      setCheckError(req.error);
      return;
    }
    const warned = checkRuns(providers);
    setCheckBusy(true);
    setCheckError(null);
    void (async () => {
      try {
        let fresh: DeliverySettings;
        try {
          fresh = await getDeliverySettings();
        } catch {
          throw { code: "settings_unreadable" };
        }
        setSettings(fresh);
        const runs = checkRuns(fresh.providers as ProviderSetting[]);
        if (!sameRuns(runs, warned)) throw { code: "settings_changed" };
        const p = await previewDelivery(req.server, req, runs);
        setCheckOut(checkResult(p, descs));
      } catch (e) {
        setCheckOut(null);
        setCheckError(checkErrorText(refusalCode(e), req.server));
      } finally {
        setCheckBusy(false);
      }
    })();
  };

  // The doors: each acts on the real control in this view, and a disabled one takes no
  // press, as a pointer's would not.
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    const uns: Array<() => void> = [];
    const within = (sel: string, key: string, value: unknown) =>
      Array.from(root.current?.querySelectorAll<HTMLElement>(sel) ?? []).filter((el) => el.dataset[key] === value);
    const press = (el: Element | null | undefined) => {
      const b = el instanceof HTMLButtonElement ? el : el?.querySelector<HTMLButtonElement>("button");
      if (b && !b.disabled) b.click();
    };
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      uns.push(await listen<{ host?: unknown }>("press-source-use", (e) => {
        within("button[data-source-use]", "sourceUse", e.payload?.host).forEach(press);
      }));
      uns.push(await listen<{ host?: unknown }>("press-source-unbind", (e) => {
        within("button[data-source-unbind]", "sourceUnbind", e.payload?.host).forEach(press);
      }));
      const row = (provider: unknown) => within("[data-delivery-provider]", "deliveryProvider", provider)[0] ?? null;
      uns.push(await listen<{ provider?: unknown; button?: unknown }>("press-delivery-button", (e) => {
        press(Array.from(row(e.payload?.provider)?.querySelectorAll<HTMLElement>("[data-delivery-button]") ?? [])
          .find((b) => b.dataset.deliveryButton === e.payload?.button));
      }));
      uns.push(await listen<{ provider?: unknown; field?: unknown; index?: unknown; value?: unknown }>("type-delivery-field", (e) => {
        const { provider, field, index, value } = e.payload ?? {};
        if (typeof value !== "string") return;
        const el = Array.from(row(provider)?.querySelectorAll<HTMLElement>("[data-delivery-input]") ?? []).find(
          (x) => x.dataset.deliveryInput === field && (index == null ? x.dataset.index === undefined : x.dataset.index === String(index)),
        );
        const ctl = el?.matches("input, select, textarea") ? el : el?.querySelector<HTMLElement>("input, select, textarea");
        if (!ctl || (ctl as HTMLInputElement).disabled) return;
        // The control's own value setter, then its own event: React sees a person typing.
        const proto = ctl instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype
          : ctl instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
        Object.getOwnPropertyDescriptor(proto, "value")?.set?.call(ctl, value);
        ctl.dispatchEvent(new Event(ctl instanceof HTMLSelectElement ? "change" : "input", { bubbles: true }));
      }));
      uns.push(await listen<{ field?: unknown; value?: unknown }>("fill-check", (e) => {
        const { field, value } = e.payload ?? {};
        const r = root.current;
        if (typeof field !== "string" || typeof value !== "string" || !r) return;
        setChecksTaken((n) => n + 1);
        if (field === "by") {
          press(Array.from(r.querySelectorAll<HTMLElement>("[data-check-by]")).find((b) => b.dataset.checkBy === value));
          return;
        }
        const at = r.querySelector<HTMLElement>(`[data-check-${field}]`);
        const ctl = at?.matches("input, select") ? at : at?.querySelector<HTMLElement>("input, select");
        if (!ctl || (ctl as HTMLInputElement).disabled) return;
        const proto = ctl instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
        Object.getOwnPropertyDescriptor(proto, "value")?.set?.call(ctl, value);
        ctl.dispatchEvent(new Event(ctl instanceof HTMLSelectElement ? "change" : "input", { bubbles: true }));
      }));
      uns.push(await listen<{ button?: unknown }>("press-check", (e) => {
        const r = root.current;
        if (!r) return;
        setChecksTaken((n) => n + 1);
        press(r.querySelector(e.payload?.button === "details" ? "button[data-check-details]" : "button[data-check-run]"));
      }));
      if (cancelled) uns.forEach((u) => u());
      else setDoorsReady(true);
    })();
    return () => {
      cancelled = true;
      uns.forEach((u) => u());
    };
  }, []);

  // Report what the view rendered, read back off its own nodes after the commit.
  const reportKey = JSON.stringify([
    src, pausedError, configError, descs, settings, loadError, busy, errors, notices, form, hosts, check, checkOut, checkError,
    checkBusy, details, recent, recentError, doorsReady, checksTaken, checksStarted,
  ]);
  const lastReport = useRef("");
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    const text = (sel: string, at: ParentNode = el) => at.querySelector(sel)?.textContent ?? null;
    const cells = (row: Element) => {
      const cell: Record<string, string | null> = {};
      row.querySelectorAll<HTMLElement>("[data-col]").forEach((c) => {
        cell[c.dataset.col!] = c.textContent;
      });
      return cell;
    };
    const where = Array.from(el.querySelectorAll<HTMLElement>("[data-source-row]")).map((r) => {
      const host = r.dataset.sourceRow!;
      const shownRow = src.rows[host];
      const btn = r.querySelector<HTMLButtonElement>("button[data-source-use], button[data-source-unbind]");
      return {
        host,
        kind: r.dataset.kind ?? null,
        who: text("[data-where-who]", r),
        source_name: shownRow?.source ? shownRow.source.name : null,
        url: text("[data-where-url]", r),
        not_delivering: !!r.querySelector("[data-where-not-delivering]"),
        detail: text("[data-where-detail]", r),
        this_machine: !!shownRow?.this_machine,
        connected: !!shownRow?.connected,
        paused: !!shownRow?.paused,
        paused_shown: !!r.querySelector("[data-where-paused]"),
        stream: shownRow?.stream ?? null,
        busy: !!src.moving[host],
        retrying: !!shownRow?.busy,
        unreachable: !!shownRow?.unreachable,
        can_use: !!r.querySelector("button[data-source-use]:not([disabled])"),
        can_stop: !!r.querySelector("button[data-source-unbind]:not([disabled])"),
        action: btn ? { id: btn.dataset.sourceUse !== undefined ? "use" : "unbind", label: btn.textContent, enabled: !btn.disabled } : null,
        actions: Array.from(r.querySelectorAll<HTMLButtonElement>("button[data-source-use], button[data-source-unbind]"))
          .map((b) => ({ id: b.dataset.sourceUse !== undefined ? "use" : "unbind", label: b.textContent, enabled: !b.disabled })),
        error: text("[data-source-error]", r),
        text: r.textContent,
      };
    });
    const editor = (() => {
      const ed = el.querySelector<HTMLElement>("[data-delivery-editor]");
      if (!ed) return null;
      const values: Record<string, unknown> = {};
      ed.querySelectorAll<HTMLElement>("[data-delivery-input]").forEach((x) => {
        const ctl = (x.matches("input, select, textarea") ? x : x.querySelector("input, select, textarea")) as HTMLInputElement | null;
        const key = x.dataset.index === undefined ? x.dataset.deliveryInput! : `${x.dataset.deliveryInput}[${x.dataset.index}]`;
        values[key] = x.dataset.sensitive !== undefined ? "redacted" : (ctl?.value ?? null);
      });
      if (ed.querySelector("[data-delivery-command-set]")) values.passphrase_command = "set";
      const buttons: Record<string, boolean> = {};
      ed.querySelectorAll<HTMLElement>("[data-delivery-button]").forEach((b) => {
        const btn = b instanceof HTMLButtonElement ? b : b.querySelector("button");
        const role = btn?.getAttribute("role");
        if (role === "switch" || role === "checkbox" || role === "radio") {
          values[b.dataset.deliveryButton!] = btn?.getAttribute("aria-checked") === "true";
        }
        buttons[b.dataset.deliveryButton!] = !!btn && !btn.disabled;
      });
      return {
        provider: ed.dataset.deliveryEditor,
        mode: ed.dataset.mode,
        values,
        buttons,
        missing: Array.from(ed.querySelectorAll("[data-delivery-missing]")).map((m) => m.textContent),
        save: text("[data-delivery-button=\"save\"]", ed),
      };
    })();
    const pv = el.querySelector("[data-check-result]");
    // A Check control's value as rendered (null when it is not shown).
    const control = (sel: string) => {
      const at = el.querySelector<HTMLElement>(sel);
      const ctl = (at?.matches("input, select") ? at : at?.querySelector("input, select")) as HTMLInputElement | null;
      return ctl ? ctl.value : null;
    };
    const report: Record<string, unknown> = {
      doors_ready: doorsReady,
      where: {
        this_machine_name: text("[data-source-this-machine-name]"),
        this_machine_error: src.thisMachineError,
        paused_error: text("[data-source-paused-error]"),
        config_error: text("[data-source-config-error]"),
        rows: where,
      },
      what: {
        path: settings?.path ?? null,
        exists: settings?.exists ?? null,
        error: text("[data-delivery-settings-error]"),
        load_error: loadError,
        providers: Array.from(el.querySelectorAll<HTMLElement>("[data-delivery-provider]")).map((row) => {
          const name = row.dataset.deliveryProvider!;
          const p = providerOf(name);
          return {
            name,
            title: text("[data-what-title]", row),
            state: row.dataset.state ?? null,
            state_text: text("[data-what-state]", row),
            configured: p?.configured ?? false,
            enabled: p ? p.enabled : null,
            // The settings the row was drawn from (`delivery.settings`'s view: paths and
            // switches, a stored command as "set").
            config: p?.config ?? null,
            lands: text("[data-what-lands]", row),
            scope: text("[data-what-scope]", row),
            missing: text("[data-what-missing]", row),
            actions: Array.from(row.querySelectorAll<HTMLButtonElement>("[data-what-actions] button[data-delivery-button]"))
              .filter((b) => !b.disabled).map((b) => b.dataset.deliveryButton),
            editing: !!row.querySelector("[data-delivery-editor]"),
            error: text("[data-delivery-error]", row),
            notice: text("[data-delivery-notice]", row),
            busy: !!busy[name],
          };
        }),
        editor,
        draft: form && descOf(form.provider)
          ? redactedConfig(descOf(form.provider)!, form.draft, saveEnabled(form.mode, providerOf(form.provider)))
          : null,
      },
      check: {
        note: text("[data-check-note]"),
        by: check.by,
        shed: control("[data-check-shed]"),
        server: control("[data-check-server]"),
        backend: control("[data-check-backend]"),
        repo: control("[data-check-repo]"),
        run: (() => {
          const b = el.querySelector<HTMLButtonElement>("button[data-check-run]");
          return b ? { text: b.textContent, enabled: !b.disabled } : null;
        })(),
        busy: checkBusy,
        taken: checksTaken,
        started: checksStarted,
        error: text("[data-check-error]"),
        about: pv ? text("[data-check-about]") : null,
        items: Array.from(el.querySelectorAll<HTMLElement>("[data-check-item]")).map((r) => ({ name: r.dataset.checkItem, ...cells(r) })),
        problems: Array.from(el.querySelectorAll("[data-check-problem]")).map((p) => p.textContent),
        left_out: text("[data-check-left-out]"),
        details,
        rendered: pv?.textContent ?? "",
      },
      recent: {
        rows: Array.from(el.querySelectorAll<HTMLElement>("[data-recent-row]")).map((r) => ({ key: r.dataset.recentRow, ...cells(r) })),
        empty: text("[data-recent-empty]"),
        error: text("[data-recent-error]"),
      },
      rendered: readText(el),
    };
    const json = JSON.stringify(report);
    if (json === lastReport.current) return;
    lastReport.current = json;
    reportCredentials(report);
  }, [reportKey]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => reportCredentials(null), []);

  const fileError = settings?.error ?? null;
  const note = checkNote(checkRuns(providers));
  const pausedWords = pausedErrorText(pausedError);
  const configWords = configErrorText(configError);

  return (
    <div ref={root} data-scroll-anchor="credentials">
      <PageHead
        title="Credentials"
        sub="What your sheds get, and who gives it to them."
        right={<RefreshHeadButton onClick={load} />}
      />

      {/* 1. Where */}
      <div data-scroll-anchor="credentials-where" />
      <Section title="Where" id="credentials-where">
        <div className="px-3.5 py-2.5 text-[12px] text-shed-text-muted" data-source-this-machine>
          This machine:{" "}
          <span className="font-medium text-shed-text" data-source-this-machine-name>
            {src.thisMachine ? clean(src.thisMachine.name) : "unknown"}
          </span>
          {src.thisMachineError && (
            <div className="mt-1 font-medium text-shed-danger" data-source-identity-error>
              This machine has no identity, so it cannot answer: {clean(src.thisMachineError)}
            </div>
          )}
          {pausedWords && (
            <div className="mt-1 font-medium" style={{ color: "var(--shed-warn-fg)" }} data-source-paused-error>
              {pausedWords}
            </div>
          )}
          {configWords && (
            <div className="mt-1 font-medium" style={{ color: "var(--shed-warn-fg)" }} data-source-config-error>
              {configWords}
            </div>
          )}
        </div>
        {rows.map((r) => (
          <WhereLine
            key={r.host}
            row={r}
            src={src}
            onUse={() => sources.use(r.host)}
            onStop={() => sources.stop(r.host)}
          />
        ))}
        {rows.length === 0 && (
          <div className="border-t border-shed-border px-3.5 py-2.5 text-[12px] text-shed-text-muted" data-where-empty>
            {settled ? "No server could be read. Come back to this view to read them again." : "Reading your servers…"}
          </div>
        )}
      </Section>
      <Footnote>
        One source answers you on each server. Where the account service answers, no machine
        does. Elsewhere, this machine answers only while the app runs, and nothing fails over on
        its own.
      </Footnote>

      {/* 2. What */}
      <div data-scroll-anchor="credentials-what" />
      <Section title="What your sheds get" id="credentials-what">
        {loadError && (
          <div className="px-3.5 py-2.5 text-[12px] font-medium text-shed-danger" data-delivery-load-error>
            The settings are unavailable: {loadError}
          </div>
        )}
        {fileError && (
          <div className="px-3.5 py-2.5">
            <div className="text-[12px] font-medium text-shed-danger" data-delivery-settings-error>{fileError}</div>
            <div className="mt-1 text-[12px] text-shed-text-muted">Fix the file by hand; nothing here changes it until it reads.</div>
          </div>
        )}
        {settings && !fileError && descs.map((d) => {
          const p = providerOf(d.name);
          const v = whatView(d, p);
          const open = form?.provider === d.name ? form : null;
          return (
            <div key={d.name} className="border-t border-shed-border px-3.5 py-2.5 first:border-t-0" data-delivery-provider={d.name} data-state={v.state}>
              <div className="flex items-start gap-3">
                <div className="min-w-0 flex-1">
                  <div className="flex items-baseline gap-2">
                    <span className="text-[13px] font-medium text-shed-text" data-what-title>{v.title}</span>
                    <span
                      className="text-[11.5px] font-semibold"
                      style={{ color: v.state === "on" ? "var(--shed-ok-fg)" : "var(--shed-text-muted)" }}
                      data-what-state
                    >
                      {v.stateText}
                    </span>
                  </div>
                  <div className="text-[12px] leading-snug text-shed-text-muted">{v.summary}</div>
                  <div className="mt-0.5 text-[11.5px] text-shed-text-secondary">
                    Lands in <span className="font-mono" data-what-lands>{v.lands}</span>
                  </div>
                  {v.scope && (
                    <div className="mt-0.5 text-[11.5px] text-shed-text-secondary">
                      <span data-what-scope>{v.scope}</span>
                    </div>
                  )}
                  {v.missing.length > 0 && !open && (
                    <div className="mt-0.5 text-[11.5px]" style={{ color: "var(--shed-warn-fg)" }} data-what-missing>
                      {v.missing.join(" ")}
                    </div>
                  )}
                </div>
                {!open && (
                  <div className="flex flex-none items-center gap-1.5" data-what-actions>
                    {v.actions.map((a) => (
                      <button key={a} type="button" className={prefsButton} disabled={!!busy[d.name]}
                        style={{ opacity: busy[d.name] ? 0.5 : 1 }}
                        onClick={() => act(d.name, a)} data-delivery-button={a}>
                        {ACTION_LABEL[a]}
                      </button>
                    ))}
                  </div>
                )}
              </div>
              {open && (
                <ProviderForm
                  desc={d}
                  form={open}
                  current={p}
                  ticksFor={ticksFor}
                  setDraft={setDraft}
                  busy={!!busy[d.name]}
                  onSave={save}
                  onCancel={() => {
                    setForm(null);
                    setErrors((e) => ({ ...e, [d.name]: "" }));
                  }}
                />
              )}
              {notices[d.name] && (
                <div className="mt-1.5 text-[12px]" style={{ color: "var(--shed-warn-fg)" }} data-delivery-notice>{notices[d.name]}</div>
              )}
              {errors[d.name] && (
                <div className="mt-1.5 text-[12px] font-medium text-shed-danger" data-delivery-error>{errors[d.name]}</div>
              )}
            </div>
          );
        })}
        {settings && (
          <div className="border-t border-shed-border px-3.5 py-2 font-mono text-[11px] text-shed-text-muted" data-delivery-path>
            {settings.path}
          </div>
        )}
      </Section>
      <Footnote>
        What this machine sends where it answers, read when a shed is created and when you
        deliver again. Values are never shown here: only names, places, sizes and fingerprints.
      </Footnote>

      {/* 3. Check */}
      <div data-scroll-anchor="credentials-check" />
      <Section title="Check" id="credentials-check">
        <div className="flex flex-col gap-2 px-3.5 py-2.5">
          <div className="w-[260px]">
            <Segmented
              options={[["shed", "A shed"], ["target", "A server"]]}
              value={check.by}
              set={(v) => {
                const by = v === "target" ? "target" : "shed";
                if (by === check.by) return;
                // Another kind of Check: the last one's result is not about it.
                setCheck((c) => ({ ...c, by }));
                setCheckOut(null);
                setCheckError(null);
                setDetails(false);
              }}
              mark="check-by"
            />
          </div>
          <div className="flex items-center gap-2">
            {check.by === "shed" ? (
              <span className="min-w-0 flex-1" data-check-shed>
                <Select
                  value={check.shed}
                  onChange={(v) => setCheck((c) => ({ ...c, shed: v }))}
                  options={sheds.length
                    ? sheds.map((s) => ({ value: shedPick(s.host, s.name), label: `${s.name} on ${s.host}` }))
                    : [{ value: "", label: "No sheds" }]}
                />
              </span>
            ) : (
              <>
                <span className="w-[150px] flex-none" data-check-server>
                  <Select
                    value={check.server}
                    onChange={(v) => setCheck((c) => ({ ...c, server: v }))}
                    options={hosts.length ? hosts.map((h) => ({ value: h, label: h })) : [{ value: "", label: "No servers" }]}
                  />
                </span>
                <span className="w-[140px] flex-none" data-check-backend>
                  <Select
                    value={check.backend}
                    onChange={(v) => setCheck((c) => ({ ...c, backend: v }))}
                    options={BACKENDS.map((b) => ({ value: b, label: BACKEND_LABEL[b] }))}
                  />
                </span>
                <input
                  value={check.repo}
                  onChange={(e) => setCheck((c) => ({ ...c, repo: e.target.value }))}
                  placeholder="owner/name (optional)"
                  className={prefsInput}
                  data-check-repo
                />
              </>
            )}
            <button
              type="button"
              className={prefsButton}
              disabled={checkBusy || !settings}
              style={{ opacity: checkBusy || !settings ? 0.5 : 1 }}
              onClick={() => runCheck(check)}
              data-check-run
            >
              {checkBusy ? "Checking…" : "Check"}
            </button>
          </div>
          <div className="text-[12px] leading-snug text-shed-text-muted" data-check-note>{note}</div>
        </div>
        {checkError && (
          <div className="border-t border-shed-border px-3.5 py-2.5 text-[12px] font-medium text-shed-danger" data-check-error>
            {checkError}
          </div>
        )}
        {checkOut && !checkError && (
          <div className="border-t border-shed-border" data-check-result>
            <div className="flex items-center gap-2 px-3.5 py-2 text-[12px] text-shed-text-muted">
              <span className="min-w-0 flex-1">
                <span className="font-medium text-shed-text" data-check-about>{checkOut.about}</span>
                {" · "}
                {checkOut.items.length} {checkOut.items.length === 1 ? "item" : "items"}
              </span>
              {checkOut.items.length > 0 && (
                <button type="button" onClick={() => setDetails((x) => !x)}
                  className="hlink inline-flex flex-none items-center gap-1 rounded-md px-1 py-0.5 text-[12px] font-medium text-shed-text-secondary"
                  data-check-details>
                  {details ? <ChevronDown size={13} /> : <ChevronRight size={13} />} Details
                </button>
              )}
            </div>
            {checkOut.items.map((it) => (
              <div key={it.name} className="border-t border-shed-border px-3.5 py-2" data-check-item={it.name}>
                <div className="text-[12.5px] text-shed-text" data-col="words">{it.words}</div>
                {details && (
                  <div className="mt-0.5 flex flex-wrap items-baseline gap-x-3 font-mono text-[11.5px] text-shed-text-muted">
                    <span data-col="target">{it.details.target}</span>
                    <span>{it.details.kind} · stage <span data-col="stage">{it.details.stage}</span></span>
                    {it.details.mode && <span data-col="mode">{it.details.mode}</span>}
                    <span data-col="size">{it.details.size}</span>
                    <span title="fingerprint" data-col="fingerprint">{it.details.fingerprint}</span>
                  </div>
                )}
              </div>
            ))}
            {checkOut.problems.map((p, i) => (
              <div key={`p${i}`} className="border-t border-shed-border px-3.5 py-2 text-[12px] leading-snug"
                style={{ color: "var(--shed-warn-fg)" }} data-check-problem>
                {p}
              </div>
            ))}
            {checkOut.leftOut.length > 0 && (
              <div className="border-t border-shed-border px-3.5 py-2 text-[12px] text-shed-text-muted" data-check-left-out>
                Left out by their scope: {checkOut.leftOut.join(", ")}
              </div>
            )}
          </div>
        )}
      </Section>
      <Footnote>
        A scope on a backend or a repo trusts what the server says about the shed.
      </Footnote>

      {/* 4. Recent deliveries */}
      <div data-scroll-anchor="credentials-recent" />
      <Section title="Recent deliveries" id="credentials-recent">
        {recentError && (
          <div className="px-3.5 py-2.5 text-[12px] font-medium text-shed-danger" data-recent-error>{recentError}</div>
        )}
        {recent && recent.length === 0 && !recentError && (
          <div className="px-3.5 py-2.5 text-[12px] text-shed-text-muted" data-recent-empty>
            This machine has answered no request since the app started.
          </div>
        )}
        {(recent ?? []).map((r) => (
          <div key={r.key} className="border-t border-shed-border px-3.5 py-2 first:border-t-0" data-recent-row={r.key}>
            <div className="flex items-baseline gap-2 text-[12.5px]">
              <span className="font-medium text-shed-text" data-col="shed">{r.shed}</span>
              <span className="text-shed-text-muted">on <span data-col="server">{r.server}</span></span>
              <span className="text-shed-text-muted" data-col="reason">{r.reason}</span>
              <span className="flex-1" />
              <span className="font-mono text-[11.5px] text-shed-text-muted" data-col="when">{r.when}</span>
            </div>
            <div className="mt-0.5 text-[11.5px] text-shed-text-secondary">
              <span data-col="items">{r.items}</span> · <span data-col="outcome">{r.outcome}</span>
            </div>
            {r.problems.length > 0 && (
              <div className="mt-0.5 text-[11.5px]" style={{ color: "var(--shed-warn-fg)" }} data-col="problems">
                Problems: {r.problems.join(", ")}
              </div>
            )}
          </div>
        ))}
      </Section>
      <Footnote>
        The answers this machine gave since the app started, newest first. Removing a provider
        does not remove what sheds already have.
      </Footnote>
    </div>
  );
}

/** One Where row. A machine's name came from another machine: a text node only. */
function WhereLine({ row, src, onUse, onStop }: {
  row: WhereRow; src: SourcesState; onUse: () => void; onStop: () => void;
}) {
  const moving = !!src.moving[row.host];
  const v = whereView(row, {
    moving,
    canUse: canUse(src, row.host),
    refusal: src.errors[row.host] ?? null,
  });
  return (
    <div className="flex items-center gap-3 border-t border-shed-border px-3.5 py-2.5" data-source-row={row.host} data-kind={v.kind}>
      <div className="min-w-0 flex-1">
        <div className="truncate text-[13px] text-shed-text">{row.host}</div>
        <div className="truncate text-[12px] text-shed-text-muted">
          <span data-where-who>{v.who}</span>
          {v.name !== null && <> <span className="text-shed-text">{v.name}</span></>}
          {v.url && <> · <span className="font-mono" data-where-url>{v.url}</span></>}
        </div>
        {v.detail && (
          <div
            className="truncate text-[12px]"
            style={{ color: v.notDelivering || v.unreachable ? "var(--shed-warn-fg)" : "var(--shed-text-muted)" }}
            data-where-detail
            {...(v.notDelivering ? { "data-where-not-delivering": "" } : {})}
            {...(v.unreachable ? { "data-source-unreachable": "" } : {})}
            {...(v.paused ? { "data-where-paused": "" } : {})}
          >
            {v.detail}
          </div>
        )}
        {v.error && <div className="mt-1 text-[12px] font-medium text-shed-danger" data-source-error>{v.error}</div>}
      </div>
      {v.actions.map((action) => (
        <button
          key={action.id}
          type="button"
          onClick={action.id === "use" ? onUse : onStop}
          disabled={!action.enabled}
          className="hbtn flex-none rounded-[8px] border border-shed-border bg-shed-inset px-2.5 py-1 text-[12px] font-medium text-shed-text"
          style={{ opacity: action.enabled ? 1 : 0.5, cursor: action.enabled ? "pointer" : "default" }}
          {...(action.id === "use" ? { "data-source-use": row.host } : { "data-source-unbind": row.host })}
        >
          {action.label}
        </button>
      ))}
    </div>
  );
}

/* ---- a provider's form -------------------------------------------------------- */

/** One string of a setting, exactly as typed: a textarea, so a line break inside an
 *  entry is kept, one line tall until it holds more. */
function Entry({ value, onChange, placeholder, field, index, sensitive }: {
  value: string; onChange: (v: string) => void; placeholder?: string;
  field: string; index?: number; sensitive?: boolean;
}) {
  return (
    <textarea
      rows={Math.max(1, value.split("\n").length)}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={placeholder}
      spellCheck={false}
      autoComplete="off"
      className={`${prefsInput} resize-none`}
      style={sensitive ? ({ WebkitTextSecurity: "disc" } as React.CSSProperties) : undefined}
      data-delivery-input={field}
      data-index={index}
      data-sensitive={sensitive ? "" : undefined}
    />
  );
}

/** A list setting, one entry per row, each edited, added and removed on its own. */
function ListEditor({ name, items, set, placeholder, sensitive, addLabel = "Add" }: {
  name: string; items: string[]; set: (f: (d: ProviderDraft) => ProviderDraft) => void;
  placeholder: string; sensitive?: boolean; addLabel?: string;
}) {
  return (
    <div className="flex flex-col gap-1">
      {items.map((v, i) => (
        <div key={i} className="flex items-start gap-1.5">
          <Entry value={v} onChange={(x) => set((d) => edit.entry(d, name, i, x))} placeholder={placeholder}
            field={name} index={i} sensitive={sensitive} />
          <button type="button" title="Remove" onClick={() => set((d) => edit.remove(d, name, i))}
            className="hlink mt-1 flex h-6 w-6 flex-none items-center justify-center rounded-md text-shed-text-muted"
            data-delivery-button={`remove:${name}:${i}`}>
            <X size={14} />
          </button>
        </div>
      ))}
      <button type="button" onClick={() => set((d) => edit.add(d, name))}
        className="hlink inline-flex w-fit items-center gap-1 rounded-md px-1 py-0.5 text-[12px] font-medium text-shed-text-secondary"
        data-delivery-button={`add:${name}`}>
        <Plus size={13} /> {addLabel}
      </button>
    </div>
  );
}

/** A tick or a radio: a button with its role and state, so a press is a click. */
function Mark({ on, role, label, button, onClick }: {
  on: boolean; role: "checkbox" | "radio"; label: React.ReactNode; button: string; onClick: () => void;
}) {
  return (
    <button type="button" role={role} aria-checked={on} onClick={onClick}
      className="hlink flex items-center gap-2 rounded-md px-1 py-0.5 text-left text-[12.5px] text-shed-text"
      data-delivery-button={button}>
      <span
        className={role === "radio" ? "flex h-[14px] w-[14px] flex-none items-center justify-center rounded-full border" : "flex h-[14px] w-[14px] flex-none items-center justify-center rounded-[4px] border"}
        style={{ borderColor: on ? "var(--shed-accent)" : "var(--shed-border-strong)", background: on && role === "checkbox" ? "var(--shed-accent)" : "transparent" }}
      >
        {on && (role === "radio"
          ? <span className="h-[7px] w-[7px] rounded-full" style={{ background: "var(--shed-accent)" }} />
          : <span className="text-[10px] leading-none" style={{ color: "var(--shed-accent-fg)" }}>✓</span>)}
      </span>
      <span>{label}</span>
    </button>
  );
}

/** A scope's editor: every server or only the ticked ones, the backends as three
 *  ticks, the repos as patterns. `at` is the provider's scope (null) or a file's. */
function ScopeEditor({ scope, at, ticks, set, prefix }: {
  scope: ScopeDraft; at: ScopeAt; ticks: string[];
  set: (f: (d: ProviderDraft) => ProviderDraft) => void; prefix: string;
}) {
  const label = "mb-1 block text-[12px] text-shed-text-muted";
  return (
    <div className="flex flex-col gap-2" data-scope={prefix || "provider"}>
      <div>
        <span className={label}>Servers</span>
        <Mark role="radio" on={scope.every} label={EVERY_SERVER} button={`${prefix}scope:every`}
          onClick={() => set((d) => edit.every(d, at, true))} />
        <Mark role="radio" on={!scope.every} label={ONLY_THESE} button={`${prefix}scope:only`}
          onClick={() => set((d) => edit.every(d, at, false))} />
        {!scope.every && (
          <div className="ml-6 flex flex-col">
            {ticks.length === 0 && <span className="text-[12px] text-shed-text-muted">No server this machine can answer on.</span>}
            {ticks.map((t) => (
              <Mark key={t} role="checkbox" on={scope.servers.includes(t)} label={t} button={`${prefix}server:${t}`}
                onClick={() => set((d) => edit.server(d, at, t, !scope.servers.includes(t)))} />
            ))}
          </div>
        )}
      </div>
      <div>
        <span className={label}>Backends (none ticked: every backend)</span>
        <div className="flex gap-3">
          {BACKENDS.map((b) => (
            <Mark key={b} role="checkbox" on={scope.backends.includes(b)} label={BACKEND_LABEL[b]} button={`${prefix}backend:${b}`}
              onClick={() => set((d) => edit.backend(d, at, b, !scope.backends.includes(b)))} />
          ))}
        </div>
      </div>
      <div>
        <span className={label}>Repos (owner/name patterns, * within a part; none: every repo)</span>
        <ListEditor name={at === null ? "only.repos" : `${prefix}only.repos`} items={scope.repos} set={set}
          placeholder="owner/*" addLabel="Add a pattern" />
      </div>
    </div>
  );
}

/** One provider's form, drawn from its descriptor: its fields, its scope, and (files)
 *  each file with its mode and its own scope. Save stays disabled until what the
 *  provider requires is filled in. */
function ProviderForm({ desc, form, current, ticksFor, setDraft, busy, onSave, onCancel }: {
  desc: ProviderDesc;
  form: Form;
  current: ProviderSetting | null;
  ticksFor: (scope: ScopeDraft | null) => string[];
  setDraft: (f: (d: ProviderDraft) => ProviderDraft) => void;
  busy: boolean;
  onSave: () => void;
  onCancel: () => void;
}) {
  const draft = form.draft;
  const set = setDraft;
  const label = "mb-1 block text-[12px] text-shed-text-muted";
  const need = missing(desc, draft);
  return (
    <div className="mt-2.5 flex flex-col gap-3 rounded-[8px] bg-shed-bg p-2.5" data-delivery-editor={desc.name} data-mode={form.mode}
      data-scroll-anchor="credentials-form">
      {machineFields(desc).map((f) => {
        const ctl = fieldControl(f);
        if (ctl === "switch") {
          return (
            <div key={f.key} className="flex items-center gap-3">
              <span className="text-[12.5px] text-shed-text">{f.title}</span>
              <span className="flex-1" />
              <span data-delivery-button={`toggle:${f.key}`}>
                <Switch on={draft.bools[f.key] === true} set={(v) => set((d) => edit.bool(d, f.key, v))} />
              </span>
            </div>
          );
        }
        if (ctl === "list") {
          return (
            <div key={f.key}>
              <span className={label}>{f.title}</span>
              <ListEditor name={f.key} items={draft.lists[f.key] ?? []} set={set} placeholder={f.kind === "list" ? "~/" : ""} />
            </div>
          );
        }
        if (ctl === "command") {
          // The passphrase command: a stored one is never shown back.
          const cmd = draft.command;
          return (
            <div key={f.key}>
              <span className={label}>{f.title}</span>
              {cmd?.keep ? (
                <div className="flex items-center gap-2">
                  <span className="flex-1 text-[12.5px] text-shed-text" data-delivery-command-set>
                    A command is set. Its arguments are not shown.
                  </span>
                  <button type="button" className={prefsButton} onClick={() => set(edit.replaceCommand)}
                    data-delivery-button={`replace:${f.key}`}>Replace</button>
                  <button type="button" className={prefsButton} onClick={() => set(edit.clearCommand)}
                    data-delivery-button={`clear:${f.key}`}>Clear</button>
                </div>
              ) : (
                <>
                  <ListEditor name={f.key} items={cmd?.args ?? []} set={set} placeholder="argument" sensitive />
                  {draft.commandStored && (
                    <button type="button" onClick={() => set(edit.keepCommand)}
                      className="hlink mt-1 rounded-md px-1 py-0.5 text-[12px] font-medium text-shed-text-secondary"
                      data-delivery-button={`keep:${f.key}`}>
                      Keep the command that is set
                    </button>
                  )}
                </>
              )}
            </div>
          );
        }
        if (ctl === "entries") {
          const fields = entryFields(f);
          return (
            <div key={f.key}>
              <span className={label}>{f.title}</span>
              <div className="flex flex-col gap-2">
                {draft.entries.map((e, i) => (
                  <div key={i} className="flex flex-col gap-2 rounded-[8px] border border-shed-border p-2" data-delivery-entry={i}>
                    <div className="flex items-start gap-1.5">
                      {fields.map((ef) => (fieldControl(ef) === "choice" ? (
                        <span key={ef.key} className={ef.key === "mode" ? "w-[168px] flex-none" : "w-[118px] flex-none"} data-delivery-input={`${f.key}.${ef.key}`} data-index={i}>
                          <Select value={e.values[ef.key] ?? ""} onChange={(v) => set((d) => edit.entryValue(d, i, ef.key, v))}
                            options={choiceOptions(ef)} />
                        </span>
                      ) : (
                        <Entry key={ef.key} value={e.values[ef.key] ?? ""} onChange={(v) => set((d) => edit.entryValue(d, i, ef.key, v))}
                          placeholder={ef.key === "from" ? "~/from" : ef.title.toLowerCase()} field={`${f.key}.${ef.key}`} index={i} />
                      )))}
                      <button type="button" title="Remove this file" onClick={() => set((d) => edit.remove(d, f.key, i))}
                        className="hlink mt-1 flex h-6 w-6 flex-none items-center justify-center rounded-md text-shed-text-muted"
                        data-delivery-button={`remove:${f.key}:${i}`}>
                        <X size={14} />
                      </button>
                    </div>
                    {f.scoped && (
                      <ScopeEditor scope={e.scope} at={i} ticks={ticksFor(e.scope)} set={set} prefix={`${f.key}.${i}.`} />
                    )}
                  </div>
                ))}
                <button type="button" onClick={() => set((d) => edit.add(d, f.key, desc))}
                  className="hlink inline-flex w-fit items-center gap-1 rounded-md px-1 py-0.5 text-[12px] font-medium text-shed-text-secondary"
                  data-delivery-button={`add:${f.key}`}>
                  <Plus size={13} /> Add a file
                </button>
              </div>
            </div>
          );
        }
        return (
          <div key={f.key}>
            <span className={label}>{f.title}{f.required ? "" : " (optional)"}</span>
            <Entry value={draft.text[f.key] ?? ""} onChange={(v) => set((d) => edit.text(d, f.key, v))}
              placeholder={typeof f.default === "string" ? f.default : f.kind === "path" ? "~/" : ""} field={f.key} />
            <span className="mt-0.5 block text-[11.5px] leading-snug text-shed-text-muted">{f.help}</span>
          </div>
        );
      })}
      <div>
        <span className="mb-1 block text-[12px] font-medium text-shed-text">Scope</span>
        <ScopeEditor scope={draft.scope} at={null} ticks={ticksFor(draft.scope)} set={set} prefix="" />
      </div>
      {need.length > 0 && (
        <div className="flex flex-col gap-0.5">
          {need.map((m) => (
            <div key={m} className="text-[12px]" style={{ color: "var(--shed-warn-fg)" }} data-delivery-missing>{m}</div>
          ))}
        </div>
      )}
      <div className="flex items-center justify-end gap-2">
        <button type="button" onClick={onCancel} className="hlink rounded-[8px] px-2.5 py-1 text-[12px] font-medium text-shed-text-secondary"
          data-delivery-button="cancel">
          Cancel
        </button>
        <button
          type="button"
          onClick={onSave}
          disabled={busy || need.length > 0}
          className="hbtn rounded-[8px] px-3 py-1 text-[12px] font-semibold"
          style={{ background: "var(--shed-accent)", color: "var(--shed-accent-fg)", opacity: busy || need.length > 0 ? 0.5 : 1 }}
          data-delivery-button="save"
        >
          {busy ? "Saving…" : saveLabel(form.mode, current)}
        </button>
      </div>
    </div>
  );
}
