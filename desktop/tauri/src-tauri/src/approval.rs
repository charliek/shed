//! Production impls of the approval seams (test mode uses shed-app's fakes).
//!
//! The real native gate is **polkit** (via `pkcheck`); the notifier posts approval
//! banners over the **freedesktop Notifications** D-Bus interface (zbus). Both are
//! `#[cfg(target_os = "linux")]`; the Tauri crate also builds on macOS, where the
//! osascript notifier + the real **Touch-ID gate** (`macos::TouchIdGate` —
//! `LAContext.evaluatePolicy` via objc2, B3) run instead. Any other target falls
//! back to the fail-closed `FailClosedGate` (`Unavailable`). The Linux gate shells out
//! to the polkit tools rather than linking a D-Bus crate — the user's secret is
//! entered into the OS polkit agent, never this app (TB5), and a missing tool fails
//! **closed** (`Unavailable`). The notifier does use zbus, so it can recall (close)
//! the exact `--urgency=critical` banner when a request resolves, rather than
//! leaving it up (that banner never auto-expires).
//!
//! The button-only ("prompt") method needs no gate and works everywhere; polkit
//! only *adds* the password-gated method on top (B6). Under the hermetic harness
//! the gate is bypassed (test-mode `AlwaysApprovedGate`), so the approval matrix is
//! green independent of polkit.

use std::sync::Arc;

use tauri::{AppHandle, Emitter};

use shed_app::traits::{AuthGateRef, CoordinatorEvent, EventSink, NotifierRef};

#[cfg(target_os = "linux")]
use dbus_notify::DBusNotifier;

/// The production notifier + auth gate for the running platform. Linux: the real
/// polkit gate + the zbus D-Bus notifier. macOS: the osascript notifier (B5) + the
/// real Touch-ID gate (B3). Other targets: the fail-closed stubs. Test mode never
/// calls this — it uses shed-app's fakes.
pub fn production_seams() -> (NotifierRef, AuthGateRef) {
    #[cfg(target_os = "linux")]
    {
        let notifier = DBusNotifier::new(Arc::new(linux::ZbusNotifyBus::new()));
        (Arc::new(notifier), Arc::new(linux::PolkitGate))
    }
    #[cfg(target_os = "macos")]
    {
        // B3 + B5: the real Touch-ID gate (LAContext.evaluatePolicy via objc2) +
        // real approval banners (osascript) — full mac parity with the Swift app.
        (Arc::new(macos::OsaNotifier), Arc::new(macos::TouchIdGate::default()))
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        (Arc::new(NoopNotifier), Arc::new(FailClosedGate))
    }
}

/// Forwards coordinator-state changes to the webview as Tauri events, so the
/// Approvals/Activity panes re-fetch reactively (no polling). The event names
/// match the `listen(...)` calls in the React bridge.
pub struct TauriEventSink {
    app: AppHandle,
}

impl TauriEventSink {
    pub fn new(app: AppHandle) -> Self {
        Self { app }
    }
}

impl EventSink for TauriEventSink {
    fn emit(&self, event: CoordinatorEvent) {
        let name = match event {
            CoordinatorEvent::Approvals => "approvals-changed",
            CoordinatorEvent::Activity => "activity-changed",
            CoordinatorEvent::Connected => "connected-changed",
        };
        let _ = self.app.emit(name, ());
    }
}

/// The no-desktop-notifications notifier (targets that are neither Linux nor
/// macOS): a no-op. Linux + macOS have real notifiers, so it's only constructed
/// on other targets.
#[cfg(not(any(target_os = "linux", target_os = "macos")))]
mod noop_notifier {
    use shed_app::traits::Notifier;
    use shed_core::approval::ApprovalRequest;

    pub struct NoopNotifier;

    impl Notifier for NoopNotifier {
        fn post(&self, _req: &ApprovalRequest) {}
        fn withdraw(&self, _id: &str) {}
    }
}
#[cfg(not(any(target_os = "linux", target_os = "macos")))]
use noop_notifier::NoopNotifier;

/// The fallback auth gate for targets with no native gate (neither Linux polkit nor
/// macOS Touch-ID): fail-closed. A biometric/password-gated approve can't be
/// confirmed, so the request stays pending and expires to deny (F5). The button-only
/// ("prompt") method needs no gate, so it still works. Only constructed on such
/// targets, so it's `#[cfg]`d out on Linux + macOS (which have real gates).
#[cfg(not(any(target_os = "linux", target_os = "macos")))]
use shed_app::traits::{AuthGate, AuthOutcome, AuthPrompt};

#[cfg(not(any(target_os = "linux", target_os = "macos")))]
pub struct FailClosedGate;

