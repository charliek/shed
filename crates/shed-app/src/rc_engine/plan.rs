//! Plan-file delivery — a port of `internal/ext/rc/plan.go`.
//!
//! A plan run ships a markdown document to the machine the agent runs on, writes
//! it to a per-kind HOME-rooted path, and delivers a kickoff line telling the
//! agent to read and implement it. The file itself is a **raw-bytes parity
//! surface** (plan 009 §3.5): a mixed Go/Rust fleet writes it into the same
//! directory, so the bytes and the 0600 mode must match exactly.

use std::fs;
use std::io::Write;
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};

use shed_core::rc::RcKind;
use shed_core::rc_agents::has_control_chars;

use super::ops::{EngineError, GetEnv};
use super::text::{has_unsafe_prompt_chars, normalize_newlines, quote_go};

/// The cap on a plan accepted by `create --plan-stdin` (`PlanMaxBytes`,
/// `plan.go:12`). 1 MiB is generous for a markdown plan and guards a runaway
/// paste from filling the guest disk.
pub const PLAN_MAX_BYTES: usize = 1 << 20;

/// On-disk permission of a written plan, owner read/write only (`planFileMode`,
/// `plan.go:16`) — a plan can carry design details the author would not want
/// world-readable.
const PLAN_FILE_MODE: u32 = 0o600;

/// Permission of a created plan directory, owner rwx only (`planDirMode`,
/// `plan.go:19`).
const PLAN_DIR_MODE: u32 = 0o700;

/// The directory a kind's plan file is written to (`planDir`, `plan.go:32`).
///
/// claude kinds use claude's native plans dir
/// (`${CLAUDE_CONFIG_DIR:-$HOME/.claude}/plans`) so the running `claude` finds
/// the plan where it looks for saved plans; every other kind uses
/// `$HOME/.shed-plans`. Both are HOME-rooted on purpose — a workdir file would
/// dirty a `--repo` clone and, with `--local-dir`, write through VirtioFS onto a
/// real host directory.
///
/// **`CLAUDE_CONFIG_DIR` is environment data, not trusted input**: a relative
/// value would yield a cwd-dependent (non-absolute) plan path and a control-char
/// value would inject into the composed kickoff line, so either is IGNORED in
/// favor of the `$HOME/.claude` default.
fn plan_dir(kind: &RcKind, env: GetEnv) -> Result<PathBuf, EngineError> {
    let home = env("HOME");
    if kind.runs_claude() {
        let mut base = env("CLAUDE_CONFIG_DIR");
        if !base.is_empty() && (!Path::new(&base).is_absolute() || has_control_chars(&base)) {
            base = String::new();
        }
        if base.is_empty() {
            if home.is_empty() {
                return Err(EngineError::bad_args(
                    "no CLAUDE_CONFIG_DIR or HOME to place the plan",
                ));
            }
            base = Path::new(&home)
                .join(".claude")
                .to_string_lossy()
                .into_owned();
        }
        return Ok(Path::new(&base).join("plans"));
    }
    if home.is_empty() {
        return Err(EngineError::bad_args("HOME unset; cannot place the plan"));
    }
    Ok(Path::new(&home).join(".shed-plans"))
}

/// The absolute path a slug's plan file is written to for a kind (`PlanPath`,
/// `plan.go:57`).
///
/// The final path is asserted absolute AND control-char-free: it is embedded
/// verbatim in the delivered kickoff line, so a hostile `$HOME` must not be able
/// to smuggle bytes into the prompt.
pub fn plan_path(kind: &RcKind, slug: &str, env: GetEnv) -> Result<String, EngineError> {
    let dir = plan_dir(kind, env)?;
    let path = dir.join(format!("plan-{slug}.md"));
    // Go builds this with filepath.Join, which CLEANS lexically (`//`→`/`,
    // `.`/`..` resolved). Path::join does not, and the string is delivered
    // verbatim inside the kickoff line — so a HOME like `/home//shed` must not
    // produce a different on-the-wire path than Go's.
    let path = clean_path(&path.to_string_lossy());
    if !Path::new(&path).is_absolute() {
        return Err(EngineError::bad_args(format!(
            "plan path {} is not absolute (bad HOME?)",
            quote_go(&path)
        )));
    }
    if has_control_chars(&path) {
        return Err(EngineError::bad_args(
            "plan path contains a control character (bad HOME/CLAUDE_CONFIG_DIR?)",
        ));
    }
    Ok(path)
}

