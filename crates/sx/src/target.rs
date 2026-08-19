//! The `--on` target model: where a porcelain verb runs (plan 009 §3.2).
//!
//! Three targets, one grammar:
//!
//! | spelling | meaning |
//! |---|---|
//! | `local` (or omitted) | this machine, in-process engine, local tmux |
//! | `machine:<name>` | a `machines:` entry — SSH to a native host running `sx` |
//! | `shed:<name>` | a shed, resolved across the configured servers |
//! | `shed:<name>@<server>` | the same, pinned to one server (skips the HTTP lookup) |
//!
//! Parsing is PURE (a string in, a [`Target`] out) and resolution is a second,
//! separate step against a [`ShedConfig`] — so the grammar is unit-testable with
//! no config file, and the config lookup is unit-testable with no argv.

use shed_core::config::{MachineEntry, ShedConfig};

/// The `sx` binary a `machine:` target invokes when its entry names none —
/// resolved on the machine's non-login SSH `PATH`.
pub const DEFAULT_MACHINE_SX_BIN: &str = "sx";

/// The `sx` namespace carrying the one-shot engine verbs.
const RC_NAMESPACE: &str = "rc";

/// The guest RC helper inside a shed. Baked into the `extensions`/`full` images
/// and on the shed user's login PATH.
pub const SHED_RC_BIN: &str = "shed-ext-rc";

/// A parsed `--on` value, before any config lookup.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum Target {
    #[default]
    Local,
    Machine(String),
    Shed {
        name: String,
        /// The `@<server>` qualifier, when given.
        server: Option<String>,
    },
}

impl Target {
    /// Parse an `--on` value. An empty string is [`Target::Local`] (the flag's
    /// absent default), so a caller never has to special-case "not given".
    pub fn parse(raw: &str) -> Result<Target, String> {
        let raw = raw.trim();
        if raw.is_empty() || raw == "local" {
            return Ok(Target::Local);
        }
        // Each SEGMENT is trimmed, not just the whole value: `--on "machine: mini2"`
        // is what a human types (and what a shell here-doc or a YAML scalar hands
        // us), and treating the space as part of the name would fail with a
        // baffling `no machine " mini2"`.
        if let Some(rest) = raw.strip_prefix("machine:") {
            let rest = rest.trim();
            if rest.is_empty() {
                return Err("machine target needs a name: --on machine:<name>".to_string());
            }
            return Ok(Target::Machine(rest.to_string()));
        }
        if let Some(rest) = raw.strip_prefix("shed:") {
            // Split on the LAST '@' so a shed name may not contain one but a
            // server name is free to be an ordinary host-ish label.
            let (name, server) = match rest.rsplit_once('@') {
                Some((n, s)) => (n.trim(), Some(s.trim())),
                None => (rest.trim(), None),
            };
            if name.is_empty() {
                return Err("shed target needs a name: --on shed:<name>[@<server>]".to_string());
            }
            if server.is_some_and(str::is_empty) {
                return Err("shed target's @server qualifier is empty".to_string());
            }
            return Ok(Target::Shed {
                name: name.to_string(),
                server: server.map(str::to_string),
            });
        }
        Err(format!(
            "unknown target {raw:?}: expected local, machine:<name>, or shed:<name>[@<server>]"
        ))
    }

    /// The label stamped into the session's `SHED_RC_TARGET` (and its DTO's
    /// `target_label`) — the provenance a watching tool reads back. Local
    /// sessions carry none, matching the Go `claude` verb this absorbs.
    ///
    /// A shed label is `shed:<name>@<server>`, the shape `RcService::launch`
    /// stamps (`shed-app/src/rc.rs`). It always carries the server because
    /// `porcelain::pin_shed_server` resolves an unqualified `shed:<name>` to the
    /// server it was found on BEFORE anything reads this — a bare `shed:<name>`
    /// would be ambiguous provenance on a multi-server fleet, and would not match
    /// what the desktop writes for the same session.
    pub fn label(&self) -> String {
        match self {
            Target::Local => String::new(),
            Target::Machine(name) => format!("machine:{name}"),
            Target::Shed { name, server } => match server {
                Some(s) => format!("shed:{name}@{s}"),
                None => format!("shed:{name}"),
            },
        }
    }

    /// How the target reads in a summary line / an error / a `ls` row.
    pub fn display(&self) -> String {
        match self {
            Target::Local => "local".to_string(),
            other => other.label(),
        }
    }

