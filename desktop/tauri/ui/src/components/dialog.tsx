/* shed desktop — the modal dialog kit, translated from the design mockup
   (design/plex/dialogs.jsx) into typed React: Scrim, DialogShell, Field, Select,
   Stepper, Switch, Segmented. Shared by the New-shed / New-session / Preferences
   dialogs so they read as one system. */
import { useEffect } from "react";
import type { LucideIcon } from "lucide-react";
import { ChevronsUpDown, X } from "lucide-react";
import { cn } from "@/lib/utils";

/** The shared dialog text-input class (Plex inset + accent focus ring). */
export const dialogInput =
  "w-full rounded-[9px] border border-shed-border bg-shed-inset px-3 py-2 text-[14px] text-shed-text outline-none focus:border-shed-accent";

/** The dialog footer's secondary (Cancel) button class. */
export const dialogBtnSecondary =
  "hlink rounded-[9px] border border-shed-border px-[18px] py-2.5 text-[14px] font-semibold text-shed-text-secondary";

/** Close the dialog on Escape while `enabled` (window keydown, scoped to mount). */
export function useEscClose(onClose: () => void, enabled = true) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && enabled) onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose, enabled]);
}

/** The backdrop scrim. `mark` stamps a `data-<mark>` attr the harness can look for
    (e.g. data-create / data-launch). Backdrop mousedown closes. */
export function Scrim({ onClose, mark, children }: { onClose: () => void; mark?: string; children: React.ReactNode }) {
  const markAttr = mark ? { [`data-${mark}`]: "" } : {};
  return (
    <div
      onMouseDown={onClose}
      {...markAttr}
      className="fixed inset-0 z-50 flex items-center justify-center p-6"
      style={{
        background: "color-mix(in oklch, var(--shed-bg) 20%, rgba(0,0,0,.5))",
        backdropFilter: "blur(3px)",
        WebkitBackdropFilter: "blur(3px)",
        animation: "scrim-in .14s ease",
      }}
    >
      {children}
    </div>
  );
}

/** The dialog card: accent-tiled header (icon + title + sub + close), a scrolling
    body, and an optional footer row. `onClose` fires from the header X. */
export function DialogShell({
  icon: Icon,
  title,
  sub,
  onClose,
  width = 480,
  footer,
  children,
}: {
  icon: LucideIcon;
  title: string;
  sub?: string;
  onClose: () => void;
  width?: number;
  footer?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div
      onMouseDown={(e) => e.stopPropagation()}
      className="flex flex-col overflow-hidden"
      style={{
        width,
        maxWidth: "100%",
        maxHeight: "calc(100% - 36px)",
        background: "var(--shed-surface)",
        border: "1px solid var(--shed-border)",
        borderRadius: 16,
        boxShadow: "var(--shed-shadow-sel), 0 24px 60px rgba(0,0,0,.28)",
        animation: "dialog-in .18s cubic-bezier(.2,.8,.2,1)",
      }}
    >
      <div className="flex items-start gap-3 border-b border-shed-border px-6 pb-[18px] pt-4">
        <div
          className="flex h-[38px] w-[38px] flex-none items-center justify-center rounded-[10px] border"
          style={{ background: "var(--shed-accent-subtle)", borderColor: "var(--shed-accent-border)" }}
        >
          <Icon size={20} style={{ color: "var(--shed-accent)" }} />
        </div>
        <div className="min-w-0 flex-1">
          <div className="text-[18px] font-bold leading-tight text-shed-text">{title}</div>
          {sub && <div className="mt-1 text-[13px] leading-snug text-shed-text-muted">{sub}</div>}
        </div>
        <button
          onClick={onClose}
          title="Close"
          className="hlink flex h-[30px] w-[30px] flex-none items-center justify-center rounded-lg text-shed-text-muted"
        >
          <X size={16} />
        </button>
      </div>
      <div className="flex flex-col gap-3.5 overflow-y-auto px-6 py-[18px]">{children}</div>
      {footer && <div className="flex items-center justify-between border-t border-shed-border px-6 py-3.5">{footer}</div>}
    </div>
  );
}

