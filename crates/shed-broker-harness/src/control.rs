//! The control socket: how the wire harness reads the broker's status, sees the
//! approval requests and audit events the bridge sends the app, and decides.
//!
//! A Unix socket (mode 0600) of newline-delimited JSON. Any number of connections
//! may be open at once, and each request is answered on its own connection:
//!
//! * `{"op":"status"}` → the status JSON (one line).
//! * `{"op":"decide","id":"…","decision":"approve"|"deny"}` → `{"ok":true}`, after
//!   handing the decision to the bridge's responder as a person's (`DecidedBy::User`).
//! * `{"op":"subscribe"}` → `{"ok":true}`, and from then on that connection carries
//!   frames only: every frame queued since spawn, in order, then each new one as it
//!   comes. At most one subscriber at a time; another `subscribe` meanwhile gets
//!   `{"error":"already subscribed"}`. A frame leaves the queue only once it has been
//!   written to the subscriber; when the subscriber goes, the slot is free again and
//!   frames queue up for the next.
//!
//! The bridge's approval gate never waits for a subscriber: the app is always its own
//! consumer, so a request waits for a decision (or the config's approval timeout)
//! whether or not anything is subscribed here.

use std::collections::VecDeque;
use std::io;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use serde_json::{json, Map, Value};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader, Lines};
use tokio::net::unix::{OwnedReadHalf, OwnedWriteHalf};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{mpsc, watch, Notify};

use shed_app::{BrokerEvent, ResponderRef};
use shed_core::approval::protocol::{AuditEventFrame, BrokerFrame};
use shed_core::approval::{ApprovalDecision, ApprovalRequest, DecidedBy};

/// The frames the bridge has sent and no subscriber has taken yet, plus the one
/// subscriber slot.
#[derive(Default)]
pub struct Hub {
    queue: Mutex<VecDeque<String>>,
    notify: Notify,
    subscribed: AtomicBool,
}

impl Hub {
    /// Queue one frame (a JSON line without its newline) and wake the subscriber.
    pub fn push(&self, line: String) {
        self.queue.lock().unwrap().push_back(line);
        self.notify.notify_one();
    }

    fn pop(&self) -> Option<String> {
        self.queue.lock().unwrap().pop_front()
    }

    /// Put back a frame whose write failed, at the head, so its order is kept.
    fn unpop(&self, line: String) {
        self.queue.lock().unwrap().push_front(line);
    }

    /// Take the subscriber slot; `false` when it is held.
    fn claim(&self) -> bool {
        !self.subscribed.swap(true, Ordering::SeqCst)
    }

    fn release(&self) {
        self.subscribed.store(false, Ordering::SeqCst);
    }
}

/// The JSON line the subscriber is sent for one bridge frame: `approval_request`
/// (the request as the app receives it; `server` left out when empty, as in
/// single-server mode) or `event` (the audit row the bridge fans in; absent fields
/// left out). Any other frame the bridge never sends: `None`.
pub fn frame_json(frame: &BrokerFrame) -> Option<String> {
    let value = match frame {
        BrokerFrame::ApprovalRequest(req) => approval_request_json(req),
        BrokerFrame::Event(ev) => event_json(ev),
        _ => return None,
    };
    Some(value.to_string())
}

fn approval_request_json(req: &ApprovalRequest) -> Value {
    let mut map = Map::new();
    map.insert("type".into(), json!("approval_request"));
    if let Ok(Value::Object(fields)) = serde_json::to_value(req) {
        map.extend(fields);
    }
    if req.server.is_empty() {
        map.remove("server");
    }
    Value::Object(map)
}

fn event_json(ev: &AuditEventFrame) -> Value {
    let mut map = Map::new();
    map.insert("type".into(), json!("event"));
    let optional = [
        ("kind", &ev.kind),
        ("server", &ev.server),
        ("shed", &ev.shed),
        ("ns", &ev.ns),
        ("op", &ev.op),
        ("detail", &ev.detail),
        ("code", &ev.code),
        ("reason", &ev.reason),
        ("approval", &ev.approval),
        ("request_id", &ev.request_id),
        ("ts", &ev.ts),
    ];
    for (key, value) in optional {
        if let Some(v) = value {
            map.insert(key.into(), json!(v));
        }
    }
    map.insert("result".into(), json!(ev.result));
    Value::Object(map)
}

