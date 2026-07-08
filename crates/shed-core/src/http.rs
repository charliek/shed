//! HTTP read client for one shed-server.
//!
//! reqwest + rustls; the base URL is injected (the app substitutes the hermetic
//! mock in test mode — the core is env-agnostic). Decoding is the defensive
//! `models` layer.
//!
//! Parity with Swift's `ShedServerClient`: an 8s GET timeout, an explicit
//! User-Agent, an https-only redirect policy, leaf-cert pinning (fail-closed on
//! a non-https URL), a control-token bearer with a 401 → invalidate + retry-once
//! (provider-backed only), and `ShedError` matching `ShedClientError`.
//! Lifecycle + SSE create land in M4.

use std::sync::Arc;
use std::time::Duration;

use futures_util::StreamExt;
use thiserror::Error;

use crate::models::{
    CreateShedRequest, EgressProfileInfo, ImageList, ServerInfo, Shed, ShedImage, ShedList,
    SystemDiskUsage,
};
use crate::sse::SseParser;
use crate::token::{ControlTokenProvider, TokenMinter};

/// Mirrors Swift's `ShedClientError` (same cases, same messages).
#[derive(Debug, Error)]
pub enum ShedError {
    #[error("shed-server returned HTTP {0}")]
    BadStatus(u16),
    #[error("transport error: {0}")]
    Transport(String),
    #[error("decode error: {0}")]
    Decode(String),
    #[error("create failed: {0}")]
    Create(String),
    #[error("{0}")]
    Config(String),
}

const GET_TIMEOUT: Duration = Duration::from_secs(8);
const WRITE_TIMEOUT: Duration = Duration::from_secs(15);
/// Max gap between SSE bytes during a create before we give up (a hung stream);
/// generous so a healthy provision with periodic progress never trips it.
const CREATE_IDLE_TIMEOUT: Duration = Duration::from_secs(120);
const USER_AGENT: &str = concat!("shed-desktop-core/", env!("CARGO_PKG_VERSION"));

/// Sink for create progress. shed-core streams the SSE and drives these; the FFI
/// layer implements it to update a create-status store the Swift side polls.
pub trait CreateSink: Send + Sync {
    fn on_progress(&self, message: String);
    fn on_complete(&self, shed: Shed);
    fn on_error(&self, message: String);
}

/// A read client for one shed-server host. `Clone` is cheap (reqwest::Client and
/// the token provider are Arc-backed) so a create task can own its own handle
/// sharing the same token cache.
#[derive(Clone)]
pub struct Client {
    base_url: String,
    server_name: String,
    /// Static open-mode config token; used only when there is no `token_provider`.
    token: String,
    token_provider: Option<Arc<ControlTokenProvider>>,
    http: reqwest::Client,
}

impl Client {
    /// `base_url` is injected by the app. `token` is the static open-mode config
    /// token (sent only when there's no minter). `pin` (`sha256:<hex>`) enables
    /// leaf pinning; a pin on a non-https URL is refused (fail-closed). `minter`,
    /// when present, backs a control-token FSM whose minted token is sent — and
    /// on a mint failure NO token is sent (never the static one; no downgrade).
    pub fn new(
        base_url: String,
        server_name: String,
        token: String,
        pin: Option<String>,
        minter: Option<Arc<dyn TokenMinter>>,
    ) -> Result<Self, ShedError> {
        let pin = pin.filter(|p| !p.is_empty());
        if pin.is_some() && !base_url.to_lowercase().starts_with("https://") {
            return Err(ShedError::Config(format!(
                "TLS pin configured for a non-https URL {base_url}; refusing to send unpinned plaintext"
            )));
        }
        let http = build_http_client(pin.as_deref())?;
        let token_provider =
            minter.map(|m| Arc::new(ControlTokenProvider::new(server_name.clone(), m)));
        Ok(Self {
            base_url,
            server_name,
            token,
            token_provider,
            http,
        })
    }

    /// The bearer token to send, or `None`. Provider-backed clients send the
    /// minted token, or NOTHING on a mint failure (never the static token — no
    /// secure-by-default downgrade); provider-less clients send the static token.
    pub(crate) async fn bearer(&self) -> Option<String> {
        if let Some(p) = &self.token_provider {
            p.token().await.ok().filter(|t| !t.is_empty())
        } else if !self.token.is_empty() {
            Some(self.token.clone())
        } else {
            None
        }
    }

