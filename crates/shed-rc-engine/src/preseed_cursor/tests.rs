//! Mirrors `internal/ext/rc/preseed_cursor_test.go`, plus the byte-exact
//! assertions the raw-bytes parity surface needs (the Go tests re-decode; these
//! pin the document).

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

/// The `hooks.json` a preseed writes into a fresh HOME, byte for byte: our entry
/// on every wired event (arrays sorted by EVENT NAME, because Go marshals map
/// keys sorted), plus the `version` we supplied.
fn fresh_hooks_document(script: &str) -> String {
    let mut events = CURSOR_HOOK_EVENTS;
    events.sort_unstable();
    let mut out = String::from("{\n  \"hooks\": {\n");
    for (i, event) in events.iter().enumerate() {
        out.push_str(&format!(
            "    \"{event}\": [\n      {{\n        \"command\": \"'{script}' {event}\"\n      }}\n    ]"
        ));
        if i + 1 < events.len() {
            out.push(',');
        }
        out.push('\n');
    }
    out.push_str("  },\n  \"version\": 1\n}");
    out
}

// --- the hub-owned script ---------------------------------------------------

/// The script is a raw-bytes parity surface: cursor execs it, and a mixed fleet
/// rewrites it on every create. The anchors below are Go's own test's
/// (`preseed_cursor_test.go:88-106`); the harness additionally byte-compares the
/// two implementations' files.
#[test]
fn script_content_and_mode() {
    let home = tempfile::tempdir().unwrap();
    preseed_cursor_hooks("/x", &home_env(home.path())).unwrap();
    let path = cursor_script_path(home.path().to_str().unwrap());
    assert_eq!(
        fs::metadata(&path).unwrap().permissions().mode() & 0o777,
        0o755,
        "cursor execs the script directly"
    );
    let script = read(&path);
    for want in [
        "SHED_RC_SLUG",
        &format!("http://{HUB_ADDR}/v1/ingest/cursor"),
        "--max-time 2",
        "--connect-timeout 1",
        "--noproxy '*'",
        "--globoff",
        "exit 0",
        "--output /dev/null",
    ] {
        assert!(
            script.contains(want),
            "script is missing {want:?}:\n{script}"
        );
    }
    // Mute by construction: cursor reads hook stdout as a VERDICT.
    assert!(
        !script.contains("echo "),
        "the hook script must never print"
    );
    // The shape that makes it one file for ten events: `$1` is the event name.
    assert!(script.contains("event=$1"), "{script}");
    assert!(script.starts_with("#!/bin/sh\n"), "{script}");
    assert!(script.ends_with("exit 0\n"), "{script}");
    // The curl continuation lines are TAB-indented — part of the byte surface.
    assert!(script.contains("\\\n\t--connect-timeout"), "{script:?}");
}

#[test]
fn the_script_is_overwritten_every_time() {
    let home = tempfile::tempdir().unwrap();
    preseed_cursor_hooks("/x", &home_env(home.path())).unwrap();
    let path = cursor_script_path(home.path().to_str().unwrap());
    let original = read(&path);
    fs::write(&path, b"#!/bin/sh\necho stale\n").unwrap();
    preseed_cursor_hooks("/x", &home_env(home.path())).unwrap();
    assert_eq!(read(&path), original, "a stale script must not survive");
    assert_eq!(read(&path), cursor_hook_script());
}

// --- the merge --------------------------------------------------------------

#[test]
fn fresh_write_is_byte_exact() {
    let home = tempfile::tempdir().unwrap();
    let home_str = home.path().to_str().unwrap();
    preseed_cursor_hooks("/home/shed/proj", &home_env(home.path())).unwrap();
    let script = cursor_script_path(home_str);
    assert_eq!(
        read(&cursor_hooks_path(home_str)),
        fresh_hooks_document(script.to_str().unwrap())
    );
}

#[test]
fn unwired_events_are_left_alone() {
    let home = tempfile::tempdir().unwrap();
    preseed_cursor_hooks("/x", &home_env(home.path())).unwrap();
    let text = read(&cursor_hooks_path(home.path().to_str().unwrap()));
    // Deliberately NOT wired (see CURSOR_HOOK_EVENTS' doc).
    for event in ["afterAgentThought", "workspaceOpen", "beforeReadFile"] {
        assert!(!text.contains(event), "{event} must stay unwired:\n{text}");
    }
}

