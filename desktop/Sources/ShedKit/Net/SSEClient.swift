// SSEClient.swift
//
// Server-Sent Events framing. A stateful, synchronous parser fed one line
// at a time, so it's trivially unit-testable and free of async/Sendable
// entanglement. The M1 create flow (ShedServerClient.createShed) feeds it
// lines split from the raw byte stream by hand — NOT
// `URLSession.bytes.lines`, whose AsyncLineSequence drops the blank lines
// SSE relies on to dispatch an event.
//
// Dialect (matches shed-server + shed-remote-agent):
//   * `event:` sets the event type for the next dispatch
//   * `data:` lines concatenate with newlines
//   * a blank line dispatches the accumulated {event, data}
//   * `:` lines are comments / keep-alive pings
//   * a final record with no trailing blank line is flushed via finish()

import Foundation

public struct SSEEvent: Sendable, Equatable {
    public let event: String
    public let data: String
    public init(event: String, data: String) {
        self.event = event
        self.data = data
    }
}

public struct SSEParser: Sendable {
    private var event = ""
    private var data = ""

    public init() {}

    /// Feed one line (without its trailing newline). Returns a record when
    /// the line is the blank line that terminates an event, else nil.
    public mutating func push(line rawLine: String) -> SSEEvent? {
        let line = rawLine.hasSuffix("\r") ? String(rawLine.dropLast()) : rawLine
        if line.isEmpty {
            return flush()
        }
        if line.hasPrefix(":") { return nil }  // comment / keep-alive
        if line.hasPrefix("event:") {
            event = String(line.dropFirst("event:".count)).trimmingCharacters(in: .whitespaces)
        } else if line.hasPrefix("data:") {
            let v = String(line.dropFirst("data:".count)).trimmingCharacters(in: .whitespaces)
            data = data.isEmpty ? v : "\(data)\n\(v)"
        }
        return nil
    }

    /// Flush any final record that lacked a trailing blank line (EOF).
    public mutating func finish() -> SSEEvent? {
        flush()
    }

    private mutating func flush() -> SSEEvent? {
        guard !event.isEmpty || !data.isEmpty else { return nil }
        let ev = SSEEvent(event: event, data: data)
        event = ""
        data = ""
        return ev
    }

    /// Convenience: parse a complete sequence of lines into all records.
    public static func parse(lines: [String]) -> [SSEEvent] {
        var parser = SSEParser()
        var out: [SSEEvent] = []
        for line in lines {
            if let ev = parser.push(line: line) { out.append(ev) }
        }
        if let ev = parser.finish() { out.append(ev) }
        return out
    }
}
