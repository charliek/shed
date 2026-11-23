//! The delivery stream's credential on a secure server: the credentials-scope client
//! certificate, from a [`CredentialSource`] over the durable store
//! (`<state>/broker/creds`) the broker's bus uses too.
//!
//! * **Fenced.** The source is built for one stream task ([`SourceCredentials::new`]'s
//!   `fence`, the task's stop signal): once the task is stopped or replaced (its server's
//!   target changed), the source never writes to the store again, checked under the
//!   store's lock, so a mint that finishes late cannot overwrite what the replacement
//!   stored.
//! * **One mint at a time.** Every credential source in the process takes a turn per
//!   store, server and scope around its mint and the store write, and re-reads the store
//!   first: a certificate another source (the broker's bus, or a replaced task) stored
//!   meanwhile is adopted, not minted over (`minter::CredentialSource`'s mint).
//! * **One certificate per stream.** [`StreamCredentials::certificate`] is the
//!   certificate armed when [`StreamCredentials::ready`] returned; the stream client
//!   fixes it on that stream's transport. [`StreamCredentials::needs_refresh`] says when
//!   the source would mint (its refresh window, or a refusal), and the stream is ended so
//!   the next opens renewed.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use rustls::sign::CertifiedKey;
use tokio::sync::watch;

use crate::delivery::StreamCredentials;
use crate::minter::{new_group_credential_source, CredentialSource, Minter, SCOPE_CREDENTIALS};
use crate::target::ServerTarget;

/// A secure server's stream credential.
pub struct SourceCredentials {
    source: Arc<CredentialSource>,
    refused: AtomicBool,
}

impl SourceCredentials {
    /// The production credential for `target`: credentials scope, minted over SSH by
    /// `minter`, stored in the durable store, never written once `fence` reads `true`.
    pub fn new(
        minter: Arc<dyn Minter>,
        target: ServerTarget,
        fence: watch::Receiver<bool>,
    ) -> Self {
        Self::over(new_group_credential_source(
            minter,
            target,
            SCOPE_CREDENTIALS,
            fence,
        ))
    }

    /// A credential over `source` (tests: an explicit store).
    pub fn over(source: Arc<CredentialSource>) -> Self {
        SourceCredentials {
            source,
            refused: AtomicBool::new(false),
        }
    }
}

#[async_trait::async_trait]
impl StreamCredentials for SourceCredentials {
    async fn ready(&self) -> Result<String, String> {
        if self.refused.swap(false, Ordering::SeqCst) {
            self.source.invalidate();
        }
        self.source.token_for_request().await.map_err(|e| e.message)
    }

    fn certificate(&self) -> Option<Arc<CertifiedKey>> {
        self.source.cert_resolver().current()
    }

    fn refused(&self) {
        self.refused.store(true, Ordering::SeqCst);
    }

