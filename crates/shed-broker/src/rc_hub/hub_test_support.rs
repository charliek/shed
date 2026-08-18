//! Shared doubles for the hub-core tests — the Rust mirror of
//! `hub_test.go:39-250`'s scripted runner (`hubTmux`), clock (`hubClock`),
//! env builder (`managedEnv`), hub factory (`newTestHub`), and SSE-frame
//! drain helpers, plus the cursor hook-event builder Go keeps package-level
//! (`hookEv`, `watch_cursor_test.go:23`). Test-only by construction.
//!
//! Go's test doubles are package-scoped, so every hub/cursor/ingest test file
//! shares ONE copy of each; this module is the Rust equivalent — reach for a
//! helper here rather than re-declaring it in a test module.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use chrono::{DateTime, TimeZone, Utc};
use shed_core::rc::RcKind;
use shed_core::rc_agents::{
    ENV_CREATED_AT, ENV_DISPLAY_NAME, ENV_ID, ENV_KIND, ENV_V, ENV_WORKDIR,
};
use shed_rc_engine::tmux::{TmuxResult, TmuxRunner};

use super::events::Subscriber;
use super::hub::{Hub, HubConfig};
use super::watch_cursor::CursorHookEvent;

/// The cursor spike capture's own conversation id (`cursorTestSessionID`,
/// `watch_cursor_test.go:20`).
pub(crate) const CURSOR_SID: &str = "4113a71f-0a42-4a6d-89b9-483e44b74103";

/// One pushed cursor hook event (`hookEv`, `watch_cursor_test.go:23`).
pub(crate) fn hook_ev(event: &str, payload: &str) -> CursorHookEvent {
    CursorHookEvent {
        event: event.to_string(),
        payload: payload.as_bytes().to_vec(),
    }
}

/// A programmable clock (`hubClock`, `hub_test.go:21`).
pub(crate) struct HubClock {
    t: Mutex<DateTime<Utc>>,
}

impl HubClock {
    pub fn new() -> Arc<HubClock> {
        Arc::new(HubClock {
            t: Mutex::new(Utc.timestamp_opt(1_700_000_000, 0).unwrap()),
        })
    }

    pub fn now(&self) -> DateTime<Utc> {
        *self.t.lock().unwrap_or_else(PoisonError::into_inner)
    }

    pub fn advance(&self, d: Duration) {
        let mut t = self.t.lock().unwrap_or_else(PoisonError::into_inner);
        *t += chrono::Duration::from_std(d).expect("test duration in range");
    }
}

#[derive(Default)]
struct HubTmuxState {
    names: Vec<String>,
    panes: HashMap<String, String>,
    /// tmux name → visible-frame capture (no -S); unset ⇒ same as panes.
    visible: HashMap<String, String>,
    envs: HashMap<String, String>,
    gone: HashSet<String>,
    /// capture-pane fails TRANSIENTLY (not gone).
    flaky: HashSet<String>,
    /// non-empty → `ls` fails transiently with this stderr.
    ls_fail: String,
    /// Recorded delivery payloads (send-keys -l / set-buffer text).
    sent: Vec<String>,
    /// Recorded set-environment "KEY=VALUE" pairs.
    set_envs: Vec<String>,
}

/// A programmable tmux runner for the hub (`hubTmux`, `hub_test.go:43`): `ls`
/// returns the configured session names, and capture-pane/show-environment
/// answer per-session from maps keyed by tmux session name. Safe for
/// concurrent use.
#[derive(Default)]
pub(crate) struct HubTmux {
    inner: Mutex<HubTmuxState>,
}

