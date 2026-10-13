//! **The `/` menu** — which of a session's slash commands a client offers, how
//! what is typed matches them, and what sending a `/` draft does (plan 029
//! §3.4; P5, P6, P9, P16, P23, P28).
//!
//! A session's [`LaneSlashCatalog`] is RAW: every row the agent described, a
//! `local` help row and a row needing a capability this session lacks
//! included. This module is the one place those rows become a menu, and the
//! one place a typed `/x` becomes an action — pure, no I/O, ungated (like
//! [`crate::lane_view`]: shed-mobile links this crate with default features).
//! The desktop reaches it through Tauri commands and the phone through FRB
//! `#[frb(sync)]` functions, and each only RENDERS and handles keys: the rule
//! for which row shows, which name a typed token means, what Enter does and
//! when a command is refused is written once, here, rather than once in
//! TypeScript and once in Dart (P5 — the dispatch guard is exactly where two
//! ports drift).
//!
//! # The five calls
//!
//! * [`resolve`]`(catalog, caps)` → [`SlashMenu`]: the rows shown, each with
//!   its resolved [`SlashAction`], and the hidden BUILTIN names a typed command
//!   is refused for. [`crate::lane_view::LaneView`] calls it, caches the result
//!   and hands it out as [`crate::lane_view::LaneViewSnapshot::slash_menu`];
//!   a client never calls it itself.
//! * [`filter`]`(menu, draft, caret, working)` → [`SlashFilter`]: whether the
//!   menu panel is up for this draft and caret, and its rows in order, each
//!   marked `dimmed` while a turn runs.
//! * [`dispatch`]`(menu, draft, working)` → [`SlashDispatch`]: what sending
//!   the draft does — the command grammar (P6) and its guard (P5). Send and
//!   Enter-with-the-menu-closed both go through it.
//! * [`accept`]`(menu, draft, row, key, working)` → [`SlashAccept`]: what Enter,
//!   Tab or a click on a menu row does (P9 and §3.5's select rule).
//! * [`is_working`]`(activity)`: the one "a turn is running" rule both
//!   clients dim and refuse by (P23).
//!
//! # Which rows show ([`resolve`])
//!
//! A row shows only when this client knows every value on it, its `requires`
//! capability is true, and its action validates:
//!
//! | catalog row (`run`) | action | needs |
//! |---|---|---|
//! | kind `Prompt` | [`SlashAction::Prompt`] | — |
//! | kind `Method`, [`LaneSlashMethod::SetTitle`], `param` `title` | [`SlashAction::SetTitle`] | `caps.set_title` |
//! | kind `Method`, [`LaneSlashMethod::Set`] with a `mode` | [`SlashAction::SetMode`] (`LaneSettingChange::Mode`) | — (P7: not `caps.settings`) |
//! | kind `Method`, [`LaneSlashMethod::Stop`] | [`SlashAction::Stop`] | `caps.stop`, with or without `requires` |
//! | kind `Picker`, [`LaneSlashPicker::Model`] | [`SlashAction::ModelPicker`] | `caps.settings` |
//! | kind `Aside`, `Local`, `Unavailable`, anything else | hidden | — |
//!
//! `requires: stop` needs `caps.stop`; `requires: aside` hides the row in phase
//! 1 whatever the session serves (X1 — there is no aside verb yet), as does any
//! `requires` this build cannot name. An unknown (`Other`) `source`,
//! `requires`, `run.kind`, `run.arg`, `run.method` or `run.picker` hides the
//! row, and only that row — craze's rule ("a client hides a row with a value it
//! does not know"). So does a `name` or `qualified` that is not one token, which
//! an accept could not write back into a draft.
//!
//! **A hidden row claims no name** (craze A1): per name, folded with Rust's
//! Unicode [`str::to_lowercase`], the first row SHOWN wins and a later shown
//! row of the same name is dropped. That is the cursor `/btw` case: the builtin
//! `btw` requires `aside`, is hidden, and the agent's own `btw` after it shows.
//!
//! [`SlashMenu::hidden`] lists the hidden BUILTIN rows a typed command is
//! refused for (P5: a builtin the menu hides is never sent to the agent as
//! text), each with a reason — the row's own `run.reason` for an `unavailable`
//! one, else "not available here yet" (deferred or unknown), "this session
//! cannot be stopped" or "not advertised" (a capability the session does not
//! advertise). A name a shown row answers to (by either spelling) is not
//! listed: typed, it runs that row. A hidden non-builtin is not listed either:
//! typed, it is sent as text, as any `/x` the menu does not know.
//!
//! No cap (P16): craze's 512-row bound is its snapshot's, a delta's catalog is
//! uncapped, and so is this — the panel scrolls.
//!
//! # The grammar ([`dispatch`])
//!
//! The grammar reads the draft AS IT WOULD BE SENT — trimmed at both ends, as
//! both clients (and craze's TUI) trim before sending — so a stray leading
//! space or trailing newline can never carry a hidden builtin out as text
//! (P5). Only a SINGLE-LINE draft whose FIRST character is then `/` can be a
//! command; a draft with a newline (or a carriage return) inside it is prompt
//! text (P6). The token runs to the first whitespace
//! character; the argument is the rest, trimmed, LITERAL — no quote handling,
//! inner whitespace kept (`/rename   x  y ` → `x  y`). The token matches a
//! shown row case-insensitively by `name`, then by `qualified`; failing that, a
//! hidden builtin's name; failing that the draft is not a command.
//!
//! | draft | answer |
//! |---|---|
//! | no leading `/`, multi-line, a bare `/`, or a `/x` no shown row and no hidden builtin answers to | [`SlashDispatch::NotCommand`] — the caller sends the draft as it does today |
//! | a shown `prompt` row | [`SlashDispatch::Prompt`] — send its `text` (it queues during a turn, so it is never refused for one) |
//! | a hidden builtin | `Refuse` — `/<name>: <its hidden reason>` |
//! | an argument to `SetMode` or `Stop` | `Refuse` — `/<name>: takes no argument` |
//! | an argument to `/model` (X2: no inline model) | `Refuse` — `/<name>: takes no argument — pick a model from the list` |
//! | `SetTitle` with no argument | `Refuse` — `type a title after /<name>` |
//! | a non-`while_working` row while [`is_working`] | `Refuse` — `/<name>: runs after this turn` |
//! | otherwise | [`SlashDispatch::Run`] — the action, and for `SetTitle` its title |
//!
//! The argument checks come before the turn check: a draft that will never
//! run should not wait for the turn to say so.
//!
//! # The menu ([`filter`]) and the keys ([`accept`])
//!
//! The panel is up (P28) when the draft is one line, starts with `/`, the
//! CARET sits inside that first token — after the `/` and at most at the
//! token's end — and at least one row matches. Matching is craze's TUI's:
//! case-insensitive, in four buckets, each in menu order — `name` starts with
//! what was typed, then `qualified` starts with it, then `name` contains it,
//! then `qualified` contains it. A bare `/` lists every row. While a turn runs
//! every row stays listed, and the ones that would be refused for it come back
//! `dimmed` (X7/P29 — dimmed, not hidden: the gate is temporal, not a
//! capability).
//!
//! **Carets are UTF-16 code units**, in and out — what a browser's
//! `selectionStart` and Flutter's `TextSelection` both report — so neither
//! client converts. A caret past the draft's end is read as its end.
//!
//! [`accept`] is P9 plus §3.5's select rule, keyed by how the row was chosen
//! ([`SlashAcceptKey`]):
//!
//! * **Enter** on the highlighted row: when the token already spells the row
//!   (either spelling, case-insensitively — the TUI's `EqualFold`), Enter
//!   SENDS: the answer is [`dispatch`] of the draft as it stands. Otherwise
//!   Enter SELECTS the row, as a click does.
//! * **Pick** (a click or a tap): SELECT. An action that takes no argument
//!   here ([`SlashAction::takes_argument`] — `SetMode`, `Stop`,
//!   `ModelPicker`) runs at once: the answer is [`dispatch`] of the completed
//!   draft, so an argument suffix still gets its refusal. A `prompt` or
//!   `SetTitle` row completes instead.
//! * **Tab**: complete, never run (the TUI's accept).
//!
//! Completing replaces only the token with `/<spelling> ` — `qualified` when
//! the typed token holds a `:` and the row has one, else `name` — taking in
//! one space that already followed it and keeping any argument after it, and
//! answers the caret just after the inserted space.
//!
//! # JSON
//!
//! Every type here is `Serialize` + `Deserialize`, snake_case, enums tagged on
//! `kind` like the lane contract's, `None` fields omitted. A [`SlashMenuRow`]:
//! `{"name":"rename","description":"…","source":"builtin","arg":"required",
//! "arg_hint":"<title>","while_working":true,"action":{"kind":"set_title"}}`; a
//! mode row's action `{"kind":"set_mode","change":{"kind":"mode","id":"plan"}}`.
//! [`SlashDispatch`]: `{"kind":"not_command"}`, `{"kind":"prompt","text":…}`,
//! `{"kind":"run","action":{…},"arg":…}`, `{"kind":"refuse","reason":…}`.
//! [`SlashAccept`]: `{"kind":"complete","draft":…,"caret":…}` or
//! `{"kind":"dispatch","dispatch":{…}}`. Every field is an owned
//! `String`/`Option`/`Vec`/scalar or a plain or fielded enum (the FRB-mirror
//! rule, `shed_core::lane`'s module doc): the phone mirrors all of them.

