//! A craze session's settings as the contract's [`LaneSettings`] — plan 025
//! §3.10's data. Changing one is `crate::lane`'s (`session.set`, C11): a client
//! names a value from what this module produced, and the change comes back on
//! the stream as a `meta` delta this module folds.
//!
//! # Where they come from
//!
//! - **The info document** every attach returns (and the `ready`
//!   notification's final one): `catalogs.models` (with its `revision`, absent
//!   while `0`) and `catalogs.modes`.
//! - **The snapshot's `settings`**: `model`, `mode`, `config.options`, `usage`,
//!   and `catalog` — the last catalog section a delta carried.
//! - **`meta` deltas** keep it current: each section present replaces that
//!   section WHOLE (`model`, `mode`, `config`, `usage`); `catalog` only at an
//!   EQUAL OR HIGHER `revision` than the list held (PM "Live models": a client
//!   never applies a revision lower than the one it holds — the same revision
//!   is the same list). A new incarnation is a new session: a reseed starts
//!   from a fresh [`SettingsState`], so revisions restart at `0`.
//!
//! # The order a client shows them in, computed here once
//!
//! - **Models** — craze's prescription (PM "The session info document"): the
//!   current model first, then the remembered ones by `recent` ascending, then
//!   the rest in catalog order.
//! - **Options** — the current model's `config.options`, excluding category
//!   `model` (the model row is the model list) and `mode` (the mode row),
//!   ordered `thought_level` first, then `model_config` in the provider's
//!   order, then the rest in the provider's order. A select's values keep the
//!   provider's order; a value maps as `{id: value, name}`.
//! - **Modes** in the catalog's order — and NONE when the session's
//!   capabilities say it has no switchable modes (`modes: false`, or absent on
//!   an older host): craze's rule is that a client hides, never disables, what
//!   a capability says the session cannot do (PM "Capabilities"), and `modes`
//!   is the one settings row with a capability of its own. (Its `effort` and
//!   `fastToggle` bits gate craze's own status chips, never a control: the
//!   current model's catalog is the one authority on which options there are.)
//!   **Usage** as `{contextTokens, contextWindow}`, a window of `0` (craze does
//!   not know it) read as absent.
//!
//! [`SettingsState::has_any`] is the capability half (§3.9): a session has
//! settings when it has any model, mode or option to show.

use serde::Deserialize;
use serde_json::Value;
use shed_core::lane::{LaneChoice, LaneSetting, LaneSettings, LaneUsage};

use crate::wire::{CatalogModel, Catalogs, ModeInfo, SessionInfo};

/// One config option as craze carries it (`config.options[]`, the event
/// codec's `configOption`).
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ConfigOption {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub category: String,
    #[serde(default)]
    pub current: String,
    #[serde(default)]
    pub select_values: Vec<SelectValue>,
}

/// One value a select option offers.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct SelectValue {
    #[serde(default)]
    pub value: String,
    #[serde(default)]
    pub name: String,
}

/// `config`: the provider's options, in full (`{}` for none).
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct ConfigSection {
    #[serde(default)]
    pub options: Vec<ConfigOption>,
}

/// The usage section (native only): what a context meter needs of it.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct UsageSection {
    #[serde(default)]
    pub context_tokens: u64,
    #[serde(default)]
    pub context_window: u64,
}

/// The catalog section: the models a native session offers now, and the
/// list's revision.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct CatalogSection {
    #[serde(default)]
    pub models: Vec<CatalogModel>,
    #[serde(default)]
    pub revision: u64,
}

/// The settings sections a snapshot carries and a `meta` delta changes — the
/// members this crate reads. An absent section was not touched; a present one
/// is set (to `""` or `{}` included).
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct SettingsSections {
    #[serde(default)]
    pub model: Option<String>,
    #[serde(default)]
    pub mode: Option<String>,
    #[serde(default)]
    pub config: Option<ConfigSection>,
    #[serde(default)]
    pub usage: Option<UsageSection>,
    #[serde(default)]
    pub catalog: Option<CatalogSection>,
}

