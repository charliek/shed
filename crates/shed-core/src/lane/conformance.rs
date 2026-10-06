//! The lane contract's **conformance kit** (plan 025 §3.2.4): the rules every
//! adapter's streams must keep, written once, so the adapters — opencode's,
//! and craze's (its source since plan 025 C7, its lane since C8) — cannot
//! drift apart at either level.
//!
//! A test drives an adapter and feeds every frame it receives through a checker
//! — [`LaneChecker`] for an [`AgentLane::subscribe`](super::AgentLane::subscribe)
//! stream, [`SourceChecker`] for an
//! [`AgentSource::subscribe`](super::AgentSource::subscribe) one: opencode's
//! (`shed-opencode`'s `tests/conformance.rs`, against `FakeOpencode`), and
//! craze's source and lane against craze's own hermetic recipe — the real hub
//! (`shed-craze`'s `tests/recipe_source.rs` and `tests/recipe_lane.rs`), and the
//! lane against a scripted host too (`tests/lane.rs`, `tests/overflow.rs`). The
//! first frame
//! that breaks a rule is a [`Violation`] naming the [`Rule`], the frame's index
//! and what was wrong. [`drive_lane`]/[`drive_source`] do the reading;
//! [`check_lane`]/[`check_source`] judge a recorded slice.
//!
//! The rules, each a [`Rule`] variant and each the module doc's:
//!
//! - **The bracket.** A stream opens with `Reset` (a lane may instead open with
//!   `Stale` or `Down`, a source with `Offline` — "Before the first seed"). A
//!   `Reset`'s generation exceeds every earlier one in the subscription; a
//!   `Ready` that closes a seed names that seed's generation; and a seed
//!   interrupted by a lane's `Stale` (or a source's `Offline`) is never closed
//!   by a `Ready` — a loss before `Ready` reseeds, it does not resume a half
//!   seed. A source has no silent resume, so its every `Ready` closes a seed.
//! - **`Capabilities` before `Ready`**, in every seed; and on a lane, `Settings`
//!   in every seed whose capabilities say `settings` — and NEVER on a session
//!   whose capabilities do not.
//! - **`seq` strictly increasing within one subscription**, across reseeds.
//! - **A silent resume keeps the generation**: a lone `Ready` names the live one
//!   — and is only the END of a resume, so it follows a `Stale` on that
//!   generation; a lone `Ready` with nothing to end is a violation.
//! - **`Stale` is non-terminal and `Down` is last**: nothing follows a `Down`,
//!   and a lane stream that ends ends with one. A source stream never ends while
//!   its subscriber holds it.
//! - **A source never sets [`LaneSession::tab_id`]** — on a source's rows or on
//!   a lane's session rows.
//! - **An adapter never constructs `Unknown`**: it is a decode outcome only.
//!
//! In-process frames never pass through serde, so these checks see exactly what
//! the adapter built.
//!
//! `#[cfg(any(test, feature = "test-support"))]`: an adapter's tests reach it
//! through shed-core's `test-support` feature, and a shipped binary never
//! carries it.

use std::time::Duration;

use tokio::sync::mpsc;

use super::{LaneCapabilities, LaneEvent, LaneSession, SourceEvent};

/// Which rule a stream broke. See the module doc for each.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Rule {
    /// The stream did not open with what it may open with.
    Opening,
    /// A generation out of order, a `Ready` that names the wrong seed, or a
    /// half seed closed after a loss.
    BracketOrder,
    /// A seed reached `Ready` without its `Capabilities`.
    CapabilitiesBeforeReady,
    /// A lane seed whose capabilities say `settings` reached `Ready` without its
    /// `Settings`.
    SettingsBeforeReady,
    /// `Settings` on a session whose capabilities do not say `settings` (or
    /// before any capabilities at all).
    SettingsNotAdvertised,
    /// A message `seq` not above every earlier one in the subscription.
    SeqMonotonic,
    /// A lone `Ready` (no seed staged) that is not the live generation.
    SilentResumeKeepsGeneration,
    /// A lone `Ready` with no `Stale` before it: it can only be the END of a
    /// silent resume, and a resume starts with a `Stale`.
    LoneReadyEndsAResume,
    /// A frame after `Down`.
    DownIsLast,
    /// The stream ended without a `Down` (a lane), or ended at all (a source).
    EndedWithoutDown,
    /// A row carrying `tab_id`.
    NoTabId,
    /// An adapter constructed `Unknown`.
    NeverUnknown,
}