use std::collections::HashSet;

use serde::{Deserialize, Serialize};
use shed_core::lane::{
    LaneCapabilities, LaneSettingChange, LaneSlashArg, LaneSlashCatalog, LaneSlashKind,
    LaneSlashMethod, LaneSlashPicker, LaneSlashRequires, LaneSlashRow, LaneSlashSource,
};
use shed_core::rc::RcActivity;

/// A hidden row's reason when this client cannot run it yet: phase 1's
/// deferred kinds (`aside`, `local`, X1) and any value it does not know.
const NOT_YET: &str = "not available here yet";
/// A hidden `stop` row's reason: the session's capabilities say no `stop`.
const CANNOT_STOP: &str = "this session cannot be stopped";
/// A hidden row's reason when the session does not advertise what it needs
/// (`set_title`, `settings`) — and an `unavailable` row's, when it gave none.
const NOT_ADVERTISED: &str = "not advertised";

/// What running a shown row does, resolved against the session's
/// capabilities ([`resolve`]'s table).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum SlashAction {
    /// Send `/<spelling>` and the argument as a prompt
    /// ([`SlashDispatch::Prompt`]); it queues during a turn.
    Prompt,
    /// Rename the session, the argument as the title
    /// (`AgentLane::set_title`).
    SetTitle,
    /// Move the session to a mode: `AgentLane::set(change)`. `change` is
    /// always `LaneSettingChange::Mode` — the one place a client builds a
    /// setting change from the inbound catalog (P14).
    SetMode { change: LaneSettingChange },
    /// End the session (`AgentLane::stop`, behind the client's confirm).
    Stop,
    /// Open the client's model picker (its settings sheet).
    ModelPicker,
}

