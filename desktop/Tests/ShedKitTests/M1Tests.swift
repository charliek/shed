// M1 unit tests: the terminal command builder and the SSE parser.

import XCTest
@testable import ShedKit

final class TerminalLauncherTests: XCTestCase {
    func testSSHCommandWithoutSession() {
        let cmd = TerminalLauncher.sshCommand(shed: "stbot", host: "mini3", sshPort: 2222)
        XCTAssertEqual(cmd.argv, ["ssh", "-t"] + ShedSSH.hostKeyOptions + ["stbot@mini3", "-p", "2222"])
        // Pins the host key with strict checking (Phase 3 hardening).
        XCTAssertTrue(cmd.argv.contains("StrictHostKeyChecking=yes"))
        XCTAssertFalse(cmd.argv.contains("StrictHostKeyChecking=accept-new"))
        XCTAssertTrue(cmd.command.contains("stbot@mini3 -p 2222"))
    }

    func testSSHCommandWithSession() {
        let cmd = TerminalLauncher.sshCommand(shed: "stbot", host: "mini3", sshPort: 2222, session: "rc-demo")
        XCTAssertEqual(
            cmd.argv,
            ["ssh", "-t"] + ShedSSH.hostKeyOptions + ["stbot@mini3", "-p", "2222", "tmux", "attach", "-t", "rc-demo"])
    }

    func testSSHHostKeyOptionsArePinned() {
        // Both SSH paths (terminal + remote-control) verify the host key against
        // ~/.shed/known_hosts with strict checking.
        XCTAssertEqual(
            ShedSSH.hostKeyOptions,
            ["-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=\(ShedSSH.knownHostsPath)"])
        XCTAssertTrue(ShedSSH.knownHostsPath.hasSuffix("/.shed/known_hosts"))
        let rc = RemoteControl.sshArgv(user: "stbot", host: "mini3", port: 2222, remoteArgv: ["true"])
        XCTAssertTrue(rc.contains("StrictHostKeyChecking=yes"))
        XCTAssertFalse(rc.contains("StrictHostKeyChecking=accept-new"))
    }

    func testShellQuoteEscapesSpacesAndQuotes() {
        XCTAssertEqual(TerminalLauncher.shellQuote("plain"), "plain")
        XCTAssertEqual(TerminalLauncher.shellQuote("a b"), "'a b'")
        XCTAssertEqual(TerminalLauncher.shellQuote("it's"), "'it'\\''s'")
    }

    // MARK: - resolveLaunch matrix

    private let fixtureCmd = TerminalLauncher.sshCommand(shed: "stbot", host: "mini3", sshPort: 2222)
    private let scriptsDir = URL(fileURLWithPath: "/Apps/ShedDesktop.app/Contents/Resources/bin")

    private func resolve(_ preset: TerminalPreset, template: String? = nil, scriptsDir: URL?) -> LaunchInvocation {
        TerminalLauncher.resolveLaunch(
            preset: preset, cmd: fixtureCmd, shed: "stbot",
            customTemplate: template, scriptsDir: scriptsDir)
    }

    func testResolveTerminalApp() {
        let inv = resolve(.terminalApp, scriptsDir: scriptsDir)
        XCTAssertEqual(inv.executable, "/usr/bin/osascript")
        XCTAssertEqual(inv.arguments.first, "-e")
        XCTAssertTrue(inv.arguments[1].contains("tell application \"Terminal\""))
        XCTAssertTrue(inv.arguments[1].contains(fixtureCmd.command))
    }

