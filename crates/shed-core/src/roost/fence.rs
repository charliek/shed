//! The revision fence, and the event fold it guards.
//!
//! roost publishes workspace changes as [`EventBatch`]es carrying a monotonic
//! `revision`, and a `tab.list` taken on a session socket carries the revision it
//! was read at. Those two numbers are the whole loss-detection protocol: a client
//! discards every batch at or below its snapshot, applies the next one, and — if
//! a number is ever skipped — knows it lost a commit and must resync rather than
//! guess. roost pushes an **empty** batch for a commit that produced no events
//! precisely so a gap on the wire always means loss.
//!
//! Everything here is pure: no I/O, no clock, no connection. That is what makes
//! the fence testable against roost's own golden vectors, and what makes it
//! possible to implement it exactly once for every client shed ships.
//!
//! ## Re-seed per connection
//!
//! A fence belongs to one connection. roost's `revision` is an **in-process**
//! counter that resets to zero when the daemon restarts, while tab ids persist —
//! so a fence carried across a reconnect would discard every batch from the fresh
//! daemon until it counted past the old high-water mark. Build a new
//! [`Fence`] (and a new [`RoostInventory`]) from each connection's first
//! `tab.list`.
//!
//! ## The fold
//!
//! [`RoostInventory::apply`] is the other half: it turns a batch into inventory
//! changes. Two things about it are worth knowing before reading it.
//!
//! * **Unowned tabs are remembered, not listed.** A `tab.opened` almost always
//!   arrives *before* an agent claims the tab — the tab is a shell, then an
//!   adapter reports into it. `agent_report.changed` carries the axes but not the
//!   tab's title, cwd or creation time, so promoting a newly claimed tab needs
//!   the base [`roost_ipc::messages::Tab`] that `tab.opened` carried. It is kept
//!   in the inventory's hidden list for exactly that.
//! * **An unknown envelope is ignored, not an error.** roost's wire is
//!   additive-only; a new event kind from a newer daemon must not stop the fold.
//!   Same for an envelope whose `data` will not decode — that is one event lost,
//!   not a connection lost, and the next full `tab.list` repairs it.

use roost_ipc::messages::{
    ops, AgentReportChangedEvent, EventBatch, ProjectCreatedEvent, ProjectDeletedEvent,
    ProjectRenamedEvent, TabClosedEvent, TabCwdChangedEvent, TabNotificationEvent, TabOpenedEvent,
    TabTitleChangedEvent,
};

use super::model::{RoostInventory, RoostSession};

/// Decode an envelope's `data` payload, or `None` if it will not decode — one
/// lost event, not a lost connection.
///
/// Borrowed rather than `serde_json::from_value`, which would need an owned copy
/// of every payload the fold reads exactly once.
fn decode<'a, T: serde::Deserialize<'a>>(data: &'a serde_json::Value) -> Option<T> {
    T::deserialize(data).ok()
}

/// What to do with a batch, given the revision a client has already folded in.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Admit {
    /// Already folded in (or predates the snapshot). Drop it.
    Discard,
    /// The next revision. Fold it in; the fence has advanced.
    Apply,
    /// A revision was skipped: at least one commit was lost. Resync — take a
    /// fresh `tab.list`, re-seed the fence from it — rather than applying this
    /// batch onto a state that is missing whatever came before it.
    Gap { expected: u64, got: u64 },
}

/// The client's position in one connection's revision sequence.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Fence {
    current: u64,
}

impl Fence {
    /// Seed from the `revision` of the `tab.list` snapshot this connection
    /// started with.
    pub fn new(snapshot_revision: u64) -> Fence {
        Fence {
            current: snapshot_revision,
        }
    }

    /// The highest revision folded in so far.
    pub fn revision(&self) -> u64 {
        self.current
    }

    /// Decide what to do with a batch — and advance, but only on
    /// [`Admit::Apply`].
    ///
    /// A gap deliberately does **not** advance: the caller's state is now known
    /// to be behind, and silently jumping the fence forward would make the next
    /// batch look contiguous and bury the loss.
    pub fn admit(&mut self, batch_revision: u64) -> Admit {
        if batch_revision <= self.current {
            Admit::Discard
        } else if batch_revision == self.current + 1 {
            self.current = batch_revision;
            Admit::Apply
        } else {
            Admit::Gap {
                expected: self.current + 1,
                got: batch_revision,
            }
        }
    }
}