impl SettingsSections {
    /// Read a snapshot's `settings` or a delta's `state` tolerantly: a section
    /// whose JSON type is wrong reads as absent, so one odd section never
    /// costs the others.
    pub fn from_value(v: &Value) -> SettingsSections {
        let Value::Object(obj) = v else {
            return SettingsSections::default();
        };
        let section = |k: &str| obj.get(k).filter(|v| !v.is_null());
        SettingsSections {
            model: section("model").and_then(|v| v.as_str().map(str::to_string)),
            mode: section("mode").and_then(|v| v.as_str().map(str::to_string)),
            config: section("config").and_then(|v| ConfigSection::deserialize(v).ok()),
            usage: section("usage").and_then(|v| UsageSection::deserialize(v).ok()),
            catalog: section("catalog").and_then(|v| CatalogSection::deserialize(v).ok()),
        }
    }
}

/// One session's settings as this lane knows them.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SettingsState {
    models: Vec<CatalogModel>,
    /// The model list's revision (PM "Live models").
    revision: u64,
    modes: Vec<ModeInfo>,
    /// The session's capabilities say it has no switchable modes: its modes
    /// are not shown (the module doc). Learned from each info document; a
    /// state that has seen none shows what it holds.
    modes_hidden: bool,
    model: Option<String>,
    mode: Option<String>,
    options: Vec<ConfigOption>,
    usage: Option<UsageSection>,
}

impl SettingsState {
    pub fn new() -> SettingsState {
        SettingsState::default()
    }

    /// An info document: its catalogs ([`SettingsState::apply_catalogs`]) and
    /// whether its capabilities offer modes. Whether anything changed.
    pub fn apply_info(&mut self, info: &SessionInfo) -> bool {
        let hidden = replace(&mut self.modes_hidden, &!info.capabilities.modes);
        self.apply_catalogs(&info.catalogs) | hidden
    }

    /// The info document's catalogs. The modes are taken whole; the model
    /// list only at an equal or higher revision than the one held — a document
    /// read before a delta and folded after it never brings back a list the
    /// session has moved past. Whether anything changed.
    pub fn apply_catalogs(&mut self, catalogs: &Catalogs) -> bool {
        let models = self.apply_models(&catalogs.models, catalogs.revision);
        replace(&mut self.modes, &catalogs.modes) | models
    }

    /// A snapshot's settings, or a `meta` delta's sections: each one present
    /// replaces its section whole; a catalog only at an equal or higher
    /// revision. Whether anything changed.
    pub fn apply_sections(&mut self, s: &SettingsSections) -> bool {
        let mut changed = false;
        if let Some(model) = &s.model {
            changed |= replace_id(&mut self.model, model);
        }
        if let Some(mode) = &s.mode {
            changed |= replace_id(&mut self.mode, mode);
        }
        if let Some(config) = &s.config {
            changed |= replace(&mut self.options, &config.options);
        }
        if let Some(usage) = s.usage {
            changed |= replace(&mut self.usage, &Some(usage));
        }
        if let Some(catalog) = &s.catalog {
            changed |= self.apply_models(&catalog.models, catalog.revision);
        }
        changed
    }

    /// A model list under the revision rule (PM "Live models"): taken only at
    /// an equal or higher revision than the one held. Whether it changed.
    fn apply_models(&mut self, models: &[CatalogModel], revision: u64) -> bool {
        if revision < self.revision {
            return false;
        }
        let revised = replace(&mut self.revision, &revision);
        if self.models == models {
            return revised;
        }
        self.models = models.to_vec();
        true
    }

    /// A change craze CONFIRMED with no revision to learn it from
    /// (`session.set` answered `rev: 0`, so no `meta` delta will carry it): its
    /// confirmed value applied to the section it names — the current model,
    /// the current mode, or one option's current value. A model's own options
    /// are not known until a `Settings` that carries them; this keeps the
    /// value the session confirmed from being lost meanwhile. Whether anything
    /// changed.
    pub fn apply_confirmed(&mut self, kind: &str, id: Option<&str>, value: &str) -> bool {
        match kind {
            "model" => replace_id(&mut self.model, value),
            "mode" => replace_id(&mut self.mode, value),
            "config" => {
                let Some(o) = id.and_then(|id| self.options.iter_mut().find(|o| o.id == id)) else {
                    return false;
                };
                if o.current == value {
                    return false;
                }
                value.clone_into(&mut o.current);
                true
            }
            _ => false,
        }
    }

    /// The model list's revision.
    pub fn revision(&self) -> u64 {
        self.revision
    }

