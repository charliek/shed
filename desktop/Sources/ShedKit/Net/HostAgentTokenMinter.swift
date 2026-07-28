// HostAgentTokenMinter.swift
//
// The foreign side of the Rust control-credential FSM: a
// `ShedRustCore.TokenMinter` that obtains a CONTROL credential for a server
// through the host agent's UDS — `credential.get` when the agent advertises it,
// the legacy `token.get` when it does not. The Rust `ControlTokenProvider`
// caches/refreshes around this and invalidates on a refusal; a throw or a
// `.failed` arm is fail-closed (the Rust client then presents nothing — never a
// static downgrade).
//
// Key containment (plan 002 §7 P3): the keypair is generated INSIDE the Rust
// provider. This type receives only `csrBase64` — public material — and relays
// it verbatim. No private key, in any encoding, is ever constructed, read, or
// sent here; the frame-capture tests assert that against the bytes actually
// written to the socket.
//
// `@unchecked Sendable`: it holds an immutable `HostAgentClient` reference
// (itself `@unchecked Sendable`), a `@Sendable` closure, and a Duration, and is
// handed to Rust across the FFI boundary.

import Foundation
import ShedRustCore

final class HostAgentTokenMinter: ShedRustCore.TokenMinter, @unchecked Sendable {
    private let hostAgent: HostAgentClient
    /// Does the CONFIG (plus anything the observer has learned) say this server
    /// issues certificates? Seeded from the entry's `auth_mode`, a CLI-written
    /// cache of what the server last issued.
    private let expectsMtls: @Sendable () -> Bool
    /// How long a mint waits for a `hello_ack` before giving up on learning the
    /// capability. Bounded and short: the ack is the agent's first frame, and a
    /// refusal here is retried by the very next request.
    private let capabilityWait: Duration
    /// Reports every shape this minter maps, SYNCHRONOUSLY, to the app-lifetime
    /// learned-mode store. It is the highest-priority writer there (the core's
    /// observer fires later and defers to it), and it is what lets a rebuilt
    /// client start out knowing what an earlier one proved.
    private let onMintedMode: (@Sendable (ShedAuthMode) -> Void)?
    private let lock = NSLock()
    /// The shape THIS minter last saw the server issue, recorded synchronously
    /// as the response is mapped. The observer learns the same thing, but only
    /// after the core has adopted the credential and its dispatcher has run —
    /// asynchronously, i.e. possibly after `supportsMtls()` has already been
    /// asked again. Keeping a local copy closes that window; the two are OR'd,
    /// so whichever is freshest wins.
    private var lastMintedMode: ShedAuthMode?

    init(
        hostAgent: HostAgentClient,
        expectsMtls: @escaping @Sendable () -> Bool = { false },
        capabilityWait: Duration = .seconds(2),
        onMintedMode: (@Sendable (ShedAuthMode) -> Void)? = nil
    ) {
        self.hostAgent = hostAgent
        self.expectsMtls = expectsMtls
        self.capabilityWait = capabilityWait
        self.onMintedMode = onMintedMode
    }

    /// Config + observer + this minter's own last mint. Any of the three saying
    /// "mtls" is enough: the cost of a false positive is a CSR the server
    /// ignores, the cost of a false negative is a `token.get` against a server
    /// that only accepts certificates.
    private func expectsMtlsNow() -> Bool {
        lock.lock()
        let local = lastMintedMode
        lock.unlock()
        return local == .mtls || expectsMtls()
    }

