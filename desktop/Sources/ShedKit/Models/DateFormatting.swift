// DateFormatting.swift
//
// Tolerant timestamp parsing. shed-server renders timestamps in more than
// one shape (`2026-05-31T13:33:00.884935839-05:00` with a nanosecond
// fraction + local offset for created_at; plain `...Z` UTC for started_at),
// so a single fixed formatter would silently fail on one of them. We carry
// timestamps as strings in the model and parse only for display.

import Foundation

public enum DateFormatting {
    // Formatters are created per-call rather than cached in a static: an
    // ISO8601DateFormatter is not Sendable, and parsing happens only on the
    // (main-actor) display path, so the allocation cost is irrelevant.
    private static func makeFormatter(fractional: Bool) -> ISO8601DateFormatter {
        let f = ISO8601DateFormatter()
        f.formatOptions = fractional
            ? [.withInternetDateTime, .withFractionalSeconds]
            : [.withInternetDateTime]
        return f
    }

    /// Parse a shed-server timestamp string, trying the fractional-seconds
    /// form, then the plain form, then a form with a trailing ` (UTC)`
    /// annotation stripped. Returns nil on no match.
    public static func parseFlexibleTimestamp(_ raw: String) -> Date? {
        let trimmed = raw.trimmingCharacters(in: .whitespaces)
        if trimmed.isEmpty { return nil }
        if let d = makeFormatter(fractional: true).date(from: trimmed) { return d }
        if let d = makeFormatter(fractional: false).date(from: trimmed) { return d }
        if trimmed.hasSuffix(" (UTC)") {
            return parseFlexibleTimestamp(String(trimmed.dropLast(" (UTC)".count)))
        }
        return nil
    }

    /// Current time as a plain ISO8601 UTC string (the wire `ts` format).
    public static func nowISO8601() -> String {
        makeFormatter(fractional: false).string(from: Date())
    }

    /// `HH:mm:ss` of a flexible timestamp string, for the activity feed.
    public static func shortTime(_ raw: String) -> String {
        guard let d = parseFlexibleTimestamp(raw) else { return "" }
        let f = DateFormatter()
        f.dateFormat = "HH:mm:ss"
        return f.string(from: d)
    }

    /// A short "3h" / "2d" style duration between `date` and `now`.
    public static func shortRelative(_ date: Date, now: Date = Date()) -> String {
        let seconds = max(0, now.timeIntervalSince(date))
        if seconds < 90 { return "\(Int(seconds))s" }
        let minutes = seconds / 60
        if minutes < 90 { return "\(Int(minutes))m" }
        let hours = minutes / 60
        if hours < 36 { return "\(Int(hours))h" }
        let days = hours / 24
        return "\(Int(days))d"
    }
}
