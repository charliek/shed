//! What the client knows about each cloud shed's extend while one is in flight or
//! its outcome is unknown (plan 041), toolkit-free so every shell shares it.
//!
//! An extend whose every send ended with no answer or a 5xx may or may not have
//! moved the lease. It is never sent again under its request id (the account
//! service keeps an answer for a limited time, and a shed created again under the
//! name is another shed); it is settled by observation instead: what the shed's
//! listings show next. While it is unsettled, every extend of that shed is refused
//! here, unsent. Each record belongs to one incarnation of a shed (its server, its
//! name and its creation time), so a shed created again under the name never
//! inherits it.
//!
//! The rules ([`settled`]):
//! * applied: the lease's end is at least its end before the extend plus the hours;
//! * not applied: at least [`EXTEND_SETTLE_S`] after the unknown outcome the lease
//!   still ends where it did before;
//! * the shed is gone from its server's listing, or is another incarnation;
//! * [`EXTEND_UNKNOWN_MAX_S`] after the unknown outcome, whatever the listing says.
//!
//! A listing that fails (authentication, transport, any error) settles nothing.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use shed_core::models::Shed;

use crate::cloud::secs;

/// How long after an unknown outcome a lease still at its old end means the extend
/// was not applied: two of the server's sweeps (60 s each) and a margin.
pub const EXTEND_SETTLE_S: i64 = 150;

/// The longest an unknown outcome is kept: the account service keeps an extend's
/// answer this long, and nothing after it says more.
pub const EXTEND_UNKNOWN_MAX_S: i64 = 24 * 60 * 60;

/// The time now in unix seconds; injected so the rules can be tested.
pub type Clock = Arc<dyn Fn() -> i64 + Send + Sync>;

/// The system clock.
pub fn system_clock() -> Clock {
    Arc::new(|| {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| i64::try_from(d.as_secs()).unwrap_or(i64::MAX))
    })
}

/// A shed's incarnation: its creation time as its server sent it ("" when none).
pub fn incarnation(shed: &Shed) -> String {
    shed.created_at.clone().unwrap_or_default()
}

/// A shed's lease end in unix seconds, when known.
pub fn lease_end(shed: &Shed) -> Option<i64> {
    shed.cloud
        .as_ref()?
        .lease
        .as_ref()
        .and_then(|l| secs(&l.expires_at))
}

/// An extend whose outcome is not known.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UnknownExtend {
    /// The shed's incarnation ([`incarnation`]) when it was sent.
    pub incarnation: String,
    pub hours: u32,
    /// The lease's end before it was sent (unix seconds), when known.
    pub before: Option<i64>,
    /// The request id its own sends went under.
    pub request_id: String,
    /// When its outcome was found unknown (unix seconds).
    pub at: i64,
}

