// shedctl — CLI driver for the shed-desktop IPC control socket.
//
// Speaks the same newline-delimited JSON protocol the pytest harness uses.
// For humans + scripts: `shedctl identify`, `shedctl ui state`,
// `shedctl screenshot --out /tmp/shot.png`, or the generic
// `shedctl call <op> key=value ...`.

import Darwin
import Foundation
import ShedKit

// MARK: - blocking UDS client

struct CTLError: Error, CustomStringConvertible {
    let message: String
    var description: String { message }
}

final class CTLClient {
    private let fd: Int32
    private var buf = Data()

    init(socketPath: String) throws {
        fd = socket(AF_UNIX, SOCK_STREAM, 0)
        if fd < 0 { throw CTLError(message: "socket() failed") }
        guard var addr = makeUnixSocketAddress(path: socketPath) else {
            Darwin.close(fd)
            throw CTLError(message: "socket path too long")
        }
        let rc = withUnsafePointer(to: &addr) { p in
            p.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                Darwin.connect(fd, sa, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        if rc != 0 {
            Darwin.close(fd)
            throw CTLError(message: "connect(\(socketPath)) failed — is ShedDesktop running?")
        }
    }

    deinit { Darwin.close(fd) }

    func call(op: String, params: [String: Any]) throws -> [String: Any] {
        let req: [String: Any] = ["id": "1", "op": op, "params": params]
        var line = try JSONSerialization.data(withJSONObject: req)
        line.append(0x0a)
        line.withUnsafeBytes { _ = Darwin.write(fd, $0.baseAddress, $0.count) }

        while !buf.contains(0x0a) {
            var chunk = [UInt8](repeating: 0, count: 65536)
            let n = chunk.withUnsafeMutableBytes { Darwin.read(fd, $0.baseAddress, $0.count) }
            if n <= 0 { throw CTLError(message: "socket closed mid-response") }
            buf.append(contentsOf: chunk.prefix(n))
        }
        let nl = buf.firstIndex(of: 0x0a)!
        let lineData = buf[..<nl]
        let resp = try JSONSerialization.jsonObject(with: Data(lineData)) as? [String: Any] ?? [:]
        if (resp["ok"] as? Bool) != true {
            let err = resp["error"] as? [String: Any] ?? [:]
            throw CTLError(message: "\(err["code"] ?? "error"): \(err["message"] ?? "")")
        }
        return resp["result"] as? [String: Any] ?? [:]
    }
}

// MARK: - CLI

func usage() -> Never {
    let text = """
    shedctl — drive the shed-desktop IPC socket

    USAGE:
      shedctl identify
      shedctl ui state
      shedctl ui navigate <sheds|approvals|agents|activity|egress|system>
      shedctl ui set-ssh-approval [--method M] [--scope S] [--ttl T]
      shedctl ui show-window
      shedctl ui hide-window
      shedctl ui show-create
      shedctl ui show-launch
      shedctl ui open-menu <true|false>
      shedctl ui window-state
      shedctl host list
      shedctl sheds list [--host NAME]
      shedctl sheds refresh
      shedctl images list
      shedctl screenshot [--surface window|menu] [--scale 1|2] --out FILE
      shedctl call <op> [key=value ...]   # generic; values parsed as JSON when possible
    """
    FileHandle.standardError.write(Data((text + "\n").utf8))
    exit(2)
}

func printJSON(_ obj: Any) {
    if let data = try? JSONSerialization.data(withJSONObject: obj, options: [.prettyPrinted, .sortedKeys]),
       let s = String(data: data, encoding: .utf8) {
        print(s)
    } else {
        print(obj)
    }
}

/// Parse `key=value` args; values become JSON when parseable (numbers,
/// bools, objects), else raw strings.
func parseKV(_ args: ArraySlice<String>) -> [String: Any] {
    var params: [String: Any] = [:]
    for arg in args {
        guard let eq = arg.firstIndex(of: "=") else { continue }
        let key = String(arg[arg.startIndex..<eq])
        let raw = String(arg[arg.index(after: eq)...])
        if let data = raw.data(using: .utf8),
           let json = try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed]) {
            params[key] = json
        } else {
            params[key] = raw
        }
    }
    return params
}

