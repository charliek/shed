//! Contract tests for [`crate::lane`].
//!
//! Two jobs. **Round-trip**: every DTO survives serialize → deserialize as
//! itself, because shed-mobile hand-mirrors these types and a field that only
//! encodes one way is a silent data loss on the Dart side. **Shape**: the tagged
//! and string-enum representations are pinned against literal JSON, because the
//! mirror is written against those bytes — a `rename_all` slipping off a type is
//! not a compile error here, it is a wire break there.

use super::*;
use serde::de::DeserializeOwned;
use serde_json::json;

/// Serialize, deserialize, assert identity — and hand back the encoded value so
/// a caller can also pin the shape.
fn round_trip<T>(v: &T) -> serde_json::Value
where
    T: Serialize + DeserializeOwned + PartialEq + std::fmt::Debug,
{
    let encoded = serde_json::to_value(v).expect("serialize");
    let decoded: T = serde_json::from_value(encoded.clone()).expect("deserialize");
    assert_eq!(&decoded, v);
    encoded
}

fn sample_session() -> LaneSession {
    LaneSession {
        id: "ses_1".into(),
        title: "wire up the lane".into(),
        cwd: "/home/shed/shed".into(),
        activity: RcActivity::Working,
        pending_approvals: 2,
        approximate: false,
        parent_id: Some("ses_root".into()),
        last_change_unix_ms: Some(1_757_260_800_000),
    }
}

/// One offered option, with an id deliberately UNRELATED to its kind — the
/// contract's rule (module doc, correction 3) is that ids are opaque and `kind`
/// carries the semantics, and a sample whose id spells its own kind would let an
/// id-sniffing bug pass every test in this file.
fn sample_option() -> LaneApprovalOption {
    LaneApprovalOption {
        id: "p-1".into(),
        label: "Allow once".into(),
        description: Some("Just this invocation".into()),
        kind: Some(option_kind::ALLOW_ONCE.into()),
    }
}

fn sample_approval() -> LaneApproval {
    LaneApproval {
        id: "perm_1".into(),
        session_id: "ses_1".into(),
        kind: LaneApprovalKind::Permission,
        status: LaneApprovalStatus::Pending,
        title: "Run `rm -rf build`?".into(),
        detail: Some("in /home/shed/shed".into()),
        options: vec![sample_option()],
        questions: vec![LaneQuestion {
            id: Some("Which directories?".into()),
            header: "Scope".into(),
            question: "Which directories?".into(),
            options: vec![sample_option()],
            multiple: true,
            custom: false,
        }],
        request_json: r#"{"tool":"bash"}"#.into(),
        created_at_unix_ms: Some(1_757_260_800_000),
    }
}

fn sample_message() -> RcFeedMessage {
    RcFeedMessage {
        seq: 3,
        ts: Some("2026-09-07T12:00:00Z".into()),
        role: "assistant".into(),
        msg_type: "text".into(),
        text: Some("on it".into()),
        tool: None,
        approval: None,
    }
}

// ---- round trips ----

#[test]
fn session_round_trips_and_omits_absent_options() {
    let full = round_trip(&sample_session());
    assert_eq!(full["parent_id"], "ses_root");
    assert_eq!(full["activity"], "working");

    let root = LaneSession {
        parent_id: None,
        last_change_unix_ms: None,
        ..sample_session()
    };
    let encoded = round_trip(&root);
    // Absent, never `null` — the crate's Go-`omitempty` posture.
    assert!(encoded.get("parent_id").is_none());
    assert!(encoded.get("last_change_unix_ms").is_none());
}

#[test]
fn history_round_trips_including_the_reused_feed_rows() {
    let h = LaneHistory {
        messages: vec![sample_message()],
        truncated: true,
        cursor: Some("evt_9".into()),
    };
    let encoded = round_trip(&h);
    // The messages are `RcFeedMessage`s verbatim, `type` key and all.
    assert_eq!(encoded["messages"][0]["type"], "text");
    let empty = round_trip(&LaneHistory::default());
    assert_eq!(empty, json!({"messages": [], "truncated": false}));
}

#[test]
fn approval_and_its_parts_round_trip() {
    let encoded = round_trip(&sample_approval());
    assert_eq!(encoded["kind"], "permission");
    assert_eq!(encoded["status"], "pending");
    assert_eq!(encoded["questions"][0]["multiple"], true);
    round_trip(&sample_option());
    round_trip(&LaneApprovalOption {
        id: "Yes".into(),
        label: "Yes".into(),
        description: None,
        kind: None,
    });
    round_trip(&LaneQuestion::default());
}

/// The two fields the second adapter added ([`LaneApprovalOption::kind`] and
/// [`LaneQuestion::id`]) survive a round trip, are OMITTED when absent — the
/// wire an older mirror decodes is byte-unchanged — and carry the ACP
/// vocabulary verbatim when present.
#[test]
fn option_kind_and_question_id_round_trip_and_are_omitted_when_absent() {
    let encoded = round_trip(&sample_option());
    assert_eq!(encoded["id"], "p-1", "the id is opaque and untouched");
    assert_eq!(encoded["kind"], "allow_once");

    // Absent on both: the object is exactly what it was before the fields
    // existed, which is what keeps a lagging shed-mobile decoding.
    let bare = LaneApprovalOption {
        id: "Yes".into(),
        label: "Yes".into(),
        description: None,
        kind: None,
    };
    assert_eq!(
        round_trip(&bare),
        serde_json::json!({"id": "Yes", "label": "Yes"}),
    );
    let q = round_trip(&LaneQuestion::default());
    assert_eq!(
        q,
        serde_json::json!({
            "header": "", "question": "", "options": [],
            "multiple": false, "custom": false,
        }),
    );

    // And absent DECODES, on both, from an object that never had the key.
    let decoded: LaneApprovalOption =
        serde_json::from_value(serde_json::json!({"id": "x", "label": "X"}))
            .expect("an option with no kind decodes");
    assert_eq!(decoded.kind, None);
    let decoded: LaneQuestion =
        serde_json::from_value(serde_json::json!({"header": "h", "question": "q"}))
            .expect("a question with no id decodes");
    assert_eq!(decoded.id, None);

    // The four ACP kinds are the wire spellings the panel and both adapters
    // agree on.
    assert_eq!(
        [
            option_kind::ALLOW_ONCE,
            option_kind::ALLOW_ALWAYS,
            option_kind::REJECT_ONCE,
            option_kind::REJECT_ALWAYS,
        ],
        ["allow_once", "allow_always", "reject_once", "reject_always"],
    );
    // Unrecognized kinds are TOLERATED, not refused: `kind` is a stream value,
    // and a fifth kind from a newer agent must not take its approval with it.
    let decoded: LaneApprovalOption = serde_json::from_value(
        serde_json::json!({"id": "x", "label": "X", "kind": "ask_the_operator"}),
    )
    .expect("an unknown option kind decodes");
    assert_eq!(decoded.kind.as_deref(), Some("ask_the_operator"));
}

