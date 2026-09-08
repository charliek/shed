//! The live smoke — the one test that talks to a REAL `opencode`.
//!
//! **Gated by `SHED_OPENCODE_LIVE=1`, and never run in CI.** Without the
//! variable it prints why it is skipping and passes, so a normal
//! `cargo test -p shed-opencode` is unaffected. It spends real money (one or
//! two turns on the cheapest model the host has configured) and depends on the
//! host's opencode install and credentials, which is exactly why it is opt-in.
//!
//! What it proves that the `FakeOpencode` suite cannot: that the routes, the
//! query parameters, the event vocabulary and the approval shapes this adapter
//! assumes are the ones opencode 1.18.29 actually serves.
//!
//! ```bash
//! SHED_OPENCODE_LIVE=1 cargo test -p shed-opencode --test live -- --nocapture
//! # and, to refresh the recorded wire fixtures:
//! SHED_OPENCODE_LIVE=1 SHED_OPENCODE_RECORD=1 cargo test -p shed-opencode --test live -- --nocapture
//! ```
//!
//! # The scratch project
//!
//! opencode reads `opencode.json` from the project root, so the test writes one
//! into a scratch directory and runs `opencode serve` with that as its cwd. The
//! host's GLOBAL config dir is deliberately left alone: the credentials that let
//! a prompt reach a model live there, and a scratch `OPENCODE_CONFIG_DIR` would
//! take them away. Nothing the test writes escapes the scratch directory.

use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use futures_util::StreamExt as _;
use serde_json::{json, Value};
use shed_core::lane::{
    AgentLane, LaneAnswer, LaneApprovalKind, LaneDecision, LaneEvent, LaneSubscription, SendMode,
};
use shed_core::rc::RcActivity;
use shed_opencode::OpencodeClient;
use tokio::io::{AsyncBufReadExt as _, BufReader};
use tokio::sync::mpsc::UnboundedReceiver;

/// The model the plan names as the cheapest known free option. If the host does
/// not offer it, the first model `GET /config/providers` lists is used instead;
/// if it lists none, the test skips rather than failing on someone's unconfigured
/// machine.
const PREFERRED_MODEL: &str = "opencode/muse-spark-1.3-contributor-free";

/// The opencode this smoke was written against. It names the fixtures directory
/// and the README, so a later run on a newer opencode records beside it rather
/// than over it.
const RECORDED_VERSION: &str = "1.18.29";

/// Every wait against a real agent. Model latency, not loopback latency.
const TURN_DEADLINE: Duration = Duration::from_secs(180);

