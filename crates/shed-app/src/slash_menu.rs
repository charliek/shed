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
//! * [`resolve`]`(catalog, caps, settings)` → [`SlashMenu`]: the rows shown,
//!   each with its resolved [`SlashAction`], and the hidden BUILTIN names a
//!   typed command is refused for. [`crate::lane_view::LaneView`] calls it —
//!   again whenever the catalog, the capabilities or the settings change (the
//!   settings decide `/effort`, plan 032 PC7) — caches the result and hands it
//!   out as [`crate::lane_view::LaneViewSnapshot::slash_menu`]; a client never
//!   calls it itself.
//! * [`filter`]`(menu, draft, caret, working)` → [`SlashFilter`]: whether the
//!   menu panel is up for this draft and caret, its rows in order, each
//!   marked `dimmed` while a turn runs, and the row a highlight starts on.
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
//! | kind `Picker`, [`LaneSlashPicker::Effort`] | [`SlashAction::EffortPicker`] (the effort option, the current model, its levels, its default level) | `caps.settings` and the session's effort (below) |
//! | kind `Aside`, `Local`, `Unavailable`, anything else | hidden | — |
//!
//! `requires: stop` needs `caps.stop`; `requires: aside` hides the row in phase
//! 1 whatever the session serves (X1 — there is no aside verb yet), as does any
//! `requires` this build cannot name. `requires: effort` (craze 0.2.0's
//! `/effort`, plan 032 §3.2) is no capability but the session's EFFORT: a
//! current model ([`LaneSettings::model`]) and an effort option
//! ([`LaneSettings::effort_option`], craze's `EffortOption`) — without both
//! the row is hidden, "the current model has no effort setting", and claims no
//! name, so an agent's own `/effort` after it shows instead (P:2123). The
//! effort picker reads the same two, plus the current model's default level:
//! its effort facts' `default_effort`, only where it is one of the option's
//! values. An unknown (`Other`) `source`,
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
//! | an `EffortPicker` row's argument naming no level | `Refuse` — craze's own text, `no effort <typed>: one of <levels>` (below) |
//! | a non-`while_working` row while [`is_working`] | `Refuse` — `/<name>: runs after this turn` |
//! | an `EffortPicker` row's argument naming a level | [`SlashDispatch::Run`] — [`SlashAction::SetEffort`] |
//! | otherwise | [`SlashDispatch::Run`] — the action, and for `SetTitle` its title |
//!
//! The argument checks come before the turn check: a draft that will never
//! run should not wait for the turn to say so.
//!
//! **`/effort [<level>]`** (plan 032 §3.2, PC8; craze's `runEffort`): bare, it
//! runs the `EffortPicker` (the client opens its settings sheet). With an
//! argument it is checked in its OWN arm, before the no-argument guard — the
//! picker still [`takes no argument`](SlashAction::takes_argument), so a pick
//! and Enter run it and Tab completes it like any picker row. `default`, in
//! any case, is reserved: the picker's `default_level`, and refused when there
//! is none — even where a level is spelled `default`. Any other argument is
//! craze's `MatchEffortValue`: trimmed, then the level whose id is it exactly,
//! else in any case, else the level whose name (trimmed) is it in any case —
//! the first rule that matches anything decides and must match exactly one.
//! "In any case" is craze's: Go's `EqualFold`, Unicode SIMPLE case folding,
//! so `Σ` is both `σ` and `ς` — ambiguous where a session offers both.
//! A match is `SetEffort { change: Config { id: <option>, value: <its id>,
//! for_model: Some(<the model>) } }`, sent even when it is the current level;
//! anything else is refused `no effort <typed>: one of default, <level>, …`
//! (`default, ` only where there is a default level; a level spelled `default`
//! left out when `default` was what failed), the draft kept. A valid level
//! mid-turn is refused "runs after this turn": the builtin row has no
//! `whileWorking`. shed holds no "a model switch is pending" state, so craze's
//! TUI refusal for one is the session's own `stale_model` answer to the
//! `for_model`-bound change (plan 032 §4.6).
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
//! capability). [`SlashFilter::start`] names the first row that is not dimmed
//! (0 when every row is, or none matches): where a client's highlight starts
//! whenever it resets, so Enter on a menu opened mid-turn runs or completes a
//! row instead of being refused (plan 030 §3.4). The arrows still visit
//! dimmed rows — Tab readies one for after the turn.
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
//!   `ModelPicker`, `EffortPicker`) runs at once: the answer is [`dispatch`]
//!   of the completed draft, so an argument suffix still gets its refusal —
//!   or, for `/effort`, its level. A `prompt` or `SetTitle` row completes
//!   instead.
//! * **Tab**: complete, never run (the TUI's accept).
//!
//! While a turn runs, selecting a dimmed row (a pick, or Enter on a token
//! that does not spell it) answers its refusal, `/<name>: runs after this
//! turn`, the draft kept — but for `/effort` with an argument, whose level is
//! judged before the turn there as in [`dispatch`] (plan 032 §3.2): the
//! completed draft is dispatched, so a level that names nothing is refused
//! for that, and a valid one for the turn.
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
//! mode row's action `{"kind":"set_mode","change":{"kind":"mode","id":"plan"}}`;
//! the effort row's `{"kind":"effort_picker","option":"effort","for_model":
//! "grok-4.6","levels":[{"id":"low","name":"Low"},…],"default_level":"high"}`
//! and a level's `{"kind":"set_effort","change":{"kind":"config","id":"effort",
//! "value":"low","for_model":"grok-4.6"}}`.
//! [`SlashDispatch`]: `{"kind":"not_command"}`, `{"kind":"prompt","text":…}`,
//! `{"kind":"run","action":{…},"arg":…}`, `{"kind":"refuse","reason":…}`.
//! [`SlashAccept`]: `{"kind":"complete","draft":…,"caret":…}` or
//! `{"kind":"dispatch","dispatch":{…}}`. [`SlashFilter`]:
//! `{"open":true,"token":"/re","rows":[{"row":{…},"dimmed":false}],"start":0}`
//! (`rows` and `start` default when absent). Every field is an owned
//! `String`/`Option`/`Vec`/scalar or a plain or fielded enum (the FRB-mirror
//! rule, `shed_core::lane`'s module doc): the phone mirrors all of them.

