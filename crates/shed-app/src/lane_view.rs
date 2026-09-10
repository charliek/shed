//! **The staged agent-lane view** — the client-side fold of a
//! [`shed_core::lane`] subscription into something renderable (plan 018 §3.5).
//!
//! One [`LaneView`] per open lane. Frames go in through [`LaneView::apply`] in
//! arrival order; what a reader sees comes out through
//! [`LaneView::snapshot`] as a typed [`LaneViewSnapshot`]. Nothing here does
//! I/O, spawns a task, or knows what a client renders with — the transport, the
//! reconnect loop and the IPC payload builders stay in whatever crate owns them.
//!
//! # Why it lives in shed-app
//!
//! It moved down out of the Tauri client (plan 018 §3.5) because it is not a
//! desktop concern: the phone folds the same subscription into the same view,
//! and two implementations of "which generation is a reader looking at" is
//! exactly the kind of divergence that shows up as "the phone shows something
//! else". `shed-app` is where the client-shared app logic lives, and this module
//! is **ungated** for the same reason [`crate::machine`] and [`crate::roost`]
//! are: shed-mobile links this crate with default features.
//!
//! The projection is therefore TYPED, not JSON. A Tauri command serialises
//! [`LaneViewSnapshot`] into its IPC envelope, the phone converts it into bridge
//! DTOs, and neither one re-parses the other's payload.
//!
//! # Staging, and what a reader is promised
//!
//! The contract's reseed bracket is [`LaneEvent::Reset`] … [`LaneEvent::Ready`],
//! and between them a client must not show a half-seeded transcript. So there
//! are two buffers: `live` is what a reader sees, `staged` is where frames go
//! mid-seed, and `Ready` swaps them in one move. A reader mid-seed sees the
//! PREVIOUS generation whole, and is told the previous generation's NUMBER —
//! which is why `generation` rides on the snapshot rather than on the view.
//!
//! [`LaneEvent::Down`] does not wipe anything: it marks the view stale with the
//! transport's reason, and the next `Ready` clears that.
//!
//! # The `since_seq` cut
//!
//! [`LaneView::snapshot`] takes an optional cursor so a poll-shaped consumer (a
//! phone waking up, a bridge re-reading after a gap) can ask for a delta instead
//! of five hundred rows. `None` is everything. `Some(s)` asks for the rows after
//! `s`, and is honored ONLY when `s` sits inside the current generation's seq
//! window; otherwise the answer is every row with `full: true`. See
//! [`LaneView::snapshot`] for why that guard is what makes a generation change
//! answer `full`.

use std::collections::{BTreeMap, VecDeque};

use shed_core::lane::{LaneApproval, LaneEvent, LaneSession};
use shed_core::rc::{RcActivity, RcFeedMessage};

/// How many transcript rows one lane keeps for the CURRENT generation.
///
/// The same 500 the adapter's own ring holds (`shed_opencode::MessageRing`'s
/// `MAX_RING_MESSAGES`), so a view that has replayed a whole generation holds
/// exactly what the adapter would hand back and no more.
pub const MAX_VIEW_MESSAGES: usize = 500;

/// One generation's accumulated truth: the transcript, the approvals, the row.
#[derive(Default)]
struct Snapshot {
    /// Which generation these rows belong to. It rides on the SNAPSHOT rather
    /// than on the view so that what a reader is told is the generation of the
    /// rows it is being handed — a client discards frames stamped older than
    /// what it HOLDS, and a number that moved at `Reset` would tell it to
    /// discard the very generation still on its screen.
    generation: u64,
    messages: VecDeque<RcFeedMessage>,
    /// Id-keyed and LAST-WRITE-WINS, exactly as the contract requires: an
    /// `Approval` frame may arrive `resolved` without its `pending` predecessor
    /// ever having been seen.
    approvals: BTreeMap<String, LaneApproval>,
    session: Option<LaneSession>,
}

