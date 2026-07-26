//! The client's in-memory record of which credential shape each configured
//! server issues (plan 002 §7 P1).
//!
//! # Why in memory, and nowhere else
//!
//! `~/.shed/config.yaml` is CLI-owned. This layer's parser is read-only and
//! lossy (it models a subset of the keys), so writing a learned mode back would
//! drop every key it does not model, race the CLI, and wake the config watcher
//! into rebuilding clients mid-mint. The cost of not persisting is one silent
//! re-bootstrap on a cold launch against a server that flipped — which is the
//! trade plan 002 pins for BOTH desktop clients (the Swift app's
//! `CredentialModeObserver` is the same decision in the other language).
//!
//! # The three writers, and why there are three
//!
//! One registry serves every server, and three sources feed it:
//!
//!   1. **config** — [`Self::seed_from_config`] at Backend build: the entry's
//!      `auth_mode`, the CLI's cache of what the server last issued. Absent
//!      means token (an entry written before certificates existed).
//!   2. **the minter, synchronously** — [`Self::record`] as a
//!      `credential.response` is mapped, BEFORE the core has adopted anything.
//!      This is what closes the TOCTOU the observer alone would leave: the
//!      observer fires on the core's dispatcher thread, i.e. possibly after the
//!      provider has already asked `supports_mtls()` again.
//!   3. **the core's `CredentialObserver`** — the sanctioned §7 P1 event, and
//!      the only one that reports what was actually ADOPTED (a mint the core
//!      then refused never reaches it).
//!
//! # Ordering: why the observer cannot walk back the minter
//!
//! Writers 2 and 3 are NOT interchangeable, and "last write wins" is wrong.
//! The observer fires on the core's dispatcher, so mint N's callback can land
//! AFTER mint N+1's synchronous write — and if mint N+1 is the one that learned
//! the server flipped to certificates, a late N callback saying `token` would
//! walk it back. The very next mint would then read `expects_mtls == false`,
//! skip the CSR, and send a `token.get` to an mtls-only server: exactly the §7
//! P5 bug the tri-state exists to prevent. (The inverse interleaving produces
//! the mirror image — a false "upgrade shed-host-agent" against a token server.)
//!
//! [`CredentialAdopted`] carries no ordering token, so the rule is a priority
//! one, and it is sound by construction:
//!
//! * every credential the observer can report was produced by a mint that
//!   [`AuthModeRegistry::record`]ed its shape SYNCHRONOUSLY first, so each
//!   observer event has a synchronous write that happens-before it;
//! * therefore, if an observer event DISAGREES with the latest synchronous
//!   write, a newer mint has already superseded it — the event is stale, and it
//!   is dropped ([`AuthModeRegistry::record_observed`]);
//! * where no synchronous write exists at all (the embedded/headless-coexist
//!   brokers, whose `EmbeddedTokenMinter` holds no registry), the observer is
//!   the only writer and always applies.
//!
//! The per-server `sync_seq` is the monotonic ordinal of the last synchronous
//! write. It is what makes "has the minter spoken for this server, and how
//! recently" answerable rather than inferred, and it is surfaced to tests.

use std::collections::HashMap;
use std::sync::Mutex;

use shed_core::config::ShedConfig;
use shed_core::token::{AuthMode, CredentialAdopted, CredentialObserver};

/// What is known about one server's credential shape.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct AuthModeState {
    pub mode: AuthMode,
    /// `true` once a mint has actually produced this shape in THIS session;
    /// `false` while the value is still just the config entry's cached claim.
    pub learned: bool,
}

/// Per-server credential-mode state, shared by the minter (which reads it to
/// decide whether to expect a certificate) and the app surface (which renders
/// it). Interior-mutable so it can be handed to the minter BEFORE the Backend
/// that seeds it from config exists — the launch order the embedded broker
/// already imposes.
#[derive(Default)]
pub struct AuthModeRegistry {
    inner: Mutex<Inner>,
}

