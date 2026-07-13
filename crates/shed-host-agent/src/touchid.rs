//! The native macOS Touch-ID / biometrics approval gate — a pure-`objc2` port of the
//! Go daemon's CGO `touchid_darwin.go` (`LAContext.canEvaluatePolicy` /
//! `evaluatePolicy`) + `touchid_stub.go` (the `!darwin` deny-all twin). This is the
//! last CGO in `cmd/shed-host-agent`, so the Rust binary needs NO C toolchain: `objc2`
//! links LocalAuthentication / Foundation at the linker level (`#[link(kind =
//! "framework")]`) — no `import "C"`, no clang glue.
//!
//! Mirrors Go EXACTLY:
//!   * LAPolicy map — `allow_password` (`biometrics-or-password`) →
//!     `DeviceOwnerAuthentication` (biometrics + Apple-Watch / password fallback);
//!     else (`biometrics`) → `DeviceOwnerAuthenticationWithBiometrics` (Touch-ID only).
//!     `allow_password = policy != "biometrics"` (Go's `resolveAllowPassword`).
//!   * COARSE result map — `canEvaluatePolicy` false → unavailable ("not available");
//!     a reply `success` → approved; ANY non-success reply (every `LAError`) → denied.
//!     No per-`LAError` branch (unlike the Tauri B3 reference's `map_la_error`): the
//!     wire only ever sees the `approved` bool, so richness is non-parity.
//!   * Scope / TTL cache — `per-session` (global) / `per-shed` (keyed `server/shed`) /
//!     `per-request` (always prompt); a fresh prompt audits `decided_by:"touchid"`, a
//!     cache hit `decided_by:"policy"`; an approve writes BOTH caches regardless of
//!     scope; TTL parse-fail → 4h; the audited `ttl` is the RAW `session_ttl` text.
//!   * (C1) deny AND unavailable BOTH return the EMPTY outcome (`decided_by:""`,
//!     `scope:None`, `ttl:None`, carrying only `reason`) — Go returns
//!     `ApprovalOutcome{}` on both (`touchid_darwin.go:129,131`), DISCARDING the built
//!     scope/ttl. Only the approve + cache-hit paths populate them.
//!
//! The proven primitive is `desktop/tauri/src-tauri/src/approval.rs` (`macos::
//! TouchIdGate`); this ADAPTS it (separate build worlds — the Tauri crate is its own
//! workspace; duplicating the ~40-line ObjC seam keeps Tauri deps out of `crates/`).
//! Deltas vs the reference: the coarse bool result (no `map_la_error`), the scope/TTL
//! cache (the Tauri gate is cache-less), and this crate's own `ApprovalGate` /
//! `ApprovalOutcome` types.
//!
//! The live prompt cannot be automated (a real fingerprint on a signed build); the
//! `LocalAuth` seam makes everything UP TO the two ObjC calls unit-testable via
//! `FakeAuth` (no prompt), on macOS AND Linux CI. The gate machinery is compiled on
//! macOS (real) and in any `test` build (the fake); the ObjC `RealLocalAuth` is
//! macOS-only. On non-macOS `new_biometric_gate` fails closed to `DenyAllGate`.

use std::sync::Arc;

use crate::approval::ApprovalGate;

/// Build the native-biometric approval gate for the given policy (the darwin factory,
/// mirroring `touchid_darwin.go:newApprovalGate`): `allow_password = policy !=
/// "biometrics"`, TTL parsed from `session_ttl` (parse-fail → 4h). The single
/// `#[cfg(target_os = "macos")]` seam lives HERE (not at the `main.rs` routing site,
/// which is an unconditional match arm) — mirroring Go's `gateFor → newApprovalGate`,
/// where the build tag picks the darwin gate vs the stub inside the factory.
#[cfg(target_os = "macos")]
pub fn new_biometric_gate(policy: &str, scope: &str, session_ttl: &str) -> Arc<dyn ApprovalGate> {
    Arc::new(TouchIdGate::new(
        Box::new(real::RealLocalAuth),
        allow_password(policy),
        scope,
        session_ttl,
    ))
}

