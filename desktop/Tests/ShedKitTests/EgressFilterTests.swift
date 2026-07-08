import XCTest

import ShedKit

final class EgressFilterTests: XCTestCase {
    func testIsEgress() {
        let egress = AuditEntry(id: "1", ts: "t", source: .hostAgent, ns: "egress", result: "deny")
        let ssh = AuditEntry(id: "2", ts: "t", source: .hostAgent, ns: "ssh-agent", result: "ok")
        let noNS = AuditEntry(id: "3", ts: "t", source: .app, result: "ok")
        XCTAssertTrue(egress.isEgress)
        XCTAssertFalse(ssh.isEgress)
        XCTAssertFalse(noNS.isEgress)
    }

    func testFilterEgressKeepsOrder() {
        let entries = [
            AuditEntry(id: "1", ts: "t", source: .hostAgent, ns: "egress", result: "deny"),
            AuditEntry(id: "2", ts: "t", source: .hostAgent, ns: "ssh-agent", result: "ok"),
            AuditEntry(id: "3", ts: "t", source: .hostAgent, ns: "egress", result: "allow"),
        ]
        let egressOnly = entries.filter(\.isEgress)
        XCTAssertEqual(egressOnly.map(\.id), ["1", "3"])
    }

    func testEgressNamespaceConstant() {
        XCTAssertEqual(AuditEntry.egressNamespace, "egress")
    }
}