impl Snapshot {
    fn push(&mut self, m: RcFeedMessage) {
        self.messages.push_back(m);
        while self.messages.len() > MAX_VIEW_MESSAGES {
            self.messages.pop_front();
        }
    }
}

/// What a reader is handed: one generation's rows, its activity, and the asks
/// still waiting on the human.
///
/// A projection, not a handle — everything in it is owned, so a caller can hold
/// it across an await or hand it over a bridge without keeping the view locked.
#[derive(Debug, Clone)]
pub struct LaneViewSnapshot {
    /// The transcript rows: every row of the current generation, or just the
    /// ones after the requested cursor. `full` says which.
    pub messages: Vec<RcFeedMessage>,
    /// `true` when `messages` is the WHOLE current generation and a reader
    /// should replace what it holds; `false` when it is a delta to append.
    pub full: bool,
    /// The session row's activity, or [`RcActivity::Unknown`] if no session row
    /// has been seen in this generation yet.
    pub activity: RcActivity,
    /// The generation `messages` belong to. Ours, monotonic, and it moves only
    /// when a seed completes — see [`Snapshot::generation`].
    pub generation: u64,
    /// `Some(reason)` between a [`LaneEvent::Down`] and the next `Ready`: the
    /// rows are the last complete generation and the transport is gone.
    pub stale: Option<String>,
    /// The asks still waiting on the human, oldest first, id as the tiebreak.
    /// Pending only — see [`LaneView::snapshot`].
    pub approvals: Vec<LaneApproval>,
}

/// The staged fold of one lane subscription.
///
/// `apply` in, [`LaneView::snapshot`] out. Cheap to construct
/// ([`Default`]) and it holds no I/O, so a client wraps it in whatever lock its
/// pump needs.
#[derive(Default)]
pub struct LaneView {
    /// Generations STARTED — bumped on every [`LaneEvent::Reset`], and stamped
    /// onto the snapshot that connect is seeding. See the module doc for why the
    /// counter is ours and not the adapter's.
    ///
    /// Not what a reader is told: that is `live.generation`, which only moves
    /// when a seed completes.
    started: u64,
    /// Set by [`LaneEvent::Down`], cleared by the next `Ready`.
    stale: Option<String>,
    /// What a reader sees.
    live: Snapshot,
    /// Where frames go between `Reset` and `Ready`. `None` in steady state.
    staged: Option<Snapshot>,
}

impl LaneView {
    /// The buffer the next frame belongs in: the staging one mid-seed, the live
    /// one otherwise.
    fn target(&mut self) -> &mut Snapshot {
        match self.staged.as_mut() {
            Some(staged) => staged,
            None => &mut self.live,
        }
    }

    /// Fold one frame in.
    pub fn apply(&mut self, event: &LaneEvent) {
        match event {
            LaneEvent::Reset { .. } => {
                self.started += 1;
                // The live view is deliberately UNTOUCHED: it keeps rendering
                // the last complete generation — and reporting ITS generation
                // number — until this one is whole.
                self.staged = Some(Snapshot {
                    generation: self.started,
                    ..Snapshot::default()
                });
            }
            LaneEvent::Message { message, .. } => self.target().push(message.clone()),
            LaneEvent::Session { session } => self.target().session = Some(session.clone()),
            LaneEvent::Approval { approval } => {
                let target = self.target();
                // Last-write-wins, then DROP what is no longer waiting on the
                // human. A generation can run for days, and every ask that ever
                // resolved inside it used to stay in this map with its whole
                // payload and `request_json` — nothing reads a non-pending entry
                // (`snapshot` filters to `is_pending`), so keeping one buys
                // nothing and costs the transcript of every tool call the agent
                // ever asked about.
                //
                // Written as insert-then-drop rather than "only insert pending"
                // because the two differ on the case that matters: a `resolved`
                // for an id this view holds as `pending` must REPLACE it, not be
                // ignored. A later `pending` for the same id re-inserts it — an
                // id the agent re-opens is a new ask, and this is the same
                // last-write-wins rule it always was.
                target
                    .approvals
                    .insert(approval.id.clone(), approval.clone());
                if !approval.status.is_pending() {
                    target.approvals.remove(&approval.id);
                }
            }
            LaneEvent::Ready { .. } => {
                if let Some(staged) = self.staged.take() {
                    self.live = staged;
                }
                self.stale = None;
            }
            LaneEvent::Down { reason } => self.stale = Some(reason.clone()),
            // A frame this build cannot name. The contract is explicit: ignore
            // it — not an error, not a gap, not a reason to resubscribe.
            LaneEvent::Unknown => {}
        }
    }

