//! Capability discovery — a port of `internal/ext/rc/capabilities.go`.
//!
//! The payload behind `rc capabilities` AND the block embedded in every `list`
//! envelope: which kinds this binary offers, which agents are actually installed
//! (and at what version), the feature-token set, and the per-kind UI hints a
//! client renders affordances from without a table of its own.
//!
//! ## The budget is the design
//!
//! `list` embeds this and is consumed on the server's session-listing hot path
//! under a ~2 s exec timeout, while some agent CLIs take seconds to answer
//! `--version`. So every tool gets TWO concurrent probes — the full one
//! (`command -v` then `<bin> --version`) and a fast install-only one — under a
//! SHARED [`PROBE_BUDGET`]. Past the budget nothing blocks: a completed full
//! result is taken if it raced in, else the already-flying installed-only result,
//! else the agent degrades to `installed: false`.
//!
//! Go runs the probes as goroutines writing to buffered channels; this port runs
//! them as detached `std::thread`s writing to `mpsc` channels — deliberately NOT
//! `thread::scope`, whose join-on-exit would reintroduce exactly the unbounded
//! wait the budget exists to prevent. A laggard thread finishing later just fails
//! its send (the receiver is gone) and exits, which is Go's "parks its result and
//! the goroutine exits — no leak".

use std::collections::HashMap;
use std::process::{Command, Stdio};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::sync::Arc;
use std::time::{Duration, Instant};

use shed_core::rc::{RcAgentInfo, RcCapabilities, RcKind, RcKindFeatures};
use shed_core::rc_agents::{all_kinds, shell_quote_always, tool_for};

/// The capabilities schema/protocol version (`CapabilityVersion`,
/// `capabilities.go:15`). Deliberately decoupled from `SHED_RC_V` (the on-session
/// metadata schema, which stays 2).
pub const CAPABILITY_VERSION: i64 = 4;

/// The advertised feature tokens, **by value AND order** (`capabilityFeatures`,
/// `capabilities.go:43`) — clients gate behavior on them, so this is a wire
/// contract and a token ships in the same change as the feature it names.
pub const CAPABILITY_FEATURES: [&str; 7] = [
    "generic-perm",
    "plan-stdin",
    "prompt-b64",
    "serve",
    "activity",
    "messages",
    "contract-v2",
];

/// The total wall-clock budget for ALL agent probes (`probeBudget`,
/// `capabilities.go:129`).
pub const PROBE_BUDGET: Duration = Duration::from_millis(750);

/// Per-command timeout for a single probe invocation (`agentProbeTimeout`,
/// `clirc.go:403`). Bounds one hung `bash`; [`PROBE_BUDGET`] bounds the whole
/// flight.
const AGENT_PROBE_TIMEOUT: Duration = Duration::from_secs(2);

/// The full per-agent probe (`rc.AgentProbe`, `capabilities.go:112`) — install
/// state plus version.
pub type AgentProbe = Arc<dyn Fn(&str) -> RcAgentInfo + Send + Sync>;

/// The fast install-only probe used to degrade a laggard (`rc.InstalledProbe`,
/// `capabilities.go:121`). `None` skips the fallback entirely, exactly as Go's
/// nil does.
pub type InstalledProbe = Arc<dyn Fn(&str) -> bool + Send + Sync>;

/// Run `work` on a DETACHED thread (never joined — see the module doc) and hand
/// back the one-shot receiver its result will arrive on.
fn spawn_probe<T: Send + 'static>(work: impl FnOnce() -> T + Send + 'static) -> mpsc::Receiver<T> {
    let (tx, rx) = mpsc::channel();
    std::thread::spawn(move || {
        // A send after the receiver is gone (the budget expired) is an Err we
        // deliberately drop — the thread just exits.
        let _ = tx.send(work());
    });
    rx
}

