//! **The probe string is a shared contract, not a private constant.**
//!
//! [`shed_gx::PROBE_SCRIPT`] crosses SSH as one argv element, and SSH has no
//! argv API: the far side re-parses one string. Plan 012 built
//! `tests/machine-transport/` for exactly that hazard, and plan 017 §3.5 put
//! the probe in it as the `gx-probe` scenario — pinned by the Rust wire leg
//! (`shed-core`'s `machine_transport_contract`), by the LIVE leg through a real
//! sshd, and, after S4m, by shed-mobile's Dart composer.
//!
//! Those three legs all read `scenarios.json`. None of them reads this crate.
//! So this file is the fourth edge of the square: it asserts the scenario still
//! carries THIS constant, which is what makes editing the script here (or
//! there) a visible, one-test failure rather than a silent divergence between a
//! probe that is sent and a probe that is pinned.
//!
//! The script's own TEXT properties — no single quote, so the shell-quoted form
//! a golden pins stays legible, plus the delimiters the parser spells as
//! constants — are pinned by `discovery::tests::the_probe_script_carries_no_single_quote`,
//! in the same crate and the same `cargo test -p shed-gx` run. Not restated here:
//! two copies of one assertion is one that can be deleted without a failure.
//!
//! **It reads a path OUTSIDE the `crates/` workspace**, exactly as shed-core's
//! `machine_transport_contract.rs` already does — the contract lives outside
//! every leg's source tree on purpose, because one that lived inside a leg would
//! not be a contract. CI's `core-linux` job runs from a full checkout and is
//! fine. A run whose working tree is `crates/` alone (`make -C desktop
//! core-linux` mounts only that) cannot see the file and this test panics rather
//! than skipping — deliberately: a guard that skipped itself when it could not
//! find what it guards is not a guard.

use std::path::PathBuf;

fn scenarios() -> serde_json::Value {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../tests/machine-transport/scenarios.json");
    let text = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("reading {}: {e}", path.display()));
    serde_json::from_str(&text).unwrap_or_else(|e| panic!("parsing {}: {e}", path.display()))
}

#[test]
fn the_gx_probe_scenario_carries_this_crates_probe_script() {
    let doc = scenarios();
    let scenario = doc["scenarios"]
        .as_array()
        .expect("scenarios array")
        .iter()
        .find(|s| s["id"] == "gx-probe")
        .expect(
            "the machine-transport contract has no `gx-probe` scenario — the SSH \
             half of gx discovery is unpinned",
        );
    let argv: Vec<&str> = scenario["argv"]
        .as_array()
        .expect("argv array")
        .iter()
        .map(|v| v.as_str().expect("argv elements are strings"))
        .collect();
    assert_eq!(
        argv,
        ["sh", "-c", shed_gx::PROBE_SCRIPT],
        "the `gx-probe` scenario and PROBE_SCRIPT have drifted. Whichever one \
         changed, BOTH goldens must be re-recorded (UPDATE_GOLDEN=1 in \
         tests/machine-transport), `scenarios.json`'s version bumped, and the \
         Dart leg re-run in shed-mobile."
    );
}
