//! Credential-broker mode selection + wiring (leg 3a.2).
//!
//! The app can broker credentials three ways (`shed_app::EffectiveMode`), chosen at
//! startup from the persisted `broker_mode` pref overlaid on a socket probe (§3.3):
//!
//! * **External** — a `shed-host-agent` desktop daemon is running: today's
//!   `HostAgentClient` over its UDS, unchanged (zero breakage for daemon users).
//! * **Embedded** — no daemon: start the in-process `EmbeddedHostAgent` (bus broker +
//!   approvals + minting), living and dying with the app.
//! * **HeadlessCoexist** — a deliberately-run *headless* daemon owns brokering: the app
//!   does NOT start a bus (no 409 fights) and gets no approvals; it keeps only the
//!   in-process token minter so its own secure-server reads still mint.
//!
//! [`build`] produces the mode-specific pieces the setup hook feeds into the shared
//! `Backend` (the minter — before `Backend`, launch-order per §3.2) and `Coordinator`
//! (the event stream + `Responder`), plus a [`BrokerRuntime`] that backs `broker.status`
//! / `identify.broker_mode` and carries the exit-drain handle. A broker that fails to
//! start (malformed config / ssh-resolve failure) fails CLOSED — surfaced in
//! `broker.status`, the app keeps running (deliberate divergence from the daemon, which
//! exits 1).

use std::sync::{Arc, Mutex};

use serde_json::{json, Value};
use tokio::sync::mpsc;

use shed_app::traits::{ClockRef, Responder, ResponderRef};
use shed_app::{
    load_or_synthesize, resolve_mode, AuthModeRegistry, BrokerConfig, EffectiveMode,
    EmbeddedHostAgent, HelloClientInfo, HostAgentClient, HostAgentEvent, HostAgentTokenMinter,
    ModePref, ModeProbe,
};
use shed_core::approval::{ApprovalDecision, DecidedBy};
use shed_core::token::TokenMinter;

use crate::env::Env;
use crate::prefs::{PrefsStore, SharedPrefs};

/// The mode-specific pieces the setup hook wires into `Backend` + `Coordinator`.
pub struct Spine {
    /// The Backend's secure-server minter (built BEFORE the Backend, §3.2). `None` when
    /// there's nothing to mint with (hermetic mock, or a broker that failed closed).
    pub minter: Option<Arc<dyn TokenMinter>>,
    /// The Coordinator's event stream (approval requests + audit rows, or a closed
    /// stream = approvals dark).
    pub event_rx: mpsc::UnboundedReceiver<HostAgentEvent>,
    /// The Coordinator's decision sink.
    pub responder: ResponderRef,
    /// The observability + lifecycle handle, managed in Tauri state.
    pub runtime: BrokerRuntime,
}

/// How the embedded broker got its config, for `broker.status`.
enum ConfigSource {
    /// External mode — no config loaded.
    NotApplicable,
    /// A user-authored `extensions.yaml` was loaded.
    Loaded,
    /// No `extensions.yaml` — the synthesized fresh-install default.
    Synthesized,
    /// `extensions.yaml` present but malformed/invalid (fail-closed; the message).
    Error(String),
}

