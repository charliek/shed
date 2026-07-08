// HostAgentTokenMinter.swift
//
// The foreign side of the Rust control-token FSM: a `ShedRustCore.TokenMinter`
// that mints a CONTROL token via the host agent (`token.get`). The Rust
// `ControlTokenProvider` caches/refreshes around this and invalidates on a 401;
// a throw here is fail-closed (the Rust client then sends no token — never a
// static downgrade), mirroring `ControlTokenProvider.hostAgent` on the Swift
// path.
//
// `@unchecked Sendable`: it holds only an immutable `HostAgentClient` reference
// (itself `@unchecked Sendable`), and is handed to Rust across the FFI boundary.

import Foundation
import ShedRustCore

final class HostAgentTokenMinter: ShedRustCore.TokenMinter, @unchecked Sendable {
    private let hostAgent: HostAgentClient

    init(hostAgent: HostAgentClient) {
        self.hostAgent = hostAgent
    }

    func mint(server: String) async throws -> ShedRustCore.MintedToken {
        let resp = try await hostAgent.requestToken(server: server)
        // A fail-closed reply (error set, or no token) → throw, so the Rust FSM
        // surfaces it and the client sends no token.
        if let err = resp.error, !err.isEmpty {
            throw ShedRustCore.ShedError.Config(message: err)
        }
        guard let token = resp.token, !token.isEmpty else {
            throw ShedRustCore.ShedError.Config(message: "host agent returned no token for \(server)")
        }
        // Expiry is carried as unix seconds; Swift owns the flexible ISO-8601
        // parsing (the Rust core never parses timestamps).
        let expiresAtUnix = resp.expiresAt
            .flatMap(DateFormatting.parseFlexibleTimestamp)
            .map { UInt64(max(0, $0.timeIntervalSince1970)) }
        return ShedRustCore.MintedToken(token: token, expiresAtUnix: expiresAtUnix)
    }
}