/// One broken rule: which, at which frame (0-based, counting every frame the
/// checker was fed), and what was wrong.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Violation {
    pub rule: Rule,
    pub index: usize,
    pub detail: String,
}

impl std::fmt::Display for Violation {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "{:?} at frame {}: {}",
            self.rule, self.index, self.detail
        )
    }
}

impl std::error::Error for Violation {}

impl Violation {
    fn new(rule: Rule, index: usize, detail: String) -> Violation {
        Violation {
            rule,
            index,
            detail,
        }
    }
}

/// A seed in progress.
struct Seed<C> {
    generation: u64,
    /// The seed's capabilities, once it carried them.
    capabilities: Option<C>,
    settings: bool,
    /// A loss (`Stale`/`Offline`) landed inside this seed: no `Ready` may close
    /// it now, only a fresh `Reset`.
    interrupted: bool,
}

impl<C> Seed<C> {
    fn new(generation: u64) -> Seed<C> {
        Seed {
            generation,
            capabilities: None,
            settings: false,
            interrupted: false,
        }
    }
}

/// The `Reset … Ready` bracket as both levels keep it — the half of the rules a
/// lane stream and a source stream share, written once so the kit's own two
/// checkers cannot drift apart either. `C` is what a seed's `Capabilities`
/// leaves behind: the lane's row, or `()` for a source (which only has to have
/// sent one).
struct Bracket<C> {
    /// The last `Reset`'s generation; `None` until the stream's first seed
    /// started.
    last_reset: Option<u64>,
    staged: Option<Seed<C>>,
    /// The generation of the last completed seed.
    live_generation: Option<u64>,
    resets: usize,
    readies: usize,
}

impl<C> Default for Bracket<C> {
    fn default() -> Bracket<C> {
        Bracket {
            last_reset: None,
            staged: None,
            live_generation: None,
            resets: 0,
            readies: 0,
        }
    }
}

impl<C> Bracket<C> {
    /// `what` may not arrive before the stream's first `Reset`.
    fn opened(&self, what: &str, at: usize) -> Result<(), Violation> {
        if self.last_reset.is_some() {
            return Ok(());
        }
        Err(Violation::new(
            Rule::Opening,
            at,
            format!("{what} before the first Reset"),
        ))
    }

    /// A `Reset`: its generation exceeds every earlier one, and it stages a new
    /// seed (abandoning any seed still in flight).
    fn reset(&mut self, generation: u64, at: usize) -> Result<(), Violation> {
        if let Some(last) = self.last_reset {
            if generation <= last {
                return Err(Violation::new(
                    Rule::BracketOrder,
                    at,
                    format!("Reset({generation}) after Reset({last})"),
                ));
            }
        }
        self.last_reset = Some(generation);
        self.resets += 1;
        self.staged = Some(Seed::new(generation));
        Ok(())
    }

    /// A loss (`Stale`/`Offline`) — if a seed is in flight, it may no longer be
    /// closed by a `Ready`.
    fn interrupt(&mut self) {
        if let Some(seed) = self.staged.as_mut() {
            seed.interrupted = true;
        }
    }

    /// A `Ready(generation)`: the staged seed it closes, or `None` when nothing
    /// was staged (which only a lane's silent resume may do — the caller
    /// judges). A closed seed must not have been interrupted (`loss` names what
    /// interrupted it) and must be that generation's own; the caller judges its
    /// capabilities.
    fn ready(
        &mut self,
        generation: u64,
        loss: &str,
        at: usize,
    ) -> Result<Option<Seed<C>>, Violation> {
        self.readies += 1;
        let Some(seed) = self.staged.take() else {
            return Ok(None);
        };
        if seed.interrupted {
            return Err(Violation::new(
                Rule::BracketOrder,
                at,
                format!(
                    "Ready({generation}) closed seed {} after {loss}",
                    seed.generation
                ),
            ));
        }
        if seed.generation != generation {
            return Err(Violation::new(
                Rule::BracketOrder,
                at,
                format!(
                    "Ready({generation}) closed the seed of Reset({})",
                    seed.generation
                ),
            ));
        }
        Ok(Some(seed))
    }
}

