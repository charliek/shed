/* Preferences → Provisioning (plan 039 §3.10, A7): each of the five providers on or
   off, with its settings and its `only` rule (`delivery.settings` / `delivery.set`);
   the credential source rows the window passes in; and a preview of what a named
   shed would get (`delivery.preview`): names, targets, stages, sizes, fingerprints
   and problems, never a value. A settings file that cannot be used is shown with
   the fixed words and the place its error names; it is never quoted.

   A passphrase command may hold the passphrase itself, so a stored one is never shown:
   it reads "set", and the editor offers Replace (type a new argv) or Clear; saving
   anything else keeps it exactly (`delivery.set` takes "set" back as "unchanged").

   Driven over IPC by `ui.prefs_section` (scroll to a section), and, in test mode, the
   doors that act on the real controls: `ui.press_delivery_toggle` (a provider's
   switch), `ui.press_delivery_button` (Edit, Save, Cancel, add/remove an entry, the
   command's Replace/Clear/Keep, an editor switch), `ui.type_delivery_field` (types
   into the real input) and `ui.preview_delivery`. Read back through `prefs.dump`'s
   `values.provisioning`, built off the rendered nodes (command arguments redacted). */
import { useCallback, useEffect, useRef, useState } from "react";
import { Plus, X } from "lucide-react";
import { Select, Switch } from "@/components/dialog";
import { Footnote, Section, prefsButton, prefsInput } from "@/components/prefsKit";
import {
  inTauri, fetchHosts, getDeliverySettings, getDeliveryStatus, previewDelivery, setDeliveryProvider,
  type DeliverySettings,
} from "@/lib/bridge";
import {
  ONLY_KEYS, PROVIDERS, PROVIDER_DETAIL, PROVIDER_FIELDS, PROVIDER_LABEL,
  configFromDraft, draftFromConfig, edit, formatSize, onlyText, redactedConfig,
  type ProviderDraft,
} from "@/lib/delivery";

/** The text a person reads in `el`, without what form controls hold: a textarea's
 *  value is also its text node, and a passphrase command's arguments must never be
 *  read back. */
function readText(el: Element): string {
  const copy = el.cloneNode(true) as Element;
  copy.querySelectorAll("textarea, input, select").forEach((x) => x.remove());
  return copy.textContent ?? "";
}

const errText = (e: unknown): string =>
  typeof e === "string" ? e : e instanceof Error ? e.message : String(e);

type Stream = { server: string; state: string };
type PreviewItem = {
  name: string; provider: string; kind: string; stage: number; target: string;
  mode?: string; size: number; fingerprint: string; expires_at?: string;
};
type Preview = {
  server: string;
  shed: string | null;
  request?: { backend?: string; repo?: string | null } | null;
  preview: {
    items: PreviewItem[];
    problems: { provider: string; code: string; message: string }[];
    providers: { provider: string; ran: boolean; restricted: boolean }[];
  };
};

/** What the section rendered, for `prefs.dump` (`values.provisioning`). */
export type ProvisioningReport = Record<string, unknown>;

