// RC tests — the pure pane classifier (backing the rc.classify IPC) plus the
// shed-ext-rc binary client: argv shapes, exit-code mapping, and the neutral DTO
// (whose golden fixture is byte-identical to shed-remote-agent's, asserted to
// decode in both repos as the cross-tool contract guard).

import Foundation
import XCTest
@testable import ShedKit

final class RCClassifierTests: XCTestCase {
    func testBrokerReadyWithURL() {
        let pane = """
        ·✔︎· Connected · my-shed · main
            Capacity: 0/32 · New sessions will be created in the current directory

        Continue coding in the Claude app or https://claude.ai/code?environment=env_01ABC
        space to show QR code · w to toggle spawn mode
        """
        let c = RemoteControl.classifyPane(kind: .claudeBroker, pane: pane)
        XCTAssertEqual(c.state, .ready)
        XCTAssertEqual(c.url, "https://claude.ai/code?environment=env_01ABC")
    }

    func testBrokerReconnecting() {
        let c = RemoteControl.classifyPane(kind: .claudeBroker, pane: "·|· Reconnecting · retrying in 2.5s · disconnected 0s")
        XCTAssertEqual(c.state, .reconnecting)
    }

    func testBrokerNeedsTrust() {
        let c = RemoteControl.classifyPane(kind: .claudeBroker, pane: "Error: Workspace not trusted. Please run `claude` ...")
        XCTAssertEqual(c.state, .needsTrust)
    }

    func testBrokerNeedsAuthSubscription() {
        let c = RemoteControl.classifyPane(kind: .claudeBroker, pane: "Remote Control requires a claude.ai subscription.")
        XCTAssertEqual(c.state, .needsAuth)
    }

    func testBrokerNeedsAuthLogin() {
        let c = RemoteControl.classifyPane(kind: .claudeBroker, pane: "You are not logged in. Run claude auth login.")
        XCTAssertEqual(c.state, .needsAuth)
    }

    func testClaudeRcReadyWithURL() {
        let pane = """
        ❯ /remote-control
          ⎿  Remote Control connecting…

          /remote-control is active · Code in CLI or at https://claude.ai/code/session_01RCkTDrdZ2Rr12sD5dfMjgr

        ────────────────────────────────────── spike1 ──
        ❯
          ? for shortcuts                                                  Remote Control active
        """
        let c = RemoteControl.classifyPane(kind: .claudeRc, pane: pane)
        XCTAssertEqual(c.state, .ready)
        XCTAssertEqual(c.url, "https://claude.ai/code/session_01RCkTDrdZ2Rr12sD5dfMjgr")
    }

    func testClaudeRcStartingConnecting() {
        let c = RemoteControl.classifyPane(kind: .claudeRc, pane: "❯ /remote-control\n  ⎿  Remote Control connecting…")
        XCTAssertEqual(c.state, .starting)
        XCTAssertNil(c.url)
    }

    func testClaudeRcFirstTimeTrustPrompt() {
        let pane = """
        Accessing workspace:

         /home/charliek/projects

         Quick safety check: Is this a project you created or one you trust?

         ❯ 1. Yes, I trust this folder
           2. No, exit
        """
        let c = RemoteControl.classifyPane(kind: .claudeRc, pane: pane)
        XCTAssertEqual(c.state, .needsTrust)
    }

    func testShellReadyAndStarting() {
        XCTAssertEqual(RemoteControl.classifyPane(kind: .shell, pane: "charliek@shed:/workspace$ ").state, .ready)
        XCTAssertEqual(RemoteControl.classifyPane(kind: .shell, pane: "   \n  \n").state, .starting)
    }

    func testSlugIsConfusableFreeAndCorrectLength() {
        let slug = RemoteControl.generateSlug()
        XCTAssertEqual(slug.count, 6)
        let forbidden = Set("ilo01")
        XCTAssertTrue(slug.allSatisfy { !forbidden.contains($0) })
        XCTAssertTrue(slug.allSatisfy { "abcdefghjkmnpqrstuvwxyz23456789".contains($0) })
    }
}

// MARK: - shed-ext-rc binary client (argv, exit codes, DTO)

final class RCBinaryTests: XCTestCase {
    func testCreateArgvWithPrompt() {
        let argv = RemoteControl.createArgv(
            kind: .claudeRc, name: "demo/abc", slug: "abc", workdir: nil,
            createdBy: "shed-desktop/0.1.0", target: "shed:demo@h", hasPrompt: true)
        XCTAssertEqual(argv.first, RemoteControl.binaryName())
        XCTAssertTrue(argv.contains("create"))
        XCTAssertTrue(argv.contains("--wait"))
        XCTAssertTrue(argv.contains("--prompt-stdin"))
        XCTAssertTrue(argvHasPair(argv, "--kind", "claude-rc"))
        XCTAssertTrue(argvHasPair(argv, "--name", "demo/abc"))
        XCTAssertTrue(argvHasPair(argv, "--slug", "abc"))
        XCTAssertFalse(argv.contains("--workdir"))  // nil → binary resolves $SHED_WORKSPACE
    }

