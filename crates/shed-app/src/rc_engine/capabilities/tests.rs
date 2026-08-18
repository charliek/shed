//! Mirrors `internal/ext/rc/capabilities_test.go` (the normative matrix + the
//! feature list by value AND order) and `capabilities_budget_test.go` (the probe
//! budget's degradation paths), plus the version-parse table `clirc.go`'s own
//! tests pin.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use super::*;

/// A latch a fake probe blocks on, so a test can hold a probe past the budget and
/// still let its thread exit at the end.
#[derive(Clone)]
struct Latch(Arc<AtomicBool>);

impl Latch {
    fn new() -> Self {
        Latch(Arc::new(AtomicBool::new(false)))
    }
    fn wait(&self) {
        while !self.0.load(Ordering::Relaxed) {
            std::thread::sleep(Duration::from_millis(5));
        }
    }
    fn release(&self) {
        self.0.store(true, Ordering::Relaxed);
    }
}

fn absent() -> RcAgentInfo {
    RcAgentInfo {
        installed: false,
        version: None,
    }
}

fn info(version: &str) -> RcAgentInfo {
    RcAgentInfo {
        installed: true,
        version: Some(version.to_string()),
    }
}

// --- the matrix (capabilities_test.go:23) -----------------------------------

/// The NORMATIVE per-kind matrix, exhaustively: every field of every kind that
/// has a row, plus the deliberate omission of `claude-broker` and `shell`.
#[test]
fn kind_features_matrix_by_value() {
    let kf = kind_features();
    let row =
        |post_input, approvals: &str, watch, input: &str, feed: &str, interrupt, attach: &str| {
            RcKindFeatures {
                post_input,
                approvals: approvals.to_string(),
                watch,
                input: input.to_string(),
                feed: feed.to_string(),
                interrupt,
                attach: attach.to_string(),
            }
        };
    let want = [
        (
            "claude-rc",
            row(true, "tui", false, "", "activity", false, "tmux"),
        ),
        (
            "codex",
            row(true, "tui", true, "gated", "messages", false, "tmux"),
        ),
        (
            "opencode",
            row(true, "remote", true, "turn", "messages", true, "tmux"),
        ),
        (
            "cursor",
            row(true, "tui", true, "gated", "messages", false, "tmux"),
        ),
    ];
    for (kind, want) in &want {
        assert_eq!(kf.get(*kind), Some(want), "kind_features[{kind}]");
    }
    // An absent entry is the wire's way of saying "no feed/input/approval
    // affordances" — a client must keep reading it that way rather than expecting
    // an all-false row.
    for kind in ["claude-broker", "shell"] {
        assert!(!kf.contains_key(kind), "kind_features must omit {kind}");
    }
    assert_eq!(kf.len(), want.len(), "kind_features row count");
}

/// The deprecation invariant: `watch` is the deprecated spelling of
/// `feed == "messages"`, and the producer holds them in lockstep so a v1 client
/// reading `watch` and a v2 client reading `feed` never disagree.
#[test]
fn watch_and_feed_stay_in_lockstep() {
    for (kind, features) in kind_features() {
        assert_eq!(
            features.watch,
            features.feed == "messages",
            "{kind}: watch={} feed={}",
            features.watch,
            features.feed
        );
    }
}

/// The feature tokens are a PUBLIC contract — clients gate behavior on them — so
/// adding, renaming or reordering one must be a deliberate edit here.
#[test]
fn features_by_value_and_order() {
    assert_eq!(
        CAPABILITY_FEATURES,
        [
            "generic-perm",
            "plan-stdin",
            "prompt-b64",
            "serve",
            "activity",
            "messages",
            "contract-v2",
        ]
    );
    let caps = build_capabilities(&(Arc::new(|_: &str| absent()) as AgentProbe), None);
    assert_eq!(caps.rc_version, CAPABILITY_VERSION);
    assert_eq!(caps.rc_version, 4, "the wire's advertised schema version");
    assert_eq!(
        caps.features.iter().map(String::as_str).collect::<Vec<_>>(),
        CAPABILITY_FEATURES
    );
}

/// `kinds` is ORDERED on the wire and the order is the registry's.
#[test]
fn kinds_are_the_registry_order() {
    let caps = build_capabilities(&(Arc::new(|_: &str| absent()) as AgentProbe), None);
    assert_eq!(
        caps.kinds
            .iter()
            .map(RcKind::as_str)
            .collect::<Vec<_>>()
            .join(","),
        "claude-broker,claude-rc,codex,opencode,cursor,shell"
    );
}

/// One probe per TOOL, not per kind: claude backs two kinds and is probed once,
/// and shell (no binary) is not probed at all.
#[test]
fn agents_are_probed_once_per_tool_and_shell_is_skipped() {
    let seen = Arc::new(std::sync::Mutex::new(Vec::new()));
    let recorder = {
        let seen = Arc::clone(&seen);
        Arc::new(move |bin: &str| {
            seen.lock().unwrap().push(bin.to_string());
            info("1.0.0")
        })
    };
    let caps = build_capabilities(&(recorder as AgentProbe), None);
    let mut probed = seen.lock().unwrap().clone();
    probed.sort();
    assert_eq!(probed, ["claude", "codex", "cursor-agent", "opencode"]);
    let mut tools: Vec<_> = caps.agents.keys().cloned().collect();
    tools.sort();
    assert_eq!(tools, ["claude", "codex", "cursor", "opencode"]);
    assert!(!caps.agents.contains_key("shell"));
}

