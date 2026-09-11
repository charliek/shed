//! **The Rust leg of the `shed roost-provider` goldens** (plan 019 §3.2, §3.3).
//!
//! The provider is Go (`internal/roostprovider/`, in the `server` component,
//! which has no Rust in it) but three of the things it has to get exactly right
//! are defined in Rust — two of them in `roost-ipc` itself, at the rev
//! `crates/Cargo.toml` pins. A Go constant that merely *looked* right on the day
//! it was copied is the failure mode; a golden asserted from BOTH sides is what
//! makes the two provably the same rather than the same today.
//!
//! | golden | this leg asserts it against | the Go leg asserts it against |
//! |---|---|---|
//! | `bootstrap/exec-chain-command.txt` | the live `roost_ipc::bootstrap::exec_chain_command(false)` | `roostprovider.ExecChainCommand` |
//! | `agent-table.json` | `launch_argv` + `roost_capabilities().kinds` | `roostprovider`'s `agentTable` |
//! | `stderr-classes.json` (`classes`) | the live `roost_ipc::ssh::classify_ssh_failure` | `roostprovider.ClassifySSHFailure` |
//!
//! `stderr-classes.json`'s other section, `provider_rows`, is shed-only (nothing
//! in Rust has a provider menu) and is asserted from Go alone — see that file's
//! own comment for why the two live together.
//!
//! **Regenerating `exec-chain-command.txt`.** It is GENERATED, never
//! hand-written, and the generator is this very test:
//!
//! ```text
//! SHED_UPDATE_ROOST_GOLDEN=1 cargo test -p shed-core --test roost_provider_vectors
//! ```
//!
//! writes the file from the live function instead of asserting against it. Run
//! that after a `roost-ipc` bump, `git diff` the result, and if the ladder moved,
//! re-copy the string into Go's `ExecChainCommand`. Without the variable the same
//! test asserts, so a bump that changes the ladder fails loudly rather than
//! leaving the Go constant quietly wrong.

use std::collections::BTreeSet;
use std::path::PathBuf;

use roost_ipc::bootstrap::exec_chain_command;
use roost_ipc::messages::SESSION_PROTOCOL_VERSION;
use roost_ipc::ssh::{classify_ssh_failure, SshFailure};
use shed_core::rc::RcKind;
use shed_core::roost::model::{launch_argv, roost_capabilities};

/// Set this to 1 to REWRITE the generated goldens instead of asserting them.
const UPDATE_ENV: &str = "SHED_UPDATE_ROOST_GOLDEN";

fn vectors_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../fixtures/roost-vectors")
}

fn read_vector(name: &str) -> String {
    let path = vectors_dir().join(name);
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("reading {}: {e}", path.display()))
}

fn read_json(name: &str) -> serde_json::Value {
    let text = read_vector(name);
    serde_json::from_str(&text).unwrap_or_else(|e| panic!("parsing {name}: {e}"))
}

fn updating() -> bool {
    std::env::var(UPDATE_ENV).is_ok_and(|v| v == "1")
}

/// The remote command the Go provider execs is roost's own, byte for byte.
///
/// The golden carries a trailing newline the command itself does not (a text
/// file without one is a nuisance in every tool that touches it), so exactly one
/// is trimmed on the way back in — `trim_end` would also eat a trailing space the
/// ladder might one day legitimately end with.
#[test]
fn the_exec_chain_golden_is_roosts_own_ladder() {
    let live = exec_chain_command(false);
    let path = vectors_dir().join("bootstrap/exec-chain-command.txt");

    if updating() {
        std::fs::create_dir_all(path.parent().expect("the golden has a parent directory"))
            .expect("create the bootstrap golden directory");
        std::fs::write(&path, format!("{live}\n")).expect("write the exec-chain golden");
        return;
    }

    let golden = read_vector("bootstrap/exec-chain-command.txt");
    let golden = golden
        .strip_suffix('\n')
        .expect("the exec-chain golden ends in exactly one newline");
    assert_eq!(
        golden, live,
        "the pinned roost-ipc rev's exec chain no longer matches the golden — \
         re-run with {UPDATE_ENV}=1 and re-copy the result into Go's ExecChainCommand"
    );

    // The one property the Go side depends on beyond equality: the whole
    // command survives as ONE single-quoted word, so no `'\''` — which is not
    // an escape in csh/tcsh/fish — has to reach a far side's login shell. roost
    // pins this on its own side too; it is re-pinned here because the Go
    // constant is a literal and a literal can be edited.
    let inner = live
        .strip_prefix("sh -c '")
        .and_then(|rest| rest.strip_suffix('\''))
        .expect("the exec chain is `sh -c '<one single-quoted word>'`");
    assert!(
        !inner.contains('\''),
        "the exec chain must carry no embedded single quote"
    );
}

/// One agent row of `agent-table.json`.
#[derive(serde::Deserialize)]
struct AgentRow {
    kind: String,
    binary: String,
    title: String,
}

/// `agent-table.json`'s shape. Deserialized straight from the file's text
/// rather than through a `serde_json::Value` — going via a `Value` means
/// parsing the whole document, deep-cloning the array out of it, and then
/// parsing that clone again, for a file whose only interesting part is
/// `agents`.
#[derive(serde::Deserialize)]
struct AgentTable {
    agents: Vec<AgentRow>,
}

