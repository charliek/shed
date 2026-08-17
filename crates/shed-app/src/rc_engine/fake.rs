//! The test-only seam doubles — the Rust twins of Go's `fakeTmux`
//! (`internal/ext/rc/rc_test.go:15-35`) and `envFrom` (`plan_test.go:12`).
//!
//! Shared by the [`super::tmux`], [`super::ops`] and [`super::plan`] test modules
//! so all three assert against the SAME recorded-argv shape the Go tables assert
//! against, and drive the same environment seam.

use std::cell::RefCell;
use std::collections::HashMap;

use super::tmux::{TmuxResult, TmuxRunner};

type Handler = Box<dyn Fn(&[&str]) -> TmuxResult>;

pub(crate) struct FakeTmux {
    calls: RefCell<Vec<Vec<String>>>,
    handler: Handler,
}

impl FakeTmux {
    /// A fake answering through `handler` (which receives the raw argv).
    pub(crate) fn new(handler: impl Fn(&[&str]) -> TmuxResult + 'static) -> Self {
        Self {
            calls: RefCell::new(Vec::new()),
            handler: Box::new(handler),
        }
    }

    /// A fake where every verb succeeds with empty output — Go's
    /// `&fakeTmux{handler: func(...) Result { return Result{Code: 0} }}`.
    pub(crate) fn ok() -> Self {
        Self::new(|_| TmuxResult::default())
    }

    /// Every recorded argv, in call order.
    pub(crate) fn calls(&self) -> Vec<Vec<String>> {
        self.calls.borrow().clone()
    }

    /// The FIRST recorded call whose verb is `verb` (Go's `callWith`).
    pub(crate) fn call_with(&self, verb: &str) -> Option<Vec<String>> {
        self.calls
            .borrow()
            .iter()
            .find(|c| c.first().map(String::as_str) == Some(verb))
            .cloned()
    }

    /// How many recorded calls used `verb` — the keystroke-count pins (exactly
    /// one Enter for a trust accept, …).
    pub(crate) fn count_with(&self, verb: &str) -> usize {
        self.calls
            .borrow()
            .iter()
            .filter(|c| c.first().map(String::as_str) == Some(verb))
            .count()
    }

    /// Whether any recorded call carried `arg` anywhere in its argv (Go's
    /// `containsArg`).
    pub(crate) fn any_arg(&self, arg: &str) -> bool {
        self.calls
            .borrow()
            .iter()
            .any(|c| c.iter().any(|a| a == arg))
    }
}

impl TmuxRunner for FakeTmux {
    fn run(&self, args: &[&str]) -> TmuxResult {
        self.calls
            .borrow_mut()
            .push(args.iter().map(|s| (*s).to_string()).collect());
        (self.handler)(args)
    }
}

/// A [`super::ops::GetEnv`] body over a fixed table, unset keys reading `""`
/// (Go's `envFrom`, `plan_test.go:12`).
pub(crate) fn env_from(pairs: &[(&str, &str)]) -> impl Fn(&str) -> String {
    let map: HashMap<String, String> = pairs
        .iter()
        .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
        .collect();
    move |key: &str| map.get(key).cloned().unwrap_or_default()
}

/// The common case: a table carrying only `HOME`.
pub(crate) fn home_env(home: &str) -> impl Fn(&str) -> String {
    env_from(&[("HOME", home)])
}
