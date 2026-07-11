//! The Rust half of mechanism 2 (language-neutral golden fixtures) of the host-agent
//! differential harness. Reads the SAME JSON vectors the Go runner reads
//! (`cmd/shed-host-agent/golden_test.go`) and asserts the Rust pure decision
//! functions (`effective_policy_from_raw`, `HostAgentConfig::gate_namespaces`) match
//! every vector. The Go and Rust runners agreeing with a committed fixture is the
//! drift guard the live differential cannot give.
//!
//! `shed-host-agent` is a binary-only crate (no `lib.rs`), so this integration test
//! pulls in the self-contained `config` module directly via `#[path]` rather than
//! importing a library target — keeping the crate a pure binary while still
//! exercising its real config functions.

#[path = "../src/config.rs"]
mod config;

use std::path::PathBuf;

use config::{effective_policy_from_raw, HostAgentConfig};

/// The shared fixture directory. `CARGO_MANIFEST_DIR` is `crates/shed-host-agent`;
/// `../../` is the repo root, where `tests/host-agent-diff/fixtures` lives (the
/// neutral home shared with the Go runner).
fn fixtures_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../tests/host-agent-diff/fixtures")
}

/// Build a config from the three raw approval policies via the crate's real
/// (block-style) config parser, so the gate golden exercises the same
/// `parse` → `gate_namespaces` path the daemon uses. Only clean policy tokens flow
/// through here, so a minimal block-YAML template is sufficient.
fn config_from_policies(ssh: &str, aws: &str, docker: &str) -> HostAgentConfig {
    let text = format!(
        "ssh:\n  approval:\n    policy: {ssh}\n\
         aws:\n  approval:\n    policy: {aws}\n\
         docker:\n  approval:\n    policy: {docker}\n"
    );
    HostAgentConfig::parse(&text)
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