#[cfg(not(any(target_os = "linux", target_os = "macos")))]
#[async_trait::async_trait]
impl AuthGate for FailClosedGate {
    async fn gate(&self, _prompt: AuthPrompt) -> AuthOutcome {
        AuthOutcome::Unavailable
    }
}

/// The desktop-notification notifier split from its D-Bus transport, so the
/// post→id→withdraw *race* is unit-testable without a real bus. An approval banner
/// is posted `--urgency=critical` so the daemon never auto-expires it; when the
/// request resolves we must therefore close its *exact* banner — which needs the
/// daemon's notification id, only known after the async `notify` returns. Compiled
/// on Linux (the sole real transport, [`linux::ZbusNotifyBus`]) and in any test
/// build (the `tests::FakeBus` drives the state machine directly — the race logic
/// is verified with `cargo test` on any host, not just via the Linux render gate).
#[cfg(any(target_os = "linux", test))]
mod dbus_notify {
    use std::collections::HashMap;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::sync::{Arc, Mutex};

    use shed_app::traits::{approval_notification_text as text, Notifier, PostedNotification};
    use shed_core::approval::ApprovalRequest;

    /// A minimal async seam over the freedesktop Notifications bus. `notify` returns
    /// the daemon's notification id (0 = no daemon / failed — nothing to recall).
    #[async_trait::async_trait]
    pub trait NotifyBus: Send + Sync + 'static {
        async fn notify(&self, title: String, body: String) -> u32;
        async fn close(&self, notif_id: u32);
    }

    /// A request's tracked banner across the async gap between posting and the
    /// daemon returning its id. `title`/`body` are retained so `posted()` (the
    /// North-Star `notifications.list` view) is truthful on a real desktop, not just
    /// in tests. `gen` distinguishes a re-post's newer post task from the superseded
    /// older one — both run concurrently after `post` returns, and the coordinator
    /// DOES re-post a same-id replacement (coordinator.rs `PolicyAction::Prompt`).
    struct Entry {
        gen: u64,
        title: String,
        body: String,
        /// The daemon's id once `notify()` returns; `None` while it's in flight.
        notif_id: Option<u32>,
        /// `withdraw` arrived before the id was known — close as soon as it lands.
        withdrawn: bool,
    }

    /// Posts + withdraws approval banners over a [`NotifyBus`], closing the exact
    /// banner when a request resolves. Fire-and-forget (the coordinator actor never
    /// blocks on the notification daemon); the shared state + `gen` reconcile the
    /// post→id→withdraw race and same-id re-posts without leaking a banner.
    pub struct DBusNotifier {
        bus: Arc<dyn NotifyBus>,
        state: Arc<Mutex<HashMap<String, Entry>>>,
        gen: Arc<AtomicU64>,
    }

    impl DBusNotifier {
        pub fn new(bus: Arc<dyn NotifyBus>) -> Self {
            Self {
                bus,
                state: Arc::new(Mutex::new(HashMap::new())),
                gen: Arc::new(AtomicU64::new(0)),
            }
        }

        /// Close a banner on a detached task (withdraw/close are fire-and-forget; the
        /// coordinator never waits on the daemon).
        fn spawn_close(bus: &Arc<dyn NotifyBus>, notif_id: u32) {
            let bus = bus.clone();
            tokio::spawn(async move { bus.close(notif_id).await });
        }
    }

    impl Notifier for DBusNotifier {
        fn post(&self, req: &ApprovalRequest) {
            let id = req.id.clone();
            let title = text::title(req);
            let body = text::body(req);
            let gen = self.gen.fetch_add(1, Ordering::Relaxed);
            // Replace any prior banner for this id. If the prior one already has a
            // daemon id, retract it now; if it's still in flight, its older-gen task
            // retracts its own banner when it sees a newer gen on reconcile.
            let close_old = {
                let mut st = self.state.lock().unwrap();
                let prev = st.insert(
                    id.clone(),
                    Entry {
                        gen,
                        title: title.clone(),
                        body: body.clone(),
                        notif_id: None,
                        withdrawn: false,
                    },
                );
                prev.and_then(|e| e.notif_id)
            };
            if let Some(old) = close_old {
                Self::spawn_close(&self.bus, old);
            }
            let bus = self.bus.clone();
            let state = self.state.clone();
            // In the coordinator actor (a tokio task), so spawn is valid.
            tokio::spawn(async move {
                let notif_id = bus.notify(title, body).await;
                let close_self = {
                    let mut st = state.lock().unwrap();
                    // Copy the fields out so the map isn't borrowed across remove/get_mut.
                    match st.get(&id).map(|e| (e.gen, e.withdrawn)) {
                        // Still the current post for this id.
                        Some((g, withdrawn)) if g == gen => {
                            if notif_id == 0 || withdrawn {
                                // No daemon, or withdrawn while in flight → drop it,
                                // closing the banner if one actually went up.
                                st.remove(&id);
                                notif_id != 0
                            } else {
                                st.get_mut(&id).unwrap().notif_id = Some(notif_id);
                                false
                            }
                        }
                        // Superseded by a newer post (or gone) → retract our own banner.
                        _ => notif_id != 0,
                    }
                };
                if close_self {
                    bus.close(notif_id).await;
                }
            });
        }

        fn withdraw(&self, id: &str) {
            let close = {
                let mut st = self.state.lock().unwrap();
                match st.get(id).and_then(|e| e.notif_id) {
                    // Posted → drop + close the exact banner.
                    Some(notif_id) => {
                        st.remove(id);
                        Some(notif_id)
                    }
                    // In flight (entry present, no id yet) → mark so the post task
                    // closes it; or absent → nothing to do.
                    None => {
                        if let Some(e) = st.get_mut(id) {
                            e.withdrawn = true;
                        }
                        None
                    }
                }
            };
            if let Some(notif_id) = close {
                Self::spawn_close(&self.bus, notif_id);
            }
        }

        fn posted(&self) -> Vec<PostedNotification> {
            let mut out: Vec<PostedNotification> = self
                .state
                .lock()
                .unwrap()
                .iter()
                .filter(|(_, e)| !e.withdrawn)
                .map(|(id, e)| PostedNotification {
                    id: id.clone(),
                    title: e.title.clone(),
                    body: e.body.clone(),
                })
                .collect();
            // HashMap iteration is unordered; sort by id so `notifications.list` is
            // deterministic for a real-notifier assertion (the fake yields Vec order).
            out.sort_by(|a, b| a.id.cmp(&b.id));
            out
        }
    }

    #[cfg(test)]
    impl DBusNotifier {
        /// True once `notify()` has returned an id for `id` (state is `Posted`, not
        /// `Pending`) — lets a test drive `withdraw` deterministically down the
        /// `withdraw`-finds-`Posted` path rather than racing the post task.
        fn is_posted(&self, id: &str) -> bool {
            matches!(self.state.lock().unwrap().get(id), Some(e) if e.notif_id.is_some())
        }
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use std::sync::atomic::{AtomicU32, Ordering};
        use std::time::Duration;
        use tokio::sync::Notify;

        /// A `NotifyBus` that records calls + hands out incrementing ids, optionally
        /// pausing `notify()` on a gate so a test can drive the withdraw-mid-post race.
        struct FakeBus {
            ids: AtomicU32,
            notified: Mutex<Vec<(String, String)>>,
            closed: Mutex<Vec<u32>>,
            return_zero: bool,
            hold: bool,
            entered: Notify, // notify() signals it parked
            release: Notify, // test signals notify() to return
        }

        impl FakeBus {
            fn build(hold: bool, return_zero: bool) -> Arc<Self> {
                Arc::new(Self {
                    ids: AtomicU32::new(0),
                    notified: Mutex::new(vec![]),
                    closed: Mutex::new(vec![]),
                    return_zero,
                    hold,
                    entered: Notify::new(),
                    release: Notify::new(),
                })
            }
            fn notified_count(&self) -> usize {
                self.notified.lock().unwrap().len()
            }
            fn closed_ids(&self) -> Vec<u32> {
                self.closed.lock().unwrap().clone()
            }
        }

        #[async_trait::async_trait]
        impl NotifyBus for FakeBus {
            async fn notify(&self, title: String, body: String) -> u32 {
                self.notified.lock().unwrap().push((title, body));
                if self.hold {
                    self.entered.notify_one();
                    self.release.notified().await;
                }
                if self.return_zero {
                    0
                } else {
                    self.ids.fetch_add(1, Ordering::SeqCst) + 1
                }
            }
            async fn close(&self, notif_id: u32) {
                self.closed.lock().unwrap().push(notif_id);
            }
        }

        fn req(id: &str) -> ApprovalRequest {
            ApprovalRequest {
                id: id.into(),
                ts: String::new(),
                server: String::new(),
                namespace: "ssh-agent".into(),
                op: "sign".into(),
                shed: "web".into(),
                detail: "ed25519".into(),
                expires_at: String::new(),
            }
        }

        /// Poll a condition — the notifier's work is fire-and-forget on spawned
        /// tasks, so a condition-wait (bounded) is how tests observe it settle.
        async fn wait_until(mut cond: impl FnMut() -> bool) {
            for _ in 0..500 {
                if cond() {
                    return;
                }
                tokio::time::sleep(Duration::from_millis(2)).await;
            }
            panic!("condition not met within timeout");
        }

        #[tokio::test]
        async fn withdraw_closes_the_exact_banner() {
            let bus = FakeBus::build(false, false);
            let n = DBusNotifier::new(bus.clone());
            n.post(&req("r1"));
            wait_until(|| n.is_posted("r1")).await; // notify() returned id 1 → Posted
            n.withdraw("r1"); // deterministically the withdraw-finds-Posted path
            wait_until(|| bus.closed_ids() == vec![1]).await;
            assert!(n.posted().is_empty()); // the tracked banner is gone
        }

        #[tokio::test]
        async fn repost_retracts_the_previous_banner() {
            // A same-id replacement (coordinator `PolicyAction::Prompt` re-posts an
            // id with no intervening withdraw) must close the PREVIOUS critical banner,
            // not orphan it — an orphaned expire_timeout=0 banner lingers forever.
            let bus = FakeBus::build(false, false);
            let n = DBusNotifier::new(bus.clone());
            n.post(&req("r1"));
            wait_until(|| n.is_posted("r1")).await; // banner 1 up
            n.post(&req("r1")); // replacement
            wait_until(|| bus.closed_ids() == vec![1]).await; // banner 1 retracted
            wait_until(|| n.is_posted("r1")).await; // banner 2 up (newer gen)
            n.withdraw("r1");
            // Exactly banners 1 and 2 were closed, once each — nothing leaked.
            wait_until(|| {
                let mut c = bus.closed_ids();
                c.sort();
                c == vec![1, 2]
            })
            .await;
        }

        #[tokio::test]
        async fn withdraw_during_pending_still_closes_after_id_arrives() {
            // The race: withdraw() runs while notify() is in flight (no id yet). The
            // post task must close the banner once the daemon returns its id.
            let bus = FakeBus::build(true, false);
            let n = DBusNotifier::new(bus.clone());
            n.post(&req("r1"));
            bus.entered.notified().await; // notify() is parked → state is Pending
            n.withdraw("r1"); // marks Withdrawn (no id to close yet)
            assert!(bus.closed_ids().is_empty());
            bus.release.notify_one(); // notify() returns id 1
            wait_until(|| bus.closed_ids() == vec![1]).await; // post task closes it
        }

        #[tokio::test]
        async fn no_daemon_tracks_and_closes_nothing() {
            let bus = FakeBus::build(false, true); // notify() returns 0
            let n = DBusNotifier::new(bus.clone());
            n.post(&req("r1"));
            wait_until(|| n.posted().is_empty() && bus.notified_count() == 1).await;
            n.withdraw("r1"); // nothing tracked → no close
            tokio::time::sleep(Duration::from_millis(10)).await;
            assert!(bus.closed_ids().is_empty());
        }
    }
}