/// The `touchid_stub.go` twin: off macOS there is no LocalAuthentication, so the
/// biometric policies fail CLOSED to deny-all (never silent-approve). `main.rs`'s
/// biometric `select_gate` arm is unconditional, so it routes here on Linux and the
/// deny-safe fallback is uniform.
#[cfg(not(target_os = "macos"))]
pub fn new_biometric_gate(
    _policy: &str,
    _scope: &str,
    _session_ttl: &str,
) -> Arc<dyn ApprovalGate> {
    Arc::new(crate::approval::DenyAllGate)
}

// The gate machinery is compiled on macOS (production) and in any `test` build (the
// `FakeAuth`-driven units run on macOS AND Linux CI). On a non-macOS NON-test build it
// is absent — `new_biometric_gate` there is the deny-all stub, so nothing references
// it and there is no dead code.
#[cfg(any(target_os = "macos", test))]
mod gate {
    use std::collections::HashMap;
    use std::time::{Duration, Instant};

    use crate::approval::{ApprovalGate, ApprovalOutcome};
    use crate::config::{self, POLICY_BIOMETRICS, POLICY_BIOMETRICS_OR_PASSWORD};

    /// Go's `resolveAllowPassword` (`config.go:414`): any policy other than the
    /// biometrics-only policy allows the password / Apple-Watch fallback. `gateFor`
    /// only routes the two biometric policies here, so in practice this is
    /// `biometrics → false`, `biometrics-or-password → true`.
    pub(super) fn allow_password(policy: &str) -> bool {
        policy != POLICY_BIOMETRICS
    }

    /// A seam over `LAContext` so the gate's decision logic is unit-testable WITHOUT a
    /// real biometric prompt (`canEvaluatePolicy` is true on any enrolled Mac, so the
    /// real gate would fire a live prompt). Tests inject `FakeAuth`; production uses
    /// `RealLocalAuth`. `Send + Sync` so `TouchIdGate` can back an `Arc<dyn
    /// ApprovalGate>`. The result is COARSE (a plain bool) — Go collapses every
    /// `LAError` into `success == false`, and the wire only sees `approved`.
    #[async_trait::async_trait]
    pub(super) trait LocalAuth: Send + Sync {
        /// Can the device satisfy the policy right now (biometrics/passcode enrolled)?
        /// `false` → the gate reports "not available" WITHOUT ever prompting.
        fn can_evaluate(&self, allow_password: bool) -> bool;
        /// Present the OS prompt + await the decision. `true` = approved; `false` =
        /// denied (incl. every `LAError`, a 120s timeout, or a task failure — all
        /// deny-safe).
        async fn evaluate(&self, allow_password: bool, reason: &str) -> bool;
    }

    /// The scope/TTL approval cache (Go's `lastApproval` + `shedApprovals`), behind the
    /// gate's `tokio::Mutex`. Per-shed keys are `server/shed` so identical shed names
    /// on different servers don't share an approval; per-session stays global.
    #[derive(Default)]
    struct CacheState {
        last_approval: Option<Instant>,
        shed_approvals: HashMap<String, Instant>,
    }

    /// The macOS Touch-ID `ApprovalGate` — `LAContext.evaluatePolicy` behind the
    /// [`LocalAuth`] seam plus the scope/TTL cache. Holds NO `objc2`/`LAContext` field
    /// (L5) — only the `Box<dyn LocalAuth>` seam — so it stays `Send + Sync`; the ObjC
    /// objects live only on the `spawn_blocking` stack inside `RealLocalAuth`.
    pub(super) struct TouchIdGate {
        auth: Box<dyn LocalAuth>,
        allow_password: bool,
        scope: String,
        /// The RAW `session_ttl` string — what the approve/cache-hit outcome audits
        /// (`out.TTL = cfg.SessionTTL`), NOT the parsed/defaulted duration (M3).
        ttl_text: String,
        /// The parsed cache TTL (parse-fail → 4h), used only for the cache math.
        ttl: Duration,
        cache: tokio::sync::Mutex<CacheState>,
    }

