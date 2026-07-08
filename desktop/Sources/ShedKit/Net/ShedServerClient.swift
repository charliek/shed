// ShedServerClient.swift
//
// HTTP client for one shed-server instance. The base URL + URLSession are
// injectable — production builds one client per configured host; the
// hermetic E2E harness points every client at a single local mock server,
// so no real shed-server is needed and no traffic leaves the box.
//
// Decoding is defensive against the real API shapes captured as fixtures:
//   * `GET /api/sheds` returns `{"sheds": [...] | null}` — null → [].
//   * Shed objects omit most optional fields; only name/status are assumed.
//   * Timestamps mix UTC `Z` and local-offset forms, so they're carried as
//     strings (see DateFormatting.parseFlexibleTimestamp for display).

import Foundation

public enum ShedClientError: Error, CustomStringConvertible {
    case badStatus(Int)
    case transport(String)
    case decode(String)
    case create(String)

    public var description: String {
        switch self {
        case .badStatus(let c): return "shed-server returned HTTP \(c)"
        case .transport(let m): return "transport error: \(m)"
        case .decode(let m): return "decode error: \(m)"
        case .create(let m): return "create failed: \(m)"
        }
    }
}

public struct ShedServerClient: Sendable {
    public let baseURL: URL
    public let serverName: String
    private let session: URLSession
    private let token: String
    // When set (secure servers), the bearer token is minted/refreshed on demand
    // by the host agent (token.get) and re-minted on a 401. nil → the static
    // `token` String above is used as-is (open-mode / pre-bootstrap servers).
    private let tokenProvider: ControlTokenProvider?
    // Set when the client is misconfigured — a TLS pin on a non-https URL, or
    // (when the Rust core is the default) a failure to construct the Rust
    // adapter. Every request throws it instead of sending unpinned plaintext or
    // silently downgrading to the Swift path.
    private let configError: ShedClientError?
    // When SHED_DESKTOP_RUST_CORE is on, read ops delegate to the Rust shed-core
    // (nil otherwise → the URLSession path below). M2: reads only; write/create +
    // the token/pin paths stay Swift until M3/M4.
    private let rustAdapter: RustShedCoreAdapter?

    public init(baseURL: URL, serverName: String, token: String = "", tlsCertFingerprint: String = "", tokenProvider: ControlTokenProvider? = nil, session: URLSession? = nil, useRustCore: Bool = false, hostAgent: HostAgentClient? = nil) {
        self.baseURL = baseURL
        self.serverName = serverName
        self.token = token
        self.tokenProvider = tokenProvider

        // The injected `session` (the hermetic test mock) is honored only on the
        // unpinned path; a pinned build always owns its delegate-backed session.
        let resolvedSession: URLSession
        var configError: ShedClientError?
        if tlsCertFingerprint.isEmpty {
            resolvedSession = session ?? .shared
        } else if baseURL.scheme?.lowercased() == "https" {
            // Pinned TLS: a delegate-backed session verifies the leaf cert
            // against the fingerprint. The session retains its delegate until
            // invalidated; these clients live for the app session, which is
            // acceptable for the handful of configured hosts.
            let delegate = PinningSessionDelegate(fingerprint: tlsCertFingerprint)
            resolvedSession = URLSession(configuration: .ephemeral, delegate: delegate, delegateQueue: nil)
        } else {
            // Fail closed: a pin only protects https, so refuse rather than
            // silently send unpinned plaintext (mirrors the Go/sdk contract).
            resolvedSession = session ?? .shared
            configError = .transport(
                "TLS pin configured for a non-https URL \(baseURL.absoluteString); refusing to send unpinned plaintext")
        }
        self.session = resolvedSession

        // Read/lifecycle/create ops go through the Rust core when it's the active
        // backend (default-on since M0). Built from the same base URL + static
        // token + pin + host-agent minter. When the core is the default, a
        // construction failure must fail this host LOUDLY: falling back to the
        // Swift URLSession path would let `identify.core=rust` mask a silent
        // per-host Swift downgrade. (A pin-on-non-https misconfig already fails
        // closed above, so we don't also build the adapter in that case.)
        var adapter: RustShedCoreAdapter?
        if useRustCore && configError == nil {
            do {
                adapter = try RustShedCoreAdapter(
                    baseURL: baseURL.absoluteString, serverName: serverName,
                    token: token, pin: tlsCertFingerprint.isEmpty ? nil : tlsCertFingerprint,
                    hostAgent: hostAgent)
            } catch {
                configError = .transport(
                    "Rust shed-core adapter failed to construct for host \(serverName): \(error); "
                    + "refusing to silently fall back to Swift (set SHED_DESKTOP_RUST_CORE=0 to force it)")
            }
        }
        self.configError = configError
        self.rustAdapter = adapter
    }