    async fn send_once(
        &self,
        method: reqwest::Method,
        path: &str,
        timeout: Duration,
    ) -> Result<Vec<u8>, ShedError> {
        let url = format!("{}{}", self.base_url.trim_end_matches('/'), path);
        let mut req = self.http.request(method, &url).timeout(timeout);
        if let Some(tok) = self.bearer().await {
            req = req.bearer_auth(tok);
        }
        let resp = req
            .send()
            .await
            .map_err(|e| ShedError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        if !(200..300).contains(&status) {
            return Err(ShedError::BadStatus(status));
        }
        Ok(resp
            .bytes()
            .await
            .map_err(|e| ShedError::Transport(e.to_string()))?
            .to_vec())
    }

    /// Send once, and on a provider-backed 401 invalidate + retry once
    /// (at-most-once, mirrors the SDK/CLI). Static/no-token clients don't retry.
    async fn request(
        &self,
        method: reqwest::Method,
        path: &str,
        timeout: Duration,
    ) -> Result<Vec<u8>, ShedError> {
        match self.send_once(method.clone(), path, timeout).await {
            Err(ShedError::BadStatus(401)) if self.token_provider.is_some() => {
                if let Some(p) = &self.token_provider {
                    p.invalidate().await;
                }
                self.send_once(method, path, timeout).await
            }
            other => other,
        }
    }

    async fn get_json<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, ShedError> {
        let bytes = self
            .request(reqwest::Method::GET, path, GET_TIMEOUT)
            .await?;
        serde_json::from_slice(&bytes).map_err(|e| ShedError::Decode(e.to_string()))
    }

    /// A lifecycle mutation (POST/DELETE, no response body). 15s timeout.
    async fn lifecycle(&self, method: reqwest::Method, path: &str) -> Result<(), ShedError> {
        self.request(method, path, WRITE_TIMEOUT).await.map(|_| ())
    }

    /// `GET /api/info`.
    pub async fn info(&self) -> Result<ServerInfo, ShedError> {
        self.get_json("/api/info").await
    }

    /// `GET /api/sheds` -> sheds stamped with this host's config name (the server
    /// omits `host`; the client stamps it, as Swift's `listSheds` does).
    pub async fn list_sheds(&self) -> Result<Vec<Shed>, ShedError> {
        let list: ShedList = self.get_json("/api/sheds").await?;
        Ok(list
            .sheds
            .into_iter()
            .map(|mut s| {
                s.host = self.server_name.clone();
                s
            })
            .collect())
    }

    /// `GET /api/system/df`.
    pub async fn system_df(&self) -> Result<SystemDiskUsage, ShedError> {
        self.get_json("/api/system/df").await
    }

    /// `GET /api/images`.
    pub async fn list_images(&self) -> Result<Vec<ShedImage>, ShedError> {
        let list: ImageList = self.get_json("/api/images").await?;
        Ok(list.images)
    }

    /// `GET /api/egress/profiles`.
    pub async fn egress_profiles(&self) -> Result<Vec<EgressProfileInfo>, ShedError> {
        self.get_json("/api/egress/profiles").await
    }

    /// `POST /api/sheds/{name}/start`.
    pub async fn start(&self, name: &str) -> Result<(), ShedError> {
        self.lifecycle(reqwest::Method::POST, &format!("/api/sheds/{name}/start"))
            .await
    }

    /// `POST /api/sheds/{name}/stop`.
    pub async fn stop(&self, name: &str) -> Result<(), ShedError> {
        self.lifecycle(reqwest::Method::POST, &format!("/api/sheds/{name}/stop"))
            .await
    }

    /// `POST /api/sheds/{name}/reset`.
    pub async fn reset(&self, name: &str) -> Result<(), ShedError> {
        self.lifecycle(reqwest::Method::POST, &format!("/api/sheds/{name}/reset"))
            .await
    }

    /// `DELETE /api/sheds/{name}`.
    pub async fn delete(&self, name: &str) -> Result<(), ShedError> {
        self.lifecycle(reqwest::Method::DELETE, &format!("/api/sheds/{name}"))
            .await
    }

    /// `POST /api/sheds` with `Accept: text/event-stream`: streams progress then
    /// a final shed, delivered via `sink`. A transport/parse/error-event failure,
    /// or a stream that ends without a `complete`, is delivered as
    /// `sink.on_error`. Create mints its token inline once and does NOT 401-retry
    /// (one-shot stream), never downgrading to the static token — mirroring
    /// Swift's `createShed`.
    pub async fn create_shed(&self, req: &CreateShedRequest, sink: &dyn CreateSink) {
        if let Err(e) = self.create_stream(req, sink).await {
            sink.on_error(e.to_string());
        }
    }

    async fn create_stream(
        &self,
        req: &CreateShedRequest,
        sink: &dyn CreateSink,
    ) -> Result<(), ShedError> {
        let url = format!("{}/api/sheds", self.base_url.trim_end_matches('/'));
        let mut rb = self
            .http
            .post(&url)
            .header(reqwest::header::ACCEPT, "text/event-stream")
            .json(req);
        if let Some(tok) = self.bearer().await {
            rb = rb.bearer_auth(tok);
        }
        let resp = rb
            .send()
            .await
            .map_err(|e| ShedError::Transport(e.to_string()))?;
        let status = resp.status().as_u16();
        if !(200..300).contains(&status) {
            return Err(ShedError::BadStatus(status));
        }
        let mut stream = resp.bytes_stream();
        let mut parser = SseParser::new();
        let mut saw_complete = false;
        loop {
            match tokio::time::timeout(CREATE_IDLE_TIMEOUT, stream.next()).await {
                Err(_) => return Err(ShedError::Create("create stream idle timeout".into())),
                Ok(None) => break,
                Ok(Some(chunk)) => {
                    let chunk = chunk.map_err(|e| ShedError::Transport(e.to_string()))?;
                    for ev in parser.feed(&chunk) {
                        self.handle_create_event(&ev, sink, &mut saw_complete)?;
                    }
                }
            }
        }
        for ev in parser.finish() {
            self.handle_create_event(&ev, sink, &mut saw_complete)?;
        }
        if !saw_complete {
            return Err(ShedError::Create(
                "stream ended before a complete event".into(),
            ));
        }
        Ok(())
    }

    fn handle_create_event(
        &self,
        ev: &crate::sse::SseEvent,
        sink: &dyn CreateSink,
        saw_complete: &mut bool,
    ) -> Result<(), ShedError> {
        match ev.event.as_str() {
            "progress" => {
                if let Some(msg) = decode_progress(&ev.data) {
                    sink.on_progress(msg);
                }
            }
            "complete" => {
                let mut shed: Shed =
                    serde_json::from_str(&ev.data).map_err(|e| ShedError::Decode(e.to_string()))?;
                shed.host = self.server_name.clone(); // stamp host (SSE-complete path)
                *saw_complete = true;
                sink.on_complete(shed);
            }
            "error" => return Err(ShedError::Create(decode_error(&ev.data))),
            _ => {}
        }
        Ok(())
    }
}

fn build_http_client(pin: Option<&str>) -> Result<reqwest::Client, ShedError> {
    let mut builder = reqwest::Client::builder()
        .user_agent(USER_AGENT)
        // Fail closed on a plaintext redirect, mirroring the Swift pinned session.
        .redirect(reqwest::redirect::Policy::custom(|attempt| {
            if attempt.url().scheme() == "https" {
                attempt.follow()
            } else {
                attempt.stop()
            }
        }));
    if let Some(pin) = pin {
        builder = builder.use_preconfigured_tls(crate::tls::pinned_client_config(pin)?);
    }
    builder
        .build()
        .map_err(|e| ShedError::Transport(e.to_string()))
}

/// A progress event's `{"message": ...}`, or the raw data as a fallback.
fn decode_progress(data: &str) -> Option<String> {
    #[derive(serde::Deserialize)]
    struct Progress {
        message: Option<String>,
    }
    if let Ok(p) = serde_json::from_str::<Progress>(data) {
        if let Some(m) = p.message {
            return Some(m);
        }
    }
    if data.is_empty() {
        None
    } else {
        Some(data.to_string())
    }
}

/// An error event's `message ?? code ?? raw` (mirrors Swift's decodeErrorMessage).
fn decode_error(data: &str) -> String {
    #[derive(serde::Deserialize)]
    struct ApiError {
        code: Option<String>,
        message: Option<String>,
    }
    if let Ok(e) = serde_json::from_str::<ApiError>(data) {
        return e.message.or(e.code).unwrap_or_else(|| data.to_string());
    }
    data.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::token::MintedToken;
    use httpmock::prelude::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn client(server: &MockServer) -> Client {
        Client::new(
            server.base_url(),
            "mini2".to_string(),
            String::new(),
            None,
            None,
        )
        .unwrap()
    }

    #[tokio::test]
    async fn info_decodes() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let info = client(&server).info().await.unwrap();
        assert_eq!(info.name, "mini2");
        assert_eq!(info.backend.as_deref(), Some("firecracker"));
    }

