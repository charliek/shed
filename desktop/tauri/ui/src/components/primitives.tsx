/* shed desktop — shared Plex primitives, translated from the design mockup
   (design/plex/ui.jsx) into typed React. lucide-react glyphs where one matches;
   sizes/strokes per the mockup. Consolidated here so the panes + the dialogs share
   one vocabulary (replaces the former in-App ad-hoc Dot/Tag/ImageChip/PageHead/
   HeadAction/IconBtn/EmptyCard/card). */
import type { LucideIcon } from "lucide-react";
import { Layers } from "lucide-react";
import { cn } from "@/lib/utils";

/** The shared card surface (border + surface bg + Plex shadow/radius). */
export const cardCls = "rounded-shed border border-shed-border bg-shed-surface shadow-shed";

/** The four status tones shared across status chips + action-button tints. */
export type Tone = "ok" | "attention" | "muted" | "danger";

/** tone → [bg token, fg token] for the chip/tint surfaces. */
const TONE: Record<Tone, [string, string]> = {
  ok: ["var(--shed-ok-bg)", "var(--shed-ok-fg)"],
  attention: ["var(--shed-warn-bg)", "var(--shed-warn-fg)"],
  muted: ["var(--shed-idle-bg)", "var(--shed-idle-fg)"],
  danger: ["var(--shed-err-bg)", "var(--shed-err-fg)"],
};
/** tone → status glyph (●◐○▲) — the mockup's STATUS vocab, keyed by tone. */
const TONE_GLYPH: Record<Tone, string> = { ok: "●", attention: "◐", muted: "○", danger: "▲" };

export function Dot({ className, style, pulse }: { className?: string; style?: React.CSSProperties; pulse?: boolean }) {
  return (
    <span
      style={{ ...(pulse ? { animation: "shed-pulse 2s infinite" } : null), ...style }}
      className={cn("inline-block h-[9px] w-[9px] flex-none rounded-full", className)}
    />
  );
}

/** A mono status chip: tone-tinted pill with the tone's glyph + a label. */
export function StatusChip({ tone, label }: { tone: Tone; label: string }) {
  const [bg, fg] = TONE[tone];
  return (
    <span
      className="inline-flex flex-none items-center gap-1.5 rounded-lg px-2.5 py-1.5 font-mono text-[12px] font-semibold leading-none"
      style={{ background: bg, color: fg }}
    >
      <span className="text-[9px] leading-none">{TONE_GLYPH[tone]}</span>
      {label}
    </span>
  );
}

/** A runtime tag (vz / firecracker), mono pill in the runtime's brand tint. */
export function Tag({ kind }: { kind: string }) {
  const vz = kind === "vz";
  return (
    <span
      className="flex-none rounded-md px-2 py-[3px] font-mono text-[10px] font-bold leading-none"
      style={{
        background: vz ? "var(--shed-tag-vz-bg)" : "var(--shed-tag-fc-bg)",
        color: vz ? "var(--shed-tag-vz-text)" : "var(--shed-tag-fc-text)",
        letterSpacing: ".03em",
      }}
    >
      {kind}
    </span>
  );
}

/** A neutral mono image/version chip with a layers glyph. */
export function ImageChip({ children }: { children: React.ReactNode }) {
  return (
    <span className="inline-flex flex-none items-center gap-1.5 whitespace-nowrap rounded-[7px] bg-shed-inset px-2 py-1 font-mono text-[12px] font-medium leading-none text-shed-text-secondary">
      <Layers size={12} className="text-shed-text-muted" />
      {children}
    </span>
  );
}

/** agent kind → left-border accent color (claude=orange, codex=green, cursor=violet,
    opencode=blue, everything else muted). */
const AGENT_COLOR: Record<string, string> = {
  "claude-rc": "var(--shed-accent)",
  "claude-broker": "var(--shed-accent)",
  "claude-code": "var(--shed-accent)",
  codex: "#10A37F",
  "codex-rc": "#10A37F",
  cursor: "#6E56CF",
  opencode: "#3B82F6",
  shell: "var(--shed-text-muted)",
};
export function agentColor(kind: string): string {
  return AGENT_COLOR[kind] ?? "var(--shed-text-muted)";
}

