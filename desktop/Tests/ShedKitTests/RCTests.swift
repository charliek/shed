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
        XCTAssertTrue(RcKind.codex.acceptsTypedInput)
        XCTAssertTrue(RcKind.opencode.acceptsTypedInput)
        XCTAssertTrue(RcKind.cursor.acceptsTypedInput)
        XCTAssertFalse(RcKind.claudeBroker.acceptsTypedInput)
        // An unknown kind gets no affordances (unknown-kind policy).
        XCTAssertFalse(RcKind.other("borg").acceptsTypedInput)
    }

    func testCreatableKindsExcludeBrokerAndUnknown() {
        XCTAssertEqual(RcKind.creatable, [.claudeRc, .codex, .opencode, .cursor, .shell])
        XCTAssertFalse(RcKind.creatable.contains(.claudeBroker))
    }

    /// Unknown-kind policy: an unrecognized wire value is preserved verbatim, round-
    /// trips as its raw string, and gets no claude affordance.
    func testUnknownKindPreservedAndNeutral() throws {
        let json = """
        {"slug":"z","tmux_session":"rc-z","kind":"borg","state":"ready","managed":true}
        """
        let dto = try RemoteControl.decodeSession(json)
        XCTAssertEqual(dto.kind, .other("borg"))
        XCTAssertFalse(dto.kind.isKnown)
        XCTAssertNil(dto.kind.tool)
        // Re-encodes as the raw string.
        let data = try JSONEncoder().encode(dto)
        let obj = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        XCTAssertEqual(obj["kind"] as? String, "borg")
        // A pane never yields a claude URL for an unknown kind.
        let c = RemoteControl.classifyPane(kind: .other("borg"), pane: "https://claude.ai/code/session_X")
        XCTAssertEqual(c.state, .ready)
        XCTAssertNil(c.url)
    }

    func testAuthHintPerKind() {
        XCTAssertTrue(RcKind.claudeRc.authHint.contains("/login"))
        XCTAssertTrue(RcKind.codex.authHint.contains("codex"))
        XCTAssertTrue(RcKind.cursor.authHint.contains("cursor-agent login"))
        XCTAssertTrue(RcKind.opencode.authHint.contains("opencode auth login"))
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
        let parsed = try RemoteControl.decodeListResponse(String(contentsOf: url, encoding: .utf8))
        let dtos = parsed.sessions
        XCTAssertEqual(dtos.count, 2)

        // The golden gained a capabilities block (in lockstep with the Go golden).
        let caps = try XCTUnwrap(parsed.capabilities)
        XCTAssertEqual(caps.rcVersion, 4)
        XCTAssertTrue(caps.hasFeature("generic-perm"))
        XCTAssertTrue(caps.hasFeature("contract-v2"))
        XCTAssertEqual(caps.agents["codex"]?.installed, true)
        XCTAssertEqual(caps.agents["cursor"]?.installed, false)
        // Gating: claude/codex/opencode installed + advertised → offered; cursor
        // advertised but not installed → not offered. broker is advertised (so
        // `offers` is true) but is excluded from the create form via `creatable`.
        XCTAssertEqual(caps.creatableKinds, [.claudeRc, .codex, .opencode, .shell])
        XCTAssertFalse(caps.offers(.cursor))
        XCTAssertFalse(caps.creatableKinds.contains(.claudeBroker))

        // Contract v2 kind_features additions (normative R0 matrix): codex streams
        // a normalized message feed and attaches over tmux; interrupt is
        // unconditionally false in this phase.
        let codex = try XCTUnwrap(caps.kindFeatures["codex"])
        XCTAssertEqual(codex.feed, "messages")
        XCTAssertTrue(codex.feedMessages)
        XCTAssertFalse(codex.interrupt)
        XCTAssertEqual(codex.attachKind, "tmux")

        // opencode is the first LIVE lane (its embedded server takes whole turns,
        // interrupts and approvals through the hub), so its row diverges from codex's
        // deliberately: input "turn" supersedes "gated" and approvals are "remote".
        let opencode = try XCTUnwrap(caps.kindFeatures["opencode"])
        XCTAssertEqual(opencode.feed, "messages")
        XCTAssertEqual(opencode.input, "turn")
        XCTAssertEqual(opencode.approvals, "remote")
        XCTAssertTrue(opencode.interrupt)
        XCTAssertEqual(opencode.attachKind, "tmux")

        let full = dtos[0]
        XCTAssertEqual(full.kind, .claudeRc)
        XCTAssertEqual(full.state, .ready)
        XCTAssertTrue(full.managed)
        XCTAssertNotNil(full.id)
        XCTAssertEqual(full.url, "https://claude.ai/code/session_01RCkTDrdZ2Rr12sD5dfMjgr")
        // Contract v2 session fields: lane + the activity trio.
        XCTAssertEqual(full.lane, "tui")
        XCTAssertEqual(full.laneOrTui, "tui")
        XCTAssertEqual(full.activity, "working")
        XCTAssertNotNil(full.activityAt)
        XCTAssertEqual(full.lastMessage, "Running the test suite now.")

        let minimal = dtos[1]
        XCTAssertEqual(minimal.kind, .claudeBroker)
        XCTAssertFalse(minimal.managed)
        XCTAssertNil(minimal.displayName)   // omitted, not null
        XCTAssertNil(minimal.workdir)
        XCTAssertNil(minimal.url)
        XCTAssertEqual(minimal.lane, "tui")
        // The adapter fills the <shed>/<slug> display fallback the binary can't know
        // and propagates lane through to the enriched RcSession.
        let adapted = RemoteControl.rcSession(fromDTO: minimal, serverName: "h", shed: "demo")
        XCTAssertEqual(adapted.displayName, "demo/brk900")
        XCTAssertEqual(adapted.laneOrTui, "tui")
    }

    /// A v3-shaped payload (contract v2's `feed`/`interrupt`/`attach` absent, only
    /// the deprecated `watch`/`input` present) must still decode safely — the
    /// tolerant defaults, and `feedMessages`' fallback to `watch` when `feed` is
    /// blank, are what let an old shed image's payload render without crashing.
    func testV3KindFeaturesDecodesWithFeedFallback() throws {
        let json = #"""
        {"post_input":true,"approvals":"tui","watch":true,"input":"gated"}
        """#
        let f = try JSONDecoder().decode(RcKindFeatures.self, from: Data(json.utf8))
        XCTAssertTrue(f.postInput)
        XCTAssertEqual(f.approvals, "tui")
        XCTAssertTrue(f.watch)
        XCTAssertEqual(f.input, "gated")
        XCTAssertEqual(f.feed, "")           // absent → default
        XCTAssertFalse(f.interrupt)          // absent → default
        XCTAssertEqual(f.attach, "")         // absent → default
        // feed is blank, so feedMessages falls back to the deprecated watch bit.
        XCTAssertTrue(f.feedMessages)
        // attach is blank, so attachKind falls back to the tmux default.
        XCTAssertEqual(f.attachKind, "tmux")
    }

    /// An old baked-in binary's bare `{"rc_sessions":[…]}` envelope decodes with
    /// capabilities == nil (tolerant of absence) — the capability leg degrades.
    func testOldBinaryEnvelopeHasNoCapabilities() throws {
        let stdout = #"{"rc_sessions":[{"slug":"a","tmux_session":"rc-a","kind":"shell","state":"ready","managed":true}]}"#
        let parsed = try RemoteControl.decodeListResponse(stdout)
        XCTAssertEqual(parsed.sessions.count, 1)
        XCTAssertNil(parsed.capabilities)
    }

    /// Present-but-EMPTY capabilities offer nothing: only ABSENT capabilities may
    /// fall back to claude+shell; a shed that advertises kinds with no installed
    /// agents yields an empty creatable set (the launch UIs show "unavailable"
    /// rather than inventing claude).
    func testPresentButEmptyCapabilitiesOfferNothing() throws {
        // Deliberately v3-shaped (no contract-v2 kind_features fields, empty map):
        // exercises the offers/creatable gating, which doesn't care about rc_version
        // or the new fields — left as v3 input rather than bumped to the current golden.
        let stdout = #"""
        {"rc_sessions":[],
         "capabilities":{"rc_version":3,
           "kinds":["claude-rc","codex"],
           "agents":{"claude":{"installed":false},"codex":{"installed":false}},
           "features":[],"kind_features":{}}}
        """#
        let caps = try XCTUnwrap(RemoteControl.decodeListResponse(stdout).capabilities)
        XCTAssertTrue(caps.creatableKinds.isEmpty)
        XCTAssertFalse(caps.offers(.claudeRc))
        XCTAssertFalse(caps.offers(.shell))  // not even advertised
    }

    /// The downgrade path: a probed shed whose refresh returned nil capabilities
    /// (old/downgraded image) must LOSE its stale cached entry; un-probed sheds
    /// keep theirs. Pins the merge AppModel.rcList applies.
    func testMergeCapabilitiesDowngradeRemovesStaleEntry() throws {
        // Deliberately v3-shaped: this test is only about the cache-merge mechanics,
        // not the capabilities content, so it stays on the older shape rather than
        // being bumped to the current golden.
        let capsJSON = #"""
        {"rc_sessions":[],
         "capabilities":{"rc_version":3,"kinds":["shell"],"agents":{},
           "features":[],"kind_features":{}}}
        """#
        let caps = try XCTUnwrap(RemoteControl.decodeListResponse(capsJSON).capabilities)
        var cache = ["srv/web": caps, "srv/api": caps, "srv/db": caps]
        // web probed with fresh caps, api probed but downgraded (nil), db un-probed.
        RemoteControl.mergeCapabilities(
            into: &cache,
            probes: [("srv/web", caps), ("srv/api", nil)])
        XCTAssertNotNil(cache["srv/web"])   // overwritten
        XCTAssertNil(cache["srv/api"])      // removed — no stale gating
        XCTAssertNotNil(cache["srv/db"])    // un-probed, kept
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

    /// A wrong-TYPED field inside a pending_approvals element must degrade that
    /// field to its default (the Rust mirror's tolerance), never fail the whole
    /// session decode.
    func testFeedApprovalDecodeToleratesWrongTypes() throws {
        let json = #"""
        {"slug":"ab12cd","tmux_session":"rc-ab12cd","kind":"codex","state":"ready",
         "managed":true,"lane":"tui",
         "pending_approvals":[{"id":42,"status":"pending","decisions":"allow"}]}
        """#
        let dto = try JSONDecoder().decode(RcSessionDTO.self, from: Data(json.utf8))
        let a = try XCTUnwrap(dto.pendingApprovals?.first)
        XCTAssertEqual(a.id, "")          // wrong-typed → default, not a throw
        XCTAssertEqual(a.status, "pending")
        XCTAssertEqual(a.decisions, [])   // wrong-typed (not an array) → default

        // A MIXED decisions array degrades per ELEMENT like the Rust mirror's
        // filter_map — the string entries survive, only the wrong-typed ones drop.
        let mixed = #"""
        {"slug":"ab12cd","tmux_session":"rc-ab12cd","kind":"codex","state":"ready",
         "managed":true,"lane":"tui",
         "pending_approvals":[{"id":"ap-1","status":"pending",
                              "decisions":["allow",5,"deny",null,{"x":1}]}]}
        """#
        let mixedDTO = try JSONDecoder().decode(RcSessionDTO.self, from: Data(mixed.utf8))
        let m = try XCTUnwrap(mixedDTO.pendingApprovals?.first)
        XCTAssertEqual(m.id, "ap-1")
        XCTAssertEqual(m.decisions, ["allow", "deny"])
    }

    /// The enriched-session adapter sanitizes the guest-controlled preview the way
    /// the Rust core does: Unicode format chars (bidi override) stripped, trimmed,
    /// only-format-chars degrades to nil.
    func testAdapterCleansLastMessagePreview() throws {
        XCTAssertEqual(RemoteControl.cleanPreview("a\u{202E}b  "), "ab")
        XCTAssertNil(RemoteControl.cleanPreview("\u{202E}\u{FEFF}"))
        XCTAssertNil(RemoteControl.cleanPreview("   "))
        XCTAssertNil(RemoteControl.cleanPreview(nil))
    }
}
