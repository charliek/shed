// AuthModeRegistry.swift
//
// The app's in-memory record of which credential shape each configured server
// issues (plan 002 §7 P1) — the Swift half of `shed-app`'s `auth_modes.rs`,
// with the same three writers and the same precedence rules.
//
// # Why it is app-lifetime, and not per-client
//
// `AppModel.reconnect()` rebuilds every `ShedServerClient` from
// `~/.shed/config.yaml` — on demand, and automatically whenever the config
// watcher fires. If the learned mode lived only inside the client's observer,
// that rebuild would discard what the session had already PROVEN (the server
// answers with certificates) and fall back to what config still claims (a
// token, because the CLI has not rewritten its cache). The very next mint would
// then skip the CSR and send a `token.get` to an mtls-only server — the §7 P5
// bug the capability tri-state exists to prevent. So the learned state outlives
// the clients, and each rebuild SEEDS it from config without clobbering it.
//
// # Why in memory, and nowhere else
//
// `~/.shed/config.yaml` is CLI-owned. This app's parser is read-only and lossy,
// so writing a learned mode back would drop every key it does not model, race
// the CLI, and wake the config watcher into rebuilding clients mid-mint. The
// cost of not persisting is one silent re-bootstrap on a cold launch against a
// server that flipped — the trade plan 002 pins for BOTH desktop clients.

import Foundation

/// What is known about one server's credential shape.
public struct AuthModeState: Equatable, Sendable {
    public let mode: ShedAuthMode
    /// `true` once a mint has actually produced this shape in THIS session;
    /// `false` while the value is still just the config entry's cached claim.
    public let learned: Bool

    public init(mode: ShedAuthMode, learned: Bool) {
        self.mode = mode
        self.learned = learned
    }
}

/// Per-server credential-mode state, shared by every client the app builds.
///
/// `@unchecked Sendable`: all state is behind an `NSLock`, and the writers run
/// on three different threads (the main actor's config load, the minter's
/// mapping, the Rust core's dispatcher).
public final class AuthModeRegistry: @unchecked Sendable {
    private struct Entry {
        var state: AuthModeState
        /// The ordinal of the last SYNCHRONOUS (`record`) write, or `0` when
        /// only config / the observer has ever spoken for this server.
        var syncSeq: UInt64
    }

    private let lock = NSLock()
    private var entries: [String: Entry] = [:]
    /// Hands out the monotonic ordinals stamped on synchronous writes. Shared
    /// across servers (one clock, not one per name) so the ordering is total.
    private var nextSeq: UInt64 = 0

    public init() {}

    /// Seed one server from its config entry. Only fills what nothing has been
    /// LEARNED for: a config reload must not walk back what this session proved
    /// (the CLI writes `auth_mode` at its own bootstrap, so its copy can be
    /// older than ours).
    public func seed(server: String, mode: ShedAuthMode) {
        lock.lock(); defer { lock.unlock() }
        guard let existing = entries[server] else {
            entries[server] = Entry(state: AuthModeState(mode: mode, learned: false), syncSeq: 0)
            return
        }
        guard !existing.state.learned else { return }
        entries[server] = Entry(
            state: AuthModeState(mode: mode, learned: false), syncSeq: existing.syncSeq)
    }

    /// Record a mode the minter observed SYNCHRONOUSLY, as it maps a
    /// `credential.response`. Always wins — over a config seed, over an earlier
    /// synchronous write, and over any observer callback that disagrees with it.
    ///
    /// Returns the ordinal stamped on the write, so a test can reason about the
    /// ordering.
    @discardableResult
    public func record(server: String, mode: ShedAuthMode) -> UInt64 {
        lock.lock(); defer { lock.unlock() }
        nextSeq += 1
        entries[server] = Entry(
            state: AuthModeState(mode: mode, learned: true), syncSeq: nextSeq)
        return nextSeq
    }

    /// Record a mode reported by the Rust core's `CredentialObserver` — a write
    /// that is strictly LOWER priority than `record`.
    ///
    /// The observer fires on the core's dispatcher, so mint N's callback can
    /// land AFTER mint N+1's synchronous write; if mint N+1 is the one that
    /// learned the server flipped, a late N callback saying `token` would walk
    /// it back. Every credential the observer can report was `record`ed
    /// synchronously first, so a DISAGREEMENT means a newer mint already
    /// superseded this event — drop it. Where no synchronous write exists at all
    /// the observer is the only writer and always applies.
    ///
    /// Returns whether it applied.
    @discardableResult
    public func recordObserved(server: String, mode: ShedAuthMode) -> Bool {
        lock.lock(); defer { lock.unlock() }
        if let existing = entries[server] {
            if existing.syncSeq != 0, existing.state.mode != mode {
                return false  // stale: mint N's callback, after mint N+1's write
            }
            entries[server] = Entry(
                state: AuthModeState(mode: mode, learned: true), syncSeq: existing.syncSeq)
            return true
        }
        entries[server] = Entry(state: AuthModeState(mode: mode, learned: true), syncSeq: 0)
        return true
    }

    /// The state known for `server`, or nil for a name nothing has been recorded
    /// or configured for.
    public func state(for server: String) -> AuthModeState? {
        lock.lock(); defer { lock.unlock() }
        return entries[server]?.state
    }

    /// The mode LEARNED for `server` this session, or nil if only config has
    /// spoken for it.
    public func learnedMode(for server: String) -> ShedAuthMode? {
        guard let s = state(for: server), s.learned else { return nil }
        return s.mode
    }

    /// Does `server` issue certificates, as far as config / the minter / the
    /// core's observer know?
    public func expectsMtls(_ server: String) -> Bool {
        state(for: server)?.mode == .mtls
    }

    /// Every known server + its state, sorted by name (a stable order for
    /// diagnostics and tests).
    public func snapshot() -> [(String, AuthModeState)] {
        lock.lock()
        let pairs = entries.map { ($0.key, $0.value.state) }
        lock.unlock()
        return pairs.sorted { $0.0 < $1.0 }
    }

    /// The ordinal of the last synchronous write for `server` (`0` = none).
    func syncSeq(for server: String) -> UInt64 {
        lock.lock(); defer { lock.unlock() }
        return entries[server]?.syncSeq ?? 0
    }
}
