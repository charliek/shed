//! The embedded credential broker's wiring (leg 3a.2).
//!
//! The app is the credential broker: [`build`] loads `extensions.yaml` (or synthesizes
//! the fresh-install default) and builds the in-process `EmbeddedBroker` (bus broker +
//! approvals + minting), which lives and dies with the app. It produces the pieces the
//! setup hook feeds into the shared `Backend` (the minter — before `Backend`,
//! launch-order per §3.2) and `Coordinator` (the event stream + `Responder`), plus a
//! [`BrokerRuntime`] that backs `broker.status` and carries the exit-drain handle. A
//! broker that fails to start (malformed config / ssh-resolve failure) fails CLOSED —
//! surfaced in `broker.status`, the app keeps running.

use std::sync::{Arc, Mutex};

use serde_json::{json, Value};
use tokio::sync::mpsc;

use shed_app::traits::{Responder, ResponderRef};
use shed_app::{load_or_synthesize, BrokerConfig, BrokerEvent, EmbeddedBroker, MachineIdentity};
use shed_core::approval::{ApprovalDecision, DecidedBy};
use shed_core::token::TokenMinter;

use crate::env::Env;

/// The pieces the setup hook wires into `Backend` + `Coordinator`.
pub struct Spine {
    /// The Backend's secure-server minter (built BEFORE the Backend, §3.2). `None` when
    /// the broker failed closed.
    pub minter: Option<Arc<dyn TokenMinter>>,
    /// The Coordinator's event stream (approval requests + audit rows, or a closed
    /// stream = approvals dark).
    pub event_rx: mpsc::UnboundedReceiver<BrokerEvent>,
    /// The Coordinator's decision sink.
    pub responder: ResponderRef,
    /// The observability + lifecycle handle, managed in Tauri state.
    pub runtime: BrokerRuntime,
}

/// How the embedded broker got its config, for `broker.status`.
enum ConfigSource {
    /// A user-authored `extensions.yaml` was loaded.
    Loaded,
    /// No `extensions.yaml` — the synthesized fresh-install default.
    Synthesized,
    /// `extensions.yaml` present but malformed/invalid (fail-closed; the message).
    Error(String),
}

/// The embedded broker (if its config loaded) and how it started. Managed in Tauri
/// state so `broker.status` reads it and the exit path drains it. Immutable except
/// `error` (set once, at [`start`](Self::start)).
pub struct BrokerRuntime {
    config_source: ConfigSource,
    /// The embedded broker — `None` when its config failed closed. Held for `health()` /
    /// `resolved_ssh_mode()` reads and the shutdown handle.
    agent: Option<Arc<EmbeddedBroker>>,
    /// A broker-startup failure (config error or ssh-resolve failure at spawn) — the
    /// single "broker is broken" signal `broker.status` + the tray/Preferences read.
    error: Mutex<Option<String>>,
    /// This machine's identity once read from the state directory when the broker did
    /// not load one (see [`this_machine`](Self::this_machine)). Only a success is kept: a failed read
    /// is tried again next time.
    machine_cache: Mutex<Option<MachineIdentity>>,
}

impl BrokerRuntime {
    /// Start the bus NOW that the Coordinator is consuming the bridge's event stream (a
    /// no-op when the config failed closed). An ssh-resolve failure at spawn is recorded
    /// as a fail-closed error, NOT a crash (minting still works — the Backend already
    /// holds the minter).
    pub async fn start(&self) {
        if let Some(agent) = &self.agent {
            if let Err(e) = agent.spawn().await {
                // Logged as well as stored (#414): `broker.status` reports it only
                // to whoever asks.
                app_error!(
                    "shed-desktop-tauri: broker: the embedded broker did not start ({e}); \
                     approvals are unavailable, token minting still works"
                );
                *self.error.lock().unwrap() = Some(e.to_string());
            }
        }
    }

    /// Best-effort drain on a deliberate quit (§3.6): flip the supervisor's shutdown
    /// watch. Non-blocking — the supervisor drains on its own task; server-side bus
    /// disconnects finish the cleanup.
    pub fn signal_shutdown(&self) {
        if let Some(agent) = &self.agent {
            let _ = agent.shutdown_handle().send(true);
        }
    }

