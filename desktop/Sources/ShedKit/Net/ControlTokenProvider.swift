// ControlTokenProvider.swift — caches + refreshes a shed-server CONTROL token
// minted on demand by the host agent over the UDS (token.get, Phase 5b). One
// provider per secure server; ShedServerClient (5c-ii) sources its Bearer token
// from it and invalidates + re-mints on a 401. Open-mode servers with a static
// configured control_token bypass this entirely — ShedServerClient keeps using
// the String it was constructed with.

import Foundation

/// A minted control token plus its optional expiry (nil when the host agent
/// reports none — then only an explicit `invalidate()` forces a refresh).
public struct MintedToken: Sendable {
    public let token: String
    public let expiresAt: Date?
    public init(token: String, expiresAt: Date?) {
        self.token = token
        self.expiresAt = expiresAt
    }
}

public enum ControlTokenError: Error, Equatable, CustomStringConvertible, Sendable {
    case mintFailed(String)
    public var description: String {
        switch self {
        case .mintFailed(let m): return "control token mint failed: \(m)"
        }
    }
}

/// Caches a control token, refreshing it near expiry or on demand
/// (`invalidate()`, called on a 401). Concurrent `token()` callers that arrive
/// while a mint is in flight join the same mint — single-flight, so a burst of
/// requests never fans out into N bootstraps.
public actor ControlTokenProvider {
    public typealias Mint = @Sendable () async throws -> MintedToken

    private let mint: Mint
    private let refreshWindow: TimeInterval
    private let now: @Sendable () -> Date

    private var cached: MintedToken?
    private var inflight: Task<MintedToken, Error>?

    /// `refreshWindow` mirrors the SDK/CLI 2h window — refresh this long before
    /// the reported expiry so routine requests rarely race a 401.
    public init(
        refreshWindow: TimeInterval = 2 * 60 * 60,
        now: @escaping @Sendable () -> Date = Date.init,
        mint: @escaping Mint
    ) {
        self.refreshWindow = refreshWindow
        self.now = now
        self.mint = mint
    }

    /// The current token, minting/refreshing when it is missing or within the
    /// refresh window of expiry.
    public func token() async throws -> String {
        if let cached, !needsRefresh(cached) { return cached.token }
        return try await mintOnce().token
    }

    /// Drop the cached token so the next `token()` re-mints. Called on a 401.
    public func invalidate() {
        cached = nil
    }

    private func needsRefresh(_ t: MintedToken) -> Bool {
        guard let exp = t.expiresAt else { return false }  // no expiry → only invalidate refreshes
        return now() >= exp.addingTimeInterval(-refreshWindow)
    }

    /// Single-flight mint: the first caller starts the mint and records it;
    /// concurrent callers join the in-flight task rather than minting again.
    private func mintOnce() async throws -> MintedToken {
        if let inflight { return try await inflight.value }
        let task = Task { [mint] in try await mint() }
        inflight = task
        do {
            let minted = try await task.value
            cached = minted
            inflight = nil
            return minted
        } catch {
            inflight = nil  // don't poison the cache; a later call retries
            throw error
        }
    }
}

extension ControlTokenProvider {
    /// A provider backed by a host agent's `token.get` for `server`. A reply
    /// carrying an `error` (fail-closed) or no token is a mint failure.
    public static func hostAgent(
        _ client: HostAgentClient, server: String,
        refreshWindow: TimeInterval = 2 * 60 * 60,
        now: @escaping @Sendable () -> Date = Date.init
    ) -> ControlTokenProvider {
        ControlTokenProvider(refreshWindow: refreshWindow, now: now) {
            let resp = try await client.requestToken(server: server)
            if let err = resp.error, !err.isEmpty {
                throw ControlTokenError.mintFailed(err)
            }
            guard let tok = resp.token, !tok.isEmpty else {
                throw ControlTokenError.mintFailed("host agent returned no token for \(server)")
            }
            return MintedToken(
                token: tok, expiresAt: resp.expiresAt.flatMap(DateFormatting.parseFlexibleTimestamp))
        }
    }
}
