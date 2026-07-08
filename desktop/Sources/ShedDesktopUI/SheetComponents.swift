// SheetComponents.swift — shared building blocks for the modal sheets
// (New shed / New session): card chrome, header, labeled fields, inset text
// fields + dropdowns, and footer buttons. Keeps the two modals consistent.

import SwiftUI

/// Inset field chrome shared by modal text fields and dropdown labels.
struct SheetFieldChrome: ViewModifier {
    func body(content: Content) -> some View {
        content
            .padding(.horizontal, 11).padding(.vertical, 8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Theme.inset, in: RoundedRectangle(cornerRadius: 8))
            .overlay(RoundedRectangle(cornerRadius: 8).stroke(Theme.border, lineWidth: 0.5))
    }
}

extension View {
    func sheetFieldChrome() -> some View { modifier(SheetFieldChrome()) }

    /// Wrap modal content in the card chrome (surface, radius, border, shadow).
    func modalCard(width: CGFloat = 500) -> some View {
        self
            .frame(width: width)
            .background(Theme.surface, in: RoundedRectangle(cornerRadius: 16))
            .overlay(RoundedRectangle(cornerRadius: 16).stroke(Theme.border, lineWidth: 0.5))
            .shadow(color: Theme.shadowColor(0.18, 0.5), radius: 24, y: 10)
            .contentShape(Rectangle())
    }

    /// Present `content` as a centered modal over a dimmed, tap-to-dismiss scrim.
    @ViewBuilder
    func modalOverlay<C: View>(isPresented: Bool, onDismiss: @escaping () -> Void,
                               @ViewBuilder content: () -> C) -> some View {
        overlay {
            if isPresented {
                ZStack {
                    Color.black.opacity(0.35).ignoresSafeArea().onTapGesture(perform: onDismiss)
                    content().padding(.vertical, 24)
                }
            }
        }
    }
}

/// Modal header: accent icon + title + subtitle + close button.
struct SheetHeader: View {
    let icon: String
    let title: String
    let subtitle: String
    let onClose: () -> Void
    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: icon)
                .font(.system(size: 17, weight: .medium))
                .foregroundStyle(Theme.accent)
                .frame(width: 40, height: 40)
                .background(Theme.accentSubtle, in: RoundedRectangle(cornerRadius: 10))
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.system(size: 18, weight: .semibold)).foregroundStyle(Theme.text)
                Text(subtitle).font(.system(size: 12)).foregroundStyle(Theme.textMuted)
            }
            Spacer()
            Button(action: onClose) {
                Image(systemName: "xmark")
                    .font(.system(size: 12, weight: .semibold))
                    .foregroundStyle(Theme.textMuted)
                    .frame(width: 24, height: 24)
            }
            .buttonStyle(.plain)
            .keyboardShortcut(.cancelAction)
        }
        .padding(16)
    }
}

/// A labeled field group (label + optional muted hint, then the control).
struct SheetField<Content: View>: View {
    let label: String
    var hint: String?
    var help: String?
    @ViewBuilder let content: () -> Content
    init(_ label: String, hint: String? = nil, help: String? = nil, @ViewBuilder content: @escaping () -> Content) {
        self.label = label
        self.hint = hint
        self.help = help
        self.content = content
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Text(label).font(.system(size: 13, weight: .medium)).foregroundStyle(Theme.text)
                if let hint { Text(hint).font(.system(size: 12)).foregroundStyle(Theme.textMuted) }
            }
            content()
            if let help { Text(help).font(.system(size: 12)).foregroundStyle(Theme.textMuted) }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// A modal text field styled as an inset field.
struct SheetTextField: View {
    let placeholder: String
    @Binding var text: String
    var body: some View {
        TextField(placeholder, text: $text)
            .textFieldStyle(.plain).font(.system(size: 13)).foregroundStyle(Theme.text)
            .sheetFieldChrome()
    }
}

/// A dropdown styled as an inset field (selected text + chevron).
struct SheetDropdown<Items: View>: View {
    let current: String
    @ViewBuilder let items: () -> Items
    var body: some View {
        Menu {
            items()
        } label: {
            HStack {
                Text(current).font(.system(size: 13)).foregroundStyle(Theme.text).lineLimit(1)
                Spacer()
                Image(systemName: "chevron.up.chevron.down")
                    .font(.system(size: 10, weight: .medium)).foregroundStyle(Theme.textMuted)
            }
            .sheetFieldChrome()
        }
        .menuStyle(.button).buttonStyle(.plain).menuIndicator(.hidden)
    }
}

/// Secondary (Cancel) modal button.
struct SheetCancelButton: View {
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            Text("Cancel")
                .font(.system(size: 13, weight: .medium))
                .foregroundStyle(Theme.textSecondary)
                .padding(.horizontal, 16).padding(.vertical, 8)
                .overlay(RoundedRectangle(cornerRadius: 8).stroke(Theme.border, lineWidth: 0.5))
        }
        .buttonStyle(.plain)
    }
}

/// Primary modal CTA (tinted background + icon). Defaults to the accent color;
/// pass `tint`/`fg` for a different intent (e.g. the green Launch action).
struct SheetPrimaryButton: View {
    let title: String
    let icon: String
    var tint: Color = Theme.accent
    var fg: Color = Theme.accentFg
    var disabled: Bool = false
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            Label(title, systemImage: icon)
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(fg)
                .padding(.horizontal, 16).padding(.vertical, 9)
                .background(tint, in: RoundedRectangle(cornerRadius: 8))
                .opacity(disabled ? 0.5 : 1)
        }
        .buttonStyle(.plain)
        .keyboardShortcut(.defaultAction)
        .disabled(disabled)
    }
}
