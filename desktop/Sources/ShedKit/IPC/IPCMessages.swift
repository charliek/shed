// IPCMessages.swift
//
// Swift Codable types for the newline-delimited JSON IPC protocol the app
// exposes over its Unix-domain control socket. Ported from roost's
// IPCMessages.swift — same envelope shape and wire rules:
//   * Request ids are int64 wrapped as strings (JSON numbers lose
//     precision past 2^53).
//   * Server-side request structs reject unknown fields; result/event
//     structs are permissive.
//   * `params` / `result` are untyped JSON (AnyCodable) decoded per-op.

import Foundation

// MARK: - Envelopes

public struct IPCRequest: Codable, Sendable {
    public var id: Int64
    public var op: String
    public var params: AnyCodable?

    enum CodingKeys: String, CodingKey { case id, op, params }

    public init(id: Int64, op: String, params: AnyCodable? = nil) {
        self.id = id
        self.op = op
        self.params = params
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        // Reject unknown top-level keys (matches the strict-typed clients
        // a future cross-language driver would use). Op-specific params get
        // the same treatment via decodeParams(expected:).
        let allowed: Set<String> = ["id", "op", "params"]
        let present = Set(c.allKeys.map(\.stringValue))
        let unknown = present.subtracting(allowed)
        if !unknown.isEmpty {
            throw DecodingError.dataCorrupted(.init(
                codingPath: c.codingPath,
                debugDescription: "unknown request fields: \(unknown.sorted().joined(separator: ", "))"
            ))
        }
        self.id = try decodeStringInt64(c, .id)
        self.op = try c.decode(String.self, forKey: .op)
        self.params = try c.decodeIfPresent(AnyCodable.self, forKey: .params)
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try encodeStringInt64(&c, .id, id)
        try c.encode(op, forKey: .op)
        try c.encodeIfPresent(params, forKey: .params)
    }
}

public struct IPCResponse: Codable, Sendable {
    public var id: Int64
    public var ok: Bool
    public var result: AnyCodable?
    public var error: IPCResponseError?

    enum CodingKeys: String, CodingKey { case id, ok, result, error }

    public init(id: Int64, ok: Bool, result: AnyCodable? = nil, error: IPCResponseError? = nil) {
        self.id = id
        self.ok = ok
        self.result = result
        self.error = error
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.id = try decodeStringInt64(c, .id)
        self.ok = try c.decode(Bool.self, forKey: .ok)
        self.result = try c.decodeIfPresent(AnyCodable.self, forKey: .result)
        self.error = try c.decodeIfPresent(IPCResponseError.self, forKey: .error)
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try encodeStringInt64(&c, .id, id)
        try c.encode(ok, forKey: .ok)
        try c.encodeIfPresent(result, forKey: .result)
        try c.encodeIfPresent(error, forKey: .error)
    }

    public static func success(id: Int64, result: AnyCodable?) -> IPCResponse {
        IPCResponse(id: id, ok: true, result: result, error: nil)
    }

    public static func failure(id: Int64, code: String, message: String) -> IPCResponse {
        IPCResponse(id: id, ok: false, result: nil, error: IPCResponseError(code: code, message: message))
    }
}

public struct IPCResponseError: Codable, Equatable, Sendable {
    public var code: String
    public var message: String
    public init(code: String, message: String) {
        self.code = code
        self.message = message
    }
}

// MARK: - String-wrapped int64 helpers

enum StringInt64DecodeError: Error, CustomStringConvertible {
    case notInt64(field: String, value: String)
    var description: String {
        switch self {
        case .notInt64(let f, let v): return "\(f): not a valid int64: \(v)"
        }
    }
}

func decodeStringInt64<K: CodingKey>(_ c: KeyedDecodingContainer<K>, _ key: K) throws -> Int64 {
    let raw = try c.decode(String.self, forKey: key)
    guard let v = Int64(raw) else {
        throw StringInt64DecodeError.notInt64(field: key.stringValue, value: raw)
    }
    return v
}

func encodeStringInt64<K: CodingKey>(
    _ c: inout KeyedEncodingContainer<K>, _ key: K, _ value: Int64
) throws {
    try c.encode(String(value), forKey: key)
}

// MARK: - AnyCodable (untyped JSON value)

/// Loose JSON value wrapper for `params` / `result`.
///
/// `@unchecked Sendable` because `Any` can't be `Sendable` in Swift 6
/// strict mode, but we treat this purely as opaque JSON — set at
/// decode/encode time and never mutated after.
public struct AnyCodable: Codable, @unchecked Sendable {
    public let value: Any

    public init(_ value: Any) { self.value = value }

    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() {
            self.value = NSNull()
        } else if let b = try? c.decode(Bool.self) {
            self.value = b
        } else if let i = try? c.decode(Int64.self) {
            self.value = i
        } else if let d = try? c.decode(Double.self) {
            self.value = d
        } else if let s = try? c.decode(String.self) {
            self.value = s
        } else if let arr = try? c.decode([AnyCodable].self) {
            self.value = arr.map { $0.value }
        } else if let obj = try? c.decode([String: AnyCodable].self) {
            self.value = obj.mapValues { $0.value }
        } else {
            throw DecodingError.dataCorruptedError(in: c, debugDescription: "Unsupported JSON type")
        }
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        try Self.encodeValue(value, into: &c)
    }

    private static func encodeValue(_ value: Any, into c: inout SingleValueEncodingContainer) throws {
        // CRITICAL: NSNumber must be disambiguated BEFORE the `as? Bool` /
        // `as? Int64` cascade. JSONSerialization boxes JSON numbers as
        // NSNumber, and `NSNumber(value: 1) as? Bool` returns
        // Optional(true) — the classic symptom is `protocol_version: 1`
        // serializing as `true` on the wire. CFGetTypeID vs
        // CFBooleanGetTypeID distinguishes "actually a Bool" from "an
        // integer that bridges to Bool".
        if value is NSNull {
            try c.encodeNil()
        } else if let n = value as? NSNumber {
            let typeID = CFGetTypeID(n)
            if typeID == CFBooleanGetTypeID() {
                try c.encode(n.boolValue)
            } else if CFNumberIsFloatType(n) {
                try c.encode(n.doubleValue)
            } else {
                try c.encode(n.int64Value)
            }
        } else if let b = value as? Bool {
            try c.encode(b)
        } else if let i = value as? Int64 {
            try c.encode(i)
        } else if let i = value as? Int {
            try c.encode(Int64(i))
        } else if let d = value as? Double {
            try c.encode(d)
        } else if let s = value as? String {
            try c.encode(s)
        } else if let arr = value as? [Any] {
            try c.encode(arr.map(AnyCodable.init))
        } else if let dict = value as? [String: Any] {
            try c.encode(dict.mapValues(AnyCodable.init))
        } else {
            throw EncodingError.invalidValue(value, EncodingError.Context(
                codingPath: c.codingPath, debugDescription: "Unsupported JSON value"))
        }
    }
}

/// Protocol version on the wire. M0 ships `1`.
public let ipcProtocolVersion: UInt32 = 1

/// Maximum length of a single framed line (16 MiB).
public let ipcMaxFrameBytes: Int = 16 * 1024 * 1024