impl HubTmux {
    pub fn new() -> Arc<HubTmux> {
        Arc::new(HubTmux::default())
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, HubTmuxState> {
        self.inner.lock().unwrap_or_else(PoisonError::into_inner)
    }

    pub fn set(&self, name: &str, pane: &str, env: &str) {
        let mut st = self.lock();
        if !st.names.iter().any(|n| n == name) {
            st.names.push(name.to_string());
        }
        st.panes.insert(name.to_string(), pane.to_string());
        st.envs.insert(name.to_string(), env.to_string());
        st.gone.remove(name);
    }

    pub fn remove(&self, name: &str) {
        let mut st = self.lock();
        st.names.retain(|n| n != name);
        st.gone.insert(name.to_string());
    }

    pub fn set_pane(&self, name: &str, pane: &str) {
        self.lock().panes.insert(name.to_string(), pane.to_string());
    }

    /// Pins what a VISIBLE-frame capture (no -S) answers, leaving the
    /// scrollback capture untouched. Clearing (`""`) restores
    /// "visible == scrollback".
    pub fn set_visible(&self, name: &str, vis: &str) {
        let mut st = self.lock();
        if vis.is_empty() {
            st.visible.remove(name);
        } else {
            st.visible.insert(name.to_string(), vis.to_string());
        }
    }

    /// Makes `ls` fail transiently (stderr must not read as "no server").
    pub fn set_ls_fail(&self, stderr: &str) {
        self.lock().ls_fail = stderr.to_string();
    }

    /// Makes a session's capture-pane fail transiently (a hiccup, NOT gone).
    pub fn set_flaky(&self, name: &str, flaky: bool) {
        let mut st = self.lock();
        if flaky {
            st.flaky.insert(name.to_string());
        } else {
            st.flaky.remove(name);
        }
    }

    pub fn recorded(&self) -> Vec<String> {
        self.lock().sent.clone()
    }

    /// The recorded set-environment "KEY=VALUE" pairs (asserts the
    /// SHED_RC_AGENT_SESSION back-write).
    pub fn set_env_calls(&self) -> Vec<String> {
        self.lock().set_envs.clone()
    }
}

fn target_of(args: &[&str]) -> String {
    args.iter()
        .position(|a| *a == "-t")
        .and_then(|i| args.get(i + 1))
        .map(|s| (*s).to_string())
        .unwrap_or_default()
}

impl TmuxRunner for HubTmux {
    fn run(&self, args: &[&str]) -> TmuxResult {
        let mut st = self.lock();
        let ok = |stdout: String| TmuxResult {
            stdout,
            stderr: String::new(),
            code: 0,
        };
        match args.first().copied() {
            Some("ls") => {
                if !st.ls_fail.is_empty() {
                    return TmuxResult {
                        stdout: String::new(),
                        stderr: st.ls_fail.clone(),
                        code: 1,
                    };
                }
                ok(st.names.join("\n"))
            }
            Some("capture-pane") => {
                let name = target_of(args);
                if st.gone.contains(&name) {
                    return TmuxResult {
                        stdout: String::new(),
                        stderr: format!("can't find pane: {name}"),
                        code: 1,
                    };
                }
                if st.flaky.contains(&name) {
                    return TmuxResult {
                        stdout: String::new(),
                        stderr: "lost server connection (transient)".to_string(),
                        code: 1,
                    };
                }
                // Real tmux answers a VISIBLE-frame capture (no -S)
                // differently from a scrollback one — the seam the
                // ApprovalAnchor path depends on.
                if !args.contains(&"-S") {
                    if let Some(vis) = st.visible.get(&name) {
                        return ok(vis.clone());
                    }
                }
                ok(st.panes.get(&name).cloned().unwrap_or_default())
            }
            Some("show-environment") => {
                let name = target_of(args);
                ok(st.envs.get(&name).cloned().unwrap_or_default())
            }
            Some("set-environment") => {
                // set-environment -t <name> <KEY> <VALUE>: record KEY=VALUE
                // and apply it to the env dump so a later show-environment
                // reads it back (realistic).
                let name = target_of(args);
                if let Some(ti) = args.iter().position(|a| *a == "-t") {
                    if let (Some(key), Some(value)) = (args.get(ti + 2), args.get(ti + 3)) {
                        let pair = format!("{key}={value}");
                        st.set_envs.push(pair.clone());
                        let entry = st.envs.entry(name).or_default();
                        entry.push_str(&pair);
                        entry.push('\n');
                    }
                }
                ok(String::new())
            }
            Some("send-keys") => {
                // Record the literal text of a `send-keys -l -- <text>`
                // delivery; the bare Enter submit is ignored.
                if args.contains(&"-l") {
                    if let Some(i) = args.iter().position(|a| *a == "--") {
                        if let Some(text) = args.get(i + 1) {
                            st.sent.push((*text).to_string());
                        }
                    }
                }
                ok(String::new())
            }
            Some("set-buffer") => {
                // The multi-line bracketed-paste path loads a buffer first.
                if let Some(i) = args.iter().position(|a| *a == "--") {
                    if let Some(text) = args.get(i + 1) {
                        st.sent.push((*text).to_string());
                    }
                }
                ok(String::new())
            }
            Some("paste-buffer") => ok(String::new()),
            _ => ok(String::new()),
        }
    }
}

/// A show-environment dump for a managed session of the given kind
/// (`managedEnv`, `hub_test.go:229`).
pub(crate) fn managed_env(id: &str, kind: &RcKind) -> String {
    format!(
        "{ENV_V}=2\n{ENV_ID}={id}\n{ENV_KIND}={}\n{ENV_DISPLAY_NAME}=disp\n{ENV_WORKDIR}=/home/shed\n",
        kind.as_str()
    )
}

/// A managed dump with created_at but NO id — the legacy/partial-creator
/// shape (`legacyEnv`, `hub_test.go:526`).
pub(crate) fn legacy_env(kind: &RcKind, created_at: &str) -> String {
    format!(
        "{ENV_V}=2\n{ENV_KIND}={}\n{ENV_CREATED_AT}={created_at}\n",
        kind.as_str()
    )
}

/// The Go-minimal hub config: the four required seams wired to the fakes, every
/// tuning field left ZERO so `resolve()` supplies the production default —
/// exactly the shape Go's tests write inline (`newHub(HubConfig{Runner, Getenv,
/// Now, Logf, …})`). Spread it to override only what a test cares about:
/// `HubConfig { subscriber_buffer: 4, ..hub_config(&f, &clk) }`.
pub(crate) fn hub_config(f: &Arc<HubTmux>, clk: &Arc<HubClock>) -> HubConfig {
    let clk = Arc::clone(clk);
    HubConfig {
        runner: Arc::clone(f) as _,
        getenv: Arc::new(|_| String::new()),
        now: Some(Arc::new(move || clk.now())),
        logf: Some(Arc::new(|_| {})),
        addr: String::new(),
        version: String::new(),
        active_interval: Duration::ZERO,
        idle_interval: Duration::ZERO,
        quiet_period: Duration::ZERO,
        idle_timeout: Duration::ZERO,
        heartbeat: Duration::ZERO,
        write_timeout: Duration::ZERO,
        subscriber_buffer: 0,
        // Go's tests zero the sendLineSettle global; the config seam is the
        // Rust spelling.
        send_line_settle: Some(Duration::ZERO),
    }
}

/// A hub wired to the fake tmux + clock and small intervals (`newTestHub`,
/// `hub_test.go:240`) — the three tuning overrides Go pins, on top of
/// [`hub_config`]'s defaults.
pub(crate) fn new_test_hub(f: &Arc<HubTmux>, clk: &Arc<HubClock>) -> Hub {
    Hub::new(HubConfig {
        quiet_period: Duration::from_secs(4),
        heartbeat: Duration::from_millis(20),
        write_timeout: Duration::from_secs(1),
        ..hub_config(f, clk)
    })
}

/// The full test rig — the fake tmux + clock + a hub wired to both. Collapses
/// the three-line preamble Go writes at the top of every hub test
/// (`newHubTmux` / `&hubClock{…}` / `newTestHub`).
pub(crate) fn rig() -> (Hub, Arc<HubTmux>, Arc<HubClock>) {
    let f = HubTmux::new();
    let clk = HubClock::new();
    let h = new_test_hub(&f, &clk);
    (h, f, clk)
}

/// A hub over throwaway doubles, for the tests that drive it purely through
/// arguments (the input gate) and never script tmux or time.
pub(crate) fn test_hub() -> Hub {
    rig().0
}

/// A Go-minimal config over an arbitrary runner (the `hookedTmux` wrapper
/// tests need a runner that is not a bare `HubTmux`).
pub(crate) fn hub_config_with_runner(
    runner: Arc<dyn shed_rc_engine::tmux::TmuxRunner + Send + Sync>,
    now: impl Fn() -> DateTime<Utc> + Send + Sync + 'static,
) -> HubConfig {
    HubConfig {
        runner,
        getenv: Arc::new(|_| String::new()),
        now: Some(Arc::new(now)),
        logf: Some(Arc::new(|_| {})),
        addr: String::new(),
        version: String::new(),
        active_interval: Duration::ZERO,
        idle_interval: Duration::ZERO,
        quiet_period: Duration::ZERO,
        idle_timeout: Duration::ZERO,
        heartbeat: Duration::ZERO,
        write_timeout: Duration::ZERO,
        subscriber_buffer: 0,
        send_line_settle: Some(Duration::ZERO),
    }
}

/// A decoded SSE frame (`drainedEvent`, `hub_test.go:253`).
#[derive(Debug)]
pub(crate) struct DrainedEvent {
    pub name: String,
    pub raw: String,
}

/// Decodes every buffered frame on a subscriber (`drainEvents`,
/// `hub_test.go:259`).
pub(crate) fn drain_events(sub: &Subscriber) -> Vec<DrainedEvent> {
    let mut out = Vec::new();
    while let Some(frame) = sub.try_recv() {
        let s = String::from_utf8_lossy(&frame);
        let mut name = String::new();
        let mut raw = String::new();
        for line in s.split('\n') {
            if let Some(rest) = line.strip_prefix("event: ") {
                name = rest.to_string();
            }
            if let Some(rest) = line.strip_prefix("data: ") {
                raw = rest.to_string();
            }
        }
        out.push(DrainedEvent { name, raw });
    }
    out
}

pub(crate) fn count_events(evs: &[DrainedEvent], name: &str) -> usize {
    evs.iter().filter(|e| e.name == name).count()
}

/// A pane fixture by basename, from the byte-parity-swept copies under
/// `crates/fixtures/panes/` (`paneFixture`, `hub_pane_approvals_test.go:20`).
pub(crate) fn pane_fixture(name: &str) -> String {
    let path = format!(
        "{}/../fixtures/panes/{name}.txt",
        env!("CARGO_MANIFEST_DIR")
    );
    std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("fixture {path}: {e}"))
}

