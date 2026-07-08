// CreateShedSheet.swift — the create-shed flow with live SSE progress (M1),
// presented as a centered modal card (see DashboardView's modalOverlay).
//
// Free-text `owner/repo` for v1 (a gh-backed picker is a later fast-follow).

import ShedKit
import SwiftUI

public struct CreateShedSheet: View {
    @ObservedObject var state: AppState

    @State private var host: String = ""
    @State private var name: String = ""
    @State private var repo: String = ""
    @State private var backend: String = "auto"
    @State private var image: String = ""  // "" → the server's default_image
    @State private var cpus: Int = 2
    @State private var memoryGB: Int = 4
    @State private var provision = true
    @State private var submitted = false

    public init(state: AppState) {
        self.state = state
    }

    public var body: some View {
        VStack(spacing: 0) {
            SheetHeader(icon: "shippingbox", title: "New shed",
                        subtitle: "Spin up a dev environment on a host.", onClose: close)
            Divider()
            if submitted {
                progress
            } else {
                ScrollView {
                    formBody.padding(.horizontal, 20).padding(.vertical, 18)
                }
                .frame(maxHeight: 520)
                Divider()
                footer
            }
        }
        .modalCard()
        .onAppear {
            if host.isEmpty { host = state.hosts.first?.name ?? "" }
            state.onImagesRefresh?()
        }
        // An alias picked for one host may not exist on another; fall back to
        // the always-valid server default when the host changes.
        .onChange(of: host) { _, _ in image = "" }
    }

    private var footer: some View {
        HStack {
            SheetCancelButton(action: close)
            Spacer()
            SheetPrimaryButton(title: "Create shed", icon: "plus", disabled: !canCreate, action: submit)
        }
        .padding(16)
    }

    private var formBody: some View {
        VStack(alignment: .leading, spacing: 16) {
            SheetField("Name") { SheetTextField(placeholder: "", text: $name) }
            HStack(alignment: .top, spacing: 14) {
                SheetField("Host") {
                    SheetDropdown(current: host.isEmpty ? "—" : host) {
                        ForEach(state.hosts, id: \.name) { h in Button(h.name) { host = h.name } }
                    }
                }
                SheetField("Image") {
                    SheetDropdown(current: imageDisplay) {
                        Button(defaultOptionLabel) { image = "" }
                        ForEach(imageAliases) { img in
                            Button(aliasLabel(img)) { image = img.alias ?? "" }
                        }
                    }
                }
            }
            SheetField("Repo", hint: "owner/repo, optional") {
                SheetTextField(placeholder: "owner/repo", text: $repo)
            }
            SheetField("Backend") {
                SheetDropdown(current: backendDisplay) {
                    Button("Auto") { backend = "auto" }
                    Button("vz") { backend = "vz" }
                    Button("firecracker") { backend = "firecracker" }
                }
            }
            HStack(alignment: .top, spacing: 14) {
                SheetField("CPUs") { stepperControl(value: $cpus, range: 1...64) }
                SheetField("Memory (GB)") { stepperControl(value: $memoryGB, range: 1...512) }
            }
            provisionRow
        }
    }

    private func stepperControl(value: Binding<Int>, range: ClosedRange<Int>) -> some View {
        HStack(spacing: 0) {
            stepButton("minus") { value.wrappedValue = max(range.lowerBound, value.wrappedValue - 1) }
            Divider().frame(height: 18)
            Text("\(value.wrappedValue)").font(.system(size: 13, weight: .medium)).foregroundStyle(Theme.text)
                .frame(minWidth: 44)
            Divider().frame(height: 18)
            stepButton("plus") { value.wrappedValue = min(range.upperBound, value.wrappedValue + 1) }
        }
        .background(Theme.inset, in: RoundedRectangle(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).stroke(Theme.border, lineWidth: 0.5))
    }

    private func stepButton(_ symbol: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: symbol)
                .font(.system(size: 11, weight: .semibold))
                .foregroundStyle(Theme.textSecondary)
                .frame(width: 36, height: 34)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    private var provisionRow: some View {
        HStack(spacing: 10) {
            VStack(alignment: .leading, spacing: 2) {
                Text("Provision").font(.system(size: 13, weight: .medium)).foregroundStyle(Theme.text)
                Text("Run setup & install tools after the shed boots.")
                    .font(.system(size: 12)).foregroundStyle(Theme.textMuted)
            }
            Spacer()
            Toggle("", isOn: $provision).labelsHidden().toggleStyle(.switch).tint(Theme.accent)
        }
        .padding(12)
        .background(Theme.inset, in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).stroke(Theme.border, lineWidth: 0.5))
    }

    @ViewBuilder
    private var progress: some View {
        let create = state.activeCreate
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 8) {
                if create?.state == .progress { ProgressView().controlSize(.small) }
                Text(statusText(create)).font(.system(size: 13, weight: .medium)).foregroundStyle(Theme.text)
            }
            ScrollView {
                VStack(alignment: .leading, spacing: 3) {
                    ForEach(Array((create?.messages ?? []).enumerated()), id: \.offset) { _, msg in
                        Text(msg).font(.system(size: 12, design: .monospaced)).foregroundStyle(Theme.textSecondary)
                    }
                    if let err = create?.error {
                        Text(err).font(.system(size: 12, design: .monospaced)).foregroundStyle(Theme.danger)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .frame(maxHeight: 200)
            HStack {
                Spacer()
                SheetPrimaryButton(title: create?.state == .progress ? "Hide" : "Done",
                                   icon: "checkmark") {
                    state.activeCreate = nil
                    close()
                }
            }
        }
        .padding(16)
        .frame(minHeight: 200)
    }

    // MARK: derived labels

    private var hostImages: [ShedImage] { state.images(forHost: host) }

    private var imageAliases: [ShedImage] {
        hostImages.filter { ($0.alias?.isEmpty == false) }.sorted { ($0.alias ?? "") < ($1.alias ?? "") }
    }

    private var defaultOptionLabel: String {
        if let name = hostImages.first(where: { $0.isDefault })?.alias, !name.isEmpty {
            return "Server default (\(name))"
        }
        return "Server default"
    }

    private func aliasLabel(_ img: ShedImage) -> String {
        var s = img.alias ?? img.name
        if img.isDefault { s += " · default" }
        if !img.cached { s += " · not pulled" }
        return s
    }

    private var imageDisplay: String {
        if image.isEmpty { return defaultOptionLabel }
        return imageAliases.first(where: { ($0.alias ?? "") == image }).map(aliasLabel) ?? image
    }

    private var backendDisplay: String {
        switch backend {
        case "vz": return "vz"
        case "firecracker": return "firecracker"
        default: return "Auto"
        }
    }

    private var canCreate: Bool {
        !name.trimmingCharacters(in: .whitespaces).isEmpty && !host.isEmpty
    }

    private func statusText(_ create: CreateProgress?) -> String {
        switch create?.state {
        case .complete: return "Created \(create?.shed?.name ?? name)"
        case .error: return "Create failed"
        default: return "Creating \(name)…"
        }
    }

    private func close() { state.showCreateSheet = false }

    private func submit() {
        guard canCreate else { return }
        let request = CreateShedRequest(
            name: name.trimmingCharacters(in: .whitespaces),
            repo: repo.isEmpty ? nil : repo,
            image: image.isEmpty ? nil : image,
            backend: backend == "auto" ? nil : backend,
            cpus: cpus,
            memoryMB: memoryGB * 1024,
            noProvision: provision ? nil : true)
        submitted = true
        state.onCreate?(host, request)
    }
}
