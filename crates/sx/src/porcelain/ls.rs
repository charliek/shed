//! `sx ls [--on <target>]` — one table of every RC session this operator can
//! reach: local tmux, every configured `machines:` entry, and every running shed
//! on every configured server.
//!
//! Two properties drive the shape:
//!
//! **Per-target failure is a ROW, never the end of the listing.** A machine that
//! is asleep, a server that is down, a shed whose image predates the RC helper —
//! each becomes one annotated line under the table while every other target still
//! renders. A fan-out that dies on its first unreachable host is useless on a
//! real fleet.
//!
//! **Rendering is capability-aware** (`docs/extensions/rc-helper.md`
//! § kind_features / § client fallbacks). The `list` envelope carries the
//! capability block, so the affordance column costs no extra round-trip:
//!
//! | what came back | `WATCH` column |
//! |---|---|
//! | a `kind_features` row with a message feed | `feed` |
//! | a `kind_features` row without one | `activity` |
//! | NO row for that kind (`shell`, `claude-broker`) | `-` — no feed/steer affordances |
//! | no `capabilities` block at all (an old binary) | `?` + a note naming the target |

use shed_core::rc::{RcCapabilities, RcSessionDto, RcSessionListDto};

use crate::args::Parsed;
use crate::cli::Deps;
use crate::porcelain::{
    load_config, remote_exec, remote_prefix, resolve_target, VerbError, VerbResult,
};
use crate::target::Resolved;

/// One rendered session.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Row {
    pub name: String,
    pub slug: String,
    pub target: String,
    pub kind: String,
    /// `ready`, or `ready (working)` when the hub reported an activity.
    pub state: String,
    pub watch: String,
    pub created_by: String,
}

/// The whole listing: rows plus the two kinds of annotation that must never be
/// silently dropped.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Listing {
    pub rows: Vec<Row>,
    pub notes: Vec<String>,
    pub errors: Vec<String>,
}

impl Listing {
    /// Fold one target's `list` envelope in.
    pub fn add(&mut self, target: &str, envelope: &RcSessionListDto) {
        match &envelope.capabilities {
            None => self.notes.push(format!(
                "{target}: no capability block (an rc binary that predates capability \
                 discovery) — watch/steer affordances unknown"
            )),
            Some(caps) if !caps.has_feature("contract-v2") => self.notes.push(format!(
                "{target}: pre-contract-v2 rc — no lane or feed hints; \
                 `sx watch` will fall back to probe polling"
            )),
            Some(_) => {}
        }
        for session in &envelope.rc_sessions {
            self.rows
                .push(row_for(target, session, envelope.capabilities.as_ref()));
        }
    }

    pub fn add_error(&mut self, target: &str, message: impl std::fmt::Display) {
        self.errors.push(format!("{target}: {message}"));
    }
}

/// The capability-aware affordance cell for one session (the table above).
pub fn watch_cell(kind: &str, caps: Option<&RcCapabilities>) -> &'static str {
    let Some(caps) = caps else { return "?" };
    match caps.kind_features.get(kind) {
        None => "-",
        Some(features) if features.feed_messages() => "feed",
        Some(_) => "activity",
    }
}

fn row_for(target: &str, s: &RcSessionDto, caps: Option<&RcCapabilities>) -> Row {
    let kind = s.kind.as_str().to_string();
    let state = match s.activity {
        // Lifecycle trumps activity: the producer already suppresses the
        // dimension for a blocking state, so an activity present here is real.
        Some(activity) => format!("{} ({})", s.state.as_str(), activity.as_str()),
        None => s.state.as_str().to_string(),
    };
    Row {
        name: s
            .display_name
            .clone()
            .filter(|n| !n.is_empty())
            .unwrap_or_else(|| s.slug.clone()),
        slug: s.slug.clone(),
        target: target.to_string(),
        watch: watch_cell(&kind, caps).to_string(),
        kind,
        state,
        created_by: s.created_by.clone().unwrap_or_default(),
    }
}

