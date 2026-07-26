// HostAgentCredentialMinterTests — plan 002 §7 P5's tri-state capability gate
// and §7 P3's key containment, driven end to end over a REAL Unix socket
// against the shared fake shed-host-agent (`FakeHostAgent` in TestSupport.swift).
//
// The four states the plan names each get a cell: pre-ack (nothing learned),
// old-agent ack (no `credential.get` capability), new-agent ack, and reconnect
// (the capability is re-learned per connection, never inherited). The frame
// capture is what turns "the app never sends a private key" from a claim into
// an assertion: every app→agent line is recorded and scanned against the shared
// forbidden-substring list.

import Foundation
import ShedRustCore
import XCTest

@testable import ShedKit

final class HostAgentCredentialMinterTests: XCTestCase {
    /// Stand-in for the base64 PKCS#10 the Rust core composes (its content is
    /// irrelevant — the claim is that it is relayed byte-for-byte).
    private static let csr = "MIIBSGVsbG9DU1IrK3Rlc3QvdmVjdG9yPT0="
    private static let certPEM = "-----BEGIN CERTIFICATE-----\nMIIBdesktop\n-----END CERTIFICATE-----\n"

    /// Run `body` against a live client + fake agent pair, torn down after.
    /// `waitFor` is the capability the cell needs before it starts (nil = just
    /// wait for the connection, which is how the pre-ack cells stay pre-ack).
    private func withAgent(
        advertisesCredentialGet: Bool,
        helloAckDelayMs: Int = 0,
        credentialReply: [String: Any] = FakeHostAgent.defaultCredentialReply,
        waitFor: AgentCapabilityState? = nil,
        _ body: (HostAgentClient, FakeHostAgent) async throws -> Void
    ) async throws {
        let path = tempSocketPath()
        let fake = FakeHostAgent(
            path: path, advertisesCredentialGet: advertisesCredentialGet,
            helloAckDelayMs: helloAckDelayMs, credentialReply: credentialReply)
        try fake.start()
        defer { fake.stop() }

        let client = HostAgentClient(socketPath: path)
        let drain = startDraining(client)
        defer { drain.cancel(); client.stop() }
        if let waitFor {
            try await waitCapability(client, waitFor)
        } else {
            try await waitConnected(client)
        }
        try await body(client, fake)
    }

    /// A minter wired to `client`, with a short capability wait so the pre-ack
    /// cells don't spend the production two seconds.
    private func minter(
        _ client: HostAgentClient, expectsMtls: Bool, wait: Duration = .milliseconds(200)
    ) -> HostAgentTokenMinter {
        HostAgentTokenMinter(
            hostAgent: client, expectsMtls: { expectsMtls }, capabilityWait: wait)
    }

    // MARK: - pre-ack (capability .unknown)

    func testCapabilityIsUnknownUntilTheAckArrives() async throws {
        try await withAgent(advertisesCredentialGet: true, helloAckDelayMs: 400) { client, _ in
            // Connected, but the agent has not spoken: neither "supported" nor
            // "unsupported" is known, and claiming either would be a fabrication.
            XCTAssertEqual(client.credentialCapability, .unknown)
            try await waitCapability(client, .supported)
        }
    }

    func testPreAckMtlsMintRefusesInsteadOfSendingATokenGet() async throws {
        // The ack lands well after the minter's 200ms capability wait.
        try await withAgent(advertisesCredentialGet: true, helloAckDelayMs: 1_200) { client, fake in
            let result = try await minter(client, expectsMtls: true).mintCredential(
                server: "prod", csrBase64: Self.csr)
            guard case .failed(let message) = result else {
                return XCTFail("expected a refusal before the ack, got \(result)")
            }
            XCTAssertTrue(
                message.contains("connecting to shed-host-agent"),
                "the pre-ack refusal must name the transient cause, got: \(message)")
            XCTAssertFalse(
                message.contains("upgrade"),
                "a pre-ack state must never produce a false upgrade error: \(message)")
            // Let the ack land so the fake drains everything the app wrote before
            // it (the socket buffered it), then assert the whole point: an mtls
            // server never receives a token.get.
            try await waitCapability(client, .supported)
            XCTAssertEqual(fake.frameTypes(), ["hello"])
        }
    }

