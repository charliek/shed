//! **The craze remote command** — how a client finds and runs `craze` on a host
//! it reaches without a login shell. The PURE half, like [`crate::machine`]: the
//! argv is composed here, and no process is spawned and no socket is opened.
//! The transports belong to the clients (plan 025 §3.3.2, §3.6.6) — an ssh
//! exec or a local `/bin/sh` child on the desktop, an ssh exec behind a stable
//! loopback port on the phone, roost's `tab.open` for Open in terminal — and
//! each runs exactly the argv this module hands it (plan 025 §3.4, P9).
//!
//! **Three forms, one ladder.** [`bridge_hub_argv`] (`… bridge --hub`, every
//! connection to a machine's hub), [`providers_hub_argv`] (`… providers --hub
//! --json`, the desktop's find-only probe for a remote machine — it answers
//! `craze providers: no hub is running`, exit 1, and never starts a hub, where
//! `bridge --hub` always does) and [`attach_argv`] (`… attach --session
//! '<hostId>'`, Open in terminal). Each is `["sh", "-c", <script>]`: one argv
//! element, so the far side never has to source a login shell (and its stdout
//! pollution, shed#231) just to find the binary.
//!
//! **The script is craze's published ladder, verbatim** — craze's
//! `docs/reference/protocol.md`, "SSH exec: what a client may assume", published
//! "for shed … to copy verbatim" so that "anything the probe can find, the
//! transport can exec" stays a property of one list, not two that drift: nine
//! rungs, each `[ -f "$p" ] && [ -x "$p" ] && exec "$p" …` (`[ -f ]` as well as
//! `[ -x ]`, because an executable *directory* passes `[ -x ]` and would then
//! fail the `exec` at 126), in the order [`RUNGS`] gives, then `craze: command
//! not found` on stderr and exit 127. `$HOME`-relative rungs are tried only
//! when `$HOME` is **absolute** (a relative `HOME=.` must not turn
//! `$HOME/.local/bin/craze` into a path under whatever directory the shell
//! started in), and the `command -v` rung only when it answers with an absolute
//! path. There is no `~/go/bin` rung: adding one would change craze's published
//! contract, and `go install` is not a supported install path.
//!
//! **One change, and only at the `exec`: the enhanced PATH.** A bridge's
//! environment becomes its hub's, and the hub hands it to every host and agent
//! it starts — so an ssh exec's `PATH=/usr/bin:/bin`, or a macOS GUI app's,
//! hides every agent in `~/.local/bin` or Homebrew, and the hub would read them
//! `unavailable` and fail every create. So a prologue computes — and never
//! exports — `cz_path`: the ladder's own directories ([`EXEC_PATH`], the same
//! guards, the same order) followed by the original `$PATH`, and each rung's
//! `exec "$p" …` becomes `PATH="$cz_path" exec "$p" …`. The prologue opens
//! with `unset cz_path`: a plain `cz_path=` keeps the export attribute of a
//! `cz_path` the caller's environment already carried (dash, bash and busybox
//! `sh` alike), and craze would then inherit the computed value. The ladder
//! itself still resolves against the **original** PATH: rung 2's `command -v`
//! must prefer the user's own PATH entry over an injected Homebrew directory. craze
//! confirmed (2026-10-04) that this is compatible with its design and is the
//! behaviour, not a stopgap; craze's own sanctioned knob for an agent anywhere
//! else is an absolute path in `[agents]` in `~/.craze/config.toml`. Two caveats
//! a user can meet: a hub's environment is fixed at birth (a hub a local TUI
//! started keeps its own PATH until it idles out), and an agent outside these
//! directories needs `[agents]`.
//!
//! **The composer is pure over two tables** — the rung list and the exec-PATH
//! list, [`Ladder`] — with the production constants ([`LADDER`]) as one
//! instance. The behaviour tests (`tests/craze_ladder.rs`) re-root the
//! production tables under a temp dir, so they run every rung for real with a
//! local `sh` and no root; the production constants themselves run in the
//! machine-transport suite's Docker recipe, where the container can put stubs
//! at `/usr/local/bin` and `/opt/homebrew/bin`.
//!
//! **The jailed variant is for test mode only.** [`JAILED_LADDER`] keeps rungs
//! 1 (`$HOME/.local/bin`) and 2 (`command -v`) and **no** exec-PATH: a hermetic
//! run controls `HOME` and `PATH`, so nothing it did not put there — no
//! absolute rung (`/usr/local/bin/craze` can be a real, stale v0.0.1 on a dev
//! box) and no injected `/opt/homebrew/bin` (a Homebrew `cursor-agent` would
//! flip a hermetic recipe's "cursor unavailable") — can reach it. It mirrors
//! roost's own jailed chain, and it is pinned by a unit golden here rather than
//! a machine-transport scenario, because no production transport sends it.
//!
//! **Raw vs displayed.** The raw script carries the host id in one ordinary pair
//! of single quotes, `'<hostId>'` — validated as twelve lowercase hex digits
//! first, because the client does not control its provenance, and quoted all
//! the same. That raw argv is what roost's `tab.open` runs (no ssh). A transport
//! that needs ONE command string (ssh) renders the argv with
//! [`crate::machine::display_line`], which single-quotes the whole script and so
//! spells each inner `'` as `'\''` — the house quoter's escape, the same one every
//! `tests/machine-transport` scenario pins on the wire. (craze's own published
//! example spells it `'"'"'`; the two deliver the identical argv, and the live
//! leg proves ours through a real sshd.) An escape sequence of either kind never
//! appears in the raw argv.
//!
//! Pinned by `tests/machine-transport`: the `craze-bridge-hub` and
//! `craze-providers-hub` scenarios' argv EQUAL [`bridge_hub_argv`] and
//! [`providers_hub_argv`] (asserted by the Rust leg), and both goldens record
//! them — `wire.json` byte for byte, `received.json` through a real sshd.

