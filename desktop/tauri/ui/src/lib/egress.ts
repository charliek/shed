/* Egress as the app shows it (plan 039 PR B): a shed's profiles and recent blocks on
 * its detail, and the servers' profiles on the System pane.
 *
 * Pure and dependency-free so node's own test runner pins it (`test/egress.test.mjs`).
 * Every string here came from a server (and, through the host a shed dialled, from
 * inside the shed); the Rust side has cleaned it, and it is rendered as text only. */

/** A shed's assigned profiles off its document's `egress_profiles`: a list of names,
 *  `[]` when none is assigned, or null when the document could not be read. */
export function readProfiles(raw: unknown): string[] {
  if (!Array.isArray(raw)) return [];
  return raw.filter((p): p is string => typeof p === "string" && p.length > 0);
}

/** The line a shed's detail shows for its profiles. */
export function profilesText(profiles: string[] | null): string {
  if (profiles === null) return "Egress profile: unknown (the shed could not be read)";
  if (profiles.length === 0) return "Egress profile: none (egress is not restricted)";
  return `Egress profile: ${profiles.join(", ")}`;
}

/** Where a blocked connection went: `host:port`, or the resolved address when the
 *  decision names no host. */
export function blockTarget(e: { host: string; port: number; resolved_ip: string }): string {
  const where = e.host || e.resolved_ip || "?";
  return e.port ? `${where}:${e.port}` : where;
}

/** A profile's one-line summary (the System pane's rows). */
export function profileSummary(p: { mode?: string | null; allow?: string[] | null; deny?: string[] | null; rule?: string | null }): string {
  const parts: string[] = [];
  if (p.mode) parts.push(`mode=${p.mode}`);
  if (p.allow?.length) parts.push(`allow=${p.allow.length}`);
  if (p.deny?.length) parts.push(`deny=${p.deny.length}`);
  if (p.rule) parts.push("rule");
  return parts.length ? parts.join(" ") : "(empty)";
}
