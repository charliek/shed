//! SSH endpoint addressing and local-port forward primitives for roost reaches.
//!
//! Named [`SshEndpoint`] (not `SshTarget`) to avoid colliding with
//! `roost_ipc::ssh::SshTarget`.

use shed_core::identityfile::IdentityFileError;
use shed_lane::shell_quote::display_line;

/// ssh `ConnectTimeout` for non-interactive ops — bounds connection setup only,
/// never a hung remote command.
pub const CONNECT_TIMEOUT_SECS: u32 = 10;

/// An SSH endpoint reached by [`crate::reach::SshBridge`] / [`crate::reach::SshExec`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SshEndpoint {
    pub name: String,
    pub host: String,
    pub user: Option<String>,
    pub ssh_port: u16,
    pub known_hosts: Option<String>,
    pub identity_file: Option<String>,
}

impl Default for SshEndpoint {
    fn default() -> Self {
        Self {
            name: String::new(),
            host: String::new(),
            user: None,
            ssh_port: 22,
            known_hosts: None,
            identity_file: None,
        }
    }
}

/// A forward could not be established: the sentence, and the typed cause when
/// no ssh ran because the endpoint's identity file cannot be used.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ForwardError(pub String, Option<IdentityFileError>);

impl ForwardError {
    pub fn new(message: impl Into<String>) -> ForwardError {
        ForwardError(message.into(), None)
    }

    /// No ssh ran: the endpoint's identity file cannot be used.
    pub fn identity(refusal: IdentityFileError) -> ForwardError {
        ForwardError(refusal.to_string(), Some(refusal))
    }

    /// The identity-file refusal this error is, if it is one.
    pub fn identity_refusal(&self) -> Option<&IdentityFileError> {
        self.1.as_ref()
    }
}

impl std::fmt::Display for ForwardError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ForwardError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        self.1
            .as_ref()
            .map(|e| e as &(dyn std::error::Error + 'static))
    }
}

/// A live local port proxying to a remote loopback service.
///
/// **Contract:** [`port`](MachineForward::port) is fixed for the lifetime of
/// the value; [`ensure`](MachineForward::ensure) is idempotent and
/// re-establishes the underlying transport if it died, without changing the port.
#[async_trait::async_trait]
pub trait MachineForward: Send + Sync {
    /// The stable local port.
    fn port(&self) -> u16;

    /// Make the forward usable, rebuilding it if necessary.
    async fn ensure(&self) -> Result<(), ForwardError>;

    /// Cheap non-blocking liveness hint. Default `true` for externally managed forwards.
    fn looks_alive(&self) -> bool {
        true
    }

    /// Close this forward now, whoever else still holds it. Default no-op (`true`).
    fn close_now(&self) -> bool {
        true
    }
}

/// A forward someone else owns — used by `shed-mobile`'s Dart `dartssh2`
/// loopback bridge where Dart owns the listener and hands Rust the `u16` port.
pub struct FixedPort(pub u16);

#[async_trait::async_trait]
impl MachineForward for FixedPort {
    fn port(&self) -> u16 {
        self.0
    }

    async fn ensure(&self) -> Result<(), ForwardError> {
        Ok(())
    }
}

/// Check the endpoint's identity file now (value rule + regular file readable).
/// `Ok` for an endpoint with no identity file.
pub fn check_identity(entry: &SshEndpoint) -> Result<(), IdentityFileError> {
    match entry.identity_file.as_deref() {
        Some(path) => shed_core::identityfile::check_entry_file(&entry.name, path),
        None => Ok(()),
    }
}

/// The `[user@]host` destination.
pub fn user_at_host(entry: &SshEndpoint) -> String {
    match entry.user.as_deref().filter(|s| !s.is_empty()) {
        Some(user) => format!("{user}@{}", entry.host),
        None => entry.host.clone(),
    }
}

/// Non-interactive ssh to an endpoint, carrying `remote_argv` as one shell-quoted
/// command line.
///
/// `Err` only for an endpoint whose identity file cannot be used now ([`check_identity`]).
pub fn ssh_argv(
    entry: &SshEndpoint,
    remote_argv: &[String],
) -> Result<Vec<String>, IdentityFileError> {
    let mut argv = vec![
        "ssh".to_string(),
        "-o".to_string(),
        "BatchMode=yes".to_string(),
    ];
    argv.extend(host_key_opts(entry));
    argv.extend(identity_opts(entry)?);
    argv.push("-o".to_string());
    argv.push(format!("ConnectTimeout={CONNECT_TIMEOUT_SECS}"));
    argv.extend(port_opts(entry));
    argv.push(user_at_host(entry));
    argv.push("--".to_string());
    argv.push(display_line(remote_argv));
    Ok(argv)
}

fn host_key_opts(entry: &SshEndpoint) -> Vec<String> {
    match entry.known_hosts.as_deref().filter(|s| !s.is_empty()) {
        Some(path) => vec![
            "-o".to_string(),
            "StrictHostKeyChecking=yes".to_string(),
            "-o".to_string(),
            format!("UserKnownHostsFile={path}"),
        ],
        None => Vec::new(),
    }
}

fn identity_opts(entry: &SshEndpoint) -> Result<Vec<String>, IdentityFileError> {
    check_identity(entry)?;
    Ok(entry
        .identity_file
        .as_deref()
        .map(shed_core::identityfile::identity_args)
        .unwrap_or_default())
}