use crate::machine::display_line;

/// The binary every rung names.
pub const BIN_NAME: &str = "craze";

/// A directory the ladder looks in — the one vocabulary both tables share, so a
/// rung and its exec-PATH entry carry the same guard by construction.
///
/// Every string is spliced into the script inside double quotes, so it must be
/// plain path text: [`Ladder`]'s composer refuses (panics on) anything but
/// ASCII letters, digits and `/._+@-`, and any string that does not start with
/// `/`. The production constants are plain by inspection; the guard is for a
/// test that re-roots them under a temp dir.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Dir<'a> {
    /// `$HOME` followed by this suffix (`"/.local/bin"`), tried only when
    /// `$HOME` is absolute: `case "${HOME:-}" in /*) …;; esac`.
    Home(&'a str),
    /// `prefix` + `$USER` + `suffix` (`/etc/profiles/per-user/$USER/bin`),
    /// tried only when `$USER` is set and non-empty: `if [ -n "${USER:-}" ];
    /// then …; fi`.
    PerUser { prefix: &'a str, suffix: &'a str },
    /// An absolute directory, tried unconditionally.
    Fixed(&'a str),
}

/// One rung of the ladder.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Rung<'a> {
    /// `<dir>/craze`, under the directory's own guard.
    At(Dir<'a>),
    /// `command -v craze` against the ORIGINAL `PATH`, accepted only when it
    /// answers with an absolute path (a builtin, function or alias answers with
    /// a bare word; a relative PATH entry is a hazard, not a convenience).
    PathLookup,
}

/// The two tables the composer is pure over.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Ladder<'a> {
    /// The rungs, in the order they are tried; the first executable regular
    /// file wins.
    pub rungs: &'a [Rung<'a>],
    /// The directories prepended, in this order, to the original `PATH` the
    /// exec'd craze sees. **Empty means no enhancement at all**: no prologue
    /// and a plain `exec "$p" …`, so `PATH` reaches craze exactly as it was.
    pub exec_path: &'a [Dir<'a>],
}

const HOME_LOCAL_BIN: Dir<'static> = Dir::Home("/.local/bin");
const OPT_HOMEBREW_BIN: Dir<'static> = Dir::Fixed("/opt/homebrew/bin");
const USR_LOCAL_BIN: Dir<'static> = Dir::Fixed("/usr/local/bin");
const LINUXBREW_BIN: Dir<'static> = Dir::Fixed("/home/linuxbrew/.linuxbrew/bin");
const USR_BIN: Dir<'static> = Dir::Fixed("/usr/bin");
const NIX_PROFILE_BIN: Dir<'static> = Dir::Home("/.nix-profile/bin");
const NIX_PER_USER_BIN: Dir<'static> = Dir::PerUser {
    prefix: "/etc/profiles/per-user/",
    suffix: "/bin",
};
const NIXOS_SYSTEM_BIN: Dir<'static> = Dir::Fixed("/run/current-system/sw/bin");