    func testCreateArgvWithWorkdirNoPrompt() {
        let argv = RemoteControl.createArgv(
            kind: .shell, name: "n", slug: "s", workdir: "/home/shed/proj",
            createdBy: "shed-desktop/0.1.0", target: "shed:demo@h", hasPrompt: false)
        XCTAssertTrue(argvHasPair(argv, "--workdir", "/home/shed/proj"))
        XCTAssertFalse(argv.contains("--prompt-stdin"))
    }

    func testListAndKillArgv() {
        XCTAssertEqual(RemoteControl.listArgv(), [RemoteControl.binaryName(), "list"])
        XCTAssertEqual(RemoteControl.killArgv(slug: "abc"), [RemoteControl.binaryName(), "kill", "--slug", "abc"])
    }

    func testAcceptsTypedInput() {
        XCTAssertTrue(RcKind.claudeRc.acceptsTypedInput)
        XCTAssertTrue(RcKind.shell.acceptsTypedInput)
        XCTAssertFalse(RcKind.claudeBroker.acceptsTypedInput)
    }

    func testCreatableKindsExcludeBroker() {
        XCTAssertEqual(RcKind.creatable, [.claudeRc, .shell])
        XCTAssertFalse(RcKind.creatable.contains(.claudeBroker))
    }

    func testNormalizeRcPromptTrimsAndAllows() throws {
        XCTAssertNil(try RemoteControl.normalizeRcPrompt(nil, kind: .claudeRc))
        XCTAssertNil(try RemoteControl.normalizeRcPrompt("   \n ", kind: .claudeRc))
        XCTAssertEqual(try RemoteControl.normalizeRcPrompt("  hi there  ", kind: .claudeRc), "hi there")
        XCTAssertEqual(try RemoteControl.normalizeRcPrompt("npm test", kind: .shell), "npm test")
        // Surrounding whitespace/newlines are trimmed (not rejected), matching
        // shed-remote-agent; only an *embedded* control char is rejected.
        XCTAssertEqual(try RemoteControl.normalizeRcPrompt("\n  npm test \n", kind: .shell), "npm test")
        // A 2000-byte value is the boundary and is allowed.
        XCTAssertEqual(try RemoteControl.normalizeRcPrompt(String(repeating: "a", count: 2000), kind: .shell)?.utf8.count, 2000)
    }

    func testNormalizeRcPromptRejects() {
        // Pin the specific badRequest reason — these strings are the IPC
        // invalid-param message contract, so a plain "throws" check would hide a
        // regression in the mapping.
        func assertBadRequest(_ run: @autoclosure () throws -> String?,
                              reasonContains needle: String, line: UInt = #line) {
            XCTAssertThrowsError(try run(), line: line) { error in
                guard case RcError.badRequest(let reason) = error else {
                    return XCTFail("expected RcError.badRequest, got \(error)", line: line)
                }
                XCTAssertTrue(reason.contains(needle), "unexpected reason: \(reason)", line: line)
            }
        }
        assertBadRequest(try RemoteControl.normalizeRcPrompt("a\nb", kind: .claudeRc),
                         reasonContains: "control characters")
        assertBadRequest(try RemoteControl.normalizeRcPrompt(String(repeating: "a", count: 2001), kind: .shell),
                         reasonContains: "exceeds 2000 bytes")
        assertBadRequest(try RemoteControl.normalizeRcPrompt("hello", kind: .claudeBroker),
                         reasonContains: "does not accept an initial prompt")
    }

    func testCreateInvocationPairsFlagAndStdin() {
        let withPrompt = RemoteControl.createInvocation(
            kind: .claudeRc, name: "n", slug: "s", workdir: nil,
            createdBy: "shed-desktop/0", target: "t", prompt: "do it")
        XCTAssertTrue(withPrompt.argv.contains("--prompt-stdin"))
        XCTAssertEqual(withPrompt.stdin, "do it")

        let noPrompt = RemoteControl.createInvocation(
            kind: .claudeRc, name: "n", slug: "s", workdir: nil,
            createdBy: "shed-desktop/0", target: "t", prompt: nil)
        XCTAssertFalse(noPrompt.argv.contains("--prompt-stdin"))
        XCTAssertNil(noPrompt.stdin)

        // A prompt is dropped for a kind that doesn't accept typed input.
        let broker = RemoteControl.createInvocation(
            kind: .claudeBroker, name: "n", slug: "s", workdir: nil,
            createdBy: "shed-desktop/0", target: "t", prompt: "ignored")
        XCTAssertFalse(broker.argv.contains("--prompt-stdin"))
        XCTAssertNil(broker.stdin)
    }

    func testExitCodeMapping() {
        XCTAssertEqual(RemoteControl.error(exitCode: 3, stderr: "exists", stdout: ""), .slugTaken("exists"))
        XCTAssertEqual(RemoteControl.error(exitCode: 4, stderr: "gone", stdout: ""), .notFound("gone"))
        XCTAssertEqual(RemoteControl.error(exitCode: 2, stderr: "bad", stdout: ""), .badRequest("bad"))
        XCTAssertEqual(RemoteControl.error(exitCode: 127, stderr: "command not found", stdout: ""), .missingBinary)
        if case .failed = RemoteControl.error(exitCode: 1, stderr: "boom", stdout: "") {} else {
            XCTFail("generic exit should map to .failed")
        }
    }