/// Move every frame the bridge sends into the hub, until the bridge's sender is gone.
/// The synthesized `Connected` carries nothing the harness reports.
pub async fn pump(mut events: mpsc::UnboundedReceiver<BrokerEvent>, hub: Arc<Hub>) {
    while let Some(event) = events.recv().await {
        if let BrokerEvent::Frame(frame) = event {
            if let Some(line) = frame_json(&frame) {
                hub.push(line);
            }
        }
    }
}

/// What the control server answers with: the queued frames, a status snapshot
/// (rebuilt per request), and the bridge's responder.
pub struct Control {
    hub: Arc<Hub>,
    status: Box<dyn Fn() -> Value + Send + Sync>,
    responder: ResponderRef,
}

impl Control {
    pub fn new(
        hub: Arc<Hub>,
        status: Box<dyn Fn() -> Value + Send + Sync>,
        responder: ResponderRef,
    ) -> Control {
        Control {
            hub,
            status,
            responder,
        }
    }
}

/// Bind the control socket at `path`, mode 0600. An existing file there is an error:
/// the harness never takes over a socket it did not make.
pub fn bind(path: &Path) -> io::Result<UnixListener> {
    let listener = UnixListener::bind(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
    Ok(listener)
}

/// Accept connections until `stop` flips, each served on its own task.
pub async fn serve(listener: UnixListener, control: Arc<Control>, mut stop: watch::Receiver<bool>) {
    loop {
        let stream = tokio::select! {
            _ = stop.wait_for(|stopped| *stopped) => return,
            accepted = listener.accept() => match accepted {
                Ok((stream, _)) => stream,
                Err(_) => return,
            },
        };
        tokio::spawn(handle(stream, control.clone()));
    }
}

async fn handle(stream: UnixStream, control: Arc<Control>) {
    let (read, mut write) = stream.into_split();
    let mut lines = BufReader::new(read).lines();
    while let Ok(Some(line)) = lines.next_line().await {
        let line = line.trim();
        if line.is_empty() {
            continue;
        }
        let answer = match serde_json::from_str::<Value>(line) {
            Err(e) => json!({"error": format!("not JSON: {e}")}),
            Ok(request) => match request.get("op").and_then(Value::as_str) {
                Some("status") => (control.status)(),
                Some("decide") => decide(&control, &request),
                Some("subscribe") => {
                    if !control.hub.claim() {
                        json!({"error": "already subscribed"})
                    } else {
                        if write_line(&mut write, json!({"ok": true})).await.is_ok() {
                            stream_frames(&control.hub, lines, write).await;
                        }
                        control.hub.release();
                        return;
                    }
                }
                _ => json!({"error": "unknown op"}),
            },
        };
        if write_line(&mut write, answer).await.is_err() {
            return;
        }
    }
}

/// Write one line: `value` and a newline.
async fn write_line(write: &mut OwnedWriteHalf, value: impl std::fmt::Display) -> io::Result<()> {
    write.write_all(format!("{value}\n").as_bytes()).await
}

fn decide(control: &Control, request: &Value) -> Value {
    let Some(id) = request.get("id").and_then(Value::as_str) else {
        return json!({"error": "decide needs an id"});
    };
    let decision = match request.get("decision").and_then(Value::as_str) {
        Some("approve") => ApprovalDecision::Approve,
        Some("deny") => ApprovalDecision::Deny,
        _ => return json!({"error": "decision must be approve or deny"}),
    };
    control
        .responder
        .respond(id, decision, DecidedBy::User, None, None);
    json!({"ok": true})
}

/// Write the queued frames, then each new one, until the subscriber goes away (its
/// connection reads EOF or a write fails). Anything else it sends is ignored.
async fn stream_frames(
    hub: &Hub,
    mut lines: Lines<BufReader<OwnedReadHalf>>,
    mut write: OwnedWriteHalf,
) {
    loop {
        while let Some(frame) = hub.pop() {
            if write_line(&mut write, &frame).await.is_err() {
                hub.unpop(frame);
                return;
            }
        }
        tokio::select! {
            _ = hub.notify.notified() => {}
            read = lines.next_line() => match read {
                Ok(Some(_)) => {}
                _ => return,
            },
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    use shed_app::Responder;

    type Decisions = Arc<Mutex<Vec<(String, ApprovalDecision, DecidedBy)>>>;

    /// Records every decision it is handed.
    #[derive(Default)]
    struct Recorder {
        decisions: Decisions,
    }

    impl Responder for Recorder {
        fn respond(
            &self,
            request_id: &str,
            decision: ApprovalDecision,
            decided_by: DecidedBy,
            _scope: Option<&str>,
            _ttl: Option<&str>,
        ) {
            self.decisions
                .lock()
                .unwrap()
                .push((request_id.to_string(), decision, decided_by));
        }
    }

    struct Running {
        _dir: tempfile::TempDir,
        path: std::path::PathBuf,
        hub: Arc<Hub>,
        decisions: Decisions,
        _stop: watch::Sender<bool>,
    }

    async fn start() -> Running {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("control.sock");
        let listener = bind(&path).unwrap();
        let hub = Arc::new(Hub::default());
        let recorder = Recorder::default();
        let decisions = recorder.decisions.clone();
        let control = Arc::new(Control::new(
            hub.clone(),
            Box::new(|| json!({"schema": 1, "servers": []})),
            Arc::new(recorder),
        ));
        let (stop_tx, stop_rx) = watch::channel(false);
        tokio::spawn(serve(listener, control, stop_rx));
        Running {
            _dir: dir,
            path,
            hub,
            decisions,
            _stop: stop_tx,
        }
    }

    struct Client {
        lines: Lines<BufReader<OwnedReadHalf>>,
        write: OwnedWriteHalf,
    }

    impl Client {
        async fn connect(path: &Path) -> Client {
            let (read, write) = UnixStream::connect(path).await.unwrap().into_split();
            Client {
                lines: BufReader::new(read).lines(),
                write,
            }
        }

        async fn send(&mut self, request: Value) {
            write_line(&mut self.write, request).await.unwrap();
        }

        async fn read(&mut self) -> Value {
            let line = tokio::time::timeout(Duration::from_secs(5), self.lines.next_line())
                .await
                .expect("a line within 5 s")
                .unwrap()
                .expect("not EOF");
            serde_json::from_str(&line).unwrap()
        }

        async fn ask(&mut self, request: Value) -> Value {
            self.send(request).await;
            self.read().await
        }
    }

    fn frame(n: u32) -> String {
        json!({"type": "event", "n": n}).to_string()
    }

    #[tokio::test]
    async fn the_socket_is_owner_only() {
        let running = start().await;
        let mode = std::fs::metadata(&running.path)
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o600);
    }

    #[tokio::test]
    async fn status_is_answered_while_a_subscriber_is_attached() {
        let running = start().await;
        let mut subscriber = Client::connect(&running.path).await;
        assert_eq!(
            subscriber.ask(json!({"op": "subscribe"})).await,
            json!({"ok": true})
        );
        // Two other connections at once, each answered on its own.
        let mut a = Client::connect(&running.path).await;
        let mut b = Client::connect(&running.path).await;
        a.send(json!({"op": "status"})).await;
        b.send(json!({"op": "status"})).await;
        assert_eq!(b.read().await["schema"], 1);
        assert_eq!(a.read().await["schema"], 1);
        // The subscriber is still served.
        running.hub.push(frame(1));
        assert_eq!(subscriber.read().await["n"], 1);
    }

    #[tokio::test]
    async fn frames_queued_before_subscribe_arrive_in_order_then_new_ones() {
        let running = start().await;
        running.hub.push(frame(1));
        running.hub.push(frame(2));
        let mut subscriber = Client::connect(&running.path).await;
        assert_eq!(
            subscriber.ask(json!({"op": "subscribe"})).await,
            json!({"ok": true})
        );
        assert_eq!(subscriber.read().await["n"], 1);
        assert_eq!(subscriber.read().await["n"], 2);
        running.hub.push(frame(3));
        assert_eq!(subscriber.read().await["n"], 3);
    }

    #[tokio::test]
    async fn a_second_subscribe_is_refused_until_the_first_leaves() {
        let running = start().await;
        let mut first = Client::connect(&running.path).await;
        assert_eq!(
            first.ask(json!({"op": "subscribe"})).await,
            json!({"ok": true})
        );
        let mut second = Client::connect(&running.path).await;
        assert_eq!(
            second.ask(json!({"op": "subscribe"})).await,
            json!({"error": "already subscribed"})
        );
        // The refused connection still answers other requests.
        assert_eq!(second.ask(json!({"op": "status"})).await["schema"], 1);

        // The first subscriber leaves: frames queue up again, and a new subscriber
        // takes the slot and gets them.
        drop(first);
        running.hub.push(frame(7));
        let mut third = None;
        for _ in 0..100 {
            let mut c = Client::connect(&running.path).await;
            if c.ask(json!({"op": "subscribe"})).await == json!({"ok": true}) {
                third = Some(c);
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        let mut third = third.expect("the slot is freed when the subscriber leaves");
        assert_eq!(third.read().await["n"], 7);
    }

    #[tokio::test]
    async fn decide_hands_a_persons_decision_to_the_responder() {
        let running = start().await;
        let mut c = Client::connect(&running.path).await;
        let ok = json!({"ok": true});
        assert_eq!(
            c.ask(json!({"op": "decide", "id": "r1", "decision": "approve"}))
                .await,
            ok
        );
        assert_eq!(
            c.ask(json!({"op": "decide", "id": "r2", "decision": "deny"}))
                .await,
            ok
        );
        // Anything else is refused and reaches no responder.
        assert!(c
            .ask(json!({"op": "decide", "id": "r3", "decision": "maybe"}))
            .await
            .get("error")
            .is_some());
        assert!(c
            .ask(json!({"op": "decide", "decision": "approve"}))
            .await
            .get("error")
            .is_some());
        assert_eq!(
            *running.decisions.lock().unwrap(),
            vec![
                ("r1".to_string(), ApprovalDecision::Approve, DecidedBy::User),
                ("r2".to_string(), ApprovalDecision::Deny, DecidedBy::User),
            ]
        );
        assert_eq!(
            c.ask(json!({"op": "nope"})).await,
            json!({"error": "unknown op"})
        );
    }

    #[test]
    fn frames_carry_what_the_app_is_sent() {
        let req = ApprovalRequest {
            id: "a1".into(),
            ts: "2026-01-01T00:00:00Z".into(),
            server: String::new(),
            namespace: "ssh-agent".into(),
            op: "sign".into(),
            shed: "web".into(),
            detail: "SSH sign request".into(),
            expires_at: "2026-01-01T00:00:25Z".into(),
        };
        let line = frame_json(&BrokerFrame::ApprovalRequest(req.clone())).unwrap();
        let value: Value = serde_json::from_str(&line).unwrap();
        assert_eq!(
            value,
            json!({
                "type": "approval_request", "id": "a1", "ts": "2026-01-01T00:00:00Z",
                "namespace": "ssh-agent", "op": "sign", "shed": "web",
                "detail": "SSH sign request", "expires_at": "2026-01-01T00:00:25Z",
            }),
            "single-server: no server"
        );
        let named = ApprovalRequest {
            server: "server-b".into(),
            ..req
        };
        let line = frame_json(&BrokerFrame::ApprovalRequest(named)).unwrap();
        assert_eq!(
            serde_json::from_str::<Value>(&line).unwrap()["server"],
            "server-b"
        );

        let ev = AuditEventFrame {
            kind: None,
            server: None,
            shed: Some("web".into()),
            ns: Some("ssh-agent".into()),
            op: Some("sign".into()),
            result: "denied".into(),
            detail: None,
            code: None,
            reason: None,
            approval: Some("shed-desktop".into()),
            request_id: None,
            ts: Some("2026-01-01T00:00:01Z".into()),
        };
        let line = frame_json(&BrokerFrame::Event(ev)).unwrap();
        assert_eq!(
            serde_json::from_str::<Value>(&line).unwrap(),
            json!({
                "type": "event", "shed": "web", "ns": "ssh-agent", "op": "sign",
                "result": "denied", "approval": "shed-desktop", "ts": "2026-01-01T00:00:01Z",
            })
        );
        assert_eq!(frame_json(&BrokerFrame::Ping { id: "p".into() }), None);
    }
}
