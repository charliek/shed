// AuditStore.swift — the app's own append-only audit log (M3, spec §FR-6).
//
// A superset of the host agent's JSON-lines log: it adds the app's own
// approval decisions (with the policy applied) and lifecycle/RC events.
// Append-only on disk; a bounded in-memory tail backs the Activity feed.

import Foundation

public final class AuditStore: @unchecked Sendable {
    public let fileURL: URL
    private let lock = NSLock()
    private var tail: [AuditEntry] = []
    private let tailLimit: Int
    private let encoder = JSONEncoder()

    public init(path: String, tailLimit: Int = 500) {
        self.fileURL = URL(fileURLWithPath: path)
        self.tailLimit = tailLimit
        try? FileManager.default.createDirectory(
            at: fileURL.deletingLastPathComponent(), withIntermediateDirectories: true)
    }

    public func append(_ entry: AuditEntry) {
        // One lock across the in-memory tail AND the encode + file write, so
        // concurrent appends can't interleave JSONL lines or race the
        // file's first-write.
        lock.lock()
        defer { lock.unlock() }
        tail.append(entry)
        if tail.count > tailLimit { tail.removeFirst(tail.count - tailLimit) }
        guard var data = try? encoder.encode(entry) else { return }
        data.append(0x0a)
        appendToFile(data)
    }

    /// Most-recent-first tail for the Activity feed.
    public func recent(limit: Int = 200) -> [AuditEntry] {
        lock.lock(); defer { lock.unlock() }
        return Array(tail.suffix(limit).reversed())
    }

    private func appendToFile(_ data: Data) {
        if let handle = try? FileHandle(forWritingTo: fileURL) {
            defer { try? handle.close() }
            _ = try? handle.seekToEnd()
            try? handle.write(contentsOf: data)
        } else {
            // File doesn't exist yet.
            try? data.write(to: fileURL, options: .atomic)
        }
    }
}
