//! The SSH credential minter — a faithful Rust port of `cmd/shed-host-agent/credmint.go`.
//!
//! [`CredentialMinter`] bootstraps a scoped token over a server's SSH `_bootstrap`
//! channel (via [`crate::bootstrap`]), after a fast-fail `known_hosts` pin pre-check.
//! [`CredentialSource`] is the `sdk.TokenProvider` analog: it caches a minted token and
//! re-mints on demand (near expiry, or after a 401 via [`CredentialSource::invalidate`])
//! and proactively ([`CredentialSource::refresh`]). A host-key pin mismatch is
//! **TERMINAL** — a possible MITM — so the source fails closed and never serves a token
//! for that server again.
//!
//! **Concurrency note:** Go uses a `sync.Mutex` + a goroutine + a `chan struct{}`
//! done-signal for single-flight. This port preserves the *behavioral* invariant —
//! one mint per burst of overlapping callers, all receiving that mint's own result;
//! the mint runs OFF the state lock; a terminal latch is never retried — using an
//! async `Minter::mint`, a `std::sync::Mutex` for the small state, a spawned mint task,
//! and a `tokio::sync::watch` join. The mutex guard is always dropped before awaiting
//! the join (Go releases `s.mu` before `<-call.done`).

use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ssh_key::known_hosts::{HostPatterns, KnownHosts};
use tokio::sync::watch;

use crate::bootstrap::{BootstrapRunner, Params, SystemSshRunner};
use crate::config::expand_tilde;
use crate::discovery::ServerTarget;
use crate::status::now_unix;

/// Token scopes the host-agent mints over SSH: `credentials` for its own bus
/// brokering, `control` for a `token.get` on the desktop's behalf (`credmint.go:48`).
/// `credentials` is the bus token provider's scope (wired by the supervisor's
/// credentials-scope `CredentialSource`); `control` is the egress side task + `token.get`.
pub const SCOPE_CREDENTIALS: &str = "credentials";
pub const SCOPE_CONTROL: &str = "control";

/// How long before expiry an on-demand token re-mints (`tokenRefreshWindow` = 2h).
const TOKEN_REFRESH_WINDOW: Duration = Duration::from_secs(2 * 3600);
/// Proactive-refresh delay before any token has been minted (`defaultRefreshDelay` = 1h).
const DEFAULT_REFRESH_DELAY: Duration = Duration::from_secs(3600);
const MIN_REFRESH_DELAY: Duration = Duration::from_secs(60);
const MAX_REFRESH_DELAY: Duration = Duration::from_secs(12 * 3600);
/// De-synchronizes a fleet re-minting together: the delay is spread ±this of its base.
const JITTER_FRACTION: f64 = 0.25;

/// A minted token plus its expiry as unix seconds (`None` = non-expiring / the server
/// returned none — Go's zero `time.Time`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Minted {
    pub token: String,
    pub expiry: Option<i64>,
}

/// A mint failure. `message` is the human string a caller surfaces (e.g. into
/// `token.response.error`); `terminal` marks the host-key-mismatch that must be latched
/// and never retried (the Rust analog of `errors.Is(err, ErrHostKeyMismatch)`).
#[derive(Debug, Clone)]
pub struct MinterError {
    message: String,
    terminal: bool,
}

impl MinterError {
    fn new(message: String) -> Self {
        Self {
            message,
            terminal: false,
        }
    }
    fn terminal(message: String) -> Self {
        Self {
            message,
            terminal: true,
        }
    }
    pub fn is_host_key_mismatch(&self) -> bool {
        self.terminal
    }
    pub fn message(&self) -> &str {
        &self.message
    }
}

impl std::fmt::Display for MinterError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

/// The subset of [`CredentialMinter`] that [`CredentialSource`] needs (mirror Go's
/// `minter` interface); a trait so tests can inject a fake without a live SSH server.
#[async_trait::async_trait]
pub trait Minter: Send + Sync {
    async fn mint(&self, target: &ServerTarget, scope: &str) -> Result<Minted, MinterError>;
}

