//! Control-token FSM — ported from Swift's `ControlTokenProvider` actor, with
//! the mobile client's FSM extensions ported from
//! `shed-mobile/lib/control/control_token_provider.dart` (itself a faithful
//! port of the orchestrator's controlToken.ts).
//!
//! Caches a shed-server CONTROL token, refreshing it near expiry or on demand
//! (`invalidate*`, called on a 401). The mint primitive is a foreign
//! `TokenMinter` (the host agent, in Swift; a mock in tests) — this crate owns
//! only the cache/refresh/single-flight logic, so it stays pure.
//!
//! FSM extensions (all builder-opted, defaults preserve the pre-existing
//! desktop behavior — plan 001 §3.4):
//!   * a tunable refresh window ([`ControlTokenProvider::with_refresh_window`])
//!   * deterministic per-name refresh jitter ([`name_jitter`],
//!     [`ControlTokenProvider::with_name_jitter`])
//!   * a mint-failure cooldown ([`ControlTokenProvider::with_mint_cooldown`])
//!   * a persisted seed token ([`ControlTokenProvider::with_seed`])
//!   * an injectable clock ([`ControlTokenProvider::with_now`])
//!
//! Two unconditional semantics ported as strict improvements (§3.4):
//!   * keep-valid-on-proactive-failure — a failed refresh mint inside the
//!     refresh window returns the still-valid cached token instead of erroring
//!     (`control_token_provider.dart:82-92`)
//!   * the stale-401 guard — [`ControlTokenProvider::invalidate_if_current`]
//!     ignores a 401 for a token already rotated past
//!     (`control_token_provider.dart:99-105`)
//!
//! Fail-closed contract (mirrors the SDK/CLI, guarded by the tests here since
//! test mode drops the token path so e2e can't reach it): a mint failure with
//! no still-valid cached token yields an error and the client then sends NO
//! token — never a static downgrade.

use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio::sync::Mutex;

use crate::http::ShedError;

/// A minted control token plus its optional expiry (unix seconds). `None` expiry
/// → only an explicit `invalidate*()` forces a refresh (mirrors `MintedToken`).
/// Swift parses the host agent's ISO-8601 expiry to epoch before handing it over,
/// keeping timestamp parsing off this crate.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MintedToken {
    pub token: String,
    pub expires_at_unix: Option<u64>,
}

/// The mint primitive: request a fresh CONTROL token for `server`. Implemented
/// by the foreign host-agent bridge (Swift) or a test mock. A failure (Err) is
/// fail-closed — the provider surfaces it and the caller sends no token.
#[async_trait::async_trait]
pub trait TokenMinter: Send + Sync {
    async fn mint(&self, server: &str) -> Result<MintedToken, ShedError>;
}

/// The default 2h refresh window mirrors the SDK/CLI: refresh this long before
/// expiry so routine requests rarely race a 401. (Mobile passes its own 2h5m
/// via [`ControlTokenProvider::with_refresh_window`] — per-client parity, not
/// forced convergence.)
const REFRESH_WINDOW: Duration = Duration::from_secs(2 * 60 * 60);

/// The PERSISTED mint-failure message, served on later cooldown-blocked
/// calls. Deliberately FIXED/redacted: a [`TokenMinter`] error's Display text
/// can embed transport detail or even token material, which must never be
/// stored and replayed to later callers. The immediate caller of the failing
/// mint still receives the minter's real error (matching the pre-existing
/// propagation and Dart's rethrow, `control_token_provider.dart:149`).
const MINT_FAILED_REDACTED: &str = "control token mint failed";

/// Deterministic per-name jitter in `[0, max_ms)` — stable across restarts, no
/// RNG. Ported exactly from mobile's `nameJitter`
/// (`control_token_provider.dart:175-181`), which mirrors controlToken.ts (a
/// 32-bit signed hash like JS `|0`): iterate the name's UTF-16 CODE UNITS
/// (Dart `codeUnitAt` parity — a non-BMP char such as an emoji contributes its
/// surrogate PAIR, two units), fold `h = h*31 + cu` wrapped to 32-bit signed
/// at every step, then take `unsigned_abs()` (`i32::MIN`-safe where Dart's
/// 64-bit `abs()` is trivially safe and a Rust `abs()` would overflow) modulo
/// `max(max_ms, 1)`. `max_ms == 0` therefore always yields 0 (jitter off).
pub fn name_jitter(name: &str, max_ms: u64) -> u64 {
    let mut h: i32 = 0;
    for cu in name.encode_utf16() {
        // Dart computes `h * 31 + cu` in 64-bit then `.toSigned(32)`; a
        // stepwise 32-bit wrapping mul+add is identical (truncation mod 2^32
        // distributes over * and +).
        h = h.wrapping_mul(31).wrapping_add(i32::from(cu));
    }
    u64::from(h.unsigned_abs()) % max_ms.max(1)
}

