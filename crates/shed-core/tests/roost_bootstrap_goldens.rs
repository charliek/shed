//! **Every script and command a bootstrap sends, pinned** (plan 019 §3.4).
//!
//! `shed_core::roost::bootstrap` composes roost's own builders rather than
//! writing its own, which is the right call and also an invisible one: a
//! `roost-ipc` rev bump can change what those builders emit without a single
//! line of shed changing, and the first person to find out would otherwise be
//! whoever ran a bootstrap against a real host.
//!
//! So the shipped form of each one — `jail_fs_root: false`, which is what
//! production passes — is a golden here. A bump that rewrites a script fails
//! this file, and a human then reads the diff and decides whether the
//! choreography in `machines.rs` still matches it. That is the whole purpose:
//! these are **not** a specification of what the scripts should say, they are a
//! tripwire on what they do say.
//!
//! The second test is the reason the hermetic lane is trustworthy at all. The
//! rig in `roost::bootstrap::tests` runs with `jail_fs_root: true` so the
//! ladder's absolute rungs cannot reach the developer's own `/usr/bin` — which
//! is only sound if the jailed scripts are otherwise identical to the shipped
//! ones. `the_jailed_delta_is_only_the_fs_root_prefix` is what makes "tested in
//! a jail" mean "tested".
//!
//! **Regenerating.** Same switch as the provider goldens next door:
//!
//! ```text
//! SHED_UPDATE_ROOST_GOLDEN=1 cargo test -p shed-core --test roost_bootstrap_goldens
//! ```
//!
//! These files are **byte-exact** — no trailing newline is added or trimmed,
//! unlike `bootstrap/exec-chain-command.txt`, which carries one because Go reads
//! that one too and a text file without a final newline is a nuisance in every
//! tool that touches it.

use std::path::PathBuf;

use roost_ipc::bootstrap::{
    cleanup_script, commit_script, discard_backup_script, discovery_script, exec_chain_command,
    identity_script, path_check_command, prepare_script, rollback_script, start_script,
    stream_command, verify_staged_script, CANDIDATES, INSTALL_DEST_SUFFIX,
};
use shed_core::roost::bootstrap::SH_STDIN;

const UPDATE_ENV: &str = "SHED_UPDATE_ROOST_GOLDEN";

/// The three paths one install works with, spelled the way a shed's own home
/// spells them — `parse_prepare`'s exact shape, so a reader of these files sees
/// what a real run sends.
const DEST: &str = "/home/shed/.local/bin/roost-session";
const TMP: &str = "/home/shed/.local/bin/roost-session.tmp.4242";
const BACKUP: &str = "/home/shed/.local/bin/roost-session.bak.4242";

fn golden_path(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../fixtures/roost-vectors/bootstrap")
        .join(name)
}

fn updating() -> bool {
    std::env::var(UPDATE_ENV).is_ok_and(|value| value == "1")
}

/// Assert `live` against the golden, or write it when regenerating.
fn pin(name: &str, live: &str) {
    let path = golden_path(name);
    if updating() {
        std::fs::create_dir_all(path.parent().expect("the goldens have a parent directory"))
            .expect("create the bootstrap golden directory");
        std::fs::write(&path, live).unwrap_or_else(|e| panic!("writing {}: {e}", path.display()));
        return;
    }
    let golden = std::fs::read_to_string(&path).unwrap_or_else(|e| {
        panic!(
            "reading {}: {e} — regenerate with {UPDATE_ENV}=1",
            path.display()
        )
    });
    assert_eq!(
        golden,
        live,
        "{} no longer matches what roost-ipc emits at the pinned rev. Read the diff: if the \
         choreography in shed_core::roost::bootstrap::machines still matches the new script, \
         regenerate with {UPDATE_ENV}=1.",
        path.display()
    );
}

/// Every script the two machines feed to `/bin/sh -s`, and every command they
/// hand to the far side's own shell, in production form.
#[test]
fn the_shipped_scripts_and_commands_are_pinned() {
    // The probe, in order.
    pin("discovery-script.sh", &discovery_script(false));
    pin("path-check-command.txt", &path_check_command());
    pin(
        "identity-script.sh",
        &identity_script(&[DEST.to_string(), "/usr/bin/roost-session".to_string()]),
    );
    // The install, in order.
    pin("prepare-script.sh", &prepare_script());
    pin("stream-command.txt", &stream_command(TMP));
    pin("verify-staged-script.sh", &verify_staged_script(TMP));
    pin("commit-script.sh", &commit_script(TMP, DEST, BACKUP));
    pin(
        "post-commit-identity-script.sh",
        &identity_script(&[DEST.to_string()]),
    );
    pin("discard-backup-script.sh", &discard_backup_script(BACKUP));
    // The undo.
    pin("rollback-script.sh", &rollback_script(DEST, BACKUP));
    pin("cleanup-script.sh", &cleanup_script(TMP));
    // The start.
    pin("start-script.txt", &start_script(DEST));

    // Not a file: two words with nothing in them to drift. Pinned here anyway
    // because roost's own `SH_STDIN` is private, so shed restates it — and a
    // restated constant is exactly the kind that quietly stops matching.
    assert_eq!(SH_STDIN, "/bin/sh -s");
}

/// **The only thing a test lane changes is a prefix.**
///
/// `jail_fs_root` exists so a hermetic run cannot probe — or install into — the
/// developer's own `/usr/bin`. It is worth exactly as much as this assertion:
/// if the jailed scripts differed from the shipped ones in any other way, every
/// hermetic test in `roost::bootstrap::tests` would be testing something shed
/// never sends.
#[test]
fn the_jailed_delta_is_only_the_fs_root_prefix() {
    const EXPANSION: &str = "${ROOST_BOOTSTRAP_FS_ROOT:-}";

    // How many rungs the prefix may touch, derived from roost's own ladder
    // rather than counted by hand: the `/`-rooted ones. The `$HOME`-relative
    // rungs are already jailed by a fake `$HOME` and must NOT gain it.
    let jailable = CANDIDATES
        .iter()
        .filter(|candidate| candidate.marker(true) != candidate.marker(false))
        .count();
    assert!(jailable > 0, "the ladder has absolute rungs to jail");

    for (name, plain, jailed) in [
        (
            "discovery_script",
            discovery_script(false),
            discovery_script(true),
        ),
        (
            "exec_chain_command",
            exec_chain_command(false),
            exec_chain_command(true),
        ),
    ] {
        assert!(
            !plain.contains("ROOST_BOOTSTRAP_FS_ROOT"),
            "{name} must name no test-mode variable in its shipped form"
        );
        assert_eq!(
            jailed.matches(EXPANSION).count(),
            jailable,
            "{name} jails exactly the absolute rungs"
        );
        assert_eq!(
            jailed.replace(EXPANSION, ""),
            plain,
            "{name}: the jailed form differs from the shipped one by the prefix and \
             nothing else"
        );
    }

    // The install destination is a `$HOME`-relative rung, so it is NEVER
    // prefixed — an install into a jailed `/usr/bin` would be an install into a
    // path the shipped transport does not try.
    assert!(prepare_script().contains(&format!("$HOME{INSTALL_DEST_SUFFIX}")));
    assert!(!prepare_script().contains("ROOST_BOOTSTRAP_FS_ROOT"));
}
