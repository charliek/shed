// CredentialModeObserver.swift
//
// The desktop's `ShedRustCore.CredentialObserver` (plan 002 §7 P1): it learns
// which credential shape the core adopted for a server and holds it IN MEMORY.
//
// The desktop persists NOTHING from these events. `~/.shed/config.yaml` is
// CLI-owned and this app's parser is read-only and lossy — writing back would
// drop every key the parser doesn't model, race the CLI, and wake the config
// watcher into rebuilding clients mid-mint. The cost of not persisting is one
// silent re-bootstrap on a cold launch against a flipped server, which is the
// trade the plan pins.
//
// It also holds NO reference to the core (the FFI's delivery contract asks
// observers to hold it weakly at most): the dispatcher retains the observer, so
// an observer that retained the core would keep it alive forever.

import Foundation
import ShedRustCore

/// `ShedAuthMode` itself lives with the config model it describes
/// (`Models/ShedConfig.swift`); this is the FFI half, kept here so the model
/// file doesn't have to import the Rust bindings.
extension ShedAuthMode {
    init(_ mode: ShedRustCore.AuthMode) {
        switch mode {
        case .token: self = .token
        case .mtls: self = .mtls
        }
    }
}

/// Records the mode the Rust core last adopted for one server, and reports
/// transitions to an optional sink (diagnostics / UI state).
///
/// `@unchecked Sendable`: state is a `let` plus an `NSLock`-guarded value, and
/// callbacks arrive on the core's dispatcher thread.
public final class CredentialModeObserver: ShedRustCore.CredentialObserver, @unchecked Sendable {
    /// Called on every `mode_changed`, on the core's dispatcher thread. MUST
    /// return promptly — a blocked handler parks that thread and every later
    /// event behind it (the FFI delivery contract).
    public typealias Sink = @Sendable (_ server: String, _ mode: ShedAuthMode) -> Void

    private let lock = NSLock()
    private var mode: ShedAuthMode?
    private let sink: Sink?

    public init(sink: Sink? = nil) {
        self.sink = sink
    }

    /// The mode learned this session, or nil if nothing has been adopted yet.
    public var learnedMode: ShedAuthMode? {
        lock.lock(); defer { lock.unlock() }
        return mode
    }

    public func credentialAdopted(event: ShedRustCore.CredentialAdopted) {
        // Adoption always precedes the derived mode_changed for the same mint,
        // and fires on same-shape rotations too — record it so a rotation that
        // did not change the shape still leaves the learned mode current. The
        // token value on this event is deliberately ignored: this client's
        // storage is not the token's home.
        lock.lock()
        mode = ShedAuthMode(event.mode)
        lock.unlock()
    }

    public func modeChanged(server: String, mode: ShedRustCore.AuthMode) {
        let learned = ShedAuthMode(mode)
        lock.lock()
        self.mode = learned
        lock.unlock()
        sink?(server, learned)
    }
}