/// The provider's mutable state, all behind one lock so the mint stays
/// single-flight (mirrors the Dart fields, `control_token_provider.dart:48-54`;
/// the Dart `_inflight` future dance is subsumed by holding the lock across
/// the mint).
#[derive(Default)]
struct State {
    cached: Option<MintedToken>,
    /// A 401 was observed for the cached token (`invalidate*`) — the next
    /// `token()` must mint and may never return the rejected token
    /// (`control_token_provider.dart:72-79`).
    must_mint: bool,
    /// Unix second before which no mint attempt is made (set by a mint
    /// failure; `control_token_provider.dart:52,147`).
    cooldown_until: u64,
    /// Presence of a recorded mint failure (`_lastError`,
    /// `control_token_provider.dart:53,148`), surfaced when a later call must
    /// error without attempting a mint (cooldown). Always the fixed
    /// [`MINT_FAILED_REDACTED`] text, never the minter's Display output — see
    /// that const's doc.
    last_error: Option<String>,
}

impl State {
    /// The cached token was rejected by a 401: drop it and force the next
    /// `token()` to mint — it may never return the rejected token
    /// (`control_token_provider.dart:72-79`). The shared tail of both
    /// `invalidate*` variants.
    fn reject_cached(&mut self) {
        self.cached = None;
        self.must_mint = true;
    }
}

/// Caches a control token, refreshing when missing or within the refresh window
/// of expiry. Concurrent `token()` callers serialize on the mint (single-flight:
/// a late caller re-checks the cache under the lock and returns the fresh token
/// rather than minting again).
///
/// Transport-identity binding is deliberately NOT ported from the Dart
/// provider (plan 001 §3.4): a shed-core `Client` + provider pair is immutable
/// per transport identity — the app layer constructs a NEW `Client` when the
/// host/port/pin changes, which deletes the identity-race class instead of
/// managing it. Recorded as a Phase B invariant.
///
/// Cancellation caveat (accepted limitation — C6 adversarial review #3, plan
/// §9): because the lock is held ACROSS the mint, cancelling a `token()`
/// future from outside (e.g. `Client::rc_events` bounds bearer resolution
/// with its connect timeout) can drop the very future that holds the lock
/// mid-mint. The lock frees on drop, but a remote mint side effect (host
/// agent request, SSH mint) may still complete out-of-band, so the caller's
/// immediate retry can start a SECOND concurrent mint. For this crate's
/// minters a rare double-mint costs one extra round trip plus one abandoned
/// short-TTL token — never a lockout (cooldown state mutates only when a mint
/// COMPLETES as a failure) and never a wrong cached token (the cancelled
/// mint's result is dropped, not cached). The upgrade, if it ever matters, is
/// provider-owned in-flight mint state joined by waiters off the lock —
/// shed-broker's watch-based single-flight (`shed-broker/src/minter.rs`,
/// module docs) is the in-repo pattern.
pub struct ControlTokenProvider {
    server: String,
    minter: Arc<dyn TokenMinter>,
    /// The clock, unix SECONDS (an `Arc<dyn Fn>` so tests can drive advancing
    /// time — a bare `fn` pointer can't capture per-test mutable state).
    now_unix: Arc<dyn Fn() -> u64 + Send + Sync>,
    refresh_window: Duration,
    /// Deterministic per-name refresh jitter, derived ONCE from the server
    /// name by [`Self::with_name_jitter`]. Subtracted from the refresh
    /// threshold, so refresh happens EARLIER — a fleet of providers with the
    /// same expiry de-synchronizes instead of thundering the minter together.
    jitter: Duration,
    /// Cooldown started by a mint failure; while it runs, `token()` makes no
    /// mint attempt (returns the cached still-valid token, or the last error).
    mint_cooldown: Duration,
    state: Mutex<State>,
}

