//! A read-only client for the **RC activity hub** on `127.0.0.1:1029` — served
//! on a machine by the `shed-host-agent` daemon's resident role (plan 010) and
//! in a shed by the guest hub. The wire is identical either way (the rc-parity
//! hub family is the proof), so this client is provider-blind.
//!
//! Graduated out of `crates/sx` in plan 012 (roadmap R4). It lives in shed-core
//! rather than shed-app deliberately: shed-mobile links shed-app with DEFAULT
//! features (no `rc`), and the hub feed is exactly the thing it needs, so gating
//! it behind a feature mobile does not enable would have forced a second Dart
//! implementation of the same wire.
//!
//! Four routes, all read-only (Go `internal/ext/rc/hub.go:298-302`):
//!
//! | route | use |
//! |---|---|
//! | `GET /v1/health` | is anything there, and is it OURS ([`HUB_APP_ID`]) |
//! | `GET /v1/sessions` | the snapshot a watch opens with |
//! | `GET /v1/events` | the SSE stream (`activity.changed`, `session.updated`, `message.appended`) |
//! | `GET /v1/sessions/{slug}/messages?since=N` | the body behind a `message.appended` |
//!
//! It deliberately does NOT go through [`crate::http::Client`]: that is a
//! shed-SERVER protocol client (TLS pinning, control-token FSM, mTLS enrollment)
//! and none of it applies to an unauthenticated loopback daemon.

use std::time::Duration;

use futures_util::StreamExt as _;
use serde_json::Value;

use crate::rc::{RcMessagesPage, RcSessionDto};
use crate::rc_events::{parse_rc_event, RcEvent};
use crate::sse::SseParser;

/// The hub's fixed loopback port (Go `rc.HubAddr`, `hub.go:62`).
///
/// A hub-WIRE fact, not a machine one: the same port is served locally, inside a
/// shed by the guest hub, and on a machine by the `shed-host-agent` daemon's
/// resident role. The wire is identical in all three cases, which is what lets
/// one client read any of them.
///
/// Duplicated as a constant rather than imported because the Go value is not
/// exported to Rust; `tests/rc-parity` pins the pair through the cursor hook
/// script's bytes.
pub const HUB_PORT: u16 = 1029;

/// The identity token `GET /v1/health` returns in `app` (Go `rc.HubAppID`) —
/// proof that whatever holds the port is a hub and not something else that
/// happened to bind it.
pub const HUB_APP_ID: &str = "shed-rc-hub";

const SNAPSHOT_TIMEOUT: Duration = Duration::from_secs(5);

/// A read-only client bound to one hub base URL.
pub struct HubClient {
    base: String,
    http: reqwest::Client,
}

/// What a hub read can go wrong with, split so a caller can tell "nothing is
/// listening" (degrade quietly) from "something answered badly" (say so).
#[derive(Debug)]
pub enum HubError {
    /// Nothing reachable on the port, or it isn't a hub.
    Unavailable(String),
    /// Reached it; the exchange failed.
    Failed(String),
}

impl std::fmt::Display for HubError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            HubError::Unavailable(m) | HubError::Failed(m) => f.write_str(m),
        }
    }
}

impl std::error::Error for HubError {}

impl HubClient {
    /// A client for `127.0.0.1:<port>` — the local hub, or the local end of a
    /// forward to a machine's hub (an `ssh -L` child on desktop, a Dart-side
    /// bridge on mobile; this client cannot tell the difference, which is the
    /// point of the seam).
    pub fn loopback(port: u16) -> Result<Self, HubError> {
        let http = reqwest::Client::builder()
            // The hub is loopback-only and unauthenticated; a proxy env var
            // pointing a 127.0.0.1 request at a corporate proxy would be both
            // wrong and confusing, so no_proxy is explicit (the Go hook script
            // passes `curl --noproxy '*'` for the same reason).
            .no_proxy()
            .build()
            .map_err(|e| HubError::Failed(format!("building the hub client: {e}")))?;
        Ok(Self {
            base: format!("http://127.0.0.1:{port}"),
            http,
        })
    }

