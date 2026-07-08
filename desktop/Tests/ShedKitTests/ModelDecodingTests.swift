// Decoding + config-parsing tests against the real shed-server shapes
// captured as fixtures (notably `{"sheds": null}` and mixed timestamps).

import XCTest
@testable import ShedKit

final class ModelDecodingTests: XCTestCase {
    func testShedDecodesRealServerFixture() throws {
        // The exact server shape: no `host` field, many optionals omitted.
        let json = """
        {"name":"hello-world","status":"running","created_at":"2026-05-31T13:33:00.884935839-05:00",
         "container_id":"fc-hello-world","backend":"firecracker","ip_address":"172.30.0.2","cpus":2,
         "memory_mb":4096,"pid":392574,"started_at":"2026-05-31T18:33:02.364547927Z"}
        """
        let shed = try JSONDecoder().decode(Shed.self, from: Data(json.utf8))
        XCTAssertEqual(shed.name, "hello-world")
        XCTAssertEqual(shed.status, .running)
        XCTAssertEqual(shed.backend, "firecracker")
        XCTAssertEqual(shed.cpus, 2)
        XCTAssertEqual(shed.memoryMB, 4096)
        XCTAssertEqual(shed.host, "")     // absent on the wire; client stamps it
        XCTAssertNil(shed.repo)           // omitted -> nil, not a decode failure
        XCTAssertEqual(shed.activeNamespaces, [])  // absent -> empty
    }

    func testShedImageDisplayLabelAndDigest() throws {
        // A shed created from an alias carries both a label and a pinned digest.
        let json = """
        {"name":"x","status":"running","image":"full",
         "image_digest":"sha256:2d9669bcf0cd25ef7dc0638dc72c7380c716e3e9d336c5d234ffa4888f28713a"}
        """
        let shed = try JSONDecoder().decode(Shed.self, from: Data(json.utf8))
        XCTAssertEqual(shed.imageDigest, "sha256:2d9669bcf0cd25ef7dc0638dc72c7380c716e3e9d336c5d234ffa4888f28713a")
        XCTAssertEqual(shed.shortImageDigest, "sha256:2d9669bcf0cd")
        XCTAssertEqual(shed.imageDisplay, "full (sha256:2d9669bcf0cd)")
    }

    func testShedImageDisplayDigestOnly() throws {
        // The v0.6.0 default-image case: no `image`, only `image_digest`.
        let json = #"{"name":"x","status":"running","image_digest":"sha256:abcdef0123456789aa"}"#
        let shed = try JSONDecoder().decode(Shed.self, from: Data(json.utf8))
        XCTAssertNil(shed.image)
        XCTAssertEqual(shed.imageDisplay, "sha256:abcdef012345")
    }