/// Assemble the capabilities payload (`BuildCapabilities`,
/// `capabilities.go:138`), probing each registered agent binary concurrently
/// under the shared [`PROBE_BUDGET`].
pub fn build_capabilities(
    probe: &AgentProbe,
    installed: Option<&InstalledProbe>,
) -> RcCapabilities {
    struct Slot {
        tool: String,
        done: mpsc::Receiver<RcAgentInfo>,
        inst_done: Option<mpsc::Receiver<bool>>,
    }

    let mut slots: Vec<Slot> = Vec::new();
    let mut seen: Vec<&'static str> = Vec::new();
    for kind in all_kinds() {
        // shell has nothing to probe; claude backs two kinds and is probed once.
        let Some(tool) = tool_for(&kind) else {
            continue;
        };
        let Some(bin) = tool.bin() else { continue };
        if seen.contains(&tool.as_str()) {
            continue;
        }
        seen.push(tool.as_str());

        let done = spawn_probe({
            let probe = Arc::clone(probe);
            let bin = bin.to_string();
            move || probe(&bin)
        });

        // The fast installed-only check launches ALONGSIDE the full probe so its
        // result is (almost always) already waiting if the full probe misses the
        // budget — the fallback never runs synchronously after expiry.
        let inst_done = installed.map(|installed| {
            let installed = Arc::clone(installed);
            let bin = bin.to_string();
            spawn_probe(move || installed(&bin))
        });

        slots.push(Slot {
            tool: tool.as_str().to_string(),
            done,
            inst_done,
        });
    }

    let deadline = Instant::now() + PROBE_BUDGET;
    let mut expired = false;
    let mut agents = HashMap::new();
    for slot in &slots {
        if !expired {
            let remaining = deadline.saturating_duration_since(Instant::now());
            match slot.done.recv_timeout(remaining) {
                Ok(info) => {
                    agents.insert(slot.tool.clone(), info);
                    continue;
                }
                Err(RecvTimeoutError::Timeout) => expired = true,
                // A panicked probe thread: fall through to the degrade path for
                // THIS agent without condemning the rest of the flight.
                Err(RecvTimeoutError::Disconnected) => {}
            }
        }
        // Budget exhausted: take a completed full result if it raced in, else the
        // already-flying installed-only one. Both reads are non-blocking — a hung
        // login shell degrades the agent to installed:false rather than stalling.
        let info = match slot.done.try_recv() {
            Ok(info) => info,
            Err(_) => RcAgentInfo {
                installed: slot
                    .inst_done
                    .as_ref()
                    .and_then(|rx| rx.try_recv().ok())
                    .unwrap_or(false),
                version: None,
            },
        };
        agents.insert(slot.tool.clone(), info);
    }

    RcCapabilities {
        rc_version: CAPABILITY_VERSION,
        kinds: all_kinds().to_vec(),
        agents,
        features: CAPABILITY_FEATURES
            .iter()
            .map(|f| (*f).to_string())
            .collect(),
        kind_features: kind_features(),
    }
}

