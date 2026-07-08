// ShedBackend.swift
//
// Process-wide singleton holding the bits the IPC handler needs that
// aren't UI: the test-mode flag, the registered UiBridge, and the
// hermetic-test wiring (mock base URL, seeded config path). The app calls
// `start(profile:)` once at launch; the SwiftUI AppModel registers itself
// as the UiBridge and binds the IPC server.
//
// Modeled on roost's RoostBackend, minus the in-process Workspace/PTY
// machinery (shed-desktop has no local state of its own — it observes
// shed-server over HTTP).

import AppKit
import Darwin
import Foundation

@MainActor
public final class ShedBackend {
    public static let shared = ShedBackend()

    public private(set) var profile: BundleProfile?
    public private(set) weak var ui: (any UiBridge)?

    /// `SHED_DESKTOP_TEST_MODE=1` at launch — read once so per-op dispatch
    /// is a cheap bool check and a test can't toggle the gate mid-session.
    public private(set) var testMode: Bool = false

    /// In test mode, all host clients are pointed at this single mock
    /// base URL (`SHED_DESKTOP_MOCK_BASE_URL`). Echoed by `identify` so the
    /// harness can fail fast if a run isn't actually hermetic.
    public private(set) var mockBaseURL: String?

    /// Route read/lifecycle/create ops through the Rust shed-core. Default **on**
    /// (M0); `SHED_DESKTOP_RUST_CORE=0` forces the legacy Swift `URLSession` path
    /// (a rollback escape hatch kept for ≥2 releases). Reported by `identify` as
    /// `core` so the parity run can confirm which backend is active.
    public private(set) var rustCore: Bool = true

    /// Path to the shed config the app should read. Defaults to
    /// ~/.shed/config.yaml; overridable via `SHED_DESKTOP_SHED_CONFIG` so
    /// tests seed a fixture without touching the real config.
    public private(set) var shedConfigPath: String = ""

    /// The shed-host-agent UDS path the approval client connects to.
    /// Overridable via `SHED_DESKTOP_HOST_AGENT_SOCKET` so the test fake
    /// can stand in for the real host agent.
    public private(set) var hostAgentSocketPath: String = ""

    private var started = false

    private init() {}

    public func registerUI(_ ui: any UiBridge) {
        self.ui = ui
    }

    nonisolated(unsafe) private static var sigpipeInstalled = false
    private static func ignoreSigpipe() {
        guard !sigpipeInstalled else { return }
        sigpipeInstalled = true
        signal(SIGPIPE, SIG_IGN)
    }

    /// Read env + stash paths. Idempotent. Does NOT bind the IPC server —
    /// the app constructs its handler and server after registering the UI.
    public func start(profile: BundleProfile) {
        Self.ignoreSigpipe()
        if started { return }
        started = true
        self.profile = profile

        let env = ProcessInfo.processInfo.environment
        self.testMode = env["SHED_DESKTOP_TEST_MODE"] == "1"
        self.mockBaseURL = env["SHED_DESKTOP_MOCK_BASE_URL"]
        self.rustCore = env["SHED_DESKTOP_RUST_CORE"] != "0"

        let home = (env["HOME"] ?? NSHomeDirectory()) as NSString
        self.shedConfigPath = env["SHED_DESKTOP_SHED_CONFIG"]
            ?? home.appendingPathComponent(".shed/config.yaml")
        self.hostAgentSocketPath = env["SHED_DESKTOP_HOST_AGENT_SOCKET"]
            ?? home.appendingPathComponent("Library/Application Support/shed/host-agent.sock")

        for dir in [profile.stateDir, (profile.socketPath as NSString).deletingLastPathComponent, profile.logDir] {
            try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        }
    }
}