    /// Confirm a hub — not merely a listener — is on the port.
    pub async fn health(&self) -> Result<(), HubError> {
        let body: Value = self.get_json("/v1/health").await?;
        match body.get("app").and_then(Value::as_str) {
            Some(HUB_APP_ID) => Ok(()),
            other => Err(HubError::Unavailable(format!(
                "something other than the rc hub is on {} (app={other:?})",
                self.base
            ))),
        }
    }

    /// The session snapshot (`{"sessions":[…]}`).
    ///
    /// This is the ENRICHED view — it overlays the activity dimension the
    /// one-shot `list` never carries — and it is authoritative, which is what
    /// makes a reconnect a full resync with no replay protocol.
    pub async fn sessions(&self) -> Result<Vec<RcSessionDto>, HubError> {
        #[derive(serde::Deserialize)]
        struct Envelope {
            #[serde(default)]
            sessions: Vec<RcSessionDto>,
        }
        let env: Envelope = self.get_json("/v1/sessions").await?;
        Ok(env.sessions)
    }

    /// One page of a session's message feed after the exclusive `since` cursor.
    pub async fn messages(&self, slug: &str, since: u64) -> Result<RcMessagesPage, HubError> {
        let raw: Value = self
            .get_json(&format!("/v1/sessions/{slug}/messages?since={since}"))
            .await?;
        Ok(RcMessagesPage::from_value(&raw))
    }

    /// Stream `GET /v1/events`, forwarding every decoded event onto `tx` until
    /// the stream ends or the receiver is dropped.
    ///
    /// A CHANNEL rather than a callback because the consumer has to `await` (a
    /// `message.appended` is a notification whose body comes from a follow-up
    /// `/messages` fetch), and an async callback inside a byte-stream loop would
    /// stall the reader while it ran. The shed transport uses the same shape for
    /// the same reason, so both feeds render through one consumer.
    ///
    /// **No reconnect and no idle timeout here** — this is ONE connection. The
    /// hub heartbeats every ~25 s with a comment frame, so a silent stream is
    /// not evidence of a dead one. Reconnect belongs to a long-lived client
    /// (`shed_app::machine::MachineHubWatcher`), not to the transport.
    pub async fn events(
        &self,
        tx: &tokio::sync::mpsc::UnboundedSender<RcEvent>,
    ) -> Result<(), HubError> {
        let sink = |event: RcEvent| tx.send(event).is_ok();
        let resp = self
            .http
            .get(format!("{}/v1/events", self.base))
            .header("Accept", "text/event-stream")
            .send()
            .await
            .map_err(|e| HubError::Unavailable(format!("hub events: {e}")))?;
        if !resp.status().is_success() {
            return Err(HubError::Failed(format!(
                "hub events: HTTP {}",
                resp.status()
            )));
        }
        let mut parser = SseParser::new();
        let mut stream = resp.bytes_stream();
        while let Some(chunk) = stream.next().await {
            let chunk = chunk.map_err(|e| HubError::Failed(format!("hub events: {e}")))?;
            for record in parser.feed(&chunk) {
                if let Some(event) = parse_rc_event(&record) {
                    if !sink(event) {
                        return Ok(());
                    }
                }
            }
        }
        for record in parser.finish() {
            if let Some(event) = parse_rc_event(&record) {
                if !sink(event) {
                    break;
                }
            }
        }
        Ok(())
    }

    // -----------------------------------------------------------------
    // The CONTROL verbs (contract v2)
    // -----------------------------------------------------------------
    //
    // These go over the SAME loopback connection the feed does — a client that
    // can read a machine's hub can steer it, with no second transport and no
    // SSH exec. That matters most on a phone, where spawning a process is not
    // an option at all.
    //
    // **Every one of these is capability-gated on the far side**, and a client
    // must gate its UI the same way from `kind_features` rather than from the
    // kind: a `409 not_supported` reaching a user as an error means the button
    // should not have been offered. See `docs/extensions/rc-helper.md`.