#[cfg(target_os = "linux")]
mod linux {
    use std::collections::HashMap;

    use shed_app::traits::{AuthGate, AuthOutcome, AuthPrompt};

    use super::dbus_notify::NotifyBus;

    /// The polkit action a credential approval authenticates against. Must match the
    /// `<action id>` in the shipped `packaging/polkit/*.policy` (installed to
    /// `/usr/share/polkit-1/actions/`); an unregistered action → `pkcheck` errors →
    /// `Unavailable` (fail-closed).
    const POLKIT_ACTION_ID: &str = "ai.stridelabs.shed-desktop.approve-credential";

    /// The real Linux gate: `pkcheck --allow-user-interaction` runs the registered
    /// polkit agent's password/PAM dialog and blocks for the result. The secret is
    /// entered into the OS agent, never this process. Runs inside the spawned gate
    /// task (never the actor), so the blocking wait doesn't head-of-line-block.
    pub struct PolkitGate;

    #[async_trait::async_trait]
    impl AuthGate for PolkitGate {
        async fn gate(&self, prompt: AuthPrompt) -> AuthOutcome {
            // F5: "biometrics only" can't be guaranteed via polkit/PAM on Linux
            // (PAM may fall back to a password) — fail closed until fprintd lands.
            if prompt.biometrics_only {
                return AuthOutcome::Unavailable;
            }
            // Authorize OUR process (the app requesting on behalf of its user);
            // pkcheck reads our start-time from /proc to pin the subject.
            let pid = std::process::id().to_string();
            // ABSOLUTE path, never a bare name: `success()` is the ONLY thing that
            // separates approve from deny, so a `pkcheck` shadowed earlier on PATH
            // (e.g. a malicious ~/.local/bin/pkcheck that `exit 0`s) would be a silent
            // gate bypass. A missing /usr/bin/pkcheck falls through to the spawn-Err
            // arm below → Unavailable (still fail-closed).
            match tokio::process::Command::new("/usr/bin/pkcheck")
                .args([
                    "--action-id",
                    POLKIT_ACTION_ID,
                    "--process",
                    &pid,
                    "--allow-user-interaction",
                ])
                .status()
                .await
            {
                // Exactly one value approves; everything else is deny-safe (the
                // coordinator leaves the request pending regardless of the variant).
                Ok(s) if s.success() => AuthOutcome::Approved,
                // Any non-zero exit = not authorized (a cancel or a wrong secret —
                // polkit's exact codes vary by version, so don't guess which). A
                // signal death is abnormal → Error. Both leave the request pending.
                Ok(s) if s.code().is_some() => AuthOutcome::Denied,
                Ok(_) => AuthOutcome::Error("pkcheck killed by signal".into()),
                // No /usr/bin/pkcheck (polkit not installed) or spawn failure → the
                // system can't gate → fail closed, distinctly from a user deny.
                Err(_) => AuthOutcome::Unavailable,
            }
        }
    }

