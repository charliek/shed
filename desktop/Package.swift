// swift-tools-version: 6.0
//
// shed-desktop — a native macOS menu-bar control surface for the shed
// toolchain. Pure Swift (no C targets, no external deps for M0–M2):
// SwiftUI `MenuBarExtra` + `WindowGroup` for the UI, an in-process
// Unix-domain-socket JSON IPC server for drivability/testing, and
// `URLSession` for talking to shed-server over HTTP/SSE.
//
// The core/UI split (ShedKit vs ShedDesktopUI/ShedDesktopApp) keeps all
// I/O and logic unit-testable without a running UI, and is the seam a
// future Linux port would cut along. `shedctl` is a second executable —
// the CLI driver for the same IPC socket the pytest harness speaks.

import PackageDescription

let package = Package(
    name: "ShedDesktop",
    platforms: [
        // MenuBarExtra needs macOS 13; .v14 buys the stable
        // MenuBarExtra window style + modern SwiftUI without scattering
        // @available annotations. The dev/target machine is macOS 26.
        .macOS(.v14),
    ],
    products: [
        .executable(name: "ShedDesktop", targets: ["ShedDesktopApp"]),
        .executable(name: "shedctl", targets: ["shedctl"]),
    ],
    dependencies: [
        // Sparkle 2 auto-update (M8). The release DMG is ad-hoc-signed (no
        // Apple Developer ID yet), so update authenticity rests on Sparkle's
        // EdDSA signature, not a Team ID. The embedded framework needs the
        // `@executable_path/../Frameworks` rpath (linkerSettings below) and the
        // `cs.disable-library-validation` entitlement (ShedDesktop.entitlements)
        // to load under the hardened runtime without a matching signing team.
        .package(url: "https://github.com/sparkle-project/Sparkle", from: "2.6.0"),
    ],
    targets: [
        // Rust protocol core (Phase 1). ShedCoreFFI is the static Rust library
        // (xcframework); ShedRustCore is the generated UniFFI Swift that links it
        // (named to avoid colliding with the generated `ShedCore` client type).
        // Both live under artifacts/ (gitignored), produced by
        // scripts/build-core.sh — run `make core` before a bare `swift build`.
        // (SwiftPM requires target paths inside the package root, so the Rust
        // core's generated artifacts land under desktop/artifacts/, not the
        // top-level crates/ workspace they're built from.)
        .binaryTarget(
            name: "ShedCoreFFI",
            path: "artifacts/ShedCoreFFI.xcframework"
        ),
        .target(
            name: "ShedRustCore",
            dependencies: ["ShedCoreFFI"],
            path: "artifacts/ShedCoreSwift",
            // The generated UniFFI bindings aren't Swift-6-strict-concurrency
            // clean; the FFI boundary is exercised under strict concurrency from
            // ShedKit + the canary test instead.
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
        // Core: no SwiftUI. Foundation + AppKit (the latter only for
        // NSBitmapImageRep in Screenshot and NSWindow types in UiBridge).
        .target(
            name: "ShedKit",
            dependencies: ["ShedRustCore"],
            path: "Sources/ShedKit"
        ),
        // SwiftUI views + the AppState view-model.
        .target(
            name: "ShedDesktopUI",
            dependencies: ["ShedKit"],
            path: "Sources/ShedDesktopUI"
        ),
        // @main app: MenuBarExtra + WindowGroup + the IPC handler impl.
        .executableTarget(
            name: "ShedDesktopApp",
            dependencies: [
                "ShedKit",
                "ShedDesktopUI",
                .product(name: "Sparkle", package: "Sparkle"),
            ],
            path: "Sources/ShedDesktopApp",
            linkerSettings: [
                // `-Xlinker` is required: a bare `-rpath` is rejected by the
                // Swift driver ("unknown argument"); only `-Xlinker` passes the
                // token straight to `ld`. Lets the binary resolve
                // `@rpath/Sparkle.framework` from Contents/Frameworks at launch.
                .unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"]),
            ]
        ),
        // CLI driver for the IPC socket (mirrors roost's roostctl).
        .executableTarget(
            name: "shedctl",
            dependencies: ["ShedKit"],
            path: "Sources/shedctl"
        ),
        .testTarget(
            name: "ShedKitTests",
            dependencies: ["ShedKit", "ShedRustCore"],
            path: "Tests/ShedKitTests"
        ),
    ]
)
