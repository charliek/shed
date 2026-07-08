// Theme.swift — the "Linen" palette (slate-blue accent) + shared view helpers.
//
// Every color is appearance-aware via an NSColor dynamic provider, which is the
// one technique that also resolves correctly under the screenshot path's
// `performAsCurrentDrawingAppearance`. Values are sRGB conversions of the OKLCH
// tokens in the design's shed-linen-theme.css (see tools/oklch for the script).

import AppKit
import ShedKit
import SwiftUI

public enum Theme {
    // Canvas + surfaces
    public static let bg = dynamic(lightHex: 0xF1EFE9, darkHex: 0x100F0C)
    public static let bgSidebar = dynamic(lightHex: 0xECEAE4, darkHex: 0x0B0A08)
    public static let surface = dynamic(lightHex: 0xFCFBF8, darkHex: 0x1A1915)
    public static let surfaceHover = dynamic(lightHex: 0xF5F4F0, darkHex: 0x23221E)
    public static let inset = dynamic(lightHex: 0xE2E0DA, darkHex: 0x2B2A26)
    public static let border = dynamic(lightHex: 0xDAD8D2, darkHex: 0x302F2B)
    public static let borderStrong = dynamic(lightHex: 0xC0BEB7, darkHex: 0x494844)

    // Text
    public static let text = dynamic(lightHex: 0x2A2820, darkHex: 0xEAE9E6)
    public static let textSecondary = dynamic(lightHex: 0x67655E, darkHex: 0xADACA9)
    public static let textMuted = dynamic(lightHex: 0x94928D, darkHex: 0x777673)

    // Accent (slate blue)
    public static let accent = dynamic(lightHex: 0x4F759A, darkHex: 0x81A7CC)
    public static let accentFg = dynamic(lightHex: 0xF6FDFF, darkHex: 0x070E15)
    public static let accentSubtle = dynamic(lightHex: 0xD7EAFC, darkHex: 0x1D3144)
    public static let accentBorder = dynamic(lightHex: 0xB8D1E9, darkHex: 0x375067)

    // Status / intent
    public static let ok = dynamic(lightHex: 0x359451, darkHex: 0x57BD72)
    public static let approve = dynamic(lightHex: 0x049544, darkHex: 0x38AC5C)
    public static let approveFg = dynamic(lightHex: 0xF3FFF5, darkHex: 0x020C04)
    public static let attention = dynamic(lightHex: 0xB86A0B, darkHex: 0xE7A64C)
    public static let danger = dynamic(lightHex: 0xCE2A29, darkHex: 0xEE5E54)
    public static let denyBg = dynamic(lightHex: 0xFFE2DD, darkHex: 0x4D1F1B)
    public static let routine = dynamic(lightHex: 0x687484, darkHex: 0x7A8798)

    // Semantic pills
    public static let tagVzBg = dynamic(lightHex: 0xCFE7FF, darkHex: 0x193356)
    public static let tagVzText = dynamic(lightHex: 0x2765B4, darkHex: 0x89BDFF)
    public static let tagFcBg = dynamic(lightHex: 0xFFDDBB, darkHex: 0x592900)
    public static let tagFcText = dynamic(lightHex: 0xA74F0C, darkHex: 0xF8AF72)
    public static let agentBg = dynamic(lightHex: 0xEFE0FF, darkHex: 0x3B2A4D)
    public static let agentText = dynamic(lightHex: 0x7641A7, darkHex: 0xCFABF9)

    public static let radius: CGFloat = 14
    public static let sidebarWidth: CGFloat = 200

    /// A SwiftUI color that resolves to `lightHex`/`darkHex` (with optional
    /// per-mode alpha) per appearance. `Color` is `Sendable`; the raw `NSColor`
    /// is never exposed.
    public static func dynamic(lightHex: UInt32, darkHex: UInt32,
                               lightAlpha: Double = 1, darkAlpha: Double = 1) -> Color {
        Color(nsColor: NSColor(name: nil) { appearance in
            let isDark = appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
            return NSColor(hex: isDark ? darkHex : lightHex, alpha: isDark ? darkAlpha : lightAlpha)
        })
    }

    /// Warm card shadow color (cool black in dark). Alpha differs per mode.
    public static func shadowColor(_ lightAlpha: Double, _ darkAlpha: Double) -> Color {
        dynamic(lightHex: 0x2D2820, darkHex: 0x000000, lightAlpha: lightAlpha, darkAlpha: darkAlpha)
    }

    public static func statusColor(_ status: ShedStatus) -> Color {
        switch status {
        case .running: return ok
        case .starting: return attention
        case .error: return danger
        case .stopped, .unknown: return textMuted
        }
    }

    public static func rcStateColor(_ state: RcState) -> Color {
        switch state {
        case .ready: return ok
        case .starting, .reconnecting: return attention
        case .needsTrust, .needsAuth: return danger
        case .dead: return routine
        }
    }