    /// The real freedesktop Notifications transport, over a single session-bus
    /// connection established lazily on first use and reused thereafter (no bus →
    /// `notify` returns 0 and `close` is a no-op, so approvals still work via the
    /// pane). Replaces the old `notify-send` subprocess: a direct `Notify` call
    /// returns the daemon's id, which `CloseNotification` needs to recall the exact
    /// banner. The post/withdraw race lives in [`super::dbus_notify::DBusNotifier`].
    pub struct ZbusNotifyBus {
        // Only a *successful* connection is cached (via `get_or_try_init`). Caching
        // a first-call failure — `OnceCell<Option<_>>` sealing a `None` — would
        // silently no-op every banner for the process lifetime even after the bus
        // came up; instead a failed attempt is retried on the next notify/close.
        conn: tokio::sync::OnceCell<zbus::Connection>,
    }

    impl ZbusNotifyBus {
        pub fn new() -> Self {
            Self {
                conn: tokio::sync::OnceCell::new(),
            }
        }

        /// A proxy over the shared connection (cheap per-call handle; the expensive
        /// connection is the cached part). `None` when there's no session bus — the
        /// attempt is not cached, so a later call retries.
        async fn proxy(&self) -> Option<NotificationsProxy<'_>> {
            let conn = self
                .conn
                .get_or_try_init(|| async { zbus::Connection::session().await })
                .await
                .ok()?;
            NotificationsProxy::new(conn).await.ok()
        }
    }

    /// Escape the markup-significant characters in a notification body: a daemon
    /// advertising "body-markup" renders an HTML-like subset, and op/shed/detail are
    /// attacker-influenced off the wire, so a raw `<b>`/`<a …>` could spoof this
    /// credential-approval banner. `&` is escaped first, so the `&` it introduces
    /// into `&lt;`/`&gt;` isn't itself re-escaped.
    pub(super) fn escape_body_markup(s: &str) -> String {
        s.replace('&', "&amp;")
            .replace('<', "&lt;")
            .replace('>', "&gt;")
    }

    #[async_trait::async_trait]
    impl NotifyBus for ZbusNotifyBus {
        async fn notify(&self, title: String, body: String) -> u32 {
            let Some(proxy) = self.proxy().await else {
                return 0;
            };
            // A daemon advertising "body-markup" renders a small HTML subset in the
            // BODY; op/shed/detail are attacker-influenced off the wire, so escape
            // them or a crafted value could spoof this credential-approval banner.
            // The summary (title) is always plain text per the freedesktop spec.
            let body = escape_body_markup(&body);
            // urgency=critical (2): the daemon must NOT auto-expire the banner — we
            // close it precisely when the request resolves (that's why we track ids).
            let urgency = zbus::zvariant::Value::from(2u8);
            let mut hints = HashMap::new();
            hints.insert("urgency", &urgency);
            proxy
                .notify(
                    "shed-desktop",
                    0,                            // replaces_id: a fresh banner
                    "ai.stridelabs.shed-desktop", // app icon
                    &title,                       // summary (plain text)
                    &body,
                    &[],   // no actions
                    hints, // urgency=critical
                    0,     // expire_timeout=0: never auto-expire (we recall it)
                )
                .await
                .unwrap_or(0)
        }

        async fn close(&self, notif_id: u32) {
            if let Some(proxy) = self.proxy().await {
                let _ = proxy.close_notification(notif_id).await;
            }
        }
    }

    /// The freedesktop notifications interface (the subset we drive). The
    /// `#[zbus::proxy]` macro generates `NotificationsProxy` with async methods.
    #[zbus::proxy(
        interface = "org.freedesktop.Notifications",
        default_service = "org.freedesktop.Notifications",
        default_path = "/org/freedesktop/Notifications"
    )]
    #[allow(clippy::too_many_arguments)]
    trait Notifications {
        fn notify(
            &self,
            app_name: &str,
            replaces_id: u32,
            app_icon: &str,
            summary: &str,
            body: &str,
            actions: &[&str],
            hints: HashMap<&str, &zbus::zvariant::Value<'_>>,
            expire_timeout: i32,
        ) -> zbus::Result<u32>;

        fn close_notification(&self, id: u32) -> zbus::Result<()>;
    }
}

