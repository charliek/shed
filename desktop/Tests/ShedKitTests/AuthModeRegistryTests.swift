// AuthModeRegistryTests — plan 002 §7 P1's learned-mode store, and the property
// it exists for: a mode this session PROVED must survive `AppModel.reconnect()`
// rebuilding every client from a config that still says `token`.
//
// The precedence cells mirror `crates/shed-app/src/auth_modes.rs`'s, because the
// two clients must not disagree about which writer wins.

import Foundation
import ShedRustCore
import XCTest

@testable import ShedKit

final class AuthModeRegistryTests: XCTestCase {
    private static let certPEM =
        "-----BEGIN CERTIFICATE-----\nMIIBdesktop\n-----END CERTIFICATE-----\n"
    private static let csr = "MIIBSGVsbG9DU1IrK3Rlc3QvdmVjdG9yPT0="

    // MARK: - precedence

    func testConfigSeedsOnlyWhatNothingHasBeenLearnedFor() {
        let r = AuthModeRegistry()
        r.seed(server: "prod", mode: .token)
        XCTAssertEqual(r.state(for: "prod"), AuthModeState(mode: .token, learned: false))
        XCTAssertNil(r.learnedMode(for: "prod"), "a config claim is not something proved")

        r.record(server: "prod", mode: .mtls)
        // The config watcher fires and every client is rebuilt: the entry still
        // says token, because the CLI has not rewritten its cache.
        r.seed(server: "prod", mode: .token)
        XCTAssertEqual(r.state(for: "prod"), AuthModeState(mode: .mtls, learned: true))
        XCTAssertTrue(r.expectsMtls("prod"))
    }

    func testASynchronousRecordAlwaysWins() {
        let r = AuthModeRegistry()
        r.seed(server: "prod", mode: .token)
        let first = r.record(server: "prod", mode: .mtls)
        let second = r.record(server: "prod", mode: .token)
        XCTAssertGreaterThan(second, first, "the ordinals are monotonic")
        XCTAssertEqual(r.state(for: "prod"), AuthModeState(mode: .token, learned: true))
    }

    func testAStaleObserverEventCannotWalkBackANewerMint() {
        // Mint N+1 learned mtls synchronously; mint N's observer callback then
        // lands, late, still saying token. Applying it would make the next mint
        // send a `token.get` to a certificate-only server (§7 P5).
        let r = AuthModeRegistry()
        r.record(server: "prod", mode: .mtls)
        XCTAssertFalse(r.recordObserved(server: "prod", mode: .token))
        XCTAssertEqual(r.state(for: "prod"), AuthModeState(mode: .mtls, learned: true))
    }

    func testAnObserverThatAgreesApplies() {
        let r = AuthModeRegistry()
        r.record(server: "prod", mode: .mtls)
        XCTAssertTrue(r.recordObserved(server: "prod", mode: .mtls))
        XCTAssertEqual(r.learnedMode(for: "prod"), .mtls)
    }

    func testTheObserverIsAuthoritativeWhenNoMintHasSpoken() {
        // The embedded/headless-coexist brokers hold no minter-side registry, so
        // there the observer is the only writer and always applies.
        let r = AuthModeRegistry()
        r.seed(server: "prod", mode: .token)
        XCTAssertEqual(r.syncSeq(for: "prod"), 0)
        XCTAssertTrue(r.recordObserved(server: "prod", mode: .mtls))
        XCTAssertEqual(r.learnedMode(for: "prod"), .mtls)
    }

    func testSnapshotIsSortedAndSeparatesLearnedFromConfigured() {
        let r = AuthModeRegistry()
        r.seed(server: "zulu", mode: .token)
        r.seed(server: "alpha", mode: .mtls)
        r.record(server: "alpha", mode: .mtls)
        let snapshot = r.snapshot()
        XCTAssertEqual(snapshot.map(\.0), ["alpha", "zulu"])
        XCTAssertTrue(snapshot[0].1.learned)
        XCTAssertFalse(snapshot[1].1.learned)
    }

    // MARK: - the observer writes through to the store

    func testCredentialModeObserverRecordsIntoTheStore() {
        let r = AuthModeRegistry()
        let o = CredentialModeObserver(registry: r)
        o.credentialAdopted(
            event: ShedRustCore.CredentialAdopted(
                server: "prod", mode: .mtls, expiresAtUnix: nil, token: nil))
        XCTAssertEqual(r.learnedMode(for: "prod"), .mtls)
        o.modeChanged(server: "prod", mode: .token)
        XCTAssertEqual(r.learnedMode(for: "prod"), .token)
    }