/// Render the listing as a padded table plus its annotations. Pure — the render
/// rules are unit-tested without any transport.
pub fn render(listing: &Listing) -> String {
    let mut out = String::new();
    if listing.rows.is_empty() {
        out.push_str("no rc sessions\n");
    } else {
        // The header is just the first row, so it is padded by the same rule.
        let headers = ["NAME", "TARGET", "KIND", "STATE", "WATCH", "CREATED_BY"];
        let cells: Vec<[&str; 6]> = std::iter::once(headers)
            .chain(listing.rows.iter().map(|r| {
                [
                    r.name.as_str(),
                    r.target.as_str(),
                    r.kind.as_str(),
                    r.state.as_str(),
                    r.watch.as_str(),
                    r.created_by.as_str(),
                ]
            }))
            .collect();
        let widths: Vec<usize> = (0..headers.len())
            .map(|i| {
                cells
                    .iter()
                    .map(|row| row[i].chars().count())
                    .max()
                    .unwrap_or(0)
            })
            .collect();
        for row in &cells {
            let mut line = String::new();
            for (i, cell) in row.iter().enumerate() {
                if i + 1 == row.len() {
                    line.push_str(cell);
                } else {
                    line.push_str(&format!("{cell:<width$}  ", width = widths[i]));
                }
            }
            // A trailing empty last column must not leave trailing blanks.
            out.push_str(line.trim_end());
            out.push('\n');
        }
    }
    for note in &listing.notes {
        out.push_str(&format!("note: {note}\n"));
    }
    for error in &listing.errors {
        out.push_str(&format!("error: {error}\n"));
    }
    out
}

/// `sx ls [--on <target>]`.
pub fn run(deps: &Deps, p: &Parsed) -> VerbResult {
    let mut listing = Listing::default();
    if p.value("on").is_empty() {
        collect_everything(deps, &mut listing);
    } else {
        let resolved = resolve_target(deps, p)?;
        collect_one(deps, &resolved, &mut listing);
    }
    listing
        .rows
        .sort_by(|a, b| (&a.target, &a.name, &a.slug).cmp(&(&b.target, &b.name, &b.slug)));
    deps.write_out(&render(&listing));
    // A per-target failure is reported, not fatal: exit 0 as long as the listing
    // itself was produced. (A caller that wants strictness names one target.)
    Ok(0)
}

/// Local + every configured machine + every running shed on every server.
///
/// Sequential on purpose for v1: the fan-out is bounded by a personal fleet, and
/// a concurrent version has to interleave ITS errors into the same annotated
/// listing. `sx ls --fast` / concurrency is recorded as follow-up in plan 009 §9.
fn collect_everything(deps: &Deps, listing: &mut Listing) {
    collect_one(deps, &Resolved::Local, listing);
    let config = load_config(deps);
    for machine in &config.machines {
        collect_one(deps, &Resolved::Machine(machine.clone()), listing);
    }
    if config.servers.is_empty() {
        return;
    }
    // WITH the host-agent minter when one is running (`crate::backend`): an
    // mTLS-enrolled server holds no static token, and without a mintable
    // credential its sheds are silently absent from this listing.
    let sheds = crate::backend::with_backend(deps, async |b| b.rc_targets(None, None).await);
    for (shed, rc_target) in sheds {
        let resolved = Resolved::Shed {
            name: shed.name.clone(),
            server: Some(rc_target.server_name.clone()),
        };
        collect_one(deps, &resolved, listing);
    }
}

fn collect_one(deps: &Deps, resolved: &Resolved, listing: &mut Listing) {
    let label = resolved.display();
    match envelope_for(deps, resolved) {
        Ok(envelope) => listing.add(&label, &envelope),
        Err(err) => listing.add_error(&label, err.message),
    }
}

/// One target's `list` envelope — the single call that carries BOTH the sessions
/// and the capability block.
pub fn envelope_for(deps: &Deps, resolved: &Resolved) -> Result<RcSessionListDto, VerbError> {
    match resolved {
        Resolved::Local => {
            let mut envelope = deps.engine(false).list(None);
            // The local engine's `list` has no capability block of its own (the
            // Go one-shot assembles it in the CLI layer too, `clirc.go:356-364`).
            envelope.capabilities = Some(deps.capabilities());
            Ok(envelope)
        }
        remote => {
            let prefix = remote_prefix(deps, remote)?;
            let argv = prefix.splice(shed_core::rc::list_argv(prefix.bin()));
            let stdout = remote_exec(deps, remote, &argv, None)?;
            shed_core::rc::decode_list_response(&stdout)
                .map_err(|e| VerbError::failed(e.to_string()))
        }
    }
}