/// Write `content` to the per-kind plan file and return its absolute path
/// (`writePlan`, `plan.go:76`). The directory is created if missing.
///
/// The 0600 mode is enforced with an **explicit `set_permissions` after the
/// open**, not just `OpenOptions::mode`: like Go's `os.OpenFile` perm, that mode
/// applies only when the file is CREATED, so a pre-existing looser copy (an
/// earlier 0644 file) would otherwise keep its old permissions.
pub(crate) fn write_plan(
    kind: &RcKind,
    slug: &str,
    content: &str,
    env: GetEnv,
) -> Result<String, EngineError> {
    let path = plan_path(kind, slug, env)?;
    if let Some(parent) = Path::new(&path).parent() {
        fs::DirBuilder::new()
            .recursive(true)
            .mode(PLAN_DIR_MODE)
            .create(parent)
            .map_err(|e| EngineError::Other(format!("creating plan dir: {e}")))?;
    }
    let mut file = fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(PLAN_FILE_MODE)
        .open(&path)
        .map_err(|e| EngineError::Other(format!("writing plan: {e}")))?;
    // Tightened BEFORE the content lands, exactly like Go's `f.Chmod` → `f.Write`
    // ordering — a rewrite of a previously world-readable plan never exposes the
    // new bytes at the old mode.
    file.set_permissions(fs::Permissions::from_mode(PLAN_FILE_MODE))
        .map_err(|e| EngineError::Other(format!("setting plan mode: {e}")))?;
    file.write_all(content.as_bytes())
        .map_err(|e| EngineError::Other(format!("writing plan: {e}")))?;
    // Go checks f.Close() and maps its error to "writing plan:" — Rust's drop
    // swallows close errors, so flush explicitly: on filesystems that defer
    // write errors to close/fsync, returning Ok here and then delivering a
    // kickoff pointing at a truncated plan would be strictly worse than the
    // extra fsync.
    file.sync_all()
        .map_err(|e| EngineError::Other(format!("writing plan: {e}")))?;
    Ok(path)
}

/// Lexical path cleaning with `filepath.Clean`'s semantics for the paths this
/// module composes (absolute or relative, `/`-separated): collapse repeated
/// slashes, resolve `.` and `..` textually, preserve a leading `/`, return "."
/// for an emptied relative path.
fn clean_path(p: &str) -> String {
    let absolute = p.starts_with('/');
    let mut parts: Vec<&str> = Vec::new();
    for seg in p.split('/') {
        match seg {
            "" | "." => {}
            ".." => {
                if parts.last().is_some_and(|s| *s != "..") {
                    parts.pop();
                } else if !absolute {
                    parts.push("..");
                }
            }
            s => parts.push(s),
        }
    }
    let joined = parts.join("/");
    match (absolute, joined.is_empty()) {
        (true, _) => format!("/{joined}"),
        (false, true) => ".".to_string(),
        (false, false) => joined,
    }
}

/// The kickoff line typed into a ready session for a plan run
/// (`composePlanKickoff`, `plan.go:105`): a natural-language instruction to read
/// and implement the plan at `path`, optionally led by the caller's framing
/// (framing leads, the plan stays referenced).
///
/// **The sentence is copied verbatim from Go** — it is delivered into a live
/// agent and the parity harness compares the delivered bytes. Do not reword it.
pub fn compose_plan_kickoff(path: &str, framing: &str) -> String {
    let base = format!(
        "Read the plan at {path} and implement it. Work through it to completion \
         autonomously — don't stop to ask for confirmation; make reasonable decisions and \
         keep going until the plan is done."
    );
    if framing.is_empty() {
        base
    } else {
        format!("{framing}\n\n{base}")
    }
}

/// Decode plan bytes read from stdin into the engine's `String` plan
/// (`readPlanStdin`'s UTF-8 gate, `clirc.go:338`, which `validatePlanInputs`
/// then re-checks at `plan.go:133`).
///
/// Rust's `String` is UTF-8 by construction, so the invalid-UTF-8 rejection that
/// Go performs INSIDE `validatePlanInputs` has to happen at the boundary instead
/// — here. The size cap is left to [`validate_plan_inputs`] so an oversize plan
/// reports the same message on both paths.
pub fn plan_from_bytes(raw: &[u8]) -> Result<String, EngineError> {
    String::from_utf8(raw.to_vec()).map_err(|_| EngineError::bad_args("plan is not valid UTF-8"))
}

