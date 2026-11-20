//! The language-neutral golden fixtures of the broker's wire harness, read by the
//! broker's own config functions. The JSON vectors were recorded while the retired Go
//! runner read them too, and this test asserts the pure decision functions
//! (`effective_policy_from_raw`, `ExtensionsYaml::gate_namespaces`) match every
//! vector: a drift guard the live wire cells cannot give.
//!
//! An integration test of the `shed-broker` library: it imports the real config
//! functions the desktop app's embedded broker runs (`make test-broker-wire`
//! runs it as `cargo test -p shed-broker golden`).

use std::path::PathBuf;

use shed_broker::config::{effective_policy_from_raw, ExtensionsYaml};

/// The shared fixture directory. `CARGO_MANIFEST_DIR` is `crates/shed-broker`;
/// `../../` is the repo root, where `tests/broker-wire/fixtures` lives (the
/// wire harness's neutral home for the vectors).
fn fixtures_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../tests/broker-wire/fixtures")
}

/// Build a config from the three raw approval policies via the crate's real
/// (block-style) config parser, so the gate golden exercises the same
/// `parse` → `gate_namespaces` path the broker uses. Only clean policy tokens flow
/// through here, so a minimal block-YAML template is sufficient.
fn config_from_policies(ssh: &str, aws: &str, docker: &str) -> ExtensionsYaml {
    let text = format!(
        "ssh:\n  approval:\n    policy: {ssh}\n\
         aws:\n  approval:\n    policy: {aws}\n\
         docker:\n  approval:\n    policy: {docker}\n"
    );
    // `parse` is a `#[cfg(test)]`-only convenience inside the crate; from this external
    // integration test use the public `try_parse` (same block-YAML path) + expect.
    ExtensionsYaml::try_parse(&text).expect("yaml fixture parses")
}

fn read_fixture(name: &str) -> serde_json::Value {
    let path = fixtures_dir().join(name);
    let data = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("read fixture {}: {e}", path.display()));
    serde_json::from_str(&data).unwrap_or_else(|e| panic!("parse fixture {}: {e}", path.display()))
}

#[test]
fn golden_effective_policy() {
    let fx = read_fixture("effective_policy.json");
    assert_eq!(
        fx["protocol_version"].as_i64(),
        Some(2),
        "effective_policy.json protocol_version skew"
    );
    let vectors = fx["vectors"].as_array().expect("vectors array");
    assert!(!vectors.is_empty(), "no vectors");
    for v in vectors {
        let raw = v["raw"].as_str().expect("raw");
        let want = v["effective"].as_str().expect("effective");
        let got = effective_policy_from_raw(raw);
        assert_eq!(got, want, "effective_policy_from_raw({raw:?})");
    }
}

#[test]
fn golden_gate_namespaces() {
    let fx = read_fixture("gate_namespaces.json");
    assert_eq!(
        fx["protocol_version"].as_i64(),
        Some(2),
        "gate_namespaces.json protocol_version skew"
    );
    let vectors = fx["vectors"].as_array().expect("vectors array");
    assert!(!vectors.is_empty(), "no vectors");
    for v in vectors {
        let ssh = v["ssh"].as_str().expect("ssh");
        let aws = v["aws"].as_str().expect("aws");
        let docker = v["docker"].as_str().expect("docker");
        let want: Vec<String> = v["gate"]
            .as_array()
            .expect("gate array")
            .iter()
            .map(|g| g.as_str().expect("gate item").to_string())
            .collect();
        let got = config_from_policies(ssh, aws, docker).gate_namespaces();
        assert_eq!(
            got, want,
            "gate for ssh={ssh:?} aws={aws:?} docker={docker:?}"
        );
    }
}