/// Where a tab sits in an inventory: which half, and at what index.
///
/// The index is what keeps roost's order: a fold that knows only "it is in the
/// listed half" can put a row back only by appending it.
#[derive(Debug, Clone, Copy)]
enum Slot {
    Listed(usize),
    Hidden(usize),
}

impl RoostInventory {
    /// Fold one admitted batch into the inventory.
    ///
    /// Call it only for a batch [`Fence::admit`] answered [`Admit::Apply`] for —
    /// this function does no fencing of its own, on purpose: the fence is the
    /// caller's sequencing decision and the fold is the state change, and mixing
    /// them would make a discard indistinguishable from a no-op batch.
    pub fn apply(&mut self, batch: &EventBatch) {
        for envelope in &batch.events {
            self.apply_event(envelope.event.as_str(), &envelope.data);
        }
        self.revision = Some(batch.revision);
    }

    fn apply_event(&mut self, event: &str, data: &serde_json::Value) {
        match event {
            ops::EVENT_TAB_OPENED => {
                // Usually an unowned shell tab: it lands hidden and becomes a row
                // later, when an adapter claims it via `agent_report.changed`.
                if let Some(opened) = decode::<TabOpenedEvent>(data) {
                    let name = self
                        .projects
                        .get(&opened.tab.project_id)
                        .cloned()
                        .unwrap_or_default();
                    let session =
                        RoostSession::from_tab_parts(&self.label_for_new(), &name, &opened.tab);
                    self.upsert(session);
                }
            }
            ops::EVENT_TAB_CLOSED => {
                if let Some(closed) = decode::<TabClosedEvent>(data) {
                    self.remove(closed.tab_id);
                }
            }
            ops::EVENT_TAB_TITLE_CHANGED => {
                if let Some(changed) = decode::<TabTitleChangedEvent>(data) {
                    if let Some(session) = self.find_mut(changed.tab_id) {
                        session.title = changed.title;
                    }
                }
            }
            ops::EVENT_TAB_CWD_CHANGED => {
                if let Some(changed) = decode::<TabCwdChangedEvent>(data) {
                    if let Some(session) = self.find_mut(changed.tab_id) {
                        session.cwd = changed.cwd;
                    }
                }
            }
            // `tab.state` is DERIVED from the three axes roost also publishes, and
            // the model carries the axes. Folding the projection in would be
            // storing the same fact twice, in two places that can disagree.
            ops::EVENT_TAB_STATE_CHANGED => {}
            ops::EVENT_TAB_NOTIFICATION => {
                if let Some(fired) = decode::<TabNotificationEvent>(data) {
                    if let Some(session) = self.find_mut(fired.tab_id) {
                        session.attention = fired.has_pending;
                    }
                }
            }
            ops::EVENT_AGENT_REPORT_CHANGED => {
                if let Some(report) = decode::<AgentReportChangedEvent>(data) {
                    // Ownership may have appeared (a shell tab becomes a session
                    // row) or gone (the adapter released it), so which half the
                    // row belongs in is re-decided — but the row only MOVES when
                    // that answer actually changed.
                    self.reclassify(report.tab_id, |session| {
                        session.shell_state = report.shell_state;
                        session.lifecycle = report.agent_lifecycle;
                        session.ownership = report.ownership;
                    });
                }
            }
            ops::EVENT_PROJECT_CREATED => {
                if let Some(created) = decode::<ProjectCreatedEvent>(data) {
                    self.projects
                        .insert(created.project.id, created.project.name.clone());
                    let label = self.label_for_new();
                    for tab in &created.project.tabs {
                        let session =
                            RoostSession::from_tab_parts(&label, &created.project.name, tab);
                        self.upsert(session);
                    }
                }
            }
            ops::EVENT_PROJECT_RENAMED => {
                if let Some(renamed) = decode::<ProjectRenamedEvent>(data) {
                    self.projects
                        .insert(renamed.project_id, renamed.name.clone());
                    for session in self.sessions.iter_mut().chain(self.hidden.iter_mut()) {
                        if session.project_id == renamed.project_id {
                            session.project_name = renamed.name.clone();
                        }
                    }
                }
            }
            ops::EVENT_PROJECT_DELETED => {
                if let Some(deleted) = decode::<ProjectDeletedEvent>(data) {
                    self.projects.remove(&deleted.project_id);
                    // A deleted project takes its tabs with it.
                    self.sessions.retain(|s| s.project_id != deleted.project_id);
                    self.hidden.retain(|s| s.project_id != deleted.project_id);
                }
            }
            // Every other envelope — `active.changed`, the reorders,
            // `hook_active.changed`, `notification.fired`, `session.stopping`,
            // and whatever a newer daemon adds — says nothing this model carries.
            _ => {}
        }
    }

