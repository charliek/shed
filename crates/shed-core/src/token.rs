//! Control-token FSM — ported from Swift's `ControlTokenProvider` actor.
//!
//! Caches a shed-server CONTROL token, refreshing it near expiry or on demand
//! (`invalidate()`, called on a 401). The mint primitive is a foreign
//! `TokenMinter` (the host agent, in Swift; a mock in tests) — this crate owns
//! only the cache/refresh/single-flight logic, so it stays pure.
//!
//! Fail-closed contract (mirrors the SDK/CLI, guarded by the tests here since
//! test mode drops the token path so e2e can't reach it): a mint failure yields
//! an error and the client then sends NO token — never a static downgrade.

use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio::sync::Mutex;

use crate::http::ShedError;

/// A minted control token plus its optional expiry (unix seconds). `None` expiry
/// → only an explicit `invalidate()` forces a refresh (mirrors `MintedToken`).
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

/// The 2h refresh window mirrors the SDK/CLI: refresh this long before expiry so
/// routine requests rarely race a 401.
const REFRESH_WINDOW: Duration = Duration::from_secs(2 * 60 * 60);

/// Caches a control token, refreshing when missing or within the refresh window
/// of expiry. Concurrent `token()` callers serialize on the mint (single-flight:
/// a late caller re-checks the cache under the lock and returns the fresh token
/// rather than minting again).
pub struct ControlTokenProvider {
    server: String,
    minter: Arc<dyn TokenMinter>,
    now_unix: fn() -> u64,
    cached: Mutex<Option<MintedToken>>,
}

impl ControlTokenProvider {
    pub fn new(server: String, minter: Arc<dyn TokenMinter>) -> Self {
        Self {
            server,
            minter,
            now_unix: default_now_unix,
            cached: Mutex::new(None),
        }
    }

    /// The current token, minting/refreshing when it is missing or near expiry.
    /// Propagates a mint failure (the caller then sends no token — fail-closed).
    pub async fn token(&self) -> Result<String, ShedError> {
        // Hold the lock across the mint so concurrent callers serialize: the
        // first mints, the rest re-check here and return its result.
        let mut cached = self.cached.lock().await;
        if let Some(t) = cached.as_ref() {
            if !self.needs_refresh(t) {
                return Ok(t.token.clone());
            }
        }
        let minted = self.minter.mint(&self.server).await?;
        if minted.token.is_empty() {
            // An empty token is a mint failure, not a usable credential — don't
            // cache it (fail-closed), even if the minter reported success.
            return Err(ShedError::Transport(
                "control-token mint returned an empty token".into(),
            ));
        }
        let token = minted.token.clone();
        *cached = Some(minted);
        Ok(token)
    }

    /// Drop the cached token so the next `token()` re-mints. Called on a 401.
    pub async fn invalidate(&self) {
        *self.cached.lock().await = None;
    }

    fn needs_refresh(&self, t: &MintedToken) -> bool {
        match t.expires_at_unix {
            None => false, // no expiry → only invalidate() refreshes
            Some(exp) => (self.now_unix)() + REFRESH_WINDOW.as_secs() >= exp,
        }
    }
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
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// A minter that counts calls and returns `tok-<n>` (or fails). Optional
    /// expiry lets a test force the refresh-window path.
    struct MockMinter {
        calls: AtomicUsize,
        fail: bool,
        expires_at_unix: Option<u64>,
        delay_ms: u64,
    }

    impl MockMinter {
        fn ok() -> Arc<Self> {
            Arc::new(Self {
                calls: AtomicUsize::new(0),
                fail: false,
                expires_at_unix: None,
                delay_ms: 0,
            })
        }
        fn failing() -> Arc<Self> {
            Arc::new(Self {
                calls: AtomicUsize::new(0),
                fail: true,
                expires_at_unix: None,
                delay_ms: 0,
            })
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
            if self.fail {
                return Err(ShedError::Transport("mint failed".into()));
            }
            Ok(MintedToken {
                token: format!("tok-{n}"),
                expires_at_unix: self.expires_at_unix,
            })
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
        // Expiry = now → always within the 2h refresh window → re-mint each call.
        let now = default_now_unix();
        let minter = Arc::new(MockMinter {
            calls: AtomicUsize::new(0),
            fail: false,
            expires_at_unix: Some(now),
            delay_ms: 0,
        });
        let p = ControlTokenProvider::new("mini2".into(), minter.clone());
        assert_eq!(p.token().await.unwrap(), "tok-1");
        assert_eq!(p.token().await.unwrap(), "tok-2");
        assert_eq!(minter.count(), 2);
    }

    #[tokio::test]
    async fn does_not_refresh_far_from_expiry() {
        let far = default_now_unix() + 10 * 60 * 60; // 10h out, beyond the 2h window
        let minter = Arc::new(MockMinter {
            calls: AtomicUsize::new(0),
            fail: false,
            expires_at_unix: Some(far),
            delay_ms: 0,
        });
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

    #[tokio::test]
    async fn concurrent_callers_mint_once() {
        // Single-flight: a slow mint + concurrent callers → exactly one mint.
        let minter = Arc::new(MockMinter {
            calls: AtomicUsize::new(0),
            fail: false,
            expires_at_unix: None,
            delay_ms: 40,
        });
        let p = Arc::new(ControlTokenProvider::new("mini2".into(), minter.clone()));
        let (a, b, c) = tokio::join!(p.token(), p.token(), p.token());
        assert_eq!(a.unwrap(), "tok-1");
        assert_eq!(b.unwrap(), "tok-1");
        assert_eq!(c.unwrap(), "tok-1");
        assert_eq!(minter.count(), 1);
    }
}