/// [`LaneAnswer::Choice`] is tagged like its siblings, round-trips, and is
/// **strict**: an unknown `kind` on an answer is a command this build cannot
/// honor, and refusing it at decode is the module doc's asymmetric rule.
#[test]
fn lane_answer_choice_round_trips_and_stays_strict() {
    let encoded = round_trip(&LaneAnswer::Choice {
        option_id: "p-2".into(),
    });
    assert_eq!(
        encoded,
        serde_json::json!({"kind": "choice", "option_id": "p-2"}),
    );

    // An unknown answer kind is REFUSED (no `#[serde(other)]` here, unlike
    // `LaneEvent`) — including one that looks like a near-miss for this variant.
    for bad in [
        serde_json::json!({"kind": "option", "option_id": "p-2"}),
        serde_json::json!({"kind": "choose", "option_id": "p-2"}),
        serde_json::json!({"kind": "unknown"}),
    ] {
        assert!(
            serde_json::from_value::<LaneAnswer>(bad.clone()).is_err(),
            "{bad} must not decode as a LaneAnswer",
        );
    }
    // And the payload is required: a `choice` with no id is not an answer.
    assert!(serde_json::from_value::<LaneAnswer>(serde_json::json!({"kind": "choice"})).is_err());
}

#[test]
fn capabilities_round_trip_and_pin_opencodes_row() {
    let caps = LaneCapabilities {
        kind: "opencode".into(),
        interject: false,
        create: true,
        cancel: true,
        approvals: true,
        history_cursor: false,
    };
    assert_eq!(
        round_trip(&caps),
        json!({
            "kind": "opencode",
            "interject": false,
            "create": true,
            "cancel": true,
            "approvals": true,
            "history_cursor": false
        })
    );
}

#[test]
fn every_lane_error_round_trips() {
    for e in [
        LaneError::Unauthorized,
        LaneError::BadRequest("no such model".into()),
        LaneError::UnknownSession,
        LaneError::UnknownApproval,
        LaneError::AlreadySubmitted,
        LaneError::AlreadyResolved,
        LaneError::NotAccepting,
        LaneError::Unavailable("dial 127.0.0.1:4096: refused".into()),
        LaneError::Failed("502 from the agent".into()),
    ] {
        round_trip(&e);
    }
    assert_eq!(
        round_trip(&LaneError::BadRequest("nope".into())),
        json!({"bad_request": "nope"})
    );
    assert_eq!(
        round_trip(&LaneError::UnknownSession),
        json!("unknown_session")
    );
    // thiserror's Display keeps the agent's own message whole.
    assert_eq!(LaneError::Failed("502".into()).to_string(), "502");
    assert_eq!(LaneError::UnknownApproval.to_string(), "no such approval");
}

// ---- the unknown-value policy ----

#[test]
fn approval_kind_other_preserves_the_raw_string() {
    let raw = "some_future_kind";
    let k = LaneApprovalKind::from_wire(raw);
    assert_eq!(k, LaneApprovalKind::Other(raw.to_string()));
    assert!(!k.is_known());
    assert_eq!(k.as_str(), raw);
    // Verbatim on the way out, and a full round trip.
    assert_eq!(round_trip(&k), json!(raw));
    // Inside an approval, too — an unknown kind must not swallow the row.
    let approval = LaneApproval {
        kind: k,
        ..sample_approval()
    };
    let encoded = round_trip(&approval);
    assert_eq!(encoded["kind"], raw);
}

#[test]
fn known_approval_kinds_are_snake_case_strings() {
    for (kind, wire) in [
        (LaneApprovalKind::Permission, "permission"),
        (LaneApprovalKind::Question, "question"),
        (LaneApprovalKind::PlanApproval, "plan_approval"),
        (LaneApprovalKind::McpElicitation, "mcp_elicitation"),
    ] {
        assert_eq!(round_trip(&kind), json!(wire));
        assert_eq!(LaneApprovalKind::from_wire(wire), kind);
        assert!(kind.is_known());
    }
}

#[test]
fn known_approval_statuses_are_snake_case_strings() {
    for (status, wire) in [
        (LaneApprovalStatus::Pending, "pending"),
        (LaneApprovalStatus::Submitted, "submitted"),
        (LaneApprovalStatus::Resolved, "resolved"),
    ] {
        assert_eq!(round_trip(&status), json!(wire));
        assert_eq!(LaneApprovalStatus::from_wire(wire), status);
        assert!(status.is_known());
        assert_eq!(status.is_pending(), status == LaneApprovalStatus::Pending);
    }
}

#[test]
fn plain_enums_are_snake_case_strings() {
    for (v, wire) in [
        (LaneDecision::AllowOnce, "allow_once"),
        (LaneDecision::AllowAlways, "allow_always"),
        (LaneDecision::Reject, "reject"),
    ] {
        assert_eq!(round_trip(&v), json!(wire));
    }
    for (v, wire) in [
        (SendMode::Queue, "queue"),
        (SendMode::Interject, "interject"),
    ] {
        assert_eq!(round_trip(&v), json!(wire));
    }
}

// ---- the tagged representations ----