func flag(_ args: [String], _ name: String) -> String? {
    guard let i = args.firstIndex(of: name), i + 1 < args.count else { return nil }
    return args[i + 1]
}

let argv = Array(CommandLine.arguments.dropFirst())
guard !argv.isEmpty else { usage() }

let socketPath = ProcessInfo.processInfo.environment["SHED_DESKTOP_SOCKET"]
    ?? BundleProfile.mac().socketPath

do {
    let client = try CTLClient(socketPath: socketPath)

    switch (argv[0], argv.count >= 2 ? argv[1] : "") {
    case ("identify", _):
        printJSON(try client.call(op: "identify", params: [:]))
    case ("ui", "state"):
        printJSON(try client.call(op: "ui.state", params: [:]))
    case ("ui", "navigate"):
        guard argv.count >= 3 else { usage() }
        printJSON(try client.call(op: "ui.navigate", params: ["pane": argv[2]]))
    case ("ui", "set-ssh-approval"):
        var params: [String: Any] = [:]
        if let m = flag(argv, "--method") { params["method"] = m }
        if let s = flag(argv, "--scope") { params["scope"] = s }
        if let t = flag(argv, "--ttl") { params["ttl"] = t }
        printJSON(try client.call(op: "ui.set_ssh_approval", params: params))
    case ("ui", "show-window"):
        printJSON(try client.call(op: "ui.show_window", params: [:]))
    case ("ui", "hide-window"):
        printJSON(try client.call(op: "ui.hide_window", params: [:]))
    case ("ui", "show-create"):
        printJSON(try client.call(op: "ui.show_create", params: [:]))
    case ("ui", "show-launch"):
        printJSON(try client.call(op: "ui.show_launch", params: [:]))
    case ("ui", "open-menu"):
        let open = (argv.count >= 3 ? argv[2] : "true") == "true"
        printJSON(try client.call(op: "ui.open_menu", params: ["open": open]))
    case ("ui", "window-state"):
        printJSON(try client.call(op: "ui.window_state", params: [:]))
    case ("host", "list"):
        printJSON(try client.call(op: "host.list", params: [:]))
    case ("sheds", "list"):
        var params: [String: Any] = [:]
        if let h = flag(argv, "--host") { params["host"] = h }
        printJSON(try client.call(op: "sheds.list", params: params))
    case ("sheds", "refresh"):
        printJSON(try client.call(op: "sheds.refresh", params: [:]))
    case ("images", "list"):
        printJSON(try client.call(op: "images.list", params: [:]))
    case ("screenshot", _):
        guard let out = flag(argv, "--out") else {
            FileHandle.standardError.write(Data("screenshot requires --out FILE\n".utf8)); exit(2)
        }
        var params: [String: Any] = [:]
        if let s = flag(argv, "--scale"), let n = Int(s) { params["scale"] = n }
        if let surface = flag(argv, "--surface") { params["surface"] = surface }
        let result = try client.call(op: "app.screenshot", params: params)
        guard let b64 = result["png"] as? String, let png = Data(base64Encoded: b64) else {
            FileHandle.standardError.write(Data("no png in response\n".utf8)); exit(1)
        }
        try png.write(to: URL(fileURLWithPath: out))
        print("wrote \(png.count) bytes -> \(out) (\(result["width"] ?? "?")x\(result["height"] ?? "?"))")
    case ("call", _):
        guard argv.count >= 2 else { usage() }
        let params = parseKV(argv[2...])
        printJSON(try client.call(op: argv[1], params: params))
    default:
        usage()
    }
} catch {
    FileHandle.standardError.write(Data("error: \(error)\n".utf8))
    exit(1)
}
