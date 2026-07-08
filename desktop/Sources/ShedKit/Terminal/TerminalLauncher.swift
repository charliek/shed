// TerminalLauncher.swift
//
// Builds the SSH command that drops the user into a shed (and optionally
// attaches a tmux session), and launches it in the user's terminal. The
// command building is pure so it's unit-testable and so `terminal.preview`
// can show exactly what would run without spawning anything.
//
// A shed is reached as `<shed>@<host> -p <sshPort>` — the shed name is the
// SSH username (shed-server's SSH daemon routes by it).
//
// How the terminal is opened is chosen by a `TerminalPreset`. Terminal.app
// and Custom resolve inline (AppleScript / `sh -c <template>`); the other
// presets defer to a small bundled opener script (one per terminal) that
// knows how to launch that app and open a new tab running the command. The
// resolution is a pure function (`resolveLaunch`) returning a Sendable
// `LaunchInvocation`, so it crosses the IPC actor boundary and is testable.

import Foundation

public struct TerminalCommand: Codable, Sendable, Equatable {
    /// The ssh command as an argv array.
    public let argv: [String]
    /// The ssh command as a shell-quoted single line (for display + for
    /// handing to a terminal app).
    public let command: String

    public init(argv: [String], command: String) {
        self.argv = argv
        self.command = command
    }
}

/// A resolved "what to actually exec" for a terminal launch — the pure
/// output of `resolveLaunch`, with no side effects.
public struct LaunchInvocation: Codable, Sendable, Equatable {
    public let executable: String
    public let arguments: [String]

    public init(executable: String, arguments: [String]) {
        self.executable = executable
        self.arguments = arguments
    }
}

/// Which terminal to open a shed in. `terminalApp` + `custom` are always
/// offered; the rest appear in Preferences only when the app is detected
/// installed (and `roost` also needs python3). See `AppModel`'s detection.
public enum TerminalPreset: String, Codable, Sendable, CaseIterable, Identifiable {
    case terminalApp = "terminal-app"
    case ghostty
    case iterm2
    case warp
    case roost
    case custom

    public var id: String { rawValue }

    public var label: String {
        switch self {
        case .terminalApp: return "Terminal.app"
        case .ghostty: return "Ghostty"
        case .iterm2: return "iTerm2"
        case .warp: return "Warp"
        case .roost: return "Roost"
        case .custom: return "Custom"
        }
    }

    /// One-line description shown in Preferences for non-custom presets.
    public var detail: String {
        switch self {
        case .terminalApp: return "Opens the ssh command in Terminal.app."
        case .ghostty: return "Opens a new Ghostty tab running the ssh command."
        case .iterm2: return "Opens a new iTerm2 tab running the ssh command."
        case .warp: return "Opens Warp in a new window running the ssh command (best-effort)."
        case .roost: return "Opens a new Roost tab in a project named after the shed."
        case .custom: return ""
        }
    }

    /// The macOS bundle id used to detect whether the terminal is installed.
    /// `nil` = always available, no detection needed.
    public var bundleID: String? {
        switch self {
        case .ghostty: return "com.mitchellh.ghostty"
        case .iterm2: return "com.googlecode.iterm2"
        case .warp: return "dev.warp.Warp-Stable"
        case .roost: return "ai.stridelabs.Roost"
        case .terminalApp, .custom: return nil
        }
    }

    /// Presets whose opener is a python script need a python3 interpreter.
    public var requiresPython: Bool { self == .roost }

    /// The bundled opener script (interpreter + filename under
    /// `Contents/Resources/bin`) for presets that defer to a script.
    /// `nil` for `terminalApp` / `custom`, which resolve inline.
    public var helper: (interpreter: String, script: String)? {
        switch self {
        case .ghostty: return ("/bin/bash", "shed-open-ghostty")
        case .iterm2: return ("/bin/bash", "shed-open-iterm2")
        case .warp: return ("/bin/bash", "shed-open-warp")
        case .roost: return ("/usr/bin/python3", "shed-open-roost.py")
        case .terminalApp, .custom: return nil
        }
    }