impl ControlTokenProvider {
    pub fn new(server: String, minter: Arc<dyn TokenMinter>) -> Self {
        Self {
            server,
            minter,
            now_unix: Arc::new(default_now_unix),
            refresh_window: REFRESH_WINDOW,
            jitter: Duration::ZERO,
            mint_cooldown: Duration::ZERO,
            state: Mutex::new(State::default()),
        }
    }

    /// Replace the clock (unix seconds). Public, not `#[cfg(test)]`: the seam
    /// must be reachable from shed-app tests and the Phase B bridge (the same
    /// public-builder convention as `SseParser::with_max_event_bytes`).
    pub fn with_now(mut self, now: impl Fn() -> u64 + Send + Sync + 'static) -> Self {
        self.now_unix = Arc::new(now);
        self
    }

    /// Refresh this long before expiry (default [`REFRESH_WINDOW`], 2h —
    /// unchanged desktop behavior; mobile passes 2h5m).
    pub fn with_refresh_window(mut self, window: Duration) -> Self {
        self.refresh_window = window;
        self
    }

    /// Enable deterministic refresh jitter in `[0, max)`: the value is derived
    /// ONCE from the server name via [`name_jitter`] (Dart parity —
    /// `control_token_provider.dart:39`, `nameJitter(name, jitterMs)`) and
    /// SUBTRACTED from the refresh threshold, so this provider refreshes up to
    /// `max` earlier than an un-jittered one. Default off (zero jitter).
    ///
    /// [`name_jitter`] works in the Dart algorithm's millisecond domain for
    /// cross-language stability; the provider's clock is unix seconds, so the
    /// derived jitter truncates to whole seconds here (sub-second precision is
    /// noise against mobile's 5-minute max).
    pub fn with_name_jitter(mut self, max: Duration) -> Self {
        self.jitter = Duration::from_millis(name_jitter(&self.server, max.as_millis() as u64));
        self
    }

    /// After a mint failure, make no further mint attempt until `cooldown` has
    /// elapsed — a polling caller can't storm a host with doomed mints
    /// (`control_token_provider.dart:107-123`). During the cooldown `token()`
    /// returns the cached still-valid token if one exists, else the recorded
    /// last error. Default `0` = off (every call may attempt a mint).
    pub fn with_mint_cooldown(mut self, cooldown: Duration) -> Self {
        self.mint_cooldown = cooldown;
        self
    }

    /// Pre-populate the cache with a persisted token (mobile's config seed,
    /// `control_token_provider.dart:153-157`) so the first `token()` can skip
    /// the mint. An empty/whitespace token is IGNORED — an unusable credential
    /// is never cached (plan 001 §3.4).
    pub fn with_seed(mut self, seed: MintedToken) -> Self {
        if !seed.token.trim().is_empty() {
            self.state.get_mut().cached = Some(seed);
        }
        self
    }

    /// The current token, minting/refreshing when it is missing or near expiry.
    /// FSM ported from Dart's `get()` (`control_token_provider.dart:57-97`),
    /// minus the identity-binding and legacy-host branches (see the type-level
    /// docs / the test-module note):
    ///   * after an `invalidate*` the mint is FORCED — on failure this errors,
    ///     never returning the rejected token (`:72-79`)
    ///   * a still-valid cached token inside the refresh window mints
    ///     proactively but KEEPS the cached token when that mint fails
    ///     (`:81-92`)
    ///   * missing/expired → mint; a failure here is fail-closed (the caller
    ///     then sends no token) (`:94-96`)
    pub async fn token(&self) -> Result<String, ShedError> {
        // Hold the lock across the mint so concurrent callers serialize: the
        // first mints, the rest re-check here and return its result.
        let mut st = self.state.lock().await;
        let now = (self.now_unix)();

        // Reactive: a prior 401 means the current token is rejected — mint,
        // and do not fall back to it.
        if st.must_mint {
            return match self.mint_locked(&mut st, now).await {
                Ok(minted) => {
                    st.must_mint = false;
                    Ok(minted.token)
                }
                Err(e) => Err(e),
            };
        }

        if let Some(current) = st.cached.clone() {
            if !expired(&current, now) {
                if self.needs_refresh(&current, now) {
                    if let Ok(minted) = self.mint_locked(&mut st, now).await {
                        return Ok(minted.token);
                    }
                    // Keep-valid-on-proactive-failure (strict improvement #1,
                    // `control_token_provider.dart:82-92`): the refresh mint
                    // failed but the cached token is not yet expired — fall
                    // through and return it rather than erroring.
                }
                return Ok(current.token);
            }
        }

        self.mint_locked(&mut st, now).await.map(|m| m.token)
    }