/// Bootstraps the host-agent's own scoped token over a server's SSH `_bootstrap`
/// channel via the injectable [`BootstrapRunner`] (mirror `credmint.go:CredentialMinter`).
pub struct CredentialMinter {
    known_hosts_path: String,
    runner: Arc<dyn BootstrapRunner>,
}

impl CredentialMinter {
    /// Build a minter from the known_hosts file that pins server host keys
    /// (tilde-expanded). The SSH identity is resolved by the system ssh client, not
    /// read from a fixed key file.
    pub fn new(known_hosts_path: &str) -> Self {
        Self {
            known_hosts_path: expand_tilde(known_hosts_path),
            runner: Arc::new(SystemSshRunner::new()),
        }
    }

    /// A minter with an injected bootstrap runner (mirror Go's `bootstrapRun` field) —
    /// tests drive minting without spawning ssh.
    #[cfg(test)]
    fn with_runner(known_hosts_path: &str, runner: Arc<dyn BootstrapRunner>) -> Self {
        Self {
            known_hosts_path: expand_tilde(known_hosts_path),
            runner,
        }
    }
}

#[async_trait::async_trait]
impl Minter for CredentialMinter {
    /// Bootstrap a fresh token of `scope` for `target`. ssh verifies the server's host
    /// key against the pin already in known_hosts, so this never trusts an unpinned
    /// server. Mirrors `credmint.go:Mint`.
    async fn mint(&self, target: &ServerTarget, scope: &str) -> Result<Minted, MinterError> {
        // Pre-check the pin — NOT a safety latch (a missing pin is non-terminal
        // downstream), but it buys an actionable "run shed server add" error instead of
        // ssh's opaque "Host key verification failed", and skips a doomed ssh spawn.
        known_hosts_pinned(&self.known_hosts_path, &target.ssh_host, target.ssh_port)
            .map_err(MinterError::new)?;

        let params = Params {
            host: target.ssh_host.clone(),
            port: target.ssh_port,
            known_hosts_path: self.known_hosts_path.clone(),
            scope: scope.to_string(),
            client_kind: "host-agent".to_string(),
        };
        match self.runner.run(&params).await {
            Ok(bundle) => Ok(Minted {
                token: bundle.token,
                expiry: bundle.expires_at,
            }),
            Err(e) => {
                let terminal = e.is_host_key_mismatch();
                // Go: fmt.Errorf("bootstrapping %s token for %q: %w", scope, name, err).
                // Use an explicit quoted-name format (NOT {:?}) to byte-match Go's %q.
                let msg = format!("bootstrapping {scope} token for \"{}\": {e}", target.name);
                Err(if terminal {
                    MinterError::terminal(msg)
                } else {
                    MinterError::new(msg)
                })
            }
        }
    }
}

/// Report whether the known_hosts file has a usable (non-marker) host-key entry pinning
/// `host:port` — the same trust anchor `shed server add` wrote. A presence predicate:
/// ssh re-verifies the pin authoritatively during the exchange, so only existence
/// matters here. Returns `Ok(())` when one is present; an `Err(message)` when the file
/// is unreadable, unparseable, or has no entry. Mirrors `credmint.go:knownHostsPinned`
/// (via the vetted `ssh-key` known_hosts parser rather than a hand-rolled one).
fn known_hosts_pinned(known_hosts_path: &str, host: &str, port: u16) -> Result<(), String> {
    let data = std::fs::read_to_string(known_hosts_path)
        .map_err(|e| format!("reading known_hosts {known_hosts_path}: {e}"))?;
    // The stored form: "[host]:port" for a non-22 port, bare "host" for port 22 —
    // matching how OpenSSH (and shed) records it.
    let want = normalize(host, port);
    for entry in KnownHosts::new(&data) {
        let entry = entry.map_err(|e| format!("parsing known_hosts {known_hosts_path}: {e}"))?;
        // Skip marked lines: a @revoked key must never be a pin, a @cert-authority line
        // is a CA not a host-key pin (Go skips any non-empty marker).
        if entry.marker().is_some() {
            continue;
        }
        if let HostPatterns::Patterns(patterns) = entry.host_patterns() {
            if patterns.iter().any(|h| h == &want) {
                return Ok(());
            }
        }
    }
    Err(format!(
        "no host key pinned for {want} in {known_hosts_path} (run `shed server add` first)"
    ))
}