impl SlashAction {
    /// Whether this client takes an argument for the action: a prompt's text
    /// and a title. `SetMode`, `Stop` and `ModelPicker` take none here (X2:
    /// `/model`'s inline `<model> [effort]` is deferred), which is what makes
    /// selecting their row run it at once ([`accept`]).
    pub fn takes_argument(&self) -> bool {
        match self {
            SlashAction::Prompt | SlashAction::SetTitle => true,
            SlashAction::SetMode { .. } | SlashAction::Stop | SlashAction::ModelPicker => false,
        }
    }
}

/// One shown row: what a menu draws, and what selecting it does.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SlashMenuRow {
    /// The token without its `/` — what the row is matched, completed and run
    /// by, and the name a client passes to [`accept`].
    pub name: String,
    /// A plugin row's second spelling (`plugin:name`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub qualified: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    pub source: LaneSlashSource,
    /// The argument the catalog says the row takes, for display. What this
    /// client accepts is the action's ([`SlashAction::takes_argument`]).
    pub arg: LaneSlashArg,
    /// How to show the argument (`<title>`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub arg_hint: Option<String>,
    /// The catalog's `whileWorking`, verbatim. A client dims and refuses by
    /// [`SlashMenuRow::waits_for_the_turn`] (or [`SlashFilterRow::dimmed`]),
    /// not by this: a prompt row leaves it `false` and still never waits.
    pub while_working: bool,
    pub action: SlashAction,
}