    /// Whether the session has any model, mode or option to show — the
    /// `settings` capability (§3.9).
    pub fn has_any(&self) -> bool {
        !self.models.is_empty() || !self.shown_modes().is_empty() || self.options.iter().any(shown)
    }

    /// The modes a client shows: the catalog's, unless the capabilities say
    /// the session has none to switch.
    fn shown_modes(&self) -> &[ModeInfo] {
        if self.modes_hidden {
            &[]
        } else {
            &self.modes
        }
    }

    /// The options a client shows: the current model's, less the model and
    /// mode rows, `thought_level` first, then `model_config`, then the rest —
    /// each group in the provider's order.
    fn shown_options(&self) -> Vec<&ConfigOption> {
        let rank = |o: &ConfigOption| match o.category.as_str() {
            "thought_level" => 0,
            "model_config" => 1,
            _ => 2,
        };
        let mut shown: Vec<&ConfigOption> = self.options.iter().filter(|o| shown(o)).collect();
        // Stable: within a group, the provider's order stands.
        shown.sort_by_key(|o| rank(o));
        shown
    }

    /// The contract's settings, ordered (the module doc).
    pub fn lane_settings(&self) -> LaneSettings {
        LaneSettings {
            model: self.model.clone(),
            models: order_models(&self.models, self.model.as_deref())
                .into_iter()
                .map(|m| LaneChoice {
                    id: m.id.clone(),
                    name: name_or_id(&m.name, &m.id),
                    rank: m.recent,
                    description: None,
                })
                .collect(),
            mode: self.mode.clone(),
            modes: self
                .shown_modes()
                .iter()
                .map(|m| LaneChoice {
                    id: m.id.clone(),
                    name: name_or_id(&m.name, &m.id),
                    rank: None,
                    description: m.description.clone().filter(|d| !d.is_empty()),
                })
                .collect(),
            options: self
                .shown_options()
                .into_iter()
                .map(|o| LaneSetting {
                    id: o.id.clone(),
                    name: name_or_id(&o.name, &o.id),
                    category: o.category.clone(),
                    current: o.current.clone(),
                    values: o
                        .select_values
                        .iter()
                        .map(|v| LaneChoice {
                            id: v.value.clone(),
                            name: name_or_id(&v.name, &v.value),
                            rank: None,
                            description: None,
                        })
                        .collect(),
                })
                .collect(),
            usage: self.usage.map(|u| LaneUsage {
                context_tokens: Some(u.context_tokens),
                context_window: (u.context_window > 0).then_some(u.context_window),
            }),
        }
    }
}

/// Whether a client shows an option: every one but the model and mode rows,
/// which have rows of their own.
fn shown(o: &ConfigOption) -> bool {
    o.category != "model" && o.category != "mode"
}

/// Set `slot` to `value` — copied only when they differ. Whether it changed.
fn replace<T: PartialEq + Clone>(slot: &mut T, value: &T) -> bool {
    if slot == value {
        return false;
    }
    slot.clone_from(value);
    true
}

/// A section's current id: `""` is none. Whether it changed.
fn replace_id(slot: &mut Option<String>, id: &str) -> bool {
    let id = Some(id).filter(|i| !i.is_empty());
    if slot.as_deref() == id {
        return false;
    }
    *slot = id.map(str::to_string);
    true
}

/// A choice's shown name: its own, else its id.
fn name_or_id(name: &str, id: &str) -> String {
    if name.is_empty() { id } else { name }.to_string()
}

/// craze's model order (PM "The session info document": "A client lists the
/// current model first, then the ranked ones by rank"): the current model
/// first, then models with `recent` ascending, then the rest in catalog order.
pub fn order_models<'a>(
    models: &'a [CatalogModel],
    current: Option<&str>,
) -> Vec<&'a CatalogModel> {
    let mut first: Vec<&CatalogModel> = Vec::new();
    let mut ranked: Vec<&CatalogModel> = Vec::new();
    let mut rest: Vec<&CatalogModel> = Vec::new();
    for m in models {
        if current == Some(m.id.as_str()) && first.is_empty() {
            first.push(m);
        } else if m.recent.is_some() {
            ranked.push(m);
        } else {
            rest.push(m);
        }
    }
    ranked.sort_by_key(|m| m.recent);
    first.into_iter().chain(ranked).chain(rest).collect()
}

#[cfg(test)]
mod tests;