    /// The bearer token to send. For a provider-backed (secure) client the host
    /// agent is authoritative: on ANY mint failure — down, refused, or a
    /// fail-closed pin mismatch — we send NO token rather than the static
    /// configured one. Never downgrade the secure-by-default issuance path to a
    /// legacy config token: a secure server then 401s (graceful offline); an
    /// open server accepts the tokenless request. The static `token` is used
    /// only when there is no provider (open-mode / pre-bootstrap clients).
    private func bearerToken() async -> String {
        guard let tokenProvider else { return token }
        return (try? await tokenProvider.token()) ?? ""
    }

    /// Sets the bearer token header on `req` when there is a token to send.
    private func authorize(_ req: inout URLRequest) async {
        let tok = await bearerToken()
        if !tok.isEmpty {
            req.setValue("Bearer \(tok)", forHTTPHeaderField: "Authorization")
        }
    }

    /// `GET /api/info`.
    public func info() async throws -> ServerInfo {
        if let rustAdapter { return try await rustAdapter.info() }
        let data = try await get("/api/info")
        do {
            return try JSONDecoder().decode(ServerInfo.self, from: data)
        } catch {
            throw ShedClientError.decode("\(error)")
        }
    }

    /// `GET /api/sheds` → sheds annotated with this host's config name.
    /// `{"sheds": null}` (the real empty shape) decodes to []. The server
    /// omits `host`; the `Shed` decoder tolerates that and we stamp it here.
    public func listSheds() async throws -> [Shed] {
        if let rustAdapter { return try await rustAdapter.listSheds() }
        let data = try await get("/api/sheds")
        do {
            let wrapper = try JSONDecoder().decode(ShedListWire.self, from: data)
            return (wrapper.sheds ?? []).map { var s = $0; s.host = serverName; return s }
        } catch {
            throw ShedClientError.decode("\(error)")
        }
    }

    /// `GET /api/system/df` → this server's disk usage (M7).
    public func systemDF() async throws -> SystemDiskUsage {
        if let rustAdapter { return try await rustAdapter.systemDF() }
        let data = try await get("/api/system/df")
        do {
            return try JSONDecoder().decode(SystemDiskUsage.self, from: data)
        } catch {
            throw ShedClientError.decode("\(error)")
        }
    }

    /// `GET /api/images` → this server's installed images (for the picker).
    public func listImages() async throws -> [ShedImage] {
        if let rustAdapter { return try await rustAdapter.listImages() }
        let data = try await get("/api/images")
        do {
            return try JSONDecoder().decode(ImageListWire.self, from: data).images ?? []
        } catch {
            throw ShedClientError.decode("\(error)")
        }
    }

    /// `GET /api/egress/profiles` → this server's egress profiles (config
    /// baseline + user store), each tagged with its source. Read-only.
    public func egressProfiles() async throws -> [EgressProfileInfo] {
        if let rustAdapter { return try await rustAdapter.egressProfiles() }
        let data = try await get("/api/egress/profiles")
        do {
            return try JSONDecoder().decode([EgressProfileInfo].self, from: data)
        } catch {
            throw ShedClientError.decode("\(error)")
        }
    }

    // MARK: - lifecycle (M1)

    public func start(name: String) async throws {
        if let rustAdapter { return try await rustAdapter.start(name: name) }
        try await send("POST", "/api/sheds/\(name)/start")
    }
    public func stop(name: String) async throws {
        if let rustAdapter { return try await rustAdapter.stop(name: name) }
        try await send("POST", "/api/sheds/\(name)/stop")
    }
    public func reset(name: String) async throws {
        if let rustAdapter { return try await rustAdapter.reset(name: name) }
        try await send("POST", "/api/sheds/\(name)/reset")
    }
    public func delete(name: String) async throws {
        if let rustAdapter { return try await rustAdapter.delete(name: name) }
        try await send("DELETE", "/api/sheds/\(name)")
    }