    func testPreAckTokenServerKeepsTheLegacyTokenGetPath() async throws {
        try await withAgent(advertisesCredentialGet: false, helloAckDelayMs: 600) { client, fake in
            let result = try await minter(client, expectsMtls: false).mintCredential(
                server: "prod", csrBase64: nil)
            guard case .token(let token) = result else {
                return XCTFail("expected the token arm, got \(result)")
            }
            XCTAssertEqual(token.token, FakeHostAgent.defaultToken)
            XCTAssertEqual(fake.frameTypes(), ["hello", "token.get"])
        }
    }

    // MARK: - old agent (capability .unsupported)

    func testOldAgentTokenServerIsUnchanged() async throws {
        try await withAgent(advertisesCredentialGet: false, waitFor: .unsupported) { client, fake in
            let m = minter(client, expectsMtls: false)
            XCTAssertFalse(m.supportsMtls())
            let result = try await m.mintCredential(server: "prod", csrBase64: nil)
            guard case .token(let token) = result else {
                return XCTFail("expected the token arm, got \(result)")
            }
            XCTAssertEqual(token.token, FakeHostAgent.defaultToken)
            XCTAssertEqual(fake.frameTypes(), ["hello", "token.get"])
        }
    }

    func testOldAgentMtlsServerRefusesWithAnUpgradeError() async throws {
        try await withAgent(advertisesCredentialGet: false, waitFor: .unsupported) { client, fake in
            let m = minter(client, expectsMtls: true)
            XCTAssertFalse(m.supportsMtls(), "an agent that cannot relay a CSR must not claim it can")
            let result = try await m.mintCredential(server: "prod", csrBase64: nil)
            guard case .failed(let message) = result else {
                return XCTFail("expected a refusal, got \(result)")
            }
            XCTAssertTrue(
                message.contains("upgrade shed-host-agent"),
                "the refusal must name the component to upgrade, got: \(message)")
            // No token.get: a bearer token cannot authenticate to an mtls server,
            // and asking for one would surface as an opaque 401/TLS alert instead.
            XCTAssertEqual(fake.frameTypes(), ["hello"])
        }
    }

    // MARK: - new agent (capability .supported)

    func testNewAgentRelaysTheCsrAndAdoptsTheCertificate() async throws {
        try await withAgent(
            advertisesCredentialGet: true,
            credentialReply: [
                "auth_mode": "mtls", "client_cert": Self.certPEM, "cert_serial": "0a0b",
                "expires_at": "2099-01-02T15:04:05Z",
            ],
            waitFor: .supported
        ) { client, fake in
            let m = minter(client, expectsMtls: true)
            XCTAssertTrue(m.supportsMtls())
            let result = try await m.mintCredential(server: "prod", csrBase64: Self.csr)
            guard case .certificate(let cert) = result else {
                return XCTFail("expected the certificate arm, got \(result)")
            }
            XCTAssertEqual(cert.certPem, Self.certPEM)
            XCTAssertEqual(cert.serial, "0a0b")
            XCTAssertEqual(cert.expiresAtUnix, 4_071_049_445)
            XCTAssertEqual(fake.frameTypes(), ["hello", "credential.get"])
            // The CSR crossed the socket verbatim — not re-encoded, not regenerated.
            let sent = try XCTUnwrap(fake.frames().last)
            XCTAssertEqual(sent["csr"] as? String, Self.csr)
            XCTAssertEqual(sent["server"] as? String, "prod")
        }
    }

    func testNewAgentTokenModeServerYieldsTheTokenArm() async throws {
        try await withAgent(
            advertisesCredentialGet: true,
            credentialReply: ["auth_mode": "token", "token": "shed_control_from_credential_get"],
            waitFor: .supported
        ) { client, fake in
            // A token-mode server reached through a CSR-carrying request: the
            // SERVER chose the shape, and the app must accept it without complaint.
            let result = try await minter(client, expectsMtls: false).mintCredential(
                server: "prod", csrBase64: Self.csr)
            guard case .token(let token) = result else {
                return XCTFail("expected the token arm, got \(result)")
            }
            XCTAssertEqual(token.token, "shed_control_from_credential_get")
            XCTAssertEqual(fake.frameTypes(), ["hello", "credential.get"])
        }
    }

    func testUnknownModeReplyFailsClosedThroughTheMinter() async throws {
        try await withAgent(
            advertisesCredentialGet: true,
            credentialReply: ["auth_mode": "quantum", "token": "must-not-be-adopted"],
            waitFor: .supported
        ) { client, _ in
            let result = try await minter(client, expectsMtls: false).mintCredential(
                server: "prod", csrBase64: Self.csr)
            guard case .failed(let message) = result else {
                return XCTFail("an unknown mode must never be adopted, got \(result)")
            }
            XCTAssertTrue(message.contains("unknown auth mode"), message)
        }
    }