fn port_opts(entry: &SshEndpoint) -> Vec<String> {
    if entry.ssh_port == 22 {
        Vec::new()
    } else {
        vec!["-p".to_string(), entry.ssh_port.to_string()]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn full() -> SshEndpoint {
        SshEndpoint {
            name: "mini2".into(),
            host: "mini2.local".into(),
            user: Some("charliek".into()),
            ssh_port: 2022,
            known_hosts: Some("/kh".into()),
            identity_file: None,
        }
    }

    fn bare() -> SshEndpoint {
        SshEndpoint {
            name: "plain".into(),
            host: "plain".into(),
            ssh_port: 22,
            ..Default::default()
        }
    }

    #[test]
    fn an_identity_file_is_carried_in_the_exact_place() {
        let dir = tempfile::tempdir().unwrap();
        let key = dir.path().join("id work");
        std::fs::write(&key, "a key file: never read\n").unwrap();
        let key_str = key.to_str().unwrap();
        let entry = SshEndpoint {
            name: "roost:shed/box".into(),
            host: "shed.example.test".into(),
            user: Some("box".into()),
            ssh_port: 2222,
            known_hosts: Some("/home/pat/.shed/known_hosts".into()),
            identity_file: Some(key_str.to_string()),
        };
        let identity = [
            "-i",
            key_str,
            "-o",
            "IdentitiesOnly=yes",
            "-o",
            "ControlMaster=no",
            "-o",
            "ControlPath=none",
        ];
        let host_keys = [
            "-o",
            "StrictHostKeyChecking=yes",
            "-o",
            "UserKnownHostsFile=/home/pat/.shed/known_hosts",
        ];
        let mut want: Vec<&str> = vec!["ssh", "-o", "BatchMode=yes"];
        want.extend(host_keys);
        want.extend(identity);
        want.extend([
            "-o",
            "ConnectTimeout=10",
            "-p",
            "2222",
            "box@shed.example.test",
            "--",
            "'true'",
        ]);
        assert_eq!(ssh_argv(&entry, &["true".into()]).unwrap(), want);

        let none = SshEndpoint {
            identity_file: None,
            ..entry.clone()
        };
        let argv = ssh_argv(&none, &["true".into()]).unwrap();
        assert!(!argv
            .iter()
            .any(|a| a == "-i" || a.starts_with("Identities")));
    }

    #[test]
    fn an_explicit_port_zero_is_preserved_not_silently_dropped() {
        let ep = SshEndpoint {
            name: "s".into(),
            host: "example.test".into(),
            ssh_port: 0,
            ..Default::default()
        };
        let argv = ssh_argv(&ep, &["true".to_string()]).expect("argv");
        let p = argv.iter().position(|a| a == "-p").expect("-p present");
        assert_eq!(argv[p + 1], "0");
    }

    #[test]
    fn a_key_that_cannot_be_used_builds_no_argv() {
        let dir = tempfile::tempdir().unwrap();
        let key = dir.path().join("id work");
        std::fs::write(&key, "a key file\n").unwrap();
        let key_str = key.to_str().unwrap();
        let entry = SshEndpoint {
            name: "roost:shed/box".into(),
            host: "shed.example.test".into(),
            identity_file: Some(key_str.to_string()),
            ..Default::default()
        };
        assert!(check_identity(&entry).is_ok());
        std::fs::remove_file(&key).unwrap();
        let want = format!(
            "entry \"roost:shed/box\": identity_file {:?} cannot be used: the file does not exist",
            key_str
        );
        assert_eq!(check_identity(&entry).unwrap_err().to_string(), want);
        assert_eq!(
            ssh_argv(&entry, &["true".into()]).unwrap_err().to_string(),
            want
        );
        let refused = SshEndpoint {
            identity_file: Some("relative/key".into()),
            ..entry.clone()
        };
        assert!(ssh_argv(&refused, &["true".into()]).is_err());
        let none = SshEndpoint {
            identity_file: None,
            ..entry
        };
        assert!(ssh_argv(&none, &["true".into()]).is_ok());
    }

    #[test]
    fn a_full_endpoint_pins_user_port_and_host_key() {
        let argv = ssh_argv(&full(), &["sx".into(), "rc".into(), "list".into()]).unwrap();
        assert_eq!(argv[0], "ssh");
        assert!(argv.contains(&"BatchMode=yes".to_string()));
        assert!(argv.contains(&"StrictHostKeyChecking=yes".to_string()));
        assert!(argv.contains(&"UserKnownHostsFile=/kh".to_string()));
        assert!(argv.windows(2).any(|w| w == ["-p", "2022"]));
        assert!(argv.contains(&"charliek@mini2.local".to_string()));
        assert_eq!(argv[argv.len() - 2], "--");
        assert_eq!(argv[argv.len() - 1], "'sx' 'rc' 'list'");
    }

    #[test]
    fn a_bare_endpoint_says_nothing_ssh_config_can_decide() {
        let argv = ssh_argv(&bare(), &["sx".into(), "rc".into(), "list".into()]).unwrap();
        assert!(!argv.iter().any(|a| a.starts_with("UserKnownHostsFile")));
        assert!(!argv.contains(&"StrictHostKeyChecking=yes".to_string()));
        assert!(!argv.contains(&"-p".to_string()));
        assert!(argv.contains(&"plain".to_string()));
        assert!(!argv.iter().any(|a| a.contains('@')));
    }

    #[tokio::test]
    async fn a_fixed_port_forward_is_stable_and_needs_no_work() {
        let f = FixedPort(41234);
        assert_eq!(f.port(), 41234);
        f.ensure().await.expect("a fixed port is always ready");
        assert_eq!(f.port(), 41234, "ensure must never change the port");
    }
}