    impl TouchIdGate {
        /// Mirror `touchid_darwin.go:newApprovalGate`: parse `session_ttl` for the
        /// cache TTL (parse-fail / non-positive → 4h) via the shared Go-duration subset
        /// (no second parser), retain the raw text for the audit.
        pub(super) fn new(
            auth: Box<dyn LocalAuth>,
            allow_password: bool,
            scope: &str,
            session_ttl: &str,
        ) -> TouchIdGate {
            // Go: `time.ParseDuration` succeeds → keep the value (incl. 0 / negative);
            // only a PARSE ERROR (or empty) falls back to 4h (`touchid_darwin.go:64`).
            // A zero/negative TTL clamps to `Duration::ZERO`, so the strict `< ttl`
            // cache check is always false → always re-prompt — matching Go, where a
            // `session_ttl: "0"` is the natural "no caching" hardening choice
            // (`now.Sub(t) < 0` is always false). The earlier `n > 0` guard wrongly
            // rewrote 0/negative to 4h (CodeRabbit F1).
            let ttl = match config::parse_go_duration_nanos(session_ttl) {
                Some(n) => Duration::from_nanos(n.max(0) as u64),
                None => Duration::from_secs(4 * 60 * 60),
            };
            TouchIdGate {
                auth,
                allow_password,
                scope: scope.to_string(),
                ttl_text: session_ttl.to_string(),
                ttl,
                cache: tokio::sync::Mutex::new(CacheState::default()),
            }
        }

        /// The POPULATED approve / cache-hit outcome (M3: carries `scope` + the RAW
        /// `ttl_text`, and `decided_by` = `"touchid"` for a fresh prompt / `"policy"`
        /// for a cache hit).
        fn approved(&self, decided_by: &str) -> ApprovalOutcome {
            ApprovalOutcome {
                approved: true,
                decided_by: decided_by.to_string(),
                scope: Some(self.scope.clone()),
                ttl: Some(self.ttl_text.clone()),
                reason: String::new(),
            }
        }

        /// (C1) The EMPTY outcome returned on BOTH deny and unavailable — Go returns
        /// `ApprovalOutcome{}` on both, DISCARDING the built scope/ttl; only `reason`
        /// is set (the ssh deny audit ignores it, but it's kept for fidelity).
        fn empty(reason: &str) -> ApprovalOutcome {
            ApprovalOutcome {
                approved: false,
                decided_by: String::new(),
                scope: None,
                ttl: None,
                reason: reason.to_string(),
            }
        }
    }

    #[async_trait::async_trait]
    impl ApprovalGate for TouchIdGate {
        async fn approve(
            &self,
            _ns: &str,
            _op: &str,
            server: &str,
            shed: &str,
            detail: &str,
        ) -> ApprovalOutcome {
            // ONE `tokio::Mutex` held across the WHOLE call (cache-check + evaluate +
            // store) reproduces Go's whole-`Approve` `sync.Mutex`: at most one live
            // Touch-ID prompt, and cache-hits serialize under the same lock. This is a
            // Rust-only mechanism (Go gets serialization free from a blocking call
            // under a sync lock); a `tokio::Mutex` held across `.await` is sound and
            // does NOT trip `clippy::await_holding_lock` (that lint targets std /
            // parking_lot locks). Monotonic `Instant` vs Go's wall-clock `time.Now()`
            // for the TTL math is a benign, non-wire-observable divergence (L7).
            let mut cache = self.cache.lock().await;
            let now = Instant::now();
            // The `server/shed` cache key is built lazily — only the `per-shed` lookup
            // and the approve write-back consume it, so a `per-session` cache hit (the
            // steady-state hot path) allocates nothing.
            let cache_hit = match self.scope.as_str() {
                "per-session" => cache
                    .last_approval
                    .is_some_and(|t| now.duration_since(t) < self.ttl),
                "per-shed" => cache
                    .shed_approvals
                    .get(&format!("{server}/{shed}"))
                    .is_some_and(|&t| now.duration_since(t) < self.ttl),
                // per-request (and any unrecognized / empty scope) → always prompt.
                _ => false,
            };
            if cache_hit {
                return self.approved("policy");
            }
            // Deny-safe pre-check (Go's `canEvaluatePolicy` false → -1): never prompt.
            if !self.auth.can_evaluate(self.allow_password) {
                return Self::empty("touch ID not available on this device");
            }
            let reason = format!("shed-extensions: {detail} (server: {server}, shed: {shed})");
            if self.auth.evaluate(self.allow_password, &reason).await {
                // Go writes BOTH caches on approve, regardless of scope (so switching
                // scope on a reload can serve a stale cross-scope approval within TTL).
                cache.last_approval = Some(now);
                cache.shed_approvals.insert(format!("{server}/{shed}"), now);
                self.approved("touchid")
            } else {
                // Denied — incl. every `LAError`, the 120s timeout, and a task failure.
                Self::empty("touch ID authentication denied")
            }
        }