    func testAgentErrorReplyIsSurfacedVerbatim() async throws {
        try await withAgent(
            advertisesCredentialGet: true,
            credentialReply: ["error": "server \"prod\" requires auth.mode: mtls; upgrade the app"],
            waitFor: .supported
        ) { client, _ in
            // A token-mode entry whose server has since flipped: the CSR-less
            // credential.get goes out and the SERVER's own upgrade error comes
            // back, relayed verbatim rather than reworded by the app.
            let result = try await minter(client, expectsMtls: false).mintCredential(
                server: "prod", csrBase64: nil)
            guard case .failed(let message) = result else {
                return XCTFail("expected a refusal, got \(result)")
            }
            XCTAssertTrue(message.contains("requires auth.mode: mtls"), message)
        }
    }

    // MARK: - reconnect (the capability is per-connection)

    func testCapabilityIsRelearnedOnReconnect() async throws {
        try await withAgent(advertisesCredentialGet: false, waitFor: .unsupported) { client, fake in
            let m = minter(client, expectsMtls: false)
            XCTAssertFalse(m.supportsMtls())

            // The agent is upgraded and restarts: the reconnect must learn the NEW
            // capability, not inherit the old connection's answer.
            fake.setAdvertisesCredentialGet(true)
            fake.dropConnection()
            try await waitCapability(client, .supported)
            XCTAssertTrue(m.supportsMtls())

            // ...and the reverse: a downgrade is learned just as readily.
            fake.setAdvertisesCredentialGet(false)
            fake.dropConnection()
            try await waitCapability(client, .unsupported)
            XCTAssertFalse(m.supportsMtls())
        }
    }

    func testASupersededAckMakesTheConnectionUnusableForCredentials() async throws {
        try await withAgent(advertisesCredentialGet: true, waitFor: .supported) { client, fake in
            // Another app took the consumer slot: this connection will answer
            // nothing further. It is not an OLD agent (that would be a false
            // upgrade error) and it is not a usable one either — back to unknown.
            fake.sendSupersededAck()
            for _ in 0..<100 where client.credentialCapability != .unknown {
                try await Task.sleep(for: .milliseconds(20))
            }
            XCTAssertEqual(client.credentialCapability, .unknown)

            let result = try await minter(client, expectsMtls: true).mintCredential(
                server: "prod", csrBase64: Self.csr)
            guard case .failed(let message) = result else {
                return XCTFail("expected a refusal on a superseded connection, got \(result)")
            }
            XCTAssertTrue(message.contains("connecting to shed-host-agent"), message)
            XCTAssertEqual(fake.frameTypes(), ["hello"], "nothing may be sent on a dead consumer slot")
        }
    }

    /// The capability is bound to the CONNECTION that advertised it: a stale
    /// snapshot (taken before a reconnect) is refused at send time rather than
    /// putting a `credential.get` on a socket whose agent never advertised it.
    func testAStaleCapabilitySnapshotIsRefusedAtSendTime() async throws {
        try await withAgent(advertisesCredentialGet: true, waitFor: .supported) { client, fake in
            let stale = client.credentialCapabilitySnapshot
            fake.dropConnection()
            let fresh = try await waitCapability(
                client, .supported, afterGeneration: stale.generation)

            do {
                _ = try await client.requestCredential(
                    server: "prod", csrBase64: Self.csr, capability: stale, timeout: .seconds(2))
                XCTFail("a stale snapshot must not be honored")
            } catch let e as HostAgentClientError {
                XCTAssertEqual(e, .capabilityLost)
            }
            // The fresh snapshot works on the same connection.
            _ = try await client.requestCredential(
                server: "prod", csrBase64: Self.csr, capability: fresh, timeout: .seconds(2))
        }
    }

    /// A `.supported` snapshot cannot be used against a connection that turned
    /// out not to support it.
    func testASupportedSnapshotIsRefusedOnAnUnsupportingConnection() async throws {
        try await withAgent(advertisesCredentialGet: true, waitFor: .supported) { client, fake in
            let snapshot = client.credentialCapabilitySnapshot
            fake.setAdvertisesCredentialGet(false)
            fake.dropConnection()
            try await waitCapability(client, .unsupported, afterGeneration: snapshot.generation)

            do {
                _ = try await client.requestCredential(
                    server: "prod", csrBase64: Self.csr, capability: snapshot, timeout: .seconds(2))
                XCTFail("expected the request to be refused")
            } catch let e as HostAgentClientError {
                XCTAssertEqual(e, .capabilityUnsupported)
            }
        }
    }

