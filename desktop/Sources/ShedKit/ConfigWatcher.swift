// ConfigWatcher — auto-reload ~/.shed/config.yaml when it changes, so a server
// flipping open→secure (or any endpoint edit) is picked up without an app
// relaunch. This is the host-agent's `discovery.watch` for the desktop.
//
// It watches the *directory* (not the file) via FSEvents, which survives atomic
// replaces (editors / `shed server add` rewrite the file). FSEvents is
// environment-bound, so the watcher wiring is build/manual-verified; the
// Debouncer it drives — which coalesces a burst of edits into one reconnect — is
// unit-tested.

import Foundation

/// Coalesces rapid `schedule` calls into a single trailing fire after `interval`.
public final class Debouncer: @unchecked Sendable {
    private let interval: TimeInterval
    private let queue: DispatchQueue
    private let lock = NSLock()
    private var pending: DispatchWorkItem?

    public init(
        interval: TimeInterval,
        queue: DispatchQueue = DispatchQueue(label: "ai.stridelabs.ShedDesktop.debounce")
    ) {
        self.interval = interval
        self.queue = queue
    }

    public func schedule(_ action: @escaping @Sendable () -> Void) {
        lock.lock()
        pending?.cancel()
        let item = DispatchWorkItem(block: action)
        pending = item
        lock.unlock()
        queue.asyncAfter(deadline: .now() + interval, execute: item)
    }
}

#if canImport(CoreServices)
import CoreServices

/// Watches a directory tree via FSEvents and invokes `onChange` (debounced) when
/// anything under it changes.
public final class ConfigWatcher: @unchecked Sendable {
    private let lock = NSLock()
    private var stream: FSEventStreamRef?
    private let debouncer: Debouncer
    private let onChange: @Sendable () -> Void
    private let callbackQueue = DispatchQueue(label: "ai.stridelabs.ShedDesktop.configwatch")

    public init(directory: String, debounce: TimeInterval = 0.3, onChange: @escaping @Sendable () -> Void) {
        self.onChange = onChange
        self.debouncer = Debouncer(interval: debounce)

        var context = FSEventStreamContext(
            version: 0, info: Unmanaged.passUnretained(self).toOpaque(),
            retain: nil, release: nil, copyDescription: nil)
        // A bare C function pointer (no captures): recover `self` from `info`.
        let callback: FSEventStreamCallback = { _, info, _, _, _, _ in
            guard let info else { return }
            let watcher = Unmanaged<ConfigWatcher>.fromOpaque(info).takeUnretainedValue()
            watcher.debouncer.schedule(watcher.onChange)
        }
        stream = FSEventStreamCreate(
            kCFAllocatorDefault, callback, &context, [directory] as CFArray,
            FSEventStreamEventId(kFSEventStreamEventIdSinceNow), 0.1,
            FSEventStreamCreateFlags(kFSEventStreamCreateFlagNoDefer | kFSEventStreamCreateFlagFileEvents))
        if let stream {
            FSEventStreamSetDispatchQueue(stream, callbackQueue)
            FSEventStreamStart(stream)
        }
    }

    public func stop() {
        lock.lock()
        defer { lock.unlock() }
        // Take + nil the stream under the lock before releasing, so a concurrent
        // stop()/deinit can't double-release it.
        guard let stream else { return }
        self.stream = nil
        FSEventStreamStop(stream)
        FSEventStreamInvalidate(stream)
        FSEventStreamRelease(stream)
    }

    deinit { stop() }
}
#endif
