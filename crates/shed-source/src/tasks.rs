//! The per-server tasks (plan 039 §3.7, commit 2): one delivery stream per configured
//! server, started and stopped with the server list ([`SourceTasks::reconcile`]), each
//! reporting a small [`StreamStatus`].
//!
//! A server whose target changes (any field: its URL, pin, SSH endpoint, user, key) is
//! restarted with a new client and a new credential; one that leaves the list is
//! stopped. [`SourceTasks::stop`] is non-blocking: it flips every task's stop signal, and
//! [`SourceTasks::join`] waits (bounded) for them to end.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde::Serialize;
use tokio::sync::watch;
use tokio::task::JoinHandle;

use crate::delivery::{
    new_status, Answerer, DeliveryClient, ErrorClass, SharedStatus, StreamCredentials, StreamState,
    StreamStatus, Timing,
};
use crate::machine::MachineIdentity;
use crate::target::ServerTarget;

/// The credential for a server's stream (see [`crate::credentials::SourceCredentials`]
/// and [`crate::delivery::NoCredentials`]), given the stream task's stop signal: a
/// credential that writes the shared store is fenced by it, so a task stopped or
/// replaced writes nothing more.
pub type CredentialsFor =
    Arc<dyn Fn(&ServerTarget, watch::Receiver<bool>) -> Arc<dyn StreamCredentials> + Send + Sync>;

/// One server's row.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct ServerStatus {
    /// The server's name in the shed CLI config.
    pub server: String,
    #[serde(flatten)]
    pub status: StreamStatus,
}

struct Running {
    target: ServerTarget,
    status: SharedStatus,
    stop: watch::Sender<bool>,
    task: Option<JoinHandle<()>>,
}

/// The source's streams, one per server.
pub struct SourceTasks {
    machine: MachineIdentity,
    answerer: Arc<dyn Answerer>,
    credentials: CredentialsFor,
    timing: Timing,
    handle: tokio::runtime::Handle,
    running: Mutex<BTreeMap<String, Running>>,
    stopped: Mutex<bool>,
}

impl SourceTasks {
    /// Tasks for `machine`, answering with `answerer`, each server's credential from
    /// `credentials`, spawned on `handle`. None runs until [`Self::reconcile`].
    pub fn new(
        handle: tokio::runtime::Handle,
        machine: MachineIdentity,
        answerer: Arc<dyn Answerer>,
        credentials: CredentialsFor,
    ) -> Self {
        SourceTasks {
            machine,
            answerer,
            credentials,
            timing: Timing::default(),
            handle,
            running: Mutex::new(BTreeMap::new()),
            stopped: Mutex::new(false),
        }
    }

    /// The same tasks on other clocks (tests).
    pub fn with_timing(mut self, timing: Timing) -> Self {
        self.timing = timing;
        self
    }

    /// Runs one stream per target of `targets` (by name): a new server starts, a changed
    /// one restarts, one no longer listed stops. After [`Self::stop`] nothing starts.
    ///
    /// Lock order, here and in [`Self::restart`] and [`Self::stop`]: `stopped`, then
    /// `running`, the first held across the map's change, so no task is added once
    /// [`Self::stop`] has run.
    pub fn reconcile(&self, targets: &[ServerTarget]) {
        let stopped = self.stopped.lock().unwrap_or_else(|e| e.into_inner());
        if *stopped {
            return;
        }
        let mut running = self.running.lock().unwrap_or_else(|e| e.into_inner());
        running.retain(|name, r| {
            let keep = targets.iter().any(|t| &t.name == name && *t == r.target);
            if !keep {
                let _ = r.stop.send(true);
            }
            keep
        });
        for t in targets {
            if running.contains_key(&t.name) {
                continue;
            }
            running.insert(t.name.clone(), self.spawn(t));
        }
    }

    fn spawn(&self, target: &ServerTarget) -> Running {
        let status = new_status();
        let (stop, rx) = watch::channel(false);
        let creds = (self.credentials)(target, rx.clone());
        let task = match DeliveryClient::new(target, self.machine.clone(), creds) {
            Ok(client) => {
                let client = client.with_timing(self.timing);
                let answerer = self.answerer.clone();
                let st = status.clone();
                Some(
                    self.handle
                        .spawn(async move { client.run(answerer, st, rx).await }),
                )
            }
            Err(_) => {
                let mut s = status.lock().unwrap_or_else(|e| e.into_inner());
                s.state = StreamState::Offline;
                s.last_error = Some(ErrorClass::Config);
                None
            }
        };
        Running {
            target: target.clone(),
            status,
            stop,
            task,
        }
    }

    /// Opens `name`'s stream again at once, whatever it was waiting for (a machine just
    /// made the source should not wait out a backoff). `false` when no stream runs for
    /// `name`, or after [`Self::stop`].
    pub fn restart(&self, name: &str) -> bool {
        let stopped = self.stopped.lock().unwrap_or_else(|e| e.into_inner());
        if *stopped {
            return false;
        }
        let mut running = self.running.lock().unwrap_or_else(|e| e.into_inner());
        let Some(old) = running.remove(name) else {
            return false;
        };
        let _ = old.stop.send(true);
        running.insert(name.to_string(), self.spawn(&old.target));
        true
    }

    /// Every server's status, by name.
    pub fn status(&self) -> Vec<ServerStatus> {
        self.running
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .map(|(name, r)| ServerStatus {
                server: name.clone(),
                status: r.status.lock().unwrap_or_else(|e| e.into_inner()).clone(),
            })
            .collect()
    }