#[derive(Default)]
struct Inner {
    modes: HashMap<String, Entry>,
    /// Hands out the monotonic ordinals stamped on synchronous writes. Shared
    /// across servers (one clock, not one per name) so the ordering is total.
    next_seq: u64,
}

#[derive(Clone, Copy)]
struct Entry {
    state: AuthModeState,
    /// The ordinal of the last SYNCHRONOUS ([`AuthModeRegistry::record`]) write,
    /// or `0` when only config/the observer has ever spoken for this server.
    sync_seq: u64,
}

impl AuthModeRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    /// Seed from the parsed config. Only fills entries nothing has been LEARNED
    /// for: a config reload must not walk back what this session proved (the
    /// CLI writes `auth_mode` at its own bootstrap, so its copy can be older
    /// than ours).
    pub fn seed_from_config(&self, config: &ShedConfig) {
        let mut inner = self.inner.lock().unwrap();
        for s in &config.servers {
            let e = inner.modes.entry(s.name.clone()).or_insert(Entry {
                state: AuthModeState {
                    mode: s.auth_mode(),
                    learned: false,
                },
                sync_seq: 0,
            });
            if !e.state.learned {
                e.state.mode = s.auth_mode();
            }
        }
    }

    /// Record a mode the minter observed SYNCHRONOUSLY, as it maps a
    /// `credential.response` arm. Always wins — over a config seed, over an
    /// earlier synchronous write, and (see the module docs) over any observer
    /// callback that disagrees with it.
    ///
    /// Returns the ordinal stamped on the write, so a caller can reason about
    /// (and a test can assert) the ordering.
    pub fn record(&self, server: &str, mode: AuthMode) -> u64 {
        let mut inner = self.inner.lock().unwrap();
        inner.next_seq += 1;
        let seq = inner.next_seq;
        inner.modes.insert(
            server.to_string(),
            Entry {
                state: AuthModeState {
                    mode,
                    learned: true,
                },
                sync_seq: seq,
            },
        );
        seq
    }

    /// Record a mode reported by the core's [`CredentialObserver`] — a write
    /// that is strictly LOWER priority than [`Self::record`].
    ///
    /// Applied only when it cannot be stale: either the minter has never spoken
    /// for this server (the embedded brokers, where the observer is the only
    /// writer), or it agrees with what the minter last said. A disagreement
    /// means a newer synchronous write already superseded the mint this event
    /// describes, so the event is dropped rather than allowed to walk it back.
    ///
    /// Returns whether it applied (for the tests and for symmetry with
    /// [`Self::record`]'s ordinal).
    pub fn record_observed(&self, server: &str, mode: AuthMode) -> bool {
        let mut inner = self.inner.lock().unwrap();
        if let Some(e) = inner.modes.get_mut(server) {
            if e.sync_seq != 0 && e.state.mode != mode {
                return false; // stale: mint N's callback, after mint N+1's write
            }
            e.state = AuthModeState {
                mode,
                learned: true,
            };
            return true;
        }
        inner.modes.insert(
            server.to_string(),
            Entry {
                state: AuthModeState {
                    mode,
                    learned: true,
                },
                sync_seq: 0,
            },
        );
        true
    }

    /// The state known for `server`, or `None` for a name nothing has been
    /// recorded or configured for.
    pub fn get(&self, server: &str) -> Option<AuthModeState> {
        self.inner
            .lock()
            .unwrap()
            .modes
            .get(server)
            .map(|e| e.state)
    }

    /// Does `server` issue certificates, as far as anything here knows?
    pub fn expects_mtls(&self, server: &str) -> bool {
        self.get(server).is_some_and(|s| s.mode == AuthMode::Mtls)
    }

    /// Does ANY known server issue certificates?
    ///
    /// The answer to a question the per-server one cannot be asked: shed-core's
    /// [`shed_core::token::TokenMinter::supports_mtls`] takes no server (it is a
    /// capability advertisement, not a preference), while one minter instance
    /// serves every configured host. Erring towards `true` costs a keypair
    /// generation and a `csr=` argument a token-mode server ignores; erring
    /// towards `false` costs a `token.get` against a server that only accepts
    /// certificates — the §7 P5 bug. So the OR is the safe direction.
    pub fn any_expects_mtls(&self) -> bool {
        self.inner
            .lock()
            .unwrap()
            .modes
            .values()
            .any(|e| e.state.mode == AuthMode::Mtls)
    }

    /// Every known server + its state, sorted by name (a stable wire order for
    /// the app surface / the harness).
    pub fn snapshot(&self) -> Vec<(String, AuthModeState)> {
        let mut v: Vec<_> = self
            .inner
            .lock()
            .unwrap()
            .modes
            .iter()
            .map(|(k, e)| (k.clone(), e.state))
            .collect();
        v.sort_by(|a, b| a.0.cmp(&b.0));
        v
    }

    /// The ordinal of the last synchronous write for `server` (`0` = none).
    #[cfg(test)]
    fn sync_seq(&self, server: &str) -> u64 {
        self.inner
            .lock()
            .unwrap()
            .modes
            .get(server)
            .map_or(0, |e| e.sync_seq)
    }
}