    /// What a reader sees: the live generation, projected.
    ///
    /// `since_seq` is `None` for everything (`full: true`) — the only thing the
    /// desktop asks for, and what any first read wants. `Some(s)` asks for a
    /// DELTA: the rows with `seq > s`, `full: false`.
    ///
    /// # When a cursor is refused
    ///
    /// A delta is honored only when `s` lands inside the current generation's
    /// seq window (`min ..= max` of the rows held). Outside it, the answer is
    /// every row with `full: true`, and that single guard is what makes the two
    /// ways a cursor goes stale safe:
    ///
    /// * **A generation change.** [`shed_core::lane`] pins `seq` as assigned by
    ///   the adapter's ring and monotonic ACROSS `Reset`s within one
    ///   subscription, so a reseed replays the transcript with FRESH, higher
    ///   seqs. A cursor from the previous generation therefore sits below the
    ///   new generation's window — refused, and the reader is handed the whole
    ///   new generation instead of a delta that would append duplicate rows to
    ///   rows it should have dropped.
    /// * **A resubscription.** A new subscription is a new ring, and its seqs
    ///   start over LOW. A cursor from the old one sits above the window —
    ///   refused, which is the contract's own "a client that sees a `seq` lower
    ///   than one it holds refetches" rule, enforced on this side of it.
    ///
    /// A cursor that fell out the back of the 500-row cap is refused by the same
    /// bound, for the same reason: the delta would have a hole in it.
    ///
    /// `generation` and `stale` ride EVERY snapshot, delta or not, so a reader
    /// that wants to react to either changing has them without a second call.
    ///
    /// `approvals` is always the complete pending set — there is no cursor for
    /// it and it is bounded by what the human has not answered. `is_pending()`
    /// rather than `!= Resolved` is the contract's rule: an unrecognized status
    /// is at least as likely to be terminal (`cancelled`, `expired`) as live,
    /// and offering answer buttons for it would post an answer the agent stopped
    /// listening for.
    pub fn snapshot(&self, since_seq: Option<u64>) -> LaneViewSnapshot {
        let (messages, full) = match since_seq.filter(|s| self.cursor_is_inside(*s)) {
            Some(since) => (
                self.live
                    .messages
                    .iter()
                    .filter(|m| m.seq > since)
                    .cloned()
                    .collect(),
                false,
            ),
            None => (self.live.messages.iter().cloned().collect(), true),
        };

        let mut approvals: Vec<LaneApproval> = self
            .live
            .approvals
            .values()
            .filter(|a| a.status.is_pending())
            .cloned()
            .collect();
        // Oldest first, id as the tiebreak so the order is total and stable
        // across reads (a BTreeMap already orders by id; this puts the ones the
        // agent has been waiting on longest at the top).
        approvals.sort_by(|a, b| {
            a.created_at_unix_ms
                .cmp(&b.created_at_unix_ms)
                .then_with(|| a.id.cmp(&b.id))
        });

        LaneViewSnapshot {
            messages,
            full,
            activity: self
                .live
                .session
                .as_ref()
                .map(|s| s.activity)
                .unwrap_or(RcActivity::Unknown),
            generation: self.live.generation,
            stale: self.stale.clone(),
            approvals,
        }
    }

