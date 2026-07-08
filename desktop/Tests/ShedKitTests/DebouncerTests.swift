import Foundation
import XCTest

@testable import ShedKit

final class DebouncerTests: XCTestCase {
    private final class Counter: @unchecked Sendable {
        private let lock = NSLock()
        private var n = 0
        func increment() {
            lock.lock()
            n += 1
            lock.unlock()
        }
        var value: Int {
            lock.lock()
            defer { lock.unlock() }
            return n
        }
    }

    func testCoalescesRapidCallsIntoOne() {
        let debouncer = Debouncer(interval: 0.05)
        let counter = Counter()
        for _ in 0..<5 { debouncer.schedule { counter.increment() } }

        let done = expectation(description: "settled")
        DispatchQueue.global().asyncAfter(deadline: .now() + 0.3) { done.fulfill() }
        wait(for: [done], timeout: 2.0)

        XCTAssertEqual(counter.value, 1, "a burst of schedules should fire exactly once")
    }

    func testFiresAgainAfterSettling() {
        let debouncer = Debouncer(interval: 0.05)
        let counter = Counter()

        let first = expectation(description: "first")
        debouncer.schedule {
            counter.increment()
            first.fulfill()
        }
        wait(for: [first], timeout: 2.0)

        let second = expectation(description: "second")
        debouncer.schedule {
            counter.increment()
            second.fulfill()
        }
        wait(for: [second], timeout: 2.0)

        XCTAssertEqual(counter.value, 2, "two separated schedules should each fire")
    }
}