/// craze's published ladder, rung for rung and in its order.
pub const RUNGS: [Rung<'static>; 9] = [
    Rung::At(HOME_LOCAL_BIN),
    Rung::PathLookup,
    Rung::At(OPT_HOMEBREW_BIN),
    Rung::At(USR_LOCAL_BIN),
    Rung::At(LINUXBREW_BIN),
    Rung::At(USR_BIN),
    Rung::At(NIX_PROFILE_BIN),
    Rung::At(NIX_PER_USER_BIN),
    Rung::At(NIXOS_SYSTEM_BIN),
];

/// The exec PATH's directories: the ladder's own directory list, in its own
/// order (every rung but `command -v`), so anything the ladder can find, the
/// hub can also find among the agents.
pub const EXEC_PATH: [Dir<'static>; 8] = [
    HOME_LOCAL_BIN,
    OPT_HOMEBREW_BIN,
    USR_LOCAL_BIN,
    LINUXBREW_BIN,
    USR_BIN,
    NIX_PROFILE_BIN,
    NIX_PER_USER_BIN,
    NIXOS_SYSTEM_BIN,
];

/// The production ladder: every rung, and the exec PATH.
pub const LADDER: Ladder<'static> = Ladder {
    rungs: &RUNGS,
    exec_path: &EXEC_PATH,
};

const JAILED_RUNGS: [Rung<'static>; 2] = [RUNGS[0], RUNGS[1]];

/// The test-mode ladder: rungs 1 and 2 only, and no exec PATH (module doc).
pub const JAILED_LADDER: Ladder<'static> = Ladder {
    rungs: &JAILED_RUNGS,
    exec_path: &[],
};

/// The arguments every form passes; the host id is the only interpolated value.
const BRIDGE_HUB_ARGS: &str = "bridge --hub";
const PROVIDERS_HUB_ARGS: &str = "providers --hub --json";

/// A host id [`attach_argv`] refused: not twelve lowercase hex digits.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("not a craze host id (twelve lowercase hex digits): {0:?}")]
pub struct BadHostId(pub String);

impl Ladder<'_> {
    /// `["sh", "-c", <ladder exec'ing `craze bridge --hub`>]`.
    pub fn bridge_hub_argv(&self) -> Vec<String> {
        self.argv(BRIDGE_HUB_ARGS)
    }

    /// `["sh", "-c", <ladder exec'ing `craze providers --hub --json`>]`.
    pub fn providers_hub_argv(&self) -> Vec<String> {
        self.argv(PROVIDERS_HUB_ARGS)
    }

    /// `["sh", "-c", <ladder exec'ing `craze attach --session '<host_id>'`>]`,
    /// or [`BadHostId`] when `host_id` is not exactly twelve lowercase hex
    /// digits — the shape craze mints (`protocol.md`: "twelve lowercase hex
    /// digits"), and the only one a roster row can legitimately carry.
    pub fn attach_argv(&self, host_id: &str) -> Result<Vec<String>, BadHostId> {
        let valid = host_id.len() == 12
            && host_id
                .bytes()
                .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b));
        if !valid {
            return Err(BadHostId(host_id.to_string()));
        }
        // One ordinary pair of single quotes, exactly as craze quotes a
        // session id: nothing in the id can end a quote early (it was just
        // validated), and it is quoted all the same.
        Ok(self.argv(&format!("attach --session '{host_id}'")))
    }

    fn argv(&self, args: &str) -> Vec<String> {
        vec!["sh".to_string(), "-c".to_string(), self.script(args)]
    }

    /// The `-c` script: the prologue (when there is an exec PATH), one
    /// statement per rung, then the fall-through. Joined with `; ` exactly as
    /// craze publishes it.
    fn script(&self, args: &str) -> String {
        let mut steps = Vec::with_capacity(self.rungs.len() + self.exec_path.len() + 4);
        let exec = if self.exec_path.is_empty() {
            format!("exec \"$p\" {args}")
        } else {
            // `unset` first: an assignment alone keeps the export attribute of
            // a `cz_path` the caller's environment already exported, and the
            // exec'd craze would inherit the computed value (the module doc).
            steps.push("unset cz_path".to_string());
            // Accumulate with a leading `:` per entry and strip the first one
            // at the end, so neither a skipped first entry nor an empty
            // original PATH can leave an empty entry behind — an empty PATH
            // entry means the current directory.
            steps.push("cz_path=".to_string());
            for dir in self.exec_path {
                steps.push(dir.guarded(&format!("cz_path=\"$cz_path:{}\"", dir.word())));
            }
            steps.push("if [ -n \"${PATH:-}\" ]; then cz_path=\"$cz_path:$PATH\"; fi".to_string());
            steps.push("cz_path=\"${cz_path#:}\"".to_string());
            format!("PATH=\"$cz_path\" exec \"$p\" {args}")
        };
        for rung in self.rungs {
            steps.push(rung.step(&exec));
        }
        steps.push(format!(
            "printf '%s\\n' '{BIN_NAME}: command not found' >&2; exit 127"
        ));
        steps.join("; ")
    }
}