        fn method(&self) -> &str {
            if self.allow_password {
                POLICY_BIOMETRICS_OR_PASSWORD
            } else {
                POLICY_BIOMETRICS
            }
        }
    }

    #[cfg(test)]
    mod tests {
        use std::sync::atomic::{AtomicUsize, Ordering};
        use std::sync::{Arc, Mutex};
        use std::time::Duration;

        use super::{allow_password, LocalAuth, TouchIdGate};
        use crate::approval::{ApprovalGate, ApprovalOutcome};
        use crate::config::{POLICY_BIOMETRICS, POLICY_BIOMETRICS_OR_PASSWORD};

        /// Shared, inspectable fake state (the gate moves the `Box<dyn LocalAuth>`, so
        /// the test holds an `Arc` clone to read the call counter + captured reason).
        struct FakeState {
            can: bool,
            approve: bool,
            calls: AtomicUsize,
            last_reason: Mutex<Option<String>>,
        }

        struct FakeAuth {
            state: Arc<FakeState>,
        }

        #[async_trait::async_trait]
        impl LocalAuth for FakeAuth {
            fn can_evaluate(&self, _allow_password: bool) -> bool {
                self.state.can
            }
            async fn evaluate(&self, _allow_password: bool, reason: &str) -> bool {
                assert!(
                    self.state.can,
                    "evaluate() must not run when can_evaluate is false"
                );
                self.state.calls.fetch_add(1, Ordering::SeqCst);
                *self.state.last_reason.lock().unwrap() = Some(reason.to_string());
                self.state.approve
            }
        }

        /// Build a gate over a `FakeAuth` and return the shared state for assertions.
        fn gate_with(
            can: bool,
            approve: bool,
            scope: &str,
            ttl: &str,
            allow_password: bool,
        ) -> (TouchIdGate, Arc<FakeState>) {
            let state = Arc::new(FakeState {
                can,
                approve,
                calls: AtomicUsize::new(0),
                last_reason: Mutex::new(None),
            });
            let gate = TouchIdGate::new(
                Box::new(FakeAuth {
                    state: state.clone(),
                }),
                allow_password,
                scope,
                ttl,
            );
            (gate, state)
        }

        /// The fixed ssh-sign call shape (`ns`/`op` unused by this gate; `detail` is
        /// Go's fixed `"SSH sign request"`).
        async fn approve(g: &TouchIdGate, server: &str, shed: &str) -> ApprovalOutcome {
            g.approve("ssh-agent", "sign", server, shed, "SSH sign request")
                .await
        }

        #[test]
        fn allow_password_only_for_or_password() {
            // Go's resolveAllowPassword: policy != biometrics.
            assert!(!allow_password(POLICY_BIOMETRICS));
            assert!(allow_password(POLICY_BIOMETRICS_OR_PASSWORD));
            // Any other non-biometrics string is also true (gateFor only routes the two).
            assert!(allow_password("anything-else"));
        }

        #[test]
        fn new_biometric_gate_resolves_policy() {
            // Mirrors TestNewApprovalGateResolvesPolicy: method() + carried scope, on
            // the BUILT gate (never calling approve, which would prompt in production).
            let (g, _) = gate_with(
                true,
                true,
                "per-session",
                "4h",
                allow_password(POLICY_BIOMETRICS),
            );
            assert_eq!(g.method(), POLICY_BIOMETRICS);
            assert_eq!(g.scope, "per-session");
            let (g, _) = gate_with(
                true,
                true,
                "per-shed",
                "1h",
                allow_password(POLICY_BIOMETRICS_OR_PASSWORD),
            );
            assert_eq!(g.method(), POLICY_BIOMETRICS_OR_PASSWORD);
            assert_eq!(g.scope, "per-shed");
        }

        #[tokio::test]
        async fn approved_sets_decided_by_touchid() {
            let (g, state) = gate_with(true, true, "per-request", "4h", false);
            let out = approve(&g, "srv", "web").await;
            assert!(out.approved);
            assert_eq!(out.decided_by, "touchid");
            assert_eq!(out.scope.as_deref(), Some("per-request"));
            assert_eq!(out.ttl.as_deref(), Some("4h"));
            assert_eq!(state.calls.load(Ordering::SeqCst), 1);
        }