    /// Start a turn (`POST /v1/sessions/{slug}/turn`), returning its id.
    ///
    /// Only for a kind whose `input` is `turn`; anything else answers `409
    /// not_supported`, which surfaces here as [`HubError::Failed`].
    pub async fn turn(&self, slug: &str, text: &str) -> Result<String, HubError> {
        #[derive(serde::Deserialize)]
        struct Resp {
            turn_id: String,
        }
        let resp: Resp = self
            .post_json(
                &format!("/v1/sessions/{slug}/turn"),
                &serde_json::json!({ "text": text }),
            )
            .await?;
        Ok(resp.turn_id)
    }

    /// Interrupt the running turn (`POST /v1/sessions/{slug}/interrupt`).
    ///
    /// Returns whether the far side actually began interrupting — `false` is a
    /// legitimate answer (nothing was running), not a failure.
    pub async fn interrupt(&self, slug: &str) -> Result<bool, HubError> {
        #[derive(serde::Deserialize)]
        struct Resp {
            interrupting: bool,
        }
        let resp: Resp = self
            .post_json(
                &format!("/v1/sessions/{slug}/interrupt"),
                &serde_json::json!({}),
            )
            .await?;
        Ok(resp.interrupting)
    }

    /// Send a line of input to a TUI-laned session
    /// (`POST /v1/sessions/{slug}/input`) — the keystroke path, as opposed to
    /// [`turn`](Self::turn)'s structured one.
    pub async fn input(&self, slug: &str, text: &str) -> Result<(), HubError> {
        let _: serde_json::Value = self
            .post_json(
                &format!("/v1/sessions/{slug}/input"),
                &serde_json::json!({ "text": text }),
            )
            .await?;
        Ok(())
    }

    /// Answer a pending approval (`POST /v1/sessions/{slug}/approvals/{id}`).
    ///
    /// `decision` is one of `allow` / `allow_always` / `deny`. Only a kind whose
    /// `approvals` capability is `"remote"` can be answered this way; a `"tui"`
    /// kind reports approvals for INFORMATION only and must be answered in its
    /// terminal — offering a button for one is the mistake this contract exists
    /// to prevent.
    pub async fn approve(&self, slug: &str, id: &str, decision: &str) -> Result<String, HubError> {
        #[derive(serde::Deserialize)]
        struct Resp {
            decision: String,
        }
        let resp: Resp = self
            .post_json(
                &format!("/v1/sessions/{slug}/approvals/{id}"),
                &serde_json::json!({ "decision": decision }),
            )
            .await?;
        Ok(resp.decision)
    }

    /// POST a JSON body and decode the reply.
    ///
    /// A non-2xx carries the hub's own error text through verbatim rather than
    /// being flattened to a status code: the contract's `not_supported` /
    /// `not_accepting` distinction is the whole reason a client can tell "this
    /// kind can never do that" from "not right now".
    async fn post_json<T: serde::de::DeserializeOwned>(
        &self,
        path: &str,
        body: &serde_json::Value,
    ) -> Result<T, HubError> {
        let resp = self
            .http
            .post(format!("{}{path}", self.base))
            .json(body)
            .timeout(SNAPSHOT_TIMEOUT)
            .send()
            .await
            .map_err(|e| HubError::Unavailable(format!("hub {path}: {e}")))?;
        let status = resp.status();
        let text = resp.text().await.unwrap_or_default();
        if !status.is_success() {
            return Err(HubError::Failed(format!(
                "hub {path}: HTTP {status}{}",
                if text.is_empty() {
                    String::new()
                } else {
                    format!(" — {}", text.trim())
                }
            )));
        }
        // An empty 2xx body is legitimate for a verb with nothing to report;
        // decode it as null so a `Value` target succeeds.
        let raw = if text.trim().is_empty() {
            "null"
        } else {
            &text
        };
        serde_json::from_str(raw).map_err(|e| HubError::Failed(format!("hub {path}: {e}")))
    }