    func testDecodeSessionAndAdapt() throws {
        let json = """
        {"slug":"abc234","tmux_session":"rc-abc234","kind":"claude-rc","state":"ready",
         "managed":true,"workdir":"/home/shed","url":"https://claude.ai/code/session_01",
         "id":"id-1","created_by":"shed-remote-agent/0.1.0"}
        """
        let dto = try RemoteControl.decodeSession(json)
        // display_name omitted → adapter applies <shed>/<slug>; id → rcID.
        let s = RemoteControl.rcSession(fromDTO: dto, serverName: "mini3", shed: "demo")
        XCTAssertEqual(s.host, "mini3")
        XCTAssertEqual(s.shed, "demo")
        XCTAssertEqual(s.slug, "abc234")
        XCTAssertEqual(s.displayName, "demo/abc234")
        XCTAssertEqual(s.kind, .claudeRc)
        XCTAssertEqual(s.state, .ready)
        XCTAssertEqual(s.rcID, "id-1")
        XCTAssertTrue(s.managed)
    }

    func testDecodeInvalidDTOThrows() {
        XCTAssertThrowsError(try RemoteControl.decodeSession("not json"))
        XCTAssertThrowsError(try RemoteControl.decodeSession("{\"slug\":\"x\"}"))  // missing required fields
    }

    /// The golden fixture is byte-identical to shed-remote-agent's
    /// packages/shared/src/schemas/rcSessionDto.golden.json — both repos assert it
    /// decodes, guarding the cross-tool stdout contract.
    func testGoldenFixtureDecodes() throws {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .appendingPathComponent("Fixtures/rcSessionDto.golden.json")
        let dtos = try RemoteControl.decodeList(String(contentsOf: url, encoding: .utf8))
        XCTAssertEqual(dtos.count, 2)

        let full = dtos[0]
        XCTAssertEqual(full.kind, .claudeRc)
        XCTAssertEqual(full.state, .ready)
        XCTAssertTrue(full.managed)
        XCTAssertNotNil(full.id)
        XCTAssertEqual(full.url, "https://claude.ai/code/session_01RCkTDrdZ2Rr12sD5dfMjgr")

        let minimal = dtos[1]
        XCTAssertEqual(minimal.kind, .claudeBroker)
        XCTAssertFalse(minimal.managed)
        XCTAssertNil(minimal.displayName)   // omitted, not null
        XCTAssertNil(minimal.workdir)
        XCTAssertNil(minimal.url)
        // The adapter fills the <shed>/<slug> display fallback the binary can't know.
        let adapted = RemoteControl.rcSession(fromDTO: minimal, serverName: "h", shed: "demo")
        XCTAssertEqual(adapted.displayName, "demo/brk900")
    }

    private func argvHasPair(_ argv: [String], _ flag: String, _ value: String) -> Bool {
        guard let i = argv.firstIndex(of: flag), i + 1 < argv.count else { return false }
        return argv[i + 1] == value
    }
}

// MARK: - RcSession wire shape (rc_id vs Identifiable id, defensive decode)

final class RCWireTests: XCTestCase {
    func testRcIDEncodesUnderRcIdNotId() throws {
        let s = RcSession(
            host: "mini3", shed: "demo", slug: "abc234", tmuxSession: "rc-abc234",
            displayName: "d", workdir: "/w", kind: .claudeBroker, state: .ready, url: nil,
            rcID: "uuid-123", createdBy: "shed-desktop/0.1.0",
            createdAt: "2026-06-13T00:00:00Z", targetLabel: "shed:demo@mini3", managed: true)
        let data = try JSONEncoder().encode(s)
        let json = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        XCTAssertEqual(json["rc_id"] as? String, "uuid-123")
        XCTAssertNil(json["id"])  // the computed Identifiable id is never encoded
        XCTAssertEqual(json["kind"] as? String, "claude-broker")
        XCTAssertEqual(json["managed"] as? Bool, true)

        let back = try JSONDecoder().decode(RcSession.self, from: data)
        XCTAssertEqual(back, s)
        XCTAssertEqual(back.id, "mini3/demo/abc234")  // Identifiable stays composite
        XCTAssertEqual(back.rcID, "uuid-123")
    }

    func testDecodeDefaultsManagedFalseWhenAbsent() throws {
        let json = Data("""
        {"host":"h","shed":"d","slug":"s","tmux_session":"rc-s","display_name":"n","workdir":"/w","kind":"claude-rc","state":"ready"}
        """.utf8)
        let s = try JSONDecoder().decode(RcSession.self, from: json)
        XCTAssertFalse(s.managed)
        XCTAssertNil(s.rcID)
        XCTAssertNil(s.createdBy)
    }
}