// ---------------------------------------------------------------------------
// Shared scripted watchers (`stubWatcher`/`stubApprovalWatcher`,
// hub_input_test.go:302-332) — used by the input-gate unit tests (hub.rs) and
// the HTTP mirrors (hub_http_tests.rs).
// ---------------------------------------------------------------------------

use chrono::DateTime as ChronoDateTime;
use shed_core::rc::RcActivity;

use super::messages::{FeedApproval, FeedMessage};
use super::watch::{ApprovalBlocker, ApprovalPublisher, SessionWatcher};

/// A scripted watcher verdict (`stubWatcher`, `hub_input_test.go:302`).
pub(crate) struct StubWatcher {
    pub activity: RcActivity,
    pub message: String,
    pub fresh: bool,
    pub expired_working: bool,
}

impl Default for StubWatcher {
    fn default() -> StubWatcher {
        StubWatcher {
            // Go's zero Activity ("") maps to Unknown (the H4 decision).
            activity: RcActivity::Unknown,
            message: String::new(),
            fresh: false,
            expired_working: false,
        }
    }
}

impl SessionWatcher for StubWatcher {
    fn refresh(&self, _now: ChronoDateTime<Utc>) {}
    fn snapshot(&self, _now: ChronoDateTime<Utc>) -> (RcActivity, String, bool, bool) {
        (
            self.activity,
            self.message.clone(),
            self.fresh,
            self.expired_working,
        )
    }
    fn drain_pending(&self) -> Vec<FeedMessage> {
        Vec::new()
    }
    fn had_event(&self) -> bool {
        true
    }
    fn close(&self) {}
}