    /// **The dispatch table's interactive-shell column** (plan 009 §3.2).
    ///
    /// On this machine and on a native machine the inner command is wrapped in
    /// `bash -ic` so a tool installed by a shell rc-file (nvm, mise, asdf, a
    /// brew shellenv) is on PATH. In a SHED it must stay OFF: the guest is
    /// reached over SSH, whose `bash -lc` wrap already supplies the login PATH,
    /// and `-ic` there would source an interactive rc-file that does not exist.
    pub fn interactive_shell(&self) -> bool {
        !matches!(self, Target::Shed { .. })
    }
}

/// A target resolved against `~/.shed/config.yaml`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Resolved {
    Local,
    Machine(MachineEntry),
    Shed {
        name: String,
        server: Option<String>,
    },
}

impl Resolved {
    pub fn target(&self) -> Target {
        match self {
            Resolved::Local => Target::Local,
            Resolved::Machine(m) => Target::Machine(m.name.clone()),
            Resolved::Shed { name, server } => Target::Shed {
                name: name.clone(),
                server: server.clone(),
            },
        }
    }

    pub fn display(&self) -> String {
        self.target().display()
    }
}

/// Resolve a parsed target against the config. Only `machine:` needs the config
/// (its entry carries the host/user/port/binary); a `shed:` target's SSH endpoint
/// is resolved later, through `shed-app`'s `Backend` (which owns server
/// resolution and the `~/.shed/known_hosts` pin).
pub fn resolve(target: &Target, config: &ShedConfig) -> Result<Resolved, String> {
    match target {
        Target::Local => Ok(Resolved::Local),
        Target::Machine(name) => match config.machine(name) {
            Some(entry) => Ok(Resolved::Machine(entry.clone())),
            None => {
                let known: Vec<&str> = config.machines.iter().map(|m| m.name.as_str()).collect();
                Err(if known.is_empty() {
                    format!(
                        "no machine {name:?} in the config: add a `machines:` section to \
                         ~/.shed/config.yaml (see `sx help`)"
                    )
                } else {
                    format!(
                        "no machine {name:?} in the config (have: {})",
                        known.join(", ")
                    )
                })
            }
        },
        Target::Shed { name, server } => Ok(Resolved::Shed {
            name: name.clone(),
            server: server.clone(),
        }),
    }
}

/// The RC argv prefix to invoke on a machine target: `<sx> rc`.
/// `machines[].rc_bin` names WHERE SX LIVES on that machine — an absolute path
/// when it is not on the non-login `PATH` an SSH exec sees — and the `rc`
/// namespace is always appended.
pub fn machine_rc_prefix(entry: &MachineEntry) -> Vec<String> {
    vec![
        entry
            .rc_bin
            .as_deref()
            .unwrap_or(DEFAULT_MACHINE_SX_BIN)
            .to_string(),
        RC_NAMESPACE.to_string(),
    ]
}