/** The agent-kind badge — the raw kind in a bordered pill with a kind-colored left rail. */
export function KindBadge({ kind }: { kind: string }) {
  return (
    <span
      className="flex-none rounded-[5px] border border-shed-border font-mono text-[12px] font-medium leading-none text-shed-text"
      style={{ borderLeft: `3px solid ${agentColor(kind)}`, padding: "4px 8px 4px 7px" }}
    >
      {kind}
    </span>
  );
}

export type ActTone = "neutral" | "accent" | "ok" | "attention" | "danger";
/** tone → [bg, fg, border] for the 36px square action button. */
const ACT_TINT: Record<ActTone, [string, string, string]> = {
  neutral: ["var(--shed-surface)", "var(--shed-text-secondary)", "var(--shed-border)"],
  accent: ["var(--shed-accent-subtle)", "var(--shed-accent)", "transparent"],
  ok: ["var(--shed-ok-bg)", "var(--shed-ok-fg)", "transparent"],
  attention: ["var(--shed-warn-bg)", "var(--shed-warn-fg)", "transparent"],
  danger: ["var(--shed-err-bg)", "var(--shed-err-fg)", "transparent"],
};

/** A 36px square action button, tinted by intent (the shed-card / session-card control). */
export function ActBtn({
  icon: Icon,
  tone = "neutral",
  onClick,
  title,
  spin,
  disabled,
}: {
  icon: LucideIcon;
  tone?: ActTone;
  onClick?: () => void;
  title?: string;
  spin?: boolean;
  disabled?: boolean;
}) {
  const [bg, fg, border] = ACT_TINT[tone];
  return (
    <button
      onClick={onClick}
      title={title}
      disabled={disabled}
      className="hbtn inline-flex h-9 w-9 flex-none items-center justify-center rounded-[9px]"
      style={{ border: `1px solid ${border}`, background: bg, color: fg, opacity: disabled ? 0.5 : 1, cursor: disabled ? "default" : "pointer" }}
    >
      <Icon size={16} className={spin ? "animate-spin" : undefined} />
    </button>
  );
}

/** The big page heading: title + optional accessory (inline), sub, and right slot. */
export function PageHead({
  title,
  accessory,
  sub,
  right,
}: {
  title: string;
  accessory?: React.ReactNode;
  sub?: string;
  right?: React.ReactNode;
}) {
  return (
    <div className={sub ? "mb-[18px]" : "mb-[22px]"}>
      <div className="flex min-h-[34px] items-center gap-4">
        <div className="flex min-w-0 flex-1 items-center gap-3.5">
          <h1 className="text-[30px] font-bold leading-[1.05] text-shed-text" style={{ letterSpacing: "-.02em" }}>{title}</h1>
          {accessory}
        </div>
        {right}
      </div>
      {sub && <p className="mt-2 max-w-[620px] text-[14px] leading-snug text-shed-text-muted">{sub}</p>}
    </div>
  );
}

/** A text+icon header action (the "New shed" / "Refresh" links). */
export function HeadAction({
  icon: Icon,
  label,
  color = "var(--shed-text-secondary)",
  onClick,
  spin,
}: {
  icon: LucideIcon;
  label: string;
  color?: string;
  onClick?: () => void;
  spin?: boolean;
}) {
  return (
    <button
      onClick={onClick}
      className="hlink inline-flex items-center gap-[7px] rounded-lg px-[11px] py-[7px] text-[14px] font-semibold"
      style={{ color, background: "transparent", border: "none" }}
    >
      <Icon size={18} className={spin ? "animate-spin" : undefined} style={{ color }} />
      {label}
    </button>
  );
}

/** The empty-state card (accent-tinted glyph tile + title + body). */
export function Empty({ icon: Icon, title, body }: { icon: LucideIcon; title: string; body: string }) {
  return (
    <div className={cn(cardCls, "flex flex-col items-center gap-3 px-6 py-[52px] text-center")}>
      <div
        className="flex h-[50px] w-[50px] items-center justify-center rounded-[13px] border"
        style={{ background: "var(--shed-accent-subtle)", borderColor: "var(--shed-accent-border)" }}
      >
        <Icon size={25} style={{ color: "var(--shed-accent)" }} />
      </div>
      <div className="text-[17px] font-semibold text-shed-text">{title}</div>
      <div className="max-w-[340px] text-[14px] leading-relaxed text-shed-text-muted">{body}</div>
    </div>
  );
}
