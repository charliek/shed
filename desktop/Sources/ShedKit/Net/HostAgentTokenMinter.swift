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
        // A fail-closed reply (error set, mismatched server, or no token) → throw,
        // so the Rust FSM surfaces it and the client sends no token. Shares the one
        // validator with `ControlTokenProvider.hostAgent` so the two can't diverge.
        let token: String
        switch resp.validatedToken(for: server) {
        case .valid(let t): token = t
        case .invalid(let m): throw ShedRustCore.ShedError.Config(message: m)
        }
        // Expiry is carried as unix seconds; Swift owns the flexible ISO-8601
        // parsing (the Rust core never parses timestamps).
        let expiresAtUnix = resp.expiresAt
            .flatMap(DateFormatting.parseFlexibleTimestamp)
            .map { UInt64(max(0, $0.timeIntervalSince1970)) }
        return ShedRustCore.MintedToken(token: token, expiresAtUnix: expiresAtUnix)
    }

    // MARK: - mtls (plan 002) — C2 implements these for real.
    //
    // The Rust core now asks every minter whether it can carry a CSR, and mints
    // through `mintCredential`. C2 answers `supportsMtls()` from the tri-state
    // `hello_ack` capability gate (§7 P5) and relays the CSR over
    // `credential.get`. Until then the app stays exactly on today's token path:
    // no CSR is ever generated (the core skips the keypair when this says
    // false), and `mintCredential` is the legacy mint wrapped in the token arm —
    // which is what the Rust-side default did before this trait grew.

    func supportsMtls() -> Bool { false }

    func mintCredential(server: String, csrBase64: String?) async throws
        -> ShedRustCore.MintedCredential
    {
        .token(token: try await mint(server: server))
    }
}
