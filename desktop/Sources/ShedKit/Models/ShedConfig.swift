// ShedConfig.swift
//
// Parser for ~/.shed/config.yaml — the multi-host server list shed and
// shed-remote-agent both read. We only need a narrow, machine-generated
// shape (servers: {NAME: {host, http_port, ssh_port}} + default_server),
// so a small indentation-aware reader scoped to that schema beats taking
// on a YAML dependency.

import Foundation

/// The credential shape a server issues. Mirrors `ShedRustCore.AuthMode` /
/// Go `config.AuthMode*`, kept in ShedKit so config, clients and tests can name
/// it without importing the FFI (the bridging init lives in
/// `Net/CredentialModeObserver.swift`).
public enum ShedAuthMode: String, Sendable, Equatable {
    case token
    case mtls

    /// Decode a config/wire value. Absent, empty, the legacy `"secure"` spelling
    /// and any UNRECOGNIZED value all decode as `.token` — the Go/Rust rule for
    /// a STORED entry (`ABSENT MEANS TOKEN`; an entry predating certificates is
    /// a token server). This is the config-cache rule and is deliberately NOT
    /// the rule for a live `credential.response`, where an unknown mode is
    /// refused outright (`CredentialResponse.validatedCredential(for:)`).
    public init(configValue: String?) {
        self = configValue?.trimmingCharacters(in: .whitespaces) == "mtls" ? .mtls : .token
    }
}

public struct ShedServerEntry: Sendable, Equatable {
    public let name: String
    public let host: String
    public let httpPort: Int
    public let sshPort: Int
    /// Control-scoped bearer token for the HTTP API. Empty when the server
    /// isn't token-gated.
    public let controlToken: String
    /// HTTPS control-plane URL (api_url); overrides host+httpPort when set.
    public let apiURL: String
    /// Pinned TLS cert fingerprint ("sha256:<hex>"); empty for plain HTTP.
    public let tlsCertFingerprint: String
    /// The RAW `auth_mode` value: the credential shape the server issued at the
    /// server's last bootstrap, as the CLI cached it. **Absent means token** —
    /// every entry written before client certificates existed omits the key and
    /// all of those are token/open servers. Read-only, like the rest of this
    /// parser: the desktop never writes config.yaml (plan 002 §7 P1).
    public let authModeValue: String

    /// The parsed credential shape (absent/unknown ⇒ `.token`, Go/Rust rule).
    public var authMode: ShedAuthMode { ShedAuthMode(configValue: authModeValue) }

    public init(name: String, host: String, httpPort: Int, sshPort: Int, controlToken: String = "", apiURL: String = "", tlsCertFingerprint: String = "", authModeValue: String = "") {
        self.name = name
        self.host = host
        self.httpPort = httpPort
        self.sshPort = sshPort
        self.controlToken = controlToken
        self.apiURL = apiURL
        self.tlsCertFingerprint = tlsCertFingerprint
        self.authModeValue = authModeValue
    }
}

/// The resolved control-plane endpoint + TLS pin for a server entry.
public struct ResolvedEndpoint: Equatable, Sendable {
    public let baseURL: URL
    public let pin: String
    public init(baseURL: URL, pin: String) {
        self.baseURL = baseURL
        self.pin = pin
    }
}

extension ShedServerEntry {
    /// Resolve the control-plane endpoint the client should use: the https
    /// `api_url` (with its pinned cert) when set, else plain `http://host:port`.
    /// Pure, so both startup and reconnect build clients through one tested path
    /// — this is the resolution whose absence caused a stale build to dial the
    /// dead `:8080` instead of the secure `:8443`.
    public func resolvedEndpoint() -> ResolvedEndpoint {
        if !apiURL.isEmpty, let url = URL(string: apiURL),
            let scheme = url.scheme?.lowercased(), scheme == "http" || scheme == "https",
            url.host != nil {
            return ResolvedEndpoint(baseURL: url, pin: tlsCertFingerprint)
        }
        let fallback = URL(string: "http://\(host):\(httpPort)") ?? URL(string: "http://localhost")!
        return ResolvedEndpoint(baseURL: fallback, pin: tlsCertFingerprint)
    }
}

public struct ShedConfig: Sendable, Equatable {
    public let servers: [ShedServerEntry]
    public let defaultServer: String?

    public init(servers: [ShedServerEntry], defaultServer: String?) {
        self.servers = servers
        self.defaultServer = defaultServer
    }

    public static let empty = ShedConfig(servers: [], defaultServer: nil)