    public static func namespaceColor(_ namespace: String) -> Color {
        switch namespace {
        case "ssh-agent": return agentText
        case "aws-credentials": return attention
        case "docker-credentials": return routine
        default: return routine
        }
    }

    public static func namespaceSymbol(_ namespace: String) -> String {
        switch namespace {
        case "ssh-agent": return "key"
        case "aws-credentials": return "cloud"
        case "docker-credentials": return "shippingbox"
        default: return "lock"
        }
    }

    public static func auditResultColor(_ result: String) -> Color {
        switch result {
        case "ok": return ok
        case "denied", "error": return danger
        default: return textMuted
        }
    }
}

/// A small colored status dot.
public struct StatusDot: View {
    let color: Color
    public init(_ color: Color) { self.color = color }
    public var body: some View {
        Circle().fill(color).frame(width: 8, height: 8)
    }
}

/// Pill tone — maps to the semantic pill tokens.
public enum BadgeTone {
    case neutral, accent, vz, firecracker, agent, legacy

    /// Tone for a backend name ("vz" → blue, "firecracker" → amber, else neutral).
    public static func backend(_ name: String?) -> BadgeTone {
        switch name {
        case "vz": return .vz
        case "firecracker": return .firecracker
        default: return .neutral
        }
    }

    var bg: Color {
        switch self {
        case .neutral: return Theme.inset
        case .accent: return Theme.accentSubtle
        case .vz: return Theme.tagVzBg
        case .firecracker: return Theme.tagFcBg
        case .agent: return Theme.agentBg
        case .legacy: return Theme.inset
        }
    }
    var fg: Color {
        switch self {
        case .neutral: return Theme.textSecondary
        case .accent: return Theme.accent
        case .vz: return Theme.tagVzText
        case .firecracker: return Theme.tagFcText
        case .agent: return Theme.agentText
        case .legacy: return Theme.textMuted
        }
    }
    var stroke: Color? { self == .accent ? Theme.accentBorder : nil }
}

/// An icon button that carries intent: subtle tint + colored border + glyph.
/// Shared by the Sheds and Agents rows.
public struct IntentButton: View {
    let symbol: String
    let help: String
    let intent: Color
    let action: () -> Void
    public init(_ symbol: String, _ help: String, _ intent: Color, action: @escaping () -> Void) {
        self.symbol = symbol
        self.help = help
        self.intent = intent
        self.action = action
    }
    public var body: some View {
        Button(action: action) {
            Image(systemName: symbol)
                .font(.system(size: 12, weight: .medium))
                .foregroundStyle(intent)
                .frame(width: 26, height: 26)
                .background(RoundedRectangle(cornerRadius: 7).fill(intent.opacity(0.12)))
                .overlay(RoundedRectangle(cornerRadius: 7).stroke(intent.opacity(0.30), lineWidth: 0.5))
        }
        .buttonStyle(.plain)
        .help(help)
    }
}

/// A pill badge (backend / image / namespace), optionally with a leading glyph.
public struct Badge: View {
    let text: String
    let tone: BadgeTone
    let symbol: String?
    public init(_ text: String, tone: BadgeTone = .neutral, symbol: String? = nil) {
        self.text = text
        self.tone = tone
        self.symbol = symbol
    }
    public var body: some View {
        HStack(spacing: 3) {
            if let symbol {
                Image(systemName: symbol).font(.system(size: 9, weight: .semibold))
            }
            Text(text).font(.system(size: 10, weight: .medium))
        }
        .padding(.horizontal, 6)
        .padding(.vertical, 1)
        .foregroundStyle(tone.fg)
        .background(tone.bg, in: RoundedRectangle(cornerRadius: 5))
        .overlay {
            if let s = tone.stroke {
                RoundedRectangle(cornerRadius: 5).stroke(s, lineWidth: 0.5)
            }
        }
    }
}

/// Shared card recipe: surface fill + radius + hairline border + soft two-layer
/// shadow (stronger when `selected`). The shadows ride the fill shape (a sibling
/// behind the content) so the card's text/icons aren't themselves shadowed.
public struct CardSurface: ViewModifier {
    var selected: Bool = false
    public func body(content: Content) -> some View {
        content
            .background {
                RoundedRectangle(cornerRadius: Theme.radius)
                    .fill(Theme.surface)
                    .shadow(color: Theme.shadowColor(selected ? 0.07 : 0.05, selected ? 0.40 : 0.30),
                            radius: 1, y: 1)
                    .shadow(color: Theme.shadowColor(selected ? 0.07 : 0.05, selected ? 0.34 : 0.24),
                            radius: selected ? 6 : 3, y: selected ? 4 : 2)
            }
            .overlay(RoundedRectangle(cornerRadius: Theme.radius).stroke(Theme.border, lineWidth: 0.5))
    }
}

public extension View {
    func cardSurface(selected: Bool = false) -> some View { modifier(CardSurface(selected: selected)) }
}