#[cfg(target_os = "macos")]
mod macos {
    use shed_app::traits::Notifier;
    use shed_core::approval::ApprovalRequest;

    /// Posts an approval banner via `osascript` (Notification Center) — the mac
    /// analog of the Linux `notify-send` notifier (B5), matching the Swift app's
    /// approval banners. Best-effort + fire-and-forget so the actor never blocks;
    /// withdraw is a no-op (Notification Center banners auto-dismiss — precise
    /// recall via `UNUserNotificationCenter` is a follow-up, as on Linux).
    pub struct OsaNotifier;

    impl Notifier for OsaNotifier {
        fn post(&self, req: &ApprovalRequest) {
            use shed_app::traits::approval_notification_text as text;
            // AppleScript string literals: the request fields (namespace/op/shed/
            // detail) are attacker-influenced off the wire, so quote them to close
            // AppleScript injection — never interpolate raw.
            let script = format!(
                "display notification {} with title {}",
                osa_quote(&text::body(req)),
                osa_quote(&text::title(req)),
            );
            // We're inside the coordinator actor (a tokio task), so spawn is valid.
            tokio::spawn(async move {
                let _ = tokio::process::Command::new("/usr/bin/osascript")
                    .args(["-e", &script])
                    .status()
                    .await;
            });
        }
        fn withdraw(&self, _id: &str) {}
    }

