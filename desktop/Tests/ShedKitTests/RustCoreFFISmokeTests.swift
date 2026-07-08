// RustCoreFFISmokeTests — the M0 de-risk canary.
//
// Proves the Rust core is reachable over the UniFFI bridge from a Swift 6
// strict-concurrency test context, with the static library linked into the
// test binary (no dylib, no runtime loader). It exercises the three FFI
// mechanisms Phase 1 depends on, before the milestones that rely on them:
//   * an async method   (every read/write op — M2/M4),
//   * an async foreign callback Swift→Rust (the TokenMinter — M3),
//   * task cancellation across the boundary (abandoned create streams — M4).
// If this fails, the whole Phase 1 toolchain is blocked (plans/…/M0).

import XCTest
import ShedRustCore

final class RustCoreFFISmokeTests: XCTestCase {
    /// Async method over FFI.
    func testRustPingOverFFI() async throws {
        let result = await ping(echo: "canary")
        XCTAssertEqual(result, "shed-core ok: canary")
    }

    /// Swift→Rust async foreign callback — the shape the real TokenMinter uses.
    func testForeignAsyncCallback() async throws {
        let out = await mintVia(minter: StubMinter(), server: "mini2")
        XCTAssertEqual(out, "token-for-mini2")
    }

    /// Cancelling the Swift Task around an in-flight FFI call must not hang or
    /// crash. (True cancellation-propagation is exercised more fully in M4.)
    func testCancellationDoesNotHang() async throws {
        let task = Task { await slowEcho(echo: "x", delayMs: 200) }
        task.cancel()
        _ = await task.value
    }

    /// The real ShedCore read client (M2): constructing it and calling a method
    /// against an unreachable base URL surfaces a typed ShedError — proving the
    /// async read method + record/error bridge without a server.
    func testShedCoreSurfacesTypedError() async throws {
        let core = try ShedCore(
            baseUrl: "http://127.0.0.1:1", serverName: "unreachable",
            token: "", pin: nil, minter: nil)
        do {
            _ = try await core.info()
            XCTFail("expected a ShedError against a closed port")
        } catch is ShedError {
            // Any variant is fine — this proves the FFI call + typed-error path.
        }
    }
}

/// A trivial `MinterProbe` conformer. `@unchecked Sendable`: its only state is
/// immutable, and it's handed to Rust across the async boundary.
private final class StubMinter: MinterProbe, @unchecked Sendable {
    func mint(server: String) async -> String { "token-for-\(server)" }
}
