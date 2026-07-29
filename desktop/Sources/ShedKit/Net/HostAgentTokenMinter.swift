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
    /// When a pre-ack capability wait last concluded still-`.unknown` — the
    /// negative-result memo behind guard 2 in `mintCredential` (one caller per
    /// burst pays the wait; the provider's mint lock serializes the rest behind
    /// it). Guarded by `lock`, like `lastMintedMode`.
    private var unknownWaitedAt: ContinuousClock.Instant?
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

    /// Guard 2's read: did a capability wait conclude still-`.unknown` within
    /// the last `capabilityWait` window?
    private func recentUnknownWait() -> Bool {
        lock.lock()
        let at = unknownWaitedAt
        lock.unlock()
        guard let at else { return false }
        return at.duration(to: .now) < capabilityWait
    }

    private func noteUnknownWait() {
        lock.lock(); unknownWaitedAt = .now; lock.unlock()
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
    //   .unsupported yes            false          refuse: the TYPED
    //                                              AgentUpgradeRequired error
    //   .unknown     any            per config     await the ack (bounded, BOTH
    //                                              modes — plan 006 D5), then the
    //                                              row it resolves to; still
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
        // The pre-ack wait is unconditional across modes (plan 006 D5, revising
        // plan 002's mtls-only gate; the Rust minter's `mint_credential` carries
        // the same change): before the first `hello_ack` this connection has
        // learned NOTHING — the ack is the agent's first frame — so a `token.get`
        // fired into that silence spends its whole reply timeout to learn less
        // than this bounded wait does. If the ack lands inside the window,
        // proceed per the resolved state (an alive agent with a late ack still
        // succeeds, at most `capabilityWait` later). If it never lands, the
        // specific "not announced its capabilities" sentence below beats the
        // outer request bound instead of losing to it (shed#297). The deliberate
        // trade: a down/never-acking agent now costs a token mint
        // `capabilityWait` before failing WITH a diagnosis, instead of failing
        // later with none.
        //
        // The negative-result memo keeps that trade from STACKING: the Rust
        // provider holds its mint lock across a `mintCredential`, so concurrent
        // callers serialize through here — without the memo, N dashboard panes
        // against a wedged/absent agent would each queue a full wait and push the
        // later ones past their outer request bounds. One caller per burst pays
        // the wait; the rest fail fast with the same diagnosis. Self-healing:
        // once the ack lands the state is no longer `.unknown` and this whole
        // branch is skipped.
        if capability.state == .unknown {
            if recentUnknownWait() {
                throw Self.capabilityError(
                    for: .capabilityLost, server: server, wantsMtls: wantsMtls)
            }
            capability = await hostAgent.awaitCredentialCapability(timeout: capabilityWait)
            if capability.state == .unknown { noteUnknownWait() }
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
                // Same mapping as the capability arms below (Rust's
                // `map_err(capability_error)`): a connection that turned out not
                // to answer `credential.get` is the typed upgrade case here too.
                throw Self.capabilityError(for: e, server: server, wantsMtls: wantsMtls)
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
                throw Self.capabilityError(
                    for: .capabilityUnsupported, server: server, wantsMtls: wantsMtls)
            }
            // Unsupported + token server: an old-but-alive agent answers
            // token.get exactly as every shipped build did.
            return .token(token: try await mint(server: server))
        case .unknown:
            // Still unknown after the bounded wait — the agent has emitted
            // nothing at all. Diagnosable for BOTH modes (plan 006 D5): sending
            // a token.get here would only trade this specific sentence for a
            // reply timeout the outer request bound truncates to a generic
            // transport error.
            throw Self.capabilityError(for: .capabilityLost, server: server, wantsMtls: wantsMtls)
        }
    }

    /// The user-facing error for a transport/capability failure — the Swift twin
    /// of the Rust minter's `capability_error`, case for case and string for
    /// string: the TYPED upgrade case when the live agent genuinely cannot do
    /// this (shed#300 — presentation branches on it), a "still connecting"
    /// `Config` when the answer is simply not in yet.
    ///
    /// `wantsMtls` picks the wording of the latter: the pre-ack wait runs for
    /// token servers too (plan 006 D5), and telling a token-mode operator their
    /// server "requires mtls" would send them debugging the wrong thing.
    static func capabilityError(
        for error: HostAgentClientError, server: String, wantsMtls: Bool
    ) -> ShedRustCore.ShedError {
        switch error {
        case .capabilityUnsupported:
            return .AgentUpgradeRequired(
                server: server,
                detail: "the connected shed-host-agent does not support `credential.get`, and "
                    + "\(server) requires auth.mode: mtls")
        case .capabilityLost, .notConnected, .disconnected:
            return .Config(
                message: wantsMtls
                    ? "connecting to shed-host-agent; \(server) requires mtls and the agent has "
                        + "not announced its capabilities yet"
                    : "connecting to shed-host-agent; it has not announced its capabilities yet, "
                        + "so no credential for \(server) can be minted")
        default:
            return .Config(message: "\(error) while obtaining a credential for \(server)")
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