    /// Quote a string as an AppleScript literal (`"..."`), escaping `\` and `"` so
    /// no wire-controlled field can break out of the string or inject script.
    fn osa_quote(s: &str) -> String {
        format!("\"{}\"", s.replace('\\', "\\\\").replace('"', "\\\""))
    }

    // -- B3: the Touch-ID gate (LAContext.evaluatePolicy via objc2) -----------

    use block2::RcBlock;
    use objc2::runtime::Bool;
    use objc2_foundation::{NSError, NSString};
    use objc2_local_authentication::{LAContext, LAPolicy};
    use shed_app::traits::{AuthGate, AuthOutcome, AuthPrompt};

    /// Map a LocalAuthentication failure code (`LAError.*`) to a rich `AuthOutcome`.
    /// The load-bearing property (asserted in tests): NO error code yields
    /// `Approved` — only a real `success` does. Cancels / unavailable / auth-failed
    /// are distinguished so they audit differently (F5).
    fn map_la_error(code: isize) -> AuthOutcome {
        match code {
            -2 | -4 | -9 => AuthOutcome::Cancelled, // user / system / app cancel
            -1 | -3 => AuthOutcome::Denied,         // auth failed / biometrics declined (fallback)
            // passcode-not-set / biometry-unavailable / not-enrolled / lockout, and
            // (newer) biometry-not-paired / -disconnected on Macs with removable
            // biometric hardware — all "the device can't gate right now" (F5).
            -8..=-5 | -12 | -13 => AuthOutcome::Unavailable,
            other => AuthOutcome::Error(format!("LAError {other}")),
        }
    }

    fn la_policy(biometrics_only: bool) -> LAPolicy {
        if biometrics_only {
            LAPolicy::DeviceOwnerAuthenticationWithBiometrics
        } else {
            LAPolicy::DeviceOwnerAuthentication
        }
    }

    /// A seam over `LAContext`, so the gate's deny-safe paths are unit-testable
    /// WITHOUT a real biometric prompt: `canEvaluatePolicy` returns `true` on any
    /// enrolled Mac, so calling the real gate in a test would fire a live Touch-ID
    /// prompt (and could auto-approve). Tests inject a fake; prod uses [`RealLocalAuth`].
    #[async_trait::async_trait]
    pub(crate) trait LocalAuth: Send + Sync {
        /// Can the device satisfy the policy right now (biometrics/passcode enrolled)?
        fn can_evaluate(&self, biometrics_only: bool) -> bool;
        /// Present the OS prompt + await the user's decision → a rich outcome.
        async fn evaluate(&self, biometrics_only: bool, reason: String) -> AuthOutcome;
    }

