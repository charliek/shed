// ProcessRunner.swift — run an external command (ssh/tmux) and capture
// output. Used for the real RC path; the test harness drives an in-memory
// session table instead, so this never runs under CI.

import Foundation

public enum ProcessRunner {
    public struct Result: Sendable {
        public let stdout: String
        public let stderr: String
        public let exitCode: Int32
        public var ok: Bool { exitCode == 0 }
    }

    public enum RunError: Error, CustomStringConvertible {
        case launch(String)
        public var description: String {
            switch self {
            case .launch(let m): return "failed to launch: \(m)"
            }
        }
    }

    /// Single-writer flag so the watchdog can mark a timeout the main flow reads.
    private final class TimeoutFlag: @unchecked Sendable { var fired = false }

    /// Run `argv` (resolved on PATH via /usr/bin/env), optionally feeding
    /// `stdin`, and return captured output. Pipes are drained concurrently
    /// so a large stdout can't deadlock against an unread stderr. `timeout`
    /// (when set) terminates a process that overruns and returns exit code 124
    /// — `ConnectTimeout` only bounds SSH setup, not a hung remote command.
    public static func run(_ argv: [String], stdin: String? = nil, timeout: Duration? = nil) async throws -> Result {
        try await Task.detached(priority: .userInitiated) {
            let proc = Process()
            proc.executableURL = URL(fileURLWithPath: "/usr/bin/env")
            proc.arguments = argv
            let outPipe = Pipe()
            let errPipe = Pipe()
            let inPipe = Pipe()
            proc.standardOutput = outPipe
            proc.standardError = errPipe
            proc.standardInput = inPipe
            do {
                try proc.run()
            } catch {
                throw RunError.launch("\(error)")
            }
            if let stdin {
                inPipe.fileHandleForWriting.write(Data(stdin.utf8))
            }
            try? inPipe.fileHandleForWriting.close()
            // Watchdog: terminate (and flag) a process that overruns the timeout.
            let timedOut = TimeoutFlag()
            let watchdog: Task<Void, Never>? = timeout.map { t in
                Task {
                    try? await Task.sleep(for: t)
                    if proc.isRunning {
                        timedOut.fired = true
                        proc.terminate()
                    }
                }
            }
            // Read both pipes concurrently to avoid a full-buffer deadlock.
            async let out = Self.readAll(outPipe.fileHandleForReading)
            async let err = Self.readAll(errPipe.fileHandleForReading)
            let (outData, errData) = await (out, err)
            proc.waitUntilExit()
            watchdog?.cancel()
            if timedOut.fired {
                return Result(
                    stdout: String(decoding: outData, as: UTF8.self),
                    stderr: "operation timed out", exitCode: 124)
            }
            return Result(
                stdout: String(decoding: outData, as: UTF8.self),
                stderr: String(decoding: errData, as: UTF8.self),
                exitCode: proc.terminationStatus)
        }.value
    }

    private static func readAll(_ handle: FileHandle) async -> Data {
        await Task.detached { handle.readDataToEndOfFile() }.value
    }
}