        #[tokio::test]
        async fn only_real_success_yields_approved() {
            // The single most important property: only a real `success` bool → approved.
            // `Denied` and `Unavailable` NEVER approve.
            for (can, appr) in [(true, true), (true, false), (false, true), (false, false)] {
                let (g, _) = gate_with(can, appr, "per-request", "4h", false);
                let out = approve(&g, "srv", "web").await;
                assert_eq!(out.approved, can && appr, "can={can} approve={appr}");
            }
        }

        #[tokio::test]
        async fn denied_returns_empty_outcome() {
            // (C1) deny discards the built scope/ttl → empty outcome, only `reason`.
            let (g, _) = gate_with(true, false, "per-session", "4h", false);
            let out = approve(&g, "srv", "web").await;
            assert!(!out.approved);
            assert_eq!(out.decided_by, "");
            assert_eq!(out.scope, None);
            assert_eq!(out.ttl, None);
            assert_eq!(out.reason, "touch ID authentication denied");
        }

        #[tokio::test]
        async fn unavailable_returns_empty_outcome() {
            // (C1) unavailable also discards the built scope/ttl → empty outcome.
            let (g, _) = gate_with(false, true, "per-session", "4h", false);
            let out = approve(&g, "srv", "web").await;
            assert!(!out.approved);
            assert_eq!(out.decided_by, "");
            assert_eq!(out.scope, None);
            assert_eq!(out.ttl, None);
            assert_eq!(out.reason, "touch ID not available on this device");
        }

        #[tokio::test]
        async fn unavailable_never_calls_evaluate() {
            // The deny-safe pre-check: can_evaluate false → evaluate() is never reached.
            let (g, state) = gate_with(false, true, "per-request", "4h", false);
            let out = approve(&g, "srv", "web").await;
            assert!(!out.approved);
            assert_eq!(state.calls.load(Ordering::SeqCst), 0);
        }

        #[tokio::test]
        async fn per_session_cache_hit_sets_policy() {
            let (g, state) = gate_with(true, true, "per-session", "4h", false);
            // First prompts (touchid); second within TTL → cache hit (policy), NO prompt.
            let first = approve(&g, "srv", "web").await;
            assert_eq!(first.decided_by, "touchid");
            let second = approve(&g, "srv", "web").await;
            assert_eq!(second.decided_by, "policy");
            // (M3) the cache-hit outcome STILL carries scope + ttl.
            assert_eq!(second.scope.as_deref(), Some("per-session"));
            assert_eq!(second.ttl.as_deref(), Some("4h"));
            assert_eq!(state.calls.load(Ordering::SeqCst), 1, "only one prompt");
        }

        #[tokio::test]
        async fn per_session_cache_expires_reprompts() {
            let (g, state) = gate_with(true, true, "per-session", "4h", false);
            approve(&g, "srv", "web").await;
            // Age the cached approval past the TTL deterministically (no wall-clock
            // sleep): push last_approval back well beyond 4h (or drop it on underflow).
            {
                let mut c = g.cache.lock().await;
                c.last_approval = c
                    .last_approval
                    .and_then(|t| t.checked_sub(Duration::from_secs(5 * 60 * 60)));
            }
            let out = approve(&g, "srv", "web").await;
            assert_eq!(out.decided_by, "touchid", "expired → re-prompt");
            assert_eq!(state.calls.load(Ordering::SeqCst), 2);
        }

        #[tokio::test]
        async fn zero_and_negative_ttl_always_reprompt() {
            // Go: ParseDuration("0")=(0,nil) → sessionTTL=0 → cache check `now.Sub(t)<0`
            // always false → always re-prompt (the "no caching" hardening choice).
            // Must NOT fall back to 4h (CodeRabbit F1). Same for a negative TTL.
            for ttl in ["0", "-5m"] {
                let (g, state) = gate_with(true, true, "per-session", ttl, false);
                assert_eq!(g.ttl, Duration::ZERO, "ttl={ttl}");
                assert_eq!(approve(&g, "srv", "web").await.decided_by, "touchid");
                // Second sign in the same instant: strict `< ZERO` is always false → prompt again.
                assert_eq!(approve(&g, "srv", "web").await.decided_by, "touchid");
                assert_eq!(state.calls.load(Ordering::SeqCst), 2, "ttl={ttl}: re-prompted");
            }
        }