    private func recordMintedMode(_ mode: ShedAuthMode) {
        lock.lock(); lastMintedMode = mode; lock.unlock()
        // Same write, one level up: the local copy serves THIS minter, the
        // store serves every client the app builds after this one.
        onMintedMode?(mode)
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

    // MARK: - mtls (plan 002 §7 P5) — the tri-state capability gate.
    //
    // The core asks `supportsMtls()` (synchronously, under its mint lock) to
    // decide whether to generate a keypair, then calls `mintCredential` with the
    // resulting CSR. The gate below is what keeps a PRE-ACK state — capability
    // `.unknown`, nothing learned yet — from either downgrading an mtls server to
    // `token.get` or inventing an "upgrade shed-host-agent" error nobody can act
    // on. "expects mtls" is config OR observer OR this minter's own last mint.
    //
    //   capability   expects mtls   supportsMtls   mintCredential
    //   .supported   any            true           credential.get (CSR relayed),
    //                                              re-checked against the SAME
    //                                              connection at send time
    //   .unsupported no             false          token.get (unchanged, forever)
    //   .unsupported yes            false          refuse: upgrade shed-host-agent
    //   .unknown     no             false          token.get IMMEDIATELY (no wait:
    //                                              a token entry has nothing to
    //                                              learn from the ack)
    //   .unknown     yes            true           await the ack (bounded), then
    //                                              the row it resolves to; still
    //                                              unknown → refuse: "connecting
    //                                              to shed-host-agent", retried by
    //                                              the next request
    //
    // Two crossings are refused rather than reinterpreted:
    //   * `.supported` but the core handed us NO CSR while we expect mtls — the
    //     capability appeared after `supportsMtls()` was asked. Throwing makes
    //     the provider's next attempt carry a CSR; sending a CSR-less
    //     `credential.get` and hoping for a certificate cannot work (D4 is
    //     CSR-only) and would burn an SSH round-trip to learn it.
    //   * a certificate arm answering a request that carried no CSR — no key
    //     exists for it, so the server cannot legitimately have issued one.
    //
    // Claiming the capability pre-ack for an mtls server is deliberate: the
    // alternative is minting WITHOUT a CSR, and a CSR-less bootstrap against an
    // mtls server is refused by the server anyway — after having sent the
    // `token.get` the §7 P5 rule exists to prevent.

    func supportsMtls() -> Bool {
        // Cached read of the last hello_ack (never I/O — this runs under the
        // provider's mutex).
        switch hostAgent.credentialCapability {
        case .supported: return true
        case .unsupported: return false
        case .unknown: return expectsMtlsNow()
        }
    }

    func mintCredential(server: String, csrBase64: String?) async throws
        -> ShedRustCore.MintedCredential
    {
        let wantsMtls = expectsMtlsNow()
        var capability = hostAgent.credentialCapabilitySnapshot
        // Only an mtls-expecting mint pays the wait: a token entry learns nothing
        // useful from the ack and keeps every shipped build's immediate token.get.
        if capability.state == .unknown, wantsMtls {
            capability = await hostAgent.awaitCredentialCapability(timeout: capabilityWait)
        }
        switch capability.state {
        case .supported:
            let csr = (csrBase64?.isEmpty ?? true) ? nil : csrBase64
            if csr == nil, wantsMtls {
                throw ShedRustCore.ShedError.Config(
                    message: "shed-host-agent announced credential.get after this mint began; "
                        + "retrying with a certificate request")
            }
            let resp: CredentialResponse
            do {
                resp = try await hostAgent.requestCredential(
                    server: server, csrBase64: csr, capability: capability)
            } catch let e as HostAgentClientError {
                return .failed(message: message(for: e, server: server))
            }
            let validated = resp.validatedCredential(for: server)
            if case .certificate = validated, csr == nil {
                return .failed(
                    message: "host agent returned a certificate for \(server) from a request that "
                        + "carried no CSR; refusing (no key exists for it)")
            }
            return map(validated)
        case .unsupported:
            guard !wantsMtls else {
                return .failed(message: message(for: .capabilityUnsupported, server: server))
            }
            return .token(token: try await mint(server: server))
        case .unknown:
            guard !wantsMtls else {
                return .failed(message: message(for: .capabilityLost, server: server))
            }
            // Token-mode servers keep the pre-ack behavior every shipped build
            // has: send the token.get. `requestToken` itself fails fast when
            // there is no live connection.
            return .token(token: try await mint(server: server))
        }
    }

    /// The user-facing sentence for a transport/capability failure: "upgrade
    /// shed-host-agent" when the live agent genuinely cannot do this, "still
    /// connecting" when the answer is simply not in yet.
    private func message(for error: HostAgentClientError, server: String) -> String {
        switch error {
        case .capabilityUnsupported:
            return "shed-host-agent is too old to obtain a client certificate for \(server), "
                + "which requires mtls; upgrade shed-host-agent"
        case .capabilityLost, .notConnected, .disconnected:
            return "connecting to shed-host-agent; \(server) requires mtls and the agent has not "
                + "announced its capabilities yet"
        default:
            return "\(error) while obtaining a credential for \(server)"
        }
    }

    /// Wire→arm mapping is `CredentialResponse.validatedCredential(for:)`; this
    /// is the 1:1 lift of its result into the FFI enum, recording the shape as
    /// it goes (see `lastMintedMode`). Keeping the strict rules in ShedKit (not
    /// here) is what lets them be fixture-tested without the FFI.
    func map(_ validation: CredentialValidation) -> ShedRustCore.MintedCredential {
        switch validation {
        case .token(let token, let expiresAt):
            recordMintedMode(.token)
            return .token(
                token: ShedRustCore.MintedToken(token: token, expiresAtUnix: Self.unix(expiresAt)))
        case .certificate(let certPEM, let serial, let expiresAt):
            recordMintedMode(.mtls)
            return .certificate(
                certificate: ShedRustCore.MintedCertificate(
                    certPem: certPEM, serial: serial, expiresAtUnix: Self.unix(expiresAt)))
        case .refused(let message):
            return .failed(message: message)
        }
    }

    private static func unix(_ date: Date?) -> UInt64? {
        date.map { UInt64(max(0, $0.timeIntervalSince1970)) }
    }
}