    /// Pick the active preset. An explicit stored rawValue wins; otherwise
    /// derive a sane default from any legacy free-form template (single-user
    /// app — a one-time derivation, not a migration framework): a non-empty
    /// template means the user had a custom command, so default to `.custom`.
    public static func derive(legacyTemplate: String, storedRaw: String?) -> TerminalPreset {
        if let raw = storedRaw, let p = TerminalPreset(rawValue: raw) {
            return p
        }
        return legacyTemplate.trimmingCharacters(in: .whitespaces).isEmpty ? .terminalApp : .custom
    }
}

public enum TerminalLauncher {
    /// Build the ssh command to reach `shed` on `host`. When `session` is
    /// given, attach that tmux session.
    public static func sshCommand(shed: String, host: String, sshPort: Int, session: String? = nil) -> TerminalCommand {
        var argv = ["ssh", "-t"] + ShedSSH.hostKeyOptions + ["\(shed)@\(host)", "-p", String(sshPort)]
        if let session, !session.isEmpty {
            argv += ["tmux", "attach", "-t", session]
        }
        return TerminalCommand(argv: argv, command: argv.map(shellQuote).joined(separator: " "))
    }

    /// Resolve (purely, no side effects) what to exec for a launch. Used by
    /// `launchInTerminal` and by `terminal.preview` for observability.
    ///
    /// - `terminalApp` → osascript driving Terminal.app.
    /// - `custom` → `sh -c <template>` with `{cmd}`/`{shed}` substituted;
    ///   an empty template falls back to Terminal.app.
    /// - script presets → `<interpreter> <scriptsDir>/<script> <shed> <cmd>`;
    ///   when `scriptsDir` is missing (e.g. `swift run`, no app bundle) they
    ///   fall back to Terminal.app so the user still gets a working terminal.
    public static func resolveLaunch(
        preset: TerminalPreset,
        cmd: TerminalCommand,
        shed: String,
        customTemplate: String?,
        scriptsDir: URL?
    ) -> LaunchInvocation {
        switch preset {
        case .terminalApp:
            return terminalAppInvocation(cmd)
        case .custom:
            let template = (customTemplate ?? "").trimmingCharacters(in: .whitespaces)
            guard !template.isEmpty else { return terminalAppInvocation(cmd) }
            let expanded = template
                .replacingOccurrences(of: "{cmd}", with: cmd.command)
                .replacingOccurrences(of: "{shed}", with: shed)
            return LaunchInvocation(executable: "/bin/sh", arguments: ["-c", expanded])
        default:
            guard let helper = preset.helper, let dir = scriptsDir else {
                return terminalAppInvocation(cmd)
            }
            let path = dir.appendingPathComponent(helper.script).path
            return LaunchInvocation(executable: helper.interpreter, arguments: [path, shed, cmd.command])
        }
    }

    /// Launch a command in the user's terminal per `preset`.
    public static func launchInTerminal(
        _ cmd: TerminalCommand,
        preset: TerminalPreset,
        shed: String,
        customTemplate: String?,
        scriptsDir: URL?
    ) throws {
        let inv = resolveLaunch(
            preset: preset, cmd: cmd, shed: shed,
            customTemplate: customTemplate, scriptsDir: scriptsDir)
        try run(inv.executable, inv.arguments)
    }

    /// Single-quote a shell argument (delegates to the shared `shellQuote`).
    public static func shellQuote(_ s: String) -> String { ShedKit.shellQuote(s) }

    private static func terminalAppInvocation(_ cmd: TerminalCommand) -> LaunchInvocation {
        let script = """
        tell application "Terminal"
            activate
            do script "\(appleScriptEscape(cmd.command))"
        end tell
        """
        return LaunchInvocation(executable: "/usr/bin/osascript", arguments: ["-e", script])
    }

    private static func appleScriptEscape(_ s: String) -> String {
        s.replacingOccurrences(of: "\\", with: "\\\\")
            .replacingOccurrences(of: "\"", with: "\\\"")
    }

    private static func run(_ launchPath: String, _ args: [String]) throws {
        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: launchPath)
        proc.arguments = args
        try proc.run()
    }
}