/// Checks one LANE subscription's frames, in arrival order.
#[derive(Default)]
pub struct LaneChecker {
    index: usize,
    bracket: Bracket<LaneCapabilities>,
    live_capabilities: Option<LaneCapabilities>,
    last_seq: Option<u64>,
    ended: bool,
    stales: usize,
    /// A `Stale` landed on the LIVE generation and no `Ready` has answered it
    /// yet — a silent resume is in progress, so a lone `Ready` may end it.
    resuming: bool,
}

impl LaneChecker {
    pub fn new() -> LaneChecker {
        LaneChecker::default()
    }

    /// Judge one frame. `Err` names the first rule it breaks.
    pub fn observe(&mut self, ev: &LaneEvent) -> Result<(), Violation> {
        let at = self.index;
        self.index += 1;
        let fail = |rule: Rule, detail: String| Err(Violation::new(rule, at, detail));
        if self.ended {
            return fail(Rule::DownIsLast, format!("{ev:?} after the Down"));
        }
        match ev {
            LaneEvent::Reset { generation, .. } => self.bracket.reset(*generation, at)?,
            LaneEvent::Ready { generation } => {
                let closed = self.bracket.ready(
                    *generation,
                    "a Stale inside it — a loss before Ready reseeds",
                    at,
                )?;
                match closed {
                    Some(seed) => {
                        let Some(caps) = seed.capabilities else {
                            return fail(
                                Rule::CapabilitiesBeforeReady,
                                format!("seed {generation} reached Ready with no Capabilities"),
                            );
                        };
                        if caps.settings && !seed.settings {
                            return fail(
                                Rule::SettingsBeforeReady,
                                format!(
                                    "seed {generation}'s capabilities say settings, and it \
                                     reached Ready with no Settings"
                                ),
                            );
                        }
                        self.bracket.live_generation = Some(*generation);
                        self.live_capabilities = Some(caps);
                        self.resuming = false;
                    }
                    None => {
                        if self.bracket.live_generation != Some(*generation) {
                            return fail(
                                Rule::SilentResumeKeepsGeneration,
                                format!(
                                    "a lone Ready({generation}) while the live generation is {:?}",
                                    self.bracket.live_generation
                                ),
                            );
                        }
                        if !self.resuming {
                            return fail(
                                Rule::LoneReadyEndsAResume,
                                format!(
                                    "a lone Ready({generation}) with no Stale before it — \
                                     nothing was resuming"
                                ),
                            );
                        }
                        self.resuming = false;
                    }
                }
            }
            LaneEvent::Capabilities { capabilities } => {
                self.bracket.opened("Capabilities", at)?;
                match self.bracket.staged.as_mut() {
                    Some(seed) => seed.capabilities = Some(capabilities.clone()),
                    None => self.live_capabilities = Some(capabilities.clone()),
                }
            }
            LaneEvent::Settings { .. } => {
                self.bracket.opened("Settings", at)?;
                let governing = match self.bracket.staged.as_ref() {
                    Some(seed) => seed.capabilities.as_ref(),
                    None => self.live_capabilities.as_ref(),
                };
                if !governing.is_some_and(|c| c.settings) {
                    return fail(
                        Rule::SettingsNotAdvertised,
                        format!("Settings under capabilities {governing:?}"),
                    );
                }
                if let Some(seed) = self.bracket.staged.as_mut() {
                    seed.settings = true;
                }
            }
            LaneEvent::Message { message, .. } => {
                self.bracket.opened("a Message", at)?;
                if let Some(last) = self.last_seq {
                    if message.seq <= last {
                        return fail(
                            Rule::SeqMonotonic,
                            format!("seq {} after seq {last}", message.seq),
                        );
                    }
                }
                self.last_seq = Some(message.seq);
            }
            LaneEvent::Session { session } => {
                self.bracket.opened("a Session", at)?;
                no_tab_id(session, at)?;
            }
            LaneEvent::Approval { .. } => self.bracket.opened("an Approval", at)?,
            LaneEvent::Stale { .. } => {
                self.stales += 1;
                // A loss inside a seed interrupts the seed (it must reseed); a
                // loss on the live generation starts a resume, which a lone
                // `Ready` of that generation may end.
                if self.bracket.staged.is_some() {
                    self.bracket.interrupt();
                } else {
                    self.resuming = self.bracket.live_generation.is_some();
                }
            }
            LaneEvent::Down { .. } => self.ended = true,
            LaneEvent::Unknown => {
                return fail(Rule::NeverUnknown, "an adapter built Unknown".to_string())
            }
        }
        Ok(())
    }