#[test]
fn merge_preserves_user_hooks_and_their_own_fields() {
    let home = tempfile::tempdir().unwrap();
    let home_str = home.path().to_str().unwrap();
    fs::create_dir_all(home.path().join(".cursor")).unwrap();
    fs::write(
        cursor_hooks_path(home_str),
        br#"{"version":2,
            "hooks":{"beforeSubmitPrompt":[{"command":"/usr/local/bin/audit.sh","failClosed":true}],
                     "afterAgentThought":[{"command":"/usr/local/bin/thoughts.sh"}]},
            "somethingElse":{"keep":"me"}}"#,
    )
    .unwrap();

    preseed_cursor_hooks("/x", &home_env(home.path())).unwrap();

    let text = read(&cursor_hooks_path(home_str));
    // The user's version is never clobbered, their unknown top-level key stays,
    // their entry keeps its own fields AND its position (ours is APPENDED), and
    // an event we do not wire gains nothing.
    assert!(text.contains("\"version\": 2"), "{text}");
    assert!(
        text.contains("\"somethingElse\": {\n    \"keep\": \"me\"\n  }"),
        "{text}"
    );
    assert!(text.contains("\"failClosed\": true"), "{text}");
    let submit = text
        .split("\"beforeSubmitPrompt\": [")
        .nth(1)
        .unwrap_or_default();
    let audit = submit.find("audit.sh").expect("the user's entry survives");
    let ours = submit.find("cursor-hook.sh").expect("ours is appended");
    assert!(
        audit < ours,
        "ours must be APPENDED after the user's:\n{text}"
    );
    let thought_block = text
        .split("\"afterAgentThought\": [")
        .nth(1)
        .unwrap_or_default();
    assert!(
        !thought_block[..thought_block.find(']').unwrap_or(0)].contains("cursor-hook.sh"),
        "an unwired event must not gain our entry:\n{text}"
    );
}

#[test]
fn repeated_preseeds_are_byte_identical() {
    let home = tempfile::tempdir().unwrap();
    let path = cursor_hooks_path(home.path().to_str().unwrap());
    preseed_cursor_hooks("/x", &home_env(home.path())).unwrap();
    let first = read(&path);
    for _ in 0..2 {
        preseed_cursor_hooks("/x", &home_env(home.path())).unwrap();
    }
    assert_eq!(read(&path), first, "the merge must be idempotent");
}

/// A hand-edited command that still invokes our script is left alone (matched on
/// the script PATH, not the exact string) — otherwise every create would append a
/// duplicate.
#[test]
fn a_hand_edited_invocation_still_counts_as_ours() {
    let home = tempfile::tempdir().unwrap();
    let home_str = home.path().to_str().unwrap();
    let script = cursor_script_path(home_str);
    fs::create_dir_all(home.path().join(".cursor")).unwrap();
    fs::write(
        cursor_hooks_path(home_str),
        format!(
            r#"{{"hooks":{{"stop":[{{"command":"SHED_DEBUG=1 {} stop"}}]}}}}"#,
            script.display()
        ),
    )
    .unwrap();
    preseed_cursor_hooks("/x", &home_env(home.path())).unwrap();
    let text = read(&cursor_hooks_path(home_str));
    let stop = text.split("\"stop\": [").nth(1).unwrap_or_default();
    let stop = &stop[..stop.find(']').unwrap_or(0)];
    assert_eq!(
        stop.matches("cursor-hook.sh").count(),
        1,
        "the hand-edited invocation must be kept as ours:\n{text}"
    );
}

/// A HOME containing a single quote makes `shell_quote_always` split the path
/// across the POSIX `'\''` escape, which used to defeat the raw-`contains` match
/// and append a duplicate on EVERY create (Go's regression test,
/// `preseed_cursor_test.go:208`).
#[test]
fn idempotent_with_a_quote_in_the_home_path() {
    let base = tempfile::tempdir().unwrap();
    let home = base.path().join("o'brien");
    fs::create_dir_all(&home).unwrap();
    let env = home_env(&home);
    preseed_cursor_hooks("/x", &env).unwrap();
    let first = read(&cursor_hooks_path(home.to_str().unwrap()));
    preseed_cursor_hooks("/x", &env).unwrap();
    let text = read(&cursor_hooks_path(home.to_str().unwrap()));
    assert_eq!(text, first, "a quoted path must stay idempotent:\n{text}");
    assert_eq!(
        text.matches("cursor-hook.sh").count(),
        CURSOR_HOOK_EVENTS.len(),
        "exactly one entry per wired event"
    );
}

#[test]
fn concurrent_preseeds_leave_exactly_one_entry_per_event() {
    let home = tempfile::tempdir().unwrap();
    std::thread::scope(|scope| {
        for _ in 0..5 {
            let path = home.path().to_path_buf();
            scope.spawn(move || {
                let env = env_from(&[("HOME", path.to_str().unwrap())]);
                preseed_cursor_hooks("/x", &env).unwrap();
            });
        }
    });
    let text = read(&cursor_hooks_path(home.path().to_str().unwrap()));
    assert_eq!(
        text.matches("cursor-hook.sh").count(),
        CURSOR_HOOK_EVENTS.len(),
        "concurrent preseeds must serialize through the sibling flock:\n{text}"
    );
}