/// The `knownhosts.Normalize(net.JoinHostPort(host, port))` form: bare `host` for port
/// 22, else `[host]:port`.
fn normalize(host: &str, port: u16) -> String {
    if port == 22 {
        host.to_string()
    } else {
        format!("[{host}]:{port}")
    }
}

/// The mutable state of a [`CredentialSource`], guarded by a single mutex.
struct SourceState {
    token: String,
    expiry: Option<i64>,
    /// Latched host-key-mismatch error — once set, the source fails closed forever.
    terminal_err: Option<String>,
    /// The in-flight mint's join handle, if one is running (single-flight).
    inflight: Option<watch::Receiver<Option<Arc<MintOutcome>>>>,
}

/// The result a joined caller reads: `(token, expiry)` or the error message.
type MintOutcome = Result<(String, Option<i64>), String>;

/// An `sdk.TokenProvider` backed by the SSH credential minter (mirror
/// `credmint.go:credentialSource`). Construct via [`new_credential_source`]; hold it as
/// `Arc<CredentialSource>` (the obtain methods spawn the mint task, which needs an owned
/// clone).
pub struct CredentialSource {
    minter: Arc<dyn Minter>,
    target: ServerTarget,
    scope: String,
    state: Mutex<SourceState>,
}

/// Build a per-server credential source for `scope`.
pub fn new_credential_source(
    minter: Arc<dyn Minter>,
    target: ServerTarget,
    scope: &str,
) -> Arc<CredentialSource> {
    Arc::new(CredentialSource {
        minter,
        target,
        scope: scope.to_string(),
        state: Mutex::new(SourceState {
            token: String::new(),
            expiry: None,
            terminal_err: None,
            inflight: None,
        }),
    })
}

impl CredentialSource {
    /// The server this source mints for (used by the control-token provider to detect an
    /// endpoint change and recreate the source). The control-token provider is
    /// desktop-gated, so this is dead in a headless build.
    #[cfg_attr(not(feature = "desktop-forwarding"), allow(dead_code))]
    pub fn target(&self) -> &ServerTarget {
        &self.target
    }

    /// The current token, minting or re-minting as needed (implements the token half of
    /// `sdk.TokenProvider`). The egress SSE consumer is its first production consumer
    /// (via the `EgressTokenSource` bridge for `Arc<CredentialSource>` in `egress.rs`,
    /// behind `desktop-forwarding`): the control-scoped egress token is cached and
    /// re-minted, and cleared by [`Self::invalidate`] after a 401. The `token.get` control
    /// path instead uses [`Self::force_token_with_expiry`] (never a cached copy).
    pub async fn token(self: &Arc<Self>) -> Result<String, String> {
        self.obtain(false).await.map(|(tok, _)| tok)
    }

    /// Drop any completed cached token and mint fresh, while still coalescing callers
    /// that overlap a single in-flight mint. The control path (`token.get`) uses this: a
    /// restarted server silently invalidates control tokens, so a cached copy must never
    /// be served (mirror `credmint.go:forceTokenWithExpiry`). The `token.get` control path
    /// is desktop-gated, so this is dead in a headless build (the bus uses [`Self::token`]).
    #[cfg_attr(not(feature = "desktop-forwarding"), allow(dead_code))]
    pub async fn force_token_with_expiry(
        self: &Arc<Self>,
    ) -> Result<(String, Option<i64>), String> {
        self.obtain(true).await
    }

    /// Clear the cached token so the next obtain re-mints. Called after a 401 (the egress
    /// consumer's `EgressTokenSource::invalidate`, mirror Go `credentialSource.Invalidate`).
    pub fn invalidate(&self) {
        let mut st = self.state.lock().unwrap();
        st.token.clear();
    }

    /// Proactively re-mint, best-effort (errors surface on the next obtain). Driven by
    /// [`Self::refresh_loop`] so an idle server's token stays fresh.
    #[cfg(test)]
    pub async fn refresh(self: &Arc<Self>) {
        let _ = self.obtain_refresh().await;
    }