/// `stubApprovalWatcher` (`hub_input_test.go:322`): adds the snapshot
/// reconcile publishes and the blocked-on-a-dialog question the input gate
/// asks. `blocked` models an open ask NOT in the snapshot — an opencode
/// question — so the two can be driven apart.
#[derive(Default)]
pub(crate) struct StubApprovalWatcher {
    pub stub: StubWatcher,
    pub approvals: Vec<FeedApproval>,
    pub blocked: bool,
}

impl SessionWatcher for StubApprovalWatcher {
    fn refresh(&self, now: ChronoDateTime<Utc>) {
        self.stub.refresh(now);
    }
    fn snapshot(&self, now: ChronoDateTime<Utc>) -> (RcActivity, String, bool, bool) {
        self.stub.snapshot(now)
    }
    fn drain_pending(&self) -> Vec<FeedMessage> {
        Vec::new()
    }
    fn had_event(&self) -> bool {
        true
    }
    fn close(&self) {}
    fn as_approval_publisher(&self) -> Option<&dyn ApprovalPublisher> {
        Some(self)
    }
    fn as_approval_blocker(&self) -> Option<&dyn ApprovalBlocker> {
        Some(self)
    }
}

impl ApprovalPublisher for StubApprovalWatcher {
    fn pending_approvals(&self) -> Vec<FeedApproval> {
        self.approvals.clone()
    }
}
impl ApprovalBlocker for StubApprovalWatcher {
    fn has_open_approvals(&self) -> bool {
        self.blocked || !self.approvals.is_empty()
    }
}

