//! Pure terminal-command building: the ssh argv that drops a user into a shed
//! (optionally attaching a tmux session), shared by every client's
//! `terminal.preview`. No spawning — how a terminal app is opened (the preset
//! openers) is platform-specific and lives in the clients.
//!
//! A shed is reached as `<shed>@<host> -p <sshPort>` (the shed name is the SSH
//! username; shed-server's SSH daemon routes by it), pinning the server's host
//! key in the shed CLI's `known_hosts` with strict checking. Mirrors the Swift
//! `TerminalLauncher.sshCommand` + `ShedSSH.hostKeyOptions`.

use serde::{Deserialize, Serialize};

/// The ssh command as an argv array + a shell-quoted single line (for display
/// and for handing to a terminal app).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TerminalCommand {
    pub argv: Vec<String>,
    pub command: String,
}

/// Build the ssh command to reach `shed` on `host:ssh_port`, pinning the server's
/// host key in `known_hosts` with `StrictHostKeyChecking=yes` (the shed CLI's
/// posture, so a changed/unknown key is rejected). A non-empty `session` attaches
/// that tmux session. Pure — the caller resolves `host`/`ssh_port` from config and
/// `known_hosts` from the environment.
pub fn ssh_command(
    shed: &str,
    host: &str,
    ssh_port: u16,
    known_hosts: &str,
    session: Option<&str>,
) -> TerminalCommand {
    let mut argv = vec!["ssh".to_string(), "-t".to_string()];
    argv.extend(ssh_host_key_opts(known_hosts));
    argv.push(format!("{shed}@{host}"));
    argv.push("-p".to_string());
    argv.push(ssh_port.to_string());
    if let Some(session) = session.filter(|s| !s.is_empty()) {
        argv.extend(["tmux", "attach", "-t", session].map(String::from));
    }
    let command = argv
        .iter()
        .map(|a| shell_quote(a))
        .collect::<Vec<_>>()
        .join(" ");
    TerminalCommand { argv, command }
}

/// The shared `-o` host-key options: strict checking against the shed CLI's
/// `known_hosts`. Used by both the interactive terminal ssh (`ssh_command`) and
/// the non-interactive RC ssh (`rc::ssh_argv`), so the two never drift. Mirrors
/// Swift `ShedSSH.hostKeyOptions`.
pub(crate) fn ssh_host_key_opts(known_hosts: &str) -> Vec<String> {
    vec![
        "-o".to_string(),
        "StrictHostKeyChecking=yes".to_string(),
        "-o".to_string(),
        format!("UserKnownHostsFile={known_hosts}"),
    ]
}

/// POSIX single-quote a shell argument (only when it contains anything outside a
/// conservative safe set), so the `command` line re-parses to the same argv.
/// `pub(crate)` so `rc::ssh_argv` shares the exact quoting for the remote command.
pub(crate) fn shell_quote(s: &str) -> String {
    let safe = !s.is_empty()
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"@%-_=+:,./".contains(&b));
    if safe {
        s.to_string()
    } else {
        format!("'{}'", s.replace('\'', r"'\''"))
    }
}

/// Which terminal to open a shed in. Narrowed to the two terminals used on both
/// platforms + a custom escape hatch (the Swift app also offers Terminal.app /
/// iTerm2 / Warp, which are macOS-only).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum TerminalPreset {
    Ghostty,
    Roost,
    /// The always-available default (a `{cmd}`/`{shed}` template, empty → the
    /// platform default terminal).
    #[default]
    Custom,
}

impl TerminalPreset {
    pub fn id(self) -> &'static str {
        match self {
            Self::Ghostty => "ghostty",
            Self::Roost => "roost",
            Self::Custom => "custom",
        }
    }

    pub fn label(self) -> &'static str {
        match self {
            Self::Ghostty => "Ghostty",
            Self::Roost => "Roost",
            Self::Custom => "Custom",
        }
    }

    /// One-line description shown in the Preferences picker (empty for custom,
    /// which shows a template field instead).
    pub fn detail(self) -> &'static str {
        match self {
            Self::Ghostty => "Opens the ssh command in Ghostty.",
            Self::Roost => "Opens a new Roost tab in a project named after the shed.",
            Self::Custom => "",
        }
    }

    /// The bundled opener `(interpreter, script)` for the script-backed presets;
    /// `None` for custom (resolved inline). The scripts are cross-platform.
    pub fn helper(self) -> Option<(&'static str, &'static str)> {
        match self {
            Self::Ghostty => Some(("/bin/bash", "shed-open-ghostty")),
            Self::Roost => Some(("/usr/bin/python3", "shed-open-roost.py")),
            Self::Custom => None,
        }
    }
}

/// A resolved "what to exec" for a launch — the pure output of [`resolve_launch`],
/// with no side effects, so it crosses the IPC boundary and is testable.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct LaunchInvocation {
    pub executable: String,
    pub arguments: Vec<String>,
}

