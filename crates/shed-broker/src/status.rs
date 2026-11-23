//! The broker's health types and the shared RFC3339 time helpers.
//!
//! [`ServerHealth`] / [`NamespaceHealth`] / [`ActiveSource`] are the per-server
//! connection snapshot the supervisor reports (`Supervisor::health()`), and
//! [`SshSigningStatus`] is the SSH backend's signing block; the desktop app's
//! `broker.status` and the wire harness's status snapshot carry both. The RFC3339
//! helpers are shared by many core modules.

use serde::{Deserialize, Serialize};

use crate::sign_check::SignPolicy;
use crate::ssh_backend::OfferCount;

// The RFC3339 helpers moved to `shed_source::time` (plan 039 §3.7); re-exported here so
// every `crate::status::…` caller and embedder keeps its path. The two that were
// crate-private here stay so.
pub(crate) use shed_source::time::{days_from_civil, parse_rfc3339_to_unix};
pub use shed_source::time::{now_rfc3339, now_unix, rfc3339_utc};

/// The `ssh` block of the desktop's `broker.status`: the SSH backend's mode, the
/// usernames a login may be signed for (`ssh.allow_users`), whether an `ssh.offer_keys`
/// filter is set, and — once a listing has happened — how many keys the last listing
/// offered to sheds of how many the backend had. The counts are absent until then: the
/// broker lists keys only when a shed asks.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SshSigningStatus {
    pub mode: String,
    pub allow_users: Vec<String>,
    pub key_filter: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub keys_offered: Option<usize>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub keys_available: Option<usize>,
}

impl SshSigningStatus {
    /// The block for a backend in `mode` under `policy`, with the last listing's
    /// `counts` (`None` before any).
    pub fn new(mode: &str, policy: &SignPolicy, counts: Option<OfferCount>) -> SshSigningStatus {
        SshSigningStatus {
            mode: mode.to_string(),
            allow_users: policy.allow_users().to_vec(),
            key_filter: policy.offer_keys().is_some(),
            keys_offered: counts.map(|c| c.offered),
            keys_available: counts.map(|c| c.available),
        }
    }
}

/// `ServerHealth` is one watched server's connection state.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ServerHealth {
    pub name: String,
    pub url: String,
    pub namespaces: Vec<NamespaceHealth>,
    /// Set when ANY namespace on this server is `not-source`: the server answers this
    /// user's credential requests through another machine, named here. Additive (schema
    /// stays 1) and absent otherwise, so an older reader and the existing goldens see
    /// nothing new.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub active_source: Option<ActiveSource>,
}

/// `NamespaceHealth` is one namespace subscription's state on a server.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct NamespaceHealth {
    pub namespace: String,
    pub state: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_error: String,
    pub since: String,
    /// The machine the server named when it refused this subscription as `not-source`
    /// (plan 035 §3.4); absent in every other state.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub active_source: Option<ActiveSource>,
}

/// The machine a server reports as the active credential source for this user — the
/// `source` object of its `403 SOURCE_NOT_BOUND` refusal, as the server sent it. Either
/// field may be empty when the server's body left it out.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ActiveSource {
    #[serde(default)]
    pub machine_id: String,
    #[serde(default)]
    pub machine_name: String,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ns(name: &str, state: &str, last_error: &str) -> NamespaceHealth {
        NamespaceHealth {
            namespace: name.to_string(),
            state: state.to_string(),
            last_error: last_error.to_string(),
            since: String::new(),
            active_source: None,
        }
    }

    /// The not-source fields are additive: absent when unset (so an old reader and the
    /// recorded `servers[]` goldens see nothing new), present and round-tripping when set,
    /// and an older daemon's snapshot without them still decodes.
    #[test]
    fn active_source_fields_are_additive() {
        let src = ActiveSource {
            machine_id: "0123456789abcdef0123456789abcdef".into(),
            machine_name: "desk".into(),
        };
        let mut sv = ServerHealth {
            name: "a".into(),
            url: "http://a".into(),
            namespaces: vec![ns("ssh-agent", "connected", "")],
            active_source: None,
        };
        let plain = serde_json::to_value(&sv).unwrap();
        assert!(plain.get("active_source").is_none(), "{plain}");
        assert!(
            plain["namespaces"][0].get("active_source").is_none(),
            "{plain}"
        );

        sv.namespaces[0].state = "not-source".into();
        sv.namespaces[0].active_source = Some(src.clone());
        sv.active_source = Some(src.clone());
        let v = serde_json::to_value(&sv).unwrap();
        assert_eq!(v["active_source"]["machine_name"], "desk");
        assert_eq!(
            v["namespaces"][0]["active_source"]["machine_id"],
            "0123456789abcdef0123456789abcdef"
        );
        let back: ServerHealth = serde_json::from_value(v).unwrap();
        assert_eq!(back, sv);

        let old: ServerHealth = serde_json::from_value(serde_json::json!({
            "name": "a", "url": "http://a",
            "namespaces": [{"namespace": "ssh-agent", "state": "connected", "since": ""}]
        }))
        .unwrap();
        assert_eq!(old.active_source, None);
        assert_eq!(old.namespaces[0].active_source, None);
    }

    /// present (zeros included) after one; it round-trips.
    #[test]
    fn ssh_block_omits_the_counts_until_a_listing_and_round_trips() {
        let policy = SignPolicy::default();
        let block = SshSigningStatus::new("agent-forward", &policy, None);
        let v = serde_json::to_value(&block).unwrap();
        assert_eq!(
            v,
            serde_json::json!({"mode": "agent-forward", "allow_users": ["git"], "key_filter": false})
        );
        let back: SshSigningStatus = serde_json::from_value(v).unwrap();
        assert_eq!(back, block);

        let mut filter = crate::sign_check::KeyFilter::new();
        filter.insert_key_blob(b"k");
        let policy = SignPolicy::new(["git", "deploy"], Some(filter));
        let counts = OfferCount {
            offered: 0,
            available: 3,
        };
        let block = SshSigningStatus::new("local-keys", &policy, Some(counts));
        let v = serde_json::to_value(&block).unwrap();
        assert_eq!(
            v,
            serde_json::json!({
                "mode": "local-keys",
                "allow_users": ["git", "deploy"],
                "key_filter": true,
                "keys_offered": 0,
                "keys_available": 3,
            })
        );
        let back: SshSigningStatus = serde_json::from_value(v).unwrap();
        assert_eq!(back, block);
    }
}