// ---------------------------------------------------------------------------
// HTTP-level helpers (the httptest.NewServer(h.handler()) idiom).
// ---------------------------------------------------------------------------

/// Serves the hub's router on an ephemeral loopback port. Serving stops when
/// the test's runtime is dropped — DROPPING the returned handle only detaches
/// the task, it does not abort it, so a test that wants to stop the server
/// early must `.abort()` the handle itself.
pub(crate) async fn serve_hub(hub: &Arc<Hub>) -> (String, tokio::task::JoinHandle<()>) {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind ephemeral");
    let addr = listener.local_addr().expect("addr");
    let h = Arc::clone(hub);
    let handle = tokio::spawn(async move {
        let _ = super::hub::serve(h, listener).await;
    });
    (format!("http://{addr}"), handle)
}

/// The shared loopback client (`verbClient` — never follows redirects, so a
/// mux-level redirect surfaces as itself). ONE client for the whole suite: a
/// fresh `reqwest::Client` per call would build a connection pool + TLS config
/// per request.
static CLIENT: std::sync::LazyLock<reqwest::Client> = std::sync::LazyLock::new(|| {
    reqwest::Client::builder()
        .no_proxy()
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .expect("client")
});

pub(crate) fn http_client() -> reqwest::Client {
    CLIENT.clone() // cheap: reqwest::Client is an Arc handle
}