impl SlashMenuRow {
    /// Whether the row cannot run while a turn is running — it is dimmed in
    /// the menu and refused with "runs after this turn" until the turn ends.
    /// A prompt row never waits: it queues as any prompt does (craze's
    /// "Which rows a client shows").
    pub fn waits_for_the_turn(&self) -> bool {
        !self.while_working && self.action != SlashAction::Prompt
    }
}

/// A hidden builtin's name and why it is hidden — what typing it is refused
/// with.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SlashHidden {
    pub name: String,
    pub reason: String,
}

/// A session's resolved menu ([`resolve`]): the rows a client shows, in
/// catalog order, and the hidden builtin names a typed command is refused for.
///
/// `rows` may be empty — a catalog of nothing a client can run — and the menu
/// is still the session's: a client offers no menu affordance then, but still
/// dispatches through it, so a typed hidden builtin is refused.
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct SlashMenu {
    #[serde(default)]
    pub rows: Vec<SlashMenuRow>,
    #[serde(default)]
    pub hidden: Vec<SlashHidden>,
}

/// One row of an open menu.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SlashFilterRow {
    pub row: SlashMenuRow,
    /// A turn is running and this row would be refused for it: drawn dimmed,
    /// labelled "after this turn", and selecting it answers the refusal (X7).
    pub dimmed: bool,
}

/// What the menu panel shows for a draft and caret ([`filter`]).
#[derive(Debug, Clone, PartialEq, Eq, Default, Serialize, Deserialize)]
pub struct SlashFilter {
    /// The panel is up: the draft is a command draft, the caret is in its
    /// token, and at least one row matches. Exactly `!rows.is_empty()` — so
    /// "zero matches" is a closed menu, and Enter sends the draft (P9).
    pub open: bool,
    /// The draft's first token as typed, `/` included — `""` when the draft
    /// is not a command draft. What a client keys an Esc dismissal by: the
    /// menu stays dismissed until this changes (P9).
    pub token: String,
    /// The matching rows, in bucket order; empty when closed.
    #[serde(default)]
    pub rows: Vec<SlashFilterRow>,
}

/// What sending a draft does ([`dispatch`]).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum SlashDispatch {
    /// Not a command: the caller sends the draft as text, exactly as it would
    /// without a menu.
    NotCommand,
    /// A shown prompt row: send `text` as the prompt — `/<spelling>`, then the
    /// argument after one space when there is one. The spelling is the row's
    /// own (`qualified` when that is what was typed), not the typed case, so
    /// the agent gets the name it advertised.
    Prompt { text: String },
    /// Run `action`. `arg` is the title for [`SlashAction::SetTitle`] — never
    /// empty — and `None` for every other action.
    Run {
        action: SlashAction,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        arg: Option<String>,
    },
    /// Refused here, nothing sent: show `reason` beside the composer and keep
    /// the draft.
    Refuse { reason: String },
}

/// How a menu row was chosen ([`accept`]).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SlashAcceptKey {
    /// Enter on the highlighted row.
    Enter,
    /// Tab on the highlighted row.
    Tab,
    /// A click or a tap on a row.
    Pick,
}

/// What choosing a menu row does ([`accept`]).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum SlashAccept {
    /// Replace the draft with `draft` and put the caret at `caret` (UTF-16
    /// code units).
    Complete { draft: String, caret: u32 },
    /// Act on `dispatch` exactly as on a Send of the draft.
    Dispatch { dispatch: SlashDispatch },
}

/// Whether a turn is running, for the while-working rule (P23): activity
/// `Working` (shed-craze's fold maps a foreign turn to it) or `NeedsApproval`
/// — conservative, since an approval card can stand over a running turn.
pub fn is_working(activity: RcActivity) -> bool {
    matches!(activity, RcActivity::Working | RcActivity::NeedsApproval)
}

