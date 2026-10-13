//! A craze session's **slash catalog** as the contract's [`LaneSlashCatalog`]
//! (plan 029 D6, P15; PM "Slash catalog") — read PRESENCE-AWARE and ROW BY
//! ROW, exactly as craze's own decoder reads it (`internal/agent/eventcodec.go`,
//! `wireSlashIn`).
//!
//! # Present or absent (P3)
//!
//! [`section`] reads a snapshot's `settings` or a `meta` delta's `state`:
//! `None` when the section is absent (or `null` — no section at all), else the
//! catalog it carries, which REPLACES the one held. Present and empty (`{}`)
//! is an empty catalog, not an absence. A `slash` that is not an object, or
//! an `items` that is not an array, is a section with no rows — present, and
//! empty — never an absent one: craze's rule, so one odd section still
//! replaces the catalog as the host meant it to. This is why the catalog is
//! not read through [`crate::settings::SettingsSections::from_value`], which
//! turns an unreadable section into an absent one.
//!
//! # One bad row costs that row
//!
//! A row that is not an object, a known key whose JSON type is wrong
//! (`"name":42`, `"run":5`, `"whileWorking":"yes"`, `"setting":"mode"`), or a
//! `name` that is not one token (`name_ok`: empty, whitespace, a control
//! character, U+FFFD) is dropped alone; the rows around it are kept, in order.
//! `null` is an absent member, as in craze's Go decoder. A key this build does
//! not know is ignored.
//!
//! # Values are the client's to judge
//!
//! An unknown `source`, `requires`, `run.kind`, `run.method`, `run.arg` or
//! `run.picker` is kept as the contract's `Other(raw)` — a client hides that
//! row, the decoder never drops it. Absent defaults: `whileWorking` false,
//! `run.arg` none, a `run` with no `kind` the empty — unknown — kind. craze's
//! method names map onto the contract's verbs (`method_of`); a `session.set`
//! row's `setting` becomes [`LaneSlashRun::mode`] when it sets a mode, and
//! nothing otherwise.
//!
//! **No cap** (P16): the 512-row bound is craze's SNAPSHOT bound; a delta
//! carries the catalog whole, and so does this.

use serde_json::{Map, Value};
use shed_core::lane::{
    LaneSlashArg, LaneSlashCatalog, LaneSlashKind, LaneSlashMethod, LaneSlashPicker,
    LaneSlashRequires, LaneSlashRow, LaneSlashRun, LaneSlashSource,
};

use crate::wire::method;

/// The `slash` section of `holder` — a snapshot's `settings`, a `meta`
/// delta's `state` — or `None` when it carries none (the module doc).
pub fn section(holder: &Value) -> Option<LaneSlashCatalog> {
    match holder.get("slash") {
        None | Some(Value::Null) => None,
        Some(v) => Some(catalog(v)),
    }
}

/// A present `slash` section's rows: every row that reads, in order; none
/// when the section or its `items` is not of its type.
fn catalog(v: &Value) -> LaneSlashCatalog {
    let rows = v
        .get("items")
        .and_then(Value::as_array)
        .map(|items| items.iter().filter_map(row).collect())
        .unwrap_or_default();
    LaneSlashCatalog { rows }
}

/// One row, or `None` when it does not read (the module doc).
fn row(v: &Value) -> Option<LaneSlashRow> {
    read_row(v.as_object()?).ok()
}

/// craze's `SlashNameOK`: a name a token could hold — not empty, and no
/// whitespace, control character or U+FFFD (`unicode.IsSpace`/`IsControl`
/// are Rust's `is_whitespace`/`is_control`: Unicode `White_Space` and `Cc`).
fn name_ok(name: &str) -> bool {
    !name.is_empty()
        && !name
            .chars()
            .any(|c| c.is_whitespace() || c.is_control() || c == char::REPLACEMENT_CHARACTER)
}

/// craze's method name ([`crate::wire::method`]) as the contract's verb; any
/// other kept as `Other(raw)`. The contract's words ARE craze's method names,
/// so this agrees with the contract's own serde on every input and a round
/// trip never turns an unknown method into one this build runs.
fn method_of(name: &str) -> LaneSlashMethod {
    match name {
        method::SESSION_SET_TITLE => LaneSlashMethod::SetTitle,
        method::SESSION_SET => LaneSlashMethod::Set,
        method::SESSION_STOP => LaneSlashMethod::Stop,
        other => LaneSlashMethod::Other(other.to_string()),
    }
}

/// A key of the wrong JSON type: the row is dropped.
struct Malformed;

/// A string member: absent or `null` is `None`; another type is malformed.
fn string<'a>(obj: &'a Map<String, Value>, key: &str) -> Result<Option<&'a str>, Malformed> {
    match obj.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(s)) => Ok(Some(s)),
        Some(_) => Err(Malformed),
    }
}

/// A string member that says something: `""` reads as absent, which is what
/// craze's encoder writes for it (it omits a zero value).
fn word<'a>(obj: &'a Map<String, Value>, key: &str) -> Result<Option<&'a str>, Malformed> {
    Ok(string(obj, key)?.filter(|s| !s.is_empty()))
}

/// [`word`], owned.
fn text(obj: &Map<String, Value>, key: &str) -> Result<Option<String>, Malformed> {
    Ok(word(obj, key)?.map(str::to_string))
}

/// An object member: absent or `null` is `None`; another type is malformed.
fn object<'a>(
    obj: &'a Map<String, Value>,
    key: &str,
) -> Result<Option<&'a Map<String, Value>>, Malformed> {
    match obj.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Object(o)) => Ok(Some(o)),
        Some(_) => Err(Malformed),
    }
}

fn read_row(obj: &Map<String, Value>) -> Result<LaneSlashRow, Malformed> {
    let name = string(obj, "name")?
        .filter(|n| name_ok(n))
        .ok_or(Malformed)?;
    let while_working = match obj.get("whileWorking") {
        None | Some(Value::Null) => false,
        Some(Value::Bool(b)) => *b,
        Some(_) => return Err(Malformed),
    };
    let empty = Map::new();
    Ok(LaneSlashRow {
        name: name.to_string(),
        description: text(obj, "description")?,
        source: LaneSlashSource::from_wire(string(obj, "source")?.unwrap_or_default()),
        plugin: text(obj, "plugin")?,
        qualified: text(obj, "qualified")?,
        requires: word(obj, "requires")?.map(LaneSlashRequires::from_wire),
        while_working,
        run: read_run(object(obj, "run")?.unwrap_or(&empty))?,
    })
}

fn read_run(run: &Map<String, Value>) -> Result<LaneSlashRun, Malformed> {
    let mode = match object(run, "setting")? {
        None => None,
        Some(setting) => {
            let kind = string(setting, "kind")?;
            let value = word(setting, "value")?;
            value.filter(|_| kind == Some("mode")).map(str::to_string)
        }
    };
    Ok(LaneSlashRun {
        kind: LaneSlashKind::from_wire(string(run, "kind")?.unwrap_or_default()),
        arg: word(run, "arg")?.map_or(LaneSlashArg::None, LaneSlashArg::from_wire),
        arg_hint: text(run, "argHint")?,
        method: word(run, "method")?.map(method_of),
        param: text(run, "param")?,
        mode,
        picker: word(run, "picker")?.map(LaneSlashPicker::from_wire),
        local: text(run, "local")?,
        reason: text(run, "reason")?,
    })
}

#[cfg(test)]
mod tests;
