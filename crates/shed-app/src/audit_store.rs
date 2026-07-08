//! The app's own append-only audit log, ported from `AuditStore.swift`.
//!
//! A superset of the host agent's JSON-lines log: it adds the app's own approval
//! decisions (with the policy applied) and the streamed host events. Append-only
//! on disk (best-effort — a write failure must never block a decision, F9); a
//! bounded in-memory tail backs the Activity feed.
//!
//! Owned by the single-task coordinator actor, so no internal locking is needed
//! (all access is already serialized through the actor).

use std::collections::VecDeque;
use std::fs::OpenOptions;
use std::io::Write;
use std::path::{Path, PathBuf};

use shed_core::approval::AuditEntry;

const DEFAULT_TAIL_LIMIT: usize = 500;

pub struct AuditStore {
    file_path: PathBuf,
    tail: VecDeque<AuditEntry>,
    tail_limit: usize,
}

impl AuditStore {
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self::with_tail_limit(path, DEFAULT_TAIL_LIMIT)
    }

    pub fn with_tail_limit(path: impl Into<PathBuf>, tail_limit: usize) -> Self {
        let file_path = path.into();
        if let Some(parent) = file_path.parent() {
            let _ = std::fs::create_dir_all(parent);
        }
        Self {
            file_path,
            tail: VecDeque::new(),
            tail_limit,
        }
    }

    /// Append an entry to the in-memory tail and (best-effort) the JSONL file.
    /// A file-write failure (disk full, etc.) is swallowed — the decision it
    /// records must still proceed (F9: record-before-transmit, but never block).
    pub fn append(&mut self, entry: AuditEntry) {
        if let Ok(mut line) = serde_json::to_vec(&entry) {
            line.push(b'\n');
            let _ = self.append_to_file(&line);
        }
        self.tail.push_back(entry);
        while self.tail.len() > self.tail_limit {
            self.tail.pop_front();
        }
    }

    /// Most-recent-first tail for the Activity feed.
    pub fn recent(&self, limit: usize) -> Vec<AuditEntry> {
        self.tail.iter().rev().take(limit).cloned().collect()
    }

    pub fn path(&self) -> &Path {
        &self.file_path
    }

    fn append_to_file(&self, data: &[u8]) -> std::io::Result<()> {
        let mut f = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.file_path)?;
        f.write_all(data)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_core::approval::AuditSource;

    fn entry(id: &str, result: &str) -> AuditEntry {
        AuditEntry {
            id: id.into(),
            ts: "2026-07-03T00:00:00Z".into(),
            source: AuditSource::App,
            server: None,
            shed: Some("s".into()),
            ns: Some("ssh-agent".into()),
            op: Some("sign".into()),
            result: result.into(),
            detail: None,
            code: None,
            reason: None,
            approval: Some("shed-desktop".into()),
            policy: Some("manual".into()),
        }
    }

    fn temp_path() -> PathBuf {
        std::env::temp_dir().join(format!("shed-audit-{}/audit.jsonl", uuid::Uuid::new_v4()))
    }

    #[test]
    fn recent_is_most_recent_first() {
        let mut store = AuditStore::new(temp_path());
        store.append(entry("a", "ok"));
        store.append(entry("b", "denied"));
        store.append(entry("c", "ok"));
        let recent = store.recent(10);
        assert_eq!(recent.len(), 3);
        assert_eq!(recent[0].id, "c"); // most recent first
        assert_eq!(recent[2].id, "a");
    }

    #[test]
    fn tail_is_bounded() {
        let mut store = AuditStore::with_tail_limit(temp_path(), 2);
        store.append(entry("a", "ok"));
        store.append(entry("b", "ok"));
        store.append(entry("c", "ok"));
        let recent = store.recent(10);
        assert_eq!(recent.len(), 2); // "a" dropped
        assert_eq!(recent[0].id, "c");
        assert_eq!(recent[1].id, "b");
    }

    #[test]
    fn recent_respects_limit() {
        let mut store = AuditStore::new(temp_path());
        for i in 0..5 {
            store.append(entry(&format!("e{i}"), "ok"));
        }
        assert_eq!(store.recent(2).len(), 2);
    }

    #[test]
    fn writes_jsonl_to_disk() {
        let path = temp_path();
        {
            let mut store = AuditStore::new(&path);
            store.append(entry("a", "ok"));
            store.append(entry("b", "denied"));
        }
        let contents = std::fs::read_to_string(&path).unwrap();
        let lines: Vec<&str> = contents.lines().collect();
        assert_eq!(lines.len(), 2);
        // Each line is a valid AuditEntry JSON object.
        let first: AuditEntry = serde_json::from_str(lines[0]).unwrap();
        assert_eq!(first.id, "a");
        let _ = std::fs::remove_dir_all(path.parent().unwrap());
    }
}
