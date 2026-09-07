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

fn sample_option() -> LaneApprovalOption {
    LaneApprovalOption {
        id: "allow-once".into(),
        label: "Allow once".into(),
        description: Some("Just this invocation".into()),
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
    });
    round_trip(&LaneQuestion::default());
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
    let (tx, rx) = mpsc::unbounded_channel::<LaneEvent>();
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
    let (tx, mut rx) = mpsc::unbounded_channel::<LaneEvent>();
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
    fn broken_open(sub: LaneSubscription) -> mpsc::UnboundedReceiver<LaneEvent> {
        sub.rx
    }

    let (tx, rx) = mpsc::unbounded_channel::<LaneEvent>();
    let task = tokio::spawn(async move {
        // A pump that would keep sending forever, if it were allowed to live.
        let mut generation = 0u64;
        loop {
            generation += 1;
            if tx.send(LaneEvent::Ready { generation }).is_err() {
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
    let (tx, rx) = mpsc::unbounded_channel::<LaneEvent>();
    let task = tokio::spawn(async move {
        for generation in 1..=3u64 {
            if tx.send(LaneEvent::Ready { generation }).is_err() {
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