fn agent_rows() -> Vec<AgentRow> {
    let table: AgentTable = serde_json::from_str(&read_vector("agent-table.json"))
        .expect("agent-table.json's `agents` array");
    table.agents
}

/// Every row's `binary` is what `launch_argv` would exec for that kind.
///
/// This is the half that matters most in practice: the provider probes for
/// `cursor-agent` and the desktop launches `cursor-agent`, and the day those
/// disagree the menu offers an agent whose tab then dies with "command not
/// found".
#[test]
fn the_agent_table_binaries_are_launch_argv() {
    for row in agent_rows() {
        let kind = RcKind::from_wire(&row.kind);
        assert_eq!(
            kind.as_str(),
            row.kind,
            "agent-table.json's kind {:?} is not a known RcKind (from_wire kept it as Other)",
            row.kind
        );
        assert_eq!(
            launch_argv(&kind),
            Some(vec![row.binary.clone()]),
            "launch_argv disagrees with agent-table.json for {}",
            row.kind
        );
    }
}

/// The table's kinds are exactly `roost_capabilities().kinds`.
///
/// Compared as SETS, never as sequences. The golden is in the PROVIDER's display
/// order (`claude codex cursor-agent opencode gx grok`, pinned twice by plan 019
/// §3.2), while `roost_capabilities` orders cursor and opencode the other way
/// round. One is a menu a human reads and the other is a capabilities
/// advertisement whose order means nothing — so the thing worth asserting is
/// that neither list gained or lost a kind, not that they were typed out the
/// same way.
#[test]
fn the_agent_table_kinds_are_the_roost_capabilities_kinds() {
    let table: BTreeSet<String> = agent_rows().into_iter().map(|row| row.kind).collect();
    let capabilities: BTreeSet<String> = roost_capabilities()
        .kinds
        .iter()
        .map(|kind| kind.as_str().to_string())
        .collect();
    assert_eq!(table, capabilities);
}

/// The titles are the provider's own copy, so nothing in Rust defines them —
/// but they must at least be distinct, or two palette rows would be
/// indistinguishable to whoever is choosing between them.
#[test]
fn the_agent_table_titles_are_distinct() {
    let rows = agent_rows();
    let titles: BTreeSet<String> = rows.iter().map(|row| row.title.clone()).collect();
    assert_eq!(titles.len(), rows.len(), "two agents share a title");
}

/// One case of `stderr-classes.json`'s `classes`.
#[derive(serde::Deserialize)]
struct ClassCase {
    name: String,
    exit_code: Option<i32>,
    stderr: String,
    class: String,
    detail: Option<String>,
}

/// The half of `stderr-classes.json` this leg reads. Its other section,
/// `provider_rows`, is shed-only and asserted from Go — serde ignores it here
/// rather than this file having to know it exists.
#[derive(serde::Deserialize)]
struct StderrClasses {
    classes: Vec<ClassCase>,
}

/// The golden's kebab-case class names, mapped onto roost's own enum.
fn class_name(failure: &SshFailure) -> (&'static str, Option<String>) {
    match failure {
        SshFailure::ChangedHostKey => ("changed-host-key", None),
        SshFailure::HostKeyUnknown => ("host-key-unknown", None),
        SshFailure::Auth => ("auth", None),
        SshFailure::NoSession => ("no-session", None),
        SshFailure::NotFound => ("not-found", None),
        SshFailure::Transport(detail) => ("transport", detail.clone()),
    }
}

/// Every pinned case classifies the way the golden says — including the
/// precedence cases, which are the ones that would break silently if roost ever
/// reordered its rules.
#[test]
fn the_stderr_classes_golden_is_roosts_own_classifier() {
    let golden: StderrClasses = serde_json::from_str(&read_vector("stderr-classes.json"))
        .expect("stderr-classes.json's `classes` array");
    let cases = &golden.classes;
    assert!(!cases.is_empty(), "the golden carries no cases");

    for case in cases {
        let (class, detail) = class_name(&classify_ssh_failure(case.exit_code, &case.stderr));
        assert_eq!(class, case.class, "class for {}", case.name);
        assert_eq!(detail, case.detail, "detail for {}", case.name);
    }

    // Every class is exercised. A golden that silently stopped covering one
    // would keep passing while the Go port drifted on exactly that branch.
    let covered: BTreeSet<&str> = cases.iter().map(|case| case.class.as_str()).collect();
    assert_eq!(
        covered,
        BTreeSet::from([
            "changed-host-key",
            "host-key-unknown",
            "auth",
            "no-session",
            "not-found",
            "transport",
        ])
    );
}

/// The vendored protocol-4 identify vector really carries this build's protocol
/// number.
///
/// This is the middle link of a three-link chain: Go's `SpokenProtocol` constant
/// is asserted against this vector's `session_protocol` on its own side, and this
/// test asserts the same field against `SESSION_PROTOCOL_VERSION` at the pinned
/// rev. Together they pin a Go integer to a Rust constant without Go ever seeing
/// Rust.
#[test]
fn the_identify_vector_carries_this_builds_protocol() {
    let vector = read_json("session.identify.response.v4.json");
    assert_eq!(
        vector["result"]["session_protocol"].as_u64(),
        Some(u64::from(SESSION_PROTOCOL_VERSION)),
    );
}