// --- the budget (capabilities_budget_test.go) -------------------------------

/// One hung `--version` must not stall assembly: the laggard degrades to the fast
/// installed-only result (version omitted), everything else keeps its full
/// result, and the whole call returns within the budget plus slack.
#[test]
fn a_slow_full_probe_degrades_within_the_budget() {
    let latch = Latch::new();
    let probe = {
        let latch = latch.clone();
        Arc::new(move |bin: &str| {
            if bin == "codex" {
                latch.wait();
                return info("9.9.9");
            }
            info("1.0.0")
        })
    };
    let installed = Arc::new(|bin: &str| bin == "codex");

    let start = Instant::now();
    let caps = build_capabilities(&(probe as AgentProbe), Some(&(installed as InstalledProbe)));
    let elapsed = start.elapsed();
    latch.release();

    assert!(
        elapsed <= PROBE_BUDGET + Duration::from_millis(500),
        "assembly took {elapsed:?}, must stay inside the {PROBE_BUDGET:?} budget"
    );
    let codex = caps.agents.get("codex").expect("codex present");
    assert!(codex.installed, "the fast check still reports installed");
    assert_eq!(codex.version, None, "a laggard omits its version");
    for tool in ["claude", "opencode", "cursor"] {
        assert_eq!(
            caps.agents.get(tool),
            Some(&info("1.0.0")),
            "{tool} should keep its full probe result"
        );
    }
}

/// When BOTH probes hang (a stuck login shell), assembly still returns inside the
/// budget and the agent degrades to `installed: false` — the fallback must never
/// run synchronously after expiry.
#[test]
fn a_hung_installed_probe_degrades_to_not_installed() {
    let latch = Latch::new();
    let probe = {
        let latch = latch.clone();
        Arc::new(move |bin: &str| {
            if bin == "codex" {
                latch.wait();
                return info("9.9.9");
            }
            info("1.0.0")
        })
    };
    let installed = {
        let latch = latch.clone();
        Arc::new(move |bin: &str| {
            if bin == "codex" {
                latch.wait();
            }
            true
        })
    };

    let start = Instant::now();
    let caps = build_capabilities(&(probe as AgentProbe), Some(&(installed as InstalledProbe)));
    let elapsed = start.elapsed();
    latch.release();

    assert!(
        elapsed <= PROBE_BUDGET + Duration::from_millis(500),
        "assembly took {elapsed:?} with a hung installed probe"
    );
    assert_eq!(caps.agents.get("codex"), Some(&absent()));
    for tool in ["claude", "opencode", "cursor"] {
        assert_eq!(caps.agents.get(tool), Some(&info("1.0.0")));
    }
}

/// With no fast fallback injected, a budget-exhausted agent reports not-installed
/// (and never blocks).
#[test]
fn a_nil_installed_fallback_yields_not_installed() {
    let latch = Latch::new();
    let probe = {
        let latch = latch.clone();
        Arc::new(move |bin: &str| {
            if bin == "codex" {
                latch.wait();
            }
            info("1.0.0")
        })
    };
    let caps = build_capabilities(&(probe as AgentProbe), None);
    latch.release();
    assert_eq!(caps.agents.get("codex"), Some(&absent()));
}

/// An agent that is simply absent reports `installed: false` with NO version key
/// — Go's `omitempty`, which the DTO spells as `None`.
#[test]
fn an_absent_agent_omits_its_version() {
    let caps = build_capabilities(&(Arc::new(|_: &str| absent()) as AgentProbe), None);
    let json = serde_json::to_string(caps.agents.get("claude").unwrap()).unwrap();
    assert_eq!(json, r#"{"installed":false}"#);
}

// --- version parsing (capabilities.go:294, clirc.go:497) --------------------

#[test]
fn parse_agent_version_matches_gos_regex() {
    let cases = [
        // Go's own doc examples.
        ("2.1.196 (Claude Code)", "2.1.196"),
        ("codex-cli 0.142.4", "0.142.4"),
        ("1.17.11", "1.17.11"),
        ("v2026.07.09-a3815c0", "2026.07.09-a3815c0"),
        // Two-component and suffixed forms.
        ("opencode 1.18", "1.18"),
        ("1.2.3-beta.4+build", "1.2.3-beta.4"),
        ("  cursor-agent 2026.08.11  ", "2026.08.11"),
        // Leftmost match wins.
        ("built 1.2 with 3.4", "1.2"),
        // No version-shaped token: the trimmed FIRST line.
        ("not a version\nsecond line", "not a version"),
        ("", ""),
        ("dev", "dev"),
    ];
    for (raw, want) in cases {
        assert_eq!(parse_agent_version(raw), want, "parsing {raw:?}");
    }
}

/// Login-shell profiles can print their own version-shaped noise BEFORE the
/// command runs, so the parse is anchored to the LAST non-empty line.
#[test]
fn version_is_read_from_the_last_non_empty_line() {
    assert_eq!(
        parse_version_from_login_shell("mise 2026.7.0\nclaude 2.1.196 (Claude Code)\n\n"),
        "2.1.196"
    );
    assert_eq!(parse_version_from_login_shell(""), "");
    assert_eq!(parse_version_from_login_shell("\n\n  \n"), "");
}
