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
//!   ends exactly where it did before (a lease moved by fewer hours proves
//!   nothing about this extend, so that record waits for the age limit);
//! * the shed is gone from its server's listing, or is another incarnation;
//! * [`EXTEND_UNKNOWN_MAX_S`] after the unknown outcome, whatever the listing says.
//!
//! A listing that fails (authentication, transport, any error) settles nothing, and
//! a listing settles only the outcomes recorded before it was sent
//! ([`ExtendBook::listing_mark`]): one sent earlier may not list a shed newer than
//! it, which would look gone.
//!
//! An extend takes its shed's turn ([`ExtendBook::reserve`]) before it reads the
//! lease's end it is measured from, so no other extend of the shed can move the
//! lease between that read and the send.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use shed_core::models::Shed;

use crate::cloud::secs;

/// How long after an unknown outcome a lease whose end is still exactly its old end
/// means the extend was not applied: two of the server's sweeps (60 s each) and a
/// margin. The time passing settles nothing by itself.
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
    let target = before + i64::from(u.hours) * 3600;
    let applied = end >= target;
    // Not applied only when the lease has not moved at all: a lease that moved
    // by fewer hours (another client's extend) proves nothing about this one,
    // and the server's listing can be stale while the service is unreachable,
    // so such a record waits for the age limit rather than risk a second add.
    let not_applied = end == before && now - u.at >= settle_s;
    applied || not_applied
}

/// An unknown outcome as the book keeps it: the record and its place in the order
/// outcomes were recorded in, which a listing's mark ([`ExtendBook::listing_mark`])
/// is compared with.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Recorded {
    u: UnknownExtend,
    seq: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum Slot {
    /// An extend's turn is taken: the shed is being read, or the extend sent (its
    /// own retries run under one request id). `held` is an earlier extend's unknown
    /// outcome, kept while the read that may settle it is made. `reread`: a listing
    /// settled `held` while the read was out, so that read may predate the lease
    /// moving and is no baseline ([`ExtendBook::admit`] asks for another).
    InFlight {
        held: Option<Recorded>,
        reread: bool,
    },
    Unknown(Recorded),
}

#[derive(Default)]
struct Slots {
    by_shed: HashMap<(String, String), Slot>,
    /// How many unknown outcomes have been recorded: the last one's `seq`.
    recorded: u64,
}

/// Each shed's extend in flight or with an unknown outcome, by (server, shed name).
pub struct ExtendBook {
    slots: Mutex<Slots>,
    clock: Clock,
    settle_s: i64,
}

impl Default for ExtendBook {
    fn default() -> Self {
        Self::with_clock(system_clock())
    }
}

/// Why [`ExtendBook::reserve`] or [`ExtendBook::admit`] refused: an earlier extend
/// of the shed is in flight or has an unknown outcome.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Busy;

/// Why [`ExtendBook::admit`] did not let an extend through.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NotAdmitted {
    /// An earlier extend's outcome is still unknown (put back), or the turn is not
    /// this extend's: nothing may be sent.
    Busy,
    /// A listing settled the earlier extend while the shed was being read: that read
    /// may predate the lease moving. The turn is kept; read the shed again and admit
    /// that read instead.
    Reread,
}