// --- refusals ---------------------------------------------------------------

#[test]
fn a_malformed_hooks_json_is_left_untouched() {
    let home = tempfile::tempdir().unwrap();
    let home_str = home.path().to_str().unwrap();
    fs::create_dir_all(home.path().join(".cursor")).unwrap();
    let garbage = "{ not json at all";
    fs::write(cursor_hooks_path(home_str), garbage).unwrap();
    let err = preseed_cursor_hooks("/x", &home_env(home.path())).unwrap_err();
    assert!(
        err.to_string()
            .contains("is not valid JSON; leaving untouched"),
        "{err}"
    );
    assert_eq!(read(&cursor_hooks_path(home_str)), garbage);
}

/// A VALID-BUT-UNEXPECTED shape is treated exactly like a malformed file, and the
/// refusal NAMES the shape it found (`jsonShapeOf`) — the failed type assertion
/// this replaces discarded the value and then overwrote it, deleting real user
/// config that merely disagreed with our idea of the schema.
#[test]
fn unexpected_shapes_are_refused_by_name() {
    let cases = [
        (
            r#"{"version":1,"hooks":[{"command":"/usr/local/bin/audit.sh"}]}"#,
            "has a array `hooks` value, not an object; leaving untouched",
        ),
        (
            r#"{"version":1,"hooks":"see ~/.cursor/hooks.d"}"#,
            "has a string `hooks` value, not an object; leaving untouched",
        ),
        (
            r#"{"version":1,"hooks":{"stop":{"command":"/usr/local/bin/audit.sh"}}}"#,
            "has a object `hooks.stop` value, not an array; leaving untouched",
        ),
        (
            r#"{"version":1,"hooks":{"beforeSubmitPrompt":"/usr/local/bin/audit.sh"}}"#,
            "has a string `hooks.beforeSubmitPrompt` value, not an array; leaving untouched",
        ),
    ];
    for (body, want) in cases {
        let home = tempfile::tempdir().unwrap();
        let home_str = home.path().to_str().unwrap();
        fs::create_dir_all(home.path().join(".cursor")).unwrap();
        fs::write(cursor_hooks_path(home_str), body).unwrap();
        let err = preseed_cursor_hooks("/x", &home_env(home.path()))
            .expect_err("the preseed must decline rather than rewrite");
        assert!(err.to_string().contains(want), "{body}\n -> {err}");
        assert_eq!(
            read(&cursor_hooks_path(home_str)),
            body,
            "{body} was rewritten"
        );
    }
}

#[test]
fn no_home_is_a_reported_no_op() {
    let env = env_from(&[]);
    let err = preseed_cursor_hooks("/x", &env).unwrap_err();
    assert_eq!(err.to_string(), "no HOME; skipping cursor hook preseed");
}

// --- the mount guard --------------------------------------------------------

/// THE MOUNT GUARD: a `~/.cursor` on another device is a host auth mount, and
/// writing a hook config there would push a script path that does not exist on
/// the host into the user's real cursor setup. The config half is skipped and
/// reported; the hub-owned script is still written (inert until referenced).
#[test]
fn a_foreign_device_cursor_dir_skips_the_config_half() {
    let home = tempfile::tempdir().unwrap();
    let home_str = home.path().to_str().unwrap();
    let err =
        preseed_cursor_hooks_with("/x", &home_env(home.path()), &|_, _| Ok(false)).unwrap_err();
    assert_eq!(err, PreseedError::CursorForeignDevice);
    assert!(
        !cursor_hooks_path(home_str).exists(),
        "hooks.json must NOT be written into a foreign-device ~/.cursor"
    );
    assert!(
        cursor_script_path(home_str).exists(),
        "the hub-owned script should still be written"
    );
}

/// The real check: `$HOME` and a directory inside it are always one filesystem,
/// and a path that does not exist is an ERROR rather than a silent "same".
#[test]
fn stat_same_device_reads_st_dev() {
    let home = tempfile::tempdir().unwrap();
    let sub = home.path().join(".cursor");
    fs::create_dir_all(&sub).unwrap();
    assert!(stat_same_device(&sub, home.path()).unwrap());
    assert!(stat_same_device(&home.path().join("nope"), home.path()).is_err());
}

// --- the wired event list ---------------------------------------------------

#[test]
fn the_event_list_matches_go_by_value_and_order() {
    assert_eq!(
        CURSOR_HOOK_EVENTS,
        [
            "sessionStart",
            "beforeSubmitPrompt",
            "preToolUse",
            "afterShellExecution",
            "afterFileEdit",
            "postToolUse",
            "postToolUseFailure",
            "afterAgentResponse",
            "stop",
            "sessionEnd",
        ]
    );
}