    /// Drop the cached token UNCONDITIONALLY so the next `token()` force-mints.
    /// Kept for back-compat; prefer [`Self::invalidate_if_current`] when the
    /// rejected token is known — this variant can erase a NEWER token when a
    /// stale 401 races a rotation.
    pub async fn invalidate(&self) {
        self.state.lock().await.reject_cached();
    }

    /// Stale-401 guard (strict improvement #2,
    /// `control_token_provider.dart:99-105`): drop the cache ONLY if the
    /// cached token is `observed` (the one the 401 rejected) — a 401 for a
    /// token already rotated past is ignored. Sets the must-mint flag so the
    /// next `token()` force-mints and never returns the rejected token.
    pub async fn invalidate_if_current(&self, observed: &str) {
        let mut st = self.state.lock().await;
        if let Some(c) = &st.cached {
            if c.token != observed {
                return; // stale 401 — the rejected token is already gone
            }
        }
        st.reject_cached();
    }

    /// One mint attempt under the held state lock (the single-flight point),
    /// with the failure cooldown — Dart's `_mint` + `_doMint`
    /// (`control_token_provider.dart:107-151`): inside the cooldown no attempt
    /// is made (the recorded redacted error is returned, state untouched); a
    /// success replaces the cache and clears the last error; a failure starts
    /// the cooldown, records the fixed [`MINT_FAILED_REDACTED`] marker (never
    /// the minter's Display text — it can embed secrets), returns the REAL
    /// error to this immediate caller, and leaves the cache alone (an
    /// expired-but-cached token stays for a later retry; a still-valid one
    /// backs keep-valid). An empty minted token is a failure, not a usable
    /// credential (fail-closed), even if the minter reported success.
    async fn mint_locked(&self, st: &mut State, now: u64) -> Result<MintedToken, ShedError> {
        if now < st.cooldown_until {
            return Err(last_error(st));
        }
        let outcome = match self.minter.mint(&self.server).await {
            Ok(minted) if minted.token.is_empty() => Err(ShedError::Transport(
                "control-token mint returned an empty token".into(),
            )),
            other => other,
        };
        match outcome {
            Ok(minted) => {
                st.cached = Some(minted.clone());
                st.last_error = None;
                Ok(minted)
            }
            Err(e) => {
                // Fresh clock read for the cooldown start (Dart `_doMint`
                // re-reads `_now()`, `:147` — the mint itself took time).
                st.cooldown_until = (self.now_unix)() + self.mint_cooldown.as_secs();
                st.last_error = Some(MINT_FAILED_REDACTED.into());
                Err(e)
            }
        }
    }

    /// Within the refresh window of expiry: `now >= exp - window - jitter`
    /// (`control_token_provider.dart:162-166`; the jitter subtraction makes
    /// refresh earlier). No expiry → never (only `invalidate*` refreshes).
    /// `saturating_sub` matches Dart's signed math: a threshold below zero
    /// means "always refresh".
    fn needs_refresh(&self, t: &MintedToken, now: u64) -> bool {
        match t.expires_at_unix {
            None => false,
            Some(exp) => {
                now >= exp.saturating_sub(self.refresh_window.as_secs() + self.jitter.as_secs())
            }
        }
    }
}

/// Past expiry (`control_token_provider.dart:159-160`). No expiry → never.
fn expired(t: &MintedToken, now: u64) -> bool {
    matches!(t.expires_at_unix, Some(exp) if now >= exp)
}

/// The error for a `token()` call blocked from minting (cooldown): the
/// recorded redacted marker, or a generic fail-closed message (Dart's
/// `_lastError ?? AppError.authExpired()`, `control_token_provider.dart:78,96`
/// — except the persisted text is [`MINT_FAILED_REDACTED`], never the original
/// minter error).
fn last_error(st: &State) -> ShedError {
    ShedError::Transport(
        st.last_error
            .clone()
            .unwrap_or_else(|| "control token expired and mint unavailable".into()),
    )
}