/// `~/.shed/config.yaml` — the same path the Go CLI computes
/// (`config.GetClientConfigPath`, `internal/config/client.go:194`), which has NO
/// env override of its own. `SHED_CONFIG` is an **sx-only** escape hatch for
/// dev/e2e runs against a scratch config; nothing in the Go tree reads it, so it
/// can never make `shed` and `sx` disagree about a real config.
pub fn default_config_path(env: &dyn Fn(&str) -> String) -> String {
    let override_path = env("SHED_CONFIG");
    if !override_path.is_empty() {
        return override_path;
    }
    let home = env("HOME");
    format!("{home}/.shed/config.yaml")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(raw: &str) -> Target {
        Target::parse(raw).unwrap_or_else(|e| panic!("parse {raw:?}: {e}"))
    }

    #[test]
    fn target_grammar_table() {
        let cases: &[(&str, Target)] = &[
            ("", Target::Local),
            ("local", Target::Local),
            ("  local  ", Target::Local),
            ("machine:mini2", Target::Machine("mini2".into())),
            // Each segment is trimmed, so a human-typed space after the colon
            // (or around the @server) is not part of the name.
            ("machine: mini2", Target::Machine("mini2".into())),
            (
                "shed: web @ mini3 ",
                Target::Shed {
                    name: "web".into(),
                    server: Some("mini3".into()),
                },
            ),
            (
                "shed:web",
                Target::Shed {
                    name: "web".into(),
                    server: None,
                },
            ),
            (
                "shed:web@mini3",
                Target::Shed {
                    name: "web".into(),
                    server: Some("mini3".into()),
                },
            ),
        ];
        for (raw, want) in cases {
            assert_eq!(&parse(raw), want, "target {raw:?}");
        }
    }

    #[test]
    fn malformed_targets_are_rejected_with_the_grammar() {
        for raw in [
            "mini2",         // bare name — ambiguous between a machine and a shed
            "machine:",      // no name
            "machine:   ",   // whitespace is trimmed away to nothing
            "shed:",         // no name
            "shed:@mini3",   // no name, only a server
            "shed:  @mini3", // …and whitespace does not make one
            "shed:web@",     // empty server
            "shed:web@  ",   // …nor a server
            "sheds:web",     // typo
            "machine",       // missing the colon
        ] {
            assert!(
                Target::parse(raw).is_err(),
                "target {raw:?} should be rejected"
            );
        }
    }

    /// The plan 009 §3.2 dispatch table's interactive-shell column, asserted as a
    /// table so a future target can't quietly inherit the wrong posture.
    #[test]
    fn interactive_shell_posture_matrix() {
        assert!(parse("local").interactive_shell());
        assert!(parse("machine:mini2").interactive_shell());
        assert!(!parse("shed:web").interactive_shell());
        assert!(!parse("shed:web@mini3").interactive_shell());
    }

    #[test]
    fn labels_carry_provenance_and_local_carries_none() {
        assert_eq!(parse("local").label(), "");
        assert_eq!(parse("local").display(), "local");
        assert_eq!(parse("machine:mini2").label(), "machine:mini2");
        assert_eq!(parse("shed:web").label(), "shed:web");
        assert_eq!(parse("shed:web@mini3").label(), "shed:web@mini3");
    }

    fn config() -> ShedConfig {
        ShedConfig::parse(
            "\
machines:
    mini2:
        host: mini2.local
        rc_bin: /opt/homebrew/bin/sx
    plain: {}
",
        )
    }

    #[test]
    fn resolving_a_machine_reads_its_entry_and_its_rc_prefix() {
        let cfg = config();
        let Resolved::Machine(entry) = resolve(&parse("machine:mini2"), &cfg).unwrap() else {
            panic!("expected a machine");
        };
        assert_eq!(entry.host, "mini2.local");
        // An override says WHERE sx lives; the `rc` namespace is still appended.
        assert_eq!(
            machine_rc_prefix(&entry),
            vec!["/opt/homebrew/bin/sx".to_string(), "rc".to_string()]
        );

        let Resolved::Machine(entry) = resolve(&parse("machine:plain"), &cfg).unwrap() else {
            panic!("expected a machine");
        };
        assert_eq!(entry.host, "plain");
        assert_eq!(
            machine_rc_prefix(&entry),
            vec!["sx".to_string(), "rc".to_string()]
        );
    }

    #[test]
    fn an_unknown_machine_names_the_configured_ones() {
        let err = resolve(&parse("machine:ghost"), &config()).unwrap_err();
        assert!(err.contains("ghost"), "{err}");
        assert!(err.contains("mini2") && err.contains("plain"), "{err}");

        // With no machines at all the error points at the fix instead.
        let err = resolve(&parse("machine:ghost"), &ShedConfig::default()).unwrap_err();
        assert!(err.contains("machines:"), "{err}");
    }

    #[test]
    fn local_and_shed_resolve_without_touching_the_config() {
        let empty = ShedConfig::default();
        assert_eq!(resolve(&parse("local"), &empty).unwrap(), Resolved::Local);
        assert_eq!(
            resolve(&parse("shed:web@mini3"), &empty).unwrap(),
            Resolved::Shed {
                name: "web".into(),
                server: Some("mini3".into())
            }
        );
    }

    #[test]
    fn config_path_defaults_under_home_and_honors_the_sx_override() {
        let home_only = |k: &str| match k {
            "HOME" => "/Users/dev".to_string(),
            _ => String::new(),
        };
        assert_eq!(
            default_config_path(&home_only),
            "/Users/dev/.shed/config.yaml"
        );
        let overridden = |k: &str| match k {
            "HOME" => "/Users/dev".to_string(),
            "SHED_CONFIG" => "/tmp/scratch.yaml".to_string(),
            _ => String::new(),
        };
        assert_eq!(default_config_path(&overridden), "/tmp/scratch.yaml");
    }
}
