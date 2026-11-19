//! The `identity_file` fixture, read through shed-core (plan 037 A8).
//!
//! `crates/fixtures/identityfile/` is written by the Go CLI's own tests
//! (`cmd/shed/identityfile_fixture_test.go`): `config.yaml` as the CLI's saver writes it,
//! `refused.yaml` as a person might edit it, and `expected.json` with Go's answers. This
//! reader must decode every value to the same string, give the same ssh options and
//! ssh_config lines byte for byte, and refuse every refused value with the same class.

use std::path::PathBuf;

use serde_json::Value;
use shed_core::config::ShedConfig;
use shed_core::identityfile::{
    check_value, identity_args, identity_config_lines, identity_file_args,
};

fn fixtures(dir: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../fixtures")
        .join(dir)
}

fn read(dir: &str, file: &str) -> String {
    std::fs::read_to_string(fixtures(dir).join(file)).expect("fixture")
}

fn strings(v: &Value) -> Vec<String> {
    v.as_array()
        .expect("an array")
        .iter()
        .map(|s| s.as_str().expect("a string").to_string())
        .collect()
}

/// Every accepted entry reads to Go's value, and gives Go's ssh options and ssh_config
/// lines byte for byte; the CLI's whole stanza, rebuilt around those lines, is Go's.
#[test]
fn the_accepted_entries_read_as_go_reads_them() {
    let expected: Value = serde_json::from_str(&read("identityfile", "expected.json")).unwrap();
    let config = ShedConfig::parse(&read("identityfile", "config.yaml"));
    let accepted = expected["accepted"].as_array().unwrap();
    assert_eq!(config.servers.len(), accepted.len(), "every entry is read");
    let host = expected["host"].as_str().unwrap();
    let port = expected["ssh_port"].as_u64().unwrap();
    let known_hosts = expected["known_hosts"].as_str().unwrap();
    let shed = expected["shed"].as_str().unwrap();
    for want in accepted {
        let name = want["entry"].as_str().unwrap();
        let entry = config
            .servers
            .iter()
            .find(|s| s.name == name)
            .unwrap_or_else(|| panic!("no entry {name}"));
        let value = want["identity_file"].as_str().unwrap();
        assert_eq!(entry.identity_file, value, "{name}: the decoded value");
        assert_eq!(
            check_value(value),
            if value.is_empty() {
                Err(shed_core::identityfile::ValueRule::Empty)
            } else {
                Ok(())
            }
        );
        assert_eq!(
            identity_args(&entry.identity_file),
            strings(&want["identity_args"]),
            "{name}: identity_args"
        );
        let lines = identity_config_lines(&entry.identity_file, "");
        let lines: Vec<String> = lines.lines().map(str::to_string).collect();
        assert_eq!(
            lines,
            strings(&want["ssh_config_lines"]),
            "{name}: ssh_config_lines"
        );
        let stanza = format!(
            "Host shed-{shed}\n    HostName {host}\n    Port {port}\n    User {shed}\n    \
             UserKnownHostsFile {known_hosts}\n{}",
            identity_config_lines(&entry.identity_file, "    ")
        );
        assert_eq!(
            stanza,
            want["ssh_config_stanza"].as_str().unwrap(),
            "{name}: the stanza"
        );
    }
}

/// Every refused entry decodes to Go's value and is refused with Go's class, before any
/// file is read (the paths need not exist) and so before any ssh could be spawned.
#[test]
fn the_refused_entries_are_refused_with_go_s_class() {
    let expected: Value = serde_json::from_str(&read("identityfile", "expected.json")).unwrap();
    let config = ShedConfig::parse(&read("identityfile", "refused.yaml"));
    let refused = expected["refused"].as_array().unwrap();
    assert_eq!(config.servers.len(), refused.len(), "every entry is read");
    for want in refused {
        let name = want["entry"].as_str().unwrap();
        let entry = config
            .servers
            .iter()
            .find(|s| s.name == name)
            .unwrap_or_else(|| panic!("no entry {name}"));
        assert_eq!(
            entry.identity_file,
            want["identity_file"].as_str().unwrap(),
            "{name}: the decoded value"
        );
        let class = want["class"].as_str().unwrap();
        assert_eq!(
            check_value(&entry.identity_file).map_err(|r| r.class()),
            Err(class),
            "{name}"
        );
        let err = identity_file_args(name, &entry.identity_file).unwrap_err();
        assert_eq!(err.rule.map(|r| r.class()), Some(class), "{name}: {err}");
    }
}
