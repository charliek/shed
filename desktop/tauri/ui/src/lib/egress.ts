/* Egress as the app shows it (plan 039 PR B): a shed's profiles and recent blocks on
 * its detail, and the servers' profiles on the System pane.
 *
 * Pure and dependency-free so node's own test runner pins it (`test/egress.test.mjs`).
 * Every string here came from a server (and, through the host a shed dialled, from
 * inside the shed); the Rust side has cleaned it, and it is rendered as text only. */

/** A shed's effective egress, as `shed.delivery`'s `egress` carries it (the Rust side
 *  reads it off `GET /api/egress/{shed}`, which resolves the server's default profiles
 *  for a shed created without `--egress`): whether egress control is on, whether the
 *  shed runs behind it, the profiles in force, whether those are the server's default,
 *  and the composed mode (null: unknown). */
export type EffectiveEgress = {
  enabled: boolean;
  restricted: boolean;
  profiles: string[];
  inherited: boolean;
  mode: "audit" | "enforce" | null;
};

/** `shed.delivery`'s `egress` as the detail takes it, or null when it is not one (the
 *  server's egress status could not be read). */
export function readEgress(raw: unknown): EffectiveEgress | null {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const r = raw as Record<string, unknown>;
  const profiles = Array.isArray(r.profiles)
    ? r.profiles.filter((p): p is string => typeof p === "string" && p.length > 0)
    : [];
  const restricted = r.restricted === true;
  return {
    enabled: r.enabled === true,
    restricted,
    profiles: restricted ? profiles : [],
    inherited: restricted && profiles.length > 0 && r.inherited === true,
    mode: r.mode === "audit" || r.mode === "enforce" ? r.mode : null,
  };
}

/** The line a shed's detail shows for its egress: the profiles in force (the server's
 *  default marked as such), and "not restricted" only when none applies. `undefined`:
 *  still reading; null: it could not be read. */
export function egressText(e: EffectiveEgress | null | undefined): string {
  if (e === undefined) return "Egress profile: reading…";
  if (e === null) return "Egress profile: unknown (the shed's egress could not be read)";
  if (!e.enabled) return "Egress profile: none (egress control is off on this server)";
  if (!e.restricted) return "Egress profile: none (egress is not restricted)";
  if (e.profiles.length === 0) return "Egress profile: restricted (the server names no profile)";
  const mode = e.mode ? ` (${e.mode})` : "";
  const from = e.inherited ? ", the server's default" : "";
  return `Egress profile: ${e.profiles.join(", ")}${mode}${from}`;
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