fn default_now_unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};

    // Port coverage of mobile's `control_token_provider_test.dart:28-252`
    // (plan 001 §7 AC#4). NOT ported (§3.4 decision, see the type docs): the
    // two identity-binding cases (`drops the cached token when the host
    // transport identity changes`, `does not hand an in-flight mint to a
    // changed identity`) — a shed-core Client+provider pair is immutable per
    // transport identity; the app layer constructs a NEW Client (and provider)
    // on a host/port/pin change, deleting the identity-race class those cases
    // guard. TRANSLATED rather than ported: `returns null for a legacy
    // (non-secure) host` — Rust represents "legacy" at Client construction (a
    // minter-less Client resolves its bearer from the static token or sends
    // none; http.rs `static_token_used_without_provider` +
    // `mint_failure_is_fail_closed_no_downgrade` pin that contract), not as a
    // provider state.

    /// A minter that counts calls and returns `tok-<n>` (or fails — the
    /// switch is runtime-flippable, the Dart tests' `shouldFail` captures; a
    /// FAILED attempt is counted in `n` too). Optional expiry lets a test
    /// force the refresh-window path; a delay lets one force single-flight.
    struct MockMinter {
        calls: AtomicUsize,
        fail: AtomicBool,
        expires_at_unix: Option<u64>,
        delay_ms: u64,
    }

    impl MockMinter {
        fn new(fail: bool, expires_at_unix: Option<u64>, delay_ms: u64) -> Arc<Self> {
            Arc::new(Self {
                calls: AtomicUsize::new(0),
                fail: AtomicBool::new(fail),
                expires_at_unix,
                delay_ms,
            })
        }
        fn ok() -> Arc<Self> {
            Self::new(false, None, 0)
        }
        fn failing() -> Arc<Self> {
            Self::new(true, None, 0)
        }
        /// The Dart FSM tests' minter shape: flippable failure, far-future
        /// expiry.
        fn flaky(fail: bool) -> Arc<Self> {
            Self::new(fail, Some(FAR_FUTURE), 0)
        }
        fn set_fail(&self, v: bool) {
            self.fail.store(v, Ordering::SeqCst);
        }
        fn count(&self) -> usize {
            self.calls.load(Ordering::SeqCst)
        }
    }

    #[async_trait::async_trait]
    impl TokenMinter for MockMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            let n = self.calls.fetch_add(1, Ordering::SeqCst) + 1;
            if self.delay_ms > 0 {
                tokio::time::sleep(Duration::from_millis(self.delay_ms)).await;
            }
            if self.fail.load(Ordering::SeqCst) {
                return Err(ShedError::Transport("mint failed".into()));
            }
            Ok(MintedToken {
                token: format!("tok-{n}"),
                expires_at_unix: self.expires_at_unix,
            })
        }
    }

    /// Dart's `farFuture` (99999999 ms there; unix seconds here — only "far
    /// beyond every test clock" matters).
    const FAR_FUTURE: u64 = 99_999_999;

    fn seed(exp: u64) -> MintedToken {
        MintedToken {
            token: "seed".into(),
            expires_at_unix: Some(exp),
        }
    }

    #[tokio::test]
    async fn caches_a_no_expiry_token() {
        let minter = MockMinter::ok();
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-1"); // cached, no re-mint
        assert_eq!(minter.count(), 1);
    }

    #[tokio::test]
    async fn invalidate_forces_remint() {
        let minter = MockMinter::ok();
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        p.invalidate().await;
        assert_eq!(p.token().await.unwrap(), "tok-2");
        assert_eq!(minter.count(), 2);
    }

    #[tokio::test]
    async fn refreshes_within_expiry_window() {
        // Expiry = now → expired → re-mint each call.
        let now = default_now_unix();
        let minter = MockMinter::new(false, Some(now), 0);
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-2");
        assert_eq!(minter.count(), 2);
    }

    #[tokio::test]
    async fn does_not_refresh_far_from_expiry() {
        let far = default_now_unix() + 10 * 60 * 60; // 10h out, beyond the 2h window
        let minter = MockMinter::new(false, Some(far), 0);
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-1"); // still fresh
        assert_eq!(minter.count(), 1);
    }

    #[tokio::test]
    async fn mint_failure_is_fail_closed() {
        let minter = MockMinter::failing();
        let p = ControlTokenProvider::new("mini2".into(), minter);
        assert!(p.token().await.is_err()); // caller then sends NO token
    }

    #[tokio::test]
    async fn empty_minted_token_is_fail_closed() {
        struct EmptyMinter;
        #[async_trait::async_trait]
        impl TokenMinter for EmptyMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                Ok(MintedToken {
                    token: String::new(),
                    expires_at_unix: None,
                })
            }
        }
        let p = ControlTokenProvider::new("mini2".into(), Arc::new(EmptyMinter));
        assert!(p.token().await.is_err()); // empty token → mint failure, not cached
    }

    // Also the port of Dart's `collapses concurrent mints into one
    // (single-flight)` (`control_token_provider_test.dart:114-134`): no seed →
    // every caller must mint, and the held lock collapses them to one.
    #[tokio::test]
    async fn concurrent_callers_mint_once() {
        // Single-flight: a slow mint + concurrent callers → exactly one mint.
        let minter = MockMinter::new(false, None, 40);
        let p = Arc::new(ControlTokenProvider::new("mini2".into(), minter.clone()));
        let (a, b, c) = tokio::join!(p.token(), p.token(), p.token());
        assert_eq!(a.unwrap(), "tok-1");
        assert_eq!(b.unwrap(), "tok-1");
        assert_eq!(c.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }

    // ---- mobile FSM ports (control_token_provider_test.dart:28-252) ----

    // Dart `uses the config seed without minting when it is fresh` (:47-62).
    #[tokio::test]
    async fn uses_the_seed_without_minting_when_fresh() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1000))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed");
        assert_eq!(minter.count(), 0);
    }

    // Dart `proactively mints when within the refresh window` (:64-79).
    #[tokio::test]
    async fn proactively_mints_within_the_refresh_window() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 9000)
            .with_refresh_window(Duration::from_secs(5000))
            .with_seed(seed(10_000));
        // 9000 >= 10000 - 5000 → inside the window (not yet expired) → mint.
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }

    // Dart `keeps a still-valid token when a proactive mint fails` (:81-91) —
    // strict improvement #1 over the pre-C6 Rust FSM, which errored here.
    #[tokio::test]
    async fn keeps_a_still_valid_token_when_a_proactive_mint_fails() {
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 9000)
            .with_refresh_window(Duration::from_secs(5000))
            .with_seed(seed(10_000));
        assert_eq!(p.token().await.unwrap(), "seed"); // kept, not an error
        assert_eq!(minter.count(), 1); // the refresh mint WAS attempted
    }

    // Dart `mints when the seed is expired, throwing if that mint fails`
    // (:93-112). Cooldown 0 (the default) so the immediate retry may mint.
    #[tokio::test]
    async fn expired_seed_mints_and_errs_when_that_mint_fails() {
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 5000)
            .with_seed(seed(1000)); // expired
        assert!(p.token().await.is_err());
        minter.set_fail(false);
        assert_eq!(p.token().await.unwrap(), "tok-2"); // attempt 1 failed
        assert_eq!(minter.count(), 2);
    }

    // Dart `forces a fresh mint on invalidate (401), never the rejected token`
    // (:136-155).
    #[tokio::test]
    async fn invalidate_if_current_forces_a_fresh_mint_never_the_rejected_token() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed");
        p.invalidate_if_current("seed").await;
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }

    // The must-mint failure half of Dart's `get()` reactive branch (:72-79):
    // after an invalidate, a failed mint is an ERROR — the still-valid (but
    // rejected) seed is never handed back.
    #[tokio::test]
    async fn must_mint_failure_errs_even_with_an_unexpired_cached_token() {
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed"); // far from expiry: no mint
        assert_eq!(minter.count(), 0);
        p.invalidate_if_current("seed").await;
        assert!(p.token().await.is_err()); // forced mint failed → Err, not "seed"
        assert_eq!(minter.count(), 1);
    }

    // Dart `does not re-mint within the cooldown after a failed mint`
    // (:157-179). Dart drives ms; the Rust clock is unix seconds — same
    // numbers, seconds domain.
    #[tokio::test]
    async fn does_not_remint_within_the_cooldown_after_a_failed_mint() {
        let now = Arc::new(AtomicU64::new(5000));
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now({
                let now = now.clone();
                move || now.load(Ordering::SeqCst)
            })
            .with_mint_cooldown(Duration::from_secs(1000))
            .with_seed(seed(1000)); // expired
        assert!(p.token().await.is_err()); // fails; cooldown until 6000
        assert_eq!(minter.count(), 1);
        now.store(5500, Ordering::SeqCst); // within cooldown → NO mint attempt
        assert!(p.token().await.is_err());
        assert_eq!(minter.count(), 1);
        now.store(6000, Ordering::SeqCst); // cooldown elapsed → mints again
        assert!(p.token().await.is_err());
        assert_eq!(minter.count(), 2);
    }

    // The cooldown's still-valid half (plan §3.4): during a cooldown a cached
    // still-valid token is returned WITHOUT a mint attempt.
    #[tokio::test]
    async fn cooldown_returns_the_cached_still_valid_token_without_minting() {
        let now = Arc::new(AtomicU64::new(9000));
        let minter = MockMinter::flaky(true);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now({
                let now = now.clone();
                move || now.load(Ordering::SeqCst)
            })
            .with_refresh_window(Duration::from_secs(5000))
            .with_mint_cooldown(Duration::from_secs(1000))
            .with_seed(seed(10_000));
        // In the refresh window: the mint fails (keep-valid) + starts cooldown.
        assert_eq!(p.token().await.unwrap(), "seed");
        assert_eq!(minter.count(), 1);
        now.store(9100, Ordering::SeqCst); // still in window, cooldown active
        assert_eq!(p.token().await.unwrap(), "seed");
        assert_eq!(minter.count(), 1); // no second attempt
    }

    // C6 adversarial review #2b: the PERSISTED last_error (served on later
    // cooldown-blocked calls) is the fixed redacted marker — a minter error
    // embedding secret material must never be stored and replayed. The
    // immediate caller of the failing mint still receives the real error
    // (pre-C6 propagation parity; Dart rethrows too, :149).
    #[tokio::test]
    async fn cooldown_blocked_error_is_redacted_never_the_minter_text() {
        struct SecretErrMinter;
        #[async_trait::async_trait]
        impl TokenMinter for SecretErrMinter {
            async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
                Err(ShedError::Transport(
                    "mint blew up: SECRET-TOKEN-MATERIAL".into(),
                ))
            }
        }
        let now = Arc::new(AtomicU64::new(5000));
        let p = ControlTokenProvider::new("mini3".into(), Arc::new(SecretErrMinter))
            .with_now({
                let now = now.clone();
                move || now.load(Ordering::SeqCst)
            })
            .with_mint_cooldown(Duration::from_secs(1000))
            .with_seed(seed(1000)); // expired seed → the mint is forced
        let immediate = p.token().await.unwrap_err().to_string();
        // The immediate failure surfaces the minter's real error.
        assert!(
            immediate.contains("SECRET-TOKEN-MATERIAL"),
            "got: {immediate}"
        );
        // A later cooldown-blocked call serves only the redacted marker.
        now.store(5500, Ordering::SeqCst);
        let blocked = p.token().await.unwrap_err().to_string();
        assert!(
            !blocked.contains("SECRET"),
            "persisted error leaked: {blocked}"
        );
        assert!(
            blocked.contains("control token mint failed"),
            "got: {blocked}"
        );
    }

    // Dart `ignores a stale 401 for a token it has already rotated past`
    // (:181-203) — strict improvement #2.
    #[tokio::test]
    async fn ignores_a_stale_401_for_an_already_rotated_token() {
        let minter = MockMinter::flaky(false);
        let p = ControlTokenProvider::new("mini3".into(), minter.clone())
            .with_now(|| 0)
            .with_refresh_window(Duration::from_secs(1))
            .with_seed(seed(FAR_FUTURE));
        assert_eq!(p.token().await.unwrap(), "seed");
        p.invalidate_if_current("seed").await; // real 401 on the seed
        assert_eq!(p.token().await.unwrap(), "tok-1"); // rotated
        p.invalidate_if_current("seed").await; // stale 401 → ignored
        assert_eq!(p.token().await.unwrap(), "tok-1"); // still cached
        assert_eq!(minter.count(), 1);
    }

    // Plan §3.4: `with_seed` ignores an empty/whitespace token — never caches
    // an unusable credential; the first token() mints as if unseeded.
    #[tokio::test]
    async fn with_seed_ignores_empty_and_whitespace_tokens() {
        for junk in ["", "   \t "] {
            let minter = MockMinter::flaky(false);
            let p = ControlTokenProvider::new("mini3".into(), minter.clone())
                .with_now(|| 0)
                .with_seed(MintedToken {
                    token: junk.into(),
                    expires_at_unix: Some(FAR_FUTURE),
                });
            assert_eq!(p.token().await.unwrap(), "tok-1", "seed {junk:?}");
            assert_eq!(minter.count(), 1);
        }
    }

    // Jitter is SUBTRACTED from the refresh threshold — refresh happens
    // EARLIER. name_jitter("my-server", 300000 ms) = 145668 ms → 145 s in the
    // provider's seconds-domain comparison. exp = 100000, window = 1000 s:
    // threshold without jitter = 99000, with = 98855. At now = 98900 only the
    // jittered provider refreshes.
    #[tokio::test]
    async fn name_jitter_is_subtracted_so_refresh_happens_earlier() {
        let jittered_minter = MockMinter::flaky(false);
        let jittered = ControlTokenProvider::new("my-server".into(), jittered_minter.clone())
            .with_now(|| 98_900)
            .with_refresh_window(Duration::from_secs(1000))
            .with_name_jitter(Duration::from_secs(300))
            .with_seed(seed(100_000));
        assert_eq!(jittered.token().await.unwrap(), "tok-1");
        assert_eq!(jittered_minter.count(), 1);

        let plain_minter = MockMinter::flaky(false);
        let plain = ControlTokenProvider::new("my-server".into(), plain_minter.clone())
            .with_now(|| 98_900)
            .with_refresh_window(Duration::from_secs(1000))
            .with_seed(seed(100_000));
        assert_eq!(plain.token().await.unwrap(), "seed");
        assert_eq!(plain_minter.count(), 0);
    }

    // Cross-language vectors for [`name_jitter`] (plan §3.4/AC#5): expected
    // values derived by running the Dart algorithm
    // (`control_token_provider.dart:175-181`) by hand — per UTF-16 code unit,
    // h = (h*31 + cu).toSigned(32), then h.abs() % max(maxMs, 1).
    #[test]
    fn name_jitter_matches_the_dart_algorithm() {
        // "my-server" (ASCII), code units m=109 y=121 -=45 s=115 e=101 r=114
        // v=118 e=101 r=114:
        //   109 → 3500 → 108545 → 3365010 → 104315411
        //   → 104315411*31+114 = 3233777855   → wraps to -1061189441
        //   → -1061189441*31+118              → wraps to  1462865815
        //   →  1462865815*31+101              → wraps to -1895799890
        //   → -1895799890*31+114              → wraps to  1359745668
        // abs(1359745668) % 300000 = 145668; % 1000 = 668.
        assert_eq!(name_jitter("my-server", 300_000), 145_668);
        assert_eq!(name_jitter("my-server", 1_000), 668);

        // "grüß-server" (non-ASCII, still BMP — ONE code unit each: ü=252
        // ß=223), units g=103 r=114 ü=252 ß=223 -=45 s=115 e=101 r=114 v=118
        // e=101 r=114:
        //   103 → 3307 → 102769 → 3186062 → 98767967
        //   → 98767967*31+115 = 3061807092    → wraps to -1233160204
        //   → -1233160204*31+101              → wraps to   426739441
        //   →   426739441*31+114              → wraps to   344020897
        //   →   344020897*31+118              → wraps to  2074713333
        //   →  2074713333*31+101              → wraps to  -108396016
        //   →  -108396016*31+114              → wraps to   934690914
        // abs(934690914) % 300000 = 190914; % 1000 = 914.
        assert_eq!(name_jitter("grüß-server", 300_000), 190_914);
        assert_eq!(name_jitter("grüß-server", 1_000), 914);

        // "shed-🚀" (emoji = a SURROGATE PAIR, TWO code units — Dart
        // `codeUnitAt` sees the UTF-16 halves of U+1F680: high 0xD83D=55357,
        // low 0xDE80=56960), units s=115 h=104 e=101 d=100 -=45 55357 56960:
        //   115 → 3669 → 113840 → 3529140 → 109403385
        //   → 109403385*31+55357 = 3391560292 → wraps to  -903407004
        //   → -903407004*31+56960             → wraps to  2059210908
        // abs(2059210908) % 300000 = 10908; % 1000 = 908.
        assert_eq!(name_jitter("shed-🚀", 300_000), 10_908);
        assert_eq!(name_jitter("shed-🚀", 1_000), 908);

        // A name whose FINAL hash is negative, exercising unsigned_abs:
        // "my-server-dev" folds to h = -267572916;
        // abs(-267572916) % 300000 = 272916.
        assert_eq!(name_jitter("my-server-dev", 300_000), 272_916);

        // Dart `maxMs < 1 ? 1 : maxMs`: max 0 → % 1 → always 0 (jitter off).
        assert_eq!(name_jitter("my-server", 0), 0);
        assert_eq!(name_jitter("", 300_000), 0); // h stays 0
    }
}