    /// The shared body of the obtain variants (mirror `obtainTokenWithExpiry`). Fails
    /// closed on the terminal error, then serves a fresh cached token or starts/joins a
    /// single-flight mint. `force` skips the cache short-circuit and clears any completed
    /// token so it is never served.
    async fn obtain(self: &Arc<Self>, force: bool) -> Result<(String, Option<i64>), String> {
        let rx = {
            let mut st = self.state.lock().unwrap();
            if let Some(err) = &st.terminal_err {
                return Err(err.clone());
            }
            if force {
                st.token.clear(); // force a re-mint; never serve a completed cached token
            } else if !st.token.is_empty() && !stale(st.expiry) {
                return Ok((st.token.clone(), st.expiry));
            }
            self.obtain_locked(&mut st)
        };
        // Await OFF the lock (Go releases s.mu before <-call.done).
        wait_outcome(rx).await
    }

    /// Always start/join a mint (no cache short-circuit and no completed-token clear) —
    /// the proactive-refresh path (mirror `credmint.go:refresh`, which calls
    /// `obtainLocked` directly). Shared by [`Self::refresh`] (tests) and
    /// [`Self::refresh_loop`] (the supervisor spawns the loop per secure server).
    async fn obtain_refresh(self: &Arc<Self>) -> Result<(String, Option<i64>), String> {
        let rx = {
            let mut st = self.state.lock().unwrap();
            if let Some(err) = &st.terminal_err {
                return Err(err.clone());
            }
            self.obtain_locked(&mut st)
        };
        wait_outcome(rx).await
    }

    /// Return the in-flight mint's receiver, starting one (spawned, off the lock) if none
    /// is running so N concurrent callers share ONE mint. Caller holds the state lock.
    fn obtain_locked(
        self: &Arc<Self>,
        st: &mut SourceState,
    ) -> watch::Receiver<Option<Arc<MintOutcome>>> {
        if let Some(rx) = &st.inflight {
            return rx.clone();
        }
        let (tx, rx) = watch::channel(None);
        st.inflight = Some(rx.clone());
        let this = Arc::clone(self);
        tokio::spawn(async move { this.do_mint(tx).await });
        rx
    }

    /// Perform the mint off the state lock, then store the result under it. A host-key
    /// pin mismatch is recorded as terminal (fail closed) so it is never retried.
    async fn do_mint(self: Arc<Self>, tx: watch::Sender<Option<Arc<MintOutcome>>>) {
        let result = self.minter.mint(&self.target, &self.scope).await;
        let outcome: MintOutcome = {
            let mut st = self.state.lock().unwrap();
            st.inflight = None;
            match result {
                Ok(minted) => {
                    st.token = minted.token.clone();
                    st.expiry = minted.expiry;
                    Ok((minted.token, minted.expiry))
                }
                Err(e) if e.is_host_key_mismatch() => {
                    // Go: fmt.Errorf("refusing to broker %q: SSH host key pin mismatch
                    // (possible MITM): %w", name, err). Explicit quoted-name (not {:?}).
                    let msg = format!(
                        "refusing to broker \"{}\": SSH host key pin mismatch (possible MITM): {}",
                        self.target.name,
                        e.message()
                    );
                    st.terminal_err = Some(msg.clone());
                    Err(msg)
                }
                Err(e) => Err(e.message().to_string()),
            }
        };
        // The joined callers read THIS call's outcome (not a re-read of shared state).
        let _ = tx.send(Some(Arc::new(outcome)));
    }

    /// Proactively re-mint at ~50% of the time until expiry (jittered), so an idle
    /// server's token stays fresh. Stops when `cancel` resolves. Spawned per secure server
    /// by the supervisor's group builder ([`crate::bus::spawn_server_group`]).
    pub async fn refresh_loop(self: Arc<Self>, mut cancel: watch::Receiver<bool>) {
        loop {
            let delay = self.next_refresh_delay();
            tokio::select! {
                _ = cancel.wait_for(|c| *c) => return,
                _ = tokio::time::sleep(delay) => {}
            }
            if *cancel.borrow() {
                return;
            }
            let _ = self.obtain_refresh().await;
        }
    }