/// The resolved broker mode + its evidence, plus the running embedded agent (if any).
/// Managed in Tauri state so `broker.status` / `identify` read it and the exit path
/// drains it. Immutable except `error` (set once, at [`start`](Self::start)).
///
/// The `pref` surfaced on the wire is read LIVE from `prefs` (the persisted store) at
/// request time, so a `broker.set_mode` is reflected immediately; `launch_pref` is the
/// frozen snapshot the app resolved `mode` from, used only to compute `restart_required`
/// (persisted pref drifted from the launch pref ⟹ a relaunch is needed to apply it).
pub struct BrokerRuntime {
    /// The pref the app LAUNCHED with — the snapshot `mode` was resolved from. Frozen;
    /// compared against the live persisted pref to derive `restart_required`.
    launch_pref: ModePref,
    /// The persisted prefs store, read live so the wire `pref` tracks a `set_mode`
    /// without a hot-swap (the effective `mode` stays put until relaunch).
    prefs: SharedPrefs,
    probe: ModeProbe,
    mode: EffectiveMode,
    config_source: ConfigSource,
    /// The embedded agent — `Some` in embedded + headless-coexist (config OK); `None`
    /// in external mode or when config failed closed. Held for `health()` /
    /// `resolved_ssh_mode()` reads and the shutdown handle.
    agent: Option<Arc<EmbeddedHostAgent>>,
    /// Embedded only: spawn the bus at [`start`](Self::start) (after the Coordinator is
    /// consuming). False for coexist (mint-only) + failed.
    should_spawn: bool,
    /// A broker-startup failure (config error or ssh-resolve failure at spawn) — the
    /// single "broker is broken" signal `broker.status` + the tray/Preferences read.
    error: Mutex<Option<String>>,
}

