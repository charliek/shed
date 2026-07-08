import AppKit
import XCTest
@testable import ShedKit

final class ColorHexTests: XCTestCase {
    private func srgb(_ c: NSColor, file: StaticString = #filePath, line: UInt = #line) -> NSColor {
        guard let s = c.usingColorSpace(.sRGB) else {
            XCTFail("color not convertible to sRGB", file: file, line: line)
            return c
        }
        return s
    }

    private func assertComponents(
        _ hex: UInt32, alpha: Double = 1.0,
        r: Double, g: Double, b: Double, a: Double,
        file: StaticString = #filePath, line: UInt = #line
    ) {
        let c = srgb(NSColor(hex: hex, alpha: alpha), file: file, line: line)
        XCTAssertEqual(c.redComponent, r, accuracy: 0.005, file: file, line: line)
        XCTAssertEqual(c.greenComponent, g, accuracy: 0.005, file: file, line: line)
        XCTAssertEqual(c.blueComponent, b, accuracy: 0.005, file: file, line: line)
        XCTAssertEqual(c.alphaComponent, a, accuracy: 0.005, file: file, line: line)
    }

    func testRGBDecoding() {
        // Cream canvas, near-white surface — the two headline palette values.
        assertComponents(0xF3F1E8, r: 0xF3 / 255.0, g: 0xF1 / 255.0, b: 0xE8 / 255.0, a: 1.0)
        assertComponents(0xFCFBF7, r: 0xFC / 255.0, g: 0xFB / 255.0, b: 0xF7 / 255.0, a: 1.0)
    }

    func testBlackAndWhiteEdges() {
        assertComponents(0x000000, r: 0, g: 0, b: 0, a: 1.0)
        assertComponents(0xFFFFFF, r: 1, g: 1, b: 1, a: 1.0)
    }

    func testAlphaArgumentApplies() {
        // Translucency rides on the explicit alpha argument, not packed into the hex.
        assertComponents(0x000000, alpha: 0.10, r: 0, g: 0, b: 0, a: 0.10)
        assertComponents(0xFFFFFF, alpha: 0.12, r: 1, g: 1, b: 1, a: 0.12)
    }

    func testMixedChannelsRoundTrip() {
        assertComponents(0x336699, r: 0x33 / 255.0, g: 0x66 / 255.0, b: 0x99 / 255.0, a: 1.0)
    }
}
