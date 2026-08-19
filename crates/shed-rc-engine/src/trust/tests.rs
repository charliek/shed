//! Mirrors `internal/ext/rc/trust_test.go`, plus the byte-equality set plan 009
//! §3.1 requires of the writer (the Go tests assert semantics through a re-decode;
//! this file additionally pins the exact bytes, because these files are a
//! raw-bytes parity surface).

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

use crate::fake::env_from;

use super::*;

/// A `HOME`-only env table over the `&Path` a tempdir hands back (the shared
/// [`crate::fake::home_env`] takes a `&str`).
fn home_env(home: &Path) -> impl Fn(&str) -> String {
    env_from(&[("HOME", home.to_str().expect("a UTF-8 temp path"))])
}

fn read(path: &Path) -> String {
    fs::read_to_string(path).unwrap_or_else(|e| panic!("reading {}: {e}", path.display()))
}

/// The document a preseed of `workdir` writes into an EMPTY home, byte for byte
/// (`json.MarshalIndent(config, "", "  ")` over the merged map).
fn fresh_document(workdir: &str) -> String {
    format!(
        concat!(
            "{{\n",
            "  \"fullscreenUpsellSeenCount\": 999,\n",
            "  \"hasCompletedOnboarding\": true,\n",
            "  \"hasSeenAutoModeEntryWarning\": true,\n",
            "  \"projects\": {{\n",
            "    \"{workdir}\": {{\n",
            "      \"hasTrustDialogAccepted\": true\n",
            "    }}\n",
            "  }},\n",
            "  \"theme\": \"dark\"\n",
            "}}"
        ),
        workdir = workdir
    )
}

// --- the literals other implementations depend on ---------------------------

/// `lockSibling` (`trust.go:122`) and the `os.CreateTemp` patterns
/// (`trust.go:112`, `preseed_cursor.go:163,283`) are CROSS-IMPLEMENTATION
/// literals: a Rust engine that spelled the lock file differently would not
/// exclude a concurrent Go create at all, and the file would be merged twice.
#[test]
fn cross_impl_literals_match_the_go_strings() {
    assert_eq!(LOCK_SUFFIX, ".shed-ext-rc.lock");
    assert_eq!(CLAUDE_TMP_PATTERN, ".claude.json.*.tmp");
    assert_eq!(
        crate::preseed_cursor::HOOKS_TMP_PATTERN,
        ".hooks.json.*.tmp"
    );
    assert_eq!(
        crate::preseed_cursor::SCRIPT_TMP_PATTERN,
        ".cursor-hook.sh.*.tmp"
    );
}

#[test]
fn lock_file_sits_beside_the_target_and_is_actually_created() {
    let home = tempfile::tempdir().unwrap();
    let target = home.path().join(".claude.json");
    {
        let _guard = lock_sibling(&target).unwrap();
        assert!(
            home.path().join(".claude.json.shed-ext-rc.lock").exists(),
            "the lock must be a SIBLING file, never the target itself"
        );
        assert!(!target.exists(), "locking must not create the target");
    }
    // Dropping the guard released it — a second acquisition returns immediately.
    let _again = lock_sibling(&target).unwrap();
}

// --- claudeConfigPath (trust.go:18) -----------------------------------------

#[test]
fn claude_config_path_prefers_claude_config_dir() {
    let check = |pairs: &[(&str, &str)], want: Option<&str>| {
        let env = env_from(pairs);
        let got = claude_config_path(&env);
        assert_eq!(got.as_deref().and_then(Path::to_str), want, "env {pairs:?}");
    };
    check(
        &[("CLAUDE_CONFIG_DIR", "/cfg"), ("HOME", "/home/x")],
        Some("/cfg/.claude.json"),
    );
    // An EMPTY CLAUDE_CONFIG_DIR is treated as unset (matching claude).
    check(
        &[("CLAUDE_CONFIG_DIR", ""), ("HOME", "/home/x")],
        Some("/home/x/.claude.json"),
    );
    check(&[("HOME", "/home/x")], Some("/home/x/.claude.json"));
    check(&[], None);
}

#[test]
fn claude_config_dir_is_created_when_missing() {
    let home = tempfile::tempdir().unwrap();
    let cfg = home.path().join("deep/config/dir");
    let env = env_from(&[("CLAUDE_CONFIG_DIR", cfg.to_str().unwrap())]);
    preseed_claude_config("/p", &env).unwrap();
    assert!(cfg.join(".claude.json").exists());
}