    pub(crate) struct RealLocalAuth;

    #[async_trait::async_trait]
    impl LocalAuth for RealLocalAuth {
        fn can_evaluate(&self, biometrics_only: bool) -> bool {
            // canEvaluatePolicy is a cheap thread-safe read; `Err` = not enrolled /
            // no passcode → the gate returns Unavailable without ever prompting.
            let ctx = unsafe { LAContext::new() };
            unsafe { ctx.canEvaluatePolicy_error(la_policy(biometrics_only)) }.is_ok()
        }

        async fn evaluate(&self, biometrics_only: bool, reason: String) -> AuthOutcome {
            let policy = la_policy(biometrics_only);
            // The `LAContext` + reply block are `!Send`, and the reply lands on an
            // arbitrary GCD thread — so confine ALL ObjC to one blocking thread and
            // hand back only the (Send) `AuthOutcome`. The async fn awaits a Send
            // `JoinHandle` and never holds an ObjC object across `.await` (the
            // `#[async_trait]` future must be Send). `ctx` lives on the blocking
            // thread's stack, kept alive by the `recv()` until the reply fires.
            tokio::task::spawn_blocking(move || {
                let (tx, rx) = std::sync::mpsc::channel::<AuthOutcome>();
                let ctx = unsafe { LAContext::new() };
                let reason = NSString::from_str(&reason);
                let reply = RcBlock::new(move |success: Bool, error: *mut NSError| {
                    let outcome = if success.as_bool() {
                        AuthOutcome::Approved
                    } else if let Some(err) = unsafe { error.as_ref() } {
                        map_la_error(err.code())
                    } else {
                        AuthOutcome::Denied
                    };
                    let _ = tx.send(outcome);
                });
                unsafe { ctx.evaluatePolicy_localizedReason_reply(policy, &reason, &reply) };
                // Bound the wait: if the reply never fires (an LA edge case, or the
                // block is dropped un-fired), don't strand this blocking-pool thread
                // — and the caller's `decide_approval` oneshot — forever. Generous vs
                // a human Touch-ID interaction (seconds); the request's own TTL
                // expires it upstream regardless. On timeout, cancel the lingering OS
                // prompt and fail closed (deny-safe `Error`, never `Approved`).
                match rx.recv_timeout(std::time::Duration::from_secs(120)) {
                    Ok(outcome) => outcome,
                    Err(_) => {
                        unsafe { ctx.invalidate() };
                        AuthOutcome::Error("touch-id timed out".into())
                    }
                }
            })
            .await
            .unwrap_or_else(|_| AuthOutcome::Error("touch-id task failed".into()))
        }
    }

    /// The macOS Touch-ID `AuthGate` (B3) — `LAContext.evaluatePolicy` behind the
    /// rich `AuthOutcome` (never a bool, F5), mirroring the Swift `TouchID.swift`.
    /// Generic over the [`LocalAuth`] seam so tests inject a fake; prod is
    /// `TouchIdGate::default()` = [`RealLocalAuth`].
    pub(crate) struct TouchIdGate<L: LocalAuth = RealLocalAuth> {
        la: L,
    }

    impl Default for TouchIdGate<RealLocalAuth> {
        fn default() -> Self {
            Self { la: RealLocalAuth }
        }
    }

    #[async_trait::async_trait]
    impl<L: LocalAuth> AuthGate for TouchIdGate<L> {
        async fn gate(&self, prompt: AuthPrompt) -> AuthOutcome {
            // Deny-safe: if the device can't satisfy the policy (no biometrics /
            // passcode enrolled), NEVER prompt — Unavailable (F5), matching
            // TouchID.swift's `canEvaluatePolicy == false` path.
            if !self.la.can_evaluate(prompt.biometrics_only) {
                return AuthOutcome::Unavailable;
            }
            self.la.evaluate(prompt.biometrics_only, prompt.reason).await
        }
    }

    #[cfg(test)]
    mod tests {
        use super::*;

        #[test]
        fn osa_quote_escapes_quotes_and_backslashes() {
            assert_eq!(osa_quote("plain"), "\"plain\"");
            assert_eq!(osa_quote("a\"b"), "\"a\\\"b\""); // " → \"  (can't break out)
            assert_eq!(osa_quote("a\\b"), "\"a\\\\b\""); // \ → \\  (escaped first)
            // Every interior double-quote is backslash-escaped, so an injection
            // attempt can't close the AppleScript literal.
            let evil = osa_quote("x\" & (do shell script \"rm -rf\")");
            let interior = &evil[1..evil.len() - 1];
            for (i, _) in interior.match_indices('"') {
                assert!(
                    i > 0 && interior.as_bytes()[i - 1] == b'\\',
                    "an unescaped quote survived at {i}"
                );
            }
        }