/// The per-kind UI hints (`kindFeatures`, `capabilities.go:224`).
///
/// `claude-broker` (driven from claude.ai, not the pane) and `shell` (no agent
/// approval surface) are OMITTED entirely — an absent entry is the wire's way of
/// saying "no feed/input/approval affordances".
///
/// ```text
/// kind      | post_input | approvals | watch | input | feed     | interrupt | attach
/// claude-rc | true       | tui       | false | ""    | activity | false     | tmux
/// codex     | true       | tui       | true  | gated | messages | false     | tmux
/// opencode  | true       | remote    | true  | turn  | messages | true      | tmux
/// cursor    | true       | tui       | true  | gated | messages | false     | tmux
/// ```
pub fn kind_features() -> HashMap<String, RcKindFeatures> {
    let mut out = HashMap::new();
    for kind in all_kinds() {
        if kind == RcKind::ClaudeBroker || kind == RcKind::Shell {
            continue;
        }
        // The BASE row is a TUI-lane session: approvals answered on the pane, a
        // terminal reaching it over tmux, no turn/interrupt verb, no feed input.
        // "activity" is the feed FLOOR — the hub derives the activity dimension
        // for every watched kind even where no message feed exists.
        let mut kf = RcKindFeatures {
            post_input: kind.accepts_typed_input(),
            approvals: "tui".to_string(),
            watch: false,
            input: String::new(),
            feed: "activity".to_string(),
            interrupt: false,
            attach: "tmux".to_string(),
        };
        match kind {
            // codex's rollout JSONL folds into a normalized message feed, and its
            // composer anchor gates POST /input acceptance.
            RcKind::Codex => {
                kf.feed = "messages".to_string();
                kf.input = "gated".to_string();
            }
            // opencode is the first LIVE lane: its TUI runs an embedded HTTP+SSE
            // server the hub steers through, so whole turns, interrupts and
            // approvals all go through the hub rather than the pane. `input` is
            // single-valued, so "turn" REPLACES codex's "gated" spelling.
            RcKind::Opencode => {
                kf.feed = "messages".to_string();
                kf.input = "turn".to_string();
                kf.approvals = "remote".to_string();
                kf.interrupt = true;
            }
            // cursor's hook scripts push its turn boundaries, tool calls and
            // messages into the hub — a message feed — and its composer anchor
            // gates POST /input exactly as codex's does. approvals stays "tui":
            // there is nothing the hub can honor remotely.
            RcKind::Cursor => {
                kf.feed = "messages".to_string();
                kf.input = "gated".to_string();
            }
            _ => {}
        }
        // watch is the deprecated spelling of feed == "messages"; DERIVED here so
        // the two cannot drift.
        kf.watch = kf.feed == "messages";
        out.insert(kind.as_str().to_string(), kf);
    }
    out
}

// ---------------------------------------------------------------------------
// the production probes (clirc.go:465-511)
// ---------------------------------------------------------------------------

/// Whether an agent binary is on PATH — `bash -lc "command -v '<bin>'"`
/// (`realInstalledProbe`, `clirc.go:465`).
///
/// A LOGIN shell on purpose: both the server's agent-exec channel and a bare sshd
/// exec otherwise inherit only the system PATH, which hides every agent installed
/// under the shed user's home. `command` is a bash builtin, so even a minimal
/// shell answers (as not-found) rather than crashing.
pub fn real_installed_probe(bin: &str) -> bool {
    run_bounded(&format!("command -v {}", shell_quote_always(bin))).is_some_and(|(ok, _)| ok)
}

/// An agent binary's install state and version (`realAgentProbe`,
/// `clirc.go:482`): the install check first, then `<bin> --version` through the
/// SAME login shell so PATH resolves the binary.
///
/// The version is parsed from the LAST non-empty stdout line, because login-shell
/// profile activation (mise printing its own version, say) can precede the
/// command's own answer.
pub fn real_agent_probe(bin: &str) -> RcAgentInfo {
    if !real_installed_probe(bin) {
        return RcAgentInfo {
            installed: false,
            version: None,
        };
    }
    let stdout = run_bounded(&format!("{} --version", shell_quote_always(bin)))
        .map(|(_, out)| out)
        .unwrap_or_default();
    let version = parse_version_from_login_shell(&stdout);
    RcAgentInfo {
        installed: true,
        // Go's Version is `omitempty`: an unreadable version is an ABSENT field,
        // not an empty string.
        version: (!version.is_empty()).then_some(version),
    }
}