/** A labelled field row (label + optional hint/help around the control). Pass
 *  `htmlFor` (matching an `id` on the wrapped control) to associate the label with
 *  its control — the control renders as a sibling, so without it the `<label>` isn't
 *  tied to any input. Omit it for non-control children (e.g. a Segmented button
 *  group, where a `for` target would be wrong): they keep the visual label only. */
export function Field({
  label,
  hint,
  help,
  htmlFor,
  children,
}: {
  label: string;
  hint?: string;
  help?: string;
  htmlFor?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex flex-col gap-[7px]">
      <label htmlFor={htmlFor} className="text-[13px] font-medium text-shed-text-secondary">
        {label}
        {hint && <span className="ml-[7px] font-normal text-shed-text-muted">{hint}</span>}
      </label>
      {children}
      {help && <div className="text-[12px] leading-snug text-shed-text-muted">{help}</div>}
    </div>
  );
}

/** A styled native select with a caret glyph. `id` is forwarded to the underlying
    `<select>` so a `Field htmlFor` can associate its label. */
export function Select({
  value,
  onChange,
  options,
  id,
}: {
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
  id?: string;
}) {
  return (
    <div className="relative">
      <select
        id={id}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="w-full appearance-none rounded-[9px] border border-shed-border bg-shed-inset px-3 py-2 pr-9 text-[14px] text-shed-text outline-none focus:border-shed-accent"
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      <ChevronsUpDown size={13} className="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-shed-text-muted" />
    </div>
  );
}

/** A −/value/+ integer stepper. */
export function Stepper({ value, set, min = 1, max = 16 }: { value: number; set: (v: number) => void; min?: number; max?: number }) {
  const btn = "flex h-[38px] w-[34px] items-center justify-center border-y border-shed-border bg-shed-surface text-[17px] font-semibold text-shed-text-secondary";
  return (
    <div className="flex items-center">
      <button onClick={() => set(Math.max(min, value - 1))} className={cn("hbtn rounded-l-lg border-l", btn)}>
        −
      </button>
      <div className="flex h-[38px] min-w-[50px] items-center justify-center border-y border-shed-border bg-shed-bg font-mono text-[14px] font-semibold text-shed-text">{value}</div>
      <button onClick={() => set(Math.min(max, value + 1))} className={cn("hbtn rounded-r-lg border-r", btn)}>
        +
      </button>
    </div>
  );
}

/** A pill toggle switch. */
export function Switch({ on, set, disabled }: { on: boolean; set: (v: boolean) => void; disabled?: boolean }) {
  return (
    <button
      onClick={() => set(!on)}
      role="switch"
      aria-checked={on}
      disabled={disabled}
      className="hbtn flex h-[27px] w-[46px] flex-none rounded-full p-[3px]"
      style={{ background: on ? "var(--shed-accent)" : "var(--shed-border-strong)", transition: "background .15s", opacity: disabled ? 0.5 : 1 }}
    >
      <span
        className="h-[21px] w-[21px] rounded-full bg-white"
        style={{ transform: on ? "translateX(19px)" : "translateX(0)", transition: "transform .18s", boxShadow: "0 1px 2px rgba(0,0,0,.35)" }}
      />
    </button>
  );
}

/** A segmented control. Each option is `[value, label, activeColor?]`. */
export function Segmented({ options, value, set }: { options: [string, string, string?][]; value: string; set: (v: string) => void }) {
  return (
    <div className="flex gap-[5px] rounded-[9px] border border-shed-border bg-shed-bg p-1">
      {options.map(([v, l, c]) => {
        const on = value === v;
        return (
          <button
            key={v}
            onClick={() => set(v)}
            className={cn(on ? "hbtn" : "hlink", "flex-1 rounded-md px-1.5 py-2 text-[13px] font-semibold")}
            style={{
              background: on ? "var(--shed-surface)" : "transparent",
              color: on ? c ?? "var(--shed-text)" : "var(--shed-text-secondary)",
              boxShadow: on ? "var(--shed-shadow)" : "none",
            }}
          >
            {l}
          </button>
        );
      })}
    </div>
  );
}
