import Foundation

/// Shared SSH host-key verification options for reaching sheds.
///
/// Both the remote-control and terminal paths pin the server's SSH host key in
/// `~/.shed/known_hosts` (the file `shed server add` populates) with strict
/// checking, so a changed or unknown host key is rejected rather than silently
/// accepted. This matches the `shed` CLI's posture (StrictHostKeyChecking=yes
/// against the same known_hosts file).
public enum ShedSSH {
    /// Absolute path to the shed CLI's known_hosts file.
    public static var knownHostsPath: String {
        (NSHomeDirectory() as NSString).appendingPathComponent(".shed/known_hosts")
    }

    /// `-o` host-key options: strict checking against the shed known_hosts file.
    public static var hostKeyOptions: [String] {
        [
            "-o", "StrictHostKeyChecking=yes",
            "-o", "UserKnownHostsFile=\(knownHostsPath)",
        ]
    }
}