    async fn get_json<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, HubError> {
        let resp = self
            .http
            .get(format!("{}{path}", self.base))
            .timeout(SNAPSHOT_TIMEOUT)
            .send()
            .await
            .map_err(|e| HubError::Unavailable(format!("hub {path}: {e}")))?;
        if !resp.status().is_success() {
            return Err(HubError::Failed(format!(
                "hub {path}: HTTP {}",
                resp.status()
            )));
        }
        resp.json()
            .await
            .map_err(|e| HubError::Failed(format!("hub {path}: {e}")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use httpmock::prelude::*;

    /// Nothing listening is `Unavailable` (retry/degrade), never `Failed`.
    /// The split is what every caller's degradation posture keys on.
    #[tokio::test]
    async fn a_dead_port_is_unavailable_not_failed() {
        // Bind then drop: a port nothing can be listening on right now.
        let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let port = ln.local_addr().expect("addr").port();
        drop(ln);

        let client = HubClient::loopback(port).expect("client");
        let err = client.health().await.expect_err("nothing is listening");
        assert!(matches!(err, HubError::Unavailable(_)), "got {err:?}");
    }

    #[tokio::test]
    async fn health_accepts_our_app_id_and_rejects_a_squatter() {
        let server = MockServer::start();
        let mut ours = server.mock(|when, then| {
            when.method(GET).path("/v1/health");
            then.status(200)
                .json_body(serde_json::json!({"app": HUB_APP_ID, "version": "test"}));
        });
        HubClient::loopback(server.port())
            .expect("client")
            .health()
            .await
            .expect("our own app id is a hub");
        ours.assert();
        ours.delete();

        // Something else bound the port and answers JSON — must NOT pass, and
        // must be `Unavailable` so a caller degrades rather than erroring out.
        server.mock(|when, then| {
            when.method(GET).path("/v1/health");
            then.status(200)
                .json_body(serde_json::json!({"app": "something-else"}));
        });
        let err = HubClient::loopback(server.port())
            .expect("client")
            .health()
            .await
            .expect_err("a squatter is not a hub");
        assert!(matches!(err, HubError::Unavailable(_)), "got {err:?}");
        // The message names the address, so the operator can find the squatter.
        assert!(
            err.to_string().contains(&server.port().to_string()),
            "{err}"
        );
    }

    #[tokio::test]
    async fn turn_posts_the_text_and_returns_the_turn_id() {
        let server = MockServer::start();
        let m = server.mock(|when, then| {
            when.method(POST)
                .path("/v1/sessions/abc123/turn")
                .json_body(serde_json::json!({"text": "do the thing"}));
            then.status(202)
                .json_body(serde_json::json!({"turn_id": "t-7"}));
        });
        let id = HubClient::loopback(server.port())
            .expect("client")
            .turn("abc123", "do the thing")
            .await
            .expect("turn accepted");
        assert_eq!(id, "t-7");
        m.assert();
    }

    /// **A capability refusal must carry the hub's REASON through.**
    ///
    /// The contract distinguishes `not_supported` ("this kind can never do
    /// that") from `not_accepting` ("not right now"), and a client cannot tell
    /// them apart — or explain itself to a user — if the transport flattens
    /// both to "HTTP 409".
    #[tokio::test]
    async fn a_409_carries_the_contract_reason_not_just_a_status() {
        let server = MockServer::start();
        server.mock(|when, then| {
            when.method(POST).path("/v1/sessions/abc123/turn");
            then.status(409).json_body(serde_json::json!({
                "error": "not_supported",
                "message": "this session's kind does not accept turns"
            }));
        });
        let err = HubClient::loopback(server.port())
            .expect("client")
            .turn("abc123", "hi")
            .await
            .expect_err("a tui-laned kind refuses turns");
        let msg = err.to_string();
        assert!(matches!(err, HubError::Failed(_)), "{msg}");
        assert!(
            msg.contains("not_supported"),
            "the reason must survive: {msg}"
        );
    }

    #[tokio::test]
    async fn interrupt_reports_whether_anything_was_running() {
        let server = MockServer::start();
        let mut m = server.mock(|when, then| {
            when.method(POST).path("/v1/sessions/abc123/interrupt");
            then.status(202)
                .json_body(serde_json::json!({"interrupting": true}));
        });
        assert!(HubClient::loopback(server.port())
            .expect("client")
            .interrupt("abc123")
            .await
            .expect("accepted"));
        m.delete();

        // `false` is a legitimate ANSWER (nothing was running), not a failure —
        // a client that treated it as an error would show a spurious problem
        // every time a user interrupted an idle session.
        server.mock(|when, then| {
            when.method(POST).path("/v1/sessions/abc123/interrupt");
            then.status(202)
                .json_body(serde_json::json!({"interrupting": false}));
        });
        assert!(!HubClient::loopback(server.port())
            .expect("client")
            .interrupt("abc123")
            .await
            .expect("still a success"));
    }

    #[tokio::test]
    async fn approve_sends_the_decision_and_echoes_the_resolved_one() {
        let server = MockServer::start();
        let m = server.mock(|when, then| {
            when.method(POST)
                .path("/v1/sessions/abc123/approvals/ap-1")
                .json_body(serde_json::json!({"decision": "allow"}));
            then.status(200)
                .json_body(serde_json::json!({"resolved": true, "decision": "allow"}));
        });
        let decided = HubClient::loopback(server.port())
            .expect("client")
            .approve("abc123", "ap-1", "allow")
            .await
            .expect("resolved");
        assert_eq!(decided, "allow");
        m.assert();
    }

    /// A verb whose success body is empty must still succeed — several answer
    /// 202 with nothing at all.
    #[tokio::test]
    async fn an_empty_success_body_is_not_a_decode_failure() {
        let server = MockServer::start();
        server.mock(|when, then| {
            when.method(POST).path("/v1/sessions/abc123/input");
            then.status(202);
        });
        HubClient::loopback(server.port())
            .expect("client")
            .input("abc123", "hello")
            .await
            .expect("an empty 202 is a success");
    }

    /// A reachable hub that answers badly is `Failed`, not `Unavailable` — the
    /// other side of the split.
    #[tokio::test]
    async fn a_non_200_is_a_failure_not_an_absence() {
        let server = MockServer::start();
        server.mock(|when, then| {
            when.method(GET).path("/v1/health");
            then.status(500);
        });
        let err = HubClient::loopback(server.port())
            .expect("client")
            .health()
            .await
            .expect_err("500 is not a hub");
        assert!(matches!(err, HubError::Failed(_)), "got {err:?}");
    }

    #[tokio::test]
    async fn the_sessions_envelope_decodes_and_tolerates_an_empty_hub() {
        let server = MockServer::start();
        let mut m = server.mock(|when, then| {
            when.method(GET).path("/v1/sessions");
            then.status(200).json_body(serde_json::json!({"sessions": [{
                "slug": "hkn4vd",
                "tmux_session": "rc-hkn4vd",
                "kind": "shell",
                "state": "ready",
                "managed": true,
                "display_name": "plan012-probe",
                "target_label": "machine:mini3"
            }]}));
        });
        let sessions = HubClient::loopback(server.port())
            .expect("client")
            .sessions()
            .await
            .expect("snapshot");
        assert_eq!(sessions.len(), 1);
        assert_eq!(sessions[0].slug, "hkn4vd");
        m.assert();
        m.delete();

        // `sessions` is `#[serde(default)]`, so a hub with nothing running —
        // which is the normal state of a fresh machine — decodes to empty
        // rather than failing the whole read.
        server.mock(|when, then| {
            when.method(GET).path("/v1/sessions");
            then.status(200).json_body(serde_json::json!({}));
        });
        let sessions = HubClient::loopback(server.port())
            .expect("client")
            .sessions()
            .await
            .expect("an empty hub is not an error");
        assert!(sessions.is_empty());
    }

    /// A body that isn't JSON at all is `Failed`, not `Unavailable`: something
    /// answered, so a caller must surface it rather than quietly retrying
    /// forever as it would for an absent hub. (A proxy error page or an HTML
    /// login interstitial on the port is the realistic shape of this.)
    #[tokio::test]
    async fn a_body_that_is_not_json_is_a_failure_not_an_absence() {
        let server = MockServer::start();
        server.mock(|when, then| {
            when.method(GET).path("/v1/sessions");
            then.status(200)
                .header("content-type", "text/html")
                .body("<html>not a hub</html>");
        });
        let err = HubClient::loopback(server.port())
            .expect("client")
            .sessions()
            .await
            .expect_err("HTML is not a snapshot");
        assert!(matches!(err, HubError::Failed(_)), "got {err:?}");
        // The message names the route, so a log line says WHICH read broke.
        assert!(err.to_string().contains("/v1/sessions"), "{err}");
    }

    /// `messages()` — the fetch behind every `message.appended` notification.
    /// Pins the ROUTE (the `since` cursor has to reach the query string, or the
    /// client silently refetches the whole feed on every append) and the
    /// decoded body.
    #[tokio::test]
    async fn messages_carries_the_since_cursor_and_decodes_a_page() {
        let server = MockServer::start();
        let mut m = server.mock(|when, then| {
            when.method(GET)
                .path("/v1/sessions/hkn4vd/messages")
                .query_param("since", "12");
            then.status(200).json_body(serde_json::json!({
                "messages": [
                    {"seq": 13, "ts": "2026-08-22T02:05:30Z", "role": "assistant",
                     "type": "text", "text": "running the suite"},
                    {"seq": 14, "role": "tool", "type": "tool_use",
                     "tool": {"name": "shell", "detail": "cargo test"}}
                ],
                "truncated": true
            }));
        });
        let page = HubClient::loopback(server.port())
            .expect("client")
            .messages("hkn4vd", 12)
            .await
            .expect("page");
        m.assert(); // the mock only matches if `since=12` was on the wire
        m.delete();

        assert_eq!(page.messages.len(), 2);
        assert_eq!(page.messages[0].seq, 13);
        assert_eq!(page.messages[0].role, "assistant");
        assert_eq!(page.messages[0].text.as_deref(), Some("running the suite"));
        assert_eq!(page.messages[1].seq, 14);
        let tool = page.messages[1].tool.as_ref().expect("tool block");
        assert_eq!(tool.name.as_deref(), Some("shell"));
        assert_eq!(tool.detail.as_deref(), Some("cargo test"));
        assert!(page.truncated);

        // A shapeless body degrades to an EMPTY page rather than failing the
        // read — `RcMessagesPage::from_value` is deliberately tolerant, unlike
        // the serde-strict `sessions()` above, because a feed page is display
        // text and a caller that got no rows simply renders none.
        server.mock(|when, then| {
            when.method(GET).path("/v1/sessions/hkn4vd/messages");
            then.status(200)
                .json_body(serde_json::json!({"messages": "nope"}));
        });
        let page = HubClient::loopback(server.port())
            .expect("client")
            .messages("hkn4vd", 0)
            .await
            .expect("a shapeless page is not an error");
        assert_eq!(page, crate::rc::RcMessagesPage::default());
    }

    /// The SSE feed decodes, and — the case the byte-stream loop gets wrong if
    /// `parser.finish()` is dropped — a final record with no trailing blank
    /// line is still delivered when the stream ends.
    #[tokio::test]
    async fn events_decode_including_an_unterminated_final_record() {
        let server = MockServer::start();
        server.mock(|when, then| {
            when.method(GET).path("/v1/events");
            then.status(200)
                .header("content-type", "text/event-stream")
                // Two records: the first properly terminated, the second left
                // hanging as a stream that ends mid-frame would leave it.
                // Both carry the empty `shed` a directly-read hub sends.
                .body(concat!(
                    "event: activity.changed\n",
                    "data: {\"shed\":\"\",\"slug\":\"hkn4vd\",\"activity\":\"working\",\"state\":\"ready\"}\n",
                    "\n",
                    "event: session.updated\n",
                    "data: {\"shed\":\"\",\"slug\":\"hkn4vd\",\"session\":null}\n",
                ));
        });
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        HubClient::loopback(server.port())
            .expect("client")
            .events(&tx)
            .await
            .expect("the feed reads to EOF");
        drop(tx);

        let mut seen = Vec::new();
        while let Ok(ev) = rx.try_recv() {
            seen.push(ev);
        }
        assert_eq!(
            seen,
            vec![
                RcEvent::ActivityChanged {
                    shed: String::new(),
                    slug: "hkn4vd".to_string(),
                    activity: Some(crate::rc::RcActivity::Working),
                    activity_at: None,
                    state: Some(crate::rc::RcState::Ready),
                    last_message: None,
                },
                RcEvent::SessionUpdated {
                    shed: String::new(),
                    slug: "hkn4vd".to_string(),
                    activity: None,
                    state: None,
                    last_message: None,
                    lane: None,
                    removed: true,
                },
            ],
            "the unterminated tail must still be flushed"
        );
    }

    /// The events route keeps the same absence/failure split as the snapshot
    /// routes — a watcher retries an `Unavailable` hub and surfaces a `Failed`
    /// one, so conflating them either spams a dead port or hides a real 503.
    #[tokio::test]
    async fn events_separates_an_absent_hub_from_one_that_refuses() {
        let ln = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
        let dead = ln.local_addr().expect("addr").port();
        drop(ln);
        let (tx, _rx) = tokio::sync::mpsc::unbounded_channel();
        let err = HubClient::loopback(dead)
            .expect("client")
            .events(&tx)
            .await
            .expect_err("nothing is listening");
        assert!(matches!(err, HubError::Unavailable(_)), "got {err:?}");

        let server = MockServer::start();
        server.mock(|when, then| {
            when.method(GET).path("/v1/events");
            then.status(503);
        });
        let err = HubClient::loopback(server.port())
            .expect("client")
            .events(&tx)
            .await
            .expect_err("503 is not a stream");
        assert!(matches!(err, HubError::Failed(_)), "got {err:?}");
        assert!(err.to_string().contains("503"), "{err}");
    }

    /// **A stream that dies mid-body must ERROR, not look like a clean EOF.**
    ///
    /// This is the failure the watcher's reconnect loop hangs off: a hub that
    /// is killed, or a forward (`ssh -L`) that drops, cuts the body without
    /// closing the response. If `events()` swallowed that as `Ok(())` the
    /// caller could not tell a finished stream from a severed one, and a live
    /// feed would go permanently silent while reporting success — the same
    /// silent-but-healthy failure mode as the empty-shed decode bug.
    ///
    /// httpmock can't cut a body, so this is a raw socket: promise a
    /// Content-Length, deliver one complete SSE record, then hang up.
    #[tokio::test]
    async fn events_surfaces_a_mid_stream_break_after_delivering_what_arrived() {
        use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind");
        let port = listener.local_addr().expect("addr").port();
        tokio::spawn(async move {
            let (mut sock, _) = listener.accept().await.expect("accept");
            let mut head = [0u8; 1024];
            let _ = sock.read(&mut head).await;
            sock.write_all(
                concat!(
                    "HTTP/1.1 200 OK\r\n",
                    "Content-Type: text/event-stream\r\n",
                    // Far more body than will ever be sent.
                    "Content-Length: 4096\r\n",
                    "\r\n",
                    "event: activity.changed\n",
                    "data: {\"shed\":\"\",\"slug\":\"hkn4vd\",\"activity\":\"working\"}\n",
                    "\n",
                )
                .as_bytes(),
            )
            .await
            .expect("write");
            sock.shutdown().await.expect("half-close");
        });

        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let err = HubClient::loopback(port)
            .expect("client")
            .events(&tx)
            .await
            .expect_err("a severed body is not a clean end of stream");
        assert!(matches!(err, HubError::Failed(_)), "got {err:?}");
        drop(tx);

        // …and the record that DID arrive before the break was delivered, not
        // discarded with the error.
        let ev = rx
            .try_recv()
            .expect("the pre-break record reached the sink");
        assert_eq!(
            ev,
            RcEvent::ActivityChanged {
                shed: String::new(),
                slug: "hkn4vd".to_string(),
                activity: Some(crate::rc::RcActivity::Working),
                activity_at: None,
                state: None,
                last_message: None,
            }
        );
        assert!(rx.try_recv().is_err(), "nothing else was sent");
    }
}