    /// Is `since` a cursor this generation can answer a delta from?
    ///
    /// The window is `min ..= max` over the rows held rather than front-to-back,
    /// and that is DEFENSIVE rather than load-bearing — worth saying plainly,
    /// because an earlier version of this comment justified it with gx's
    /// non-monotonic event-id counters, which is a conflation: those counters
    /// are gx's own SSE resume cursor, not the `seq` on an [`RcFeedMessage`].
    /// `seq` is assigned by the adapter's bounded ring and
    /// [`shed_core::lane`]'s module doc pins it monotonic across `Reset`s within
    /// one subscription, so for a view driven by a real subscription
    /// `first ..= last` would be identical.
    ///
    /// It stays a fold anyway because `snapshot` and `apply` are both `pub` on a
    /// `pub` struct, and that monotonicity is a promise made in ANOTHER crate's
    /// docs and kept by adapter ring implementations. The phone's bridge builds
    /// a `LaneView` and feeds it events itself; `min ..= max` is correct under
    /// any input, `first ..= last` only while the promise holds. The cost is
    /// integer comparisons over at most 500 rows on a path that already clones
    /// up to 500 messages.
    ///
    /// An empty generation answers no cursor at all: a reader holding rows must
    /// be told to drop them, which is `full`.
    fn cursor_is_inside(&self, since: u64) -> bool {
        let mut seqs = self.live.messages.iter().map(|m| m.seq);
        let first = match seqs.next() {
            Some(s) => s,
            None => return false,
        };
        let (min, max) = seqs.fold((first, first), |(lo, hi), s| (lo.min(s), hi.max(s)));
        (min..=max).contains(&since)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    use shed_core::lane::{LaneApprovalKind, LaneApprovalStatus};

    /// A transcript row. `text` is the identity in these tests — the feed row
    /// has no id of its own, `seq` is the transport's, and the text is what a
    /// reader would actually see.
    fn message(text: &str, seq: u64) -> RcFeedMessage {
        RcFeedMessage {
            seq,
            role: "assistant".to_string(),
            msg_type: "text".to_string(),
            text: Some(text.to_string()),
            ..RcFeedMessage::default()
        }
    }

    fn approval(id: &str, status: LaneApprovalStatus) -> LaneApproval {
        LaneApproval {
            id: id.to_string(),
            session_id: "ses_a".to_string(),
            kind: LaneApprovalKind::Permission,
            status,
            title: id.to_string(),
            detail: None,
            options: Vec::new(),
            questions: Vec::new(),
            request_json: "{}".to_string(),
            created_at_unix_ms: None,
        }
    }

    fn rows(view: &LaneView) -> Vec<String> {
        view.live
            .messages
            .iter()
            .map(|m| m.text.clone().unwrap_or_default())
            .collect::<Vec<_>>()
    }

    fn texts(messages: &[RcFeedMessage]) -> Vec<String> {
        messages
            .iter()
            .map(|m| m.text.clone().unwrap_or_default())
            .collect()
    }

    fn approval_ids(snap: &LaneViewSnapshot) -> Vec<String> {
        snap.approvals.iter().map(|a| a.id.clone()).collect()
    }

    /// **The whole point of staging**: between `Reset` and `Ready` a reader sees
    /// the PREVIOUS generation whole, never an empty or half-seeded one.
    #[test]
    fn a_reseed_never_shows_a_partial_view() {
        let mut view = LaneView::default();
        for event in [
            LaneEvent::Reset {
                reason: "seed".into(),
                generation: 1,
            },
            LaneEvent::Message {
                message: message("m1", 1),
                cursor: None,
            },
            LaneEvent::Ready { generation: 1 },
        ] {
            view.apply(&event);
        }
        assert_eq!(rows(&view), ["m1"]);
        assert_eq!(view.live.generation, 1);

        // A reconnect starts over. Mid-seed the live view is untouched …
        view.apply(&LaneEvent::Reset {
            reason: "reconnect".into(),
            generation: 1,
        });
        assert_eq!(rows(&view), ["m1"], "the live view was cleared mid-seed");
        assert_eq!(
            view.snapshot(None).generation,
            1,
            "the reported generation moved before the rows it names did"
        );
        view.apply(&LaneEvent::Message {
            message: message("m1", 2),
            cursor: None,
        });
        assert_eq!(
            rows(&view),
            ["m1"],
            "a staged row leaked into the live view"
        );
        view.apply(&LaneEvent::Message {
            message: message("m2", 3),
            cursor: None,
        });
        assert_eq!(rows(&view), ["m1"]);

        // … and swaps in whole at Ready, with a HIGHER generation.
        view.apply(&LaneEvent::Ready { generation: 1 });
        assert_eq!(rows(&view), ["m1", "m2"]);
        assert_eq!(
            view.snapshot(None).generation,
            2,
            "generation must be ours, monotonic, and move only on the swap"
        );
        assert_eq!(
            view.live.messages.back().map(|m| m.seq),
            Some(3),
            "the re-seeded rows keep the adapter's higher seqs"
        );
    }

    /// `Down` is stale-with-a-reason, not a wipe; the next `Ready` clears it.
    #[test]
    fn down_marks_stale_and_keeps_the_rows() {
        let mut view = LaneView::default();
        view.apply(&LaneEvent::Reset {
            reason: "seed".into(),
            generation: 1,
        });
        view.apply(&LaneEvent::Message {
            message: message("m1", 1),
            cursor: None,
        });
        view.apply(&LaneEvent::Ready { generation: 1 });

        view.apply(&LaneEvent::Down {
            reason: "unknown_session".into(),
        });
        assert_eq!(rows(&view), ["m1"], "Down cleared the transcript");
        let payload = view.snapshot(None);
        assert_eq!(payload.stale.as_deref(), Some("unknown_session"));
        assert_eq!(payload.generation, 1);
        assert_eq!(payload.activity, RcActivity::Unknown);

        view.apply(&LaneEvent::Reset {
            reason: "reconnect".into(),
            generation: 1,
        });
        view.apply(&LaneEvent::Ready { generation: 1 });
        assert_eq!(view.snapshot(None).stale, None);
    }

    /// An `Unknown` frame is IGNORED — not a gap, not a reason to resubscribe,
    /// and above all not something that disturbs a staging swap in progress.
    #[test]
    fn an_unknown_frame_changes_nothing() {
        let mut view = LaneView::default();
        view.apply(&LaneEvent::Reset {
            reason: "seed".into(),
            generation: 1,
        });
        view.apply(&LaneEvent::Unknown);
        view.apply(&LaneEvent::Message {
            message: message("m1", 1),
            cursor: None,
        });
        view.apply(&LaneEvent::Unknown);
        view.apply(&LaneEvent::Ready { generation: 1 });
        assert_eq!(rows(&view), ["m1"]);
        assert_eq!(view.live.generation, 1);
    }

    /// The ring is bounded, oldest-first, at the adapter's own 500.
    #[test]
    fn the_view_keeps_the_last_500_rows() {
        let mut view = LaneView::default();
        for i in 0..(MAX_VIEW_MESSAGES + 25) {
            view.apply(&LaneEvent::Message {
                message: message(&format!("m{i}"), i as u64 + 1),
                cursor: None,
            });
        }
        assert_eq!(view.live.messages.len(), MAX_VIEW_MESSAGES);
        assert_eq!(
            view.live.messages.front().and_then(|m| m.text.clone()),
            Some("m25".into())
        );
    }

    /// Approvals are id-keyed and last-write-wins, and only PENDING ones are
    /// offered — an unknown status is not an affordance.
    #[test]
    fn approvals_are_last_write_wins_and_only_pending_surface() {
        let mut view = LaneView::default();
        for a in [
            approval("per_1", LaneApprovalStatus::Pending),
            approval("per_2", LaneApprovalStatus::Pending),
            approval("per_3", LaneApprovalStatus::Other("cancelled".into())),
            // The resolution of per_1, arriving without its own `pending`
            // predecessor having been re-sent.
            approval("per_1", LaneApprovalStatus::Resolved),
        ] {
            view.apply(&LaneEvent::Approval { approval: a });
        }
        assert_eq!(approval_ids(&view.snapshot(None)), ["per_2"]);
    }

    /// **Review finding: resolved approvals accumulated without bound.** A
    /// generation ends only at a `Reset`, and a healthy
    /// lane can run for days without one — so every approval that RESOLVED
    /// inside it used to stay in the snapshot forever, whole payload and
    /// `request_json` included, invisible to every reader.
    #[test]
    fn a_resolved_approval_is_dropped_and_a_reopened_one_comes_back() {
        let mut view = LaneView::default();
        let resolutions = [
            LaneApprovalStatus::Resolved,
            LaneApprovalStatus::Submitted,
            LaneApprovalStatus::Other("cancelled".into()),
        ];
        for i in 0..300 {
            let id = format!("per_{i}");
            view.apply(&LaneEvent::Approval {
                approval: approval(&id, LaneApprovalStatus::Pending),
            });
            assert_eq!(
                view.live.approvals.len(),
                1,
                "an ask the agent is waiting on must be held"
            );
            view.apply(&LaneEvent::Approval {
                approval: approval(&id, resolutions[i % resolutions.len()].clone()),
            });
            assert_eq!(
                view.live.approvals.len(),
                0,
                "{id} was still held after it stopped waiting on anyone"
            );
        }

        // A resolution for an id this view never saw pending is not a way to
        // plant one either.
        view.apply(&LaneEvent::Approval {
            approval: approval("never_asked", LaneApprovalStatus::Resolved),
        });
        assert!(view.live.approvals.is_empty());

        // Dropping is not forgetting: the agent re-opening an id it already
        // resolved is a NEW ask, and it has to render.
        view.apply(&LaneEvent::Approval {
            approval: approval("per_7", LaneApprovalStatus::Pending),
        });
        assert_eq!(approval_ids(&view.snapshot(None)), ["per_7"]);
    }

    /// Pending approvals come back oldest-first with the id as the tiebreak, and
    /// the order is the same on a delta read as on a full one — the cursor cuts
    /// the transcript, never the asks.
    #[test]
    fn pending_approvals_sort_by_created_at_then_id() {
        let mut view = LaneView::default();
        // One row, so `Some(1)` below is a cursor this generation can honor and
        // the second read really is a delta.
        view.apply(&LaneEvent::Message {
            message: message("m1", 1),
            cursor: None,
        });
        for (id, created) in [
            ("per_z", Some(100)),
            ("per_a", Some(300)),
            ("per_b", Some(100)),
            ("per_m", None),
        ] {
            let mut a = approval(id, LaneApprovalStatus::Pending);
            a.created_at_unix_ms = created;
            view.apply(&LaneEvent::Approval { approval: a });
        }
        // `None` sorts first (Option's own order), then 100 with the id as the
        // tiebreak, then 300.
        let expected = ["per_m", "per_b", "per_z", "per_a"];
        assert_eq!(approval_ids(&view.snapshot(None)), expected);
        assert_eq!(
            approval_ids(&view.snapshot(Some(1))),
            expected,
            "a delta read must answer the same pending set"
        );
    }

    /// `snapshot(Some(s))` is a DELTA: the rows after `s`, and nothing else.
    #[test]
    fn a_cursor_inside_the_generation_cuts_the_rows_it_names() {
        let mut view = LaneView::default();
        view.apply(&LaneEvent::Reset {
            reason: "seed".into(),
            generation: 1,
        });
        for (i, text) in ["m1", "m2", "m3", "m4"].iter().enumerate() {
            view.apply(&LaneEvent::Message {
                message: message(text, i as u64 + 1),
                cursor: None,
            });
        }
        view.apply(&LaneEvent::Ready { generation: 1 });

        let all = view.snapshot(None);
        assert!(all.full, "no cursor is a full read");
        assert_eq!(texts(&all.messages), ["m1", "m2", "m3", "m4"]);

        let delta = view.snapshot(Some(2));
        assert!(!delta.full, "an honored cursor answers a delta");
        assert_eq!(
            texts(&delta.messages),
            ["m3", "m4"],
            "the cut is seq > s, exclusive of the row the reader says it holds"
        );
        assert_eq!(delta.generation, 1, "a delta still names its generation");

        // Caught up exactly: nothing new, and still a delta (the reader's rows
        // are current, so telling it to replace them would be a lie).
        let caught_up = view.snapshot(Some(4));
        assert!(!caught_up.full);
        assert!(caught_up.messages.is_empty());

        // The first row's own seq is a legal cursor — the reader holds it.
        assert_eq!(texts(&view.snapshot(Some(1)).messages), ["m2", "m3", "m4"]);
    }

    /// A cursor from the PREVIOUS generation is refused, and the refusal is a
    /// full read.
    ///
    /// The reseed replays the transcript with fresh, higher seqs (the contract's
    /// ring rule), so the stale cursor sits below the new window. Honoring it
    /// would append the whole new generation as a "delta" onto rows the reader
    /// should have dropped.
    #[test]
    fn a_cursor_from_an_older_generation_answers_full() {
        let mut view = LaneView::default();
        view.apply(&LaneEvent::Reset {
            reason: "seed".into(),
            generation: 1,
        });
        view.apply(&LaneEvent::Message {
            message: message("m1", 1),
            cursor: None,
        });
        view.apply(&LaneEvent::Ready { generation: 1 });
        assert!(!view.snapshot(Some(1)).full, "gen 1's own cursor is fine");

        // Gen 2 replays the transcript with higher seqs.
        view.apply(&LaneEvent::Reset {
            reason: "reconnect".into(),
            generation: 1,
        });
        for (i, text) in ["m1", "m2"].iter().enumerate() {
            view.apply(&LaneEvent::Message {
                message: message(text, i as u64 + 2),
                cursor: None,
            });
        }
        view.apply(&LaneEvent::Ready { generation: 1 });

        let answer = view.snapshot(Some(1));
        assert_eq!(answer.generation, 2);
        assert!(
            answer.full,
            "a cursor below the new generation's window must answer full"
        );
        assert_eq!(texts(&answer.messages), ["m1", "m2"]);
    }

    /// A cursor from a previous SUBSCRIPTION — whose ring numbered from the
    /// bottom again — sits above the window, and is refused too. Honoring it
    /// would answer "nothing new" forever.
    #[test]
    fn a_cursor_above_the_window_answers_full() {
        let mut view = LaneView::default();
        for (i, text) in ["m1", "m2"].iter().enumerate() {
            view.apply(&LaneEvent::Message {
                message: message(text, i as u64 + 1),
                cursor: None,
            });
        }
        let answer = view.snapshot(Some(97));
        assert!(answer.full, "a cursor past the last row must answer full");
        assert_eq!(texts(&answer.messages), ["m1", "m2"]);
    }

    /// A cursor that fell out the back of the 500-row cap is refused by the same
    /// bound: the delta would have a hole in it.
    #[test]
    fn a_cursor_evicted_by_the_cap_answers_full() {
        let mut view = LaneView::default();
        for i in 0..(MAX_VIEW_MESSAGES + 25) {
            view.apply(&LaneEvent::Message {
                message: message(&format!("m{i}"), i as u64 + 1),
                cursor: None,
            });
        }
        // Rows 1..=25 are gone; a reader that stopped at 10 cannot be caught up
        // with a delta.
        let answer = view.snapshot(Some(10));
        assert!(answer.full);
        assert_eq!(answer.messages.len(), MAX_VIEW_MESSAGES);
        // 26 is the oldest row still held, so it IS a legal cursor.
        assert!(!view.snapshot(Some(26)).full);
    }

    /// An empty generation answers no cursor: a reader holding rows has to be
    /// told to drop them.
    #[test]
    fn an_empty_generation_answers_full() {
        let view = LaneView::default();
        let answer = view.snapshot(Some(3));
        assert!(answer.full);
        assert!(answer.messages.is_empty());
    }
}