        #[tokio::test]
        async fn per_shed_cache_keyed_by_server_shed() {
            let (g, state) = gate_with(true, true, "per-shed", "4h", false);
            approve(&g, "srv", "web").await; // caches srv/web
            assert_eq!(approve(&g, "srv", "web").await.decided_by, "policy"); // hit
            assert_eq!(approve(&g, "srv", "api").await.decided_by, "touchid"); // other shed
            assert_eq!(approve(&g, "srv2", "web").await.decided_by, "touchid"); // other server
                                                                                // srv/web hit (no prompt) + srv/api + srv2/web prompted = 3 prompts.
            assert_eq!(state.calls.load(Ordering::SeqCst), 3);
        }

        #[tokio::test]
        async fn per_request_always_prompts() {
            let (g, state) = gate_with(true, true, "per-request", "4h", false);
            assert_eq!(approve(&g, "srv", "web").await.decided_by, "touchid");
            assert_eq!(approve(&g, "srv", "web").await.decided_by, "touchid");
            assert_eq!(state.calls.load(Ordering::SeqCst), 2);
        }

        #[tokio::test]
        async fn unknown_scope_always_prompts() {
            // An empty / unrecognized scope has no cache arm → always prompt.
            for scope in ["", "bogus"] {
                let (g, state) = gate_with(true, true, scope, "4h", false);
                approve(&g, "srv", "web").await;
                approve(&g, "srv", "web").await;
                assert_eq!(state.calls.load(Ordering::SeqCst), 2, "scope {scope:?}");
            }
        }

        #[tokio::test]
        async fn approve_populates_both_caches() {
            // Go writes BOTH caches on approve, regardless of scope (per-request here).
            let (g, _) = gate_with(true, true, "per-request", "4h", false);
            approve(&g, "srv", "web").await;
            let c = g.cache.lock().await;
            assert!(c.last_approval.is_some());
            assert!(c.shed_approvals.contains_key("srv/web"));
        }

        #[tokio::test]
        async fn bad_session_ttl_defaults_to_4h() {
            // (M3) parse-fail → the parsed cache TTL = 4h, BUT the approve outcome's
            // ttl is the RAW `"nonsense"` text (`out.TTL = cfg.SessionTTL`), verbatim.
            let (g, _) = gate_with(true, true, "per-request", "nonsense", false);
            assert_eq!(g.ttl, Duration::from_secs(4 * 60 * 60));
            let out = approve(&g, "srv", "web").await;
            assert_eq!(out.ttl.as_deref(), Some("nonsense"));
        }

        #[tokio::test]
        async fn prompt_string_matches_go() {
            let (g, state) = gate_with(true, true, "per-request", "4h", false);
            approve(&g, "srv", "web").await;
            assert_eq!(
                state.last_reason.lock().unwrap().as_deref(),
                Some("shed-extensions: SSH sign request (server: srv, shed: web)")
            );
        }

        #[test]
        fn method_reflects_allow_password() {
            let (g, _) = gate_with(true, true, "per-request", "4h", true);
            assert_eq!(g.method(), POLICY_BIOMETRICS_OR_PASSWORD);
            let (g, _) = gate_with(true, true, "per-request", "4h", false);
            assert_eq!(g.method(), POLICY_BIOMETRICS);
        }
    }
}

// Only `new_biometric_gate`'s macOS body references these; the gate module's own
// tests reach them via `super::`, so scope the re-export to macOS to avoid an
// unused-import warning in the Linux `test` build.
#[cfg(target_os = "macos")]
use gate::{allow_password, TouchIdGate};

// The real ObjC gate — macOS-only (objc2 links LocalAuthentication / Foundation at the
// linker level). A near-verbatim adaptation of the proven Tauri B3 `RealLocalAuth`,
// but returning Go's COARSE bool (no `map_la_error`).
#[cfg(target_os = "macos")]
mod real {
    use block2::RcBlock;
    use objc2::runtime::Bool;
    use objc2_foundation::{NSError, NSString};
    use objc2_local_authentication::{LAContext, LAPolicy};

    use super::gate::LocalAuth;