/// Whether `u` is settled by what a listing shows: `observed` is the shed as its
/// server's listing has it now (`None`: the listing has no shed of that name),
/// `now` the time, `settle_s` the not-applied wait ([`EXTEND_SETTLE_S`]).
pub fn settled(u: &UnknownExtend, observed: Option<&Shed>, now: i64, settle_s: i64) -> bool {
    if now - u.at >= EXTEND_UNKNOWN_MAX_S {
        return true;
    }
    let Some(shed) = observed else {
        return true; // gone
    };
    if incarnation(shed) != u.incarnation {
        return true; // another shed under the name
    }
    let (Some(before), Some(end)) = (u.before, lease_end(shed)) else {
        return false;
    };
    let applied = end >= before + i64::from(u.hours) * 3600;
    let not_applied = end == before && now - u.at >= settle_s;
    applied || not_applied
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum Slot {
    /// An extend is being sent (its own retries run under one request id).
    InFlight,
    Unknown(UnknownExtend),
}

/// Each shed's extend in flight or with an unknown outcome, by (server, shed name).
pub struct ExtendBook {
    slots: Mutex<HashMap<(String, String), Slot>>,
    clock: Clock,
    settle_s: i64,
}

impl Default for ExtendBook {
    fn default() -> Self {
        Self::with_clock(system_clock())
    }
}

/// Why [`ExtendBook::begin`] refused: an earlier extend of the shed is in flight or
/// has an unknown outcome.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Busy;

impl ExtendBook {
    pub fn with_clock(clock: Clock) -> Self {
        Self {
            slots: Mutex::default(),
            clock,
            settle_s: EXTEND_SETTLE_S,
        }
    }

    /// This book with another not-applied wait (a test seam; at least 1 s).
    pub fn with_settle_s(mut self, settle_s: i64) -> Self {
        self.settle_s = settle_s.max(1);
        self
    }

    pub fn now(&self) -> i64 {
        (self.clock)()
    }

    /// Whether an extend of `name` on `host` is in flight (the cheap check before
    /// the shed is read).
    pub fn in_flight(&self, host: &str, name: &str) -> bool {
        matches!(
            self.slots.lock().unwrap().get(&key(host, name)),
            Some(Slot::InFlight)
        )
    }

    /// Whether `name` on `host` has an unknown outcome (in flight or not).
    pub fn has_unknown(&self, host: &str, name: &str) -> bool {
        matches!(
            self.slots.lock().unwrap().get(&key(host, name)),
            Some(Slot::Unknown(_))
        )
    }

    /// Start an extend of `observed` (the shed as just read) on `host`: an earlier
    /// unknown outcome is first settled by `observed` if it can be; one still
    /// unsettled, or an extend in flight, refuses ([`Busy`]). On `Ok` the slot is in
    /// flight until [`Self::finish`].
    pub fn begin(&self, host: &str, observed: &Shed) -> Result<(), Busy> {
        let now = self.now();
        let mut slots = self.slots.lock().unwrap();
        let k = key(host, &observed.name);
        match slots.get(&k) {
            Some(Slot::InFlight) => return Err(Busy),
            Some(Slot::Unknown(u)) if !settled(u, Some(observed), now, self.settle_s) => {
                return Err(Busy)
            }
            _ => {}
        }
        slots.insert(k, Slot::InFlight);
        Ok(())
    }

    /// End the extend [`Self::begin`] started: `unknown` records its unknown
    /// outcome (stamped now); `None` (an answer, or nothing sent) frees the slot.
    pub fn finish(&self, host: &str, name: &str, unknown: Option<UnknownExtend>) {
        let mut slots = self.slots.lock().unwrap();
        let k = key(host, name);
        if slots.get(&k) != Some(&Slot::InFlight) {
            return;
        }
        match unknown {
            Some(u) => {
                let at = self.now();
                slots.insert(k, Slot::Unknown(UnknownExtend { at, ..u }));
            }
            None => {
                slots.remove(&k);
            }
        }
    }

    /// Settle what `host`'s listing (`rows`, every shed it listed) shows. Called only
    /// for a listing that succeeded: a failed one settles nothing.
    pub fn observe(&self, host: &str, rows: &[Shed]) {
        let now = self.now();
        let mut slots = self.slots.lock().unwrap();
        slots.retain(|(h, name), slot| match slot {
            Slot::Unknown(u) if h == host => {
                let shed = rows.iter().find(|s| &s.name == name);
                !settled(u, shed, now, self.settle_s)
            }
            _ => true,
        });
    }

    /// Every unknown outcome: (server, shed) to (incarnation, hours).
    pub fn snapshot(&self) -> HashMap<(String, String), (String, u32)> {
        self.slots
            .lock()
            .unwrap()
            .iter()
            .filter_map(|(k, s)| match s {
                Slot::Unknown(u) => Some((k.clone(), (u.incarnation.clone(), u.hours))),
                Slot::InFlight => None,
            })
            .collect()
    }

    /// The hours of `name`'s extend on `host` whose outcome is not known, if any.
    pub fn unknown_hours(&self, host: &str, name: &str) -> Option<u32> {
        match self.slots.lock().unwrap().get(&key(host, name)) {
            Some(Slot::Unknown(u)) => Some(u.hours),
            _ => None,
        }
    }
}

fn key(host: &str, name: &str) -> (String, String) {
    (host.to_string(), name.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicI64, Ordering};

    const T0: i64 = 1_764_000_000;
    const BEFORE: i64 = T0 + 5 * 3600;

    fn rfc(t: i64) -> String {
        chrono::DateTime::from_timestamp(t, 0)
            .unwrap()
            .format("%Y-%m-%dT%H:%M:%SZ")
            .to_string()
    }

    fn shed(name: &str, created: &str, end: Option<i64>) -> Shed {
        let lease = end.map(|e| {
            serde_json::json!({"expires_at": rfc(e), "max_expires_at": rfc(e + 100 * 3600),
                               "extendable": true})
        });
        serde_json::from_value(serde_json::json!({
            "name": name, "host": "cloud-a", "status": "running", "backend": "remote",
            "created_at": created, "cloud": {"size": {"name": "standard"}, "lease": lease},
        }))
        .unwrap()
    }

    fn unknown(hours: u32, at: i64) -> UnknownExtend {
        UnknownExtend {
            incarnation: "c1".into(),
            hours,
            before: Some(BEFORE),
            request_id: "6f9619ff-8b86-4d01-b42d-00cf4fc964ff".into(),
            at,
        }
    }

    #[test]
    fn applied_when_the_lease_moved_by_the_hours() {
        let u = unknown(6, T0);
        let moved = shed("w", "c1", Some(BEFORE + 6 * 3600));
        assert!(settled(&u, Some(&moved), T0 + 1, EXTEND_SETTLE_S));
        // Further still (another extend landed too): applied.
        let further = shed("w", "c1", Some(BEFORE + 9 * 3600));
        assert!(settled(&u, Some(&further), T0 + 1, EXTEND_SETTLE_S));
        // Moved by less: not decided.
        let less = shed("w", "c1", Some(BEFORE + 3 * 3600));
        assert!(!settled(&u, Some(&less), T0 + 1000, EXTEND_SETTLE_S));
    }

    #[test]
    fn not_applied_only_once_the_settle_time_has_passed() {
        let u = unknown(6, T0);
        let same = shed("w", "c1", Some(BEFORE));
        assert!(!settled(&u, Some(&same), T0, EXTEND_SETTLE_S));
        assert!(!settled(
            &u,
            Some(&same),
            T0 + EXTEND_SETTLE_S - 1,
            EXTEND_SETTLE_S
        ));
        assert!(settled(
            &u,
            Some(&same),
            T0 + EXTEND_SETTLE_S,
            EXTEND_SETTLE_S
        ));
    }

    #[test]
    fn gone_or_another_incarnation_settles_it() {
        let u = unknown(6, T0);
        assert!(settled(&u, None, T0, EXTEND_SETTLE_S));
        let again = shed("w", "c2", Some(BEFORE));
        assert!(settled(&u, Some(&again), T0, EXTEND_SETTLE_S));
    }

    #[test]
    fn an_unknown_lease_waits_for_the_age_limit() {
        let mut u = unknown(6, T0);
        let no_lease = shed("w", "c1", None);
        assert!(!settled(&u, Some(&no_lease), T0 + 3600, EXTEND_SETTLE_S));
        u.before = None;
        let same = shed("w", "c1", Some(BEFORE));
        assert!(!settled(&u, Some(&same), T0 + 3600, EXTEND_SETTLE_S));
        assert!(settled(
            &u,
            Some(&same),
            T0 + EXTEND_UNKNOWN_MAX_S,
            EXTEND_SETTLE_S
        ));
        assert!(!settled(
            &u,
            Some(&same),
            T0 + EXTEND_UNKNOWN_MAX_S - 1,
            EXTEND_SETTLE_S
        ));
    }

    fn book() -> (ExtendBook, Arc<AtomicI64>) {
        let t = Arc::new(AtomicI64::new(T0));
        let c = t.clone();
        (
            ExtendBook::with_clock(Arc::new(move || c.load(Ordering::SeqCst))),
            t,
        )
    }

    #[test]
    fn the_book_refuses_while_unknown_and_settles_by_listing() {
        let (b, t) = book();
        let w = shed("w", "c1", Some(BEFORE));
        b.begin("cloud-a", &w).unwrap();
        assert_eq!(b.begin("cloud-a", &w), Err(Busy), "in flight");
        assert!(b.in_flight("cloud-a", "w"));
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        assert_eq!(b.unknown_hours("cloud-a", "w"), Some(6));
        assert_eq!(
            b.snapshot()[&("cloud-a".into(), "w".into())],
            ("c1".to_string(), 6)
        );
        // Still at its old end, too soon: refused, kept.
        assert_eq!(b.begin("cloud-a", &w), Err(Busy));
        b.observe("cloud-a", std::slice::from_ref(&w));
        assert!(b.has_unknown("cloud-a", "w"));
        // Another server's listing settles nothing here.
        b.observe("other", &[]);
        assert!(b.has_unknown("cloud-a", "w"));
        // After the settle time the same end: not applied, settled.
        t.store(T0 + EXTEND_SETTLE_S, Ordering::SeqCst);
        b.observe("cloud-a", std::slice::from_ref(&w));
        assert!(!b.has_unknown("cloud-a", "w"));
        assert!(b.snapshot().is_empty());
    }

    #[test]
    fn a_begin_settles_an_applied_one_by_the_shed_it_read() {
        let (b, _) = book();
        let w = shed("w", "c1", Some(BEFORE));
        b.begin("cloud-a", &w).unwrap();
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        let moved = shed("w", "c1", Some(BEFORE + 6 * 3600));
        b.begin("cloud-a", &moved).unwrap();
        assert!(b.in_flight("cloud-a", "w"));
        b.finish("cloud-a", "w", None);
        assert!(b.snapshot().is_empty());
    }

    #[test]
    fn a_new_incarnation_or_a_gone_shed_clears_it() {
        let (b, _) = book();
        let w = shed("w", "c1", Some(BEFORE));
        for observed in [vec![], vec![shed("w", "c2", Some(BEFORE))]] {
            b.begin("cloud-a", &w).unwrap();
            b.finish("cloud-a", "w", Some(unknown(6, 0)));
            b.observe("cloud-a", &observed);
            assert!(!b.has_unknown("cloud-a", "w"), "{observed:?}");
        }
        // A begin for another incarnation is not refused.
        b.begin("cloud-a", &w).unwrap();
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        b.begin("cloud-a", &shed("w", "c2", Some(BEFORE))).unwrap();
    }

    #[test]
    fn the_age_limit_clears_it() {
        let (b, t) = book();
        let w = shed("w", "c1", None);
        b.begin("cloud-a", &w).unwrap();
        b.finish(
            "cloud-a",
            "w",
            Some(UnknownExtend {
                before: None,
                ..unknown(6, 0)
            }),
        );
        t.store(T0 + EXTEND_UNKNOWN_MAX_S - 1, Ordering::SeqCst);
        b.observe("cloud-a", std::slice::from_ref(&w));
        assert!(b.has_unknown("cloud-a", "w"));
        t.store(T0 + EXTEND_UNKNOWN_MAX_S, Ordering::SeqCst);
        b.observe("cloud-a", std::slice::from_ref(&w));
        assert!(!b.has_unknown("cloud-a", "w"));
    }

    #[test]
    fn a_finish_without_a_begin_changes_nothing() {
        let (b, _) = book();
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        assert!(b.snapshot().is_empty());
    }
}