#[tokio::test(flavor = "multi_thread")]
async fn live_smoke() {
    if std::env::var("SHED_OPENCODE_LIVE").as_deref() != Ok("1") {
        eprintln!(
            "SKIP live_smoke: set SHED_OPENCODE_LIVE=1 to run it. It spawns a real \
             `opencode serve`, spends real tokens on the cheapest configured model, \
             and is never run in CI."
        );
        return;
    }
    let scratch = Scratch::new("shed-opencode-live");

    // --- phase 1: which model can this host afford? -----------------------
    scratch.write_config(&json!({
        "$schema": "https://opencode.ai/config.json",
        // Verified against the artifact's `Config` schema: `permission` is a
        // `PermissionConfig`, whose `bash` key takes a `PermissionRuleConfig`
        // whose action form is the enum `ask|allow|deny`.
        "permission": { "bash": "ask" },
    }));
    let probe = Opencode::spawn(scratch.path()).await;
    let model = match choose_model(probe.base_url()).await {
        Some(model) => model,
        None => {
            eprintln!(
                "SKIP live_smoke: this host's opencode offers no models \
                 (GET /config/providers was empty). Log in with `opencode auth login` first."
            );
            return;
        }
    };
    eprintln!("live_smoke: driving {model}");
    drop(probe);

    // --- phase 2: the real run --------------------------------------------
    // The model goes in the config rather than on each prompt, because the lane
    // contract has no model parameter — and it should not: choosing a model is
    // the agent's configuration, not a transport concern.
    scratch.write_config(&json!({
        "$schema": "https://opencode.ai/config.json",
        "permission": { "bash": "ask" },
        "model": model,
    }));
    let server = Opencode::spawn(scratch.path()).await;
    let lane = OpencodeClient::new(server.base_url(), None).expect("the client builds");

    // A raw recorder alongside the lane, so `SHED_OPENCODE_RECORD=1` captures
    // the wire EXACTLY as opencode wrote it rather than as the fold read it.
    let recorder = Recorder::start(server.base_url(), scratch.path()).await;

    // create + subscribe.
    let created = lane
        .create(
            scratch.path().to_str().expect("a utf-8 scratch path"),
            "Reply with the single word: pong",
        )
        .await
        .expect("create");
    eprintln!("live_smoke: session {}", created.id);
    let subscription: LaneSubscription =
        lane.subscribe(&created.id, None).await.expect("subscribe");
    let (mut rx, _stop) = subscription.into_parts();

    // The seed bracket, live.
    let seed = until_ready(&mut rx).await;
    assert!(
        matches!(seed.first(), Some(LaneEvent::Reset { generation: 1, .. })),
        "the stream opens with a Reset: {seed:#?}"
    );

    // An assistant row, then the turn settling to needs_input.
    let said = wait_for_message(&mut rx, |m| m.role == "assistant").await;
    eprintln!("live_smoke: assistant said {:?}", said.text);
    let settled = wait_for_session(&mut rx, |s| s.activity == RcActivity::NeedsInput).await;
    assert_eq!(settled.activity, RcActivity::NeedsInput);

    // --- a permission, induced by the `bash: ask` policy -------------------
    lane.send(
        &created.id,
        "Use the bash tool to run exactly: echo shed-lane-live",
        SendMode::Queue,
    )
    .await
    .expect("send");
    let permission = wait_for_approval(&mut rx, |a| a.kind == LaneApprovalKind::Permission).await;
    eprintln!(
        "live_smoke: permission {} — {}",
        permission.id, permission.title
    );
    assert!(
        permission.status.is_pending(),
        "a fresh ask is answerable: {permission:?}"
    );
    lane.answer(
        &created.id,
        &permission.id,
        LaneAnswer::Permission {
            decision: LaneDecision::AllowOnce,
        },
    )
    .await
    .expect("answering the permission");
    // The tool result proves the approval actually unblocked the turn.
    let tool = wait_for_message(&mut rx, |m| m.msg_type == "tool_result").await;
    eprintln!("live_smoke: tool result {:?}", tool.text);
    wait_for_session(&mut rx, |s| s.activity == RcActivity::NeedsInput).await;

    // --- a question, if the agent can be induced to ask one ---------------
    lane.send(
        &created.id,
        "Use your question tool to ask me whether to proceed, offering exactly two options: yes and no.",
        SendMode::Queue,
    )
    .await
    .expect("send");
    match try_wait_for_approval(&mut rx, Duration::from_secs(90), |a| {
        a.kind == LaneApprovalKind::Question
    })
    .await
    {
        Some(question) => {
            eprintln!("live_smoke: question {} — {}", question.id, question.title);
            let first = question
                .questions
                .first()
                .expect("a question approval carries its questions");
            let answer = first
                .options
                .first()
                .map(|o| o.id.clone())
                .unwrap_or_else(|| "yes".to_string());
            lane.answer(
                &created.id,
                &question.id,
                LaneAnswer::Question {
                    answers: vec![vec![answer]],
                },
            )
            .await
            .expect("answering the question");
        }
        None => eprintln!(
            "live_smoke: NOTE the agent did not raise a question within 90s. The question \
             path stays covered by the hand-authored fold inputs (plan 015 §11.6's coverage \
             seam), and this run records nothing for it."
        ),
    }

    // --- cancel a turn in flight ------------------------------------------
    lane.send(
        &created.id,
        "Count slowly from one to five hundred, one number per line.",
        SendMode::Queue,
    )
    .await
    .expect("send");
    wait_for_session(&mut rx, |s| s.activity == RcActivity::Working).await;
    lane.cancel(&created.id).await.expect("cancel");
    let after_cancel = wait_for_session(&mut rx, |s| s.activity != RcActivity::Working).await;
    eprintln!(
        "live_smoke: after cancel, activity {:?}",
        after_cancel.activity
    );

    // --- clean up ----------------------------------------------------------
    // `DELETE /session/{id}` is not a contract verb (nothing in `AgentLane`
    // deletes), so the teardown goes direct.
    let deleted = reqwest::Client::builder()
        .no_proxy()
        .build()
        .expect("a raw client")
        .delete(
            server
                .base_url()
                .join(&format!("session/{}", created.id))
                .expect("url"),
        )
        .send()
        .await
        .expect("delete");
    assert!(deleted.status().is_success(), "the session is deleted");

    recorder.finish(&model).await;
}

// ---- the scratch project -------------------------------------------------

/// A scratch directory that removes itself. Hand-rolled rather than `tempfile`
/// so this crate takes no dev-dependency the plan did not name.
struct Scratch(PathBuf);