/// One request, no body for an empty string (`doRequest`,
/// `hub_verbs_test.go:61`).
pub(crate) async fn do_request(
    client: &reqwest::Client,
    method: reqwest::Method,
    url: &str,
    body: &str,
) -> reqwest::Response {
    let mut req = client
        .request(method, url)
        .header("Content-Type", "application/json");
    if !body.is_empty() {
        req = req.body(body.to_string());
    }
    req.send().await.expect("request")
}

/// A raw, blocking loopback POST that sends the request line VERBATIM — for
/// the two cases a reqwest request cannot express: a path reqwest's `Url`
/// parser would rewrite client-side (it resolves `%2E%2E` dot segments, where
/// Go's `net/url` keeps them escaped), and a POST issued from INSIDE the sync
/// reconcile pass (a reqwest client would need the async runtime that thread
/// is blocking). Returns the status code.
pub(crate) fn raw_post(authority: &str, path_and_query: &str, body: &str) -> u16 {
    use std::io::{Read, Write};
    let mut conn = std::net::TcpStream::connect(authority).expect("connect");
    let req = format!(
        "POST {path_and_query} HTTP/1.1\r\nHost: hub\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    conn.write_all(req.as_bytes()).expect("write");
    let mut buf = Vec::new();
    let _ = conn.read_to_end(&mut buf);
    let head = String::from_utf8_lossy(&buf);
    head.split_whitespace()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(0)
}

/// Asserts the hub's `{error, message}` envelope (`wantEnvelope`,
/// `hub_verbs_test.go:101`): status + error code, a non-empty human message,
/// and EXACTLY those two keys.
pub(crate) async fn want_envelope(resp: reqwest::Response, status: u16, code: &str) {
    let got_status = resp.status().as_u16();
    let body = resp.text().await.expect("body");
    let env: std::collections::HashMap<String, String> = serde_json::from_str(&body)
        .unwrap_or_else(|e| {
            panic!("status {got_status} body is not the JSON error envelope: {e} ({body})")
        });
    assert_eq!(got_status, status, "status (body {body})");
    assert_eq!(
        env.get("error").map(String::as_str),
        Some(code),
        "error code (body {body})"
    );
    assert!(
        env.get("message").is_some_and(|m| !m.is_empty()),
        "envelope must carry a human message: {body}"
    );
    assert_eq!(
        env.len(),
        2,
        "envelope must be exactly {{error, message}}: {body}"
    );
}

/// A managed opencode session's env plus the two keys the watcher needs
/// (`opencodeVerbEnv`, `hub_verbs_test.go:506`): the create-time embedded
/// server port, and (optionally) a prior back-written agent session id so the
/// watcher is PINNED synchronously at construction.
pub(crate) fn opencode_verb_env(id: &str, port: u16, agent_session: &str) -> String {
    let mut env = format!(
        "{}{}={port}\n",
        managed_env(id, &RcKind::Opencode),
        shed_core::rc_agents::ENV_OPENCODE_PORT
    );
    if !agent_session.is_empty() {
        env.push_str(&format!(
            "{}={agent_session}\n",
            shed_core::rc_agents::ENV_AGENT_SESSION
        ));
    }
    env
}

/// `opencodeReadyPane` (`hub_input_test.go:19`) — parked at the opencode
/// composer placeholder, so a gate rejection can only come from an activity
/// arm.
pub(crate) fn opencode_ready_pane() -> String {
    "opencode\n> Ask anything...".to_string()
}

/// `codexReadyPane` (`hub_input_test.go:15`).
pub(crate) fn codex_ready_pane() -> String {
    "codex\n> Find and fix a bug in @filename".to_string()
}

/// `waitFor` (`hub_test.go:864`) — async polling variant.
pub(crate) async fn wait_for(what: &str, mut cond: impl FnMut() -> bool) {
    let deadline = std::time::Instant::now() + Duration::from_secs(2);
    while std::time::Instant::now() < deadline {
        if cond() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    panic!("condition not met within timeout: {what}");
}
