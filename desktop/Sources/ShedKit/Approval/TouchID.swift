// TouchID.swift — app-side biometric gate (M3, spec §9).
//
// On-device LocalAuthentication; no biometric data leaves the machine. The
// app uses this for the `prompt` + `touchid` policy path before sending an
// approve. Under the test harness it's bypassed (see ShedBackend.testMode
// at the call site) so CI never blocks on a biometric prompt.

import Foundation
import LocalAuthentication

public enum TouchID {
    /// Prompt for Touch ID (or the device password fallback). Returns true
    /// on success. Returns false (deny-safe) if no authentication is
    /// available or the user cancels.
    /// `biometricsOnly` selects Touch ID only (no Apple Watch / password
    /// fallback) — the "biometrics" method; otherwise the password fallback is
    /// allowed (works in clamshell mode / on Macs without a sensor).
    public static func authenticate(reason: String, biometricsOnly: Bool = false) async -> Bool {
        let policy: LAPolicy = biometricsOnly ? .deviceOwnerAuthenticationWithBiometrics : .deviceOwnerAuthentication
        let context = LAContext()
        var error: NSError?
        guard context.canEvaluatePolicy(policy, error: &error) else {
            return false
        }
        return await withCheckedContinuation { continuation in
            context.evaluatePolicy(policy, localizedReason: reason) { success, _ in
                continuation.resume(returning: success)
            }
        }
    }
}