    func testCredentialModeObserverDefersToASynchronousMint() {
        let r = AuthModeRegistry()
        let o = CredentialModeObserver(registry: r)
        r.record(server: "prod", mode: .mtls)
        // A late callback from the PREVIOUS mint.
        o.credentialAdopted(
            event: ShedRustCore.CredentialAdopted(
                server: "prod", mode: .token, expiresAtUnix: nil, token: nil))
        XCTAssertEqual(r.learnedMode(for: "prod"), .mtls)
    }

    // MARK: - the property: a rebuild does not forget

    /// `AppModel.reconnect()` (and the config watcher behind it) rebuilds every
    /// client and every minter. The mode learned by the FIRST client must still
    /// drive the SECOND one, or the first mint after an innocuous config touch
    /// downgrades an mtls server to `token.get`.
    func testALearnedMtlsModeSurvivesAClientRebuiltFromTokenConfig() async throws {
        let path = tempSocketPath()
        let fake = FakeHostAgent(
            path: path, advertisesCredentialGet: true,
            credentialReply: [
                "auth_mode": "mtls", "client_cert": Self.certPEM, "cert_serial": "0a0b",
            ])
        try fake.start()
        defer { fake.stop() }

        let agent = HostAgentClient(socketPath: path)
        let drain = startDraining(agent)
        defer { drain.cancel(); agent.stop() }
        try await waitCapability(agent, .supported)

        // Client #1, built from a config entry that says `token`.
        let registry = AuthModeRegistry()
        registry.seed(server: "prod", mode: .token)
        let first = RustShedCoreAdapter.makeMinter(
            hostAgent: agent, serverName: "prod", authModes: registry)
        guard case .certificate = try await first.mintCredential(
            server: "prod", csrBase64: Self.csr)
        else {
            return XCTFail("expected the certificate arm from the fake agent")
        }
        XCTAssertEqual(registry.learnedMode(for: "prod"), .mtls, "the mint was not recorded")

        // The config watcher fires: clients are rebuilt and re-seeded from the
        // SAME on-disk config, which still says token.
        registry.seed(server: "prod", mode: .token)
        XCTAssertEqual(registry.learnedMode(for: "prod"), .mtls, "the rebuild forgot the mint")

        // Client #2's minter, on an agent that has not acked yet (the cold state
        // that matters — pre-ack, `supportsMtls` falls through to what is
        // known). It must still expect a certificate.
        let coldAgent = HostAgentClient(socketPath: tempSocketPath())
        XCTAssertEqual(coldAgent.credentialCapability, .unknown)
        let second = RustShedCoreAdapter.makeMinter(
            hostAgent: coldAgent, serverName: "prod", authModes: registry)
        XCTAssertTrue(second.supportsMtls(), "a rebuilt minter must not downgrade to token.get")
    }

    /// The same property one level up, where the app sees it: two
    /// `ShedServerClient`s built from the same store, the way `AppModel` builds
    /// them on every reconnect.
    func testShedServerClientRebuiltFromTheSameStoreKeepsTheLearnedMode() {
        let registry = AuthModeRegistry()
        let url = URL(string: "http://127.0.0.1:1")!
        let first = ShedServerClient(
            baseURL: url, serverName: "prod", useRustCore: true, authMode: .token,
            authModes: registry)
        XCTAssertNil(first.learnedAuthMode, "nothing is learned before the first mint")

        // What the minter's `onMintedMode` does when a mint returns a certificate.
        registry.record(server: "prod", mode: .mtls)
        XCTAssertEqual(first.learnedAuthMode, .mtls)

        let rebuilt = ShedServerClient(
            baseURL: url, serverName: "prod", useRustCore: true, authMode: .token,
            authModes: registry)
        XCTAssertEqual(rebuilt.learnedAuthMode, .mtls, "the rebuild lost the learned mode")
    }

    /// A client built WITHOUT a store keeps its old, per-client behavior — the
    /// parameter is additive, so every other caller is unchanged.
    func testAClientWithoutAStoreIsSelfContained() {
        let url = URL(string: "http://127.0.0.1:1")!
        let a = ShedServerClient(
            baseURL: url, serverName: "prod", useRustCore: true, authMode: .token)
        let b = ShedServerClient(
            baseURL: url, serverName: "prod", useRustCore: true, authMode: .token)
        XCTAssertNil(a.learnedAuthMode)
        XCTAssertNil(b.learnedAuthMode)
    }
}