    /// `POST /api/sheds` with `Accept: text/event-stream`, surfaced as a
    /// stream of create events (`progress` messages then a final shed). The
    /// producer parses the SSE bytes; the caller consumes on its own actor.
    public func createShed(_ body: CreateShedRequest) -> AsyncThrowingStream<CreateEvent, Error> {
        if let rustAdapter { return rustAdapter.createShed(body) }
        let baseURL = self.baseURL
        let serverName = self.serverName
        let session = self.session
        let token = self.token
        let tokenProvider = self.tokenProvider
        let configError = self.configError
        // Bounded buffer: a runaway server can't grow our heap without limit
        // if the consumer lags (256 is far more than a real create streams).
        return AsyncThrowingStream(CreateEvent.self, bufferingPolicy: .bufferingNewest(256)) { continuation in
            let task = Task {
                var sawComplete = false
                do {
                    if let configError { throw configError }
                    var req = URLRequest(url: baseURL.appendingPathComponent("/api/sheds"))
                    req.httpMethod = "POST"
                    req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
                    req.setValue("application/json", forHTTPHeaderField: "Content-Type")
                    // Same auth decision as every other request: the provider's
                    // minted token, or NONE on a mint failure — never the static
                    // token (no secure-by-default downgrade). An open server accepts
                    // the tokenless request; a secure server 401s. One-shot stream,
                    // so a 401 surfaces (badStatus) rather than retrying.
                    var bearer = token
                    if let tokenProvider {
                        bearer = (try? await tokenProvider.token()) ?? ""
                    }
                    if !bearer.isEmpty {
                        req.setValue("Bearer \(bearer)", forHTTPHeaderField: "Authorization")
                    }
                    req.httpBody = try JSONEncoder().encode(body)
                    let (bytes, response) = try await session.bytes(for: req)
                    if let http = response as? HTTPURLResponse, !(200...299).contains(http.statusCode) {
                        throw ShedClientError.badStatus(http.statusCode)
                    }
                    var parser = SSEParser()
                    func handle(_ ev: SSEEvent) throws {
                        switch ev.event {
                        case "progress":
                            if let msg = decodeProgressMessage(ev.data) { continuation.yield(.progress(msg)) }
                        case "complete":
                            var shed = try JSONDecoder().decode(Shed.self, from: Data(ev.data.utf8))
                            shed.host = serverName
                            sawComplete = true
                            continuation.yield(.complete(shed))
                        case "error":
                            throw ShedClientError.create(decodeErrorMessage(ev.data))
                        default:
                            break
                        }
                    }
                    // NB: split on \n ourselves rather than using
                    // `bytes.lines` — AsyncLineSequence drops empty lines,
                    // but SSE relies on the blank line to dispatch an event.
                    var line = [UInt8]()
                    for try await byte in bytes {
                        if byte == 0x0a {
                            if let ev = parser.push(line: String(decoding: line, as: UTF8.self)) { try handle(ev) }
                            line.removeAll(keepingCapacity: true)
                        } else {
                            line.append(byte)
                        }
                    }
                    if !line.isEmpty, let ev = parser.push(line: String(decoding: line, as: UTF8.self)) { try handle(ev) }
                    if let ev = parser.finish() { try handle(ev) }
                    // A stream that ends without a complete event is a
                    // failure, not a silent success.
                    if !sawComplete { throw ShedClientError.create("stream ended before a complete event") }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    // MARK: - transport

    private func get(_ path: String) async throws -> Data {
        try await requestData("GET", path, accept: "application/json", timeout: 8)
    }

    private func send(_ method: String, _ path: String) async throws {
        _ = try await requestData(method, path, timeout: 15)
    }

    /// One place that builds the request, checks the status, and maps
    /// errors. `get`/`send` are thin wrappers; the streaming create path
    /// reuses only the status-check shape (it needs the byte stream).
    private func requestData(_ method: String, _ path: String, accept: String? = nil, timeout: TimeInterval) async throws -> Data {
        if let configError { throw configError }
        // Build + authorize + send one attempt. Factored so a 401 can retry with
        // a freshly minted token (the provider path only).
        func attempt() async throws -> Data {
            var req = URLRequest(url: baseURL.appendingPathComponent(path))
            req.httpMethod = method
            req.timeoutInterval = timeout
            if let accept { req.setValue(accept, forHTTPHeaderField: "Accept") }
            await authorize(&req)
            let (data, response) = try await session.data(for: req)
            if let http = response as? HTTPURLResponse, !(200...299).contains(http.statusCode) {
                throw ShedClientError.badStatus(http.statusCode)
            }
            return data
        }
        do {
            return try await attempt()
        } catch ShedClientError.badStatus(401) where tokenProvider != nil {
            // Stale control token: drop it, re-mint, retry once (at-most-once,
            // mirrors the SDK/CLI). Static-token clients fall through unchanged.
            await tokenProvider?.invalidate()
            return try await attempt()
        } catch let e as ShedClientError {
            throw e
        } catch {
            throw ShedClientError.transport("\(error)")
        }
    }
}

/// A streamed create event: progress lines, then the final shed.
public enum CreateEvent: Sendable {
    case progress(String)
    case complete(Shed)
}

private func decodeProgressMessage(_ data: String) -> String? {
    struct Progress: Decodable { let message: String? }
    if let p = try? JSONDecoder().decode(Progress.self, from: Data(data.utf8)), let m = p.message {
        return m
    }
    // Fall back to the raw data when it isn't the expected JSON shape.
    return data.isEmpty ? nil : data
}

private func decodeErrorMessage(_ data: String) -> String {
    struct APIError: Decodable { let code: String?; let message: String? }
    if let e = try? JSONDecoder().decode(APIError.self, from: Data(data.utf8)) {
        return e.message ?? e.code ?? data
    }
    return data
}

// `GET /api/sheds` wrapper. `Shed` itself decodes the server's shed object
// (its decoder tolerates the missing `host`), so there's one field list to
// maintain rather than a parallel wire DTO.
private struct ShedListWire: Decodable {
    let sheds: [Shed]?
}

private struct ImageListWire: Decodable {
    let images: [ShedImage]?
}