    /// The servers being served, by name.
    pub fn servers(&self) -> Vec<String> {
        self.running
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .keys()
            .cloned()
            .collect()
    }

    /// Stops every stream (non-blocking) and starts none after.
    pub fn stop(&self) {
        let mut stopped = self.stopped.lock().unwrap_or_else(|e| e.into_inner());
        *stopped = true;
        for r in self
            .running
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .values()
        {
            let _ = r.stop.send(true);
        }
    }

    /// [`Self::stop`], then waits up to `bound` for every task to end; `true` when all did.
    pub async fn join(&self, bound: Duration) -> bool {
        self.stop();
        let tasks: Vec<JoinHandle<()>> = self
            .running
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .values_mut()
            .filter_map(|r| r.task.take())
            .collect();
        tokio::time::timeout(bound, async {
            for t in tasks {
                let _ = t.await;
            }
        })
        .await
        .is_ok()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::delivery::fake::Fake;
    use crate::delivery::tests::{fast, machine, request_event, target, Recorder, FAR};
    use crate::delivery::NoCredentials;

    fn tasks() -> SourceTasks {
        SourceTasks::new(
            tokio::runtime::Handle::current(),
            machine(),
            Arc::new(Recorder::default()),
            Arc::new(|_: &ServerTarget, _: watch::Receiver<bool>| {
                Arc::new(NoCredentials) as Arc<dyn StreamCredentials>
            }),
        )
        .with_timing(fast())
    }

    async fn wait_state(t: &SourceTasks, server: &str, state: StreamState) {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        loop {
            if t.status()
                .iter()
                .any(|s| s.server == server && s.status.state == state)
            {
                return;
            }
            assert!(tokio::time::Instant::now() < deadline, "{:?}", t.status());
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    }

    #[tokio::test]
    async fn streams_follow_the_server_list() {
        let a = Fake::start().await;
        let b = Fake::start().await;
        let t = tasks();
        let mut ta = target(&a.url);
        ta.name = "a".into();
        let mut tb = target(&b.url);
        tb.name = "b".into();
        t.reconcile(&[ta.clone(), tb.clone()]);
        a.wait_streams(1).await;
        b.wait_streams(1).await;
        wait_state(&t, "a", StreamState::Connected).await;
        assert_eq!(t.servers(), vec!["a", "b"]);

        // b leaves the list: its stream ends and is not reopened.
        t.reconcile(&[ta.clone()]);
        assert_eq!(t.servers(), vec!["a"]);

        // a's target changes: it restarts (a second stream on a).
        let mut ta2 = ta.clone();
        ta2.user = "ada".into();
        t.reconcile(&[ta2]);
        a.wait_streams(2).await;

        // An unchanged list changes nothing: a request on a's live stream is answered.
        a.push(&request_event("99999999999999999999999999999999", FAR));
        a.wait("answer", |_, p| !p.is_empty()).await;
        assert!(t.join(Duration::from_secs(10)).await);
        assert!(t
            .status()
            .iter()
            .all(|s| s.status.state == StreamState::Stopped));
        assert_eq!(b.streams().len(), 1);
    }

    /// "Use this machine" reopens a server's stream at once; an unknown server, or a
    /// stopped set, restarts nothing.
    #[tokio::test]
    async fn restart_reopens_one_servers_stream() {
        let a = Fake::start().await;
        let t = tasks();
        let mut ta = target(&a.url);
        ta.name = "a".into();
        t.reconcile(&[ta]);
        a.wait_streams(1).await;
        assert!(t.restart("a"));
        a.wait_streams(2).await;
        assert_eq!(t.servers(), vec!["a"]);
        assert!(!t.restart("nope"));
        t.stop();
        assert!(!t.restart("a"));
        assert!(t.join(Duration::from_secs(10)).await);
    }

    /// A restart or reconcile racing the shutdown's stop never leaves a task whose stop
    /// signal was not sent: both hold the stopped flag across the map's change.
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn nothing_escapes_a_stop_that_races_restart_or_reconcile() {
        for i in 0..200 {
            let t = Arc::new(tasks());
            let mut ta = target("http://127.0.0.1:1");
            ta.name = "a".into();
            t.reconcile(std::slice::from_ref(&ta));
            let (r, s) = (t.clone(), t.clone());
            let mut changed = ta.clone();
            changed.user = format!("u{i}");
            let racer = std::thread::spawn(move || {
                if i % 2 == 0 {
                    r.restart("a");
                } else {
                    r.reconcile(&[changed]);
                }
            });
            let stopper = std::thread::spawn(move || s.stop());
            racer.join().unwrap();
            stopper.join().unwrap();
            for (name, run) in t.running.lock().unwrap().iter() {
                assert!(*run.stop.borrow(), "round {i}: {name} escaped the stop");
            }
            assert!(t.join(Duration::from_secs(10)).await);
        }
    }

    #[tokio::test]
    async fn a_server_that_cannot_be_reached_by_config_says_so() {
        let t = tasks();
        let mut bad = target("https://mini.example.test:18443");
        bad.name = "bad".into();
        t.reconcile(&[bad]);
        let s = t.status();
        assert_eq!(s[0].status.state, StreamState::Offline);
        assert_eq!(s[0].status.last_error, Some(ErrorClass::Config));
        assert!(t.join(Duration::from_secs(1)).await);
    }

    #[tokio::test]
    async fn nothing_starts_after_stop() {
        let a = Fake::start().await;
        let t = tasks();
        t.stop();
        t.reconcile(&[target(&a.url)]);
        assert!(t.servers().is_empty());
    }
}