impl Scratch {
    fn new(tag: &str) -> Scratch {
        let path = std::env::temp_dir().join(format!("{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&path);
        std::fs::create_dir_all(&path).expect("creating the scratch project");
        Scratch(path)
    }

    fn path(&self) -> &Path {
        &self.0
    }

    fn write_config(&self, config: &Value) {
        std::fs::write(
            self.0.join("opencode.json"),
            serde_json::to_vec_pretty(config).expect("the config serializes"),
        )
        .expect("writing opencode.json");
    }
}

impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// A spawned `opencode serve`, killed on drop.
struct Opencode {
    child: tokio::process::Child,
    base: reqwest::Url,
}

impl Opencode {
    async fn spawn(cwd: &Path) -> Opencode {
        let mut child = tokio::process::Command::new("opencode")
            .arg("serve")
            .arg("--port")
            .arg("0")
            .arg("--hostname")
            .arg("127.0.0.1")
            .current_dir(cwd)
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .kill_on_drop(true)
            .spawn()
            .expect("`opencode serve` starts — is opencode on PATH?");

        // `--port 0` means the port is only knowable from what the server
        // prints, so the URL is read off stdout rather than guessed.
        let stdout = child.stdout.take().expect("opencode's stdout is piped");
        let mut lines = BufReader::new(stdout).lines();
        let deadline = tokio::time::Instant::now() + Duration::from_secs(60);
        let base = loop {
            let line = tokio::time::timeout_at(deadline, lines.next_line())
                .await
                .expect("opencode printed its listen URL within 60s")
                .expect("reading opencode's stdout")
                .expect("opencode exited before printing a listen URL");
            eprintln!("opencode: {line}");
            if let Some(url) = line.split_whitespace().find(|w| w.starts_with("http://")) {
                break url
                    .trim_end_matches(['.', ','])
                    .parse::<reqwest::Url>()
                    .expect("opencode printed a URL");
            }
        };
        Opencode { child, base }
    }

    fn base_url(&self) -> reqwest::Url {
        self.base.clone()
    }
}

impl Drop for Opencode {
    fn drop(&mut self) {
        let _ = self.child.start_kill();
    }
}

/// The cheapest model the host offers, as opencode's `provider/model` string.
async fn choose_model(base: reqwest::Url) -> Option<String> {
    let client = reqwest::Client::builder().no_proxy().build().ok()?;
    let body: Value = client
        .get(base.join("config/providers").ok()?)
        .send()
        .await
        .ok()?
        .json()
        .await
        .ok()?;
    let providers = body.get("providers")?.as_array()?;
    let mut offered = Vec::new();
    for provider in providers {
        let provider_id = provider.get("id")?.as_str()?;
        if let Some(models) = provider.get("models").and_then(Value::as_object) {
            for model_id in models.keys() {
                offered.push(format!("{provider_id}/{model_id}"));
            }
        }
    }
    if offered.iter().any(|m| m == PREFERRED_MODEL) {
        return Some(PREFERRED_MODEL.to_string());
    }
    offered.into_iter().next()
}

// ---- the wire recorder ---------------------------------------------------

/// Appends every raw `/event` `data:` payload to a file, so
/// `SHED_OPENCODE_RECORD=1` captures the wire as opencode wrote it. A no-op
/// unless that variable is set.
struct Recorder {
    task: Option<tokio::task::JoinHandle<()>>,
    path: PathBuf,
    frames: PathBuf,
}

impl Recorder {
    async fn start(base: reqwest::Url, directory: &Path) -> Recorder {
        let dir = fixtures_dir();
        let frames = dir.join("event-frames.jsonl");
        if std::env::var("SHED_OPENCODE_RECORD").as_deref() != Ok("1") {
            return Recorder {
                task: None,
                path: dir,
                frames,
            };
        }
        std::fs::create_dir_all(&dir).expect("creating the fixtures directory");
        let mut file = std::fs::File::create(&frames).expect("creating the frame log");

        let mut url = base.join("event").expect("url");
        url.query_pairs_mut()
            .append_pair("directory", directory.to_str().expect("a utf-8 path"));
        let client = reqwest::Client::builder()
            .no_proxy()
            .connect_timeout(Duration::from_secs(3))
            .build()
            .expect("a raw client");
        let task = tokio::spawn(async move {
            let Ok(resp) = client.get(url).send().await else {
                return;
            };
            let mut parser = shed_core::sse::SseParser::new();
            let mut stream = resp.bytes_stream();
            while let Some(Ok(chunk)) = stream.next().await {
                for event in parser.feed(&chunk) {
                    let _ = writeln!(file, "{}", event.data);
                }
                let _ = file.flush();
            }
        });
        Recorder {
            task: Some(task),
            path: dir,
            frames,
        }
    }

    async fn finish(self, model: &str) {
        let Some(task) = self.task else { return };
        // Let whatever is still on the wire land, then stop reading.
        tokio::time::sleep(Duration::from_millis(500)).await;
        task.abort();

        let version = std::process::Command::new("opencode")
            .arg("--version")
            .output()
            .ok()
            .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
            .unwrap_or_else(|| RECORDED_VERSION.to_string());
        let readme = format!(
            "# opencode {version} wire fixtures\n\
             \n\
             Recorded by `crates/shed-opencode/tests/live.rs` on {date} with:\n\
             \n\
             ```bash\n\
             SHED_OPENCODE_LIVE=1 SHED_OPENCODE_RECORD=1 \\\n\
             \x20 cargo test -p shed-opencode --test live -- --nocapture\n\
             ```\n\
             \n\
             - `opencode --version`: `{version}`\n\
             - server: `opencode serve --port 0 --hostname 127.0.0.1` in a scratch project whose\n\
             \x20 `opencode.json` sets `permission.bash = \"ask\"` and `model = \"{model}\"`.\n\
             - `event-frames.jsonl`: one line per `/event` SSE `data:` payload, in arrival order,\n\
             \x20 verbatim.\n\
             \n\
             ## The keep-alive\n\
             \n\
             opencode's `/event` has **no `server.heartbeat` variant**. The stream opens with\n\
             `server.connected` and its only idle traffic is whatever the effect encoder emits —\n\
             comment lines (`: …`), which carry no event and therefore appear NOWHERE in this\n\
             file. That is why `shed-opencode`'s stall timer counts BYTES received rather than\n\
             events decoded: a healthy but quiet stream produces zero lines here.\n\
             \n\
             Re-record on an opencode upgrade into a directory named for the new version; do not\n\
             overwrite this one.\n",
            version = version,
            date = today(),
            model = model,
        );
        std::fs::write(self.path.join("README.md"), readme).expect("writing the fixtures README");
        eprintln!(
            "live_smoke: recorded {} into {}",
            self.frames.display(),
            self.path.display()
        );
    }
}

/// Today, as `YYYY-MM-DD`. `chrono`'s `clock` feature is deliberately not
/// enabled in this crate (it would pull `iana-time-zone` into every consumer),
/// so the wall clock comes from `SystemTime` and chrono only formats it.
fn today() -> String {
    let since_epoch = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default();
    chrono::DateTime::from_timestamp(since_epoch.as_secs() as i64, 0)
        .map(|t| t.format("%Y-%m-%d").to_string())
        .unwrap_or_else(|| "unknown".to_string())
}

fn fixtures_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("fixtures")
        .join(RECORDED_VERSION)
}

// ---- bounded waits -------------------------------------------------------

async fn next_event(rx: &mut UnboundedReceiver<LaneEvent>, what: &str) -> LaneEvent {
    match tokio::time::timeout(TURN_DEADLINE, rx.recv()).await {
        Err(_) => panic!("timed out after {TURN_DEADLINE:?} waiting for {what}"),
        Ok(None) => panic!("the lane stream ENDED while waiting for {what}"),
        Ok(Some(ev)) => ev,
    }
}

async fn until_ready(rx: &mut UnboundedReceiver<LaneEvent>) -> Vec<LaneEvent> {
    let mut out = Vec::new();
    loop {
        let ev = next_event(rx, "the seed's Ready").await;
        let done = matches!(ev, LaneEvent::Ready { .. } | LaneEvent::Down { .. });
        out.push(ev);
        if done {
            return out;
        }
    }
}

async fn wait_for_message(
    rx: &mut UnboundedReceiver<LaneEvent>,
    want: impl Fn(&shed_core::rc::RcFeedMessage) -> bool,
) -> shed_core::rc::RcFeedMessage {
    loop {
        if let LaneEvent::Message { message, .. } = next_event(rx, "a transcript row").await {
            if want(&message) {
                return message;
            }
        }
    }
}

async fn wait_for_session(
    rx: &mut UnboundedReceiver<LaneEvent>,
    want: impl Fn(&shed_core::lane::LaneSession) -> bool,
) -> shed_core::lane::LaneSession {
    loop {
        if let LaneEvent::Session { session } = next_event(rx, "a session row").await {
            if want(&session) {
                return session;
            }
        }
    }
}

async fn wait_for_approval(
    rx: &mut UnboundedReceiver<LaneEvent>,
    want: impl Fn(&shed_core::lane::LaneApproval) -> bool,
) -> shed_core::lane::LaneApproval {
    try_wait_for_approval(rx, TURN_DEADLINE, want)
        .await
        .expect("an approval within the turn deadline")
}

/// `None` when nothing matching arrived inside `deadline` — the question leg
/// tolerates that, the permission leg does not.
async fn try_wait_for_approval(
    rx: &mut UnboundedReceiver<LaneEvent>,
    deadline: Duration,
    want: impl Fn(&shed_core::lane::LaneApproval) -> bool,
) -> Option<shed_core::lane::LaneApproval> {
    let until = tokio::time::Instant::now() + deadline;
    loop {
        let event = tokio::time::timeout_at(until, rx.recv()).await.ok()??;
        if let LaneEvent::Approval { approval } = event {
            if want(&approval) && approval.status.is_pending() {
                return Some(approval);
            }
        }
    }
}