impl Dir<'_> {
    /// The directory as it appears inside double quotes.
    fn word(&self) -> String {
        match *self {
            Dir::Home(suffix) => {
                assert_plain(suffix);
                format!("$HOME{suffix}")
            }
            Dir::PerUser { prefix, suffix } => {
                assert_plain(prefix);
                assert_plain(suffix);
                format!("{prefix}$USER{suffix}")
            }
            Dir::Fixed(dir) => {
                assert_plain(dir);
                dir.to_string()
            }
        }
    }

    /// `body` under this directory's guard — craze's, verbatim.
    fn guarded(&self, body: &str) -> String {
        match self {
            Dir::Home(_) => format!("case \"${{HOME:-}}\" in /*) {body};; esac"),
            Dir::PerUser { .. } => format!("if [ -n \"${{USER:-}}\" ]; then {body}; fi"),
            Dir::Fixed(_) => body.to_string(),
        }
    }
}

impl Rung<'_> {
    /// This rung as one complete statement, `exec` being the action its
    /// `[ -f ] && [ -x ]` gate guards.
    fn step(&self, exec: &str) -> String {
        const GATE: &str = "[ -f \"$p\" ] && [ -x \"$p\" ] && ";
        match self {
            Rung::At(dir) => dir.guarded(&format!(
                "p=\"{}/{BIN_NAME}\"; {GATE}{exec}",
                dir.word()
            )),
            Rung::PathLookup => format!(
                "p=$(command -v {BIN_NAME} 2>/dev/null) || p=; case \"$p\" in /*) {GATE}{exec};; esac"
            ),
        }
    }
}

/// Refuse a table string that is not plain path text starting with `/` (see
/// [`Dir`]) — a `Home` or `PerUser` suffix included, which follows `$HOME` or
/// `$USER` directly.
fn assert_plain(s: &str) {
    assert!(
        s.starts_with('/')
            && s.bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"/._+@-".contains(&b)),
        "a craze ladder table entry must be plain absolute path text, got {s:?}"
    );
}

/// `["sh", "-c", <ladder>]` running `craze bridge --hub` — every connection to
/// a machine's hub, local or remote. Always starts a hub if none runs.
pub fn bridge_hub_argv() -> Vec<String> {
    LADDER.bridge_hub_argv()
}

/// [`bridge_hub_argv`] as the ONE command string an ssh transport sends:
/// [`display_line`], the same composer every machine transport shares.
pub fn bridge_hub_command() -> String {
    display_line(&bridge_hub_argv())
}

/// `["sh", "-c", <ladder>]` running `craze providers --hub --json` — the
/// find-only probe: answers from a running hub, or `craze providers: no hub is
/// running` and exit 1 without starting one.
pub fn providers_hub_argv() -> Vec<String> {
    LADDER.providers_hub_argv()
}

/// `["sh", "-c", <ladder>]` running `craze attach --session '<host_id>'` — Open
/// in terminal. See [`Ladder::attach_argv`] for the id rule.
pub fn attach_argv(host_id: &str) -> Result<Vec<String>, BadHostId> {
    LADDER.attach_argv(host_id)
}

