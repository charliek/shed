//! An in-process fake of the server side of the delivery transport, for the client's
//! tests: plain HTTP/1.1 on 127.0.0.1, one connection per request (`Connection: close`).
//! A stream is held until the test ends it; what is written on it is what the test
//! pushes. Every stream opened and every answer posted is recorded.

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio::sync::{mpsc, watch};

/// One stream the fake opened (or refused).
#[derive(Debug, Clone)]
pub struct StreamRec {
    /// The id it named, or `None` for a refused one.
    pub id: Option<String>,
    pub machine_id: Option<String>,
    pub machine_name: Option<String>,
    /// The client certificate the connection presented, by the label it was given
    /// ([`Fake::start_tls`]); `None` on plain HTTP or with none.
    pub peer: Option<String>,
    pub at: std::time::Instant,
}

/// One answer posted.
#[derive(Debug, Clone)]
pub struct PostRec {
    pub request_id: String,
    pub stream: Option<String>,
    pub machine_id: Option<String>,
    pub body: Vec<u8>,
    pub status: u16,
    pub peer: Option<String>,
}

/// A scripted answer to the next stream or post: a status, a body and extra headers.
#[derive(Debug, Clone)]
pub struct Scripted {
    /// `0`: accept the request and never answer it.
    pub status: u16,
    pub body: String,
    pub headers: Vec<(String, String)>,
}

#[derive(Default)]
struct State {
    stream_script: VecDeque<Scripted>,
    post_script: VecDeque<Scripted>,
    streams: Vec<StreamRec>,
    posts: Vec<PostRec>,
    live: Option<mpsc::UnboundedSender<Option<String>>>,
    counter: u64,
}

/// A TLS fake's config, and the label of each client certificate (by its DER).
type TlsSetup = (Arc<rustls::ServerConfig>, Vec<(Vec<u8>, String)>);

/// The fake server.
pub struct Fake {
    pub url: String,
    state: Arc<Mutex<State>>,
    changed: watch::Sender<u64>,
}

impl Fake {
    pub async fn start() -> Fake {
        Self::start_with("127.0.0.1:0", None).await
    }

    /// A plain-HTTP fake bound to `addr` (`ip:port`), or `None` when it cannot be bound.
    /// (Its one user binds 127.0.0.3, which only Linux has.)
    #[cfg(target_os = "linux")]
    pub async fn start_at(addr: &str) -> Option<Fake> {
        TcpListener::bind(addr).await.ok()?;
        Some(Self::start_with(addr, None).await)
    }

    /// A fake on `https://127.0.0.1` with `config` (its client-certificate verifier
    /// decides who may connect); each connection's client certificate is recorded by the
    /// label `labels` gives its DER.
    pub async fn start_tls(
        config: Arc<rustls::ServerConfig>,
        labels: Vec<(Vec<u8>, String)>,
    ) -> Fake {
        Self::start_with("127.0.0.1:0", Some((config, labels))).await
    }

    async fn start_with(addr: &str, tls: Option<TlsSetup>) -> Fake {
        let listener = TcpListener::bind(addr).await.unwrap();
        let scheme = if tls.is_some() { "https" } else { "http" };
        let url = format!("{scheme}://{}", listener.local_addr().unwrap());
        let state = Arc::new(Mutex::new(State::default()));
        let (changed, _) = watch::channel(0u64);
        let fake = Fake {
            url,
            state: state.clone(),
            changed: changed.clone(),
        };
        let tls = tls.map(|(cfg, labels)| (tokio_rustls::TlsAcceptor::from(cfg), Arc::new(labels)));
        tokio::spawn(async move {
            while let Ok((conn, _)) = listener.accept().await {
                let (state, changed) = (state.clone(), changed.clone());
                match &tls {
                    None => {
                        tokio::spawn(serve(conn, None, state, changed));
                    }
                    Some((acceptor, labels)) => {
                        let (acceptor, labels) = (acceptor.clone(), labels.clone());
                        tokio::spawn(async move {
                            let Ok(conn) = acceptor.accept(conn).await else {
                                return;
                            };
                            let peer = conn
                                .get_ref()
                                .1
                                .peer_certificates()
                                .and_then(|c| c.first())
                                .and_then(|der| {
                                    labels
                                        .iter()
                                        .find(|(d, _)| d.as_slice() == der.as_ref())
                                        .map(|(_, l)| l.clone())
                                });
                            serve(conn, peer, state, changed).await;
                        });
                    }
                }
            }
        });
        fake
    }