/// Check a plan-delivery request BEFORE any side effect (`validatePlanInputs`,
/// `plan.go:120`) and return the normalized framing to prepend to the composed
/// kickoff.
///
/// The kind must accept a typed kickoff, the plan must be non-empty and within
/// [`PLAN_MAX_BYTES`], a raw prompt and a plan are mutually exclusive (a plan
/// composes its own kickoff), and any framing must normalize to control-char-free
/// text.
pub(crate) fn validate_plan_inputs(
    kind: &RcKind,
    plan: &str,
    prompt: &str,
    framing: &str,
) -> Result<String, EngineError> {
    if !prompt.is_empty() {
        return Err(EngineError::bad_args(
            "a plan and a prompt are mutually exclusive",
        ));
    }
    if !kind.accepts_typed_input() {
        return Err(EngineError::bad_args(format!(
            "kind {} does not accept a plan",
            quote_go(kind.as_str())
        )));
    }
    if plan.is_empty() {
        return Err(EngineError::bad_args("plan is empty"));
    }
    if plan.len() > PLAN_MAX_BYTES {
        return Err(EngineError::bad_args(format!(
            "plan is {} bytes; the limit is {PLAN_MAX_BYTES}",
            plan.len()
        )));
    }
    // Go additionally re-checks `utf8.ValidString(plan)` here; see
    // [`plan_from_bytes`] for why that gate lives at the byte boundary in Rust.
    if framing.is_empty() {
        return Ok(String::new());
    }
    let framing = normalize_newlines(framing);
    if has_unsafe_prompt_chars(&framing) {
        return Err(EngineError::bad_args(
            "plan framing contains an unsupported control character",
        ));
    }
    Ok(framing)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::rc_engine::fake::{env_from, home_env};

    // ---- paths (mirrors Go TestPlanPathPerKind, plan_test.go:19) ----

    /// `(case name, kind, env table, expected path)`.
    type PathCase<'a> = (&'a str, RcKind, &'a [(&'a str, &'a str)], &'a str);

    #[test]
    fn clean_path_matches_filepath_clean() {
        for (input, want) in [
            (
                "/home//shed/.claude/plans/plan-a.md",
                "/home/shed/.claude/plans/plan-a.md",
            ),
            ("/a/../b/plan.md", "/b/plan.md"),
            ("/a/./b", "/a/b"),
            ("/", "/"),
            ("/../x", "/x"), // filepath.Clean: leading .. off root is dropped
            ("a//b", "a/b"),
            ("./", "."),
        ] {
            assert_eq!(clean_path(input), want, "input {input:?}");
        }
    }

    #[test]
    fn plan_path_per_kind() {
        let home = "/home/shed";
        let cases: &[PathCase] = &[
            (
                "claude-rc default",
                RcKind::ClaudeRc,
                &[("HOME", home)],
                "/home/shed/.claude/plans/plan-abc123.md",
            ),
            (
                "claude-broker default",
                RcKind::ClaudeBroker,
                &[("HOME", home)],
                "/home/shed/.claude/plans/plan-abc123.md",
            ),
            (
                "claude honors CLAUDE_CONFIG_DIR",
                RcKind::ClaudeRc,
                &[("HOME", home), ("CLAUDE_CONFIG_DIR", "/cfg")],
                "/cfg/plans/plan-abc123.md",
            ),
            (
                "codex home-rooted",
                RcKind::Codex,
                &[("HOME", home)],
                "/home/shed/.shed-plans/plan-abc123.md",
            ),
            (
                "opencode home-rooted",
                RcKind::Opencode,
                &[("HOME", home)],
                "/home/shed/.shed-plans/plan-abc123.md",
            ),
            (
                "cursor home-rooted",
                RcKind::Cursor,
                &[("HOME", home)],
                "/home/shed/.shed-plans/plan-abc123.md",
            ),
            (
                "shell home-rooted",
                RcKind::Shell,
                &[("HOME", home)],
                "/home/shed/.shed-plans/plan-abc123.md",
            ),
        ];
        for (name, kind, env, want) in cases {
            let env = env_from(env);
            assert_eq!(
                plan_path(kind, "abc123", &env).unwrap(),
                *want,
                "case {name}"
            );
        }
    }

    #[test]
    fn plan_path_requires_home() {
        let empty = env_from(&[]);
        for kind in [RcKind::Codex, RcKind::ClaudeRc] {
            let err = plan_path(&kind, "abc123", &empty).unwrap_err();
            assert_eq!(err.exit_code(), 2, "{err}");
        }
    }

    // Mirrors Go TestPlanDirRejectsUntrustedClaudeConfigDir (plan_test.go:322).
    #[test]
    fn untrusted_claude_config_dir_falls_back() {
        let fallback = "/home/shed/.claude/plans/plan-abc123.md";
        for bad in ["relative/dir", "/cfg\u{1b}]0;pwn"] {
            let env = env_from(&[("HOME", "/home/shed"), ("CLAUDE_CONFIG_DIR", bad)]);
            assert_eq!(
                plan_path(&RcKind::ClaudeRc, "abc123", &env).unwrap(),
                fallback,
                "CLAUDE_CONFIG_DIR {bad:?} must be ignored"
            );
        }
    }

    #[test]
    fn hostile_home_is_rejected() {
        for home in ["/home/\nshed", "home/shed"] {
            let env = home_env(home);
            let err = plan_path(&RcKind::Codex, "abc123", &env).unwrap_err();
            assert_eq!(err.exit_code(), 2, "HOME {home:?}: {err}");
        }
    }

    // ---- write (mirrors Go TestWritePlanEnforcesModeOnExistingFile, plan_test.go:198) ----

    #[test]
    fn write_plan_tightens_a_preexisting_loose_file() {
        let home = tempfile::tempdir().unwrap();
        let dir = home.path().join(".shed-plans");
        fs::create_dir_all(&dir).unwrap();
        let path = dir.join("plan-abc123.md");
        fs::write(&path, "old").unwrap();
        fs::set_permissions(&path, fs::Permissions::from_mode(0o644)).unwrap();

        let env = home_env(home.path().to_str().unwrap());
        let got = write_plan(&RcKind::Shell, "abc123", "new plan", &env).unwrap();

        assert_eq!(got, path.to_string_lossy());
        assert_eq!(fs::read_to_string(&path).unwrap(), "new plan");
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600,
            "a pre-existing 0644 plan must be tightened by the explicit chmod"
        );
    }

    #[test]
    fn write_plan_creates_the_dir_0700() {
        let home = tempfile::tempdir().unwrap();
        let env = home_env(home.path().to_str().unwrap());
        write_plan(&RcKind::Codex, "abc123", "body", &env).unwrap();
        let dir = home.path().join(".shed-plans");
        assert_eq!(
            fs::metadata(&dir).unwrap().permissions().mode() & 0o777,
            0o700
        );
    }

    // ---- validation table (mirrors Go TestCreatePlanValidation, plan_test.go:134) ----

    /// `(case name, kind, plan, prompt, framing, expected message fragment)`.
    type ValidationCase<'a> = (&'a str, RcKind, &'a str, &'a str, &'a str, &'a str);

    #[test]
    fn plan_validation_table() {
        let big = "a".repeat(PLAN_MAX_BYTES + 1);
        let cases: &[ValidationCase] = &[
            (
                "broker rejects a plan",
                RcKind::ClaudeBroker,
                "x",
                "",
                "",
                "does not accept a plan",
            ),
            (
                "unknown kind rejects a plan",
                RcKind::Other("future".to_string()),
                "x",
                "",
                "",
                "does not accept a plan",
            ),
            (
                "plan and prompt are mutually exclusive",
                RcKind::ClaudeRc,
                "x",
                "y",
                "",
                "mutually exclusive",
            ),
            ("empty plan", RcKind::ClaudeRc, "", "", "", "plan is empty"),
            (
                "oversize plan",
                RcKind::ClaudeRc,
                &big,
                "",
                "",
                "the limit is 1048576",
            ),
            (
                "framing with a control char",
                RcKind::ClaudeRc,
                "x",
                "",
                "a\u{1b}b",
                "unsupported control character",
            ),
        ];
        for (name, kind, plan, prompt, framing, want) in cases {
            let err = validate_plan_inputs(kind, plan, prompt, framing).unwrap_err();
            assert_eq!(err.exit_code(), 2, "case {name}");
            assert!(
                err.to_string().contains(want),
                "case {name}: {err} should mention {want:?}"
            );
        }
    }

    #[test]
    fn plan_validation_accepts_and_normalizes_framing() {
        let framing =
            validate_plan_inputs(&RcKind::ClaudeRc, "plan body", "", "lead\r\nline").unwrap();
        assert_eq!(
            framing, "lead\nline",
            "CRLF framing is normalized, not rejected"
        );
        // A plan exactly at the cap is accepted (the check is `>`, not `>=`).
        let at_cap = "a".repeat(PLAN_MAX_BYTES);
        assert!(validate_plan_inputs(&RcKind::Shell, &at_cap, "", "").is_ok());
    }

    #[test]
    fn plan_from_bytes_rejects_invalid_utf8() {
        let err = plan_from_bytes(b"a\xffb").unwrap_err();
        assert_eq!(err.exit_code(), 2);
        assert!(err.to_string().contains("not valid UTF-8"), "{err}");
        assert_eq!(plan_from_bytes("héllo".as_bytes()).unwrap(), "héllo");
    }

    // ---- kickoff (mirrors Go TestComposePlanKickoff, plan_test.go:186) ----

    #[test]
    fn compose_plan_kickoff_verbatim() {
        assert_eq!(
            compose_plan_kickoff("/p/plan.md", ""),
            "Read the plan at /p/plan.md and implement it. Work through it to completion \
             autonomously — don't stop to ask for confirmation; make reasonable decisions and \
             keep going until the plan is done."
        );
        let framed = compose_plan_kickoff("/p/plan.md", "lead line");
        assert!(framed.starts_with("lead line\n\n"));
        assert!(framed.contains("/p/plan.md"));
        // No framing ⇒ a SINGLE line, so delivery uses `send-keys -l` rather than
        // the bracketed paste (Go pins the same property).
        assert!(!compose_plan_kickoff("/p/plan.md", "").contains('\n'));
    }
}