impl BrokerRuntime {
    /// Start the bus NOW that the Coordinator is consuming the bridge's event stream
    /// (embedded only; a no-op for external/coexist/failed). An ssh-resolve failure at
    /// spawn is recorded as a fail-closed error, NOT a crash (minting still works — the
    /// Backend already holds the minter).
    pub async fn start(&self) {
        if !self.should_spawn {
            return;
        }
        if let Some(agent) = &self.agent {
            if let Err(e) = agent.spawn().await {
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

    /// The effective mode's wire string.
    pub fn effective_mode_str(&self) -> &'static str {
        effective_str(self.mode)
    }

    /// The persisted pref, read LIVE from the store (parse-with-fallback-to-auto) — the
    /// value a `broker.set_mode` writes, reflected without a relaunch.
    fn persisted_pref(&self) -> ModePref {
        mode_pref(&self.prefs)
    }

    /// The persisted pref's wire string (the value the UI's mode Select reconciles to).
    pub fn pref_str(&self) -> &'static str {
        mode_pref_str(self.persisted_pref())
    }

    /// Whether the persisted pref has drifted from the pref the app launched with — a
    /// relaunch is needed to apply it (the effective `mode` is not hot-swapped). Setting
    /// the pref back to the launch value clears it.
    pub fn restart_required(&self) -> bool {
        self.persisted_pref() != self.launch_pref
    }

    /// The `identify.broker_mode` fragment (`{effective, pref, restart_required}`).
    pub fn identify_fragment(&self) -> Value {
        json!({
            "effective": self.effective_mode_str(),
            "pref": self.pref_str(),
            "restart_required": self.restart_required(),
        })
    }

    /// The `broker.status` snapshot: effective mode + pref + probe evidence + config
    /// provenance, and — in embedded mode — the resolved ssh mode, gate namespaces, and
    /// LiveStatus-shaped per-server health (per-namespace conn state incl. `Rejected`).
    pub fn status(&self) -> Value {
        let (config_source, config_message) = match &self.config_source {
            ConfigSource::NotApplicable => (Value::Null, Value::Null),
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
        json!({
            "effective_mode": self.effective_mode_str(),
            "pref": self.pref_str(),
            "restart_required": self.restart_required(),
            "probe": {
                "desktop_socket_live": self.probe.desktop_socket_live,
                "status_socket_live": self.probe.status_socket_live,
            },
            "config": { "source": config_source, "message": config_message },
            "resolved_ssh_mode": resolved_ssh_mode,
            "gate_namespaces": gate_namespaces,
            "servers": servers,
            "broker_error": self.error.lock().unwrap().clone().map_or(Value::Null, Value::from),
        })
    }
}

/// A no-op `Responder` for the modes with no approval channel (headless-coexist, and a
/// config-failed embedded broker): no approval requests ever arrive, so a decision never
/// needs delivering.
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

/// Build the mode-specific spine. Runs inside the Tauri tokio runtime (external's
/// `HostAgentClient::start` + `EmbeddedHostAgent::new` both `tokio::spawn`); the bus
/// itself starts later, via [`BrokerRuntime::start`], once the Coordinator is consuming.
///
/// `probe` is taken by the caller BEFORE entering async (bounded-blocking `connect(2)`s).
pub fn build(
    env: &Env,
    pref: ModePref,
    probe: ModeProbe,
    prefs: SharedPrefs,
    clock: ClockRef,
    modes: Arc<AuthModeRegistry>,
) -> Spine {
    let mode = resolve_mode(pref, probe).mode;
    match mode {
        EffectiveMode::External => build_external(env, pref, probe, prefs, clock, modes),
        EffectiveMode::Embedded => build_embedded(env, pref, probe, prefs, true),
        EffectiveMode::HeadlessCoexist => build_embedded(env, pref, probe, prefs, false),
    }
}

fn build_external(
    env: &Env,
    pref: ModePref,
    probe: ModeProbe,
    prefs: SharedPrefs,
    clock: ClockRef,
    modes: Arc<AuthModeRegistry>,
) -> Spine {
    let host = HostAgentClient::new(env.host_agent_socket.clone(), clock);
    // Minting is for real (non-mock) servers only — the hermetic mock is tokenless
    // (parity with the pre-3a.2 wiring).
    //
    // `with_modes` is what makes mtls work in EXTERNAL mode (plan 002 §7 P5):
    // the minter has to know which servers issue certificates so a mint that
    // begins before the agent's `hello_ack` lands neither downgrades to
    // `token.get` nor invents an "upgrade shed-host-agent" error. The embedded
    // modes need no such gate — their broker is this build (see
    // `EmbeddedTokenMinter::supports_mtls`) — so the registry reaches them only
    // as the Backend's observer.
    let minter: Option<Arc<dyn TokenMinter>> = env.mock_base_url.is_none().then(|| {
        Arc::new(HostAgentTokenMinter::new(host.clone()).with_modes(modes)) as Arc<dyn TokenMinter>
    });
    let event_rx = host.start(hello_info());
    let responder: ResponderRef = Arc::new(host);
    Spine {
        minter,
        event_rx,
        responder,
        runtime: BrokerRuntime {
            launch_pref: pref,
            prefs,
            probe,
            mode: EffectiveMode::External,
            config_source: ConfigSource::NotApplicable,
            agent: None,
            should_spawn: false,
            error: Mutex::new(None),
        },
    }
}

fn build_embedded(
    env: &Env,
    pref: ModePref,
    probe: ModeProbe,
    prefs: SharedPrefs,
    spawn: bool,
) -> Spine {
    let mode = if spawn {
        EffectiveMode::Embedded
    } else {
        EffectiveMode::HeadlessCoexist
    };
    match load_or_synthesize(&env.broker_extensions_path.to_string_lossy()) {
        Ok(cfg) => {
            let config_source = match &cfg {
                BrokerConfig::Loaded(_) => ConfigSource::Loaded,
                BrokerConfig::Synthesized(_) => ConfigSource::Synthesized,
            };
            let (agent, agent_rx) = EmbeddedHostAgent::new(cfg.into_config());
            let agent = Arc::new(agent);
            let minter = Some(agent.token_minter());
            let (event_rx, responder) = if spawn {
                // Embedded: the bridge's own event stream + Responder drive the
                // Coordinator (the bus starts in `BrokerRuntime::start`).
                (agent_rx, agent.responder())
            } else {
                // Headless-coexist: token-mint ONLY (§3.3). The headless daemon owns
                // brokering, so start no bus and take no approvals — the Coordinator
                // gets a closed stream + a no-op Responder (approvals dark). `agent_rx`
                // is dropped unused.
                empty_source()
            };
            Spine {
                minter,
                event_rx,
                responder,
                runtime: BrokerRuntime {
                    launch_pref: pref,
                    prefs,
                    probe,
                    mode,
                    config_source,
                    agent: Some(agent),
                    should_spawn: spawn,
                    error: Mutex::new(None),
                },
            }
        }
        Err(e) => {
            // Malformed/invalid `extensions.yaml` — fail the broker CLOSED without
            // taking the app down (the daemon would exit 1). No minter, approvals dark;
            // the error surfaces in `broker.status` + the Preferences window.
            let msg = e.to_string();
            let (event_rx, responder) = empty_source();
            Spine {
                minter: None,
                event_rx,
                responder,
                runtime: BrokerRuntime {
                    launch_pref: pref,
                    prefs,
                    probe,
                    mode,
                    config_source: ConfigSource::Error(msg.clone()),
                    agent: None,
                    should_spawn: false,
                    error: Mutex::new(Some(msg)),
                },
            }
        }
    }
}

/// A closed event stream + no-op Responder: the Coordinator's feeder task ends at once,
/// but the actor lives on (fed by its own command channel), so prefs/approval reads keep
/// working — approvals are simply dark. The dropped sender is intentional.
fn empty_source() -> (mpsc::UnboundedReceiver<HostAgentEvent>, ResponderRef) {
    let (_tx, rx) = mpsc::unbounded_channel();
    (rx, Arc::new(NoopResponder))
}

/// The external client's hello (parity with the pre-3a.2 wiring in `lib.rs`).
fn hello_info() -> HelloClientInfo {
    HelloClientInfo {
        name: "shed-desktop".to_string(),
        version: env!("CARGO_PKG_VERSION").to_string(),
        pid: std::process::id() as i32,
        capabilities: vec!["approval.ssh".to_string(), "event.stream".to_string()],
        replay_events: 50,
    }
}

/// The persisted `broker_mode` pref (default `auto`; an unparseable value → `auto`).
pub fn mode_pref(prefs: &PrefsStore) -> ModePref {
    prefs
        .get()
        .broker_mode
        .as_deref()
        .and_then(parse_mode_pref)
        .unwrap_or(ModePref::Auto)
}

/// Parse a `broker_mode` wire string; `None` for an unknown value.
pub fn parse_mode_pref(s: &str) -> Option<ModePref> {
    match s {
        "auto" => Some(ModePref::Auto),
        "embedded" => Some(ModePref::Embedded),
        "external" => Some(ModePref::External),
        _ => None,
    }
}

fn mode_pref_str(p: ModePref) -> &'static str {
    match p {
        ModePref::Auto => "auto",
        ModePref::Embedded => "embedded",
        ModePref::External => "external",
    }
}

fn effective_str(m: EffectiveMode) -> &'static str {
    match m {
        EffectiveMode::External => "external",
        EffectiveMode::Embedded => "embedded",
        EffectiveMode::HeadlessCoexist => "headless-coexist",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mode_pref_round_trips_and_falls_back() {
        assert_eq!(parse_mode_pref("auto"), Some(ModePref::Auto));
        assert_eq!(parse_mode_pref("embedded"), Some(ModePref::Embedded));
        assert_eq!(parse_mode_pref("external"), Some(ModePref::External));
        assert_eq!(parse_mode_pref("bogus"), None);
        assert_eq!(mode_pref_str(ModePref::Auto), "auto");
        assert_eq!(mode_pref_str(ModePref::Embedded), "embedded");
        assert_eq!(mode_pref_str(ModePref::External), "external");
    }

    #[test]
    fn effective_str_covers_all_modes() {
        assert_eq!(effective_str(EffectiveMode::External), "external");
        assert_eq!(effective_str(EffectiveMode::Embedded), "embedded");
        assert_eq!(effective_str(EffectiveMode::HeadlessCoexist), "headless-coexist");
    }

    fn probe(desktop: bool, status: bool) -> ModeProbe {
        ModeProbe {
            desktop_socket_live: desktop,
            status_socket_live: status,
        }
    }

    /// A throwaway `PrefsStore` on a unique temp path, optionally pre-seeded with a
    /// persisted `broker_mode` — the live source `pref`/`restart_required` read from.
    fn store(mode: Option<&str>) -> SharedPrefs {
        use std::sync::atomic::{AtomicU64, Ordering};
        static SEQ: AtomicU64 = AtomicU64::new(0);
        let n = SEQ.fetch_add(1, Ordering::Relaxed);
        let path = std::env::temp_dir()
            .join(format!("shed-broker-test-{}-{n}", std::process::id()))
            .join("prefs.json");
        let s: SharedPrefs = Arc::new(PrefsStore::load(path));
        if let Some(m) = mode {
            s.set_broker_mode(m.to_string());
        }
        s
    }

    #[test]
    fn external_runtime_status_reports_probe_and_no_config() {
        let rt = BrokerRuntime {
            launch_pref: ModePref::Auto,
            prefs: store(None),
            probe: probe(true, false),
            mode: EffectiveMode::External,
            config_source: ConfigSource::NotApplicable,
            agent: None,
            should_spawn: false,
            error: Mutex::new(None),
        };
        let v = rt.status();
        assert_eq!(v["effective_mode"], "external");
        assert_eq!(v["pref"], "auto");
        assert_eq!(v["restart_required"], false);
        assert_eq!(v["probe"]["desktop_socket_live"], true);
        assert_eq!(v["config"]["source"], Value::Null);
        assert_eq!(v["servers"], json!([]));
        assert_eq!(v["broker_error"], Value::Null);
        assert_eq!(
            rt.identify_fragment(),
            json!({"effective": "external", "pref": "auto", "restart_required": false})
        );
    }

    #[test]
    fn failed_embedded_status_surfaces_the_config_error() {
        let rt = BrokerRuntime {
            launch_pref: ModePref::Embedded,
            prefs: store(Some("embedded")),
            probe: probe(false, false),
            mode: EffectiveMode::Embedded,
            config_source: ConfigSource::Error("bad policy".into()),
            agent: None,
            should_spawn: false,
            error: Mutex::new(Some("bad policy".into())),
        };
        let v = rt.status();
        assert_eq!(v["effective_mode"], "embedded");
        assert_eq!(v["config"]["source"], "error");
        assert_eq!(v["config"]["message"], "bad policy");
        assert_eq!(v["broker_error"], "bad policy");
    }

    #[test]
    fn pref_and_restart_required_track_the_live_persisted_pref() {
        // Launch pref = auto; the effective mode stays put (no hot-swap) across a set.
        let prefs = store(None);
        let rt = BrokerRuntime {
            launch_pref: ModePref::Auto,
            prefs: prefs.clone(),
            probe: probe(false, false),
            mode: EffectiveMode::Embedded,
            config_source: ConfigSource::Synthesized,
            agent: None,
            should_spawn: false,
            error: Mutex::new(None),
        };
        // Persisted == launch ⟹ pref echoes it, no restart needed.
        assert_eq!(rt.pref_str(), "auto");
        assert!(!rt.restart_required());
        assert_eq!(rt.status()["restart_required"], false);
        assert_eq!(rt.identify_fragment()["restart_required"], false);

        // A live set drifts the persisted pref: pref tracks it, restart is now required,
        // while the effective mode is unchanged (deferred-apply contract).
        prefs.set_broker_mode("external".to_string());
        assert_eq!(rt.pref_str(), "external");
        assert!(rt.restart_required());
        assert_eq!(rt.status()["pref"], "external");
        assert_eq!(rt.status()["restart_required"], true);
        assert_eq!(rt.effective_mode_str(), "embedded");

        // Setting it BACK to the launch value clears the restart requirement.
        prefs.set_broker_mode("auto".to_string());
        assert_eq!(rt.pref_str(), "auto");
        assert!(!rt.restart_required());
    }
}