    /// The next stream is answered with `status` and `body` (and `headers`).
    pub fn refuse_next_stream(&self, status: u16, body: &str, headers: &[(&str, &str)]) {
        self.state
            .lock()
            .unwrap()
            .stream_script
            .push_back(scripted(status, body, headers));
    }

    /// The next answer post is answered with `status` and `body`.
    pub fn script_post(&self, status: u16, body: &str) {
        self.state
            .lock()
            .unwrap()
            .post_script
            .push_back(scripted(status, body, &[]));
    }

    /// Writes `text` on the live stream.
    pub fn push(&self, text: &str) {
        let st = self.state.lock().unwrap();
        st.live
            .as_ref()
            .expect("a live stream")
            .send(Some(text.to_string()))
            .unwrap();
    }

    /// Ends the live stream (the server closes it).
    pub fn end_stream(&self) {
        if let Some(live) = self.state.lock().unwrap().live.take() {
            let _ = live.send(None);
        }
    }

    pub fn streams(&self) -> Vec<StreamRec> {
        self.state.lock().unwrap().streams.clone()
    }

    pub fn posts(&self) -> Vec<PostRec> {
        self.state.lock().unwrap().posts.clone()
    }

    /// Waits (at most 10 s) until `cond` holds of the state.
    pub async fn wait(&self, what: &str, cond: impl Fn(&[StreamRec], &[PostRec]) -> bool) {
        let mut rx = self.changed.subscribe();
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        loop {
            {
                let st = self.state.lock().unwrap();
                if cond(&st.streams, &st.posts) {
                    return;
                }
            }
            if tokio::time::timeout_at(deadline, rx.changed())
                .await
                .is_err()
            {
                panic!("timed out waiting for {what}");
            }
        }
    }

    /// Waits until `n` streams were opened (refused ones not counted) and the last is
    /// live.
    pub async fn wait_streams(&self, n: usize) -> String {
        self.wait(&format!("{n} streams"), |s, _| {
            s.iter().filter(|r| r.id.is_some()).count() >= n
        })
        .await;
        let st = self.state.lock().unwrap();
        st.streams
            .iter()
            .filter_map(|r| r.id.clone())
            .nth(n - 1)
            .unwrap()
    }
}

fn scripted(status: u16, body: &str, headers: &[(&str, &str)]) -> Scripted {
    Scripted {
        status,
        body: body.to_string(),
        headers: headers
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect(),
    }
}

struct Head {
    method: String,
    path: String,
    headers: Vec<(String, String)>,
}

impl Head {
    fn header(&self, name: &str) -> Option<String> {
        self.headers
            .iter()
            .find(|(k, _)| k.eq_ignore_ascii_case(name))
            .map(|(_, v)| v.clone())
    }
}

async fn read_head<S: AsyncRead + Unpin>(conn: &mut S) -> Option<(Head, Vec<u8>)> {
    let mut buf = Vec::new();
    let mut byte = [0u8; 1024];
    let end = loop {
        let n = conn.read(&mut byte).await.ok()?;
        if n == 0 {
            return None;
        }
        buf.extend_from_slice(&byte[..n]);
        if let Some(i) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
            break i;
        }
    };
    let text = String::from_utf8_lossy(&buf[..end]).into_owned();
    let mut lines = text.split("\r\n");
    let mut first = lines.next()?.split(' ');
    let method = first.next()?.to_string();
    let path = first.next()?.to_string();
    let headers = lines
        .filter_map(|l| l.split_once(':'))
        .map(|(k, v)| (k.trim().to_string(), v.trim().to_string()))
        .collect();
    Some((
        Head {
            method,
            path,
            headers,
        },
        buf[end + 4..].to_vec(),
    ))
}