    /// The host label to stamp on a row this fold creates: the reach's, stored on
    /// the inventory at `from_list` time so it is right even when the inventory
    /// is empty (its first `tab.opened`, or the one after its last `tab.closed`).
    fn label_for_new(&self) -> String {
        self.host_label.clone()
    }

    /// Insert or replace by tab id, filing the row into the listed or the hidden
    /// half according to ownership.
    ///
    /// A row that is already in the half it belongs in is replaced **in place**.
    /// Order is roost's, not ours: the rows are carried in `tab.list` order and
    /// both clients render them in it, so re-appending a row that merely changed
    /// would shuffle a session to the bottom of the user's list for no reason
    /// they can see. Only a row that crosses the listed/hidden line — or one that
    /// is genuinely new — is appended.
    fn upsert(&mut self, session: RoostSession) {
        let listed = session.is_agent_owned();
        match (self.locate(session.tab_id), listed) {
            (Some(Slot::Listed(at)), true) => self.sessions[at] = session,
            (Some(Slot::Hidden(at)), false) => self.hidden[at] = session,
            (slot, _) => {
                if let Some(slot) = slot {
                    self.take_at(slot);
                }
                if listed {
                    self.sessions.push(session);
                } else {
                    self.hidden.push(session);
                }
            }
        }
    }

    /// Apply `change` to a tab wherever it sits, **keeping its index**, and move
    /// it between the halves only if the change flipped its ownership.
    ///
    /// The alternative — take the row out, mutate, put it back — is what makes an
    /// ordinary `agent_report.changed` (a lifecycle tick, several a minute on a
    /// busy agent) re-append the row and reorder the whole session list.
    fn reclassify(&mut self, tab_id: i64, change: impl FnOnce(&mut RoostSession)) {
        let Some(slot) = self.locate(tab_id) else {
            return;
        };
        let session = match slot {
            Slot::Listed(at) => &mut self.sessions[at],
            Slot::Hidden(at) => &mut self.hidden[at],
        };
        change(session);
        let listed = session.is_agent_owned();
        if listed == matches!(slot, Slot::Listed(_)) {
            return;
        }
        let moved = self.take_at(slot);
        if listed {
            self.sessions.push(moved);
        } else {
            self.hidden.push(moved);
        }
    }

    fn remove(&mut self, tab_id: i64) {
        self.sessions.retain(|s| s.tab_id != tab_id);
        self.hidden.retain(|s| s.tab_id != tab_id);
    }

    /// Which half holds a tab, and where in it.
    fn locate(&self, tab_id: i64) -> Option<Slot> {
        if let Some(at) = self.sessions.iter().position(|s| s.tab_id == tab_id) {
            return Some(Slot::Listed(at));
        }
        self.hidden
            .iter()
            .position(|s| s.tab_id == tab_id)
            .map(Slot::Hidden)
    }

    /// Pull the row at a slot [`Self::locate`] just returned out of its half.
    fn take_at(&mut self, slot: Slot) -> RoostSession {
        match slot {
            Slot::Listed(at) => self.sessions.remove(at),
            Slot::Hidden(at) => self.hidden.remove(at),
        }
    }