// --- the merge --------------------------------------------------------------

#[test]
fn fresh_config_is_byte_exact_and_0600() {
    let home = tempfile::tempdir().unwrap();
    preseed_claude_config("/home/shed/proj", &home_env(home.path())).unwrap();
    let path = home.path().join(".claude.json");
    assert_eq!(read(&path), fresh_document("/home/shed/proj"));
    assert_eq!(
        fs::metadata(&path).unwrap().permissions().mode() & 0o777,
        0o600
    );
}

#[test]
fn an_empty_file_is_treated_as_an_empty_object() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    fs::write(&path, b"").unwrap();
    preseed_claude_config("/p", &home_env(home.path())).unwrap();
    assert_eq!(read(&path), fresh_document("/p"));
}

#[test]
fn a_null_document_is_reseeded() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    fs::write(&path, b"null").unwrap();
    preseed_claude_config("/home/shed/proj", &home_env(home.path())).unwrap();
    assert_eq!(read(&path), fresh_document("/home/shed/proj"));
}

#[test]
fn an_existing_theme_and_upsell_count_are_never_clobbered() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    fs::write(
        &path,
        br#"{"theme":"light","fullscreenUpsellSeenCount":100000,"hasSeenAutoModeEntryWarning":false}"#,
    )
    .unwrap();
    preseed_claude_config("/p", &home_env(home.path())).unwrap();
    assert_eq!(
        read(&path),
        concat!(
            "{\n",
            "  \"fullscreenUpsellSeenCount\": 100000,\n",
            "  \"hasCompletedOnboarding\": true,\n",
            "  \"hasSeenAutoModeEntryWarning\": false,\n",
            "  \"projects\": {\n",
            "    \"/p\": {\n",
            "      \"hasTrustDialogAccepted\": true\n",
            "    }\n",
            "  },\n",
            "  \"theme\": \"light\"\n",
            "}"
        )
    );
}

#[test]
fn a_lower_upsell_count_is_raised_to_the_floor() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    fs::write(&path, br#"{"fullscreenUpsellSeenCount":3}"#).unwrap();
    preseed_claude_config("/p", &home_env(home.path())).unwrap();
    assert!(read(&path).contains("\"fullscreenUpsellSeenCount\": 999,"));
}

#[test]
fn unrelated_keys_and_other_projects_survive_verbatim() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    fs::write(
        &path,
        br#"{"oauthAccount":{"emailAddress":"x@y.z"},"mcpServers":{"foo":{"command":"bar"}},
            "projects":{"/other":{"hasTrustDialogAccepted":true,"customKey":7.0}}}"#,
    )
    .unwrap();
    preseed_claude_config("/home/shed/proj", &home_env(home.path())).unwrap();
    assert_eq!(
        read(&path),
        concat!(
            "{\n",
            "  \"fullscreenUpsellSeenCount\": 999,\n",
            "  \"hasCompletedOnboarding\": true,\n",
            "  \"hasSeenAutoModeEntryWarning\": true,\n",
            "  \"mcpServers\": {\n",
            "    \"foo\": {\n",
            "      \"command\": \"bar\"\n",
            "    }\n",
            "  },\n",
            "  \"oauthAccount\": {\n",
            "    \"emailAddress\": \"x@y.z\"\n",
            "  },\n",
            "  \"projects\": {\n",
            "    \"/home/shed/proj\": {\n",
            "      \"hasTrustDialogAccepted\": true\n",
            "    },\n",
            "    \"/other\": {\n",
            "      \"customKey\": 7.0,\n",
            "      \"hasTrustDialogAccepted\": true\n",
            "    }\n",
            "  },\n",
            "  \"theme\": \"dark\"\n",
            "}"
        )
    );
}

/// The §3.1 number-fidelity set: every one of these is corrupted by a decode
/// through `f64` (or through an integer), and all of them can appear in a real
/// `.claude.json` (timeouts, ids, telemetry counters).
#[test]
fn number_fidelity_survives_the_merge() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    fs::write(
        &path,
        br#"{"a":9007199254740993,"b":18446744073709551615,"c":1e10,"d":0.10,"e":-0,"f":[1.0,2.50]}"#,
    )
    .unwrap();
    preseed_claude_config("/p", &home_env(home.path())).unwrap();
    let text = read(&path);
    for literal in [
        "\"a\": 9007199254740993,",
        "\"b\": 18446744073709551615,",
        "\"c\": 1e10,",
        "\"d\": 0.10,",
        "\"e\": -0,",
        "    1.0,",
        "    2.50\n",
    ] {
        assert!(text.contains(literal), "{literal} missing from:\n{text}");
    }
}