    /// `allow_password` (biometrics-or-password) → `DeviceOwnerAuthentication`
    /// (biometrics + Apple-Watch / account-password fallback — works clamshell / no
    /// sensor); else (biometrics) → `DeviceOwnerAuthenticationWithBiometrics` (Touch-ID
    /// only). NOTE the Go sense: the Tauri reference names the inverse (`biometrics_only`).
    pub(super) fn la_policy(allow_password: bool) -> LAPolicy {
        if allow_password {
            LAPolicy::DeviceOwnerAuthentication
        } else {
            LAPolicy::DeviceOwnerAuthenticationWithBiometrics
        }
    }

    pub(super) struct RealLocalAuth;

    #[async_trait::async_trait]
    impl LocalAuth for RealLocalAuth {
        fn can_evaluate(&self, allow_password: bool) -> bool {
            // `canEvaluatePolicy` is a cheap thread-safe read; `Err` = not enrolled /
            // no passcode → the gate reports "not available" without ever prompting.
            let ctx = unsafe { LAContext::new() };
            unsafe { ctx.canEvaluatePolicy_error(la_policy(allow_password)) }.is_ok()
        }

        async fn evaluate(&self, allow_password: bool, reason: &str) -> bool {
            let policy = la_policy(allow_password);
            let reason = reason.to_string();
            // The `LAContext` + reply block are `!Send` and the reply lands on an
            // arbitrary GCD thread — so confine ALL ObjC to one blocking thread and
            // hand back only the (Send) bool. `TouchIdGate` holds no ObjC field, so the
            // async future stays Send; `ctx` lives on the blocking thread's stack, kept
            // alive by `recv_timeout` until the reply fires.
            tokio::task::spawn_blocking(move || {
                let (tx, rx) = std::sync::mpsc::channel::<bool>();
                let ctx = unsafe { LAContext::new() };
                let reason = NSString::from_str(&reason);
                // Go's COARSE map: `success` → approved; ANY failure (every `LAError`,
                // e.g. user cancel / auth-failed / lockout) → denied. Deliberately NO
                // per-error branch (no `map_la_error`, L1) — the wire only sees the bool.
                let reply = RcBlock::new(move |success: Bool, _error: *mut NSError| {
                    let _ = tx.send(success.as_bool());
                });
                unsafe { ctx.evaluatePolicy_localizedReason_reply(policy, &reason, &reply) };
                // 120s bound: a DOCUMENTED, SAFER divergence from Go's
                // `DISPATCH_TIME_FOREVER` (M1). Go never times out, so a hung reply
                // would wedge the gate AND hold the mutex forever; the bound caps the
                // mutex hold. On timeout, cancel the lingering OS prompt and fail closed
                // (denied). Go emits no timeout outcome, so there is no wire-parity
                // conflict. Generous vs a human interaction (seconds); the request's own
                // TTL expires it upstream regardless.
                match rx.recv_timeout(std::time::Duration::from_secs(120)) {
                    Ok(v) => v,
                    Err(_) => {
                        unsafe { ctx.invalidate() };
                        false
                    }
                }
            })
            .await
            .unwrap_or(false) // task-fail → deny-safe
        }
    }

    #[cfg(test)]
    mod tests {
        use super::la_policy;
        use objc2_local_authentication::LAPolicy;

        #[test]
        fn la_policy_maps_biometrics_and_password() {
            // allow_password=true → DeviceOwnerAuthentication (password fallback);
            // false → WithBiometrics (Touch-ID only). The Go LAPolicy map, verbatim.
            assert_eq!(la_policy(true), LAPolicy::DeviceOwnerAuthentication);
            assert_eq!(
                la_policy(false),
                LAPolicy::DeviceOwnerAuthenticationWithBiometrics
            );
        }
    }
}

// The non-macOS deny-all twin (`touchid_stub.go`) — a fail-closed unit that runs on
// Linux CI (where the biometric arm routes to `DenyAllGate`).
#[cfg(all(test, not(target_os = "macos")))]
mod stub_tests {
    use super::new_biometric_gate;
    use crate::config::POLICY_DENY_ALL;

    #[tokio::test]
    async fn non_mac_gate_is_deny_all() {
        let g = new_biometric_gate("biometrics", "per-session", "4h");
        assert_eq!(g.method(), POLICY_DENY_ALL);
        let out = g
            .approve("ssh-agent", "sign", "srv", "web", "SSH sign request")
            .await;
        assert!(!out.approved);
    }
}
