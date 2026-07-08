// UnixSocket.swift — shared helper for the Unix-domain socket address
// byte-copy the IPC server and shedctl both need for bind/connect.

import Darwin
import Foundation

/// Build a filled `sockaddr_un` for `path`, or nil if the path is too long
/// for `sun_path`. Centralizes the unsafe pointer dance that would
/// otherwise be copy-pasted at every bind/connect site.
public func makeUnixSocketAddress(path: String) -> sockaddr_un? {
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let bytes = Array(path.utf8)
    guard bytes.count < MemoryLayout.size(ofValue: addr.sun_path) else { return nil }
    withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
        ptr.withMemoryRebound(to: CChar.self, capacity: bytes.count + 1) { c in
            for (i, b) in bytes.enumerated() { c[i] = CChar(b) }
            c[bytes.count] = 0
        }
    }
    return addr
}

/// Write `data` in full to `fd`, retrying EINTR + partial writes. Returns
/// false on unrecoverable error.
public func writeAll(fd: Int32, data: Data) -> Bool {
    var offset = 0
    let total = data.count
    return data.withUnsafeBytes { buf -> Bool in
        guard let base = buf.baseAddress else { return true }
        while offset < total {
            let written = Darwin.write(fd, base.advanced(by: offset), total - offset)
            if written < 0 { if errno == EINTR { continue }; return false }
            if written == 0 { return false }
            offset += written
        }
        return true
    }
}

/// Newline-delimited frame reader over a blocking fd. The read buffer is
/// reused across reads, and a frame cap guards against unbounded growth.
public struct LineFrameReader {
    private let fd: Int32
    private let maxBytes: Int
    private var pending = Data()
    private var scratch = [UInt8](repeating: 0, count: 65536)

    public enum ReadError: Error { case frameTooLarge }

    public init(fd: Int32, maxBytes: Int = ipcMaxFrameBytes) {
        self.fd = fd
        self.maxBytes = maxBytes
    }

    /// Next line (without the trailing newline); nil on EOF / read error.
    public mutating func readLine() throws -> Data? {
        while true {
            if let pos = pending.firstIndex(of: 0x0a) {
                let line = Data(pending[pending.startIndex..<pos])
                pending = Data(pending[pending.index(after: pos)...])
                if line.count > maxBytes { throw ReadError.frameTooLarge }
                return line
            }
            if pending.count > maxBytes { throw ReadError.frameTooLarge }
            let n = scratch.withUnsafeMutableBufferPointer { Darwin.read(fd, $0.baseAddress, $0.count) }
            if n == 0 { return nil }
            if n < 0 { if errno == EINTR { continue }; return nil }
            pending.append(contentsOf: scratch.prefix(n))
        }
    }
}