        // -- B3 Touch-ID gate: the deny-safe unit tests (no real biometric prompt).

        /// A fake `LocalAuth` so the gate's decision logic is exercised without
        /// touching `LAContext` — the only automated coverage of the real gate
        /// (the live Touch-ID path needs a signed build → the A5 manual smoke).
        struct FakeAuth {
            can: bool,
            outcome: AuthOutcome,
        }

        #[async_trait::async_trait]
        impl LocalAuth for FakeAuth {
            fn can_evaluate(&self, _biometrics_only: bool) -> bool {
                self.can
            }
            async fn evaluate(&self, _biometrics_only: bool, _reason: String) -> AuthOutcome {
                assert!(self.can, "evaluate() must not run when can_evaluate is false");
                self.outcome.clone()
            }
        }

        fn prompt() -> AuthPrompt {
            AuthPrompt { reason: "unlock a credential".into(), biometrics_only: false }
        }

        #[tokio::test]
        async fn unavailable_when_device_cannot_evaluate() {
            // Deny-safe: no enrolled biometrics/passcode → Unavailable, and the real
            // evaluate() (which would prompt) is never reached (asserted in FakeAuth).
            let gate = TouchIdGate {
                la: FakeAuth { can: false, outcome: AuthOutcome::Approved },
            };
            assert_eq!(gate.gate(prompt()).await, AuthOutcome::Unavailable);
        }

        #[tokio::test]
        async fn gate_returns_the_evaluated_outcome() {
            for outcome in [AuthOutcome::Approved, AuthOutcome::Denied, AuthOutcome::Cancelled] {
                let gate = TouchIdGate {
                    la: FakeAuth { can: true, outcome: outcome.clone() },
                };
                assert_eq!(gate.gate(prompt()).await, outcome);
            }
        }

        #[test]
        fn no_la_error_maps_to_approved() {
            // The single most important property of the gate: NO failure code can be
            // mistaken for approval — only a real `success` bool yields Approved.
            assert_eq!(map_la_error(-2), AuthOutcome::Cancelled);
            assert_eq!(map_la_error(-1), AuthOutcome::Denied);
            assert_eq!(map_la_error(-7), AuthOutcome::Unavailable);
            assert_eq!(map_la_error(-13), AuthOutcome::Unavailable); // biometry disconnected
            assert!(matches!(map_la_error(-9999), AuthOutcome::Error(_)));
            for code in [-1, -2, -3, -4, -5, -6, -7, -8, -9, -10, -12, -13, -1004, 0, -9999] {
                assert_ne!(map_la_error(code), AuthOutcome::Approved);
            }
        }
    }
}

#[cfg(all(test, target_os = "linux"))]
mod tests {
    use super::linux::{escape_body_markup, PolkitGate};
    use shed_app::traits::{AuthGate, AuthOutcome, AuthPrompt};

    #[test]
    fn escape_body_markup_neutralizes_spoofing() {
        assert_eq!(escape_body_markup("plain"), "plain");
        assert_eq!(escape_body_markup("<b>x</b>"), "&lt;b&gt;x&lt;/b&gt;");
        // `&` is escaped first, so `<` → `&lt;` is NOT re-escaped to `&amp;lt;`.
        assert_eq!(escape_body_markup("a & <c>"), "a &amp; &lt;c&gt;");
    }

    #[tokio::test]
    async fn biometrics_only_is_unavailable() {
        // F5: biometrics-only can't be honored via polkit on Linux — fail closed
        // without even invoking pkcheck.
        let out = PolkitGate
            .gate(AuthPrompt {
                reason: "r".into(),
                biometrics_only: true,
            })
            .await;
        assert_eq!(out, AuthOutcome::Unavailable);
    }

    #[tokio::test]
    async fn gate_never_approves_without_real_auth() {
        // No polkit agent / display / registered action in CI, so pkcheck — whether
        // absent (→ Unavailable) or present-but-unable-to-authorize (→ Denied/
        // Cancelled/Error) — must NEVER yield Approved. The single most important
        // property of the gate: it can't approve without a real authentication.
        let out = PolkitGate
            .gate(AuthPrompt {
                reason: "r".into(),
                biometrics_only: false,
            })
            .await;
        assert_ne!(out, AuthOutcome::Approved);
    }
}