    /// Load + parse the config at `path`. Missing file → empty config (a
    /// degraded but non-fatal state the dashboard surfaces, never a crash).
    public static func load(path: String) -> ShedConfig {
        guard let text = try? String(contentsOfFile: path, encoding: .utf8) else {
            return .empty
        }
        return parse(text)
    }

    public static func parse(_ text: String) -> ShedConfig {
        guard case let .map(top) = YAMLLite.parse(text) else { return .empty }
        var entries: [ShedServerEntry] = []
        if case let .map(servers)? = top["servers"] {
            for (name, value) in servers {
                guard case let .map(fields) = value else { continue }
                let host = fields["host"]?.scalar ?? name
                let httpPort = fields["http_port"]?.scalar.flatMap { Int($0) } ?? 8080
                let sshPort = fields["ssh_port"]?.scalar.flatMap { Int($0) } ?? 22
                let controlToken = fields["control_token"]?.scalar ?? ""
                let apiURL = fields["api_url"]?.scalar ?? ""
                // Canonicalize to the lowercase "sha256:<hex>" the server emits,
                // so a hand-edited upper/mixed-case pin still matches at handshake
                // time rather than silently failing every connection.
                let tlsCertFingerprint = fields["tls_cert_fingerprint"]?.scalar?.lowercased() ?? ""
                // Lowercased to match the Rust parser (`config.rs`), which
                // stores the raw value lowercased; interpretation is
                // ShedAuthMode's.
                let authModeValue = fields["auth_mode"]?.scalar?.lowercased() ?? ""
                entries.append(ShedServerEntry(name: name, host: host, httpPort: httpPort, sshPort: sshPort, controlToken: controlToken, apiURL: apiURL, tlsCertFingerprint: tlsCertFingerprint, authModeValue: authModeValue))
            }
        }
        entries.sort { $0.name < $1.name }
        return ShedConfig(servers: entries, defaultServer: top["default_server"]?.scalar)
    }
}

/// A deliberately tiny indentation-based reader. Handles exactly what
/// ~/.shed/config.yaml contains: nested maps and scalar leaves. Inline
/// `{}` is treated as an empty map; comments (`#`) and blanks are skipped.
enum YAMLLite {
    indirect enum Node: Equatable {
        case map([String: Node])
        case scalar(String)

        var scalar: String? {
            if case let .scalar(s) = self { return s }
            return nil
        }
    }

    private struct Line {
        let indent: Int
        let key: String
        let value: String?  // nil → nested block follows
    }

    static func parse(_ text: String) -> Node {
        let lines: [Line] = text.split(separator: "\n", omittingEmptySubsequences: false).compactMap { raw in
            let line = String(raw)
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.isEmpty || trimmed.hasPrefix("#") { return nil }
            let indent = line.prefix { $0 == " " }.count
            guard let colon = line.firstIndex(of: ":") else { return nil }
            let key = String(line[line.startIndex..<colon]).trimmingCharacters(in: .whitespaces)
            var rest = String(line[line.index(after: colon)...]).trimmingCharacters(in: .whitespaces)
            // Strip an inline comment after a scalar value.
            if let hash = rest.firstIndex(of: "#") { rest = String(rest[rest.startIndex..<hash]).trimmingCharacters(in: .whitespaces) }
            let value: String? = (rest.isEmpty || rest == "{}") ? nil : unquote(rest)
            return Line(indent: indent, key: unquote(key), value: value)
        }
        var index = 0
        return build(lines, &index, parentIndent: -1)
    }

    private static func build(_ lines: [Line], _ index: inout Int, parentIndent: Int) -> Node {
        var map: [String: Node] = [:]
        guard index < lines.count else { return .map(map) }
        let childIndent = lines[index].indent
        while index < lines.count {
            let line = lines[index]
            if line.indent <= parentIndent { break }
            if line.indent != childIndent {
                // Skip lines deeper than expected without a parent (defensive).
                index += 1
                continue
            }
            if let value = line.value {
                map[line.key] = .scalar(value)
                index += 1
            } else {
                index += 1
                let child = build(lines, &index, parentIndent: childIndent)
                map[line.key] = child
            }
        }
        return .map(map)
    }

    private static func unquote(_ s: String) -> String {
        if s.count >= 2, (s.hasPrefix("\"") && s.hasSuffix("\"")) || (s.hasPrefix("'") && s.hasSuffix("'")) {
            return String(s.dropFirst().dropLast())
        }
        return s
    }
}