/// What a listing is compared with ([`ExtendBook::listing_mark`]): the outcomes
/// recorded before it was sent, and when it was sent (the time the not-applied wait
/// is measured to: the server read the lease no earlier than that).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ListingMark {
    seq: u64,
    at: i64,
}

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

    /// Whether an extend of `name` on `host` has its turn (being read or sent).
    pub fn in_flight(&self, host: &str, name: &str) -> bool {
        matches!(
            self.slots.lock().unwrap().by_shed.get(&key(host, name)),
            Some(Slot::InFlight { .. })
        )
    }

    /// Whether `name` on `host` has an unknown outcome (held by a read or not).
    pub fn has_unknown(&self, host: &str, name: &str) -> bool {
        self.recorded(host, name).is_some()
    }

    /// Take the turn of an extend of `name` on `host`, before the shed is read: an
    /// extend in flight refuses ([`Busy`]); an earlier unknown outcome is held, to
    /// be settled by that read ([`Self::admit`]) or put back ([`Self::release`]).
    /// Taken before the read, so no other extend can move the lease between the
    /// read and the send and leave this one's baseline stale.
    pub fn reserve(&self, host: &str, name: &str) -> Result<(), Busy> {
        let mut slots = self.slots.lock().unwrap();
        let k = key(host, name);
        let held = match slots.by_shed.remove(&k) {
            Some(s @ Slot::InFlight { .. }) => {
                slots.by_shed.insert(k, s);
                return Err(Busy);
            }
            Some(Slot::Unknown(r)) => Some(r),
            None => None,
        };
        slots.by_shed.insert(
            k,
            Slot::InFlight {
                held,
                reread: false,
            },
        );
        Ok(())
    }

    /// Let the reserved extend of `name` on `host` be sent, `observed` being the
    /// shed as read after [`Self::reserve`]: an earlier unknown outcome it held is
    /// first settled by `observed` if it can be; one still unsettled is put back and
    /// refuses ([`NotAdmitted::Busy`]). A read during which a listing settled the
    /// held outcome is no baseline ([`NotAdmitted::Reread`], the turn kept). On `Ok`
    /// the turn is the extend's until [`Self::finish`].
    pub fn admit(
        &self,
        host: &str,
        name: &str,
        observed: &Shed,
        read_at: i64,
    ) -> Result<(), NotAdmitted> {
        // `read_at`: when the read of `observed` was sent (see [`Self::observe`]).
        let now = read_at;
        let mut slots = self.slots.lock().unwrap();
        let k = key(host, name);
        let admitted = Slot::InFlight {
            held: None,
            reread: false,
        };
        match slots.by_shed.remove(&k) {
            Some(Slot::InFlight { held: None, reread }) => {
                slots.by_shed.insert(k, admitted);
                if reread {
                    return Err(NotAdmitted::Reread);
                }
            }
            Some(Slot::InFlight { held: Some(r), .. }) => {
                if !settled(&r.u, Some(observed), now, self.settle_s) {
                    slots.by_shed.insert(k, Slot::Unknown(r));
                    return Err(NotAdmitted::Busy);
                }
                slots.by_shed.insert(k, admitted);
            }
            other => {
                if let Some(s) = other {
                    slots.by_shed.insert(k, s);
                }
                return Err(NotAdmitted::Busy);
            }
        }
        Ok(())
    }

    /// Give back a turn [`Self::reserve`] took when nothing was sent (the read
    /// failed): an unknown outcome it held is put back, unsettled (`true`).
    pub fn release(&self, host: &str, name: &str) -> bool {
        let mut slots = self.slots.lock().unwrap();
        let k = key(host, name);
        match slots.by_shed.remove(&k) {
            Some(Slot::InFlight { held: Some(r), .. }) => {
                slots.by_shed.insert(k, Slot::Unknown(r));
                true
            }
            Some(Slot::InFlight { held: None, .. }) => false,
            Some(s) => {
                slots.by_shed.insert(k, s);
                false
            }
            None => false,
        }
    }

    /// End the extend [`Self::admit`] let through: `unknown` records its unknown
    /// outcome (stamped now, and after every listing mark taken so far); `None` (an
    /// answer) frees the slot.
    pub fn finish(&self, host: &str, name: &str, unknown: Option<UnknownExtend>) {
        let mut slots = self.slots.lock().unwrap();
        let k = key(host, name);
        if !matches!(
            slots.by_shed.get(&k),
            Some(Slot::InFlight { held: None, .. })
        ) {
            return;
        }
        match unknown {
            Some(u) => {
                let at = self.now();
                slots.recorded += 1;
                let seq = slots.recorded;
                slots.by_shed.insert(
                    k,
                    Slot::Unknown(Recorded {
                        u: UnknownExtend { at, ..u },
                        seq,
                    }),
                );
            }
            None => {
                slots.by_shed.remove(&k);
            }
        }
    }

    /// The mark to take before a listing is sent and hand to [`Self::observe`] with
    /// what it showed: a listing settles only the outcomes recorded before it was
    /// sent, since one sent earlier may predate the extend (a shed not yet listed
    /// would look gone).
    pub fn listing_mark(&self) -> ListingMark {
        let at = self.now();
        ListingMark {
            seq: self.slots.lock().unwrap().recorded,
            at,
        }
    }

    /// Settle what `host`'s listing (`rows`, every shed it listed), sent after
    /// `mark` was taken, shows. Called only for a listing that succeeded: a failed
    /// one settles nothing.
    pub fn observe(&self, host: &str, rows: &[Shed], mark: ListingMark) {
        // Measured to when the listing was sent, not when it answered: the server
        // read the lease no earlier than that, so a slow answer cannot stand in for
        // a later reading.
        let sent = mark.at;
        let settle_s = self.settle_s;
        let settles = |name: &str, r: &Recorded| {
            r.seq <= mark.seq && settled(&r.u, rows.iter().find(|s| s.name == name), sent, settle_s)
        };
        let mut slots = self.slots.lock().unwrap();
        slots.by_shed.retain(|(h, name), slot| {
            if h != host {
                return true;
            }
            match slot {
                Slot::Unknown(r) => !settles(name, r),
                Slot::InFlight { held, reread } => {
                    if held.as_ref().is_some_and(|r| settles(name, r)) {
                        // The read out now may predate what settled it.
                        *held = None;
                        *reread = true;
                    }
                    true
                }
            }
        });
    }

    /// Every unknown outcome: (server, shed) to (incarnation, hours).
    pub fn snapshot(&self) -> HashMap<(String, String), (String, u32)> {
        self.slots
            .lock()
            .unwrap()
            .by_shed
            .iter()
            .filter_map(|(k, s)| {
                unknown_of(s).map(|r| (k.clone(), (r.u.incarnation.clone(), r.u.hours)))
            })
            .collect()
    }

    /// The hours of `name`'s extend on `host` whose outcome is not known, if any.
    pub fn unknown_hours(&self, host: &str, name: &str) -> Option<u32> {
        self.recorded(host, name).map(|u| u.hours)
    }

    fn recorded(&self, host: &str, name: &str) -> Option<UnknownExtend> {
        self.slots
            .lock()
            .unwrap()
            .by_shed
            .get(&key(host, name))
            .and_then(unknown_of)
            .map(|r| r.u.clone())
    }
}

