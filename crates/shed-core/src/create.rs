//! Pull-based create orchestration: kick off an SSE `POST /api/sheds` in the
//! background, poll its progress by id, cancel it. Hoisted out of `shed-core-ffi`
//! (M1) so the Swift FFI wrapper and the GTK app share ONE implementation rather
//! than writing this store twice — the duplication the Rust core exists to kill.
//!
//! The low-level SSE streaming + `CreateSink` live in `http`; this module only
//! adds the id-keyed store the pull-based callers poll. It is an owned object
//! (not a global): the FFI keeps one process-wide instance for its host-less
//! `create_status(id)` contract, and the GTK app makes its own per-`App` instance
//! and drives `status` from a glib timer (the SSE task runs on the tokio runtime,
//! so reqwest never touches the glib executor).

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use tokio::runtime::Handle;
use tokio::task::AbortHandle;

use crate::http::{Client, CreateSink};
use crate::models::{CreateShedRequest, Shed};

/// The state of an in-flight create (maps to Swift's `CreateState` wire strings).
#[derive(Clone, Debug, PartialEq, Eq, serde::Serialize)]
#[serde(rename_all = "lowercase")]
pub enum CreateState {
    Progress,
    Complete,
    Error,
}

/// A snapshot of an in-flight create, returned by [`CreateStore::status`].
#[derive(Clone, Debug, serde::Serialize)]
pub struct CreateProgress {
    pub id: String,
    pub state: CreateState,
    pub messages: Vec<String>,
    pub shed: Option<Shed>,
    pub error: Option<String>,
}

struct StoredProgress {
    state: CreateProgress,
    abort: Option<AbortHandle>,
}

type Store = Mutex<HashMap<String, StoredProgress>>;

/// A [`CreateSink`] that folds progress/complete/error into a store entry.
struct StoreSink {
    store: Arc<Store>,
    id: String,
}

impl CreateSink for StoreSink {
    fn on_progress(&self, message: String) {
        if let Some(p) = self.store.lock().unwrap().get_mut(&self.id) {
            p.state.messages.push(message);
        }
    }
    fn on_complete(&self, shed: Shed) {
        if let Some(p) = self.store.lock().unwrap().get_mut(&self.id) {
            p.state.state = CreateState::Complete;
            p.state.shed = Some(shed);
        }
    }
    fn on_error(&self, message: String) {
        if let Some(p) = self.store.lock().unwrap().get_mut(&self.id) {
            p.state.state = CreateState::Error;
            p.state.error = Some(message);
        }
    }
}

/// A pull-based store of in-flight creates, keyed by an opaque id. Cheap to clone
/// (Arc-backed), so the threads that start, poll, and cancel a create share one
/// store.
#[derive(Clone, Default)]
pub struct CreateStore {
    inner: Arc<Store>,
}

impl CreateStore {
    pub fn new() -> Self {
        Self::default()
    }

    /// Start a create: `POST /api/sheds` is streamed on `rt` in the background;
    /// returns an id whose progress the caller polls via [`status`](Self::status).
    /// The task is spawned on the caller's runtime handle (never an ambient
    /// `tokio::spawn`), so a GTK caller keeps reqwest off the glib executor.
    pub fn start(&self, rt: &Handle, client: &Client, request: CreateShedRequest) -> String {
        let id = uuid::Uuid::new_v4().to_string();
        self.inner.lock().unwrap().insert(
            id.clone(),
            StoredProgress {
                state: CreateProgress {
                    id: id.clone(),
                    state: CreateState::Progress,
                    messages: Vec::new(),
                    shed: None,
                    error: None,
                },
                abort: None,
            },
        );
        let sink = StoreSink {
            store: self.inner.clone(),
            id: id.clone(),
        };
        let client = client.clone();
        let handle = rt.spawn(async move {
            client.create_shed(&request, &sink).await;
        });
        if let Some(p) = self.inner.lock().unwrap().get_mut(&id) {
            p.abort = Some(handle.abort_handle());
        }
        id
    }

    /// Snapshot of an in-flight create (poll until `state` is complete/error), or
    /// `None` once cancelled/unknown.
    pub fn status(&self, id: &str) -> Option<CreateProgress> {
        self.inner.lock().unwrap().get(id).map(|p| p.state.clone())
    }

    /// Abort a create's stream + drop its state. Idempotent; a caller's
    /// `onTermination`/cancel path calls this, since `Task.cancel` doesn't
    /// propagate over the FFI (M0 finding).
    pub fn cancel(&self, id: &str) {
        if let Some(p) = self.inner.lock().unwrap().remove(id) {
            if let Some(h) = p.abort {
                h.abort();
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use httpmock::prelude::*;

    fn client(server: &MockServer) -> Client {
        Client::new(server.base_url(), "mini2".into(), String::new(), None, None).unwrap()
    }

    /// Poll a create to its terminal (non-`Progress`) state.
    async fn poll_until_terminal(store: &CreateStore, id: &str) -> CreateProgress {
        loop {
            let p = store.status(id).expect("present while in-flight");
            if p.state != CreateState::Progress {
                return p;
            }
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn start_polls_to_complete() {
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
        let store = CreateStore::new();
        let id = store.start(
            &Handle::current(),
            &client(&server),
            CreateShedRequest {
                name: "folio".into(),
                ..Default::default()
            },
        );
        let final_state = poll_until_terminal(&store, &id).await;
        assert_eq!(final_state.state, CreateState::Complete);
        assert_eq!(final_state.messages, vec!["building"]);
        let shed = final_state.shed.expect("a complete shed");
        assert_eq!(shed.name, "folio");
        assert_eq!(shed.host, "mini2"); // stamped on the SSE-complete path
        assert!(final_state.error.is_none());
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn error_event_becomes_error_state() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds");
                t.status(200)
                    .body("event: error\ndata: {\"message\":\"disk full\"}\n\n");
            })
            .await;
        let store = CreateStore::new();
        let id = store.start(
            &Handle::current(),
            &client(&server),
            CreateShedRequest {
                name: "x".into(),
                ..Default::default()
            },
        );
        let final_state = poll_until_terminal(&store, &id).await;
        assert_eq!(final_state.state, CreateState::Error);
        assert_eq!(
            final_state.error.as_deref(),
            Some("create failed: disk full")
        );
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn cancel_removes_entry() {
        let server = MockServer::start_async().await;
        server
            .mock_async(|w, t| {
                w.method(POST).path("/api/sheds");
                t.status(200)
                    .body("event: complete\ndata: {\"name\":\"y\",\"status\":\"running\"}\n\n");
            })
            .await;
        let store = CreateStore::new();
        let id = store.start(
            &Handle::current(),
            &client(&server),
            CreateShedRequest {
                name: "y".into(),
                ..Default::default()
            },
        );
        assert!(store.status(&id).is_some());
        store.cancel(&id);
        assert!(store.status(&id).is_none());
    }
}