    /// This machine's identity (plan 035 §3.4) — the id a "Use this machine" sends.
    ///
    /// It is the identity the running broker loaded at spawn, so the id the server is
    /// asked to bind is exactly the one on the broker's own requests (and the broker's
    /// load error, when it had none). A broker that never spawned (its config failed
    /// closed) has none, so it is read from the state directory. A success is cached;
    /// the read is file I/O under an advisory lock, so it runs off the async worker.
    pub async fn this_machine(&self) -> Result<MachineIdentity, String> {
        if let Some(loaded) = self.agent.as_ref().and_then(|a| a.machine_identity()) {
            return loaded;
        }
        if let Some(cached) = self.machine_cache.lock().unwrap().clone() {
            return Ok(cached);
        }
        let loaded = tokio::task::spawn_blocking(shed_app::machine_identity)
            .await
            .unwrap_or_else(|e| Err(format!("machine identity load task failed: {e}")));
        if let Ok(m) = &loaded {
            *self.machine_cache.lock().unwrap() = Some(m.clone());
        }
        loaded
    }

    /// Wake the embedded broker's waiting subscriptions so a machine just made the
    /// source connects at once rather than on its next backoff. `false` when there is
    /// no broker to wake (its config failed closed).
    pub fn retry_now(&self) -> bool {
        match &self.agent {
            Some(agent) => {
                agent.retry_now();
                true
            }
            None => false,
        }
    }

    /// Whether the embedded broker failed to start (config or spawn), for wording.
    pub fn broker_failed(&self) -> bool {
        self.error.lock().unwrap().is_some()
    }

    /// The startup log line (#414): where the embedded broker's config came from.
    pub fn describe(&self) -> String {
        let config = match &self.config_source {
            ConfigSource::Loaded => "loaded".to_string(),
            ConfigSource::Synthesized => "synthesized".to_string(),
            ConfigSource::Error(m) => format!("error: {m}"),
        };
        format!("broker: config={config}")
    }

    /// The `broker.status` snapshot: config provenance, the resolved ssh mode, gate
    /// namespaces, and LiveStatus-shaped per-server health (per-namespace conn state incl.
    /// `Rejected`).
    pub fn status(&self) -> Value {
        let (config_source, config_message) = match &self.config_source {
            ConfigSource::Loaded => (json!("loaded"), Value::Null),
            ConfigSource::Synthesized => (json!("synthesized"), Value::Null),
            ConfigSource::Error(m) => (json!("error"), json!(m)),
        };
        let (resolved_ssh_mode, gate_namespaces, servers) = match &self.agent {
            Some(a) => (
                a.resolved_ssh_mode().map_or(Value::Null, Value::from),
                json!(a.gate_namespaces()),
                // ServerHealth: Serialize — never named here (the tauri crate doesn't
                // depend on shed-broker directly), just serialized in place.
                serde_json::to_value(a.health()).unwrap_or_else(|_| json!([])),
            ),
            None => (Value::Null, json!([]), json!([])),
        };
        // This machine's identity as the embedded broker loaded it (plan 035 §3.4), or
        // why there is none; both null before the bus started.
        let (machine, machine_error) = match self.agent.as_ref().and_then(|a| a.machine_identity())
        {
            Some(Ok(m)) => (serde_json::to_value(m).unwrap_or(Value::Null), Value::Null),
            Some(Err(e)) => (Value::Null, Value::from(e)),
            None => (Value::Null, Value::Null),
        };
        // What the embedded broker signs for a shed (plan 035 §3.5); null before the bus
        // started.
        let ssh = self
            .agent
            .as_ref()
            .and_then(|a| a.ssh_signing_status())
            .and_then(|s| serde_json::to_value(s).ok())
            .unwrap_or(Value::Null);
        json!({
            "config": { "source": config_source, "message": config_message },
            "resolved_ssh_mode": resolved_ssh_mode,
            "gate_namespaces": gate_namespaces,
            "servers": servers,
            "machine": machine,
            "machine_error": machine_error,
            "ssh": ssh,
            "broker_error": self.error.lock().unwrap().clone().map_or(Value::Null, Value::from),
        })
    }
}

/// A no-op `Responder` for a config-failed broker (approvals dark): no approval
/// requests ever arrive, so a decision never needs delivering.
struct NoopResponder;

impl Responder for NoopResponder {
    fn respond(
        &self,
        _request_id: &str,
        _decision: ApprovalDecision,
        _decided_by: DecidedBy,
        _scope: Option<&str>,
        _ttl: Option<&str>,
    ) {
    }
}