/// The session's menu: the rows shown and the hidden builtin names (the
/// module doc's table and claiming rule).
///
/// Does not read `caps.slash`: a catalog is only ever held where the
/// capabilities say `slash`, and the view gates the whole menu on it.
pub fn resolve(catalog: &LaneSlashCatalog, caps: &LaneCapabilities) -> SlashMenu {
    let mut rows: Vec<SlashMenuRow> = Vec::new();
    let mut hidden: Vec<SlashHidden> = Vec::new();
    let mut claimed: HashSet<String> = HashSet::new();
    for row in &catalog.rows {
        // A name no draft's token could spell is no row at all: not shown,
        // claiming nothing, and no refusal (an empty one would turn a bare `/`
        // into one).
        if !is_token(&row.name) {
            continue;
        }
        match judge(row, caps) {
            // The first shown row of a name keeps it; a hidden one never
            // claimed it (A1).
            Ok(action) => {
                if claimed.insert(fold(&row.name)) {
                    rows.push(SlashMenuRow {
                        name: row.name.clone(),
                        qualified: row.qualified.clone(),
                        description: row.description.clone(),
                        source: row.source.clone(),
                        arg: row.run.arg.clone(),
                        arg_hint: row.run.arg_hint.clone(),
                        while_working: row.while_working,
                        action,
                    });
                }
            }
            Err(reason) if row.source == LaneSlashSource::Builtin => {
                hidden.push(SlashHidden {
                    name: row.name.clone(),
                    reason: reason.to_string(),
                });
            }
            Err(_) => {}
        }
    }
    // A hidden name that a shown row answers to (by either spelling) is not a
    // refusal — typed, it runs that row — and two hidden rows of one name
    // refuse with the first's reason. Spellings join only now: during the
    // loop a `qualified` claims no name.
    let mut taken = claimed;
    taken.extend(rows.iter().filter_map(|r| r.qualified.as_deref()).map(fold));
    hidden.retain(|h| taken.insert(fold(&h.name)));
    SlashMenu { rows, hidden }
}

/// One catalog row (its name a token) against the session's capabilities:
/// its action when it shows, else why not.
fn judge<'a>(row: &'a LaneSlashRow, caps: &LaneCapabilities) -> Result<SlashAction, &'a str> {
    let run = &row.run;
    let unknown = !row.source.is_known()
        || row.requires.as_ref().is_some_and(|r| !r.is_known())
        || !run.kind.is_known()
        || !run.arg.is_known()
        || run.method.as_ref().is_some_and(|m| !m.is_known())
        || run.picker.as_ref().is_some_and(|p| !p.is_known());
    if unknown || row.qualified.as_deref().is_some_and(|q| !is_token(q)) {
        return Err(NOT_YET);
    }
    match &row.requires {
        Some(LaneSlashRequires::Stop) if !caps.stop => return Err(CANNOT_STOP),
        // Phase 1 has no aside verb (X1): hidden whatever the session serves.
        Some(LaneSlashRequires::Aside) => return Err(NOT_YET),
        _ => {}
    }
    match run.kind {
        LaneSlashKind::Prompt => Ok(SlashAction::Prompt),
        LaneSlashKind::Method => match (&run.method, run.param.as_deref(), &run.mode) {
            (Some(LaneSlashMethod::SetTitle), Some("title"), _) => caps
                .set_title
                .then_some(SlashAction::SetTitle)
                .ok_or(NOT_ADVERTISED),
            (Some(LaneSlashMethod::Set), _, Some(id)) => Ok(SlashAction::SetMode {
                change: LaneSettingChange::Mode { id: id.clone() },
            }),
            (Some(LaneSlashMethod::Stop), _, _) => {
                caps.stop.then_some(SlashAction::Stop).ok_or(CANNOT_STOP)
            }
            _ => Err(NOT_YET),
        },
        LaneSlashKind::Picker => match run.picker {
            Some(LaneSlashPicker::Model) => caps
                .settings
                .then_some(SlashAction::ModelPicker)
                .ok_or(NOT_ADVERTISED),
            _ => Err(NOT_YET),
        },
        LaneSlashKind::Unavailable => Err(run.reason.as_deref().unwrap_or(NOT_ADVERTISED)),
        _ => Err(NOT_YET),
    }
}

