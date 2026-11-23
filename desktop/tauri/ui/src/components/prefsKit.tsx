/* The Preferences window's grouped-form pieces: a titled card and its footnote.
   Shared by `PreferencesWindow` and its sections in their own files. */

/** A titled grouped card: the mac per-section mono label + the rounded container
    the section's rows live in. The optional Footnote is a separate sibling. */
export function Section({ title, children, id }: { title: string; children: React.ReactNode; id?: string }) {
  return (
    <>
      <div className="mb-1.5 font-mono text-[11px] font-semibold uppercase tracking-wider text-shed-text-muted" data-prefs-section={id}>
        {title}
      </div>
      <div className="mb-5 overflow-hidden rounded-[10px] border border-shed-border bg-shed-surface">{children}</div>
    </>
  );
}

/** The muted footnote below a grouped card (mac's per-section footer text). */
export function Footnote({ children }: { children: React.ReactNode }) {
  return <p className="-mt-3.5 mb-5 px-1 text-[12px] leading-relaxed text-shed-text-muted">{children}</p>;
}

/** The Preferences text-input class (the Terminal template's). */
export const prefsInput =
  "w-full rounded-[8px] border border-shed-border bg-shed-inset px-2.5 py-1.5 font-mono text-[13px] text-shed-text outline-none focus:border-shed-accent";

/** The small bordered row button ("Use this machine", "Edit", "Preview"). */
export const prefsButton =
  "hbtn flex-none rounded-[8px] border border-shed-border bg-shed-inset px-2.5 py-1 text-[12px] font-medium text-shed-text";