/// Run `bash -lc <script>` under [`AGENT_PROBE_TIMEOUT`], returning
/// `(exited-zero, stdout)` — or `None` when bash could not be spawned at all.
/// stderr is dropped (Go captures only stdout; profile noise on stderr is not the
/// agent's answer).
fn run_bounded(script: &str) -> Option<(bool, String)> {
    let mut child = Command::new("bash")
        .arg("-lc")
        .arg(script)
        .stdin(Stdio::null())
        .stderr(Stdio::null())
        .stdout(Stdio::piped())
        .spawn()
        .ok()?;
    // Drain the pipe on a thread: a child blocked writing into a full pipe while
    // we poll would never exit, and `wait_with_output` would ignore the timeout.
    let reader = child.stdout.take().map(|mut out| {
        std::thread::spawn(move || {
            use std::io::Read;
            let mut buf = Vec::new();
            let _ = out.read_to_end(&mut buf);
            buf
        })
    });
    let deadline = Instant::now() + AGENT_PROBE_TIMEOUT;
    let ok = loop {
        match child.try_wait() {
            Ok(Some(status)) => break status.success(),
            Ok(None) if Instant::now() < deadline => {
                std::thread::sleep(Duration::from_millis(10));
            }
            _ => {
                let _ = child.kill();
                let _ = child.wait();
                break false;
            }
        }
    };
    let stdout = reader
        .and_then(|handle| handle.join().ok())
        .map(|buf| String::from_utf8_lossy(&buf).into_owned())
        .unwrap_or_default();
    Some((ok, stdout))
}

/// Extract an agent's version from `bash -lc "<bin> --version"` stdout, anchored
/// to the LAST non-empty line (`parseVersionFromLoginShell`, `clirc.go:497`).
pub fn parse_version_from_login_shell(out: &str) -> String {
    out.split('\n')
        .rev()
        .map(str::trim)
        .find(|line| !line.is_empty())
        .map(parse_agent_version)
        .unwrap_or_default()
}

/// Pull a clean version string out of raw `--version` output, or fall back to the
/// trimmed first line (`ParseAgentVersion`, `capabilities.go:294`).
///
/// Go uses `regexp.MustCompile("v?(\\d+\\.\\d+(?:\\.\\d+)?[\\w.\\-]*)")` and takes
/// the leftmost match's group 1. This is that scan by hand — shed-app has no
/// `regex` dependency and this is the only pattern it would need — pinned against
/// Go's own examples (and a wider table) in the tests below: an optional leading
/// `v`, then `<digits>.<digits>` with an optional third component, then a greedy
/// run of word chars, dots and hyphens.
pub fn parse_agent_version(out: &str) -> String {
    let out = out.trim();
    let bytes = out.as_bytes();
    for start in 0..bytes.len() {
        // `v?` is greedy, but a failure after it backtracks to the empty match —
        // so a `v` here means TWO alignments are tried, in that order.
        let after_v = (bytes[start] == b'v').then_some(start + 1);
        for from in after_v.into_iter().chain([start]) {
            if let Some(end) = match_version(bytes, from) {
                return out[from..end].to_string();
            }
        }
    }
    // No version-shaped token: the trimmed FIRST line.
    match out.split_once('\n') {
        Some((first, _)) => first.trim().to_string(),
        None => out.to_string(),
    }
}

/// `\d+\.\d+(?:\.\d+)?[\w.\-]*` anchored at `from`, returning the end offset.
fn match_version(b: &[u8], from: usize) -> Option<usize> {
    let mut i = from;
    let digits = |b: &[u8], i: &mut usize| {
        let start = *i;
        while b.get(*i).is_some_and(u8::is_ascii_digit) {
            *i += 1;
        }
        *i > start
    };
    if !digits(b, &mut i) || b.get(i) != Some(&b'.') {
        return None;
    }
    i += 1;
    if !digits(b, &mut i) {
        return None;
    }
    // The optional third component is inside the greedy tail's character class
    // anyway, so it needs no separate step.
    while b
        .get(i)
        .is_some_and(|c| c.is_ascii_alphanumeric() || *c == b'_' || *c == b'.' || *c == b'-')
    {
        i += 1;
    }
    Some(i)
}

#[cfg(test)]
mod tests;