/// Resolve (purely, no spawn) what to exec to open `cmd` in the chosen terminal:
/// - `Custom` → `/bin/sh -c <template>` with `{cmd}`/`{shed}` substituted (an
///   empty template falls back to the platform default terminal).
/// - script presets → `<interp> <scripts_dir>/<script> <shed> <cmd.command>`
///   (the opener scripts handle macOS vs Linux themselves); a missing
///   `scripts_dir` (e.g. an unbundled dev run) falls back too.
///
/// Mirrors the Swift `TerminalLauncher.resolveLaunch`.
pub fn resolve_launch(
    preset: TerminalPreset,
    cmd: &TerminalCommand,
    shed: &str,
    custom_template: Option<&str>,
    scripts_dir: Option<&str>,
) -> LaunchInvocation {
    match preset {
        TerminalPreset::Custom => {
            let template = custom_template.map(str::trim).unwrap_or_default();
            if template.is_empty() {
                return default_terminal(cmd);
            }
            let expanded = template.replace("{cmd}", &cmd.command).replace("{shed}", shed);
            LaunchInvocation {
                executable: "/bin/sh".to_string(),
                arguments: vec!["-c".to_string(), expanded],
            }
        }
        _ => match (preset.helper(), scripts_dir) {
            (Some((interp, script)), Some(dir)) => LaunchInvocation {
                executable: interp.to_string(),
                arguments: vec![format!("{dir}/{script}"), shed.to_string(), cmd.command.clone()],
            },
            _ => default_terminal(cmd),
        },
    }
}

/// The platform default terminal running the ssh command — the fallback when a
/// preset's opener is unavailable (no bundled scripts / empty custom template).
fn default_terminal(cmd: &TerminalCommand) -> LaunchInvocation {
    #[cfg(target_os = "macos")]
    {
        let escaped = cmd.command.replace('\\', "\\\\").replace('"', "\\\"");
        let script = format!("tell application \"Terminal\"\nactivate\ndo script \"{escaped}\"\nend tell");
        LaunchInvocation {
            executable: "/usr/bin/osascript".to_string(),
            arguments: vec!["-e".to_string(), script],
        }
    }
    #[cfg(not(target_os = "macos"))]
    {
        LaunchInvocation {
            executable: "x-terminal-emulator".to_string(),
            arguments: vec![
                "-e".to_string(),
                "/bin/sh".to_string(),
                "-lc".to_string(),
                cmd.command.clone(),
            ],
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ssh_command_pins_host_key_and_targets_the_shed() {
        let c = ssh_command("web", "10.0.0.5", 2222, "/home/u/.shed/known_hosts", None);
        assert_eq!(
            c.argv,
            [
                "ssh", "-t",
                "-o", "StrictHostKeyChecking=yes",
                "-o", "UserKnownHostsFile=/home/u/.shed/known_hosts",
                "web@10.0.0.5", "-p", "2222",
            ]
        );
        // The shell line round-trips (no quoting needed for these safe args).
        assert_eq!(
            c.command,
            "ssh -t -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/home/u/.shed/known_hosts web@10.0.0.5 -p 2222"
        );
    }

    #[test]
    fn ssh_command_attaches_tmux_when_session_given() {
        let c = ssh_command("web", "h", 22, "/k", Some("main"));
        assert_eq!(&c.argv[c.argv.len() - 4..], ["tmux", "attach", "-t", "main"]);
        // An empty session is treated as none.
        let c0 = ssh_command("web", "h", 22, "/k", Some(""));
        assert!(!c0.argv.contains(&"tmux".to_string()));
    }

    #[test]
    fn shell_quote_escapes_unsafe_args() {
        // A known_hosts path with a space must be quoted so the line re-parses.
        let c = ssh_command("web", "h", 22, "/home/a b/.shed/known_hosts", None);
        assert!(c.command.contains("'UserKnownHostsFile=/home/a b/.shed/known_hosts'"));
        assert_eq!(shell_quote("it's"), r"'it'\''s'");
    }

    #[test]
    fn resolve_launch_custom_expands_template() {
        let cmd = ssh_command("web", "h", 22, "/k", None);
        let inv = resolve_launch(
            TerminalPreset::Custom,
            &cmd,
            "web",
            Some("kitty -e {cmd} # {shed}"),
            None,
        );
        assert_eq!(inv.executable, "/bin/sh");
        assert_eq!(inv.arguments[0], "-c");
        assert!(inv.arguments[1].contains(&cmd.command)); // {cmd} substituted
        assert!(inv.arguments[1].contains("# web")); // {shed} substituted
    }

    #[test]
    fn resolve_launch_script_presets_run_the_bundled_opener() {
        let cmd = ssh_command("web", "h", 22, "/k", None);
        let g = resolve_launch(TerminalPreset::Ghostty, &cmd, "web", None, Some("/opt/bin"));
        assert_eq!(g.executable, "/bin/bash");
        assert_eq!(g.arguments, ["/opt/bin/shed-open-ghostty", "web", &cmd.command]);
        let r = resolve_launch(TerminalPreset::Roost, &cmd, "web", None, Some("/opt/bin"));
        assert_eq!(r.executable, "/usr/bin/python3");
        assert_eq!(r.arguments[0], "/opt/bin/shed-open-roost.py");
    }

    #[test]
    fn resolve_launch_falls_back_when_opener_missing() {
        // A script preset without a scripts_dir, or an empty custom template, still
        // yields a runnable invocation carrying the ssh command (platform default).
        let cmd = ssh_command("web", "h", 22, "/k", None);
        for inv in [
            resolve_launch(TerminalPreset::Ghostty, &cmd, "web", None, None),
            resolve_launch(TerminalPreset::Custom, &cmd, "web", Some("  "), None),
        ] {
            assert!(!inv.executable.is_empty());
            assert!(inv.arguments.iter().any(|a| a.contains(&cmd.command)));
        }
    }

    #[test]
    fn terminal_preset_serde_is_kebab() {
        assert_eq!(
            serde_json::to_string(&TerminalPreset::Ghostty).unwrap(),
            "\"ghostty\""
        );
        assert_eq!(
            serde_json::from_str::<TerminalPreset>("\"custom\"").unwrap(),
            TerminalPreset::Custom
        );
    }
}