    /// The subscription's channel closed. A lane stream ends with `Down` and
    /// nothing else — `Stale` included.
    pub fn closed(&self) -> Result<(), Violation> {
        if self.ended {
            return Ok(());
        }
        Err(Violation::new(
            Rule::EndedWithoutDown,
            self.index,
            "the lane stream closed without a Down".to_string(),
        ))
    }

    /// The generation of the last completed seed.
    pub fn live_generation(&self) -> Option<u64> {
        self.bracket.live_generation
    }

    /// The capabilities that govern the live view.
    pub fn live_capabilities(&self) -> Option<&LaneCapabilities> {
        self.live_capabilities.as_ref()
    }

    /// A `Down` was seen.
    pub fn ended(&self) -> bool {
        self.ended
    }

    /// How many `Reset`s, `Ready`s and `Stale`s were seen — what a test asserts
    /// "no reseed" or "a silent resume" with.
    pub fn resets(&self) -> usize {
        self.bracket.resets
    }

    pub fn readies(&self) -> usize {
        self.bracket.readies
    }

    pub fn stales(&self) -> usize {
        self.stales
    }
}

/// Checks one SOURCE subscription's frames, in arrival order.
#[derive(Default)]
pub struct SourceChecker {
    index: usize,
    bracket: Bracket<()>,
    offlines: usize,
}

impl SourceChecker {
    pub fn new() -> SourceChecker {
        SourceChecker::default()
    }

    /// Judge one frame. `Err` names the first rule it breaks.
    pub fn observe(&mut self, ev: &SourceEvent) -> Result<(), Violation> {
        let at = self.index;
        self.index += 1;
        let fail = |rule: Rule, detail: String| Err(Violation::new(rule, at, detail));
        match ev {
            SourceEvent::Reset { generation, .. } => self.bracket.reset(*generation, at)?,
            SourceEvent::Ready { generation, .. } => {
                let closed = self
                    .bracket
                    .ready(*generation, "an Offline inside it", at)?;
                let Some(seed) = closed else {
                    return fail(
                        Rule::BracketOrder,
                        format!("Ready({generation}) with no seed — a source has no silent resume"),
                    );
                };
                if seed.capabilities.is_none() {
                    return fail(
                        Rule::CapabilitiesBeforeReady,
                        format!("roster seed {generation} reached Ready with no Capabilities"),
                    );
                }
                self.bracket.live_generation = Some(*generation);
            }
            SourceEvent::Session { session } => {
                self.bracket.opened("a Session", at)?;
                no_tab_id(session, at)?;
            }
            SourceEvent::Removed { .. } => self.bracket.opened("a Removed", at)?,
            SourceEvent::Capabilities { .. } => {
                self.bracket.opened("Capabilities", at)?;
                if let Some(seed) = self.bracket.staged.as_mut() {
                    seed.capabilities = Some(());
                }
            }
            SourceEvent::Offline { .. } => {
                self.offlines += 1;
                self.bracket.interrupt();
            }
            SourceEvent::Unknown => {
                return fail(Rule::NeverUnknown, "an adapter built Unknown".to_string())
            }
        }
        Ok(())
    }

    /// The subscription's channel closed while its subscriber still held it —
    /// which a source never does (`Offline` is how it says it cannot reach its
    /// sessions).
    pub fn closed(&self) -> Result<(), Violation> {
        Err(Violation::new(
            Rule::EndedWithoutDown,
            self.index,
            "the source stream closed while its subscriber held it".to_string(),
        ))
    }

    pub fn live_generation(&self) -> Option<u64> {
        self.bracket.live_generation
    }

    pub fn resets(&self) -> usize {
        self.bracket.resets
    }

    pub fn readies(&self) -> usize {
        self.bracket.readies
    }

    pub fn offlines(&self) -> usize {
        self.offlines
    }
}

fn no_tab_id(session: &LaneSession, at: usize) -> Result<(), Violation> {
    match session.tab_id {
        None => Ok(()),
        Some(tab) => Err(Violation::new(
            Rule::NoTabId,
            at,
            format!(
                "row {:?} carries tab_id {tab} — only the client-side merge sets it",
                session.id
            ),
        )),
    }
}