/// The §7 P1 consumer: every adoption updates the learned mode, and NOTHING is
/// persisted. The event's `token` is deliberately dropped — this client's
/// storage is not the token's home (that is `~/.shed/config.yaml`, CLI-owned).
impl CredentialObserver for AuthModeRegistry {
    fn on_credential_adopted(&self, event: &CredentialAdopted) {
        // NOT `record`: this arrives on the core's dispatcher and may be older
        // than a synchronous minter write. See the module docs' ordering rule.
        self.record_observed(&event.server, event.mode);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn config(yaml: &str) -> ShedConfig {
        ShedConfig::parse(yaml)
    }

    #[test]
    fn config_seed_reads_auth_mode_with_absent_meaning_token() {
        let r = AuthModeRegistry::new();
        r.seed_from_config(&config(
            "servers:\n  a:\n    host: a\n    auth_mode: mtls\n  b:\n    host: b\n",
        ));
        assert_eq!(
            r.get("a"),
            Some(AuthModeState {
                mode: AuthMode::Mtls,
                learned: false
            })
        );
        assert_eq!(
            r.get("b"),
            Some(AuthModeState {
                mode: AuthMode::Token,
                learned: false
            })
        );
        assert!(r.expects_mtls("a"));
        assert!(!r.expects_mtls("b"));
        assert!(r.any_expects_mtls());
        assert!(!r.expects_mtls("never-heard-of-it"));
    }

    #[test]
    fn a_learned_mode_survives_a_config_reseed() {
        // The flip case: the server now issues certificates, the CLI has not
        // rewritten its entry yet. A config watcher re-seed must not undo it.
        let r = AuthModeRegistry::new();
        let cfg = config("servers:\n  a:\n    host: a\n");
        r.seed_from_config(&cfg);
        r.record("a", AuthMode::Mtls);
        r.seed_from_config(&cfg);
        assert_eq!(
            r.get("a"),
            Some(AuthModeState {
                mode: AuthMode::Mtls,
                learned: true
            })
        );
    }

    #[test]
    fn the_observer_records_the_adopted_mode_and_drops_the_token() {
        let r = AuthModeRegistry::new();
        r.on_credential_adopted(&CredentialAdopted {
            server: "a".into(),
            mode: AuthMode::Token,
            expires_at_unix: Some(1),
            token: Some("shed_control_secret".into()),
        });
        assert_eq!(
            r.sync_seq("a"),
            0,
            "the observer is not a synchronous write"
        );
        assert_eq!(
            r.get("a"),
            Some(AuthModeState {
                mode: AuthMode::Token,
                learned: true
            })
        );
        // Nothing here can hold the token: the registry has no field for it.
        r.on_credential_adopted(&CredentialAdopted {
            server: "a".into(),
            mode: AuthMode::Mtls,
            expires_at_unix: None,
            token: None,
        });
        assert!(r.expects_mtls("a"));
    }

    fn adopted(server: &str, mode: AuthMode) -> CredentialAdopted {
        CredentialAdopted {
            server: server.into(),
            mode,
            expires_at_unix: None,
            token: None,
        }
    }

    /// The reported interleaving, in the direction that BREAKS mtls: mint N is a
    /// token answer, mint N+1 learns the server flipped to certificates, and
    /// only then does the core's dispatcher deliver mint N's adoption.
    ///
    /// Without the guard the late callback wins, `expects_mtls` goes false, and
    /// the next forced mint skips the CSR and sends a `token.get` to a
    /// certificate-only server — the §7 P5 bug.
    #[test]
    fn a_delayed_observer_callback_cannot_walk_back_a_newer_synchronous_write() {
        let r = AuthModeRegistry::new();
        let n = r.record("a", AuthMode::Token); // mint N, synchronous
        let n1 = r.record("a", AuthMode::Mtls); // mint N+1, synchronous
        assert!(n1 > n, "synchronous writes are monotonically ordered");
        assert_eq!(r.sync_seq("a"), n1);

        // ... and NOW mint N's callback lands.
        r.on_credential_adopted(&adopted("a", AuthMode::Token));

        assert!(
            r.expects_mtls("a"),
            "a delayed mint-N callback must not walk back mint N+1's observation"
        );
        assert!(r.any_expects_mtls());
        assert_eq!(
            r.sync_seq("a"),
            n1,
            "a dropped observer write must not disturb the ordinal"
        );
    }

    /// The mirror image: mint N was mtls, mint N+1 saw the server back on
    /// tokens, and N's late callback would re-assert certificates — producing a
    /// false "upgrade shed-host-agent" against a token server.
    #[test]
    fn the_inverse_interleaving_cannot_re_assert_certificates() {
        let r = AuthModeRegistry::new();
        r.record("a", AuthMode::Mtls);
        r.record("a", AuthMode::Token);
        // Through the OBSERVER, not `record_observed` directly, so this also
        // pins the wiring: `on_credential_adopted` must take the low-priority
        // path.
        r.on_credential_adopted(&adopted("a", AuthMode::Mtls));
        assert!(!r.expects_mtls("a"));
        assert!(!r.any_expects_mtls());
        assert!(!r.record_observed("a", AuthMode::Mtls), "stale, dropped");
    }

    /// The guard is a PRIORITY rule, not a mute: an observer write that agrees
    /// with the minter still applies (it is the same fact), and where the minter
    /// has never spoken — the embedded/headless-coexist brokers, whose
    /// `EmbeddedTokenMinter` holds no registry — the observer is the only writer
    /// and always applies.
    #[test]
    fn the_observer_still_writes_when_it_is_not_contradicting_the_minter() {
        // (a) no synchronous write has ever landed for this server.
        let r = AuthModeRegistry::new();
        assert!(r.record_observed("embedded", AuthMode::Mtls));
        assert!(r.expects_mtls("embedded"));
        assert_eq!(r.sync_seq("embedded"), 0);

        // (b) a config seed is not a synchronous write either — the observer
        //     must be able to correct a stale `auth_mode` the CLI cached.
        let r = AuthModeRegistry::new();
        r.seed_from_config(&config("servers:\n  a:\n    host: a\n"));
        assert!(r.record_observed("a", AuthMode::Mtls));
        assert_eq!(
            r.get("a"),
            Some(AuthModeState {
                mode: AuthMode::Mtls,
                learned: true
            })
        );

        // (c) agreeing with the minter applies (and marks it learned).
        let r = AuthModeRegistry::new();
        r.record("a", AuthMode::Mtls);
        assert!(r.record_observed("a", AuthMode::Mtls));
        assert!(r.expects_mtls("a"));
    }

    #[test]
    fn snapshot_is_name_sorted() {
        let r = AuthModeRegistry::new();
        r.record("z", AuthMode::Token);
        r.record("a", AuthMode::Mtls);
        let names: Vec<_> = r.snapshot().into_iter().map(|(n, _)| n).collect();
        assert_eq!(names, vec!["a".to_string(), "z".to_string()]);
    }
}