    /// ~50% of the time until the cached token expires, jittered ±25%, clamped
    /// `[MIN_REFRESH_DELAY, MAX_REFRESH_DELAY]`; `DEFAULT_REFRESH_DELAY` when no token has
    /// been minted yet (mirror `nextRefreshDelay`).
    fn next_refresh_delay(&self) -> Duration {
        let expiry = self.state.lock().unwrap().expiry;
        apply_jitter_and_clamp(
            base_refresh_delay(expiry, now_unix()),
            random_jitter_fraction(),
        )
    }
}

/// Whether a cached token with the given expiry is within [`TOKEN_REFRESH_WINDOW`] of
/// expiry (a `None` expiry is a non-expiring token — never stale). Mirror `staleLocked`:
/// `!expiry.IsZero() && now >= expiry - window`.
fn stale(expiry: Option<i64>) -> bool {
    match expiry {
        None => false,
        Some(exp) => now_unix() >= exp - TOKEN_REFRESH_WINDOW.as_secs() as i64,
    }
}

/// The pre-jitter refresh base: `remaining/2` while the token is valid, `MIN_REFRESH_DELAY`
/// once expired, `DEFAULT_REFRESH_DELAY` before any mint (mirror the branch in
/// `nextRefreshDelay`).
fn base_refresh_delay(expiry: Option<i64>, now: i64) -> Duration {
    match expiry {
        None => DEFAULT_REFRESH_DELAY,
        Some(exp) => {
            let remaining = exp - now;
            if remaining > 0 {
                Duration::from_secs((remaining / 2) as u64)
            } else {
                MIN_REFRESH_DELAY
            }
        }
    }
}

/// Spread `base` by ±[`JITTER_FRACTION`] using `frac` ∈ [-1, 1], then clamp to
/// `[MIN_REFRESH_DELAY, MAX_REFRESH_DELAY]`.
fn apply_jitter_and_clamp(base: Duration, frac: f64) -> Duration {
    let base_s = base.as_secs_f64();
    let d = base_s + frac * JITTER_FRACTION * base_s;
    let clamped = d.clamp(
        MIN_REFRESH_DELAY.as_secs_f64(),
        MAX_REFRESH_DELAY.as_secs_f64(),
    );
    Duration::from_secs_f64(clamped)
}

/// A pseudo-random fraction in [-1, 1) for jitter (fleet de-synchronization; not
/// cryptographic). Derived from the sub-nanosecond wall clock to avoid a `rand` dep,
/// matching the crate's dependency-minimal posture.
fn random_jitter_fraction() -> f64 {
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.subsec_nanos())
        .unwrap_or(0);
    // (2*u - 1) is uniform in [-1, 1).
    2.0 * (nanos as f64 / 1_000_000_000.0) - 1.0
}

