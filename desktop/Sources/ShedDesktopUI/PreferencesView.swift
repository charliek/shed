// PreferencesView.swift — the preferences window content.
//
// The approval sections are per-provider and appear only for the credential
// namespaces the host agent delegates to shed-desktop (gate_namespaces).

import ShedKit
import SwiftUI

public struct PreferencesView: View {
    @ObservedObject var prefs: Preferences
    @ObservedObject var state: AppState

    public init(prefs: Preferences, state: AppState) {
        self.prefs = prefs
        self.state = state
    }

    private var anyGated: Bool {
        CredentialNamespace.all.contains { prefs.gatedNamespaces.contains($0) }
    }

    public var body: some View {
        Form {
            Section("General") {
                Toggle("Launch at login", isOn: $prefs.launchAtLogin)
                    .onChange(of: prefs.launchAtLogin) { _, v in prefs.onLaunchAtLogin?(v) }
            }

            Section("Terminal") {
                Picker("Open sheds in", selection: $prefs.terminalPreset) {
                    ForEach(prefs.availableTerminalPresets) { Text($0.label).tag($0) }
                }
                .onChange(of: prefs.terminalPreset) { _, v in prefs.onTerminalPreset?(v) }

                if prefs.terminalPreset == .custom {
                    TextField("Command template", text: $prefs.terminalTemplate, prompt: Text("ghostty -e {cmd}"))
                        .onChange(of: prefs.terminalTemplate) { _, v in prefs.onTerminalTemplate?(v) }
                    Text("`{cmd}` is replaced with the ssh command, `{shed}` with the shed name. Leave empty to use Terminal.app.")
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                } else {
                    Text(prefs.terminalPreset.detail)
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                }
            }

            if !anyGated {
                Section("Approvals") {
                    Text("No extensions are delegated to shed-desktop. Set an extension's `approval.policy` to `shed-desktop` in extensions.yaml to manage it here.")
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                }
            }

            if prefs.gatedNamespaces.contains(CredentialNamespace.ssh) {
                Section("SSH approvals") {
                    Picker("Approval policy", selection: $prefs.sshPolicy) {
                        ForEach(SSHApprovalPolicy.allCases) { Text($0.label).tag($0) }
                    }
                    .onChange(of: prefs.sshPolicy) { _, v in prefs.onSSHPolicy?(v) }
                    if prefs.sshPolicy.usesDuration {
                        TextField("Duration", text: $prefs.sshTTL, prompt: Text("2h"))
                            .onChange(of: prefs.sshTTL) { _, v in prefs.onSSHTTL?(v) }
                    }
                    if prefs.sshPolicy.prompts {
                        Picker("Method", selection: $prefs.sshMethod) {
                            ForEach(ApprovalMethod.allCases, id: \.self) { Text($0.label).tag($0) }
                        }
                        .onChange(of: prefs.sshMethod) { _, v in prefs.onSSHMethod?(v) }
                    }
                    Text("Always Allow / Always Deny decide every SSH sign with no prompt. The others prompt, then remember your approval per the policy. Changing the policy clears live grants. “Method” is the Touch ID prompt shown when approving.")
                        .font(.system(size: 11)).foregroundStyle(.secondary)
                }
            }

            if prefs.gatedNamespaces.contains(CredentialNamespace.aws) {
                providerSection("AWS credentials", ns: CredentialNamespace.aws, mode: $prefs.awsMode)
            }
            if prefs.gatedNamespaces.contains(CredentialNamespace.docker) {
                providerSection("Docker credentials", ns: CredentialNamespace.docker, mode: $prefs.dockerMode)
            }

            if !prefs.shedRules.isEmpty {
                Section("Per-shed overrides") {
                    ForEach(prefs.shedRules) { row in
                        HStack(spacing: 8) {
                            Text(row.shed)
                            if !row.server.isEmpty {
                                Text(row.server).foregroundStyle(.secondary).font(.system(size: 11))
                            }
                            Spacer()
                            Text(row.action == .deny ? "auto-deny" : "auto-approve")
                                .foregroundStyle(row.action == .deny ? .red : .secondary).font(.system(size: 11))
                            Button { prefs.onRemoveShedRule?(row.server, row.shed) } label: {
                                Image(systemName: "xmark.circle")
                            }
                            .buttonStyle(.borderless).foregroundStyle(.secondary)
                            .help("Remove this override")
                        }
                    }
                }
            }
        }
        .formStyle(.grouped)
        .frame(width: 460, height: 560)
    }

    private func providerSection(_ title: String, ns: String, mode: Binding<ApprovalDecision>) -> some View {
        Section(title) {
            Picker("Mode", selection: mode) {
                Text("Allow").tag(ApprovalDecision.approve)
                Text("Deny").tag(ApprovalDecision.deny)
            }
            .pickerStyle(.segmented)
            .onChange(of: mode.wrappedValue) { _, v in prefs.onProviderMode?(ns, v) }
            Text("Live — takes effect immediately. Credentials fail closed (deny) when shed-desktop isn't running.")
                .font(.system(size: 11)).foregroundStyle(.secondary)
        }
    }
}