#[test]
fn lane_answer_is_tagged_on_kind_in_snake_case() {
    assert_eq!(
        round_trip(&LaneAnswer::Permission {
            decision: LaneDecision::AllowAlways
        }),
        json!({"kind": "permission", "decision": "allow_always"})
    );
    assert_eq!(
        round_trip(&LaneAnswer::Question {
            answers: vec![vec!["a".into(), "b".into()], vec!["c".into()]]
        }),
        json!({"kind": "question", "answers": [["a", "b"], ["c"]]})
    );
    assert_eq!(round_trip(&LaneAnswer::Reject), json!({"kind": "reject"}));
    assert_eq!(
        round_trip(&LaneAnswer::Raw {
            json: r#"{"x":1}"#.into()
        }),
        json!({"kind": "raw", "json": r#"{"x":1}"#})
    );
}

#[test]
fn lane_event_is_tagged_on_kind_in_snake_case() {
    let encoded = round_trip(&LaneEvent::Message {
        message: sample_message(),
        cursor: Some("evt_9".into()),
    });
    assert_eq!(
        encoded,
        json!({
            "kind": "message",
            "message": {
                "seq": 3,
                "ts": "2026-09-07T12:00:00Z",
                "role": "assistant",
                "type": "text",
                "text": "on it"
            },
            "cursor": "evt_9"
        })
    );
    // No cursor → the key is absent, not `null`.
    let encoded = round_trip(&LaneEvent::Message {
        message: sample_message(),
        cursor: None,
    });
    assert!(encoded.get("cursor").is_none());

    // Payloads nest under a named key — see the type's doc: flattening
    // `LaneApproval` would collide its own `kind` field with the tag.
    let encoded = round_trip(&LaneEvent::Session {
        session: sample_session(),
    });
    assert_eq!(encoded["kind"], "session");
    assert_eq!(encoded["session"]["id"], "ses_1");
    let encoded = round_trip(&LaneEvent::Approval {
        approval: sample_approval(),
    });
    assert_eq!(encoded["kind"], "approval");
    assert_eq!(encoded["approval"]["id"], "perm_1");
    // The approval's own `kind` survives the tag intact.
    assert_eq!(encoded["approval"]["kind"], "permission");

    assert_eq!(
        round_trip(&LaneEvent::Reset {
            reason: "reconnect".into(),
            generation: 2
        }),
        json!({"kind": "reset", "reason": "reconnect", "generation": 2})
    );
    assert_eq!(
        round_trip(&LaneEvent::Ready { generation: 2 }),
        json!({"kind": "ready", "generation": 2})
    );
    assert_eq!(
        round_trip(&LaneEvent::Down {
            reason: "opencode exited".into()
        }),
        json!({"kind": "down", "reason": "opencode exited"})
    );
}

// ---- the subscription handle ----

/// A short bound for every `recv()` in this file.
///
/// These assertions are all "the channel closed because the pump was aborted". If
/// the abort ever regresses, the pump keeps its sender alive forever and a bare
/// `rx.recv().await` BLOCKS — the test hangs until CI's global timeout kills the
/// run, which reads as an infrastructure flake instead of the contract break it
/// is. Every wait is wrapped so a regression fails, readably, in milliseconds.
const RECV_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(2);

/// [`LaneStop`] is the `RoostWatcher` contract: `stop()` aborts the pump, and so
/// does `Drop` — a client that simply drops the subscription must not leave a
/// task holding a transport open.
#[tokio::test]
async fn lane_stop_aborts_the_pump_on_stop_and_on_drop() {
    let (tx, rx) = LanePublisher::channel();
    let task = tokio::spawn(async move {
        let _held = tx;
        std::future::pending::<()>().await;
    });
    let sub = LaneSubscription {
        rx,
        stop: LaneStop::new(task),
    };
    sub.stop.stop();
    // The pump held the sender; aborting it drops the sender, so the receiver
    // observes the close.
    let mut rx = sub.rx;
    let closed = tokio::time::timeout(RECV_TIMEOUT, rx.recv())
        .await
        .expect("stop() must abort the pump, closing the channel — it did not, so recv() hung");
    assert!(closed.is_none(), "an aborted pump yields a closed channel");

    // And again with no explicit stop: `Drop` does it.
    let (tx, mut rx) = LanePublisher::channel();
    let task = tokio::spawn(async move {
        let _held = tx;
        std::future::pending::<()>().await;
    });
    drop(LaneStop::new(task));
    let closed = tokio::time::timeout(RECV_TIMEOUT, rx.recv())
        .await
        .expect("dropping LaneStop must abort the pump — it did not, so recv() hung");
    assert!(closed.is_none(), "a dropped LaneStop closes the channel");
}

/// The partial-move trap [`LaneSubscription`]'s doc warns about, pinned as
/// executable behavior: a helper that hands back only `sub.rx` drops the `stop`
/// half on the way out, which aborts the pump, so the receiver its caller holds is
/// DEAD — silently, with no `Down` frame and no error.
///
/// This is exactly `Ok(lane.subscribe(id, None).await?.rx)`. It is here so the
/// failure is documented and caught, rather than discovered in a client rendering
/// an empty transcript.
#[tokio::test]
async fn a_helper_returning_only_the_receiver_kills_the_pump() {
    /// The broken idiom, in the smallest shape that reproduces it: the
    /// subscription is owned HERE, so the `stop` left behind by the partial move
    /// drops when this returns.
    fn broken_open(sub: LaneSubscription) -> mpsc::Receiver<LaneEvent> {
        sub.rx
    }

    let (tx, rx) = LanePublisher::channel();
    let task = tokio::spawn(async move {
        // A pump that would keep sending forever, if it were allowed to live.
        let mut generation = 0u64;
        loop {
            generation += 1;
            if tx.publish(LaneEvent::Ready { generation }) == Publish::Closed {
                return;
            }
            tokio::task::yield_now().await;
        }
    });
    let mut rx = broken_open(LaneSubscription {
        rx,
        stop: LaneStop::new(task),
    });

    // Whatever the pump managed to queue before the abort may still drain, but the
    // channel closes and never reopens — a subscription that is over before its
    // owner read a frame.
    let drained =
        tokio::time::timeout(RECV_TIMEOUT, async { while rx.recv().await.is_some() {} }).await;
    assert!(
        drained.is_ok(),
        "the abandoned LaneStop must abort the pump; if this hangs, the partial \
         move stopped being a hazard and LaneSubscription's doc needs revisiting"
    );
}

/// The safe idiom, pinned: [`LaneSubscription::into_parts`] with BOTH halves bound
/// keeps the pump alive and the frames flowing.
#[tokio::test]
async fn into_parts_keeps_frames_flowing_while_both_halves_live() {
    let (tx, rx) = LanePublisher::channel();
    let task = tokio::spawn(async move {
        for generation in 1..=3u64 {
            if tx.publish(LaneEvent::Ready { generation }) == Publish::Closed {
                return;
            }
            tokio::task::yield_now().await;
        }
        std::future::pending::<()>().await;
    });
    let sub = LaneSubscription {
        rx,
        stop: LaneStop::new(task),
    };

    let (mut rx, stop) = sub.into_parts();
    for expected in 1..=3u64 {
        let frame = tokio::time::timeout(RECV_TIMEOUT, rx.recv())
            .await
            .expect("a live subscription must keep delivering frames")
            .expect("the channel must stay open while the stop handle is held");
        assert_eq!(
            frame,
            LaneEvent::Ready {
                generation: expected
            }
        );
    }

    // And the handle still ends it.
    drop(stop);
    let closed = tokio::time::timeout(RECV_TIMEOUT, rx.recv())
        .await
        .expect("dropping the stop half must still abort the pump");
    assert!(closed.is_none());
}

// ---- the bounded channel (module doc, correction 13) ----

/// [`LanePublisher::publish`]'s three outcomes, and the one that matters: a full
/// queue DROPS the frame rather than queueing it behind, which is why the caller
/// has to end its generation instead of carrying on.
#[tokio::test]
async fn publish_answers_sent_until_it_is_full_then_lagged() {
    let (tx, mut rx) = LanePublisher::channel();
    for generation in 0..LANE_CHANNEL_CAPACITY as u64 {
        assert_eq!(
            tx.publish(LaneEvent::Ready { generation }),
            Publish::Sent,
            "frame {generation} is inside LANE_CHANNEL_CAPACITY and must fit"
        );
    }
    assert_eq!(
        tx.publish(LaneEvent::Ready { generation: 9_999 }),
        Publish::Lagged,
        "the frame past the bound is refused, not queued"
    );

    // And it is GONE — the queue holds exactly what fit, in order.
    for generation in 0..LANE_CHANNEL_CAPACITY as u64 {
        assert_eq!(rx.recv().await, Some(LaneEvent::Ready { generation }));
    }
    assert!(
        rx.try_recv().is_err(),
        "the lagged frame was dropped; nothing may arrive behind the bound"
    );
}

/// The terminal `Down` is the one frame that is never dropped:
/// [`LanePublisher::publish_final`] WAITS for room and lands after everything
/// the client had queued.
#[tokio::test]
async fn publish_final_waits_for_room_and_the_down_still_lands() {
    let (tx, mut rx) = LanePublisher::channel();
    for generation in 0..LANE_CHANNEL_CAPACITY as u64 {
        assert_eq!(tx.publish(LaneEvent::Ready { generation }), Publish::Sent);
    }
    let sending = tokio::spawn(async move {
        tx.publish_final(LaneEvent::Down {
            reason: "lagged-then-gone".to_string(),
        })
        .await;
    });
    // Nothing can move while the queue is full, and `publish_final` must be
    // waiting rather than having quietly dropped the frame.
    tokio::task::yield_now().await;
    assert!(
        !sending.is_finished(),
        "publish_final must await room for the Down, not drop it"
    );

    let mut frames = 0usize;
    let mut last = None;
    while let Some(ev) = tokio::time::timeout(RECV_TIMEOUT, rx.recv())
        .await
        .expect("the Down must arrive once the queue drains")
    {
        frames += 1;
        last = Some(ev);
    }
    assert_eq!(
        frames,
        LANE_CHANNEL_CAPACITY + 1,
        "everything, plus the Down"
    );
    assert_eq!(
        last,
        Some(LaneEvent::Down {
            reason: "lagged-then-gone".to_string()
        }),
        "the Down is the last thing a subscription says"
    );
    sending.await.expect("the sender task finishes");
}

/// [`LanePublisher::wait_drained`] is "the client has caught up ENTIRELY", not
/// "there is room again". A reseed republishes a whole seed, so starting one
/// against a merely-not-full queue would lag again a few frames in.
#[tokio::test]
async fn wait_drained_resolves_only_when_every_slot_is_free() {
    let (tx, mut rx) = LanePublisher::channel();
    for generation in 0..3u64 {
        assert_eq!(tx.publish(LaneEvent::Ready { generation }), Publish::Sent);
    }
    rx.recv().await.expect("the first frame");
    assert!(
        tokio::time::timeout(std::time::Duration::from_millis(50), tx.wait_drained())
            .await
            .is_err(),
        "two frames are still queued: wait_drained must not resolve"
    );

    rx.recv().await.expect("the second frame");
    rx.recv().await.expect("the third frame");
    tokio::time::timeout(RECV_TIMEOUT, tx.wait_drained())
        .await
        .expect("an empty queue must resolve wait_drained")
        .expect("the receiver is still alive");
}

/// A subscriber that went away ends BOTH awaits — the adapter then stops
/// silently, with no `Down` and no reseed, exactly as its `is_closed` checks
/// already do.
#[tokio::test]
async fn a_dropped_receiver_ends_publish_and_both_awaits() {
    let (tx, rx) = LanePublisher::channel();
    assert_eq!(
        tx.publish(LaneEvent::Ready { generation: 1 }),
        Publish::Sent
    );
    drop(rx);

    assert!(tx.is_closed());
    assert_eq!(
        tx.publish(LaneEvent::Ready { generation: 2 }),
        Publish::Closed,
        "a closed channel is Closed, never Lagged — the two mean different things"
    );
    assert_eq!(
        tokio::time::timeout(RECV_TIMEOUT, tx.wait_drained())
            .await
            .expect("wait_drained must not hang on a closed channel"),
        Err(Closed),
    );
    tokio::time::timeout(
        RECV_TIMEOUT,
        tx.publish_final(LaneEvent::Down {
            reason: "nobody is listening".to_string(),
        }),
    )
    .await
    .expect("publish_final must return on a closed channel rather than hang");
}

// ---- the trait ----

/// The trait is object-safe and its futures are `Send`: a client holds one
/// adapter behind `Arc<dyn AgentLane>` and drives it from any task, which is the
/// whole point of the `&self` signatures.
#[test]
fn agent_lane_is_object_safe() {
    struct Nothing;

    #[async_trait::async_trait]
    impl AgentLane for Nothing {
        fn capabilities(&self) -> LaneCapabilities {
            LaneCapabilities {
                kind: "nothing".into(),
                interject: false,
                create: false,
                cancel: false,
                approvals: false,
                history_cursor: false,
            }
        }
        async fn sessions(&self) -> Result<Vec<LaneSession>, LaneError> {
            Ok(Vec::new())
        }
        async fn session(&self, _id: &str) -> Result<LaneSession, LaneError> {
            Err(LaneError::UnknownSession)
        }
        async fn history(
            &self,
            _id: &str,
            _cursor: Option<&str>,
            _limit: u32,
        ) -> Result<LaneHistory, LaneError> {
            Err(LaneError::UnknownSession)
        }
        async fn create(&self, _cwd: &str, _text: &str) -> Result<LaneSession, LaneError> {
            Err(LaneError::NotAccepting)
        }
        async fn send(&self, _id: &str, _text: &str, _mode: SendMode) -> Result<(), LaneError> {
            Err(LaneError::NotAccepting)
        }
        async fn cancel(&self, _id: &str) -> Result<(), LaneError> {
            Err(LaneError::NotAccepting)
        }
        async fn approvals(&self, _id: &str) -> Result<Vec<LaneApproval>, LaneError> {
            Ok(Vec::new())
        }
        async fn answer(
            &self,
            _id: &str,
            _approval_id: &str,
            _answer: LaneAnswer,
        ) -> Result<(), LaneError> {
            Err(LaneError::UnknownApproval)
        }
        async fn subscribe(
            &self,
            _id: &str,
            _cursor: Option<String>,
        ) -> Result<LaneSubscription, LaneError> {
            Err(LaneError::Unavailable("nothing to dial".into()))
        }
    }

    let lane: std::sync::Arc<dyn AgentLane> = std::sync::Arc::new(Nothing);
    assert_eq!(lane.capabilities().kind, "nothing");
    fn assert_send<T: Send>(_: &T) {}
    assert_send(&lane.sessions());
}

/// A `LaneQuestion` whose optional booleans are absent must decode — opencode
/// marks both `multiple?` and `custom?` optional, and a producer one version
/// behind may omit them. Without `#[serde(default)]` serde fails the field, which
/// fails the whole enclosing `LaneEvent`, so a pending approval would silently
/// vanish from the panel instead of rendering with the conservative default.
#[test]
fn question_decodes_without_its_optional_booleans() {
    let q: LaneQuestion = serde_json::from_value(json!({
        "header": "Scope",
        "question": "Which directories?",
        "options": [],
    }))
    .expect("a question without `multiple`/`custom` must decode");
    assert!(!q.multiple);
    assert!(!q.custom);

    // The real failure mode: nested inside a tagged approval event.
    let ev: LaneEvent = serde_json::from_value(json!({
        "kind": "approval",
        "approval": {
            "id": "req-1",
            "session_id": "ses-1",
            "kind": "question",
            "status": "pending",
            "title": "Pick a directory",
            "options": [],
            "questions": [{"header": "Scope", "question": "Which?", "options": []}],
            "request_json": "{}",
        },
    }))
    .expect("an approval event carrying such a question must decode");
    let LaneEvent::Approval { approval } = ev else {
        panic!("expected an approval event");
    };
    assert_eq!(approval.questions.len(), 1);
    assert!(!approval.questions[0].custom);
}

// ---- the asymmetry: tolerant inbound, strict outbound ----
//
// Every other decode test in this file feeds WELL-FORMED input, which is exactly
// how a tolerance gap hides: the types that must survive a newer producer are
// never handed anything a newer producer would send. These are the negative
// cases — one per tolerant type, plus the deliberate refusals that pin the other
// half of the rule.

/// An approval status this build has never heard of must decode to
/// [`LaneApprovalStatus::Other`] with the raw preserved — and must NOT read as
/// pending, or a client offers answer buttons for a `cancelled` approval and
/// posts an answer nobody is listening for.
#[test]
fn an_unknown_approval_status_decodes_to_other_and_is_not_pending() {
    let s: LaneApprovalStatus =
        serde_json::from_value(json!("cancelled")).expect("an unknown status must decode");
    assert_eq!(s, LaneApprovalStatus::Other("cancelled".into()));
    assert_eq!(s.as_str(), "cancelled");
    assert!(!s.is_known());
    assert!(!s.is_pending(), "an unknown status is not actionable");
    // Preserved verbatim on the way back out.
    assert_eq!(round_trip(&s), json!("cancelled"));

    // The real failure mode this guards: nested in an approval frame. Strict, the
    // unknown status fails the WHOLE `LaneEvent` and the approval vanishes from
    // the panel while the session still reads as blocked.
    let ev: LaneEvent = serde_json::from_value(json!({
        "kind": "approval",
        "approval": {
            "id": "perm_1",
            "session_id": "ses_1",
            "kind": "permission",
            "status": "cancelled",
            "title": "Run `rm -rf build`?",
            "options": [],
            "questions": [],
            "request_json": "{}",
        },
    }))
    .expect("an approval carrying an unknown status must not fail the whole event");
    let LaneEvent::Approval { approval } = ev else {
        panic!("expected an approval event");
    };
    assert_eq!(
        approval.status,
        LaneApprovalStatus::Other("cancelled".into())
    );
    assert!(!approval.status.is_pending());
}

/// An event kind this build has never heard of decodes to [`LaneEvent::Unknown`]
/// rather than failing the read loop and taking the subscription with it.
#[test]
fn an_unknown_event_kind_decodes_to_unknown() {
    let ev: LaneEvent = serde_json::from_value(json!({"kind": "heartbeat"}))
        .expect("an unknown event kind must decode");
    assert_eq!(ev, LaneEvent::Unknown);

    // Including one that carries a body this build cannot name: the payload is
    // dropped (a unit variant is all `#[serde(other)]` allows), the frame is not.
    let ev: LaneEvent = serde_json::from_value(json!({
        "kind": "cost",
        "input_tokens": 12,
        "usd": 0.004,
    }))
    .expect("an unknown event kind with a payload must still decode");
    assert_eq!(ev, LaneEvent::Unknown);

    // It serializes to a nameable frame, so a relay that re-encodes degrades
    // rather than corrupts.
    assert_eq!(round_trip(&LaneEvent::Unknown), json!({"kind": "unknown"}));
}

/// The other half of the rule, pinned so nobody "helpfully" makes the command
/// types tolerant too: a [`SendMode`], a [`LaneDecision`] or a [`LaneAnswer`] the
/// build cannot name is an ERROR. Coercing one would deliver a message with
/// semantics the caller did not ask for, or turn a user's "allow" into a no-op.
#[test]
fn unknown_command_values_are_rejected() {
    serde_json::from_value::<SendMode>(json!("interject_hard"))
        .expect_err("an unknown send mode must be rejected, not coerced");
    serde_json::from_value::<LaneDecision>(json!("allow_forever"))
        .expect_err("an unknown decision must be rejected, not coerced");
    serde_json::from_value::<LaneAnswer>(json!({"kind": "elicit", "value": "x"}))
        .expect_err("an unknown answer kind must be rejected, not degraded");
    // And a decision nested in an answer fails the answer, which is the point:
    // a half-understood command must never reach an adapter.
    serde_json::from_value::<LaneAnswer>(json!({"kind": "permission", "decision": "maybe"}))
        .expect_err("an unknown decision must fail its enclosing answer");
    // `LaneError` stays strict too — a caller branches on these.
    serde_json::from_value::<LaneError>(json!("rate_limited"))
        .expect_err("an unknown error code must be rejected, not filed under a live variant");
}

// ---- the by-kind permission resolver ----

/// An option whose id is deliberately UNRELATED to its kind.
///
/// Every case below uses ids an id-matching implementation could not resolve
/// from (`p-1`…`p-4`, `x`, `"7"`), so a regression that reads `option.id` and
/// compares it against the decision's wire spelling fails here rather than
/// passing by coincidence — which is exactly what it would do against
/// opencode's real ids.
fn opt(id: &str, kind: Option<&str>) -> LaneApprovalOption {
    LaneApprovalOption {
        id: id.into(),
        label: format!("label for {id}"),
        description: None,
        kind: kind.map(str::to_string),
    }
}

fn approval_offering(options: Vec<LaneApprovalOption>) -> LaneApproval {
    LaneApproval {
        options,
        ..sample_approval()
    }
}

/// The table the whole correction rests on: a decision picks an option by
/// KIND, in offered order, and never by id.
#[test]
fn option_for_resolves_every_decision_by_kind_never_by_id() {
    // A full four-option set, ids scrambled against their kinds.
    let full = approval_offering(vec![
        opt("p-1", Some(option_kind::ALLOW_ONCE)),
        opt("p-2", Some(option_kind::ALLOW_ALWAYS)),
        opt("p-3", Some(option_kind::REJECT_ONCE)),
        opt("p-4", Some(option_kind::REJECT_ALWAYS)),
    ]);
    // `Reject`-with-both-present resolves to the EXACT `reject_once`, not to the
    // fallback's first-reject — the exact match is tried first.
    let cases = [
        (LaneDecision::AllowOnce, Some("p-1")),
        (LaneDecision::AllowAlways, Some("p-2")),
        (LaneDecision::Reject, Some("p-3")),
    ];
    for (decision, want) in cases {
        assert_eq!(
            full.option_for(decision).map(|o| o.id.as_str()),
            want,
            "{decision:?} on the full set",
        );
    }

    // The `Reject` FALLBACK: no `reject_once` offered, only `reject_always`.
    // An agent that offers one flavour of refusal must still be refusable.
    let only_always = approval_offering(vec![
        opt("x", Some(option_kind::ALLOW_ONCE)),
        opt("7", Some(option_kind::REJECT_ALWAYS)),
    ]);
    assert_eq!(
        only_always
            .option_for(LaneDecision::Reject)
            .map(|o| o.id.as_str()),
        Some("7"),
    );

    // The fallback takes the FIRST reject-kind in OFFERED order. Two reject
    // kinds, neither an exact `reject_once`, so ordering is what decides — and
    // the answer is the earlier one, not the last and not the alphabetically
    // smaller (`reject_always` < `reject_something_else`, so both orderings
    // would agree; the pair below is chosen so they do NOT).
    let fallback_order = approval_offering(vec![
        opt("p-9", Some("reject_something_else")),
        opt("p-4", Some(option_kind::REJECT_ALWAYS)),
    ]);
    assert_eq!(
        fallback_order
            .option_for(LaneDecision::Reject)
            .map(|o| o.id.as_str()),
        Some("p-9"),
        "first in offered order, not last and not alphabetical",
    );

    // Both reject kinds present, offered in REVERSE (`reject_always` first):
    // the EXACT `reject_once` still wins, because step 1 runs before the
    // fallback. Offered order only breaks ties the fallback has to break.
    let reversed = approval_offering(vec![
        opt("p-4", Some(option_kind::REJECT_ALWAYS)),
        opt("p-3", Some(option_kind::REJECT_ONCE)),
    ]);
    assert_eq!(
        reversed
            .option_for(LaneDecision::Reject)
            .map(|o| o.id.as_str()),
        Some("p-3"),
        "an exact reject_once beats an earlier-offered reject_always",
    );

    // There is NO fallback for the allow decisions, deliberately: broadening a
    // refusal is safe, broadening an ALLOW is the bug this contract prevents.
    // `allow_always` offered, `allow_once` asked for → no match.
    let only_allow_always = approval_offering(vec![
        opt("p-2", Some(option_kind::ALLOW_ALWAYS)),
        opt("p-3", Some(option_kind::REJECT_ONCE)),
    ]);
    assert_eq!(only_allow_always.option_for(LaneDecision::AllowOnce), None);
    // …and the converse.
    let only_allow_once = approval_offering(vec![opt("p-1", Some(option_kind::ALLOW_ONCE))]);
    assert_eq!(only_allow_once.option_for(LaneDecision::AllowAlways), None);

    // An option with NO kind never matches — not even one whose ID spells the
    // decision. This is the id-sniffing regression, caught.
    let kindless = approval_offering(vec![
        opt("allow_once", None),
        opt("allow_always", None),
        opt("reject", None),
    ]);
    for decision in [
        LaneDecision::AllowOnce,
        LaneDecision::AllowAlways,
        LaneDecision::Reject,
    ] {
        assert_eq!(
            kindless.option_for(decision),
            None,
            "{decision:?} must not match an id",
        );
    }

    // No options at all: `None`, not a panic.
    let empty = approval_offering(vec![]);
    assert_eq!(empty.option_for(LaneDecision::Reject), None);

    // An unrecognized kind is inert for the allow decisions and, because it does
    // not start with `reject`, for the fallback too.
    let alien = approval_offering(vec![opt("p-5", Some("ask_the_operator"))]);
    for decision in [
        LaneDecision::AllowOnce,
        LaneDecision::AllowAlways,
        LaneDecision::Reject,
    ] {
        assert_eq!(
            alien.option_for(decision),
            None,
            "{decision:?} on an alien kind"
        );
    }
}

/// gx's REAL five, and the escalation the by-kind rule used to pick.
///
/// Recorded off a live gx leader: one `session/request_permission` for
/// `id -un`. Two of the five options declare `allow_once` — the ordinary "Yes,
/// proceed" AND an "always-approve mode" switch that stops the agent asking
/// about ANYTHING for the rest of the session — and the escalating one is
/// offered FIRST.
///
/// Under the original rule ("the first option in offered order whose kind
/// matches"), [`LaneDecision::AllowOnce`] resolved to `enable-always-approve`:
/// a human tapping "Allow once" would silently have disabled permission
/// prompting. That is the exact failure the by-kind design exists to prevent,
/// and only a real agent exposes it — the plan assumed `kind` disambiguates,
/// and against gx it does not.
///
/// So the rule is now "exactly one, or nothing". This test fails on the gx case
/// under the old first-match implementation, which is what makes it
/// load-bearing rather than decorative.
///
/// The option set is gx's own generic vocabulary — no user data, no paths, no
/// session content — which is why it is safe to pin here verbatim.
#[test]
fn real_gx_offers_two_allow_once_options_so_a_decision_cannot_choose() {
    let gx = approval_offering(vec![
        // "Yes, and don't ask again for anything (always-approve mode)"
        opt("enable-always-approve", Some(option_kind::ALLOW_ONCE)),
        // "Always allow: id -un"
        opt("allow-always-command", Some(option_kind::ALLOW_ALWAYS)),
        // "Yes, proceed"
        opt("allow-once", Some(option_kind::ALLOW_ONCE)),
        // "No, and tell Grok what to do differently"
        opt("reject-once", Some(option_kind::REJECT_ONCE)),
        // "Never allow: id -un"
        opt("reject-always-command", Some(option_kind::REJECT_ALWAYS)),
    ]);

    assert_eq!(
        gx.option_for(LaneDecision::AllowOnce),
        None,
        "two options declare allow_once — one of them turns prompting OFF for \
         the session — so the decision alone cannot say which the human meant, \
         and picking either would be a guess at a privilege escalation",
    );
    // The unambiguous kinds on the SAME set still resolve: refusing is scoped
    // to the ambiguity, not to the approval.
    assert_eq!(
        gx.option_for(LaneDecision::AllowAlways)
            .map(|o| o.id.as_str()),
        Some("allow-always-command"),
    );
    assert_eq!(
        gx.option_for(LaneDecision::Reject).map(|o| o.id.as_str()),
        Some("reject-once"),
    );
}

/// The ambiguity rule, over every decision — and the fallback's boundary.
#[test]
fn option_for_refuses_an_ambiguous_kind_and_still_resolves_a_unique_one() {
    // One of a kind still resolves: opencode's whole set is like this, and it
    // must keep working.
    let unique = approval_offering(vec![
        opt("a", Some(option_kind::ALLOW_ONCE)),
        opt("b", Some(option_kind::ALLOW_ALWAYS)),
        opt("c", Some(option_kind::REJECT_ONCE)),
    ]);
    for (decision, want) in [
        (LaneDecision::AllowOnce, "a"),
        (LaneDecision::AllowAlways, "b"),
        (LaneDecision::Reject, "c"),
    ] {
        assert_eq!(
            unique.option_for(decision).map(|o| o.id.as_str()),
            Some(want),
            "{decision:?}",
        );
    }

    // Two of a kind refuses — for EVERY decision, allow and reject alike. Two
    // `reject_once` options can differ materially ("refuse" vs "refuse and tell
    // the agent why"), so a refusal is not automatically interchangeable
    // either.
    for (kind, decision) in [
        (option_kind::ALLOW_ONCE, LaneDecision::AllowOnce),
        (option_kind::ALLOW_ALWAYS, LaneDecision::AllowAlways),
        (option_kind::REJECT_ONCE, LaneDecision::Reject),
    ] {
        let ambiguous =
            approval_offering(vec![opt("first", Some(kind)), opt("second", Some(kind))]);
        assert_eq!(
            ambiguous.option_for(decision),
            None,
            "two options carry {kind}; {decision:?} must refuse rather than \
             break the tie by order",
        );
    }

    // The `Reject` fallback survives, and its boundary is "no `reject_once` AT
    // ALL": with none offered, the first reject-kind still answers, so an agent
    // offering only `reject_always` stays refusable.
    let no_reject_once = approval_offering(vec![
        opt("a", Some(option_kind::ALLOW_ONCE)),
        opt("far", Some(option_kind::REJECT_ALWAYS)),
    ]);
    assert_eq!(
        no_reject_once
            .option_for(LaneDecision::Reject)
            .map(|o| o.id.as_str()),
        Some("far"),
    );
    // …and it may still break a tie by offered order, because every candidate
    // it can reach is a refusal. Broadening a refusal is safe; that asymmetry
    // is the whole reason the allow decisions have no fallback.
    let two_always = approval_offering(vec![
        opt("far-1", Some(option_kind::REJECT_ALWAYS)),
        opt("far-2", Some(option_kind::REJECT_ALWAYS)),
    ]);
    assert_eq!(
        two_always
            .option_for(LaneDecision::Reject)
            .map(|o| o.id.as_str()),
        Some("far-1"),
    );
    // But an ambiguous EXACT `reject_once` short-circuits before the fallback:
    // the fallback is for "none offered", not for "too many offered".
    let two_once_one_always = approval_offering(vec![
        opt("once-1", Some(option_kind::REJECT_ONCE)),
        opt("once-2", Some(option_kind::REJECT_ONCE)),
        opt("far", Some(option_kind::REJECT_ALWAYS)),
    ]);
    assert_eq!(two_once_one_always.option_for(LaneDecision::Reject), None);
}

/// opencode's REAL three, resolved through the shared rule.
///
/// This is the case that would silently pass under an id-matching
/// implementation for two of the three decisions and fail for the third — its
/// reject option's id is `reject` while its kind is `reject_once`. Pinning it
/// here means the contract's resolver is proven against a live adapter's actual
/// option set, not only against synthetic ids.
#[test]
fn option_for_resolves_opencodes_real_three() {
    let oc = approval_offering(vec![
        opt("allow_once", Some(option_kind::ALLOW_ONCE)),
        opt("allow_always", Some(option_kind::ALLOW_ALWAYS)),
        opt("reject", Some(option_kind::REJECT_ONCE)),
    ]);
    assert_eq!(
        oc.option_for(LaneDecision::AllowOnce)
            .map(|o| o.id.as_str()),
        Some("allow_once"),
    );
    assert_eq!(
        oc.option_for(LaneDecision::AllowAlways)
            .map(|o| o.id.as_str()),
        Some("allow_always"),
    );
    // id `reject`, kind `reject_once` — the two are not the same string, and the
    // resolver reads the kind.
    let rejected = oc
        .option_for(LaneDecision::Reject)
        .expect("a reject option");
    assert_eq!(rejected.id, "reject");
    assert_eq!(rejected.kind.as_deref(), Some(option_kind::REJECT_ONCE));
}

/// [`LaneAnswer`] enforces the strictness its doc claims: unknown FIELDS inside
/// a variant are refused, not silently dropped.
///
/// The bug this pins: `{"kind":"permission","decision":"allow_always",
/// "option_id":"reject"}` used to decode as `Permission{AllowAlways}` with the
/// `option_id` thrown away — a payload that reads as "refuse" to a human and
/// executes as "allow always". It is not hypothetical: shed-mobile
/// hand-mirrors this enum, and a Dart encoder that emits both fields (a
/// half-finished migration from one answer shape to the other) would get
/// permission semantics and no error to tell it otherwise.
///
/// `deny_unknown_fields` on an INTERNALLY-tagged enum is the subtle part — the
/// tag key must still be accepted while every other unknown key is refused —
/// so both halves are asserted here rather than assumed.
#[test]
fn lane_answer_refuses_unknown_fields_inside_a_variant() {
    // The ambiguous payload: two answers in one object. Refused.
    let ambiguous = json!({
        "kind": "permission",
        "decision": "allow_always",
        "option_id": "reject",
    });
    assert!(
        serde_json::from_value::<LaneAnswer>(ambiguous).is_err(),
        "a permission carrying an option_id must not decode as a bare permission",
    );

    // …and the mirror image, so neither variant absorbs the other's field.
    assert!(serde_json::from_value::<LaneAnswer>(json!({
        "kind": "choice",
        "option_id": "p-2",
        "decision": "allow_once",
    }))
    .is_err());

    // A junk field on any STRUCT variant.
    for bad in [
        json!({"kind": "permission", "decision": "allow_once", "nonsense": 1}),
        json!({"kind": "choice", "option_id": "p-1", "nonsense": 1}),
        json!({"kind": "question", "answers": [["a"]], "nonsense": 1}),
        json!({"kind": "raw", "json": "{}", "nonsense": 1}),
    ] {
        assert!(
            serde_json::from_value::<LaneAnswer>(bad.clone()).is_err(),
            "{bad} must be refused",
        );
    }

    // **The one carve-out, pinned because it is a serde limitation and not a
    // choice.** `deny_unknown_fields` binds the STRUCT variants; an internally
    // tagged UNIT variant is deserialized from the tag alone and ignores
    // whatever else the object carries. So this decodes, and there is no
    // attribute that would stop it short of turning `Reject` into a struct
    // variant — which would change the Rust API at every construction site
    // (including the Tauri crate, a separate workspace) for no wire change.
    //
    // It is left alone because the direction is safe: extra keys on a `reject`
    // are ignored in favour of REFUSING, the conservative answer. The dangerous
    // direction — a reject-shaped field silently ignored on an ALLOW — is what
    // the struct-variant cases above now refuse.
    assert_eq!(
        serde_json::from_value::<LaneAnswer>(json!({"kind": "reject", "nonsense": 1})).unwrap(),
        LaneAnswer::Reject,
        "documented: a unit variant ignores extra keys, and refusing is the safe direction",
    );

    // The OTHER half: the `kind` tag itself is not an unknown field, and every
    // legitimate payload still decodes — `deny_unknown_fields` on an internally
    // tagged enum would be useless if it broke these.
    let legit = [
        (
            json!({"kind": "permission", "decision": "allow_once"}),
            LaneAnswer::Permission {
                decision: LaneDecision::AllowOnce,
            },
        ),
        (
            json!({"kind": "choice", "option_id": "p-2"}),
            LaneAnswer::Choice {
                option_id: "p-2".into(),
            },
        ),
        (
            json!({"kind": "question", "answers": [["yes"], ["a", "b"]]}),
            LaneAnswer::Question {
                answers: vec![vec!["yes".into()], vec!["a".into(), "b".into()]],
            },
        ),
        // `answers` is `#[serde(default)]`, so an omitted field is still fine —
        // deny_unknown_fields refuses EXTRA keys, never missing optional ones.
        (
            json!({"kind": "question"}),
            LaneAnswer::Question { answers: vec![] },
        ),
        (json!({"kind": "reject"}), LaneAnswer::Reject),
        (
            json!({"kind": "raw", "json": "{\"a\":1}"}),
            LaneAnswer::Raw {
                json: "{\"a\":1}".into(),
            },
        ),
    ];
    for (payload, want) in legit {
        let got: LaneAnswer = serde_json::from_value(payload.clone())
            .unwrap_or_else(|e| panic!("{payload} must decode: {e}"));
        assert_eq!(got, want, "{payload}");
        // And it still round-trips as itself — the encoder emits the tag, and
        // the stricter decoder accepts what the encoder produced.
        round_trip(&want);
    }

    // An unknown variant tag stays refused too (the strictness that already
    // existed, re-pinned so a future `#[serde(other)]` cannot creep in).
    assert!(serde_json::from_value::<LaneAnswer>(json!({"kind": "nope"})).is_err());
}