/// [`bridge_hub_argv`] on the [`JAILED_LADDER`] — test mode only.
pub fn bridge_hub_argv_jailed() -> Vec<String> {
    JAILED_LADDER.bridge_hub_argv()
}

/// [`providers_hub_argv`] on the [`JAILED_LADDER`] — test mode only.
pub fn providers_hub_argv_jailed() -> Vec<String> {
    JAILED_LADDER.providers_hub_argv()
}

/// [`attach_argv`] on the [`JAILED_LADDER`] — test mode only.
pub fn attach_argv_jailed(host_id: &str) -> Result<Vec<String>, BadHostId> {
    JAILED_LADDER.attach_argv(host_id)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// craze's published ladder (`docs/reference/protocol.md`, "SSH exec",
    /// the plain-`bridge` form, at craze `a3aa101`), as the RAW `-c` script —
    /// the published `sh -c '…'` line with its `'"'"'` spelled back to `'`.
    /// Copied, not composed: it is what "verbatim" is measured against.
    const PUBLISHED_PLAIN_BRIDGE: &str = r#"case "${HOME:-}" in /*) p="$HOME/.local/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge;; esac; p=$(command -v craze 2>/dev/null) || p=; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge;; esac; p="/opt/homebrew/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; p="/usr/local/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; p="/home/linuxbrew/.linuxbrew/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; p="/usr/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; case "${HOME:-}" in /*) p="$HOME/.nix-profile/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge;; esac; if [ -n "${USER:-}" ]; then p="/etc/profiles/per-user/$USER/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; fi; p="/run/current-system/sw/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge; printf '%s\n' 'craze: command not found' >&2; exit 127"#;

    /// The one addition: the exec-PATH prologue, in the ladder's order.
    const PROLOGUE: &str = r#"unset cz_path; cz_path=; case "${HOME:-}" in /*) cz_path="$cz_path:$HOME/.local/bin";; esac; cz_path="$cz_path:/opt/homebrew/bin"; cz_path="$cz_path:/usr/local/bin"; cz_path="$cz_path:/home/linuxbrew/.linuxbrew/bin"; cz_path="$cz_path:/usr/bin"; case "${HOME:-}" in /*) cz_path="$cz_path:$HOME/.nix-profile/bin";; esac; if [ -n "${USER:-}" ]; then cz_path="$cz_path:/etc/profiles/per-user/$USER/bin"; fi; cz_path="$cz_path:/run/current-system/sw/bin"; if [ -n "${PATH:-}" ]; then cz_path="$cz_path:$PATH"; fi; cz_path="${cz_path#:}"; "#;

    /// **Verbatim, measured**: strip the prologue and the `PATH="$cz_path" `
    /// before each `exec`, and what is left is craze's published ladder,
    /// byte for byte — for each form's arguments.
    #[test]
    fn the_production_script_is_crazes_published_ladder_plus_the_exec_path() {
        for (args, script) in [
            ("bridge --hub", &bridge_hub_argv()[2]),
            ("providers --hub --json", &providers_hub_argv()[2]),
            (
                "attach --session '0123456789ab'",
                &attach_argv("0123456789ab").unwrap()[2],
            ),
        ] {
            let rest = script.strip_prefix(PROLOGUE).unwrap_or_else(|| {
                panic!("{args}: the script must open with the prologue: {script}")
            });
            let published = PUBLISHED_PLAIN_BRIDGE
                .replace("exec \"$p\" bridge", &format!("exec \"$p\" {args}"));
            assert_eq!(
                rest.replace("PATH=\"$cz_path\" exec \"$p\"", "exec \"$p\""),
                published,
                "{args}: the ladder after the prologue is not craze's published one"
            );
            // …and every rung's exec carries the PATH, none resolves with it.
            assert_eq!(
                rest.matches("PATH=\"$cz_path\" exec \"$p\"").count(),
                RUNGS.len()
            );
            assert_eq!(script.matches("exec \"$p\"").count(), RUNGS.len());
        }
    }

    /// The exec PATH is the ladder's own directory list, in its own order.
    #[test]
    fn the_exec_path_is_the_ladders_directories_in_order() {
        let ladder_dirs: Vec<Dir<'_>> = RUNGS
            .iter()
            .filter_map(|rung| match rung {
                Rung::At(dir) => Some(*dir),
                Rung::PathLookup => None,
            })
            .collect();
        assert_eq!(ladder_dirs, EXEC_PATH);
    }

    /// **The jailed unit golden**: rungs 1 and 2 of the published ladder,
    /// with no prologue and a plain `exec` — `PATH` passes through untouched.
    #[test]
    fn the_jailed_bridge_is_rungs_one_and_two_with_no_exec_path() {
        assert_eq!(JAILED_LADDER.rungs, &RUNGS[..2]);
        assert!(JAILED_LADDER.exec_path.is_empty());
        assert_eq!(
            bridge_hub_argv_jailed(),
            [
                "sh",
                "-c",
                r#"case "${HOME:-}" in /*) p="$HOME/.local/bin/craze"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --hub;; esac; p=$(command -v craze 2>/dev/null) || p=; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] && exec "$p" bridge --hub;; esac; printf '%s\n' 'craze: command not found' >&2; exit 127"#,
            ]
        );
        for argv in [
            bridge_hub_argv_jailed(),
            providers_hub_argv_jailed(),
            attach_argv_jailed("0123456789ab").unwrap(),
        ] {
            let script = &argv[2];
            assert!(
                !script.contains("cz_path") && !script.contains("PATH="),
                "{script}"
            );
            // No absolute rung, under any form.
            for rung in &RUNGS[2..] {
                if let Rung::At(dir) = rung {
                    assert!(!script.contains(&dir.word()), "{script}");
                }
            }
        }
    }

    #[test]
    fn bridge_hub_command_is_the_display_line_of_the_argv() {
        assert_eq!(bridge_hub_command(), display_line(&bridge_hub_argv()));
        assert!(bridge_hub_command().starts_with("'sh' '-c' 'unset cz_path; cz_path=; "));
    }

    /// The raw argv carries the id in ONE ordinary pair of single quotes; an
    /// escape sequence of any kind belongs to a displayed line only.
    #[test]
    fn attach_argv_quotes_the_id_once_in_the_raw_element() {
        let id = "0123456789ab";
        for argv in [attach_argv(id).unwrap(), attach_argv_jailed(id).unwrap()] {
            let raw = &argv[2];
            assert!(raw.contains("attach --session '0123456789ab'"), "{raw}");
            assert!(
                !raw.contains(r#"'"'"'"#),
                "a display escape leaked into the raw argv: {raw}"
            );
            assert!(
                !raw.contains(r"'\''"),
                "a display escape leaked into the raw argv: {raw}"
            );
            // The displayed line is the house quoter's rendering of the raw one.
            let line = display_line(&argv);
            assert!(
                line.contains(r"attach --session '\''0123456789ab'\''"),
                "{line}"
            );
        }
    }

    #[test]
    fn attach_argv_refuses_anything_but_twelve_lowercase_hex_digits() {
        assert!(attach_argv("0a1b2c3d4e5f").is_ok());
        for bad in [
            "",
            "0123456789a",
            "0123456789abc",
            "0123456789AB",
            "0123456789ag",
            "0123456789a'",
            "01234567 9ab",
            "0123456789a\n",
            "012345678é",
            "$(id)0123456",
        ] {
            assert_eq!(attach_argv(bad), Err(BadHostId(bad.to_string())), "{bad:?}");
            assert_eq!(
                attach_argv_jailed(bad),
                Err(BadHostId(bad.to_string())),
                "{bad:?}"
            );
        }
    }

    #[test]
    #[should_panic(expected = "plain absolute path text")]
    fn a_table_entry_with_shell_syntax_is_refused() {
        let rungs = [Rung::At(Dir::Fixed("/tmp/$(id)"))];
        Ladder {
            rungs: &rungs,
            exec_path: &[],
        }
        .bridge_hub_argv();
    }

    #[test]
    #[should_panic(expected = "plain absolute path text")]
    fn a_relative_table_entry_is_refused() {
        let exec_path = [Dir::Fixed("opt/homebrew/bin")];
        Ladder {
            rungs: &RUNGS,
            exec_path: &exec_path,
        }
        .bridge_hub_argv();
    }
}