/// What [`drive`] and [`check`] need of a checker: the one seam the two levels'
/// readers share, so each is written once.
trait Checker: Default {
    type Event;
    fn observe(&mut self, ev: &Self::Event) -> Result<(), Violation>;
    fn closed(&self) -> Result<(), Violation>;
}

impl Checker for LaneChecker {
    type Event = LaneEvent;
    fn observe(&mut self, ev: &LaneEvent) -> Result<(), Violation> {
        LaneChecker::observe(self, ev)
    }
    fn closed(&self) -> Result<(), Violation> {
        LaneChecker::closed(self)
    }
}

impl Checker for SourceChecker {
    type Event = SourceEvent;
    fn observe(&mut self, ev: &SourceEvent) -> Result<(), Violation> {
        SourceChecker::observe(self, ev)
    }
    fn closed(&self) -> Result<(), Violation> {
        SourceChecker::closed(self)
    }
}

fn check<K: Checker>(events: &[K::Event]) -> Result<K, Violation> {
    let mut checker = K::default();
    for ev in events {
        checker.observe(ev)?;
    }
    Ok(checker)
}

/// Judge a recorded lane stream. It is not treated as ended: call
/// [`LaneChecker::closed`] on the result when the recording ran to the
/// channel's close.
pub fn check_lane(events: &[LaneEvent]) -> Result<LaneChecker, Violation> {
    check(events)
}

/// Judge a recorded source stream.
pub fn check_source(events: &[SourceEvent]) -> Result<SourceChecker, Violation> {
    check(events)
}

/// How a [`drive_lane`]/[`drive_source`] read stopped.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DriveEnd {
    /// The `until` predicate matched a frame (the last one returned).
    Matched,
    /// The channel closed — and the checker accepted the close.
    Closed,
    /// `within` passed first. Not a rule broken; the caller decides whether it
    /// is a failure.
    Deadline,
}

/// What one drive read, and why it stopped.
#[derive(Debug)]
pub struct Drive<E> {
    pub frames: Vec<E>,
    pub end: DriveEnd,
}

/// [`drive_lane`] and [`drive_source`], written once.
async fn drive<K, F>(
    rx: &mut mpsc::Receiver<K::Event>,
    checker: &mut K,
    within: Duration,
    mut until: F,
) -> Result<Drive<K::Event>, Violation>
where
    K: Checker,
    F: FnMut(&K::Event) -> bool,
{
    let deadline = tokio::time::Instant::now() + within;
    let mut frames = Vec::new();
    loop {
        match tokio::time::timeout_at(deadline, rx.recv()).await {
            Err(_) => {
                return Ok(Drive {
                    frames,
                    end: DriveEnd::Deadline,
                })
            }
            Ok(None) => {
                checker.closed()?;
                return Ok(Drive {
                    frames,
                    end: DriveEnd::Closed,
                });
            }
            Ok(Some(ev)) => {
                checker.observe(&ev)?;
                let done = until(&ev);
                frames.push(ev);
                if done {
                    return Ok(Drive {
                        frames,
                        end: DriveEnd::Matched,
                    });
                }
            }
        }
    }
}

/// Read a lane subscription's frames through `checker` until `until` matches
/// one, the channel closes, or `within` passes. Every frame is judged as it
/// arrives; the first violation is the `Err`.
pub async fn drive_lane<F>(
    rx: &mut mpsc::Receiver<LaneEvent>,
    checker: &mut LaneChecker,
    within: Duration,
    until: F,
) -> Result<Drive<LaneEvent>, Violation>
where
    F: FnMut(&LaneEvent) -> bool,
{
    drive(rx, checker, within, until).await
}

