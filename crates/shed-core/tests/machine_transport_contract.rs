//! **The Rust leg of the machine-transport differential** (plan 012 S3, AC2).
//!
//! SSH has no argv API: a remote command is sent as ONE string the far side's
//! shell re-parses. The Tauri app composes that string in Rust
//! (`machine::display_line`, today reached only through `shed-gx`'s discovery
//! probe — see `crates/shed-gx/src/discovery.rs::PROBE_SCRIPT` — `sx`, its
//! other caller, was sunset, unreleased, in plan 016). There is no Dart leg:
//! shed-mobile execs the string `roost_ipc::ssh::remote_command()` composes,
//! not an argv built from this contract (see the README's "The Dart leg").
//! This file is one of the two legs that keep the Rust composer honest:
//!
//! | leg | lives in | asserts |
//! |---|---|---|
//! | **Rust** | here | `machine::display_line` composes `goldens/wire.json` |
//! | live | `tests/machine-transport/` (pytest + a hermetic sshd) | that wire line really delivers `goldens/received.json` |
//!
//! The scenarios and goldens are shared files, deliberately outside this crate
//! (`tests/machine-transport/`), because a contract that lived in one leg's
//! source tree would not be a contract.
//!
//! **This leg is pure and fast** — no ssh, no network. It answers "does Rust
//! still compose the agreed bytes?", which is the question that catches a
//! quoter change. Whether those bytes actually produce the intended argv is the
//! live leg's job, and that separation is deliberate: a golden that only ever
//! checked itself would be self-fulfilling.

use std::path::PathBuf;

use shed_core::machine::display_line;

fn contract_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../tests/machine-transport")
}

fn read_json(name: &str) -> serde_json::Value {
    let path = contract_dir().join(name);
    let text = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("reading {}: {e}", path.display()));
    serde_json::from_str(&text).unwrap_or_else(|e| panic!("parsing {}: {e}", path.display()))
}

fn argv_of(scenario: &serde_json::Value) -> Vec<String> {
    scenario["argv"]
        .as_array()
        .expect("scenario.argv is an array")
        .iter()
        .map(|v| v.as_str().expect("argv elements are strings").to_string())
        .collect()
}

/// Every scenario's composed wire line matches the recorded golden.
///
/// A failure here means Rust's quoting changed. That is either a bug or a
/// deliberate contract change — and if deliberate it must be re-recorded AND
/// the Dart leg re-run, because the two transports would otherwise be putting
/// different bytes on the wire for identical argv.
#[test]
fn rust_composes_the_agreed_wire_line_for_every_scenario() {
    let scenarios = read_json("scenarios.json");
    let wire = read_json("goldens/wire.json");

    let cases = scenarios["scenarios"].as_array().expect("scenarios array");
    assert!(!cases.is_empty(), "the contract has no scenarios");

    for scenario in cases {
        let id = scenario["id"].as_str().expect("scenario.id");
        let want = wire[id].as_str().unwrap_or_else(|| {
            panic!("no golden wire line for scenario {id:?} — re-record with UPDATE_GOLDEN=1")
        });
        let got = display_line(&argv_of(scenario));
        assert_eq!(
            got, want,
            "scenario {id:?}: composed wire line differs from the golden"
        );
    }

    // The goldens must not carry entries for scenarios that no longer exist —
    // a stale golden is a silently-unchecked cell.
    let ids: Vec<&str> = cases
        .iter()
        .map(|s| s["id"].as_str().expect("scenario.id"))
        .collect();
    for key in wire.as_object().expect("wire.json is an object").keys() {
        assert!(
            ids.contains(&key.as_str()),
            "goldens/wire.json has a stale entry {key:?} with no matching scenario"
        );
    }
}

/// The contract's `version` is pinned here.
///
/// Bumping `scenarios.json` without bumping `version` is the failure mode this
/// catches: this assertion pins the revision the corpus was last validated
/// against, so a silent edit here fails loudly instead of leaving the Rust
/// byte pin and the live leg's fresh read of `scenarios.json` disagreeing
/// about which contract is current.
#[test]
fn the_contract_version_is_explicit() {
    let scenarios = read_json("scenarios.json");
    let version = scenarios["version"].as_u64().expect("scenarios.version");
    assert_eq!(
        version, 3,
        "the machine-transport contract changed version — update this assertion \
         once the new revision is re-recorded"
    );
}

/// Quoting is not optional anywhere: every element of every scenario ends up
/// single-quoted, including the bare-safe ones.
///
/// This is the property that distinguishes the house quoter
/// ([`shed_core::machine::shell_quote_always`], the verbatim port of Go's
/// `shellQuote`) from a conditional one. Both produce the same ARGV after the
/// remote shell parses them, so only a byte-level assertion catches a transport
/// that quietly switched — which is exactly the divergence shed-mobile's own
/// `shell_quote.dart` had before this contract existed.
#[test]
fn every_element_is_always_quoted_never_conditionally() {
    let scenarios = read_json("scenarios.json");
    for scenario in scenarios["scenarios"].as_array().expect("scenarios array") {
        let id = scenario["id"].as_str().expect("scenario.id");
        let argv = argv_of(scenario);
        let line = display_line(&argv);
        assert!(
            line.starts_with('\''),
            "scenario {id:?}: the first element must be quoted even when bare-safe: {line}"
        );
        // Every scenario's argv[0] is `sh` (C8 re-pointed the corpus onto the
        // one surviving composer, gx's `sh -c <script>` probe) — a bare-safe
        // token that must still appear quoted.
        assert!(
            line.starts_with("'sh'"),
            "scenario {id:?}: bare-safe tokens are quoted too: {line}"
        );
    }
}