/// Await a single-flight mint's result off the state lock.
async fn wait_outcome(
    mut rx: watch::Receiver<Option<Arc<MintOutcome>>>,
) -> Result<(String, Option<i64>), String> {
    loop {
        {
            let guard = rx.borrow_and_update();
            if let Some(outcome) = guard.as_ref() {
                return (**outcome).clone();
            }
        }
        if rx.changed().await.is_err() {
            return Err("mint task ended unexpectedly".to_string());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bootstrap::{BootstrapError, Bundle};
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn target(name: &str, host: &str, port: u16) -> ServerTarget {
        ServerTarget {
            name: name.to_string(),
            url: String::new(),
            token: String::new(),
            tls_fingerprint: String::new(),
            ssh_host: host.to_string(),
            ssh_port: port,
        }
    }

    // ---- known_hosts_pinned + normalize (mirror credmint_test.go) -------------------

    fn write_known_hosts(lines: &str) -> (tempfile::TempDir, String) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("known_hosts");
        std::fs::write(&path, lines).unwrap();
        (dir, path.to_string_lossy().into_owned())
    }

    /// A syntactically-valid ssh-ed25519 known_hosts key blob (the committed harness
    /// test key's public part); the VALUE is irrelevant to the presence check.
    const ED25519_KEY: &str =
        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILMdrPP0NLsfgrc8JIa6OWX1qhgyfW/UwJTSXdRuUsJJ";

    #[test]
    fn normalize_bare_host_for_port_22() {
        assert_eq!(normalize("mini3", 22), "mini3");
        assert_eq!(normalize("mini3", 2222), "[mini3]:2222");
    }

    #[test]
    fn known_hosts_pinned_present() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_ok());
    }

    #[test]
    fn known_hosts_pinned_port_22_bare_host() {
        let (_d, path) = write_known_hosts(&format!("mini3 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 22).is_ok());
    }

    #[test]
    fn known_hosts_pinned_comma_host_list() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222,[alt]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "alt", 2222).is_ok());
    }

    #[test]
    fn known_hosts_pinned_accepts_ecdsa() {
        // Go's x/crypto ParseKnownHosts accepts ecdsa (and rsa/dsa) host keys, so the
        // Rust presence check must too (shed treats ecdsa as first-class; a host key
        // other than ed25519 must not fail the pin pre-check). Regression guard against
        // an ssh-key ed25519-only build, which would `Err("unknown algorithm")` here.
        const ECDSA: &str = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBOTs3y4oiuC0EFlPcY3mL1v7XtzII37Vtz9qXbbXlglHDWUbdPnfUZ0Z42cpLHYwAQSy8hOqmzOkD5EGrCXKuh4=";
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ECDSA}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_ok());
    }

    #[test]
    fn known_hosts_pinned_errors() {
        // Missing file.
        assert!(known_hosts_pinned("/nonexistent/known_hosts", "mini3", 2222).is_err());
        // Pins a different host.
        let (_d, path) = write_known_hosts(&format!("[other]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_err());
    }

    #[test]
    fn known_hosts_pinned_skips_revoked() {
        let (_d, path) = write_known_hosts(&format!("@revoked [mini3]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_err());
    }

    #[test]
    fn known_hosts_pinned_skips_cert_authority() {
        let (_d, path) =
            write_known_hosts(&format!("@cert-authority [mini3]:2222 {ED25519_KEY}\n"));
        assert!(known_hosts_pinned(&path, "mini3", 2222).is_err());
    }

    // ---- CredentialMinter::mint (mirror TestCredentialMinterMint) -------------------

    /// A BootstrapRunner returning a canned outcome and counting calls.
    struct FakeRunner {
        result: std::sync::Mutex<Vec<Result<Bundle, BootstrapErrorKind>>>,
        calls: AtomicUsize,
    }
    /// A cloneable stand-in for BootstrapError (which isn't Clone) for the fake's script.
    enum BootstrapErrorKind {
        Mismatch,
    }
    impl FakeRunner {
        fn ok(bundle: Bundle) -> Arc<Self> {
            Arc::new(Self {
                result: std::sync::Mutex::new(vec![Ok(bundle)]),
                calls: AtomicUsize::new(0),
            })
        }
        fn mismatch() -> Arc<Self> {
            Arc::new(Self {
                result: std::sync::Mutex::new(vec![Err(BootstrapErrorKind::Mismatch)]),
                calls: AtomicUsize::new(0),
            })
        }
    }
    #[async_trait::async_trait]
    impl BootstrapRunner for FakeRunner {
        async fn run(&self, _p: &Params) -> Result<Bundle, BootstrapError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let mut r = self.result.lock().unwrap();
            match r.remove(0) {
                Ok(b) => Ok(b),
                Err(BootstrapErrorKind::Mismatch) => {
                    Err(BootstrapError::HostKeyMismatch("changed".into()))
                }
            }
        }
    }

    fn bundle(token: &str) -> Bundle {
        Bundle {
            http_port: 8080,
            https_port: 0,
            tls_cert_fingerprint: String::new(),
            token: token.to_string(),
            scope: String::new(),
            token_id: "t1".into(),
            expires_at: Some(now_unix() + 3600),
        }
    }

    #[tokio::test]
    async fn mint_success_passes_params_and_returns_token() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let runner = FakeRunner::ok(bundle("tok"));
        let m = CredentialMinter::with_runner(&path, runner.clone());
        let minted = m
            .mint(&target("s", "mini3", 2222), SCOPE_CREDENTIALS)
            .await
            .unwrap();
        assert_eq!(minted.token, "tok");
        assert_eq!(runner.calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn mint_host_key_mismatch_is_terminal() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let m = CredentialMinter::with_runner(&path, FakeRunner::mismatch());
        let err = m
            .mint(&target("s", "mini3", 2222), SCOPE_CONTROL)
            .await
            .unwrap_err();
        assert!(err.is_host_key_mismatch());
        assert!(err
            .message()
            .starts_with("bootstrapping control token for \"s\":"));
    }

    #[tokio::test]
    async fn mint_missing_pin_is_non_terminal_and_skips_runner() {
        let (_d, path) = write_known_hosts(&format!("[mini3]:2222 {ED25519_KEY}\n"));
        let runner = FakeRunner::ok(bundle("x"));
        let m = CredentialMinter::with_runner(&path, runner.clone());
        // A different, unpinned server: the pre-check fails before the runner runs.
        let err = m
            .mint(&target("s", "unpinned", 1), SCOPE_CREDENTIALS)
            .await
            .unwrap_err();
        assert!(!err.is_host_key_mismatch());
        assert_eq!(runner.calls.load(Ordering::SeqCst), 0);
    }

    // ---- CredentialSource (mirror the credmint_test.go source tests) ----------------

    /// A `Minter` returning canned results in sequence (repeating the last) + call count.
    struct FakeMinter {
        results: std::sync::Mutex<Vec<Result<Minted, bool>>>, // Err(bool) = terminal?
        calls: AtomicUsize,
    }
    impl FakeMinter {
        fn new(results: Vec<Result<Minted, bool>>) -> Arc<Self> {
            Arc::new(Self {
                results: std::sync::Mutex::new(results),
                calls: AtomicUsize::new(0),
            })
        }
    }
    #[async_trait::async_trait]
    impl Minter for FakeMinter {
        async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
            let i = self.calls.fetch_add(1, Ordering::SeqCst);
            let results = self.results.lock().unwrap();
            let idx = i.min(results.len() - 1);
            match &results[idx] {
                Ok(m) => Ok(m.clone()),
                Err(true) => Err(MinterError::terminal("mismatch".into())),
                Err(false) => Err(MinterError::new("transient".into())),
            }
        }
    }

    fn minted(token: &str) -> Minted {
        Minted {
            token: token.to_string(),
            expiry: Some(now_unix() + 24 * 3600),
        }
    }

    #[tokio::test]
    async fn source_caches_and_re_mints_on_invalidate() {
        let fm = FakeMinter::new(vec![Ok(minted("tok1")), Ok(minted("tok2"))]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CREDENTIALS);
        assert_eq!(s.token().await.unwrap(), "tok1");
        assert_eq!(s.token().await.unwrap(), "tok1"); // served from cache
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1);
        s.invalidate();
        assert_eq!(s.token().await.unwrap(), "tok2"); // re-mint
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
    }

    // The egress slice un-gated `token()`/`invalidate()` from `#[cfg(test)]` to `pub`
    // (first production consumer). These two rows re-pin the cache + invalidate semantics
    // unguarded — the plan's BLOCKER-1 mapping rows.
    #[tokio::test]
    async fn credential_source_token_caches() {
        let fm = FakeMinter::new(vec![Ok(minted("tok1")), Ok(minted("tok2"))]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CONTROL);
        assert_eq!(s.token().await.unwrap(), "tok1");
        assert_eq!(s.token().await.unwrap(), "tok1"); // 2nd call served from cache
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1, "cached → one mint");
    }

    #[tokio::test]
    async fn credential_source_invalidate_forces_remint() {
        let fm = FakeMinter::new(vec![Ok(minted("tok1")), Ok(minted("tok2"))]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CONTROL);
        assert_eq!(s.token().await.unwrap(), "tok1");
        s.invalidate();
        assert_eq!(s.token().await.unwrap(), "tok2"); // next token() re-mints
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn source_pin_mismatch_is_terminal_no_retry() {
        let fm = FakeMinter::new(vec![Err(true)]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CREDENTIALS);
        assert!(s.token().await.is_err());
        assert!(s.token().await.is_err()); // persists, no re-mint
        assert_eq!(fm.calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn source_re_mints_near_expiry() {
        // Within the refresh window → the next obtain re-mints.
        let near = Minted {
            token: "near".into(),
            expiry: Some(now_unix() + (TOKEN_REFRESH_WINDOW.as_secs() as i64) / 2),
        };
        let fm = FakeMinter::new(vec![Ok(near), Ok(minted("fresh"))]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CREDENTIALS);
        assert_eq!(s.token().await.unwrap(), "near");
        assert_eq!(s.token().await.unwrap(), "fresh");
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn source_proactive_refresh() {
        let fm = FakeMinter::new(vec![Ok(minted("first")), Ok(minted("second"))]);
        let s = new_credential_source(fm.clone(), target("s", "", 0), SCOPE_CREDENTIALS);
        assert_eq!(s.token().await.unwrap(), "first");
        s.refresh().await; // proactive re-mint even though the cached token is valid
        assert_eq!(fm.calls.load(Ordering::SeqCst), 2);
        assert_eq!(s.token().await.unwrap(), "second");
    }

    /// A `Minter` that holds every mint open on a barrier so overlap can be forced.
    struct GatedMinter {
        calls: AtomicUsize,
        release: tokio::sync::Semaphore,
    }
    #[async_trait::async_trait]
    impl Minter for GatedMinter {
        async fn mint(&self, _t: &ServerTarget, _scope: &str) -> Result<Minted, MinterError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            let _p = self.release.acquire().await.unwrap();
            Ok(minted("tok"))
        }
    }

    #[tokio::test]
    async fn source_single_flight_collapses_concurrent_callers() {
        let gm = Arc::new(GatedMinter {
            calls: AtomicUsize::new(0),
            release: tokio::sync::Semaphore::new(0),
        });
        let s = new_credential_source(gm.clone(), target("s", "", 0), SCOPE_CREDENTIALS);

        let mut handles = Vec::new();
        for _ in 0..8 {
            let s = s.clone();
            handles.push(tokio::spawn(async move { s.token().await }));
        }
        // Let all 8 join the single in-flight mint, then release it.
        tokio::time::sleep(Duration::from_millis(50)).await;
        gm.release.add_permits(8);
        for h in handles {
            assert_eq!(h.await.unwrap().unwrap(), "tok");
        }
        assert_eq!(
            gm.calls.load(Ordering::SeqCst),
            1,
            "single-flight → one mint"
        );
    }

    // ---- refresh-delay math (no Go test; panel F6) ----------------------------------

    #[test]
    fn base_refresh_delay_branches() {
        let now = 1_000_000;
        // No token minted → default (1h).
        assert_eq!(base_refresh_delay(None, now), DEFAULT_REFRESH_DELAY);
        // Valid token → remaining/2.
        assert_eq!(
            base_refresh_delay(Some(now + 4 * 3600), now),
            Duration::from_secs(2 * 3600)
        );
        // Expired → min.
        assert_eq!(base_refresh_delay(Some(now - 10), now), MIN_REFRESH_DELAY);
    }

    #[test]
    fn jitter_and_clamp_bounds() {
        // A 1h base spread ±25% stays within [45m, 75m] and inside the global clamp.
        for frac in [-1.0, -0.5, 0.0, 0.5, 0.999] {
            let d = apply_jitter_and_clamp(Duration::from_secs(3600), frac);
            assert!(d >= MIN_REFRESH_DELAY && d <= MAX_REFRESH_DELAY);
            assert!(d >= Duration::from_secs(45 * 60) && d <= Duration::from_secs(75 * 60));
        }
        // A huge base clamps to the 12h max; a tiny base clamps to the 1m min.
        assert_eq!(
            apply_jitter_and_clamp(Duration::from_secs(48 * 3600), 0.0),
            MAX_REFRESH_DELAY
        );
        assert_eq!(
            apply_jitter_and_clamp(Duration::from_secs(1), -1.0),
            MIN_REFRESH_DELAY
        );
    }
}
