// BundleProfile.swift
//
// Resolves the per-user paths shed-desktop uses: the IPC control socket,
// a single-instance lock, the state dir, and the log dir. Modeled on
// roost's BundleProfile but with one profile (no gtk sibling).
//
// `SHED_DESKTOP_STATE_DIR` redirects ONLY the state dir (for hermetic
// tests + side-by-side instances); the socket/lock/log stay on the
// default path so the harness and a dev session agree on where to dial.

import Foundation

public struct BundleProfile: Sendable {
    /// Directory component under ~/Library/{Caches,Application Support,Logs}/.
    public let appLabel: String
    /// CFBundleIdentifier.
    public let appID: String
    public let socketPath: String
    public let stateDir: String
    public let logDir: String

    public var logPath: String { (logDir as NSString).appendingPathComponent("shed-desktop.log") }

    /// The single-instance lock lives next to the socket so the flock and
    /// the IPC socket share a parent dir. Derived from `socketPath`, so a
    /// `SHED_DESKTOP_STATE_DIR` override never moves the lock.
    public var lockPath: String {
        let parent = (socketPath as NSString).deletingLastPathComponent
        return (parent as NSString).appendingPathComponent("shed-desktop.lock")
    }

    public init(
        appLabel: String, appID: String,
        socketPath: String, stateDir: String, logDir: String
    ) {
        self.appLabel = appLabel
        self.appID = appID
        self.socketPath = socketPath
        self.stateDir = stateDir
        self.logDir = logDir
    }

    /// The macOS profile shed-desktop ships.
    public static func mac(
        environment env: [String: String] = ProcessInfo.processInfo.environment
    ) -> BundleProfile {
        let appLabel = "ShedDesktop"
        let appID = "ai.stridelabs.ShedDesktop"

        let home: String? = {
            guard let h = env["HOME"], !h.isEmpty, h.hasPrefix("/") else { return nil }
            return h
        }()

        let socket: String
        let stateDir: String
        let logDir: String
        if let home {
            socket = "\(home)/Library/Caches/\(appLabel)/shed-desktop.sock"
            stateDir = "\(home)/Library/Application Support/\(appLabel)"
            logDir = "\(home)/Library/Logs/\(appLabel)"
        } else {
            socket = "/tmp/\(appLabel)/shed-desktop.sock"
            stateDir = "/tmp/\(appLabel)"
            logDir = "/tmp/\(appLabel)"
        }

        return BundleProfile(
            appLabel: appLabel,
            appID: appID,
            socketPath: socket,
            stateDir: applyStateDirOverride(stateDir, env["SHED_DESKTOP_STATE_DIR"]),
            logDir: logDir
        )
    }

    /// Apply a `SHED_DESKTOP_STATE_DIR` override. Strict (absolute +
    /// non-empty): a relative state dir resolves against the process CWD,
    /// which is nondeterministic. A set-but-relative value is ignored with
    /// a warning; empty/unset falls back silently.
    private static func applyStateDirOverride(_ defaultDir: String, _ raw: String?) -> String {
        guard let raw, !raw.isEmpty else { return defaultDir }
        if raw.hasPrefix("/") { return raw }
        FileHandle.standardError.write(Data(
            "SHED_DESKTOP_STATE_DIR ignored: not an absolute path; using default\n".utf8))
        return defaultDir
    }
}