export function ProvisioningSection({ reloadKey, sources, sourcesKey, onReport }: {
  /** Bumped by the window when its values should be read again. */
  reloadKey: number;
  /** The credential source rows, shown between the providers and the preview. */
  sources: React.ReactNode;
  /** Changes whenever the source rows do, so the report reads them again. */
  sourcesKey: string;
  onReport: (r: ProvisioningReport) => void;
}) {
  const root = useRef<HTMLDivElement>(null);
  const [settings, setSettings] = useState<DeliverySettings | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [streams, setStreams] = useState<Stream[]>([]);
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [editing, setEditing] = useState<string | null>(null);
  const [draft, setDraft] = useState<ProviderDraft | null>(null);
  // The provider's config the open draft was made from: a reload that finds it
  // changed discards the draft (and says so), so Save never writes over a change
  // made elsewhere, nor an `enabled` the switch no longer shows.
  const [draftBase, setDraftBase] = useState<string | null>(null);
  const [notices, setNotices] = useState<Record<string, string>>({});
  const [hosts, setHosts] = useState<string[]>([]);
  const [pvServer, setPvServer] = useState("");
  const [pvShed, setPvShed] = useState("");
  const [preview, setPreview] = useState<Preview | null>(null);
  const [pvError, setPvError] = useState<string | null>(null);
  const [pvBusy, setPvBusy] = useState(false);
  const [shownSection, setShownSection] = useState<string | null>(null);
  const loadGen = useRef(0);

  const load = useCallback(() => {
    const gen = ++loadGen.current;
    // A discarded draft is said until the window is read again.
    setNotices({});
    void getDeliverySettings()
      .then((s) => {
        if (gen !== loadGen.current) return;
        setSettings(s);
        setLoadError(null);
      })
      .catch((e) => {
        if (gen === loadGen.current) setLoadError(errText(e));
      });
    void getDeliveryStatus()
      .then((st) => {
        if (gen !== loadGen.current) return;
        const servers = Array.isArray(st.servers) ? (st.servers as Array<Record<string, unknown>>) : [];
        setStreams(servers.map((s) => ({ server: String(s.server ?? ""), state: String(s.state ?? "") })));
      })
      .catch(() => {});
    void fetchHosts().then((h) => {
      if (gen !== loadGen.current) return;
      setHosts(h);
      setPvServer((cur) => (cur && h.includes(cur) ? cur : (h[0] ?? "")));
    });
  }, []);
  useEffect(() => {
    if (inTauri()) load();
  }, [load, reloadKey]);

  const providerView = (name: string) => settings?.providers.find((p) => p.name === name) ?? null;

  // A switch: the provider's table as it is, with `enabled` flipped. The answer is
  // the file as written, so the switch shows what is on disk.
  const toggle = (name: string) => {
    if (busy[name]) return;
    const p = providerView(name);
    const config = { ...(p?.config ?? {}), enabled: !(p?.enabled ?? false) };
    setBusy((b) => ({ ...b, [name]: true }));
    setErrors((e) => ({ ...e, [name]: "" }));
    void setDeliveryProvider(name, config)
      .then((s) => setSettings(s))
      .catch((e) => setErrors((x) => ({ ...x, [name]: errText(e) })))
      .finally(() => setBusy((b) => ({ ...b, [name]: false })));
  };
  const closeEditor = () => {
    setEditing(null);
    setDraft(null);
    setDraftBase(null);
  };
  const startEdit = (name: string) => {
    const config = providerView(name)?.config ?? null;
    setEditing(name);
    setDraft(draftFromConfig(name, config));
    setDraftBase(JSON.stringify(config));
    setErrors((e) => ({ ...e, [name]: "" }));
    setNotices((n) => ({ ...n, [name]: "" }));
  };
  useEffect(() => {
    if (!editing || draftBase === null || !settings) return;
    const now = JSON.stringify(providerView(editing)?.config ?? null);
    if (now === draftBase) return;
    const name = editing;
    closeEditor();
    setNotices((n) => ({ ...n, [name]: "The settings changed while you were editing; your unsaved changes were discarded." }));
  }, [settings]); // eslint-disable-line react-hooks/exhaustive-deps
  // Save writes the draft with the switch's `enabled`, as shown.
  const save = (name: string) => {
    if (!draft || editing !== name || busy[name]) return;
    setBusy((b) => ({ ...b, [name]: true }));
    void setDeliveryProvider(name, configFromDraft(name, draft, providerView(name)?.enabled ?? false))
      .then((s) => {
        setSettings(s);
        closeEditor();
        setErrors((e) => ({ ...e, [name]: "" }));
      })
      .catch((e) => setErrors((x) => ({ ...x, [name]: errText(e) })))
      .finally(() => setBusy((b) => ({ ...b, [name]: false })));
  };
  const runPreview = (server: string, shed: string) => {
    if (!server) return;
    setPvBusy(true);
    setPvError(null);
    void previewDelivery(server, shed.trim() || undefined)
      .then((p) => setPreview(p as unknown as Preview))
      .catch((e) => {
        setPreview(null);
        setPvError(errText(e));
      })
      .finally(() => setPvBusy(false));
  };

  // The doors: each acts on the real control in this window, and a disabled one
  // takes no press, as a pointer's would not.
  useEffect(() => {
    if (!inTauri()) return;
    let cancelled = false;
    const uns: Array<() => void> = [];
    const within = (sel: string, key: string, value: unknown) =>
      Array.from(root.current?.querySelectorAll<HTMLElement>(sel) ?? []).filter((el) => el.dataset[key] === value);
    void (async () => {
      const { listen } = await import("@tauri-apps/api/event");
      uns.push(
        await listen<{ section?: unknown }>("prefs-section", (e) => {
          const id = e.payload?.section;
          if (typeof id !== "string") return;
          const el = Array.from(document.querySelectorAll<HTMLElement>("[data-prefs-section]")).find(
            (x) => x.dataset.prefsSection === id,
          );
          if (!el) return;
          el.scrollIntoView({ block: "start" });
          setShownSection(id);
        }),
      );
      uns.push(
        await listen<{ provider?: unknown }>("press-delivery-toggle", (e) => {
          within("[data-delivery-toggle]", "deliveryToggle", e.payload?.provider).forEach((wrap) => {
            const b = wrap.querySelector<HTMLButtonElement>("button");
            if (b && !b.disabled) b.click();
          });
        }),
      );
      const row = (provider: unknown) =>
        within("[data-delivery-provider]", "deliveryProvider", provider)[0] ?? null;
      uns.push(
        await listen<{ provider?: unknown; button?: unknown }>("press-delivery-button", (e) => {
          const el = Array.from(row(e.payload?.provider)?.querySelectorAll<HTMLElement>("[data-delivery-button]") ?? [])
            .find((b) => b.dataset.deliveryButton === e.payload?.button);
          const b = el instanceof HTMLButtonElement ? el : el?.querySelector<HTMLButtonElement>("button");
          if (b && !b.disabled) b.click();
        }),
      );
      uns.push(
        await listen<{ provider?: unknown; field?: unknown; index?: unknown; value?: unknown }>("type-delivery-field", (e) => {
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
        }),
      );
      uns.push(
        await listen<{ server?: unknown; shed?: unknown }>("preview-delivery", (e) => {
          const server = typeof e.payload?.server === "string" ? e.payload.server : "";
          const shed = typeof e.payload?.shed === "string" ? e.payload.shed : "";
          setPvServer(server);
          setPvShed(shed);
          runPreview(server, shed);
        }),
      );
      if (cancelled) uns.forEach((u) => u());
    })();
    return () => {
      cancelled = true;
      uns.forEach((u) => u());
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  // Report what the section rendered, read back off its own nodes after the commit.
  const reportKey = JSON.stringify([
    settings, loadError, streams, busy, errors, notices, editing, draft, preview, pvError, pvBusy, shownSection,
    hosts, pvServer, pvShed, sourcesKey,
  ]);
  const lastReport = useRef("");
  useEffect(() => {
    const el = root.current;
    if (!el) return;
    const text = (sel: string) => el.querySelector(sel)?.textContent ?? null;
    const pvRoot = el.querySelector("[data-delivery-preview]");
    const report: ProvisioningReport = {
      path: settings?.path ?? null,
      exists: settings?.exists ?? null,
      error: text("[data-delivery-settings-error]"),
      load_error: loadError,
      providers: Array.from(el.querySelectorAll<HTMLElement>("[data-delivery-provider]")).map((row) => {
        const name = row.dataset.deliveryProvider!;
        const sw = row.querySelector<HTMLButtonElement>("[data-delivery-toggle] button");
        return {
          name,
          configured: providerView(name)?.configured ?? false,
          // The settings the row was drawn from (`delivery.settings`'s view: paths and
          // switches, a stored command as "set").
          config: providerView(name)?.config ?? null,
          enabled: sw ? sw.getAttribute("aria-checked") === "true" : null,
          only: row.querySelector("[data-delivery-only]")?.textContent ?? null,
          editing: !!row.querySelector("[data-delivery-editor]"),
          error: row.querySelector("[data-delivery-error]")?.textContent ?? null,
          notice: row.querySelector("[data-delivery-notice]")?.textContent ?? null,
          busy: !!busy[name],
        };
      }),
      // The open editor's controls as committed to the DOM: each input's value
      // (a command argument's redacted), each switch, each button that would press.
      editor: (() => {
        const ed = el.querySelector<HTMLElement>("[data-delivery-editor]");
        if (!ed) return null;
        const values: Record<string, unknown> = {};
        ed.querySelectorAll<HTMLElement>("[data-delivery-input]").forEach((x) => {
          const ctl = (x.matches("input, select, textarea") ? x : x.querySelector("input, select, textarea")) as
            HTMLInputElement | null;
          const key = x.dataset.index === undefined ? x.dataset.deliveryInput! : `${x.dataset.deliveryInput}[${x.dataset.index}]`;
          values[key] = x.dataset.sensitive !== undefined ? "redacted" : (ctl?.value ?? null);
        });
        if (ed.querySelector("[data-delivery-command-set]")) values.passphrase_command = "set";
        const buttons: Record<string, boolean> = {};
        ed.querySelectorAll<HTMLElement>("[data-delivery-button]").forEach((b) => {
          const btn = b instanceof HTMLButtonElement ? b : b.querySelector("button");
          if (b.getAttribute("role") === "switch" || btn?.getAttribute("role") === "switch") {
            values[b.dataset.deliveryButton!] = btn?.getAttribute("aria-checked") === "true";
          }
          buttons[b.dataset.deliveryButton!] = !!btn && !btn.disabled;
        });
        return { provider: ed.dataset.deliveryEditor, values, buttons };
      })(),
      streams: Array.from(el.querySelectorAll<HTMLElement>("[data-delivery-stream]")).map((s) => ({
        server: s.dataset.deliveryStream,
        text: s.textContent,
      })),
      editing,
      draft: editing && draft ? redactedConfig(editing, draft, providerView(editing)?.enabled ?? false) : null,
      preview: pvRoot || pvError || pvBusy
        ? {
            server: preview?.server ?? null,
            shed: preview?.shed ?? null,
            busy: pvBusy,
            error: text("[data-delivery-preview-error]"),
            items: Array.from(el.querySelectorAll<HTMLElement>("[data-preview-item]")).map((row) => {
              const cell: Record<string, string | null> = {};
              row.querySelectorAll<HTMLElement>("[data-col]").forEach((c) => {
                cell[c.dataset.col!] = c.textContent;
              });
              return cell;
            }),
            problems: Array.from(el.querySelectorAll("[data-preview-problem]")).map((p) => p.textContent),
            rendered: pvRoot?.textContent ?? "",
          }
        : null,
      shown_section: shownSection,
      rendered: readText(el),
    };
    const json = JSON.stringify(report);
    if (json === lastReport.current) return;
    lastReport.current = json;
    onReport(report);
  }, [reportKey]); // eslint-disable-line react-hooks/exhaustive-deps

  const fileError = settings?.error ?? null;
  const pvItems = preview?.preview?.items ?? [];
  const pvProblems = preview?.preview?.problems ?? [];
  const leftOut = (preview?.preview?.providers ?? []).filter((p) => !p.ran);

  return (
    <div ref={root}>
      <Section title="Provisioning" id="provisioning">
        {streams.length > 0 && (
          <div className="px-3.5 py-2.5 text-[12px] text-shed-text-muted" data-delivery-streams>
            Answering for:{" "}
            {streams.map((s, i) => (
              <span key={s.server} data-delivery-stream={s.server}>
                {i > 0 && ", "}
                <span className="font-medium text-shed-text">{s.server}</span>
                {" "}
                <span style={{ color: s.state === "connected" ? "var(--shed-ok-fg)" : "var(--shed-warn-fg)" }}>
                  ({s.state.replace(/_/g, " ")})
                </span>
              </span>
            ))}
          </div>
        )}
        {loadError && (
          <div className="border-t border-shed-border px-3.5 py-2.5 text-[12px] font-medium text-shed-danger first:border-t-0" data-delivery-load-error>
            Provisioning is unavailable: {loadError}
          </div>
        )}
        {fileError && (
          <div className="border-t border-shed-border px-3.5 py-2.5 first:border-t-0">
            <div className="text-[12px] font-medium text-shed-danger" data-delivery-settings-error>{fileError}</div>
            <div className="mt-1 text-[12px] text-shed-text-muted">
              Fix the file by hand; nothing here changes it until it reads.
            </div>
          </div>
        )}
        {settings && !fileError &&
          PROVIDERS.map((name) => {
            const p = providerView(name);
            const on = p?.enabled ?? false;
            const only = onlyText(p?.only);
            const isEditing = editing === name && draft !== null;
            const err = errors[name];
            return (
              <div key={name} className="border-t border-shed-border px-3.5 py-2.5 first:border-t-0" data-delivery-provider={name}>
                <div className="flex items-center gap-3">
                  <div className="min-w-0 flex-1">
                    <div className="text-[13px] text-shed-text">{PROVIDER_LABEL[name]}</div>
                    <div className="text-[12px] leading-snug text-shed-text-muted">{PROVIDER_DETAIL[name]}</div>
                    {only && (
                      <div className="mt-0.5 truncate font-mono text-[11.5px] text-shed-text-secondary" data-delivery-only>
                        only {only}
                      </div>
                    )}
                  </div>
                  {!isEditing && (
                    <button type="button" className={prefsButton} onClick={() => startEdit(name)} data-delivery-button="edit">
                      Edit
                    </button>
                  )}
                  <span data-delivery-toggle={name}>
                    <Switch on={on} set={() => toggle(name)} disabled={!!busy[name] || isEditing} />
                  </span>
                </div>
                {isEditing && draft && (
                  <ProviderEditor
                    name={name}
                    draft={draft}
                    setDraft={(f) => {
                      // A refusal is about the draft it refused: an edit retires it.
                      setDraft(f);
                      setErrors((e) => (e[name] ? { ...e, [name]: "" } : e));
                    }}
                    busy={!!busy[name]}
                    onSave={() => save(name)}
                    onCancel={() => {
                      closeEditor();
                      setErrors((e) => ({ ...e, [name]: "" }));
                    }}
                  />
                )}
                {notices[name] && (
                  <div className="mt-1.5 text-[12px]" style={{ color: "var(--shed-warn-fg)" }} data-delivery-notice>{notices[name]}</div>
                )}
                {err && (
                  <div className="mt-1.5 text-[12px] font-medium text-shed-danger" data-delivery-error>{err}</div>
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
        On a server where this machine is the source, each shed gets what the providers
        switched on here read, when it is created and when you deliver again. What is sent is
        never shown here: only names, places, sizes and fingerprints.
      </Footnote>

      {sources}

      <Section title="Preview" id="delivery-preview">
        <div className="flex items-center gap-2 px-3.5 py-2.5">
          <span className="w-[150px] flex-none">
            <Select
              value={pvServer}
              onChange={setPvServer}
              options={hosts.length ? hosts.map((h) => ({ value: h, label: h })) : [{ value: "", label: "No servers" }]}
            />
          </span>
          <input
            value={pvShed}
            onChange={(e) => setPvShed(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") runPreview(pvServer, pvShed);
            }}
            placeholder="shed name (optional)"
            className={prefsInput}
            data-delivery-preview-shed
          />
          <button
            type="button"
            className={prefsButton}
            disabled={!pvServer || pvBusy}
            style={{ opacity: !pvServer || pvBusy ? 0.5 : 1 }}
            onClick={() => runPreview(pvServer, pvShed)}
            data-delivery-preview-run
          >
            {pvBusy ? "Reading…" : "Preview"}
          </button>
        </div>
        {pvError && (
          <div className="border-t border-shed-border px-3.5 py-2.5 text-[12px] font-medium text-shed-danger" data-delivery-preview-error>
            {pvError}
          </div>
        )}
        {preview && !pvError && (
          <div className="border-t border-shed-border" data-delivery-preview>
            <div className="px-3.5 py-2 text-[12px] text-shed-text-muted">
              {preview.shed ? (
                <>
                  <span className="font-medium text-shed-text">{preview.shed}</span> on {preview.server}
                  {preview.request?.backend ? ` · ${preview.request.backend}` : ""}
                  {preview.request?.repo ? ` · ${preview.request.repo}` : ""}
                </>
              ) : (
                <>Everything the providers have for {preview.server}, before any only rule.</>
              )}
              {" · "}
              {pvItems.length} {pvItems.length === 1 ? "item" : "items"}
            </div>
            {pvItems.map((it) => (
              <div key={it.name} className="border-t border-shed-border px-3.5 py-2" data-preview-item={it.name}>
                <div className="flex items-baseline gap-2">
                  <span className="truncate font-mono text-[12.5px] font-semibold text-shed-text" data-col="name">{it.name}</span>
                  <span className="text-[11.5px] text-shed-text-muted" data-col="provider">{it.provider}</span>
                  <span className="flex-1" />
                  <span className="flex-none font-mono text-[11.5px] text-shed-text-secondary">
                    <span data-col="kind">{it.kind}</span> · stage <span data-col="stage">{it.stage}</span>
                    {it.mode ? <> · <span data-col="mode">{it.mode}</span></> : null}
                  </span>
                </div>
                <div className="mt-0.5 flex items-baseline gap-2 font-mono text-[11.5px]">
                  <span className="min-w-0 truncate text-shed-text" data-col="target">{it.target}</span>
                  <span className="flex-1" />
                  <span className="flex-none text-shed-text-muted" data-col="size">{formatSize(it.size)}</span>
                  <span className="flex-none text-shed-text-muted" title="fingerprint" data-col="fingerprint">{it.fingerprint}</span>
                </div>
              </div>
            ))}
            {pvProblems.map((p, i) => (
              <div
                key={`pp${i}`}
                className="border-t border-shed-border px-3.5 py-2 text-[12px] leading-snug"
                style={{ color: "var(--shed-warn-fg)" }}
                data-preview-problem
              >
                <span className="font-mono font-semibold">{p.provider}</span>: {p.message}
                <span className="font-mono text-shed-text-muted"> ({p.code})</span>
              </div>
            ))}
            {leftOut.length > 0 && (
              <div className="border-t border-shed-border px-3.5 py-2 text-[12px] text-shed-text-muted" data-preview-left-out>
                Left out by their only rule: {leftOut.map((p) => p.provider).join(", ")}
              </div>
            )}
          </div>
        )}
      </Section>
      <Footnote>
        Reads this machine as a delivery would, and sends nothing. Name a shed to apply its
        server, backend and repository to each only rule.
      </Footnote>
    </div>
  );
}

/** One string of a setting, exactly as typed: a textarea, so a line break inside an
 *  entry is kept (an `<input>` would drop it), one line tall until it holds more. */
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
function ListEditor({ name, items, set, placeholder, sensitive }: {
  name: string; items: string[]; set: (f: (d: ProviderDraft) => ProviderDraft) => void;
  placeholder: string; sensitive?: boolean;
}) {
  return (
    <div className="flex flex-col gap-1">
      {items.map((v, i) => (
        <div key={i} className="flex items-start gap-1.5">
          <Entry value={v} onChange={(x) => set((d) => edit.entry(d, name, i, x))} placeholder={placeholder}
            field={name} index={i} sensitive={sensitive} />
          <button
            type="button"
            title="Remove"
            onClick={() => set((d) => edit.remove(d, name, i))}
            className="hlink mt-1 flex h-6 w-6 flex-none items-center justify-center rounded-md text-shed-text-muted"
            data-delivery-button={`remove:${name}:${i}`}
          >
            <X size={14} />
          </button>
        </div>
      ))}
      <button
        type="button"
        onClick={() => set((d) => edit.add(d, name))}
        className="hlink inline-flex w-fit items-center gap-1 rounded-md px-1 py-0.5 text-[12px] font-medium text-shed-text-secondary"
        data-delivery-button={`add:${name}`}
      >
        <Plus size={13} /> Add
      </button>
    </div>
  );
}

/** One provider's settings, as a draft: its fields, its only rule, and (files) its
 *  entries. Save writes the provider's table; a refusal stays under the row. */
function ProviderEditor({ name, draft, setDraft, busy, onSave, onCancel }: {
  name: string;
  draft: ProviderDraft;
  setDraft: (f: (d: ProviderDraft | null) => ProviderDraft | null) => void;
  busy: boolean;
  onSave: () => void;
  onCancel: () => void;
}) {
  const set = (f: (d: ProviderDraft) => ProviderDraft) => setDraft((d) => (d ? f(d) : d));
  const label = "mb-1 block text-[12px] text-shed-text-muted";
  return (
    <div className="mt-2.5 flex flex-col gap-2.5 rounded-[8px] bg-shed-bg p-2.5" data-delivery-editor={name}>
      {(PROVIDER_FIELDS[name] ?? []).map(([key, text, kind, placeholder]) => {
        if (kind === "bool") {
          return (
            <div key={key} className="flex items-center gap-3">
              <span className="text-[12.5px] text-shed-text">{text}</span>
              <span className="flex-1" />
              <span data-delivery-button={`toggle:${key}`}>
                <Switch on={draft.bools[key] === true} set={(v) => set((d) => edit.bool(d, key, v))} />
              </span>
            </div>
          );
        }
        if (kind === "text") {
          return (
            <div key={key}>
              <span className={label}>{text}</span>
              <Entry value={draft.text[key] ?? ""} onChange={(v) => set((d) => edit.text(d, key, v))}
                placeholder={placeholder} field={key} />
            </div>
          );
        }
        if (kind === "list") {
          return (
            <div key={key}>
              <span className={label}>{text}</span>
              <ListEditor name={key} items={draft.lists[key] ?? []} set={set} placeholder={placeholder} />
            </div>
          );
        }
        // The passphrase command: a stored one is never shown back.
        const cmd = draft.command;
        return (
          <div key={key}>
            <span className={label}>{text}</span>
            {cmd?.keep ? (
              <div className="flex items-center gap-2">
                <span className="flex-1 text-[12.5px] text-shed-text" data-delivery-command-set>
                  A command is set. Its arguments are not shown.
                </span>
                <button type="button" className={prefsButton} onClick={() => set(edit.replaceCommand)}
                  data-delivery-button={`replace:${key}`}>
                  Replace
                </button>
                <button type="button" className={prefsButton} onClick={() => set(edit.clearCommand)}
                  data-delivery-button={`clear:${key}`}>
                  Clear
                </button>
              </div>
            ) : (
              <>
                <ListEditor name={key} items={cmd?.args ?? []} set={set} placeholder={placeholder} sensitive />
                {draft.commandStored && (
                  <button type="button" onClick={() => set(edit.keepCommand)}
                    className="hlink mt-1 rounded-md px-1 py-0.5 text-[12px] font-medium text-shed-text-secondary"
                    data-delivery-button={`keep:${key}`}>
                    Keep the command that is set
                  </button>
                )}
              </>
            )}
          </div>
        );
      })}
      {name === "files" && (
        <div>
          <span className={label}>Files</span>
          <div className="flex flex-col gap-1.5">
            {draft.files.map((f, i) => (
              <div key={i} className="flex items-start gap-1.5" data-delivery-file={i}>
                <Entry value={f.from} onChange={(v) => set((d) => edit.file(d, i, "from", v))} placeholder="~/from"
                  field="files.from" index={i} />
                <Entry value={f.to} onChange={(v) => set((d) => edit.file(d, i, "to", v))} placeholder="to"
                  field="files.to" index={i} />
                <span className="w-[96px] flex-none" data-delivery-input="files.base" data-index={i}>
                  <Select
                    value={f.base}
                    onChange={(v) => set((d) => edit.file(d, i, "base", v))}
                    options={[{ value: "home", label: "home" }, { value: "project", label: "project" }]}
                  />
                </span>
                <button
                  type="button"
                  title="Remove this file"
                  onClick={() => set((d) => edit.remove(d, "files", i))}
                  className="hlink mt-1 flex h-6 w-6 flex-none items-center justify-center rounded-md text-shed-text-muted"
                  data-delivery-button={`remove:files:${i}`}
                >
                  <X size={14} />
                </button>
              </div>
            ))}
            <button
              type="button"
              onClick={() => set((d) => edit.add(d, "files"))}
              className="hlink inline-flex w-fit items-center gap-1 rounded-md px-1 py-0.5 text-[12px] font-medium text-shed-text-secondary"
              data-delivery-button="add:files"
            >
              <Plus size={13} /> Add a file
            </button>
          </div>
        </div>
      )}
      <div>
        <span className={label}>Only for (every list given must match)</span>
        <div className="grid grid-cols-3 gap-1.5">
          {ONLY_KEYS.map((k) => (
            <div key={k}>
              <span className="mb-0.5 block text-[11px] text-shed-text-muted">{k}</span>
              <ListEditor name={`only.${k}`} items={draft.only[k]} set={set}
                placeholder={k === "repos" ? "owner/*" : k === "backends" ? "vz" : "server"} />
            </div>
          ))}
        </div>
      </div>
      <div className="flex items-center justify-end gap-2">
        <button type="button" onClick={onCancel} className="hlink rounded-[8px] px-2.5 py-1 text-[12px] font-medium text-shed-text-secondary"
          data-delivery-button="cancel">
          Cancel
        </button>
        <button
          type="button"
          onClick={onSave}
          disabled={busy}
          className="hbtn rounded-[8px] px-3 py-1 text-[12px] font-semibold"
          style={{ background: "var(--shed-accent)", color: "var(--shed-accent-fg)", opacity: busy ? 0.6 : 1 }}
          data-delivery-button="save"
        >
          {busy ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}