async fn respond<S: AsyncWrite + Unpin>(conn: &mut S, s: &Scripted) {
    let mut out = format!(
        "HTTP/1.1 {} X\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n",
        s.status,
        s.body.len()
    );
    for (k, v) in &s.headers {
        out.push_str(&format!("{k}: {v}\r\n"));
    }
    out.push_str("\r\n");
    out.push_str(&s.body);
    let _ = conn.write_all(out.as_bytes()).await;
    let _ = conn.shutdown().await;
}

async fn serve<S: AsyncRead + AsyncWrite + Unpin>(
    mut conn: S,
    peer: Option<String>,
    state: Arc<Mutex<State>>,
    changed: watch::Sender<u64>,
) {
    let Some((head, mut rest)) = read_head(&mut conn).await else {
        return;
    };
    let bump = || changed.send_modify(|n| *n += 1);
    if head.method == "GET" && head.path == super::STREAM_PATH {
        let scripted = state.lock().unwrap().stream_script.pop_front();
        let rec = |id: Option<String>| StreamRec {
            id,
            machine_id: head.header(super::HEADER_MACHINE_ID),
            machine_name: head.header(super::HEADER_MACHINE_NAME),
            peer: peer.clone(),
            at: std::time::Instant::now(),
        };
        if let Some(s) = scripted {
            state.lock().unwrap().streams.push(rec(None));
            bump();
            if s.status == 0 {
                // Never answers: the connection is held until the test ends.
                std::future::pending::<()>().await;
            }
            respond(&mut conn, &s).await;
            return;
        }
        let (tx, mut rx) = mpsc::unbounded_channel::<Option<String>>();
        let id = {
            let mut st = state.lock().unwrap();
            st.counter += 1;
            let id = format!("{:032x}", 0xab00 + st.counter);
            st.streams.push(rec(Some(id.clone())));
            st.live = Some(tx);
            id
        };
        let header = format!(
            "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-store\r\n{}: {id}\r\nConnection: close\r\n\r\n",
            super::HEADER_STREAM
        );
        if conn.write_all(header.as_bytes()).await.is_err() {
            return;
        }
        bump();
        while let Some(Some(text)) = rx.recv().await {
            if conn.write_all(text.as_bytes()).await.is_err() {
                break;
            }
        }
        let _ = conn.shutdown().await;
        return;
    }
    if head.method == "POST" {
        if let Some(request_id) = head.path.strip_prefix(super::ANSWER_PATH_PREFIX) {
            let len: usize = head
                .header("Content-Length")
                .and_then(|v| v.parse().ok())
                .unwrap_or(0);
            while rest.len() < len {
                let mut more = vec![0u8; len - rest.len()];
                match conn.read(&mut more).await {
                    Ok(0) | Err(_) => return,
                    Ok(n) => rest.extend_from_slice(&more[..n]),
                }
            }
            let s = state
                .lock()
                .unwrap()
                .post_script
                .pop_front()
                .unwrap_or_else(|| scripted(204, "", &[]));
            state.lock().unwrap().posts.push(PostRec {
                request_id: request_id.to_string(),
                stream: head.header(super::HEADER_STREAM),
                machine_id: head.header(super::HEADER_MACHINE_ID),
                body: rest,
                status: s.status,
                peer: peer.clone(),
            });
            bump();
            if s.status == 204 {
                let _ = conn
                    .write_all(b"HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
                    .await;
                let _ = conn.shutdown().await;
            } else {
                respond(&mut conn, &s).await;
            }
            return;
        }
    }
    respond(&mut conn, &scripted(404, "404 page not found\n", &[])).await;
}