fn unknown_of(s: &Slot) -> Option<&Recorded> {
    match s {
        Slot::Unknown(r) | Slot::InFlight { held: Some(r), .. } => Some(r),
        Slot::InFlight { held: None, .. } => None,
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
        // Moved by less: not applied, so it waits for the settle time.
        let less = shed("w", "c1", Some(BEFORE + 3 * 3600));
        assert!(!settled(&u, Some(&less), T0 + 1, EXTEND_SETTLE_S));
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
    fn a_lease_moved_by_fewer_hours_stays_unknown() {
        // Another client's smaller extend moved the lease: this one's outcome is
        // still unknown, so the record stays (to the age limit) rather than
        // risk adding the hours twice.
        let u = unknown(6, T0);
        let less = shed("w", "c1", Some(BEFORE + 3 * 3600));
        assert!(!settled(
            &u,
            Some(&less),
            T0 + EXTEND_SETTLE_S + 600,
            EXTEND_SETTLE_S
        ));
        assert!(settled(
            &u,
            Some(&less),
            T0 + EXTEND_UNKNOWN_MAX_S,
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

    /// Reserve and admit at once (the shed read in between is `observed`).
    fn begin(b: &ExtendBook, host: &str, observed: &Shed) -> Result<(), Busy> {
        b.reserve(host, &observed.name)?;
        b.admit(host, &observed.name, observed, b.now())
            .map_err(|_| Busy)
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
        begin(&b, "cloud-a", &w).unwrap();
        assert_eq!(begin(&b, "cloud-a", &w), Err(Busy), "in flight");
        assert!(b.in_flight("cloud-a", "w"));
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        assert_eq!(b.unknown_hours("cloud-a", "w"), Some(6));
        assert_eq!(
            b.snapshot()[&("cloud-a".into(), "w".into())],
            ("c1".to_string(), 6)
        );
        // Still at its old end, too soon: refused, kept.
        assert_eq!(begin(&b, "cloud-a", &w), Err(Busy));
        b.observe("cloud-a", std::slice::from_ref(&w), b.listing_mark());
        assert!(b.has_unknown("cloud-a", "w"));
        // Another server's listing settles nothing here.
        b.observe("other", &[], b.listing_mark());
        assert!(b.has_unknown("cloud-a", "w"));
        // After the settle time the same end: not applied, settled.
        t.store(T0 + EXTEND_SETTLE_S, Ordering::SeqCst);
        b.observe("cloud-a", std::slice::from_ref(&w), b.listing_mark());
        assert!(!b.has_unknown("cloud-a", "w"));
        assert!(b.snapshot().is_empty());
    }

    /// The not-applied wait is measured to when a listing (or the baseline read) was
    /// sent, not to when it answered: a listing sent before the wait was up that
    /// answers after it settles nothing, since the server read the lease too soon.
    #[test]
    fn the_wait_is_measured_to_when_the_listing_was_sent() {
        let (b, t) = book();
        let w = shed("w", "c1", Some(BEFORE));
        begin(&b, "cloud-a", &w).unwrap();
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        // Sent a second before the wait is up, answered six seconds after it.
        t.store(T0 + EXTEND_SETTLE_S - 1, Ordering::SeqCst);
        let mark = b.listing_mark();
        t.store(T0 + EXTEND_SETTLE_S + 5, Ordering::SeqCst);
        b.observe("cloud-a", std::slice::from_ref(&w), mark);
        assert!(
            b.has_unknown("cloud-a", "w"),
            "a slow early listing settled it"
        );
        // The same for a baseline read: sent early, it does not admit.
        assert!(b.reserve("cloud-a", "w").is_ok());
        assert_eq!(
            b.admit("cloud-a", "w", &w, T0 + EXTEND_SETTLE_S - 1),
            Err(NotAdmitted::Busy)
        );
        // A listing sent once the wait is up settles it.
        b.observe("cloud-a", std::slice::from_ref(&w), b.listing_mark());
        assert!(!b.has_unknown("cloud-a", "w"));
    }

    #[test]
    fn a_begin_settles_an_applied_one_by_the_shed_it_read() {
        let (b, _) = book();
        let w = shed("w", "c1", Some(BEFORE));
        begin(&b, "cloud-a", &w).unwrap();
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        let moved = shed("w", "c1", Some(BEFORE + 6 * 3600));
        begin(&b, "cloud-a", &moved).unwrap();
        assert!(b.in_flight("cloud-a", "w"));
        b.finish("cloud-a", "w", None);
        assert!(b.snapshot().is_empty());
    }

    #[test]
    fn a_new_incarnation_or_a_gone_shed_clears_it() {
        let (b, _) = book();
        let w = shed("w", "c1", Some(BEFORE));
        for observed in [vec![], vec![shed("w", "c2", Some(BEFORE))]] {
            begin(&b, "cloud-a", &w).unwrap();
            b.finish("cloud-a", "w", Some(unknown(6, 0)));
            b.observe("cloud-a", &observed, b.listing_mark());
            assert!(!b.has_unknown("cloud-a", "w"), "{observed:?}");
        }
        // A begin for another incarnation is not refused.
        begin(&b, "cloud-a", &w).unwrap();
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        begin(&b, "cloud-a", &shed("w", "c2", Some(BEFORE))).unwrap();
    }

    #[test]
    fn the_age_limit_clears_it() {
        let (b, t) = book();
        let w = shed("w", "c1", None);
        begin(&b, "cloud-a", &w).unwrap();
        b.finish(
            "cloud-a",
            "w",
            Some(UnknownExtend {
                before: None,
                ..unknown(6, 0)
            }),
        );
        t.store(T0 + EXTEND_UNKNOWN_MAX_S - 1, Ordering::SeqCst);
        b.observe("cloud-a", std::slice::from_ref(&w), b.listing_mark());
        assert!(b.has_unknown("cloud-a", "w"));
        t.store(T0 + EXTEND_UNKNOWN_MAX_S, Ordering::SeqCst);
        b.observe("cloud-a", std::slice::from_ref(&w), b.listing_mark());
        assert!(!b.has_unknown("cloud-a", "w"));
    }

    /// A listing sent before an unknown outcome was recorded settles nothing of it
    /// (the shed it does not list may simply be newer than the listing); one sent
    /// after does.
    #[test]
    fn a_listing_sent_before_the_outcome_was_recorded_settles_nothing() {
        let (b, _) = book();
        let w = shed("w", "c1", Some(BEFORE));
        let early = b.listing_mark();
        begin(&b, "cloud-a", &w).unwrap();
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        b.observe("cloud-a", &[], early);
        assert!(
            b.has_unknown("cloud-a", "w"),
            "an earlier listing settles nothing"
        );
        b.observe("cloud-a", &[], b.listing_mark());
        assert!(!b.has_unknown("cloud-a", "w"), "a later one shows it gone");
    }

    /// The turn is taken before the shed is read: a second extend is refused from
    /// then on; a read that fails gives the turn back, an unknown outcome it held
    /// put back unsettled; a listing during the read can settle a held one.
    #[test]
    fn the_turn_is_taken_before_the_read() {
        let (b, t) = book();
        let w = shed("w", "c1", Some(BEFORE));
        b.reserve("cloud-a", "w").unwrap();
        assert_eq!(b.reserve("cloud-a", "w"), Err(Busy), "reading");
        assert!(!b.release("cloud-a", "w"), "nothing held");
        assert!(!b.in_flight("cloud-a", "w"));

        begin(&b, "cloud-a", &w).unwrap();
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        b.reserve("cloud-a", "w").unwrap();
        assert_eq!(
            b.unknown_hours("cloud-a", "w"),
            Some(6),
            "held, still shown"
        );
        assert_eq!(b.reserve("cloud-a", "w"), Err(Busy));
        assert!(b.release("cloud-a", "w"), "the read failed: put back");
        assert_eq!(b.unknown_hours("cloud-a", "w"), Some(6));
        assert!(!b.in_flight("cloud-a", "w"));
        // Read, still at its old end, too soon: refused and put back.
        b.reserve("cloud-a", "w").unwrap();
        assert_eq!(b.admit("cloud-a", "w", &w, b.now()), Err(NotAdmitted::Busy));
        assert!(b.has_unknown("cloud-a", "w") && !b.in_flight("cloud-a", "w"));
        // A listing during the read settles the held one; that read is no
        // baseline, so the shed is read again (the turn kept) and that read admits.
        b.reserve("cloud-a", "w").unwrap();
        t.store(T0 + EXTEND_SETTLE_S, Ordering::SeqCst);
        b.observe("cloud-a", std::slice::from_ref(&w), b.listing_mark());
        assert!(!b.has_unknown("cloud-a", "w") && b.in_flight("cloud-a", "w"));
        assert_eq!(
            b.admit("cloud-a", "w", &w, b.now()),
            Err(NotAdmitted::Reread)
        );
        assert!(
            b.in_flight("cloud-a", "w"),
            "the turn is kept for the reread"
        );
        b.admit("cloud-a", "w", &w, b.now()).unwrap();
        b.finish("cloud-a", "w", None);
        assert!(!b.in_flight("cloud-a", "w"));
    }

    /// Extend A's outcome is unknown at end E; extend B takes the turn (holding A)
    /// and its read comes back at E, but a listing showing A applied (E + 6 h)
    /// settled A while B's read was out. B's read is stale: it is never admitted as
    /// B's baseline (else B, unknown in turn, would be settled as applied by A's
    /// move at once). B reads again and is measured from the fresh end.
    #[test]
    fn a_read_out_while_a_listing_settled_the_held_one_is_read_again() {
        let (b, _) = book();
        let at_e = shed("w", "c1", Some(BEFORE));
        let applied = shed("w", "c1", Some(BEFORE + 6 * 3600));
        begin(&b, "cloud-a", &at_e).unwrap();
        b.finish("cloud-a", "w", Some(unknown(6, 0))); // A
        b.reserve("cloud-a", "w").unwrap(); // B's turn; its read goes out
        b.observe("cloud-a", std::slice::from_ref(&applied), b.listing_mark());
        assert!(!b.has_unknown("cloud-a", "w"), "A settled as applied");
        // B's read, sent before that listing, comes back at E.
        assert_eq!(
            b.admit("cloud-a", "w", &at_e, b.now()),
            Err(NotAdmitted::Reread)
        );
        // B reads again: E + 6 h, its baseline.
        b.admit("cloud-a", "w", &applied, b.now()).unwrap();
        b.finish(
            "cloud-a",
            "w",
            Some(UnknownExtend {
                before: lease_end(&applied),
                ..unknown(6, 0)
            }),
        );
        // A listing still at A's E + 6 h does not settle B as applied.
        b.observe("cloud-a", std::slice::from_ref(&applied), b.listing_mark());
        assert_eq!(b.unknown_hours("cloud-a", "w"), Some(6));
    }

    #[test]
    fn a_finish_without_a_begin_changes_nothing() {
        let (b, _) = book();
        b.finish("cloud-a", "w", Some(unknown(6, 0)));
        assert!(b.snapshot().is_empty());
    }
}
