//! The terminal operations (preset resolution, launch, install detection, the
//! terminal preference), factored out of the IPC handler so the frontend `invoke`
//! commands share the exact same logic — the two surfaces differ only in envelope
//! (an IPC `{code,message}` error vs a command `String`), not behavior.

use std::path::Path;
use std::process::Command;
use std::sync::Arc;

use serde_json::{json, Value};

use shed_app::Backend;
use shed_core::terminal::{self, TerminalPreset};

use crate::prefs::SharedPrefs;

/// An IPC-shaped `(code, message)` error; the frontend commands drop the code.
pub type TermError = (String, String);

fn err(code: &str, message: impl Into<String>) -> TermError {
    (code.to_string(), message.into())
}

/// Shared by the IPC handler + the invoke commands. Holds the backend (SSH target
/// resolution), the persisted prefs, and the bundled-opener scripts dir.
pub struct TerminalCtl {
    backend: Arc<Backend>,
    prefs: SharedPrefs,
    scripts_dir: Option<String>,
}

impl TerminalCtl {
    pub fn new(backend: Arc<Backend>, prefs: SharedPrefs, scripts_dir: Option<String>) -> Self {
        Self {
            backend,
            prefs,
            scripts_dir,
        }
    }

    /// The offerable presets + whether each is installed (the picker's source).
    pub fn presets(&self) -> Value {
        let presets: Vec<Value> = [
            TerminalPreset::Ghostty,
            TerminalPreset::Roost,
            TerminalPreset::Custom,
        ]
        .into_iter()
        .map(|p| {
            json!({
                "id": p.id(),
                "label": p.label(),
                "detail": p.detail(),
                "available": preset_available(p),
            })
        })
        .collect();
        json!({ "presets": presets })
    }

    /// The persisted prefs (terminal preset + template).
    pub fn prefs_get(&self) -> Value {
        let p = self.prefs.get();
        json!({
            "terminal_preset": p.terminal_preset,
            "terminal_template": p.terminal_template,
        })
    }

    /// Persist the terminal preference.
    pub fn prefs_set_terminal(&self, preset: &str, template: Option<String>) -> Result<Value, TermError> {
        self.prefs.set_terminal(parse_preset(preset)?, template);
        Ok(json!({}))
    }

    /// The ssh command + resolved preset/invocation — WITHOUT spawning.
    pub fn preview(
        &self,
        host: Option<&str>,
        shed: &str,
        session: Option<&str>,
        preset: Option<&str>,
        template: Option<String>,
    ) -> Result<Value, TermError> {
        let cmd = self
            .backend
            .terminal_preview(host, shed, session)
            .map_err(|e| err("action_failed", e.to_string()))?;
        let (preset, template) = self.resolve_pref(preset, template)?;
        let inv =
            terminal::resolve_launch(preset, &cmd, shed, template.as_deref(), self.scripts_dir.as_deref());
        Ok(json!({
            "argv": cmd.argv,
            "command": cmd.command,
            "preset": preset,
            "invocation": inv,
        }))
    }

    /// Spawn the resolved opener. The CALLER enforces the test-mode gate (a spawn
    /// isn't hermetic).
    pub fn open(
        &self,
        host: Option<&str>,
        shed: &str,
        session: Option<&str>,
        preset: Option<&str>,
        template: Option<String>,
    ) -> Result<Value, TermError> {
        let cmd = self
            .backend
            .terminal_preview(host, shed, session)
            .map_err(|e| err("action_failed", e.to_string()))?;
        let (preset, template) = self.resolve_pref(preset, template)?;
        let inv =
            terminal::resolve_launch(preset, &cmd, shed, template.as_deref(), self.scripts_dir.as_deref());
        let child = Command::new(&inv.executable)
            .args(&inv.arguments)
            .spawn()
            .map_err(|e| err("action_failed", format!("spawn {}: {e}", inv.executable)))?;
        // Reap the short-lived opener so it doesn't linger as a zombie.
        std::thread::spawn(move || {
            let mut child = child;
            let _ = child.wait();
        });
        Ok(json!({ "command": cmd.command }))
    }

    /// The preset + template for an op: an explicit `preset` wins (with its
    /// optional template); otherwise the persisted pref — so the shed-card button
    /// opens the user's chosen terminal.
    fn resolve_pref(
        &self,
        preset: Option<&str>,
        template: Option<String>,
    ) -> Result<(TerminalPreset, Option<String>), TermError> {
        match preset {
            Some(s) => Ok((parse_preset(s)?, template)),
            None => {
                let p = self.prefs.get();
                Ok((p.terminal_preset, Some(p.terminal_template)))
            }
        }
    }
}

/// Parse an explicit preset string (kebab), or a `bad_request` naming it.
fn parse_preset(s: &str) -> Result<TerminalPreset, TermError> {
    serde_json::from_value(Value::String(s.to_string()))
        .map_err(|_| err("bad_request", format!("unknown preset: {s}")))
}

/// Whether a preset's terminal is installed (drives the picker). Custom is always
/// offered; the script presets need their app (+ python3 for Roost).
fn preset_available(preset: TerminalPreset) -> bool {
    match preset {
        TerminalPreset::Custom => true,
        TerminalPreset::Ghostty => app_installed("ghostty", "Ghostty"),
        TerminalPreset::Roost => {
            app_installed("roost", "Roost") && Path::new("/usr/bin/python3").exists()
        }
    }
}

#[cfg(target_os = "macos")]
fn app_installed(_cli: &str, app: &str) -> bool {
    let home = std::env::var("HOME").unwrap_or_default();
    [
        format!("/Applications/{app}.app"),
        format!("{home}/Applications/{app}.app"),
    ]
    .iter()
    .any(|p| Path::new(p).exists())
}

#[cfg(not(target_os = "macos"))]
fn app_installed(cli: &str, _app: &str) -> bool {
    std::env::var_os("PATH")
        .is_some_and(|paths| std::env::split_paths(&paths).any(|dir| dir.join(cli).is_file()))
}

pub type SharedTerminal = Arc<TerminalCtl>;