use std::collections::HashSet;

use serde::{Deserialize, Serialize};
use shed_core::lane::{
    LaneCapabilities, LaneChoice, LaneSettingChange, LaneSettings, LaneSlashArg, LaneSlashCatalog,
    LaneSlashKind, LaneSlashMethod, LaneSlashPicker, LaneSlashRequires, LaneSlashRow,
    LaneSlashSource,
};
use shed_core::rc::RcActivity;

mod display_line;
use display_line::display_line;

/// A hidden row's reason when this client cannot run it yet: phase 1's
/// deferred kinds (`aside`, `local`, X1) and any value it does not know.
const NOT_YET: &str = "not available here yet";
/// A hidden `stop` row's reason: the session's capabilities say no `stop`.
const CANNOT_STOP: &str = "this session cannot be stopped";
/// A hidden row's reason when the session does not advertise what it needs
/// (`set_title`, `settings`) — and an `unavailable` row's, when it gave none.
const NOT_ADVERTISED: &str = "not advertised";
/// A hidden `requires: effort` (or effort picker) row's reason: no current
/// model, or no effort option for it (plan 032 §3.2).
const NO_EFFORT: &str = "the current model has no effort setting";
/// The reserved `/effort` argument: the current model's default level
/// (craze's `effortDefaultTag`).
const DEFAULT_LEVEL: &str = "default";

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
    /// Open the client's effort picker — its settings sheet, whose effort row
    /// is this option (plan 032 §3.2). Resolved from the session's settings
    /// when the menu was: what a `/effort <level>` is matched against, and
    /// bound to.
    EffortPicker {
        /// The effort option's id ([`LaneSettings::effort_option`]) — what a
        /// [`LaneSettingChange::Config`] names.
        option: String,
        /// The model the option belongs to: the session's current model.
        for_model: String,
        /// The option's values, in the agent's order: the levels.
        levels: Vec<LaneChoice>,
        /// That model's default level (its effort facts' `default_effort`),
        /// only where it is one of `levels`' ids — what `/effort default`
        /// sets. `None` without effort facts.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        default_level: Option<String>,
    },
    /// Set the effort: `AgentLane::set(change)`, `change` always a
    /// `LaneSettingChange::Config` of the effort option bound to its model
    /// (`for_model`). Only [`dispatch`] builds one, from an `EffortPicker`
    /// row and a level — no menu row carries it — and a client sends it even
    /// when it is the current level (craze's rule: a dropped no-op could lose
    /// a concurrent change).
    SetEffort { change: LaneSettingChange },
}