    // MARK: - CSR/arm coherence

    func testACertificateForACsrLessRequestIsRefused() async throws {
        // The server cannot have issued a certificate for a key it never saw a
        // request for (D4 is CSR-only) — defense in depth against an agent (or a
        // future bug) answering out of shape.
        try await withAgent(
            advertisesCredentialGet: true,
            credentialReply: ["auth_mode": "mtls", "client_cert": Self.certPEM, "cert_serial": "0a0b"],
            waitFor: .supported
        ) { client, _ in
            let result = try await minter(client, expectsMtls: false).mintCredential(
                server: "prod", csrBase64: nil)
            guard case .failed(let message) = result else {
                return XCTFail("expected a refusal, got \(result)")
            }
            XCTAssertTrue(message.contains("carried no CSR"), message)
        }
    }

    func testACapabilityThatAppearsMidMintIsRetriedRatherThanSentCsrLess() async throws {
        // supportsMtls() answered `false` (capability unknown, config said token),
        // then the ack landed and the local learned mode says mtls: the request
        // in flight has no CSR, so it must NOT be sent expecting a certificate.
        try await withAgent(advertisesCredentialGet: true, waitFor: .supported) { client, fake in
            let m = minter(client, expectsMtls: true)
            do {
                _ = try await m.mintCredential(server: "prod", csrBase64: nil)
                XCTFail("expected a retryable error")
            } catch let e as ShedRustCore.ShedError {
                XCTAssertTrue("\(e)".contains("retrying"), "\(e)")
            }
            XCTAssertEqual(fake.frameTypes(), ["hello"], "nothing may go out on the doomed attempt")
        }
    }

    // MARK: - the minter's own learned mode (no TOCTOU on the async observer)

    func testAnMtlsMintIsRememberedSynchronouslyAcrossAReconnect() async throws {
        try await withAgent(
            advertisesCredentialGet: true,
            credentialReply: ["auth_mode": "mtls", "client_cert": Self.certPEM, "cert_serial": "0a0b"],
            waitFor: .supported
        ) { client, fake in
            // The config says token and no observer has fired — only the mint
            // itself knows this server issues certificates.
            let m = minter(client, expectsMtls: false)
            guard case .certificate = try await m.mintCredential(server: "prod", csrBase64: Self.csr)
            else {
                return XCTFail("expected the certificate arm")
            }

            // Reconnecting resets the capability to .unknown; the minter must
            // still claim mtls, or the next mint would ask for a bearer token
            // against a certificate-only server.
            fake.dropConnection()
            for _ in 0..<100 where client.credentialCapability != .unknown {
                try await Task.sleep(for: .milliseconds(10))
            }
            XCTAssertEqual(client.credentialCapability, .unknown)
            XCTAssertTrue(m.supportsMtls(), "the minter forgot the mode it just minted")
        }
    }

    // MARK: - §7 P3: no private key ever crosses the socket

    func testAppToAgentFramesCarryNoPrivateKeyMaterial() async throws {
        try await withAgent(
            advertisesCredentialGet: true,
            credentialReply: [
                "auth_mode": "mtls", "client_cert": Self.certPEM, "cert_serial": "0a0b",
            ],
            waitFor: .supported
        ) { client, fake in
            _ = try await minter(client, expectsMtls: true).mintCredential(
                server: "prod", csrBase64: Self.csr)
            // Approvals + pings share the socket; exercise one more writer so the
            // capture isn't only the mint path.
            client.respond(requestID: "req-1", decision: .approve, decidedBy: .user)
            try await Task.sleep(for: .milliseconds(100))

            // The forbidden markers come from the shared §7 P9 fixture so Swift,
            // Go and Rust assert the same list.
            let forbidden = try XCTUnwrap(
                RepoFixtures.desktopCredential("credential_get")["forbidden_substrings"] as? [String])
            let captured = fake.rawLines()
            XCTAssertGreaterThanOrEqual(
                captured.count, 2, "nothing was captured — the assertion is vacuous")
            for line in captured {
                for marker in forbidden {
                    XCTAssertFalse(
                        line.localizedCaseInsensitiveContains(marker),
                        "an app→agent frame carried \"\(marker)\": \(line)")
                }
            }
            // The one key-adjacent value that IS sanctioned crossed as itself.
            XCTAssertTrue(
                captured.contains { $0.contains(Self.csr) }, "the CSR never reached the agent")
        }
    }
}
