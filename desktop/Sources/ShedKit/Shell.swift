// Shell.swift — shell argument quoting, shared by the terminal launcher
// and the RC command builders.

import Foundation

/// Single-quote a shell argument, escaping embedded single quotes. A safe
/// bareword (letters, digits, and `-_./@:`) is returned unquoted.
public func shellQuote(_ s: String) -> String {
    if s.isEmpty { return "''" }
    if s.allSatisfy({ $0.isLetter || $0.isNumber || "-_./@:".contains($0) }) { return s }
    return "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
}