/// Go's encoder HTML-escapes and this one must too — a `<`/`>`/`&` or a
/// U+2028/U+2029 in a preserved string is where a naive writer diverges first.
#[test]
fn preserved_strings_are_re_escaped_the_go_way() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    fs::write(
        &path,
        "{\"note\":\"a<b>c&d\",\"js\":\"x\u{2028}y\u{2029}z\",\"uni\":\"héllo 世界\"}".as_bytes(),
    )
    .unwrap();
    preseed_claude_config("/p", &home_env(home.path())).unwrap();
    let text = read(&path);
    assert!(
        text.contains(r#""note": "a\u003cb\u003ec\u0026d""#),
        "{text}"
    );
    assert!(text.contains(r#""js": "x\u2028y\u2029z""#), "{text}");
    // Non-ASCII stays RAW (Go escapes only the four classes above).
    assert!(text.contains("\"uni\": \"héllo 世界\""), "{text}");
}

// --- refusals (the file must be left exactly as it is) ----------------------

#[test]
fn malformed_and_trailing_content_are_refused_without_touching_the_file() {
    for original in [
        "{ this is not json",
        r#"{"theme":"dark"}{"extra":true}"#,
        r#"{"theme":"dark"} garbage after"#,
        "[1,2,3]",
    ] {
        let home = tempfile::tempdir().unwrap();
        let path = home.path().join(".claude.json");
        fs::write(&path, original).unwrap();
        let err = preseed_claude_config("/p", &home_env(home.path()))
            .expect_err("the preseed must decline");
        assert!(
            err.to_string()
                .contains("is not valid JSON; leaving untouched"),
            "{original:?} -> {err}"
        );
        assert_eq!(read(&path), original, "{original:?} was modified");
        // No debris left behind either.
        let leftovers: Vec<_> = fs::read_dir(home.path())
            .unwrap()
            .filter_map(Result::ok)
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|name| name.contains(".tmp"))
            .collect();
        assert!(leftovers.is_empty(), "temp debris: {leftovers:?}");
    }
}

#[test]
fn no_home_is_a_reported_no_op() {
    let env = env_from(&[]);
    let err = preseed_claude_config("/x", &env).unwrap_err();
    assert_eq!(
        err.to_string(),
        "no CLAUDE_CONFIG_DIR or HOME; skipping trust preseed"
    );
}

// --- mode + concurrency -----------------------------------------------------

#[test]
fn an_existing_files_permission_bits_are_carried_through() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    fs::write(&path, b"{}").unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o644)).unwrap();
    preseed_claude_config("/p", &home_env(home.path())).unwrap();
    assert_eq!(
        fs::metadata(&path).unwrap().permissions().mode() & 0o777,
        0o644,
        "the rewrite must preserve the existing mode, not reset it to 0600"
    );
}

/// Concurrent creates serialize through the sibling flock: every writer's merge
/// survives and the file is always valid JSON (atomic rename, never a partial
/// write).
#[test]
fn concurrent_preseeds_all_survive() {
    let home = tempfile::tempdir().unwrap();
    let workdirs = ["/a", "/b", "/c", "/d", "/e"];
    std::thread::scope(|scope| {
        for workdir in workdirs {
            let path = home.path().to_path_buf();
            scope.spawn(move || {
                let env = env_from(&[("HOME", path.to_str().unwrap())]);
                preseed_claude_config(workdir, &env).unwrap();
            });
        }
    });
    let text = read(&home.path().join(".claude.json"));
    for workdir in workdirs {
        assert!(
            text.contains(&format!("\"{workdir}\": {{")),
            "concurrent insert lost {workdir}:\n{text}"
        );
    }
}

#[test]
fn a_second_preseed_of_the_same_workdir_is_a_no_op_byte_for_byte() {
    let home = tempfile::tempdir().unwrap();
    let path = home.path().join(".claude.json");
    preseed_claude_config("/p", &home_env(home.path())).unwrap();
    let first = read(&path);
    preseed_claude_config("/p", &home_env(home.path())).unwrap();
    assert_eq!(read(&path), first, "the merge must be idempotent");
}