    func testResolveScriptPresetsArgvContract() {
        // Every script preset → its interpreter + [scriptPath, shed, cmd].
        let cases: [(TerminalPreset, String, String)] = [
            (.ghostty, "/bin/bash", "shed-open-ghostty"),
            (.iterm2, "/bin/bash", "shed-open-iterm2"),
            (.warp, "/bin/bash", "shed-open-warp"),
            (.roost, "/usr/bin/python3", "shed-open-roost.py"),
        ]
        for (preset, interp, script) in cases {
            let inv = resolve(preset, scriptsDir: scriptsDir)
            XCTAssertEqual(inv.executable, interp, "\(preset)")
            XCTAssertEqual(
                inv.arguments,
                ["/Apps/ShedDesktop.app/Contents/Resources/bin/\(script)", "stbot", fixtureCmd.command],
                "\(preset)")
        }
    }

    func testResolveCustomSubstitutesCmdAndShed() {
        let inv = resolve(.custom, template: "echo {shed}: {cmd}", scriptsDir: scriptsDir)
        XCTAssertEqual(inv.executable, "/bin/sh")
        XCTAssertEqual(inv.arguments, ["-c", "echo stbot: \(fixtureCmd.command)"])
    }

    func testResolveScriptPresetFallsBackToTerminalAppWithoutScriptsDir() {
        let inv = resolve(.ghostty, scriptsDir: nil)
        XCTAssertEqual(inv.executable, "/usr/bin/osascript")
        XCTAssertTrue(inv.arguments[1].contains("tell application \"Terminal\""))
    }

    func testResolveEmptyCustomTemplateFallsBackToTerminalApp() {
        let inv = resolve(.custom, template: "   ", scriptsDir: scriptsDir)
        XCTAssertEqual(inv.executable, "/usr/bin/osascript")
        XCTAssertTrue(inv.arguments[1].contains("tell application \"Terminal\""))
    }

    // MARK: - preset derive-default

    func testDeriveDefaults() {
        // No explicit store: empty legacy → terminalApp; non-empty → custom.
        XCTAssertEqual(TerminalPreset.derive(legacyTemplate: "", storedRaw: nil), .terminalApp)
        XCTAssertEqual(TerminalPreset.derive(legacyTemplate: "   ", storedRaw: nil), .terminalApp)
        XCTAssertEqual(TerminalPreset.derive(legacyTemplate: "ghostty -e {cmd}", storedRaw: nil), .custom)
        // An explicit stored preset wins over the legacy template.
        XCTAssertEqual(TerminalPreset.derive(legacyTemplate: "ghostty -e {cmd}", storedRaw: "roost"), .roost)
        // Unknown stored raw → fall back to derivation.
        XCTAssertEqual(TerminalPreset.derive(legacyTemplate: "", storedRaw: "bogus"), .terminalApp)
    }
}

final class SSEParserTests: XCTestCase {
    func testParsesProgressThenComplete() {
        let lines = [
            "event: progress", "data: {\"message\":\"resolving\"}", "",
            "event: progress", "data: {\"message\":\"starting\"}", "",
            "event: complete", "data: {\"name\":\"folio\"}", "",
        ]
        let events = SSEParser.parse(lines: lines)
        XCTAssertEqual(events.count, 3)
        XCTAssertEqual(events[0].event, "progress")
        XCTAssertEqual(events[0].data, "{\"message\":\"resolving\"}")
        XCTAssertEqual(events[2].event, "complete")
    }

    func testFlushesFinalRecordWithoutTrailingBlank() {
        var parser = SSEParser()
        XCTAssertNil(parser.push(line: "event: complete"))
        XCTAssertNil(parser.push(line: "data: {}"))
        // No trailing blank line — finish() must flush it.
        let ev = parser.finish()
        XCTAssertEqual(ev?.event, "complete")
        XCTAssertEqual(ev?.data, "{}")
    }

    func testIgnoresCommentAndStripsCR() {
        var parser = SSEParser()
        XCTAssertNil(parser.push(line: ": keep-alive"))
        XCTAssertNil(parser.push(line: "event: progress\r"))
        XCTAssertNil(parser.push(line: "data: hi\r"))
        let ev = parser.push(line: "")
        XCTAssertEqual(ev, SSEEvent(event: "progress", data: "hi"))
    }
}
