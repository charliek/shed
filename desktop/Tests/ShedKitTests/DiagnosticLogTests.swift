import Foundation
import XCTest

@testable import ShedKit

final class DiagnosticLogTests: XCTestCase {
    private func tempLogPath() -> String {
        NSTemporaryDirectory() + "diaglog-\(UUID().uuidString)/shed-desktop.log"
    }

    func testWritesFormattedLine() throws {
        let path = tempLogPath()
        let log = DiagnosticLog(path: path, now: { Date(timeIntervalSince1970: 0) }, mirror: nil)
        log.log(.info, "config", "resolved server", [("server", "localmac"), ("security", "secure")])
        log.flush()

        let contents = try String(contentsOfFile: path, encoding: .utf8)
        XCTAssertTrue(contents.contains("INFO config resolved server"), contents)
        XCTAssertTrue(contents.contains("server=localmac"), contents)
        XCTAssertTrue(contents.contains("security=secure"), contents)
    }

    func testRedactsTokensEverywhere() throws {
        let path = tempLogPath()
        let log = DiagnosticLog(path: path, mirror: nil)
        // Token-shaped strings in both the message and a field value.
        log.log(
            .error, "token",
            "mint failed: Authorization Bearer shed_control_abc123def456 cert abcdef0123456789abcdef0123456789abcdef01",
            [("hdr", "Bearer supersecrettoken99")])
        log.flush()

        let contents = try String(contentsOfFile: path, encoding: .utf8)
        XCTAssertFalse(contents.contains("supersecrettoken99"), contents)
        XCTAssertFalse(contents.contains("shed_control_abc123def456"), contents)
        XCTAssertFalse(contents.contains("abcdef0123456789abcdef0123456789abcdef01"), contents)
        XCTAssertTrue(contents.contains("[REDACTED"), contents)
    }

    func testRotatesKeepingGenerations() throws {
        let path = tempLogPath()
        // Tiny cap so a couple of writes trigger rotation; keep 2 generations.
        let log = DiagnosticLog(path: path, maxBytes: 80, keep: 2, mirror: nil)
        for i in 0..<20 { log.log(.info, "c", "line \(i) padding-padding-padding-padding") }
        log.flush()

        let fm = FileManager.default
        XCTAssertTrue(fm.fileExists(atPath: path), "current log should exist")
        XCTAssertTrue(fm.fileExists(atPath: path + ".1"), "first rotated generation should exist")
        XCTAssertTrue(fm.fileExists(atPath: path + ".2"), "second rotated generation should exist")
        XCTAssertFalse(fm.fileExists(atPath: path + ".3"), "generations beyond keep=2 must be pruned")
    }
}
