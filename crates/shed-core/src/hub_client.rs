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
        assert!(err.to_string().contains(&server.port().to_string()), "{err}");
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
                .body(concat!(
                    "event: activity.changed\n",
                    "data: {\"shed\":\"\",\"slug\":\"hkn4vd\",\"activity\":\"working\"}\n",
                    "\n",
                    "event: session.updated\n",
                    "data: {\"shed\":\"\",\"slug\":\"hkn4vd\",\"removed\":true}\n",
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
            seen.len(),
            2,
            "the unterminated tail must still be flushed: {seen:?}"
        );
    }
}