    fn needs_refresh(&self) -> bool {
        self.refused.load(Ordering::SeqCst) || self.source.would_mint()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::credstore::CredStore;
    use crate::minter::tests::{minted_cert_for, FakeMinter, TestCa, TestUser};
    use crate::minter::{new_credential_source_with_store, new_group_credential_source_with_store};
    use crate::minter::{Minted, MinterError};
    use std::sync::atomic::AtomicUsize;
    use tokio::sync::Notify;

    fn target() -> ServerTarget {
        ServerTarget {
            name: "mini".into(),
            url: "https://mini.example.test:18443".into(),
            token: String::new(),
            tls_fingerprint: "sha256:00".into(),
            ssh_host: "mini.example.test".into(),
            ssh_port: 2222,
            auth_mode: "mtls".into(),
            user: String::new(),
            identity_file: String::new(),
        }
    }

    /// Counts its mints; each is a fresh certificate (serial = the count). With a gate,
    /// each mint waits for it.
    struct CountingMinter {
        ca: TestCa,
        mints: AtomicUsize,
        gate: Option<Arc<Notify>>,
        started: Arc<Notify>,
    }

    impl CountingMinter {
        fn new(gate: Option<Arc<Notify>>) -> Arc<Self> {
            Arc::new(CountingMinter {
                ca: TestCa::new(),
                mints: AtomicUsize::new(0),
                gate,
                started: Arc::new(Notify::new()),
            })
        }
    }

    #[async_trait::async_trait]
    impl Minter for CountingMinter {
        async fn mint(&self, _t: &ServerTarget, _s: &str) -> Result<Minted, MinterError> {
            let n = self.mints.fetch_add(1, Ordering::SeqCst) as u64;
            self.started.notify_one();
            if let Some(g) = &self.gate {
                g.notified().await;
            }
            Ok(minted_cert_for(
                &self.ca,
                n + 1,
                Some(4_102_444_800),
                &TestUser::Implicit,
            ))
        }
    }

    fn over(store: &CredStore, minter: Arc<dyn Minter>) -> SourceCredentials {
        SourceCredentials::over(new_credential_source_with_store(
            minter,
            target(),
            SCOPE_CREDENTIALS,
            Some(store.clone()),
        ))
    }

    fn stored_pem(store: &CredStore) -> Option<String> {
        store.load("mini").unwrap().map(|c| c.cert_pem)
    }

    /// The first holder mints and stores; a second holder over the same store presents
    /// the stored certificate without a mint of its own.
    #[tokio::test]
    async fn a_second_holder_reuses_the_stored_certificate() {
        let dir = tempfile::tempdir().unwrap();
        let store = CredStore::new(dir.path());
        let minter = CountingMinter::new(None);
        let a = over(&store, minter.clone());
        assert!(a.certificate().is_none());
        assert_eq!(a.ready().await.unwrap(), "");
        assert!(a.certificate().is_some(), "the certificate is armed");
        let b = over(&store, minter.clone());
        assert!(b.certificate().is_some(), "hydrated at build");
        b.ready().await.unwrap();
        assert_eq!(minter.mints.load(Ordering::SeqCst), 1, "no second mint");
    }

    /// Two holders that both need a certificate at once: one mint, and both present it.
    #[tokio::test]
    async fn two_concurrent_mints_produce_one() {
        let dir = tempfile::tempdir().unwrap();
        let store = CredStore::new(dir.path());
        let gate = Arc::new(Notify::new());
        let minter = CountingMinter::new(Some(gate.clone()));
        let a = Arc::new(over(&store, minter.clone()));
        let b = Arc::new(over(&store, minter.clone()));
        let (a2, b2) = (a.clone(), b.clone());
        let ta = tokio::spawn(async move { a2.ready().await });
        let tb = tokio::spawn(async move { b2.ready().await });
        minter.started.notified().await;
        gate.notify_one();
        ta.await.unwrap().unwrap();
        tb.await.unwrap().unwrap();
        assert_eq!(minter.mints.load(Ordering::SeqCst), 1);
        let (ca, cb) = (a.certificate().unwrap(), b.certificate().unwrap());
        assert_eq!(ca.cert, cb.cert, "the same certificate");
    }

    /// A mint whose task was replaced while it ran writes nothing: the replacement's
    /// certificate stays the stored one.
    #[tokio::test]
    async fn a_retired_tasks_slow_mint_does_not_overwrite_the_replacement() {
        let dir = tempfile::tempdir().unwrap();
        let store = CredStore::new(dir.path());
        // The replacement's certificate, already stored.
        let replacement = CountingMinter::new(None);
        over(&store, replacement.clone()).ready().await.unwrap();
        let theirs = stored_pem(&store).unwrap();

        // The retired task: it holds a refused certificate, so it mints (slowly).
        let gate = Arc::new(Notify::new());
        let slow = CountingMinter::new(Some(gate.clone()));
        let (stop, fence) = watch::channel(false);
        let retired = Arc::new(SourceCredentials::over(
            new_group_credential_source_with_store(
                slow.clone(),
                target(),
                SCOPE_CREDENTIALS,
                Some(store.clone()),
                fence,
            ),
        ));
        retired.refused();
        let r = retired.clone();
        let t = tokio::spawn(async move { r.ready().await });
        slow.started.notified().await;
        stop.send(true).unwrap(); // replaced while its mint runs
        gate.notify_one();
        let _ = t.await.unwrap();
        assert_eq!(slow.mints.load(Ordering::SeqCst), 1);
        assert_eq!(stored_pem(&store).unwrap(), theirs, "not overwritten");
    }

    /// After a 401 the next ready mints, even with a good certificate stored, and says it
    /// needs a refresh until then.
    #[tokio::test]
    async fn a_refusal_mints_afresh() {
        let dir = tempfile::tempdir().unwrap();
        let store = CredStore::new(dir.path());
        let minter = CountingMinter::new(None);
        let c = over(&store, minter.clone());
        c.ready().await.unwrap();
        assert!(!c.needs_refresh());
        c.refused();
        assert!(c.needs_refresh());
        c.ready().await.unwrap();
        assert_eq!(minter.mints.load(Ordering::SeqCst), 2);
        assert!(!c.needs_refresh());
        c.ready().await.unwrap();
        assert_eq!(minter.mints.load(Ordering::SeqCst), 2, "once per refusal");
    }

    /// A mint that fails is the ready's error.
    #[tokio::test]
    async fn a_failed_mint_is_an_error() {
        let dir = tempfile::tempdir().unwrap();
        let c = over(
            &CredStore::new(dir.path()),
            FakeMinter::new(vec![Err(false)]),
        );
        assert!(c.ready().await.is_err());
        assert!(c.certificate().is_none());
    }
}