/// [`drive_lane`]'s twin for a source subscription.
pub async fn drive_source<F>(
    rx: &mut mpsc::Receiver<SourceEvent>,
    checker: &mut SourceChecker,
    within: Duration,
    until: F,
) -> Result<Drive<SourceEvent>, Violation>
where
    F: FnMut(&SourceEvent) -> bool,
{
    drive(rx, checker, within, until).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::lane::{LaneSettings, SourceCapabilities, SourceOffline};
    use crate::rc::RcFeedMessage;

    fn caps(settings: bool) -> LaneEvent {
        LaneEvent::Capabilities {
            capabilities: LaneCapabilities {
                kind: "test".into(),
                interject: false,
                cancel: true,
                approvals: true,
                history_cursor: true,
                settings,
                stop: false,
            },
        }
    }

    fn settings() -> LaneEvent {
        LaneEvent::Settings {
            settings: LaneSettings::default(),
        }
    }

    fn msg(seq: u64) -> LaneEvent {
        LaneEvent::Message {
            message: RcFeedMessage {
                seq,
                role: "assistant".into(),
                msg_type: "text".into(),
                text: Some(format!("row {seq}")),
                ..RcFeedMessage::default()
            },
            cursor: None,
        }
    }

    fn session(tab: Option<i64>) -> LaneSession {
        LaneSession {
            id: "row".into(),
            tab_id: tab,
            ..LaneSession::default()
        }
    }

    fn reset(generation: u64) -> LaneEvent {
        LaneEvent::Reset {
            reason: "seed".into(),
            generation,
        }
    }

    fn ready(generation: u64) -> LaneEvent {
        LaneEvent::Ready { generation }
    }

    fn stale() -> LaneEvent {
        LaneEvent::Stale {
            reason: "reconnecting".into(),
        }
    }

    fn down() -> LaneEvent {
        LaneEvent::Down {
            reason: "session_closed".into(),
        }
    }

    fn rule_of(events: &[LaneEvent]) -> Rule {
        match check_lane(events) {
            Ok(_) => panic!("{events:?} must break a rule"),
            Err(v) => v.rule,
        }
    }

    /// The shapes the two real adapters produce, all accepted: opencode's
    /// reseed-every-reconnect, craze's silent resume and refused cursor, an
    /// abandoned lagged reseed, a stale-before-seed, and a terminal Down.
    #[test]
    fn conforming_streams_pass() {
        // opencode: two seeds, seqs climbing across them, then the end.
        let oc = [
            reset(1),
            msg(1),
            msg(2),
            LaneEvent::Session {
                session: session(None),
            },
            caps(false),
            ready(1),
            msg(3),
            reset(2),
            msg(4),
            msg(5),
            caps(false),
            ready(2),
            down(),
        ];
        let checker = check_lane(&oc).expect("opencode's shape conforms");
        checker.closed().expect("it ended with Down");
        assert_eq!(
            (checker.resets(), checker.readies(), checker.stales()),
            (2, 2, 0)
        );

        // craze: a seed with settings, a silent resume, a refused cursor.
        let craze = [
            reset(1),
            msg(1),
            caps(true),
            settings(),
            ready(1),
            stale(),
            msg(2),
            ready(1),
            settings(),
            stale(),
            reset(2),
            msg(3),
            caps(true),
            settings(),
            ready(2),
        ];
        let checker = check_lane(&craze).expect("craze's shape conforms");
        assert_eq!(checker.live_generation(), Some(2));
        assert_eq!(checker.stales(), 2);

        // correction 13: a lagged reseed abandoned for a fresh one.
        check_lane(&[
            reset(1),
            caps(false),
            ready(1),
            reset(2),
            msg(1),
            reset(3),
            caps(false),
            ready(3),
        ])
        .expect("an abandoned reseed is legal");

        // Before the first seed: a Stale, then a seed; or a Down outright.
        check_lane(&[stale(), reset(1), caps(false), ready(1)]).expect("stale first");
        check_lane(&[down()])
            .expect("down first")
            .closed()
            .expect("and that is an end");
    }

    #[test]
    fn opening_is_reset_stale_or_down() {
        assert_eq!(rule_of(&[msg(1)]), Rule::Opening);
        assert_eq!(rule_of(&[caps(false)]), Rule::Opening);
        assert_eq!(rule_of(&[stale(), msg(1)]), Rule::Opening);
    }

    #[test]
    fn the_bracket_is_ordered_and_matched() {
        // Generations climb.
        assert_eq!(rule_of(&[reset(2), reset(2)]), Rule::BracketOrder);
        assert_eq!(
            rule_of(&[reset(3), caps(false), ready(3), reset(1)]),
            Rule::BracketOrder
        );
        // A Ready closes ITS seed.
        assert_eq!(
            rule_of(&[reset(2), caps(false), ready(1)]),
            Rule::BracketOrder
        );
        // A loss inside a seed reseeds; it never resumes the half seed.
        assert_eq!(
            rule_of(&[reset(1), msg(1), stale(), caps(false), ready(1)]),
            Rule::BracketOrder
        );
    }

    #[test]
    fn every_seed_carries_capabilities_and_settings_when_advertised() {
        assert_eq!(
            rule_of(&[reset(1), ready(1)]),
            Rule::CapabilitiesBeforeReady
        );
        assert_eq!(
            rule_of(&[reset(1), caps(false), ready(1), reset(2), ready(2)]),
            Rule::CapabilitiesBeforeReady,
            "every seed, not only the first"
        );
        assert_eq!(
            rule_of(&[reset(1), caps(true), ready(1)]),
            Rule::SettingsBeforeReady
        );
        // Settings where they are not advertised: in a seed, before the
        // seed's capabilities, and in steady state.
        assert_eq!(
            rule_of(&[reset(1), caps(false), settings()]),
            Rule::SettingsNotAdvertised
        );
        assert_eq!(
            rule_of(&[reset(1), settings(), caps(true)]),
            Rule::SettingsNotAdvertised
        );
        assert_eq!(
            rule_of(&[reset(1), caps(false), ready(1), settings()]),
            Rule::SettingsNotAdvertised
        );
    }

    #[test]
    fn seq_climbs_across_the_whole_subscription() {
        assert_eq!(rule_of(&[reset(1), msg(2), msg(2)]), Rule::SeqMonotonic);
        assert_eq!(
            rule_of(&[reset(1), msg(5), caps(false), ready(1), reset(2), msg(1)]),
            Rule::SeqMonotonic,
            "a reseed never reissues a lower seq"
        );
    }

    #[test]
    fn a_silent_resume_keeps_the_generation() {
        assert_eq!(
            rule_of(&[reset(1), caps(false), ready(1), stale(), ready(2)]),
            Rule::SilentResumeKeepsGeneration
        );
        assert_eq!(
            rule_of(&[stale(), ready(1)]),
            Rule::SilentResumeKeepsGeneration
        );
    }

    /// A lone `Ready` is the END of a silent resume, so it follows a `Stale` on
    /// the live generation (review, sol 7). One with nothing to end — straight
    /// after a seed's own `Ready`, or a second one after a resume already ended
    /// — is not "the same generation, resumed"; it is a frame that means nothing.
    #[test]
    fn a_lone_ready_only_ends_a_resume() {
        assert_eq!(
            rule_of(&[reset(1), caps(false), ready(1), ready(1), down()]),
            Rule::LoneReadyEndsAResume
        );
        assert_eq!(
            rule_of(&[reset(1), caps(false), ready(1), stale(), ready(1), ready(1)]),
            Rule::LoneReadyEndsAResume,
            "one Stale ends one resume"
        );
        // The resume a Stale opens is the LIVE generation's: a Stale inside a
        // seed interrupts that seed instead, and opens no resume.
        assert_eq!(
            rule_of(&[
                reset(1),
                caps(false),
                ready(1),
                reset(2),
                stale(),
                reset(3),
                caps(false),
                ready(3),
                ready(3)
            ]),
            Rule::LoneReadyEndsAResume
        );
        check_lane(&[reset(1), caps(false), ready(1), stale(), stale(), ready(1)])
            .expect("a resume through two losses ends with one Ready");
    }

    #[test]
    fn down_is_last_and_stale_is_not_an_end() {
        assert_eq!(
            rule_of(&[reset(1), caps(false), ready(1), down(), msg(1)]),
            Rule::DownIsLast
        );
        let checker =
            check_lane(&[reset(1), caps(false), ready(1), stale()]).expect("legal so far");
        assert_eq!(
            checker.closed().map_err(|v| v.rule),
            Err(Rule::EndedWithoutDown),
            "a stream that stops at a Stale did not end"
        );
    }

    #[test]
    fn no_row_carries_a_tab_id_and_no_adapter_builds_unknown() {
        assert_eq!(
            rule_of(&[
                reset(1),
                LaneEvent::Session {
                    session: session(Some(7))
                }
            ]),
            Rule::NoTabId
        );
        assert_eq!(rule_of(&[reset(1), LaneEvent::Unknown]), Rule::NeverUnknown);
    }

    // ---- sources ----

    fn sreset(generation: u64) -> SourceEvent {
        SourceEvent::Reset {
            reason: "seed".into(),
            generation,
        }
    }

    fn sready(generation: u64) -> SourceEvent {
        SourceEvent::Ready {
            generation,
            truncated: false,
        }
    }

    fn scaps() -> SourceEvent {
        SourceEvent::Capabilities {
            capabilities: SourceCapabilities {
                kind: "test".into(),
                create: true,
                create_options: true,
            },
        }
    }

    fn srow(tab: Option<i64>) -> SourceEvent {
        SourceEvent::Session {
            session: session(tab),
        }
    }

    fn offline() -> SourceEvent {
        SourceEvent::Offline {
            reason: "poll failed".into(),
            cause: SourceOffline::Unreachable,
        }
    }

    fn source_rule(events: &[SourceEvent]) -> Rule {
        match check_source(events) {
            Ok(_) => panic!("{events:?} must break a rule"),
            Err(v) => v.rule,
        }
    }

    #[test]
    fn conforming_source_streams_pass() {
        let checker = check_source(&[
            offline(),
            sreset(1),
            srow(None),
            scaps(),
            sready(1),
            srow(None),
            SourceEvent::Removed {
                session_id: "row".into(),
            },
            offline(),
            sreset(2),
            scaps(),
            sready(2),
        ])
        .expect("an offline-first source with a reseed conforms");
        assert_eq!((checker.resets(), checker.offlines()), (2, 2));
        assert!(
            checker.closed().is_err(),
            "a source stream never ends under its subscriber"
        );
    }

    #[test]
    fn a_source_breaks_the_same_rules() {
        assert_eq!(source_rule(&[srow(None)]), Rule::Opening);
        assert_eq!(source_rule(&[sreset(2), sreset(1)]), Rule::BracketOrder);
        assert_eq!(
            source_rule(&[sreset(1), scaps(), sready(2)]),
            Rule::BracketOrder
        );
        assert_eq!(
            source_rule(&[sreset(1), scaps(), sready(1), sready(1)]),
            Rule::BracketOrder,
            "a source has no silent resume"
        );
        assert_eq!(
            source_rule(&[sreset(1), scaps(), offline(), sready(1)]),
            Rule::BracketOrder,
            "an Offline inside a seed needs a fresh Reset"
        );
        assert_eq!(
            source_rule(&[sreset(1), sready(1)]),
            Rule::CapabilitiesBeforeReady
        );
        assert_eq!(source_rule(&[sreset(1), srow(Some(3))]), Rule::NoTabId);
        assert_eq!(
            source_rule(&[sreset(1), SourceEvent::Unknown]),
            Rule::NeverUnknown
        );
    }

    /// The drivers judge as they read, and say why they stopped.
    #[tokio::test]
    async fn the_drivers_judge_as_they_read() {
        let (tx, mut rx) = crate::lane::LanePublisher::channel();
        for ev in [reset(1), caps(false), ready(1)] {
            tx.publish(ev);
        }
        let mut checker = LaneChecker::new();
        let drive = drive_lane(&mut rx, &mut checker, Duration::from_secs(2), |e| {
            matches!(e, LaneEvent::Ready { .. })
        })
        .await
        .expect("conforms");
        assert_eq!((drive.frames.len(), drive.end), (3, DriveEnd::Matched));

        // Nothing more is coming: the deadline is the end, not a violation.
        let drive = drive_lane(&mut rx, &mut checker, Duration::from_millis(20), |_| false)
            .await
            .expect("waiting is not a rule");
        assert_eq!(drive.end, DriveEnd::Deadline);

        // A close with no Down IS one.
        drop(tx);
        let err = drive_lane(&mut rx, &mut checker, Duration::from_secs(2), |_| false)
            .await
            .expect_err("a lane that closes without Down broke a rule");
        assert_eq!(err.rule, Rule::EndedWithoutDown);

        // The source driver, on a stream whose second frame breaks a rule.
        let (tx, mut rx) = crate::lane::SourcePublisher::channel();
        tx.publish(sreset(1));
        tx.publish(sready(1));
        let mut checker = SourceChecker::new();
        let err = drive_source(&mut rx, &mut checker, Duration::from_secs(2), |_| false)
            .await
            .expect_err("a seed with no Capabilities");
        assert_eq!((err.rule, err.index), (Rule::CapabilitiesBeforeReady, 1));
    }
}