/// What the menu panel shows for `draft` with the caret at `caret` (UTF-16
/// code units), `working` from [`is_working`] (the module doc).
pub fn filter(menu: &SlashMenu, draft: &str, caret: u32, working: bool) -> SlashFilter {
    let Some(command) = Command::parse(draft) else {
        return SlashFilter::default();
    };
    let token = format!("/{}", command.typed);
    let caret = (caret as usize).min(utf16_len(draft));
    if caret == 0 || caret > utf16_len(&token) {
        return SlashFilter {
            token,
            ..SlashFilter::default()
        };
    }
    let query = fold(command.typed);
    // craze's four buckets, each in menu order, each row in its first only.
    let mut buckets: [Vec<&SlashMenuRow>; 4] = Default::default();
    for row in &menu.rows {
        if let Some(bucket) = match_bucket(row, &query) {
            buckets[bucket].push(row);
        }
    }
    let rows: Vec<SlashFilterRow> = buckets
        .into_iter()
        .flatten()
        .map(|row| SlashFilterRow {
            dimmed: working && row.waits_for_the_turn(),
            row: row.clone(),
        })
        .collect();
    SlashFilter {
        open: !rows.is_empty(),
        token,
        rows,
    }
}

/// Which of craze's four buckets `row` falls in for the folded `query`, if
/// any: `name` prefix, `qualified` prefix, `name` substring, `qualified`
/// substring.
fn match_bucket(row: &SlashMenuRow, query: &str) -> Option<usize> {
    let name = fold(&row.name);
    if name.starts_with(query) {
        return Some(0);
    }
    // Past a name-prefix miss the query is not empty, so a row with no
    // second spelling ("") matches neither of its buckets.
    let qualified = row.qualified.as_deref().map(fold).unwrap_or_default();
    if qualified.starts_with(query) {
        Some(1)
    } else if name.contains(query) {
        Some(2)
    } else if qualified.contains(query) {
        Some(3)
    } else {
        None
    }
}

/// What sending `draft` does (the module doc's grammar and guard).
pub fn dispatch(menu: &SlashMenu, draft: &str, working: bool) -> SlashDispatch {
    let Some(command) = Command::parse(draft.trim()) else {
        return SlashDispatch::NotCommand;
    };
    let arg = command.rest.trim();
    let wanted = fold(command.typed);
    let Some((row, spelling)) = menu.answering(&wanted) else {
        return match menu.hidden.iter().find(|h| fold(&h.name) == wanted) {
            Some(h) => refuse(&h.name, &h.reason),
            None => SlashDispatch::NotCommand,
        };
    };
    // The argument checks come before the turn check.
    match &row.action {
        SlashAction::Prompt if arg.is_empty() => SlashDispatch::Prompt {
            text: format!("/{spelling}"),
        },
        SlashAction::Prompt => SlashDispatch::Prompt {
            text: format!("/{spelling} {arg}"),
        },
        SlashAction::SetTitle if arg.is_empty() => SlashDispatch::Refuse {
            reason: format!("type a title after /{}", row.name),
        },
        SlashAction::ModelPicker if !arg.is_empty() => {
            refuse(&row.name, "takes no argument — pick a model from the list")
        }
        action if !action.takes_argument() && !arg.is_empty() => {
            refuse(&row.name, "takes no argument")
        }
        _ if working && row.waits_for_the_turn() => refuse(&row.name, AFTER_THIS_TURN),
        action => SlashDispatch::Run {
            action: action.clone(),
            arg: (*action == SlashAction::SetTitle).then(|| arg.to_string()),
        },
    }
}