    /// A tab by id, listed or hidden.
    fn find_mut(&mut self, tab_id: i64) -> Option<&mut RoostSession> {
        self.sessions
            .iter_mut()
            .chain(self.hidden.iter_mut())
            .find(|s| s.tab_id == tab_id)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::rc::{RcActivity, RcKind};
    use crate::roost::result_of;
    use roost_ipc::agent::{AgentLifecycle, ShellState};
    use roost_ipc::messages::{EventEnvelope, SessionIdentify, TabListResult};

    // The vendored roost vectors. Never semantically edited — see the README
    // beside them.
    const VECTOR_TAB_LIST: &str =
        include_str!("../../../fixtures/roost-vectors/tab.list.session.response.json");
    const VECTOR_SESSION_IDENTIFY: &str =
        include_str!("../../../fixtures/roost-vectors/session.identify.response.v4.json");
    const VECTOR_EVENTS_BATCH: &str =
        include_str!("../../../fixtures/roost-vectors/events.batch.json");
    const VECTOR_TAB_OPENED: &str =
        include_str!("../../../fixtures/roost-vectors/tab.opened.event.json");
    const VECTOR_TAB_STATE_CHANGED: &str =
        include_str!("../../../fixtures/roost-vectors/tab.state_changed.event.json");
    const VECTOR_AGENT_REPORT_CHANGED: &str =
        include_str!("../../../fixtures/roost-vectors/agent_report.changed.event.json");
    const VECTOR_SESSION_STOPPING: &str =
        include_str!("../../../fixtures/roost-vectors/session.stopping.event.json");

    fn envelope(vector: &str) -> EventEnvelope {
        serde_json::from_str(vector).expect("an event vector decodes as an envelope")
    }

    /// A batch at `revision`, built from the vendored envelope vectors — the
    /// exact shape `events.batch.json` has (`{revision, events: […]}`).
    fn batch(revision: u64, vectors: &[&str]) -> EventBatch {
        EventBatch {
            revision,
            events: vectors.iter().copied().map(envelope).collect(),
        }
    }

    fn seeded() -> (Fence, RoostInventory) {
        let list: TabListResult = result_of(VECTOR_TAB_LIST);
        let identify: SessionIdentify = result_of(VECTOR_SESSION_IDENTIFY);
        let revision = list
            .revision
            .expect("the session tab.list carries a revision");
        assert_eq!(revision, 42, "the vendored snapshot is at revision 42");
        (
            Fence::new(revision),
            RoostInventory::from_list("localhost", &list, &identify),
        )
    }

    #[test]
    fn the_vendored_batch_vector_decodes_as_a_batch() {
        let batch: EventBatch = serde_json::from_str(VECTOR_EVENTS_BATCH).expect("valid batch");
        assert_eq!(batch.revision, 42);
        assert_eq!(batch.events.len(), 2);
        assert_eq!(batch.events[0].event, "tab.opened");
        assert_eq!(batch.events[1].event, "active.changed");
    }

    /// The full replay the plan pins: a snapshot at 42, batches at 41, 42, 43,
    /// 44, then a jump to 46.
    #[test]
    fn replays_the_vendored_snapshot_against_a_batch_sequence() {
        let (mut fence, mut inventory) = seeded();
        // The vendored snapshot's one tab (id 5) is an unowned shell — no rows.
        assert!(inventory.sessions.is_empty());
        assert_eq!(inventory.revision, Some(42));

        // 41 — behind the snapshot. Discarded, nothing folded, fence unmoved.
        let stale = batch(41, &[VECTOR_AGENT_REPORT_CHANGED]);
        assert_eq!(fence.admit(stale.revision), Admit::Discard);
        assert!(inventory.sessions.is_empty());
        assert_eq!(fence.revision(), 42);

        // 42 — the snapshot's own revision. Already reflected; discarded.
        let replayed: EventBatch = serde_json::from_str(VECTOR_EVENTS_BATCH).expect("valid batch");
        assert_eq!(replayed.revision, 42);
        assert_eq!(fence.admit(replayed.revision), Admit::Discard);
        assert!(inventory.sessions.is_empty());
        assert_eq!(fence.revision(), 42);

        // 43 — the next revision. Applied: an adapter claims tab 5, which was the
        // hidden shell tab, so it becomes the inventory's first row.
        let claim = batch(43, &[VECTOR_AGENT_REPORT_CHANGED, VECTOR_TAB_STATE_CHANGED]);
        assert_eq!(fence.admit(claim.revision), Admit::Apply);
        inventory.apply(&claim);
        assert_eq!(fence.revision(), 43);
        assert_eq!(inventory.revision, Some(43));
        assert_eq!(inventory.sessions.len(), 1);
        let row = &inventory.sessions[0];
        assert_eq!(row.tab_id, 5);
        // The promoted row kept the base tab's identity from the snapshot ...
        assert_eq!(row.title, "zsh");
        assert_eq!(row.cwd, "/Users/me/projects/roost");
        assert_eq!(row.project_name, "Roost");
        assert_eq!(row.host_label, "localhost");
        assert_eq!(row.created_at, 1_700_000_000);
        // ... and took its axes from the report.
        assert_eq!(row.lifecycle, AgentLifecycle::Failed);
        assert_eq!(row.shell_state, ShellState::ForegroundProcess);
        assert_eq!(row.agent_kind(), RcKind::ClaudeRc);
        assert_eq!(row.activity(), Some(RcActivity::NeedsInput));
        assert_eq!(row.to_rc_dto().id.as_deref(), Some("abc123"));

        // 44 — applied. A plain shell tab opens: remembered, but not a row.
        let opened = batch(44, &[VECTOR_TAB_OPENED]);
        assert_eq!(fence.admit(opened.revision), Admit::Apply);
        inventory.apply(&opened);
        assert_eq!(fence.revision(), 44);
        assert_eq!(inventory.revision, Some(44));
        assert_eq!(inventory.sessions.len(), 1, "an unowned tab is not a row");

        // 46 — a revision was skipped. Gap, nothing folded, fence unmoved so the
        // loss cannot be buried by the next contiguous-looking batch.
        let jumped = batch(46, &[VECTOR_TAB_OPENED]);
        assert_eq!(
            fence.admit(jumped.revision),
            Admit::Gap {
                expected: 45,
                got: 46
            }
        );
        assert_eq!(fence.revision(), 44);
        assert_eq!(inventory.revision, Some(44));
        assert_eq!(inventory.sessions.len(), 1);
    }

    #[test]
    fn a_gap_does_not_advance_the_fence() {
        let mut fence = Fence::new(10);
        assert_eq!(
            fence.admit(13),
            Admit::Gap {
                expected: 11,
                got: 13
            }
        );
        assert_eq!(fence.revision(), 10);
        // Still asking for 11, not 14.
        assert_eq!(fence.admit(11), Admit::Apply);
        assert_eq!(fence.revision(), 11);
    }

    #[test]
    fn admits_a_contiguous_run_and_discards_every_replay() {
        let mut fence = Fence::new(0);
        for revision in 1..=5 {
            assert_eq!(fence.admit(revision), Admit::Apply, "revision {revision}");
        }
        assert_eq!(fence.revision(), 5);
        for revision in 0..=5 {
            assert_eq!(fence.admit(revision), Admit::Discard, "replay {revision}");
        }
        assert_eq!(fence.revision(), 5);
    }

    #[test]
    fn title_cwd_and_notification_fold_onto_a_row() {
        let (_, mut inventory) = seeded();
        inventory.apply(&batch(43, &[VECTOR_AGENT_REPORT_CHANGED]));

        let renamed = EventEnvelope {
            event: ops::EVENT_TAB_TITLE_CHANGED.to_string(),
            data: serde_json::json!({ "tab_id": "5", "title": "claude" }),
        };
        let moved = EventEnvelope {
            event: ops::EVENT_TAB_CWD_CHANGED.to_string(),
            data: serde_json::json!({ "tab_id": "5", "cwd": "/tmp/work" }),
        };
        let noticed = EventEnvelope {
            event: ops::EVENT_TAB_NOTIFICATION.to_string(),
            data: serde_json::json!({ "tab_id": "5", "has_pending": true }),
        };
        inventory.apply(&EventBatch {
            revision: 44,
            events: vec![renamed, moved, noticed],
        });

        let row = &inventory.sessions[0];
        assert_eq!(row.title, "claude");
        assert_eq!(row.cwd, "/tmp/work");
        assert!(row.attention, "roost's sticky notification bit");
        assert_eq!(row.to_rc_dto().display_name.as_deref(), Some("claude"));

        // ... and clears when roost says it cleared.
        inventory.apply(&EventBatch {
            revision: 45,
            events: vec![EventEnvelope {
                event: ops::EVENT_TAB_NOTIFICATION.to_string(),
                data: serde_json::json!({ "tab_id": "5", "has_pending": false }),
            }],
        });
        assert!(!inventory.sessions[0].attention);
    }

    #[test]
    fn a_closed_tab_leaves_the_inventory() {
        let (_, mut inventory) = seeded();
        inventory.apply(&batch(43, &[VECTOR_AGENT_REPORT_CHANGED]));
        assert_eq!(inventory.sessions.len(), 1);

        inventory.apply(&EventBatch {
            revision: 44,
            events: vec![EventEnvelope {
                event: ops::EVENT_TAB_CLOSED.to_string(),
                data: serde_json::json!({ "tab_id": "5" }),
            }],
        });
        assert!(inventory.sessions.is_empty());
        // Really gone, not demoted into the hidden half.
        inventory.apply(&batch(45, &[VECTOR_AGENT_REPORT_CHANGED]));
        assert!(inventory.sessions.is_empty());
    }

    #[test]
    fn releasing_ownership_demotes_a_row_and_reclaiming_promotes_it_again() {
        let (_, mut inventory) = seeded();
        inventory.apply(&batch(43, &[VECTOR_AGENT_REPORT_CHANGED]));
        assert_eq!(inventory.sessions.len(), 1);

        // The adapter releases the tab: it stops being a session row ...
        inventory.apply(&EventBatch {
            revision: 44,
            events: vec![EventEnvelope {
                event: ops::EVENT_AGENT_REPORT_CHANGED.to_string(),
                data: serde_json::json!({
                    "tab_id": "5",
                    "shell_state": "at_prompt",
                    "agent_lifecycle": "inactive",
                    "state": "none",
                    "hook_active": false
                }),
            }],
        });
        assert!(inventory.sessions.is_empty());

        // ... and is still there to be promoted when something claims it again,
        // with its title/cwd intact.
        inventory.apply(&batch(45, &[VECTOR_AGENT_REPORT_CHANGED]));
        assert_eq!(inventory.sessions.len(), 1);
        assert_eq!(inventory.sessions[0].title, "zsh");
    }

    /// A `tab.opened` envelope for a plain shell tab.
    fn opened(tab_id: i64) -> EventEnvelope {
        EventEnvelope {
            event: ops::EVENT_TAB_OPENED.to_string(),
            data: serde_json::json!({
                "tab": {
                    "id": tab_id.to_string(),
                    "project_id": "1",
                    "title": format!("tab {tab_id}"),
                    "cwd": "/Users/me",
                    "state": "none",
                    "has_notification": false,
                    "is_active": false,
                    "user_titled": false,
                    "position": tab_id,
                    "created_at": 1_700_001_000i64,
                    "last_active": 1_700_001_000i64,
                    "hook_active": false
                }
            }),
        }
    }

    /// An `agent_report.changed` envelope claiming `tab_id` for opencode at
    /// `lifecycle` — the ordinary lifecycle tick a live agent emits over and over.
    fn reported(tab_id: i64, lifecycle: &str) -> EventEnvelope {
        EventEnvelope {
            event: ops::EVENT_AGENT_REPORT_CHANGED.to_string(),
            data: serde_json::json!({
                "tab_id": tab_id.to_string(),
                "shell_state": "foreground_process",
                "agent_lifecycle": lifecycle,
                "ownership": {
                    "source": "opencode",
                    "session_id": format!("ses_{tab_id}"),
                    "last_event_at": 1_700_001_100i64,
                    "detail": "session_status",
                    "metadata": {}
                },
                "state": "running",
                "hook_active": true
            }),
        }
    }

    fn tab_ids(rows: &[RoostSession]) -> Vec<i64> {
        rows.iter().map(|s| s.tab_id).collect()
    }

    /// **An ordinary status change must not reshuffle the list.** roost owns the
    /// order — the rows are carried in `tab.list` order and both clients render
    /// them in it — and a lifecycle tick is not a reorder. Folding one by taking
    /// the row out and putting it back sent that session to the bottom of the
    /// user's list several times a minute, which the poller used to paper over by
    /// re-deriving the order from every `tab.list`.
    #[test]
    fn an_agent_report_keeps_the_rows_order() {
        let (_, mut inventory) = seeded();
        // Three claimed rows, in the order they arrived.
        inventory.apply(&EventBatch {
            revision: 43,
            events: vec![
                opened(7),
                opened(8),
                reported(5, "working"),
                reported(7, "working"),
                reported(8, "working"),
            ],
        });
        assert_eq!(tab_ids(&inventory.sessions), vec![5, 7, 8]);

        // The middle row ticks through a whole lifecycle. It stays put, and it
        // still carries the new axes.
        for lifecycle in ["waiting", "working", "finished"] {
            inventory.apply(&EventBatch {
                revision: inventory.revision.unwrap_or(43) + 1,
                events: vec![reported(7, lifecycle)],
            });
            assert_eq!(
                tab_ids(&inventory.sessions),
                vec![5, 7, 8],
                "a {lifecycle} report moved the row"
            );
        }
        assert_eq!(inventory.sessions[1].lifecycle, AgentLifecycle::Finished);

        // The control: crossing the listed/hidden line DOES re-file the row —
        // there is no index to keep in the other half.
        inventory.apply(&EventBatch {
            revision: inventory.revision.unwrap_or(43) + 1,
            events: vec![EventEnvelope {
                event: ops::EVENT_AGENT_REPORT_CHANGED.to_string(),
                data: serde_json::json!({
                    "tab_id": "5",
                    "shell_state": "at_prompt",
                    "agent_lifecycle": "inactive",
                    "state": "none",
                    "hook_active": false
                }),
            }],
        });
        assert_eq!(tab_ids(&inventory.sessions), vec![7, 8]);
        inventory.apply(&EventBatch {
            revision: inventory.revision.unwrap_or(43) + 1,
            events: vec![reported(5, "working")],
        });
        assert_eq!(
            tab_ids(&inventory.sessions),
            vec![7, 8, 5],
            "a re-promoted row lands where the fold learned of it, until the \
             next tab.list re-derives roost's own order"
        );
    }

    #[test]
    fn project_events_fold_names_and_deletions() {
        let (_, mut inventory) = seeded();
        inventory.apply(&batch(43, &[VECTOR_AGENT_REPORT_CHANGED]));
        assert_eq!(inventory.sessions[0].project_name, "Roost");

        inventory.apply(&EventBatch {
            revision: 44,
            events: vec![EventEnvelope {
                event: ops::EVENT_PROJECT_RENAMED.to_string(),
                data: serde_json::json!({ "project_id": "1", "name": "Roost (main)" }),
            }],
        });
        assert_eq!(inventory.sessions[0].project_name, "Roost (main)");

        inventory.apply(&EventBatch {
            revision: 45,
            events: vec![EventEnvelope {
                event: ops::EVENT_PROJECT_DELETED.to_string(),
                data: serde_json::json!({ "project_id": "1" }),
            }],
        });
        assert!(
            inventory.sessions.is_empty(),
            "a deleted project takes its tabs"
        );
    }

    /// A newly opened tab under a project the fold has never seen still gets a
    /// row once claimed — the project name is simply blank until a `tab.list`
    /// refreshes it.
    #[test]
    fn a_tab_opened_under_a_created_project_carries_that_projects_name() {
        let (_, mut inventory) = seeded();
        inventory.apply(&EventBatch {
            revision: 43,
            events: vec![
                EventEnvelope {
                    event: ops::EVENT_PROJECT_CREATED.to_string(),
                    data: serde_json::json!({
                        "project": {
                            "id": "9",
                            "name": "shed",
                            "cwd": "/home/me/projects/shed",
                            "position": 1,
                            "created_at": 1_700_000_100i64,
                            "tabs": []
                        }
                    }),
                },
                EventEnvelope {
                    event: ops::EVENT_TAB_OPENED.to_string(),
                    data: serde_json::json!({
                        "tab": {
                            "id": "11",
                            "project_id": "9",
                            "title": "opencode",
                            "cwd": "/home/me/projects/shed",
                            "state": "running",
                            "has_notification": false,
                            "is_active": true,
                            "user_titled": false,
                            "position": 0,
                            "created_at": 1_700_000_200i64,
                            "last_active": 1_700_000_200i64,
                            "hook_active": true,
                            "shell_state": "foreground_process",
                            "agent_lifecycle": "working",
                            "ownership": {
                                "source": "opencode",
                                "session_id": "ses_abc",
                                "last_event_at": 1_700_000_200i64,
                                "detail": "session_created",
                                "metadata": {}
                            }
                        }
                    }),
                },
            ],
        });

        assert_eq!(inventory.sessions.len(), 1);
        let row = &inventory.sessions[0];
        assert_eq!(row.tab_id, 11);
        assert_eq!(row.project_id, 9);
        assert_eq!(row.project_name, "shed");
        assert_eq!(
            row.host_label, "localhost",
            "the reach's label is inherited"
        );
        assert_eq!(row.agent_kind(), RcKind::Opencode);
        assert_eq!(row.activity(), Some(RcActivity::Working));
    }

    /// An inventory with no rows to copy a label from — freshly listed on an
    /// idle daemon, or one that has just folded its last `tab.closed` — still
    /// stamps the reach's label on the next row the fold creates. (CodeRabbit
    /// review finding on C2: the label used to be recomputed from the rows.)
    #[test]
    fn a_fold_created_row_carries_the_reach_label_even_from_an_empty_inventory() {
        let identify: SessionIdentify = result_of(VECTOR_SESSION_IDENTIFY);
        let empty = TabListResult {
            projects: Vec::new(),
            revision: Some(5),
        };
        let mut inventory = RoostInventory::from_list("mini3", &empty, &identify);
        assert!(inventory.sessions.is_empty());
        assert_eq!(inventory.host_label(), "mini3");

        let owned_tab_opened = || EventEnvelope {
            event: ops::EVENT_TAB_OPENED.to_string(),
            data: serde_json::json!({
                "tab": {
                    "id": "21",
                    "project_id": "0",
                    "title": "opencode",
                    "cwd": "/home/me",
                    "state": "running",
                    "has_notification": false,
                    "is_active": true,
                    "user_titled": false,
                    "position": 0,
                    "created_at": 1_700_000_300i64,
                    "last_active": 1_700_000_300i64,
                    "hook_active": true,
                    "shell_state": "foreground_process",
                    "agent_lifecycle": "working",
                    "ownership": {
                        "source": "opencode",
                        "session_id": "ses_new",
                        "last_event_at": 1_700_000_300i64,
                        "detail": "session_created",
                        "metadata": {}
                    }
                }
            }),
        };
        inventory.apply(&EventBatch {
            revision: 6,
            events: vec![owned_tab_opened()],
        });
        assert_eq!(inventory.sessions.len(), 1);
        assert_eq!(inventory.sessions[0].host_label, "mini3");

        // The same after the inventory empties out again.
        let (_, mut seeded_inventory) = seeded();
        let ids: Vec<i64> = seeded_inventory
            .sessions
            .iter()
            .chain(seeded_inventory.hidden.iter())
            .map(|s| s.tab_id)
            .collect();
        for id in ids {
            seeded_inventory.apply(&EventBatch {
                revision: 43,
                events: vec![EventEnvelope {
                    event: ops::EVENT_TAB_CLOSED.to_string(),
                    data: serde_json::json!({ "tab_id": id.to_string() }),
                }],
            });
        }
        assert!(seeded_inventory.sessions.is_empty());
        assert!(seeded_inventory.hidden.is_empty());
        seeded_inventory.apply(&EventBatch {
            revision: 44,
            events: vec![owned_tab_opened()],
        });
        assert_eq!(seeded_inventory.sessions[0].host_label, "localhost");
    }

    /// Envelopes this model has no opinion about — a derived projection, a
    /// transport control frame, and something no daemon has invented yet — are
    /// skipped without disturbing anything. Same for a well-named envelope whose
    /// `data` will not decode: one lost event, not a lost connection.
    #[test]
    fn unknown_and_undecodable_envelopes_are_ignored() {
        let (_, mut inventory) = seeded();
        inventory.apply(&batch(43, &[VECTOR_AGENT_REPORT_CHANGED]));
        let before = inventory.clone();

        inventory.apply(&EventBatch {
            revision: 44,
            events: vec![
                envelope(VECTOR_TAB_STATE_CHANGED),
                envelope(VECTOR_SESSION_STOPPING),
                EventEnvelope {
                    event: "something.newer".to_string(),
                    data: serde_json::json!({ "whatever": true }),
                },
                EventEnvelope {
                    event: ops::EVENT_TAB_TITLE_CHANGED.to_string(),
                    data: serde_json::json!({ "tab_id": 5, "title": 42 }),
                },
            ],
        });

        assert_eq!(inventory.sessions, before.sessions);
        assert_eq!(inventory.revision, Some(44), "the batch still advanced it");
    }

    /// An empty batch is roost's "this commit changed nothing" — it must still
    /// move the revision, which is exactly what keeps a gap meaning loss.
    #[test]
    fn an_empty_batch_still_advances_the_revision() {
        let (mut fence, mut inventory) = seeded();
        let empty = EventBatch {
            revision: 43,
            events: Vec::new(),
        };
        assert_eq!(fence.admit(empty.revision), Admit::Apply);
        inventory.apply(&empty);
        assert_eq!(inventory.revision, Some(43));
        assert!(inventory.sessions.is_empty());
    }
}
