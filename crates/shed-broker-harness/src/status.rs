//! The harness's status snapshot (`{"op":"status"}` on the control socket): the JSON
//! shape the wire suite's goldens record. It keeps the field names and order of the
//! retired daemon's status report, less its approval-channel block, so the recorded
//! values compare unchanged. Only the harness builds it.

use std::collections::BTreeMap;

use serde::Serialize;

use shed_broker::config::{
    ExtensionsYaml, NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS, NS_SSH_AGENT,
};
use shed_broker::status::now_rfc3339;
use shed_broker::{MachineIdentity, ServerHealth, SshSigningStatus};

/// The version of the status JSON. Bumped only on a breaking change; additive fields do
/// not bump it.
pub const STATUS_SCHEMA_VERSION: u32 = 1;

/// The namespaces whose policy the report shows, in a fixed order.
const STATUS_NAMESPACES: [&str; 3] = [NS_SSH_AGENT, NS_AWS_CREDENTIALS, NS_DOCKER_CREDENTIALS];

/// The broker's self-report. Field order is the JSON order.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct LiveStatus {
    pub schema: u32,
    pub version: String,
    pub pid: i64,
    pub started_at: String,
    pub written_at: String,
    pub config_path: String,
    pub policies: BTreeMap<String, String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub gate_namespaces: Vec<String>,
    pub servers: Vec<ServerHealth>,
    /// This machine's identity; absent when it could not be loaded, in which case
    /// [`Self::machine_error`] says why.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub machine: Option<MachineIdentity>,
    /// Why there is no [`Self::machine`]; empty when there is one.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub machine_error: String,
    /// What this machine signs for a shed (plan 035 §3.5).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ssh: Option<SshSigningStatus>,
}

/// Snapshot the broker's live self-report. `servers` is the supervisor's per-server
/// connection snapshot, `machine` the identity the broker loaded or the reason there is
/// none, `ssh` the signing block.
pub fn build_live_status(
    cfg: &ExtensionsYaml,
    config_path: &str,
    started_at: &str,
    version: &str,
    servers: Vec<ServerHealth>,
    machine: &Result<MachineIdentity, String>,
    ssh: Option<SshSigningStatus>,
) -> LiveStatus {
    let policies = STATUS_NAMESPACES
        .iter()
        .map(|&ns| (ns.to_string(), cfg.effective_policy(ns)))
        .collect();
    let (machine, machine_error) = match machine {
        Ok(identity) => (Some(identity.clone()), String::new()),
        Err(e) => (None, e.clone()),
    };
    LiveStatus {
        schema: STATUS_SCHEMA_VERSION,
        version: version.to_string(),
        pid: current_pid(),
        started_at: started_at.to_string(),
        written_at: now_rfc3339(),
        config_path: config_path.to_string(),
        policies,
        gate_namespaces: cfg.gate_namespaces(),
        servers,
        machine,
        machine_error,
        ssh,
    }
}

fn current_pid() -> i64 {
    i64::from(std::process::id())
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_broker::NamespaceHealth;

    fn sample_status() -> LiveStatus {
        LiveStatus {
            schema: STATUS_SCHEMA_VERSION,
            version: "v9".to_string(),
            pid: 4242,
            started_at: "2026-06-11T00:00:00Z".to_string(),
            written_at: "2026-06-11T00:00:01Z".to_string(),
            config_path: "/etc/shed/extensions.yaml".to_string(),
            policies: BTreeMap::from([
                ("ssh-agent".to_string(), "shed-desktop".to_string()),
                ("aws-credentials".to_string(), "deny-all".to_string()),
                ("docker-credentials".to_string(), "approve-all".to_string()),
            ]),
            gate_namespaces: vec!["ssh-agent".to_string()],
            servers: vec![ServerHealth {
                name: "mac".to_string(),
                url: "http://localhost:8080".to_string(),
                namespaces: vec![NamespaceHealth {
                    namespace: "ssh-agent".to_string(),
                    state: "connected".to_string(),
                    last_error: String::new(),
                    since: String::new(),
                    active_source: None,
                }],
                active_source: None,
            }],
            machine: None,
            machine_error: String::new(),
            ssh: None,
        }
    }

    /// The keys, in the order the goldens record them; no approval-channel block.
    #[test]
    fn the_json_keys_are_in_the_recorded_order() {
        let text = serde_json::to_string(&sample_status()).unwrap();
        let order: Vec<usize> = [
            "\"schema\"",
            "\"version\"",
            "\"pid\"",
            "\"started_at\"",
            "\"written_at\"",
            "\"config_path\"",
            "\"policies\"",
            "\"gate_namespaces\"",
            "\"servers\"",
        ]
        .iter()
        .map(|k| text.find(k).unwrap_or_else(|| panic!("{k} in {text}")))
        .collect();
        assert!(order.windows(2).all(|w| w[0] < w[1]), "{text}");
        assert!(!text.contains("approval_channel"), "{text}");
        let v = serde_json::to_value(sample_status()).unwrap();
        assert_eq!(v["policies"]["ssh-agent"], "shed-desktop");
        // `since` is always present, even when empty.
        assert_eq!(v["servers"][0]["namespaces"][0]["since"], "");
    }

    #[test]
    fn empty_gate_namespaces_machine_and_ssh_are_omitted() {
        let mut ls = sample_status();
        ls.gate_namespaces.clear();
        let text = serde_json::to_string(&ls).unwrap();
        for absent in ["gate_namespaces", "machine", "\"ssh\""] {
            assert!(!text.contains(absent), "{absent}: {text}");
        }
        assert!(text.contains("\"servers\":["), "{text}");
    }

    #[test]
    fn build_live_status_reports_the_machine_or_why_not() {
        let cfg = ExtensionsYaml::try_parse("").unwrap();
        let identity = MachineIdentity {
            id: "0123456789abcdef0123456789abcdef".into(),
            name: "laptop".into(),
        };
        let ok = build_live_status(&cfg, "/c", "", "v", vec![], &Ok(identity.clone()), None);
        assert_eq!(ok.machine, Some(identity));
        assert_eq!(ok.machine_error, "");
        assert_eq!(ok.pid, i64::from(std::process::id()));

        let err = build_live_status(&cfg, "/c", "", "v", vec![], &Err("no way".into()), None);
        assert_eq!(err.machine, None);
        let v = serde_json::to_value(&err).unwrap();
        assert!(v.get("machine").is_none());
        assert_eq!(v["machine_error"], "no way");
    }

    #[test]
    fn build_live_status_carries_the_policies_and_the_ssh_block() {
        let cfg = ExtensionsYaml::try_parse("").unwrap();
        let ssh = SshSigningStatus::new("local-keys", &cfg.ssh_sign_policy(), None);
        let ls = build_live_status(
            &cfg,
            "/c",
            "",
            "v",
            vec![],
            &Err("x".into()),
            Some(ssh.clone()),
        );
        assert_eq!(ls.ssh, Some(ssh));
        assert_eq!(ls.schema, STATUS_SCHEMA_VERSION);
        let names: Vec<&str> = ls.policies.keys().map(String::as_str).collect();
        assert_eq!(
            names,
            ["aws-credentials", "docker-credentials", "ssh-agent"]
        );
        assert_eq!(ls.gate_namespaces, cfg.gate_namespaces());
    }
}
