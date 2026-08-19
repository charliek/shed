//! A minimal client for the **local activity hub** on `127.0.0.1:1029` —
//! served on machines by the `shed-host-agent` daemon's resident role (plan
//! 010) and in a shed by the guest hub. The wire is identical either way (the
//! rc-parity hub family is the proof), so this client is provider-blind.
//!
//! `sx watch` *reads* the hub over plain loopback HTTP, and reaches a
//! remote machine's hub through an `ssh -L` tunnel to the same port. Four
//! routes are used, all read-only (`internal/ext/rc/hub.go:298-302`):
//!
//! | route | use |
//! |---|---|
//! | `GET /v1/health` | is anything there, and is it OURS (`app == shed-rc-hub`) |
//! | `GET /v1/sessions` | the snapshot `sx watch` opens with |
//! | `GET /v1/events` | the SSE stream (`activity.changed`, `session.updated`, `message.appended`) |
//! | `GET /v1/sessions/{slug}/messages?since=N` | the body behind a `message.appended` |
//!
//! It deliberately does NOT go through `shed_core::http::Client`: that is a
//! shed-SERVER protocol client (TLS pinning, control-token FSM, mTLS enrollment)
//! and none of it applies to an unauthenticated loopback daemon.

use std::time::Duration;

use futures_util::StreamExt as _;
use serde_json::Value;

use shed_core::rc::{RcMessagesPage, RcSessionDto};
use shed_core::rc_events::{parse_rc_event, RcEvent};
use shed_core::sse::SseParser;

/// The hub's fixed loopback port (`rc.HubAddr`, `hub.go:62`). Duplicated as a
/// constant rather than imported because the Go value is not exported to Rust;
/// the parity harness pins the pair through the cursor hook script's bytes.
pub const HUB_PORT: u16 = 1029;

/// The identity token `GET /v1/health` returns in `app` (`rc.HubAppID`) — proof
/// that the process on the port is a hub and not something else that bound 1029.
const HUB_APP_ID: &str = "shed-rc-hub";

const SNAPSHOT_TIMEOUT: Duration = Duration::from_secs(5);

/// A read-only client bound to one hub base URL.
pub struct HubClient {
    base: String,
    http: reqwest::Client,
}

/// What a hub read can go wrong with, split so `sx watch` can tell "nothing is
/// listening" (degrade to probe polling, quietly) from "something answered
/// badly" (say so).
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

impl HubClient {
    /// A client for `127.0.0.1:<port>` (the local hub, or the local end of an
    /// `ssh -L` tunnel to a machine's hub).
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
    /// No reconnect and no idle timeout here: the hub heartbeats every ~25 s with
    /// a comment frame, and `sx watch` is a foreground command a person ends with
    /// Ctrl-C — a reconnect loop belongs to a long-lived client, not to this.
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