    #[tokio::test]
    async fn list_sheds_stamps_host() {
        let server = MockServer::start_async().await;
        let body = format!(
            r#"{{"sheds":[{}]}}"#,
            include_str!("../../fixtures/shed_real.json")
        );
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/sheds");
                t.status(200).body(body);
            })
            .await;
        let sheds = client(&server).list_sheds().await.unwrap();
        assert_eq!(sheds.len(), 1);
        assert_eq!(sheds[0].name, "hello-world");
        assert_eq!(sheds[0].host, "mini2"); // stamped by the client
    }

    #[tokio::test]
    async fn list_sheds_null_is_empty() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/sheds");
                t.status(200).body(r#"{"sheds":null}"#);
            })
            .await;
        assert!(client(&server).list_sheds().await.unwrap().is_empty());
    }

    #[tokio::test]
    async fn system_df_decodes() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/system/df");
                t.status(200)
                    .body(include_str!("../../fixtures/system_df.json"));
            })
            .await;
        let df = client(&server).system_df().await.unwrap();
        assert_eq!(df.images.len(), 1);
        assert_eq!(df.totals.all.logical_bytes, 1073743872);
    }

    #[tokio::test]
    async fn images_and_egress_decode() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/images");
                t.status(200).body(format!(
                    r#"{{"images":[{}]}}"#,
                    include_str!("../../fixtures/image_enriched.json")
                ));
            })
            .await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/egress/profiles");
                t.status(200)
                    .body(include_str!("../../fixtures/egress_profiles.json"));
            })
            .await;
        let c = client(&server);
        let imgs = c.list_images().await.unwrap();
        assert_eq!(imgs.len(), 1);
        assert_eq!(imgs[0].alias.as_deref(), Some("base"));
        let profiles = c.egress_profiles().await.unwrap();
        assert_eq!(profiles.len(), 2);
    }

    #[tokio::test]
    async fn bad_status_maps() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(404);
            })
            .await;
        let err = client(&server).info().await.unwrap_err();
        assert!(matches!(err, ShedError::BadStatus(404)));
    }

    #[tokio::test]
    async fn malformed_maps_to_decode() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(200).body("not json");
            })
            .await;
        let err = client(&server).info().await.unwrap_err();
        assert!(matches!(err, ShedError::Decode(_)));
    }

    #[tokio::test]
    async fn lifecycle_start_posts() {
        let server = MockServer::start_async().await;
        let m = server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds/hello/start");
                t.status(200);
            })
            .await;
        client(&server).start("hello").await.unwrap();
        m.assert_async().await;
    }

    #[tokio::test]
    async fn lifecycle_delete_ok_and_stop_bad_status() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(DELETE).path("/api/sheds/gone");
                t.status(200);
            })
            .await;
        client(&server).delete("gone").await.unwrap();
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds/x/stop");
                t.status(500);
            })
            .await;
        assert!(matches!(
            client(&server).stop("x").await,
            Err(ShedError::BadStatus(500))
        ));
    }

    // A minter returning tok-1, tok-2, ... on successive mints.
    struct SeqMinter {
        calls: AtomicUsize,
    }
    #[async_trait::async_trait]
    impl TokenMinter for SeqMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            let n = self.calls.fetch_add(1, Ordering::SeqCst) + 1;
            Ok(MintedToken {
                token: format!("tok-{n}"),
                expires_at_unix: None,
            })
        }
    }
    struct FailMinter;
    #[async_trait::async_trait]
    impl TokenMinter for FailMinter {
        async fn mint(&self, _server: &str) -> Result<MintedToken, ShedError> {
            Err(ShedError::Transport("mint down".into()))
        }
    }

    #[tokio::test]
    async fn provider_sends_bearer_token() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer tok-1");
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let minter = Arc::new(SeqMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter),
        )
        .unwrap();
        assert_eq!(c.info().await.unwrap().name, "mini2");
    }

    #[tokio::test]
    async fn retries_once_on_401_with_reminted_token() {
        let server = MockServer::start_async().await;
        // Stale token -> 401.
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer tok-1");
                t.status(401);
            })
            .await;
        // Re-minted token -> 200.
        server
            .mock_async(|w, t| {
                w.method(GET)
                    .path("/api/info")
                    .header("authorization", "Bearer tok-2");
                t.status(200)
                    .body(include_str!("../../fixtures/server_info.json"));
            })
            .await;
        let minter = Arc::new(SeqMinter {
            calls: AtomicUsize::new(0),
        });
        let c = Client::new(
            server.base_url(),
            "mini2".into(),
            String::new(),
            None,
            Some(minter),
        )
        .unwrap();
        assert_eq!(c.info().await.unwrap().name, "mini2"); // succeeds after retry
    }

    #[tokio::test]
    async fn static_token_used_without_provider() {
        let c = Client::new(
            "http://x".into(),
            "s".into(),
            "static-tok".into(),
            None,
            None,
        )
        .unwrap();
        assert_eq!(c.bearer().await, Some("static-tok".to_string()));
    }

    #[tokio::test]
    async fn mint_failure_is_fail_closed_no_downgrade() {
        // Provider fails + a static token is set → NO token (never the static).
        let c = Client::new(
            "http://x".into(),
            "s".into(),
            "static-tok".into(),
            None,
            Some(Arc::new(FailMinter)),
        )
        .unwrap();
        assert_eq!(c.bearer().await, None);
    }

    #[test]
    fn pin_on_non_https_is_config_error() {
        let result = Client::new(
            "http://x".into(),
            "s".into(),
            String::new(),
            Some("sha256:aa".into()),
            None,
        );
        assert!(matches!(result, Err(ShedError::Config(_))));
    }

    #[tokio::test]
    async fn redirect_to_non_https_is_not_followed() {
        // The https-only redirect policy must NOT follow a redirect to a
        // non-https URL (a plaintext downgrade) — it stops, surfacing the 3xx
        // rather than dialing the target. Exercised on Linux since the GTK
        // e2e's plain-HTTP mock never trips the pin/redirect paths.
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(GET).path("/api/info");
                t.status(302)
                    .header("location", "http://example.invalid/api/info");
            })
            .await;
        // BadStatus(302), not a transport error from dialing example.invalid.
        match client(&server).info().await {
            Err(ShedError::BadStatus(302)) => {}
            other => panic!("expected the redirect to be stopped (BadStatus 302), got {other:?}"),
        }
    }

    #[derive(Default, Clone)]
    struct RecordState {
        messages: Vec<String>,
        shed: Option<Shed>,
        error: Option<String>,
    }
    #[derive(Default)]
    struct RecordingSink {
        state: std::sync::Mutex<RecordState>,
    }
    impl RecordingSink {
        fn snapshot(&self) -> RecordState {
            self.state.lock().unwrap().clone()
        }
    }
    impl CreateSink for RecordingSink {
        fn on_progress(&self, message: String) {
            self.state.lock().unwrap().messages.push(message);
        }
        fn on_complete(&self, shed: Shed) {
            self.state.lock().unwrap().shed = Some(shed);
        }
        fn on_error(&self, message: String) {
            self.state.lock().unwrap().error = Some(message);
        }
    }

    #[tokio::test]
    async fn create_streams_progress_then_complete() {
        let server = MockServer::start_async().await;
        let sse = "event: progress\ndata: {\"message\":\"building\"}\n\n\
                   event: complete\ndata: {\"name\":\"folio\",\"status\":\"running\"}\n\n";
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds");
                t.status(200)
                    .header("content-type", "text/event-stream")
                    .body(sse);
            })
            .await;
        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "folio".into(),
            repo: Some("charliek/folio".into()),
            ..Default::default()
        };
        client(&server).create_shed(&req, sink.as_ref()).await;
        let s = sink.snapshot();
        assert_eq!(s.messages, vec!["building"]);
        let shed = s.shed.expect("a complete shed");
        assert_eq!(shed.name, "folio");
        assert_eq!(shed.host, "mini2"); // stamped on the SSE-complete path
        assert!(s.error.is_none());
    }

    #[tokio::test]
    async fn create_error_event_reports_error() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds");
                t.status(200)
                    .body("event: error\ndata: {\"message\":\"disk full\"}\n\n");
            })
            .await;
        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "x".into(),
            ..Default::default()
        };
        client(&server).create_shed(&req, sink.as_ref()).await;
        assert_eq!(
            sink.snapshot().error.as_deref(),
            Some("create failed: disk full")
        );
    }

    #[tokio::test]
    async fn create_end_without_complete_reports_error() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds");
                t.status(200)
                    .body("event: progress\ndata: {\"message\":\"x\"}\n\n");
            })
            .await;
        let sink = Arc::new(RecordingSink::default());
        let req = CreateShedRequest {
            name: "x".into(),
            ..Default::default()
        };
        client(&server).create_shed(&req, sink.as_ref()).await;
        assert_eq!(
            sink.snapshot().error.as_deref(),
            Some("create failed: stream ended before a complete event")
        );
    }
}
