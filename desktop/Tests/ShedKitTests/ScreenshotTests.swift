import Foundation
import XCTest

@testable import ShedKit

final class ScreenshotTests: XCTestCase {
    @MainActor
    func testCaptureRejectsInvalidScale() {
        // The scale guard runs before the window checks, so a nil window still
        // surfaces the scale error for out-of-range values.
        for bad in [0, 3, -1] {
            XCTAssertThrowsError(try captureWindowPNG(nil, scale: bad)) { err in
                guard case ScreenshotError.invalidScale(let s) = err else {
                    return XCTFail("expected invalidScale for scale \(bad), got \(err)")
                }
                XCTAssertEqual(s, bad)
            }
        }
    }

    @MainActor
    func testCaptureAcceptsValidScaleThenChecksWindow() {
        // Valid scales pass the guard; with no window the next check fires,
        // proving 1/2 are not rejected by the scale guard.
        for good in [1, 2] {
            XCTAssertThrowsError(try captureWindowPNG(nil, scale: good)) { err in
                guard case ScreenshotError.noWindow = err else {
                    return XCTFail("expected noWindow for scale \(good), got \(err)")
                }
            }
        }
    }
}