impl SlashAction {
    /// Whether this client takes an argument for the action: a prompt's text
    /// and a title. `SetMode`, `Stop` and `ModelPicker` take none here (X2:
    /// `/model`'s inline `<model> [effort]` is deferred), which is what makes
    /// selecting their row run it at once ([`accept`]). Nor does
    /// `EffortPicker` (PC8): its optional `<level>` is read by [`dispatch`]'s
    /// own arm, so a pick and Enter still run the completed draft and Tab
    /// completes it. `SetEffort` is never a row's.
    pub fn takes_argument(&self) -> bool {
        match self {
            SlashAction::Prompt | SlashAction::SetTitle => true,
            SlashAction::SetMode { .. }
            | SlashAction::Stop
            | SlashAction::ModelPicker
            | SlashAction::EffortPicker { .. }
            | SlashAction::SetEffort { .. } => false,
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
    /// labelled "after this turn", and selecting it answers the refusal (X7)
    /// — `/effort` with a level after that level is judged ([`accept`]).
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
    /// The row the highlight starts on: the first row that is not dimmed,
    /// else 0 (0 when closed). A client puts the highlight here wherever it
    /// resets it — a new token, new rows, a new catalog (P9) — so a menu
    /// opened mid-turn starts on a row Enter acts on rather than refuses; a
    /// working flip alone moves no highlight (plan 030 §3.4).
    #[serde(default)]
    pub start: u32,
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
/// `settings` are the session's as a reader is shown them — `None` when it
/// has none — and decide `/effort` alone (`requires: effort` and the effort
/// picker, plan 032 §3.2).
///
/// Does not read `caps.slash`: a catalog is only ever held where the
/// capabilities say `slash`, and the view gates the whole menu on it.
pub fn resolve(
    catalog: &LaneSlashCatalog,
    caps: &LaneCapabilities,
    settings: Option<&LaneSettings>,
) -> SlashMenu {
    let effort = settings.and_then(effort_picker);
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
        match judge(row, caps, effort.as_ref()) {
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

/// What `/effort` acts on (plan 032 §3.2), as the [`SlashAction::EffortPicker`]
/// a row runs: the session's current model, its effort option, and that
/// model's default level. `None` — the row hidden, [`NO_EFFORT`] — without a
/// current model (PC9) or without an effort option.
fn effort_picker(settings: &LaneSettings) -> Option<SlashAction> {
    let model = settings.model.as_deref().filter(|m| !m.is_empty())?;
    let option = settings.effort_option()?;
    // craze's `effortDefault` and `offers` together: the current model's
    // `defaultEffort` as its own catalog row states it (effort facts alone),
    // only where the effort option offers that level.
    let default_level = settings
        .models
        .iter()
        .find(|m| m.id == model)
        .and_then(|m| m.effort.as_ref()?.default_effort.clone())
        .filter(|d| offers(&option.values, d));
    Some(SlashAction::EffortPicker {
        option: option.id.clone(),
        for_model: model.to_string(),
        levels: option.values.clone(),
        default_level,
    })
}

/// One catalog row (its name a token) against the session's capabilities and
/// its effort picker ([`effort_picker`]): its action when it shows, else why
/// not.
fn judge<'a>(
    row: &'a LaneSlashRow,
    caps: &LaneCapabilities,
    effort: Option<&SlashAction>,
) -> Result<SlashAction, &'a str> {
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
        // The session's state, not a capability: shown only while the
        // current model has an effort option (P:2052, 2115-2128).
        Some(LaneSlashRequires::Effort) if effort.is_none() => return Err(NO_EFFORT),
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
            Some(LaneSlashPicker::Effort) if !caps.settings => Err(NOT_ADVERTISED),
            Some(LaneSlashPicker::Effort) => effort.cloned().ok_or(NO_EFFORT),
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
    let start = rows.iter().position(|r| !r.dimmed).unwrap_or(0);
    SlashFilter {
        open: !rows.is_empty(),
        token,
        start: u32::try_from(start).unwrap_or(0),
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
        // `/effort <level>` (PC8): its own arm, ahead of the no-argument
        // guard, and still ahead of the turn check — a level that names
        // nothing is refused for that, a valid one mid-turn for the turn.
        SlashAction::EffortPicker {
            option,
            for_model,
            levels,
            default_level,
        } if !arg.is_empty() => {
            match set_effort(option, for_model, levels, default_level.as_deref(), arg) {
                Err(reason) => SlashDispatch::Refuse { reason },
                Ok(_) if working && row.waits_for_the_turn() => refuse(&row.name, AFTER_THIS_TURN),
                Ok(action) => SlashDispatch::Run { action, arg: None },
            }
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
    // `/effort` with a level is the exception (plan 032 §3.2): its argument
    // is judged before the turn, by pick and Enter as by dispatch, so it
    // falls through to the completed draft's dispatch — a level that names
    // nothing is refused for that, a valid one still for the turn.
    let level_first =
        matches!(row.action, SlashAction::EffortPicker { .. }) && !command.rest.trim().is_empty();
    if key != SlashAcceptKey::Tab && working && row.waits_for_the_turn() && !level_first {
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

/// `/effort <level>` — craze's `runEffort` with an argument (its
/// `internal/tui/effort_picker.go`; the module doc's grammar): `typed` (the
/// argument, trimmed) as the [`SlashAction::SetEffort`] it names, else the
/// refusal. `default`, in any case, is reserved and decided first: the
/// default level where there is one the option offers, refused anywhere else
/// — even where a level is spelled `default`, which the sheet still reaches.
fn set_effort(
    option: &str,
    for_model: &str,
    levels: &[LaneChoice],
    default_level: Option<&str>,
    typed: &str,
) -> Result<SlashAction, String> {
    // Checked again here: the picker may have crossed a client bridge as JSON.
    let default_level = default_level.filter(|d| offers(levels, d));
    let reserved = eq_fold(typed, DEFAULT_LEVEL);
    let level = if reserved {
        default_level.map(str::to_string)
    } else {
        match_effort_value(levels, typed)
    };
    let Some(value) = level else {
        return Err(effort_refusal(levels, default_level, typed, reserved));
    };
    Ok(SlashAction::SetEffort {
        change: LaneSettingChange::Config {
            id: option.to_string(),
            value,
            for_model: Some(for_model.to_string()),
        },
    })
}

/// craze's `agent.MatchEffortValue` (`effortValue`, `internal/agent/
/// startsettings.go`): `typed` trimmed, then the level whose id is it
/// exactly, else the one whose id is it in any case, else the one whose name,
/// trimmed, is it in any case — the first rule that matches anything decides,
/// and it must match exactly one level (an ambiguous rule never falls through
/// to a looser one). Answered in the agent's spelling: the level's id. "In
/// any case" is craze's `strings.EqualFold` ([`eq_fold`]), not this module's
/// [`fold`]: with levels `σ` and `ς`, `Σ` is both, so it is refused as
/// ambiguous rather than taken as the `σ` lowercasing makes it.
fn match_effort_value(levels: &[LaneChoice], typed: &str) -> Option<String> {
    let want = typed.trim();
    if want.is_empty() {
        return None;
    }
    let id_exactly = |l: &LaneChoice| l.id == want;
    let id_in_any_case = |l: &LaneChoice| eq_fold(&l.id, want);
    let name_in_any_case = |l: &LaneChoice| eq_fold(l.name.trim(), want);
    let rules: [&dyn Fn(&LaneChoice) -> bool; 3] =
        [&id_exactly, &id_in_any_case, &name_in_any_case];
    for rule in rules {
        let mut found = levels.iter().filter(|l| rule(l));
        match (found.next(), found.next()) {
            (None, _) => continue,
            (Some(one), None) => return Some(one.id.clone()),
            (Some(_), Some(_)) => return None,
        }
    }
    None
}

/// craze's `effortRefusal`, its exact text: what was typed and what would do
/// — `default` first where the model has a default level the option offers,
/// then the levels as the agent spells them, less one spelled `default` when
/// the reserved word was what failed (typed, it is the reserved word again).
fn effort_refusal(
    levels: &[LaneChoice],
    default_level: Option<&str>,
    typed: &str,
    reserved: bool,
) -> String {
    let mut one: Vec<String> = Vec::new();
    if default_level.is_some() {
        one.push(DEFAULT_LEVEL.to_string());
    }
    for level in levels {
        if reserved && eq_fold(&level.id, DEFAULT_LEVEL) {
            continue;
        }
        one.push(display_line(&level.id));
    }
    format!(
        "no effort {}: one of {}",
        display_line(typed),
        one.join(", ")
    )
}

/// Whether `levels` offers the level `id` (craze's `offers`).
fn offers(levels: &[LaneChoice], id: &str) -> bool {
    levels.iter().any(|l| l.id == id)
}

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

/// Go's `strings.EqualFold`, which craze matches an effort level by (and
/// spots its reserved `default` with): as many characters each, every pair
/// equal or of one [`simple_fold`] class. Unlike [`fold`] it relates `Σ` to
/// BOTH `σ` and `ς` — a case-insensitive match that can be ambiguous, as
/// craze's is.
fn eq_fold(a: &str, b: &str) -> bool {
    let (mut a, mut b) = (a.chars(), b.chars());
    loop {
        match (a.next(), b.next()) {
            (None, None) => return true,
            (Some(x), Some(y)) if x == y || simple_fold(x) == simple_fold(y) => {}
            _ => return false,
        }
    }
}

/// `c`'s Unicode SIMPLE case-folding class, as one representative — Go's
/// `unicode.SimpleFold` orbit: `σ`, `ς` and `Σ` one class, `k`, `K` and the
/// Kelvin sign another. It is `c` uppercased, then lowercased, each step
/// taken only where it maps to ONE character: an expansion (`ß` → `SS`,
/// `İ` → `i̇`) is full case folding, which Go's simple folding never does.
/// `ı` (U+0131) is the one exception, its own class: its `I` is a Turkic
/// fold only (CaseFolding.txt's `T`), not a simple one, and Go keeps it out
/// of `i`'s orbit. Checked against Go 1.25's orbits over every scalar value
/// (plan 032 C2 review): the same classes, but for the case pairs Unicode
/// added after Go's tables (15.0), which Rust's newer tables know.
fn simple_fold(c: char) -> char {
    if c == '\u{131}' {
        return c;
    }
    let upper = only_char(c.to_uppercase()).unwrap_or(c);
    only_char(upper.to_lowercase()).unwrap_or(upper)
}

/// The one character `chars` yields, if it yields exactly one.
fn only_char(mut chars: impl Iterator<Item = char>) -> Option<char> {
    let c = chars.next()?;
    chars.next().is_none().then_some(c)
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