    func testShedImageDisplayNilWhenNeither() throws {
        let shed = try JSONDecoder().decode(Shed.self, from: Data(#"{"name":"x","status":"running"}"#.utf8))
        XCTAssertNil(shed.imageDisplay)
    }

    func testShedImageDecodesEnrichedFields() throws {
        let json = """
        {"name":"ghcr.io/x/base:v1","docker_ref":"ghcr.io/x/base:v1","alias":"base",
         "is_default":true,"cached":true,"in_use":false,"source":"config",
         "digest":"sha256:aa11","size_bytes":1073741824}
        """
        let img = try JSONDecoder().decode(ShedImage.self, from: Data(json.utf8))
        XCTAssertEqual(img.alias, "base")
        XCTAssertTrue(img.isDefault)
        XCTAssertTrue(img.cached)
        XCTAssertEqual(img.dockerRef, "ghcr.io/x/base:v1")
        XCTAssertEqual(img.sizeBytes, 1073741824)
    }

    func testShedImageShortRef() {
        // Registry/namespace stripped → repo:tag.
        XCTAssertEqual(ShedImage(name: "ghcr.io/charliek/shed-vz-full:v0.6.2",
                                 dockerRef: "ghcr.io/charliek/shed-vz-full:v0.6.2").shortRef,
                       "shed-vz-full:v0.6.2")
        // No registry/namespace → unchanged.
        XCTAssertEqual(ShedImage(name: "base:latest").shortRef, "base:latest")
        // Empty docker_ref falls back to name.
        XCTAssertEqual(ShedImage(name: "ghcr.io/x/img:tag", dockerRef: "").shortRef, "img:tag")
    }

    func testShedImageLabelResolvesDigestToRepoTag() {
        let images = [
            ShedImage(name: "ghcr.io/x/shed-vz-full:v0.6.2", dockerRef: "ghcr.io/x/shed-vz-full:v0.6.2",
                      digest: "sha256:2d9669bcf0cd25ef"),
        ]
        // A digest match resolves to the image's repo:tag (not the bare sha).
        let matched = Shed(host: "h", name: "a", status: .running, imageDigest: "sha256:2d9669bcf0cd25ef")
        XCTAssertEqual(matched.imageLabel(in: images), "shed-vz-full:v0.6.2")
        // No matching image → fall back to the short digest.
        let unmatched = Shed(host: "h", name: "b", status: .running,
                             imageDigest: "sha256:abcdef0123456789abcdef")
        XCTAssertEqual(unmatched.imageLabel(in: images), "sha256:abcdef012345")
        // Empty image list → fall back too.
        XCTAssertEqual(matched.imageLabel(in: []), "sha256:2d9669bcf0cd")
    }

    func testShedImageLenientForPreV061Server() throws {
        // Older server: no alias / is_default — must default cleanly, not throw.
        let img = try JSONDecoder().decode(ShedImage.self, from: Data(#"{"name":"base","source":"config","cached":true}"#.utf8))
        XCTAssertNil(img.alias)
        XCTAssertFalse(img.isDefault)
        XCTAssertTrue(img.cached)
    }

    func testShedListNullDecodesToEmpty() throws {
        struct Wrapper: Decodable { let sheds: [Shed]? }
        let w = try JSONDecoder().decode(Wrapper.self, from: Data(#"{"sheds": null}"#.utf8))
        XCTAssertEqual(w.sheds ?? [], [])
    }

    func testUnknownStatusMapsToUnknown() {
        XCTAssertEqual(ShedStatus(serverValue: "provisioning"), .unknown)
        XCTAssertEqual(ShedStatus(serverValue: "running"), .running)
    }

    func testFlexibleTimestampParsesBothForms() {
        XCTAssertNotNil(DateFormatting.parseFlexibleTimestamp("2026-05-31T13:33:00.884935839-05:00"))
        XCTAssertNotNil(DateFormatting.parseFlexibleTimestamp("2026-05-31T18:33:02.364547927Z"))
        XCTAssertNotNil(DateFormatting.parseFlexibleTimestamp("2026-05-31T18:33:02Z (UTC)"))
        XCTAssertNil(DateFormatting.parseFlexibleTimestamp("not-a-date"))
    }

    func testConfigParsesServersAndDefault() {
        let yaml = """
        servers:
            mini2:
                host: mini2
                http_port: 8080
                ssh_port: 2222
                control_token: shed_control_abc123
                added_at: 2026-05-09T01:44:44.395385-05:00
            my-server:
                host: localhost
                http_port: 8080
                ssh_port: 2222
        default_server: mini2
        sheds: {}
        """
        let config = ShedConfig.parse(yaml)
        XCTAssertEqual(config.servers.count, 2)
        XCTAssertEqual(config.defaultServer, "mini2")
        let mini2 = config.servers.first { $0.name == "mini2" }
        XCTAssertEqual(mini2?.host, "mini2")
        XCTAssertEqual(mini2?.httpPort, 8080)
        XCTAssertEqual(mini2?.sshPort, 2222)
        XCTAssertEqual(mini2?.controlToken, "shed_control_abc123")
        // An entry without a token parses to an empty controlToken.
        XCTAssertEqual(config.servers.first { $0.name == "my-server" }?.controlToken, "")
    }

    func testConfigMissingFileIsEmptyNotCrash() {
        let config = ShedConfig.load(path: "/nonexistent/\(UUID().uuidString)/config.yaml")
        XCTAssertEqual(config, .empty)
    }

    func testAuditEventFrameDecodesCodeAndReason() throws {
        // A failed docker get from an enriched host-agent carries code + reason.
        let json = """
        {"type":"event","kind":"audit","ns":"docker-credentials","op":"get","shed":"t1",
         "result":"error","detail":"https://index.docker.io/v1/",
         "code":"REGISTRY_NOT_ALLOWED","reason":"registry index.docker.io not in allowlist",
         "approval":"approve-all","ts":"2026-06-10T23:00:00Z"}
        """
        let frame = try JSONDecoder().decode(AuditEventFrame.self, from: Data(json.utf8))
        XCTAssertEqual(frame.code, "REGISTRY_NOT_ALLOWED")
        XCTAssertEqual(frame.reason, "registry index.docker.io not in allowlist")

        // …and the mapping into the stored entry preserves them.
        let entry = AuditEntry(frame: frame)
        XCTAssertEqual(entry.result, "error")
        XCTAssertEqual(entry.code, "REGISTRY_NOT_ALLOWED")
        XCTAssertEqual(entry.reason, "registry index.docker.io not in allowlist")
    }

    func testAuditEventFrameWithoutCodeReasonDecodesNil() throws {
        // Pre-enrichment host-agents omit code/reason — must default to nil, not throw.
        let json = #"{"type":"event","ns":"ssh-agent","op":"sign","shed":"x","result":"ok"}"#
        let frame = try JSONDecoder().decode(AuditEventFrame.self, from: Data(json.utf8))
        XCTAssertNil(frame.code)
        XCTAssertNil(frame.reason)
        let entry = AuditEntry(frame: frame)
        XCTAssertNil(entry.code)
        XCTAssertNil(entry.reason)
    }
}