/// What choosing the menu row named `row_name` with `key` does (the module
/// doc's key rules). `None` when there is nothing to accept: `draft` is not a
/// command draft, or no shown row has that name (the menu changed under the
/// highlight — re-filter).
pub fn accept(
    menu: &SlashMenu,
    draft: &str,
    row_name: &str,
    key: SlashAcceptKey,
    working: bool,
) -> Option<SlashAccept> {
    let command = Command::parse(draft)?;
    let row = menu.row_named(&fold(row_name))?;

    let typed = fold(command.typed);
    let spelled = !typed.is_empty()
        && (typed == fold(&row.name) || row.qualified.as_deref().is_some_and(|q| typed == fold(q)));
    if key == SlashAcceptKey::Enter && spelled {
        return Some(SlashAccept::Dispatch {
            dispatch: dispatch(menu, draft, working),
        });
    }
    // A dimmed row (it waits for the turn) SELECTS to its refusal, the draft
    // kept (§3.5, X7) — whether or not it takes an argument. Tab only
    // completes, so a command can still be readied for after the turn.
    if key != SlashAcceptKey::Tab && working && row.waits_for_the_turn() {
        return Some(SlashAccept::Dispatch {
            dispatch: refuse(&row.name, AFTER_THIS_TURN),
        });
    }

    // Complete: the token becomes `/<spelling> `, one space that already
    // followed it is taken in, and whatever came after stays.
    let spelling = match &row.qualified {
        Some(qualified) if command.typed.contains(':') => qualified,
        _ => &row.name,
    };
    let head = format!("/{spelling} ");
    let tail = command.rest.strip_prefix(' ').unwrap_or(command.rest);
    let completed = format!("{head}{tail}");
    if key != SlashAcceptKey::Tab && !row.action.takes_argument() {
        return Some(SlashAccept::Dispatch {
            dispatch: dispatch(menu, &completed, working),
        });
    }
    Some(SlashAccept::Complete {
        caret: u32::try_from(utf16_len(&head)).unwrap_or(u32::MAX),
        draft: completed,
    })
}

impl SlashMenu {
    /// The shown row named `wanted` (already [`fold`]ed).
    fn row_named(&self, wanted: &str) -> Option<&SlashMenuRow> {
        self.rows.iter().find(|r| fold(&r.name) == wanted)
    }

    /// The shown row a typed name (without its `/`, already [`fold`]ed)
    /// answers to, and the spelling it answered by: a `name` first, then a
    /// `qualified`.
    fn answering(&self, wanted: &str) -> Option<(&SlashMenuRow, &str)> {
        if let Some(row) = self.row_named(wanted) {
            return Some((row, &row.name));
        }
        self.rows.iter().find_map(|r| {
            let qualified = r.qualified.as_deref()?;
            (fold(qualified) == wanted).then_some((r, qualified))
        })
    }
}

/// A command draft, split by the grammar: one line, first character `/`.
struct Command<'a> {
    /// The token without its `/`, up to the first whitespace character; empty
    /// for a bare `/`.
    typed: &'a str,
    /// Everything after the token, its leading whitespace included.
    rest: &'a str,
}

impl<'a> Command<'a> {
    fn parse(draft: &'a str) -> Option<Command<'a>> {
        if draft.contains(['\n', '\r']) {
            return None;
        }
        let body = draft.strip_prefix('/')?;
        let end = body.find(char::is_whitespace).unwrap_or(body.len());
        let (typed, rest) = body.split_at(end);
        Some(Command { typed, rest })
    }
}

/// Why a row that waits for the turn is refused while one runs.
const AFTER_THIS_TURN: &str = "runs after this turn";

/// `/<name>: <why>`, refused.
fn refuse(name: &str, why: &str) -> SlashDispatch {
    SlashDispatch::Refuse {
        reason: format!("/{name}: {why}"),
    }
}

/// The case fold every name comparison goes through: Rust's full Unicode
/// lowercase, so `/Ünïcode` finds `ünïcode`.
fn fold(s: &str) -> String {
    s.to_lowercase()
}

/// A name a draft's token could hold: not empty, no whitespace, no control
/// character.
fn is_token(s: &str) -> bool {
    !s.is_empty() && !s.chars().any(|c| c.is_whitespace() || c.is_control())
}

/// `s`'s length in UTF-16 code units — the caret's unit.
fn utf16_len(s: &str) -> usize {
    s.chars().map(char::len_utf16).sum()
}

#[cfg(test)]
pub(crate) mod tests;
