// DiagnosticLog — an append-only, size-rotated diagnostic log for ShedDesktop,
// written to BundleProfile.logPath and surfaced via "Reveal diagnostic log".
//
// Distinct from the credential AuditStore (product/audit JSONL): this is plain
// operational support data — config resolution, per-host reachability, control-
// token mint outcomes, TLS pinning — the breadcrumbs that turn a "why is this
// host unreachable?" investigation into a one-line answer.
//
// Writes are serialized on a private queue (rotation + append are atomic
// relative to each other); token-shaped strings are redacted so a bearer token
// or cert fingerprint never lands on disk, even if a caller interpolates one. A
// best-effort os.Logger mirror keeps unified logging working too.

import Foundation
import OSLog

public final class DiagnosticLog: @unchecked Sendable {
    public enum Level: String, Sendable {
        case debug = "DEBUG"
        case info = "INFO"
        case warn = "WARN"
        case error = "ERROR"
    }

    private let path: String
    private let maxBytes: Int
    private let keep: Int
    private let now: @Sendable () -> Date
    private let mirror: (@Sendable (Level, String) -> Void)?
    private let queue = DispatchQueue(label: "ai.stridelabs.ShedDesktop.diaglog")
    // ISO8601DateFormatter isn't thread-safe, so it is only ever touched on `queue`.
    private let iso: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    public init(
        path: String,
        maxBytes: Int = 5 * 1024 * 1024,
        keep: Int = 3,
        now: @escaping @Sendable () -> Date = Date.init,
        mirror: (@Sendable (Level, String) -> Void)? = DiagnosticLog.osLogMirror
    ) {
        self.path = path
        self.maxBytes = maxBytes
        self.keep = max(1, keep)
        self.now = now
        self.mirror = mirror
    }

    /// Log one line. Returns immediately; the disk write is serialized off-thread.
    public func log(
        _ level: Level, _ component: String, _ message: String, _ fields: [(String, String)] = []
    ) {
        let date = now()
        mirror?(level, Self.redact("\(component): \(message)"))
        queue.async { [weak self] in
            guard let self else { return }
            let line = Self.format(
                timestamp: self.iso.string(from: date), level: level,
                component: component, message: message, fields: fields)
            self.appendLocked(line)
        }
    }

    /// Block until queued writes have flushed — for tests and shutdown.
    public func flush() { queue.sync {} }

    // MARK: - formatting + redaction

    static func format(
        timestamp: String, level: Level, component: String,
        message: String, fields: [(String, String)]
    ) -> String {
        var s = "\(timestamp) \(level.rawValue) \(component) \(redact(message))"
        for (k, v) in fields { s += " \(k)=\(redact(v))" }
        return s + "\n"
    }

    /// Scrub token-shaped strings so a bearer token / fingerprint never lands on
    /// disk. NSRegularExpression is immutable + thread-safe, so this is callable
    /// from any thread.
    static func redact(_ s: String) -> String {
        var out = s
        for (re, repl) in redactionRules {
            out = re.stringByReplacingMatches(
                in: out, range: NSRange(out.startIndex..., in: out), withTemplate: repl)
        }
        return out
    }

    private static let redactionRules: [(NSRegularExpression, String)] = {
        func re(_ p: String) -> NSRegularExpression {
            // Patterns are compile-time constants; a bad one is a programmer error.
            guard let r = try? NSRegularExpression(pattern: p) else {
                preconditionFailure("invalid redaction pattern: \(p)")
            }
            return r
        }
        return [
            (re(#"Bearer\s+[A-Za-z0-9._\-]+"#), "Bearer [REDACTED]"),
            (re(#"shed_(?:control|credentials)_[A-Za-z0-9._\-]+"#), "[REDACTED-TOKEN]"),
            (re(#"\b[0-9a-fA-F]{32,}\b"#), "[REDACTED-HEX]"),
        ]
    }()

    // MARK: - file write + rotation (queue-confined)

    private func appendLocked(_ line: String) {
        rotateIfNeeded()
        ensureDir()
        guard let data = line.data(using: .utf8) else { return }
        if let h = FileHandle(forWritingAtPath: path) {
            defer { try? h.close() }
            _ = try? h.seekToEnd()
            try? h.write(contentsOf: data)
        } else {
            try? data.write(to: URL(fileURLWithPath: path), options: .atomic)
        }
    }

    private func ensureDir() {
        let dir = (path as NSString).deletingLastPathComponent
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
    }

    private func rotateIfNeeded() {
        let fm = FileManager.default
        guard let size = (try? fm.attributesOfItem(atPath: path))?[.size] as? Int, size >= maxBytes
        else { return }
        // Shift base -> .1 -> .2 ... dropping the oldest beyond `keep`.
        try? fm.removeItem(atPath: "\(path).\(keep)")
        var i = keep - 1
        while i >= 1 {
            let from = "\(path).\(i)"
            if fm.fileExists(atPath: from) { try? fm.moveItem(atPath: from, toPath: "\(path).\(i + 1)") }
            i -= 1
        }
        try? fm.moveItem(atPath: path, toPath: "\(path).1")
    }

    // MARK: - os.Logger mirror

    private static let osLog = Logger(subsystem: "ai.stridelabs.ShedDesktop", category: "diagnostics")
    public static let osLogMirror: @Sendable (Level, String) -> Void = { level, msg in
        switch level {
        case .debug: osLog.debug("\(msg, privacy: .public)")
        case .info: osLog.info("\(msg, privacy: .public)")
        case .warn: osLog.warning("\(msg, privacy: .public)")
        case .error: osLog.error("\(msg, privacy: .public)")
        }
    }
}