/// Build the broker spine. Runs inside the Tauri tokio runtime (`EmbeddedBroker::new`
/// `tokio::spawn`s); the bus itself starts later, via [`BrokerRuntime::start`], once the
/// Coordinator is consuming.
pub fn build(env: &Env) -> Spine {
    match load_or_synthesize(&env.broker_extensions_path.to_string_lossy()) {
        Ok(cfg) => {
            let config_source = match &cfg {
                BrokerConfig::Loaded(_) => ConfigSource::Loaded,
                BrokerConfig::Synthesized(_) => ConfigSource::Synthesized,
            };
            // The broker's own lines (bus connects, drops, 409s, handler warnings) go
            // to the app's log file, not just stderr (#414).
            let (agent, event_rx) =
                EmbeddedBroker::new(cfg.into_config(), Arc::new(crate::applog::BrokerLog));
            let agent = Arc::new(agent);
            Spine {
                minter: Some(agent.token_minter()),
                event_rx,
                responder: agent.responder(),
                runtime: BrokerRuntime {
                    config_source,
                    agent: Some(agent),
                    error: Mutex::new(None),
                    machine_cache: Mutex::new(None),
                },
            }
        }
        Err(e) => {
            // Malformed/invalid `extensions.yaml` — fail the broker CLOSED without
            // taking the app down. No minter, approvals dark; the error surfaces in
            // `broker.status` + the Preferences window.
            let msg = e.to_string();
            let (event_rx, responder) = empty_source();
            Spine {
                minter: None,
                event_rx,
                responder,
                runtime: BrokerRuntime {
                    config_source: ConfigSource::Error(msg.clone()),
                    agent: None,
                    error: Mutex::new(Some(msg)),
                    machine_cache: Mutex::new(None),
                },
            }
        }
    }
}

/// A closed event stream + no-op Responder: the Coordinator's feeder task ends at once,
/// but the actor lives on (fed by its own command channel), so prefs/approval reads keep
/// working — approvals are simply dark. The dropped sender is intentional.
fn empty_source() -> (mpsc::UnboundedReceiver<BrokerEvent>, ResponderRef) {
    let (_tx, rx) = mpsc::unbounded_channel();
    (rx, Arc::new(NoopResponder))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The status a failed config reports: the error, no broker, and no mode fields
    /// (the app has one broker, so there is no mode to report).
    #[test]
    fn failed_status_surfaces_the_config_error_and_has_no_mode_fields() {
        let rt = BrokerRuntime {
            config_source: ConfigSource::Error("bad policy".into()),
            agent: None,
            error: Mutex::new(Some("bad policy".into())),
            machine_cache: Mutex::new(None),
        };
        let v = rt.status();
        assert_eq!(v["config"]["source"], "error");
        assert_eq!(v["config"]["message"], "bad policy");
        assert_eq!(v["broker_error"], "bad policy");
        assert_eq!(v["servers"], json!([]));
        assert_eq!(v["machine"], Value::Null);
        let mut keys: Vec<&str> = v.as_object().unwrap().keys().map(String::as_str).collect();
        keys.sort_unstable();
        assert_eq!(
            keys,
            [
                "broker_error",
                "config",
                "gate_namespaces",
                "machine",
                "machine_error",
                "resolved_ssh_mode",
                "servers",
                "ssh",
            ]
        );
        assert_eq!(rt.describe(), "broker: config=error: bad policy");
        assert!(rt.broker_failed());
        assert!(!rt.retry_now(), "no broker to wake");
    }

    /// A config with the retired `desktop:` block fails the broker closed, and its
    /// status (what Preferences › Credential broker shows) carries the refusal.
    #[tokio::test]
    async fn a_desktop_block_fails_closed_with_its_message_in_the_status() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("extensions.yaml");
        std::fs::write(&path, "ssh:\n  mode: local-keys\ndesktop:\n  enabled: true\n").unwrap();
        let mut env = crate::env::Env::from_process();
        env.broker_extensions_path = path;
        let spine = build(&env);
        assert!(spine.minter.is_none(), "no broker on a refused config");
        let v = spine.runtime.status();
        let want = "broker config error: the `desktop:` block was removed; delete it \
                    (a `desktop.timeout_ms` becomes `approval_timeout` with a unit: \
                    30000 ms is `approval_timeout: 30s`)";
        assert_eq!(v["config"]["source"], "error");
        assert_eq!(v["config"]["message"], want);
        assert_eq!(v["broker_error"], want);
        assert!(spine.runtime.broker_failed());
    }

    /// A loaded config's status: the source is reported and nothing failed. The broker
    /// is built (never spawned), so nothing connects.
    #[tokio::test]
    async fn built_status_reports_the_config_source_and_no_mode_fields() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("extensions.yaml");
        std::fs::write(&path, "ssh:\n  mode: local-keys\n").unwrap();
        let mut env = crate::env::Env::from_process();
        env.broker_extensions_path = path;
        let spine = build(&env);
        assert!(spine.minter.is_some());
        let v = spine.runtime.status();
        assert_eq!(v["config"]["source"], "loaded");
        assert_eq!(v["broker_error"], Value::Null);
        for gone in ["effective_mode", "pref", "restart_required", "probe"] {
            assert!(v.get(gone).is_none(), "{gone} is still in {v}");
        }
        assert_eq!(spine.runtime.describe(), "broker: config=loaded");
    }
}
