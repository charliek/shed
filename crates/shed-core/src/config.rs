//! Host discovery: parse `~/.shed/config.yaml` — the multi-host server list that
//! shed and shed-remote-agent both read: a deliberately tiny
//! indentation-aware reader scoped to the machine-generated shape
//! (`servers: {NAME: {host, http_port, ssh_port, control_token, api_url,
//! tls_cert_fingerprint, auth_mode, client_cert_file, client_key_file,
//! client_cert_expires_at, user, identity_file}}` + `default_server`), so no client takes on a
//! YAML dependency. The GTK client (M2) reads secure/token-gated Linux hosts through
//! this, so it carries **all** per-server fields — not just host/ports.
//!
//! A parity fixture (`fixtures/config_sample.yaml`) pins this parser's output —
//! the config analog of Phase 1's golden-JSON backbone.
//!
//! **What this reader promises** (plan 037 A8). It reads `config.yaml` **as the shed CLI
//! writes it**: every server entry the CLI's own save path writes with ordinary values is
//! read with the same host, ports, user and `identity_file` the CLI reads
//! (`fixtures/clientconfig/`, written by a Go test through that save path, holds this
//! promise). An entry, or the whole file, written in a form this reader does not read
//! exactly is REFUSED, by name and with a reason ([`ShedServerEntry::identity_file`],
//! [`ShedConfig::refused`]); it is never read with missing fields filled by defaults. A
//! file edited by hand into YAML forms the CLI never writes (a block scalar, a flow
//! mapping, a tag, an anchor, an explicit `? key`) may be refused here while the CLI
//! accepts it: that difference is documented, not a defect.
//!
//! **The threat model.** `config.yaml` is the person's own file, and whoever can write it
//! can already point the app anywhere. The reader's job is never to MISREAD what the CLI
//! wrote (a field the CLI reads, read here as absent or as another value), not to defend
//! against a hostile config file.

/// One server entry from `~/.shed/config.yaml`.
///
/// `Default` is derived for construction convenience in tests and callers that
/// build a partial entry (`..Default::default()`); it is NOT the parser's default
/// entry — [`ShedConfig::parse`] applies the real per-field fallbacks (host =
/// server name, http_port 8080, ssh_port 22).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ShedServerEntry {
    pub name: String,
    pub host: String,
    pub http_port: u16,
    pub ssh_port: u16,
    /// Control-scoped bearer token; empty when the server isn't token-gated.
    pub control_token: String,
    /// HTTPS control-plane URL (`api_url`); overrides host+http_port when set.
    pub api_url: String,
    /// Pinned TLS cert fingerprint (`sha256:<hex>`, lowercased); empty for plain HTTP.
    pub tls_cert_fingerprint: String,
    /// The credential shape the server issued at the last bootstrap: `"token"` or
    /// `"mtls"` (Go `config.ServerEntry.AuthMode`).
    ///
    /// **ABSENT MEANS TOKEN.** Every entry written before client-certificate
    /// support has no `auth_mode` key and every one of those is a token/open
    /// server, so an empty value must never be read as "unknown, go find out". It
    /// is a CACHE of what the server last said, not a setting — a bootstrap that
    /// comes back in the other mode rewrites it, which is what lets an operator
    /// flip a server's `auth.mode` without re-adding every client. Kept as the raw
    /// string (rather than an enum) because this parser's job is to reproduce the
    /// file; interpretation is [`crate::token::AuthMode::from_wire`]'s.
    pub auth_mode: String,
    /// Path to this entry's client certificate (mtls only). A PATH, not inline
    /// PEM: the key beside it must live in a 0600 file of its own, not inside a
    /// config users hand-edit, copy between machines, and paste into issues.
    pub client_cert_file: String,
    /// Path to the private key matching [`Self::client_cert_file`].
    pub client_key_file: String,
    /// RFC3339 expiry of the certificate, cached so the proactive re-enrollment
    /// check costs no file read. The certificate itself remains the authority.
    pub client_cert_expires_at: String,
    /// The named user this entry is held to (Go `config.ServerEntry.User`), written by
    /// the shed CLI when it enrols the entry; empty when absent (a server with no user
    /// list, or an entry with no certificate yet). The app only reads it: a client
    /// certificate issued for another named user is refused
    /// ([`crate::token::ControlTokenProvider::with_held_user`]). A `user` that is not a
    /// plain string reads as [`USER_NOT_A_STRING`] (fail closed).
    pub user: String,
    /// The SSH key this entry uses (Go `config.ServerEntry.IdentityFile`, set by `shed
    /// server add --identity-file`), as config.yaml holds it; empty when absent or null.
    /// Never trusted as read: every site that builds an ssh command, or opens a tunnel or a
    /// runner, for the entry checks it first ([`crate::identityfile`]), and a value this
    /// reader cannot read exactly reads as [`crate::identityfile::IDENTITY_FILE_UNREADABLE`]
    /// (or, for an entry it does not read at all,
    /// [`crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE`]), which every check refuses
    /// (fail closed, never "no identity file").
    pub identity_file: String,
}

/// The resolved control-plane endpoint + TLS pin for a server entry.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedEndpoint {
    pub base_url: String,
    pub pin: String,
}

impl ShedServerEntry {
    /// The credential shape this entry last saw the server issue (absent ⇒
    /// [`crate::token::AuthMode::Token`]).
    pub fn auth_mode(&self) -> crate::token::AuthMode {
        crate::token::AuthMode::from_wire(Some(&self.auth_mode))
    }

    /// Is this entry's control plane HTTPS — which, for a shed-server, means the
    /// server enforces auth (token or mtls)? Open mode serves plain HTTP and has
    /// no HTTPS listener at all, so this is the one durable signal in a stored
    /// entry that separates "wants a credential" from "wants nothing".
    ///
    /// Either marker alone is enough (Go parity, `ServerEntry.UsesTLS`): an entry
    /// is user-editable, and a half-filled one still points at a listener that
    /// will demand a credential.
    pub fn uses_tls(&self) -> bool {
        !self.tls_cert_fingerprint.is_empty() || self.api_url.to_lowercase().starts_with("https://")
    }

    /// Does this entry hold NOTHING it could present to a server that demands a
    /// credential, while having enough information (host + ssh port) to obtain one
    /// over the `_bootstrap` SSH channel?
    ///
    /// The Go rule verbatim (`ServerEntry.NeedsEnrollment`, fix `4553cc8`), because
    /// "no credential recorded" is ambiguous in a stored entry and the two readings
    /// need opposite handling:
    ///
    ///   * an OPEN server legitimately has none, and must never pay an SSH
    ///     round-trip to discover that — it is plain HTTP, so `uses_tls` is false;
    ///   * a SECURE server's entry that LOST its credential fields must enroll.
    ///     That is routine during an upgrade, not a corner case: a pre-mtls client
    ///     that loads and re-saves `config.yaml` silently drops every key its own
    ///     struct does not know (`auth_mode`, `client_cert_file`,
    ///     `client_key_file`, `client_cert_expires_at`), leaving exactly this
    ///     shape. Without enrollment such an entry fails FOREVER: the client holds
    ///     no credential to be rejected, so it never reaches its reactive re-mint.
    ///
    /// A legacy static token (a `control_token` with no expiry) is NOT this case —
    /// it has something to present and is deliberately never re-minted.
    pub fn needs_enrollment(&self) -> bool {
        if !self.uses_tls() || self.host.is_empty() || self.ssh_port == 0 {
            return false;
        }
        match self.auth_mode() {
            crate::token::AuthMode::Mtls => {
                self.client_cert_file.is_empty() || self.client_key_file.is_empty()
            }
            crate::token::AuthMode::Token => self.control_token.is_empty(),
        }
    }

    /// Resolve the control-plane endpoint the client should dial: the https
    /// `api_url` (with its pinned cert) when it's a valid http(s) URL with a
    /// host, else plain `http://host:http_port` — the resolution whose absence
    /// once dialed the dead `:8080` instead of the secure `:8443`.
    pub fn resolved_endpoint(&self) -> ResolvedEndpoint {
        if !self.api_url.is_empty() && is_http_url_with_host(&self.api_url) {
            ResolvedEndpoint {
                base_url: self.api_url.clone(),
                pin: self.tls_cert_fingerprint.clone(),
            }
        } else {
            ResolvedEndpoint {
                base_url: format!("http://{}:{}", self.host, self.http_port),
                pin: self.tls_cert_fingerprint.clone(),
            }
        }
    }
}

/// True iff `s` is an `http://`/`https://` URL with a non-empty, whitespace-free
/// host — the check `resolved_endpoint` relies on (a relative path, a non-http
/// scheme, an empty host, or an unparseable string all fail).
fn is_http_url_with_host(s: &str) -> bool {
    let lower = s.to_lowercase();
    let rest = match lower
        .strip_prefix("http://")
        .or_else(|| lower.strip_prefix("https://"))
    {
        Some(r) => r,
        None => return false,
    };
    let host = rest.split(['/', ':', '?', '#']).next().unwrap_or("");
    !host.is_empty() && !host.contains(char::is_whitespace)
}

/// The shed CLI's `known_hosts` file (`~/.shed/known_hosts`, the same file
/// `shed server add` pins keys into) — `~/.shed` on both macOS and Linux.
pub fn known_hosts_path() -> String {
    static PATH: std::sync::LazyLock<String> = std::sync::LazyLock::new(|| {
        let home = std::env::var("HOME").unwrap_or_default();
        format!("{home}/.shed/known_hosts")
    });
    PATH.clone()
}

/// The parsed `~/.shed/config.yaml`: server entries (sorted by name) and the
/// default server.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct ShedConfig {
    pub servers: Vec<ShedServerEntry>,
    pub default_server: Option<String>,
    /// Why the WHOLE file was not read, when it was not (plan 037 A8): it holds a line
    /// break this reader does not split lines on, a second `servers:` key, or `servers:`
    /// in a form other than a block of entries ([`ShedConfig::parse`]). Nothing in it is
    /// read then: no server, no default server. `None` for a file read as usual
    /// (including a missing one).
    pub refused: Option<String>,
}

impl ShedConfig {
    /// Load + parse the config at `path`. A missing/unreadable file → an empty
    /// config (a degraded but non-fatal state the dashboard surfaces, never an
    /// error).
    pub fn load(path: &str) -> Self {
        match std::fs::read_to_string(path) {
            Ok(text) => Self::parse(&text),
            Err(_) => Self::default(),
        }
    }

    /// Parse config.yaml's text. A byte-order mark at the start is skipped; only the first
    /// YAML document is read. A file this reader would read differently from the CLI's
    /// YAML reader as a whole is not read at all: [`ShedConfig::refused`] says why.
    pub fn parse(text: &str) -> Self {
        let text = text.strip_prefix('\u{feff}').unwrap_or(text);
        if let Some(reason) = yaml_lite::file_refusal(text, "servers") {
            return Self {
                refused: Some(reason),
                ..Self::default()
            };
        }
        let Node::Map(top) = yaml_lite::parse(text) else {
            return Self::default();
        };
        // A bare `name:` entry has every field at its default, like `name: {}`.
        let no_fields = std::collections::HashMap::new();
        let fields_of = |value| entry_fields(value, &no_fields);
        // The server entries this reader does not read at all (plan 037 A8): decided from
        // the entries' LINES, before any field is read or given a default
        // ([`yaml_lite::refused_entries`]), so a field this reader would miss can never
        // become no `identity_file`, no `user`, or the entry's name as its host.
        let refused = yaml_lite::refused_entries(text, "servers");
        let mut servers = Vec::new();
        if let Some(Node::Map(server_map)) = top.get("servers") {
            for (name, value) in server_map {
                let fields = match fields_of(value) {
                    Some(fields) if !refused.contains(name) => fields,
                    // Never dropped silently: an entry it cannot read (or whose value is
                    // not a mapping at all, such as `prod: {host: …}` on one line) is
                    // kept, as an entry every use refuses by name.
                    _ => {
                        servers.push(ShedServerEntry::unreadable(name));
                        continue;
                    }
                };
                let scalar = |k: &str| fields.get(k).and_then(Node::as_scalar);
                // `host` is the entry's name only when the key is absent. An explicit
                // null (`host:`, `host: ~`, `host: null`; Go reads no host) or a host
                // that is not a string is no destination at all: the entry is refused,
                // never sent to a host named after it or named "null".
                let host = match fields.get("host") {
                    None => name.as_str(),
                    Some(Node::Scalar(host)) => host.as_str(),
                    Some(_) => {
                        servers.push(ShedServerEntry::unreadable(name));
                        continue;
                    }
                };
                servers.push(ShedServerEntry {
                    name: name.clone(),
                    host: host.to_string(),
                    http_port: scalar("http_port")
                        .and_then(|s| s.parse().ok())
                        .unwrap_or(8080),
                    ssh_port: scalar("ssh_port")
                        .and_then(|s| s.parse().ok())
                        .unwrap_or(22),
                    control_token: scalar("control_token").unwrap_or("").to_string(),
                    api_url: scalar("api_url").unwrap_or("").to_string(),
                    // Canonicalize to the lowercase "sha256:<hex>" the server emits,
                    // so a hand-edited upper/mixed-case pin still matches at
                    // handshake time rather than silently failing every connection.
                    tls_cert_fingerprint: scalar("tls_cert_fingerprint")
                        .unwrap_or("")
                        .to_lowercase(),
                    // Lower-cased like the pin: the writer always emits the
                    // canonical literal, and a hand-edited `MTLS` should mean what
                    // it obviously means rather than silently falling back to
                    // token mode.
                    auth_mode: scalar("auth_mode").unwrap_or("").to_lowercase(),
                    client_cert_file: scalar("client_cert_file").unwrap_or("").to_string(),
                    client_key_file: scalar("client_key_file").unwrap_or("").to_string(),
                    client_cert_expires_at: scalar("client_cert_expires_at")
                        .unwrap_or("")
                        .to_string(),
                    user: config_user(fields.get("user")),
                    identity_file: config_identity_file(fields.get("identity_file")),
                });
            }
        }
        // An entry the tree holds no key for (its name written as an explicit `? name`) is
        // still listed, refused under that name.
        for name in &refused {
            if !servers.iter().any(|s| &s.name == name) {
                servers.push(ShedServerEntry::unreadable(name));
            }
        }
        servers.sort_by(|a, b| a.name.cmp(&b.name));

        ShedConfig {
            servers,
            default_server: top
                .get("default_server")
                .and_then(Node::as_scalar)
                .map(str::to_string),
            refused: None,
        }
    }
}

/// What [`ShedServerEntry::user`] holds when config.yaml's `user` is not a plain string (a
/// list or a map, which no shed CLI writes and Go refuses to load at all). It is no valid
/// user name (a plain YAML value cannot start with `!`), so a provider held to it refuses
/// every certificate for a named user, as for any other value that is no user name: the
/// entry fails closed, never as "no user recorded".
pub const USER_NOT_A_STRING: &str = "!not-a-string";

/// An entry's fields: its map, or `empty` for a bare `name:` entry; `None` for anything
/// else (the entry is skipped).
fn entry_fields<'a>(
    value: &'a Node,
    empty: &'a std::collections::HashMap<String, Node>,
) -> Option<&'a std::collections::HashMap<String, Node>> {
    match value {
        Node::Map(fields) => Some(fields),
        Node::Null => Some(empty),
        _ => None,
    }
}

impl ShedServerEntry {
    /// A server entry this reader does not read ([`yaml_lite::refused_entries`], or a null
    /// `host`): only its name. Its `identity_file` is
    /// [`crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE`], which every check refuses
    /// with the entry's name and what to write instead, and its `user` is
    /// [`USER_NOT_A_STRING`]; every other field is empty or zero (no host, no port, no
    /// credential), never a default.
    fn unreadable(name: &str) -> Self {
        Self {
            name: name.to_string(),
            user: USER_NOT_A_STRING.to_string(),
            identity_file: crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE.to_string(),
            ..Self::default()
        }
    }
}

/// An entry's `user` as the readers hold it: absent, a bare `user:`, `null` or `~` → `""`
/// (no user recorded, as Go's reader and the broker's discovery read them); a scalar →
/// verbatim (the CLI writes a valid user name, and the provider compares it byte for byte,
/// so a hand-edited value that is no user name matches no certificate and refuses);
/// anything else (a nested mapping; a value this reader cannot read exactly, which only an
/// entry it refuses whole can hold) → [`USER_NOT_A_STRING`].
fn config_user(node: Option<&Node>) -> String {
    match node {
        None | Some(Node::Null) => String::new(),
        Some(Node::Scalar(s)) => s.clone(),
        Some(Node::Map(_) | Node::Seq | Node::Malformed(_)) => USER_NOT_A_STRING.to_string(),
    }
}

/// An entry's `identity_file` as the readers hold it, the same way as its `user`: absent or
/// null → `""` (no identity file); a scalar → verbatim (checked at every use); anything
/// else (as for `user`: a nested mapping) → [`crate::identityfile::IDENTITY_FILE_UNREADABLE`],
/// which every check refuses.
fn config_identity_file(node: Option<&Node>) -> String {
    match node {
        None | Some(Node::Null) => String::new(),
        Some(Node::Scalar(s)) => s.clone(),
        Some(Node::Map(_) | Node::Seq | Node::Malformed(_)) => {
            crate::identityfile::IDENTITY_FILE_UNREADABLE.to_string()
        }
    }
}

pub use crate::identityfile::identity_file_args;

/// A deliberately tiny indentation-based reader. Handles exactly what
/// `~/.shed/config.yaml` contains: nested maps and scalar leaves. Inline `{}` is
/// an empty map, a bare `key:` is null; comments and blank lines are skipped. A
/// block sequence under a key is recognized as one (so it is never mistaken for an
/// empty value), but its items are not read.
///
/// **Scalars are read as YAML reads them** (plan 037 A8: a path in `identity_file` must
/// mean in Rust what it means to the Go CLI, which wrote it with gopkg.in/yaml.v3):
///
/// * a plain value ends at a comment, which is a `#` after a space or a tab (a `#` inside
///   a word is text: `/a#b` is the path `/a#b`); only spaces and tabs are trimmed around
///   it (a trailing U+00A0 is part of the value, as in YAML), and `~`, `null`, `Null` and
///   `NULL` are null;
/// * a single-quoted value is literal, with `''` for one `'`;
/// * a double-quoted value decodes YAML's escapes: `\0 \a \b \t \n \v \f \r \e \" \/ \\`,
///   a backslash-space, `\N` (U+0085), `\_` (U+00A0), `\L` (U+2028), `\P` (U+2029), and
///   `\xXX`, `\uXXXX`, `\UXXXXXXXX`; a `#` inside the quotes is text;
/// * a quoted key is decoded as a quoted value is, so `"identity_file":` is the key
///   `identity_file`; a plain key ends at the first `:` followed by a space, a tab or the
///   end of the line, so `prod:blue:` is the key `prod:blue` ([`split_key`]);
/// * only the first YAML document is read (a `---` or `...` after content ends it), and a
///   line indented with a tab is text held to the line before it ([`lines`]);
/// * a value this reader cannot read exactly is kept as [`Node::Malformed`], which the
///   fields that must fail closed (`user`, `identity_file`) refuse, and which every other
///   field reads as the text it always read: a quoted value that does not close on its
///   line, holds an escape YAML does not define, or is followed by anything but a comment;
///   a plain value that begins with a tag `!`, an anchor `&`, an alias `*` or a block
///   scalar `|` or `>`, or that holds `: `; a value that runs on to a
///   following line indented under its key (YAML folds those lines into it, or refuses
///   them); and a second value for a key the map already has (Go refuses the file).
///
/// **A server entry is read only when every line of it is understood**
/// ([`refused_entries`], plan 037 A8). The tree above is a best effort for the rest of the
/// file; a server entry, which says which host ssh reaches and with which key and as which
/// user, is held to a whitelist instead: each of its lines must be a `key: value` this
/// reader reads exactly as YAML does, or the whole entry is refused.
mod yaml_lite {
    use std::collections::{HashMap, HashSet};

    #[derive(Debug, Clone, PartialEq, Eq)]
    pub enum Node {
        Map(HashMap<String, Node>),
        Scalar(String),
        /// A bare `key:` with no value and no block under it, or a plain null (`~`,
        /// `null`).
        Null,
        /// A block sequence (`- item` lines) under a key. Its items are not read: no
        /// field of the config is a list, and a value that is one is only ever an
        /// error to report, never data (a `user:` list must not read as no user).
        Seq,
        /// A value this reader cannot read exactly, kept as the text it reads as
        /// otherwise (see the module doc).
        Malformed(String),
    }

    impl Node {
        /// The value's text. A [`Node::Malformed`] value reads as the text it was written
        /// as, as it always has for the fields that need no more care; `user` and
        /// `identity_file` match on the node instead and refuse it.
        pub fn as_scalar(&self) -> Option<&str> {
            match self {
                Node::Scalar(s) | Node::Malformed(s) => Some(s),
                _ => None,
            }
        }
    }

    enum Value {
        /// `key: text` — a scalar.
        Scalar(String),
        /// `key: ~` or `key: null`, with the text it was written as.
        Null(String),
        /// A scalar this reader cannot read exactly.
        Malformed(String),
        /// `key: {}` — an inline empty map.
        EmptyMap,
        /// `key:` — a nested block, a block sequence, or nothing.
        Open,
    }

    /// What YAML trims around a value on its line (and a CR, for a CRLF file).
    const SPACE: &[char] = &[' ', '\t', '\r'];

    /// The value after `key:`.
    fn scalar_value(rest: &str) -> Value {
        if rest.is_empty() || rest.starts_with('#') {
            return Value::Open;
        }
        let quoted = match rest.chars().next() {
            Some('"') => Some(double_quoted(rest)),
            Some('\'') => Some(single_quoted(rest)),
            _ => None,
        };
        if let Some(parsed) = quoted {
            return match parsed {
                Some(text) => Value::Scalar(text),
                None => Value::Malformed(rest.to_string()),
            };
        }
        let plain = strip_comment(rest);
        match plain {
            "{}" => Value::EmptyMap,
            "~" | "null" | "Null" | "NULL" => Value::Null(plain.to_string()),
            _ if !plain_is_exact(plain) => Value::Malformed(plain.to_string()),
            _ => Value::Scalar(plain.to_string()),
        }
    }

    /// Whether a plain value is read by YAML as the text it is: it does not begin with a
    /// tag (`!`), an anchor (`&`), an alias (`*`) or a block scalar (`|`, `>`), where YAML
    /// reads another value than the text (`!!str null` is the string "null"; `&a /k` is
    /// `/k`), and it holds no `: ` and does not end in `:` (YAML refuses a mapping inside a
    /// plain value). A flow collection (`[pat]`) stays the text it always read as: no
    /// user name or absolute path begins with one, so `user` and `identity_file` refuse it
    /// anyway.
    fn plain_is_exact(plain: &str) -> bool {
        const INDICATORS: [char; 5] = ['!', '&', '*', '|', '>'];
        !plain.starts_with(INDICATORS)
            && !plain.contains(": ")
            && !plain.contains(":\t")
            && !plain.ends_with(':')
    }

    /// A plain value up to its comment (a `#` after a space or a tab), trimmed.
    fn strip_comment(rest: &str) -> &str {
        let cut = rest
            .char_indices()
            .find(|&(i, c)| c == '#' && rest[..i].ends_with([' ', '\t']))
            .map_or(rest.len(), |(i, _)| i);
        rest[..cut].trim_end_matches(SPACE)
    }

    /// After a closing quote only a comment may follow.
    fn ends_cleanly(after: &str) -> bool {
        let after = after.trim_start_matches(SPACE);
        after.is_empty() || after.starts_with('#')
    }

    /// `'…'`: literal, `''` is one `'`.
    fn single_quoted(rest: &str) -> Option<String> {
        let body = &rest[1..];
        let mut out = String::new();
        let mut chars = body.char_indices().peekable();
        while let Some((i, c)) = chars.next() {
            if c != '\'' {
                out.push(c);
                continue;
            }
            if chars.peek().is_some_and(|&(_, next)| next == '\'') {
                chars.next();
                out.push('\'');
                continue;
            }
            return ends_cleanly(&body[i + 1..]).then_some(out);
        }
        None // never closed on this line
    }

    /// `"…"` with YAML's escapes.
    fn double_quoted(rest: &str) -> Option<String> {
        let body = &rest[1..];
        let mut out = String::new();
        let mut chars = body.char_indices();
        while let Some((i, c)) = chars.next() {
            match c {
                '"' => return ends_cleanly(&body[i + 1..]).then_some(out),
                '\\' => {
                    let (_, escape) = chars.next()?;
                    let decoded = match escape {
                        '0' => '\0',
                        'a' => '\u{7}',
                        'b' => '\u{8}',
                        't' | '\t' => '\t',
                        'n' => '\n',
                        'v' => '\u{b}',
                        'f' => '\u{c}',
                        'r' => '\r',
                        'e' => '\u{1b}',
                        ' ' => ' ',
                        '"' => '"',
                        '/' => '/',
                        '\\' => '\\',
                        'N' => '\u{85}',
                        '_' => '\u{a0}',
                        'L' => '\u{2028}',
                        'P' => '\u{2029}',
                        'x' | 'u' | 'U' => {
                            let digits = match escape {
                                'x' => 2,
                                'u' => 4,
                                _ => 8,
                            };
                            let mut code = 0u32;
                            for _ in 0..digits {
                                let (_, d) = chars.next()?;
                                code = code * 16 + d.to_digit(16)?;
                            }
                            char::from_u32(code)?
                        }
                        _ => return None, // an escape YAML does not define
                    };
                    out.push(decoded);
                }
                c => out.push(c),
            }
        }
        None // never closed on this line
    }

    /// What kind of line a [`Line`] is.
    #[derive(PartialEq)]
    enum Kind {
        /// `key: …`.
        Entry,
        /// A block-sequence item (`- …`); its key and value are not read.
        Item,
        /// Any other text (no `key:`): never data itself, but a value it follows, indented
        /// under the value's key, runs on to it.
        Text,
    }

    struct Line {
        indent: usize,
        key: String,
        value: Value,
        kind: Kind,
        /// Whether the line's key, and its value, are forms this reader reads exactly as
        /// YAML does ([`split_key`], [`value_understood`]): what [`refused_entries`]
        /// holds a server entry's lines to.
        key_ok: bool,
        value_ok: bool,
        /// A tab in the line's indentation (YAML indents with spaces only).
        tab: bool,
        /// The key of an explicit `? key` line (the form gopkg.in/yaml.v3 writes a key over
        /// 128 bytes in), decoded as well as this reader can: the name such an entry is
        /// refused under.
        explicit_key: Option<String>,
    }

    impl Line {
        /// A `key: value` line whose key and value are both understood.
        fn understood(&self) -> bool {
            self.kind == Kind::Entry && self.key_ok && self.value_ok
        }
    }

    /// Whether `raw` is a YAML document marker: `---` (a document starts) or `...` (it
    /// ends) at the start of the line, followed by a space, a tab or the end of the line.
    fn document_marker(raw: &str) -> Option<&str> {
        let raw = raw.strip_suffix('\r').unwrap_or(raw);
        ["---", "..."].into_iter().find(|marker| {
            raw.strip_prefix(marker)
                .is_some_and(|after| after.is_empty() || after.starts_with([' ', '\t']))
        })
    }

    /// Whether a raw line carries nothing (blank, or only a comment).
    fn is_blank(raw: &str) -> bool {
        let trimmed = raw.trim_matches(SPACE);
        trimmed.is_empty() || trimmed.starts_with('#')
    }

    /// The raw lines of the file's FIRST YAML document, as Go's reader and the broker's
    /// read only that one: a `---` before any content starts it and is skipped, and a
    /// `---` or a `...` after content has begun ends the reading, so nothing after it can
    /// add, replace or remove an entry.
    fn first_document(text: &str) -> impl Iterator<Item = &str> {
        let mut begun = false;
        text.split('\n')
            .take_while(move |raw| {
                if document_marker(raw).is_some() {
                    return !begun;
                }
                begun |= !is_blank(raw);
                true
            })
            .filter(|raw| document_marker(raw).is_none())
    }

    /// The lines of the first document that carry something (not blank, not a comment),
    /// each read once.
    fn lines(text: &str) -> Vec<Line> {
        let mut out: Vec<Line> = Vec::new();
        for raw in first_document(text) {
            if is_blank(raw) {
                continue;
            }
            let trimmed = raw.trim_matches(SPACE);
            let indent = raw.len() - raw.trim_start_matches([' ', '\t']).len();
            let blank_line = |indent: usize, kind: Kind| Line {
                indent,
                key: String::new(),
                value: Value::Open,
                kind,
                key_ok: false,
                value_ok: false,
                tab: false,
                explicit_key: None,
            };
            // YAML indents with spaces only (Go refuses a tab there). A tab-indented line
            // is held to the line before it: it sits at that line's indent, inside the
            // entry that line is in, and is text no entry may hold, so that entry is
            // refused whole (never read without the field the line meant to give it, and
            // never taken for an entry of its own).
            if raw[..indent].contains('\t') {
                let at = out.last().map_or(0, |l| l.indent);
                out.push(Line {
                    tab: true,
                    ..blank_line(at, Kind::Text)
                });
                continue;
            }
            let lone_or_spaced = |indicator: char| {
                trimmed
                    .strip_prefix(indicator)
                    .is_some_and(|after| after.is_empty() || after.starts_with(SPACE))
            };
            if lone_or_spaced('-') {
                out.push(blank_line(indent, Kind::Item));
                continue;
            }
            // An explicit key (`? key`) and the value line that goes with it (`: …`): text
            // to this reader, never a key of its own; the key is kept to name the entry.
            if lone_or_spaced('?') {
                let key = trimmed[1..].trim_matches(SPACE);
                let name = match key.chars().next() {
                    Some('"') => double_quoted(key),
                    Some('\'') => single_quoted(key),
                    _ => Some(strip_comment(key).to_string()),
                };
                out.push(Line {
                    explicit_key: Some(name.unwrap_or_else(|| key.to_string())),
                    ..blank_line(indent, Kind::Text)
                });
                continue;
            }
            if lone_or_spaced(':') {
                out.push(blank_line(indent, Kind::Text));
                continue;
            }
            let content = &raw[indent..];
            out.push(match split_key(content) {
                Some((key, key_ok, rest)) => {
                    let rest = rest.trim_matches(SPACE);
                    Line {
                        key,
                        value: scalar_value(rest),
                        key_ok,
                        value_ok: value_understood(rest),
                        ..blank_line(indent, Kind::Entry)
                    }
                }
                None => blank_line(indent, Kind::Text),
            });
        }
        out
    }

    pub fn parse(text: &str) -> Node {
        let lines = lines(text);
        let mut index = 0;
        build(&lines, &mut index, -1)
    }

    /// **The whitelist** (plan 037 A8): the names of the entries under the top-level map
    /// `section` (`servers`) that this reader does not read, because a line inside one is
    /// not a form it fully understands. A server entry is read ONLY when:
    ///
    /// * its own line is `name:` (its fields follow, one per line) or `name: {}`;
    /// * every line inside it is a `key: value` the reader reads exactly as YAML does
    ///   ([`understood`]): a plain or quoted key on its own line, then a plain or quoted
    ///   value on the same line, or nothing (a null, or the start of a nested mapping);
    /// * a deeper line comes only right after a `key:` with no value, as the first line
    ///   of a nested mapping made of the same lines (deeper after a value is a
    ///   continuation line); a shallower one returns to a level already open; and no key
    ///   appears twice in one mapping, nor the entry's name twice in the section.
    ///
    /// Every other line inside an entry makes the WHOLE entry unreadable: an explicit `?`
    /// key or a `:` line, a flow collection (`{…}`, `[…]`), a tag, an anchor or an alias,
    /// a block scalar (`|`, `>`), a merge key (`<<`), a continuation line, a repeated key,
    /// a sequence item, a line indented with a tab, and any line this reader cannot
    /// classify. The list is what is ACCEPTED, so a YAML form nobody listed is refused, not
    /// read with a default.
    ///
    /// The section is the one `section:` at the top level ([`file_refusal`] refuses a file
    /// with two). A line inside the section that is no entry (a sequence item, an indent no
    /// entry has, a line indented with a tab) makes the entry before it unreadable. An
    /// entry whose key is written in the explicit form (`? name`, then `: …`) is refused
    /// under the name its `?` line gives, decoded as well as this reader can.
    pub fn refused_entries(text: &str, section: &str) -> HashSet<String> {
        let lines = lines(text);
        let mut refused = HashSet::new();
        let Some(top) = lines.first().map(|l| l.indent) else {
            return refused;
        };
        let Some(at) = lines
            .iter()
            .position(|l| l.indent == top && l.kind == Kind::Entry && l.key == section)
        else {
            return refused;
        };
        let body: Vec<&Line> = lines[at + 1..]
            .iter()
            .take_while(|l| l.indent > top)
            .collect();
        let Some(entry_indent) = body.first().map(|l| l.indent) else {
            return refused;
        };
        // `section: {}` (or any value) with entries under it: YAML refuses that; refuse
        // them all rather than read entries from a map that has none.
        let section_opens = matches!(lines[at].value, Value::Open);
        let mut names = HashSet::new();
        let mut previous: Option<&str> = None;
        let mut i = 0;
        while i < body.len() {
            let header = body[i];
            i += 1;
            let start = i;
            while i < body.len() && body[i].indent > entry_indent {
                i += 1;
            }
            if let (true, Some(name)) = (header.indent == entry_indent, &header.explicit_key) {
                // `? name`: an entry this reader does not read, under its own name; the
                // `: …` line that follows it is then held to it.
                refused.insert(name.clone());
                previous = Some(name.as_str());
                continue;
            }
            if header.indent != entry_indent || header.kind != Kind::Entry {
                if let Some(name) = previous {
                    refused.insert(name.to_string());
                }
                continue;
            }
            let read = section_opens
                && entry_is_understood(header, &body[start..i])
                && names.insert(header.key.as_str());
            if !read {
                refused.insert(header.key.clone());
            }
            previous = Some(header.key.as_str());
        }
        refused
    }

    /// Why the WHOLE file is not read, or `None` (plan 037 A8). These are files whose
    /// server entries this reader could read differently from Go's YAML reader in a way no
    /// entry-by-entry refusal can catch, so none of the file is read:
    ///
    /// * a raw line break this reader does not split lines on, anywhere in the file: a CR
    ///   that is not part of a CRLF pair, NEL (U+0085), a line separator (U+2028) or a
    ///   paragraph separator (U+2029). YAML breaks a line at each, so a field after one
    ///   inside an apparent comment is a field to Go. (Their escaped forms inside a
    ///   double-quoted value, `\r`, `\N`, `\L`, `\P`, are text and are not affected.)
    /// * a first document whose `---` has content on its own line;
    /// * a top-level `section:` key that appears twice (Go refuses the file), or one written
    ///   with a tag or an anchor (`!!str servers:`, `&s servers:`), or whose value is not a
    ///   block mapping of entries (`servers: {prod: {…}}`, `servers: [...]`,
    ///   `servers: &s`, `servers: !!map`, `servers: *s`);
    /// * a line indented with a tab right under `section:`, which no entry holds.
    pub fn file_refusal(text: &str, section: &str) -> Option<String> {
        let bytes = text.as_bytes();
        for (i, c) in text.char_indices() {
            let what = match c {
                '\r' if bytes.get(i + 1) != Some(&b'\n') => {
                    "a carriage return (CR) that is not part of a CRLF line ending"
                }
                '\u{85}' => "a next-line character (NEL, U+0085)",
                '\u{2028}' => "a line separator (U+2028)",
                '\u{2029}' => "a paragraph separator (U+2029)",
                _ => continue,
            };
            return Some(format!(
                "it holds {what}, which YAML reads as a line break and this app does not"
            ));
        }
        let opener = text
            .split('\n')
            .find(|raw| document_marker(raw).is_some() || !is_blank(raw));
        if let Some(raw) = opener.filter(|raw| document_marker(raw) == Some("---")) {
            let after = raw.trim_end_matches('\r')[3..].trim_matches(SPACE);
            if !after.is_empty() && !after.starts_with('#') {
                return Some(
                    "its first document starts with content on the `---` line".to_string(),
                );
            }
        }
        let lines = lines(text);
        let top = lines.first()?.indent;
        let mut found = 0;
        for (at, line) in lines.iter().enumerate() {
            if line.indent != top || line.kind != Kind::Entry {
                continue;
            }
            if line.key == section && line.key_ok {
                found += 1;
                if !matches!(line.value, Value::Open | Value::EmptyMap | Value::Null(_)) {
                    return Some(format!(
                        "its `{section}:` is not a block of entries, one per line (it is \
                         written in flow form, or with a tag, an anchor or an alias)"
                    ));
                }
                if lines.get(at + 1).is_some_and(|next| next.tab) {
                    return Some(format!(
                        "a line under `{section}:` is indented with a tab (YAML indents with \
                         spaces)"
                    ));
                }
                continue;
            }
            // The key with a tag or an anchor before it, or the section opened in flow form
            // on the line (`{servers: …}`).
            let bare = line
                .key
                .trim_start_matches(['{', '['])
                .rsplit([' ', '\t'])
                .next()
                .unwrap_or("")
                .trim_matches(['"', '\'']);
            if !line.key_ok && bare == section {
                return Some(format!(
                    "its `{section}:` is written with a tag, an anchor or in flow form"
                ));
            }
        }
        (found > 1).then(|| format!("it has the key `{section}:` more than once"))
    }

    /// One entry, its own line and the lines inside it, held to [`refused_entries`]'s
    /// rules.
    fn entry_is_understood(header: &Line, inside: &[&Line]) -> bool {
        // Its own line: `name:` (fields follow), or `name: {}` with nothing under it.
        let own_line = header.kind == Kind::Entry
            && header.key_ok
            && match header.value {
                Value::Open => true,
                Value::EmptyMap => inside.is_empty(),
                _ => false,
            };
        if !own_line {
            return false;
        }
        // The open mappings, outermost first: each one's indent and the keys it holds.
        let mut open: Vec<(usize, HashSet<&str>)> = Vec::new();
        // Whether the line before opened a mapping (`key:` with no value).
        let mut opens = true;
        for line in inside {
            if !line.understood() {
                return false;
            }
            let deepest = open.last().map_or(header.indent, |(indent, _)| *indent);
            if line.indent > deepest {
                if !opens {
                    return false; // a continuation of the value above it
                }
                open.push((line.indent, HashSet::new()));
            } else {
                while open.last().is_some_and(|(indent, _)| *indent > line.indent) {
                    open.pop();
                }
                if open.last().map(|(indent, _)| *indent) != Some(line.indent) {
                    return false; // an indent no open mapping has
                }
            }
            let Some((_, keys)) = open.last_mut() else {
                return false;
            };
            if !keys.insert(line.key.as_str()) {
                return false; // a repeated key
            }
            opens = matches!(line.value, Value::Open);
        }
        true
    }

    /// What a plain key or value may not begin with: YAML gives each a meaning there.
    const INDICATORS: [char; 19] = [
        '-', '?', ':', ',', '[', ']', '{', '}', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@',
        '`',
    ];

    /// Whether a value (`rest`, the text after its key's colon, trimmed) is a form this
    /// reader reads exactly as YAML does: nothing (or a comment), a quoted value that
    /// closes on the line and decodes with nothing but a comment after it, or a plain value
    /// that begins with no indicator (as for a key, and no `-`, `?` or `:` followed by a
    /// space or the end) and holds no `: ` and does not end in `:`.
    fn value_understood(rest: &str) -> bool {
        if rest.is_empty() || rest.starts_with('#') {
            return true;
        }
        match rest.chars().next() {
            Some('"') => double_quoted(rest).is_some(),
            Some('\'') => single_quoted(rest).is_some(),
            _ => {
                let plain = strip_comment(rest);
                // `-`, `?` and `:` begin a plain value when a character other than a
                // space follows (`-1`); alone, or before a space, each is an indicator.
                let lone = |c: char| {
                    plain
                        .strip_prefix(c)
                        .is_some_and(|after| after.is_empty() || after.starts_with(SPACE))
                };
                plain_is_exact(plain)
                    && !plain.starts_with(&INDICATORS[3..])
                    && !['-', '?', ':'].into_iter().any(lone)
            }
        }
    }

    /// The byte index of the colon that ends a plain key: the first `:` followed by a
    /// space, a tab or the end of the line. A colon followed by anything else is part of
    /// the key, as in YAML (`prod:blue:` is the key `prod:blue`; `10.0.0.5:8443:` is the
    /// key `10.0.0.5:8443`).
    fn key_colon(line: &str) -> Option<usize> {
        line.char_indices()
            .find(|&(i, c)| {
                c == ':'
                    && line[i + 1..]
                        .chars()
                        .next()
                        .is_none_or(|next| SPACE.contains(&next))
            })
            .map(|(i, _)| i)
    }

    /// A `key: …` line's key, whether the key is a form this reader reads exactly as YAML
    /// does, and the text after its colon; `None` for a line that is not one. The one
    /// place a key is read, for the tree and for the whitelist alike:
    ///
    /// * a quoted key is decoded as a quoted value is (YAML's escapes, `''`), and its colon
    ///   is the first one after its closing quote, with only spaces between; it is
    ///   understood when it closes and decodes and is not `<<` (whether a YAML reader takes
    ///   a quoted `<<` for the merge key is not worth betting an `identity_file` on; the
    ///   broker refuses it as well); a quoted key that does not close is kept as written
    ///   (it then names no field this reader looks for);
    /// * a plain key ends at the colon [`key_colon`] finds, and only spaces and tabs are
    ///   trimmed from its end (`host` followed by a no-break space is not the key `host`,
    ///   as in YAML); it is understood when it begins with no YAML indicator (`- ? : , [ ]
    ///   { } # & * ! | > ' " % @` and a backtick), is not the merge key `<<`, and holds no
    ///   comment;
    /// * either is followed by a space, a tab or the end of the line after its colon.
    fn split_key(line: &str) -> Option<(String, bool, &str)> {
        if let Some(close) = closing_quote(line) {
            let after = &line[close + 1..];
            let colon = after.find(':')?;
            if !after[..colon].trim_matches(SPACE).is_empty() {
                return None;
            }
            let quoted = &line[..=close];
            let key = match quoted.chars().next() {
                Some('"') => double_quoted(quoted),
                _ => single_quoted(quoted),
            };
            let rest = &after[colon + 1..];
            let ok = key.as_deref().is_some_and(|key| key != "<<")
                && (rest.is_empty() || rest.starts_with(SPACE));
            return Some((key.unwrap_or_else(|| quoted.to_string()), ok, rest));
        }
        let colon = key_colon(line)?;
        let key = line[..colon].trim_end_matches(SPACE);
        let ok = !key.is_empty()
            && !key.starts_with(INDICATORS)
            && key != "<<"
            && !key.contains(" #")
            && !key.contains("\t#");
        Some((key.to_string(), ok, &line[colon + 1..]))
    }

    /// The byte index of the quote that closes a quoted key at the start of `line`, or
    /// `None` when `line` does not start with a quote or the quote does not close.
    fn closing_quote(line: &str) -> Option<usize> {
        let quote = line.chars().next().filter(|c| matches!(c, '"' | '\''))?;
        let mut chars = line.char_indices().skip(1).peekable();
        while let Some((i, c)) = chars.next() {
            match (quote, c) {
                ('"', '\\') => {
                    chars.next();
                }
                ('\'', '\'') if chars.peek().is_some_and(|&(_, next)| next == '\'') => {
                    chars.next();
                }
                (q, c) if q == c => return Some(i),
                _ => {}
            }
        }
        None
    }

    fn build(lines: &[Line], index: &mut usize, parent_indent: isize) -> Node {
        let mut map = HashMap::new();
        if *index >= lines.len() {
            return Node::Map(map);
        }
        let child_indent = lines[*index].indent as isize;
        while *index < lines.len() {
            let indent = lines[*index].indent as isize;
            if indent <= parent_indent {
                break;
            }
            if indent != child_indent || lines[*index].kind != Kind::Entry {
                // A line deeper than expected without a parent, a sequence item no key
                // owns, or text that is no `key:` line (defensive skip).
                *index += 1;
                continue;
            }
            let key = lines[*index].key.clone();
            *index += 1;
            // A value that runs on: the next line is indented under this key, or is text
            // (no `key:`) inside this map. YAML folds such lines into a plain value, or
            // refuses the file; this reader reads neither, so the value is not exact.
            let runs_on = lines.get(*index).is_some_and(|next| {
                next.indent as isize > child_indent
                    || (next.kind == Kind::Text && next.indent as isize > parent_indent)
            });
            let node = match &lines[*index - 1].value {
                Value::Scalar(text) | Value::Null(text) | Value::Malformed(text) if runs_on => {
                    Node::Malformed(text.clone())
                }
                Value::Scalar(value) => Node::Scalar(value.clone()),
                Value::Null(_) => Node::Null,
                Value::Malformed(raw) => Node::Malformed(raw.clone()),
                Value::EmptyMap | Value::Open
                    if lines.get(*index).is_some_and(|next| {
                        next.kind == Kind::Item && next.indent as isize >= child_indent
                    }) =>
                {
                    // A block sequence: its items may sit at the key's own indent. Skip
                    // them, and anything nested under them.
                    let item_indent = lines[*index].indent;
                    while lines.get(*index).is_some_and(|l| {
                        (l.kind == Kind::Item && l.indent == item_indent) || l.indent > item_indent
                    }) {
                        *index += 1;
                    }
                    Node::Seq
                }
                Value::Open
                    if !lines
                        .get(*index)
                        .is_some_and(|next| next.indent as isize > child_indent) =>
                {
                    Node::Null
                }
                Value::EmptyMap | Value::Open => build(lines, index, child_indent),
            };
            // A key the map already has: Go refuses the whole file. A nested map keeps
            // the last one, as it always has; a value is no longer exact, so `user` and
            // `identity_file` fail closed rather than take whichever came last.
            let node = match (map.contains_key(&key), node) {
                (true, Node::Map(m)) => Node::Map(m),
                (true, other) => Node::Malformed(other.as_scalar().unwrap_or("").to_string()),
                (false, node) => node,
            };
            map.insert(key, node);
        }
        Node::Map(map)
    }
}

use yaml_lite::Node;

#[cfg(test)]
mod tests {
    use super::*;

    fn by_name<'a>(config: &'a ShedConfig, name: &str) -> &'a ShedServerEntry {
        config
            .servers
            .iter()
            .find(|s| s.name == name)
            .unwrap_or_else(|| panic!("no server {name}"))
    }

    #[test]
    fn parses_servers_and_default() {
        let yaml = "\
servers:
    mini2:
        host: mini2
        http_port: 8080
        ssh_port: 2222
        control_token: shed_control_abc123
        added_at: 2026-05-09T01:44:44.395385-05:00
    my-server:
        host: localhost
        http_port: 8080
        ssh_port: 2222
default_server: mini2
sheds: {}
";
        let config = ShedConfig::parse(yaml);
        assert_eq!(config.servers.len(), 2);
        assert_eq!(config.default_server.as_deref(), Some("mini2"));
        let mini2 = by_name(&config, "mini2");
        assert_eq!(mini2.host, "mini2");
        assert_eq!(mini2.http_port, 8080);
        assert_eq!(mini2.ssh_port, 2222);
        assert_eq!(mini2.control_token, "shed_control_abc123");
        // An entry without a token parses to an empty control_token.
        assert_eq!(by_name(&config, "my-server").control_token, "");
    }

    #[test]
    fn missing_file_is_empty_not_error() {
        assert_eq!(
            ShedConfig::load("/nonexistent/does-not-exist.yaml"),
            ShedConfig::default()
        );
    }

    #[test]
    fn empty_or_absent_servers_parse_to_zero_servers() {
        // A present-but-empty `servers`, an entirely-absent one, and an empty
        // document all yield an empty server list — a degraded-but-valid config
        // (the dashboard surfaces it), never a parse failure.
        assert!(ShedConfig::parse("servers: []\n").servers.is_empty());
        assert!(ShedConfig::parse("default_server: mini2\nsheds: {}\n")
            .servers
            .is_empty());
        assert!(ShedConfig::parse("").servers.is_empty());
    }

    #[test]
    fn default_server_absent_from_servers_still_parses() {
        // Validation ("does the default exist?") is deferred to the client, not the
        // parser: an unknown default_server parses fine and is surfaced verbatim.
        let yaml = "\
servers:
    mini2:
        host: mini2
        http_port: 8080
default_server: ghost
";
        let config = ShedConfig::parse(yaml);
        assert_eq!(config.servers.len(), 1);
        assert_eq!(config.default_server.as_deref(), Some("ghost"));
    }

    #[test]
    fn https_api_url_keeps_pin() {
        let e = ShedServerEntry {
            name: "localmac".into(),
            host: "localhost".into(),
            http_port: 8080,
            ssh_port: 2222,
            api_url: "https://localhost:8443".into(),
            tls_cert_fingerprint: "sha256:abc".into(),
            ..Default::default()
        };
        let r = e.resolved_endpoint();
        assert_eq!(r.base_url, "https://localhost:8443");
        assert_eq!(r.pin, "sha256:abc");
    }

    #[test]
    fn plain_http_fallback_when_no_api_url() {
        let e = ShedServerEntry {
            name: "mini2".into(),
            host: "mini2".into(),
            http_port: 8080,
            ssh_port: 2222,
            ..Default::default()
        };
        let r = e.resolved_endpoint();
        assert_eq!(r.base_url, "http://mini2:8080");
        assert_eq!(r.pin, "");
    }

    #[test]
    fn malformed_api_url_falls_back_to_plain_http() {
        // Relative path, wrong scheme, no host, unparseable, host-with-space —
        // all fall back to the plain endpoint.
        for bad in [
            "/api/info",
            "ftp://example.com:21",
            "https://",
            "not a url",
            "http://exa mple.com",
        ] {
            let e = ShedServerEntry {
                name: "s".into(),
                host: "h".into(),
                http_port: 8080,
                ssh_port: 2222,
                api_url: bad.into(),
                tls_cert_fingerprint: "sha256:x".into(),
                ..Default::default()
            };
            assert_eq!(
                e.resolved_endpoint().base_url,
                "http://h:8080",
                "api_url {bad:?} should fall back to the plain endpoint"
            );
        }
    }

    /// Parity backbone: the expected values for the shared
    /// `fixtures/config_sample.yaml`. Keep the two in lockstep.
    #[test]
    fn parity_fixture_matches_expected() {
        let config = ShedConfig::parse(include_str!("../../fixtures/config_sample.yaml"));
        assert_eq!(config.default_server.as_deref(), Some("mini2"));
        // Sorted by name (byte-wise: '2' (0x32) < 'm' (0x6d), so mini2 < minimal).
        let names: Vec<&str> = config.servers.iter().map(|s| s.name.as_str()).collect();
        assert_eq!(
            names,
            [
                "mini2",
                "minimal",
                "secure",
                "u-bare",
                "u-blockmap",
                "u-flowlist",
                "u-flowmap",
                "u-list",
                "u-listflat",
                "u-number",
                "u-true",
                "u-truequoted"
            ]
        );

        let mini2 = by_name(&config, "mini2");
        assert_eq!(mini2.host, "mini2");
        assert_eq!(mini2.http_port, 8080);
        assert_eq!(mini2.ssh_port, 2222);
        assert_eq!(mini2.control_token, "shed_control_abc123");
        assert_eq!(mini2.api_url, "");
        assert_eq!(mini2.tls_cert_fingerprint, "");
        assert_eq!(mini2.resolved_endpoint().base_url, "http://mini2:8080");
        assert_eq!(mini2.resolved_endpoint().pin, "");

        let secure = by_name(&config, "secure");
        assert_eq!(secure.host, "localhost");
        assert_eq!(secure.control_token, "");
        assert_eq!(secure.api_url, "https://localhost:8443");
        // Mixed-case pin lowercased.
        assert_eq!(secure.tls_cert_fingerprint, "sha256:aabbcc");
        assert_eq!(
            secure.resolved_endpoint().base_url,
            "https://localhost:8443"
        );
        assert_eq!(secure.resolved_endpoint().pin, "sha256:aabbcc");

        // `minimal: {}` → all defaults, host defaults to the server name, ssh_port 22.
        let minimal = by_name(&config, "minimal");
        assert_eq!(minimal.host, "minimal");
        assert_eq!(minimal.http_port, 8080);
        assert_eq!(minimal.ssh_port, 22);
        assert_eq!(minimal.resolved_endpoint().base_url, "http://minimal:8080");
    }

    /// A config carrying a legacy `machines:` section still parses cleanly,
    /// ignoring `machines:` without affecting `servers:`.
    #[test]
    fn legacy_machines_section_is_ignored_and_servers_are_unaffected() {
        let config = ShedConfig::parse(include_str!("../../fixtures/config_sample.yaml"));
        assert!(!config.servers.is_empty());
        assert_eq!(config.refused, None);
    }

    /// The mtls half of the same fixture (plan 001 D6).
    #[test]
    fn parity_fixture_carries_the_mtls_entry_fields() {
        use crate::token::AuthMode;
        let config = ShedConfig::parse(include_str!("../../fixtures/config_sample.yaml"));

        let secure = by_name(&config, "secure");
        assert_eq!(secure.auth_mode, "mtls");
        assert_eq!(secure.auth_mode(), AuthMode::Mtls);
        assert_eq!(
            secure.client_cert_file,
            "/Users/dev/.shed/creds/secure/client.crt"
        );
        assert_eq!(
            secure.client_key_file,
            "/Users/dev/.shed/creds/secure/client.key"
        );
        assert_eq!(secure.client_cert_expires_at, "2026-07-26T12:00:00Z");
        // The user the CLI recorded; an entry without the key parses to "".
        assert_eq!(secure.user, "pat");
        assert_eq!(by_name(&config, "mini2").user, "");
        assert_eq!(by_name(&config, "minimal").user, "");
        // A `user` that is not a plain string fails closed: never "" (no user). A flow
        // collection or a list is a line the whitelist does not accept, so the whole entry
        // is refused (plan 037 A8); a nested block mapping is read, and is no string.
        for (name, user) in [
            ("u-bare", ""),
            ("u-blockmap", USER_NOT_A_STRING),
            ("u-flowmap", USER_NOT_A_STRING),
            ("u-list", USER_NOT_A_STRING),
            ("u-listflat", USER_NOT_A_STRING),
            ("u-flowlist", USER_NOT_A_STRING),
            ("u-number", "5"),
        ] {
            assert_eq!(by_name(&config, name).user, user, "{name}");
            assert!(!crate::certuser::is_user_name(user), "{name}");
        }
        for name in ["u-flowmap", "u-flowlist", "u-list", "u-listflat"] {
            assert_eq!(
                by_name(&config, name).identity_file,
                crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE,
                "{name}"
            );
        }
        assert_eq!(by_name(&config, "u-blockmap").host, "um");
        // An unquoted `true` is read as its text, as Go's reader reads it (yaml.v3 into a
        // string field): it satisfies the user-name rule, so it is the user name `true`,
        // the same as the quoted value.
        for name in ["u-true", "u-truequoted"] {
            assert_eq!(by_name(&config, name).user, "true", "{name}");
            assert!(crate::certuser::is_user_name("true"), "{name}");
        }
        // The list's items are never read as the entry's next fields: the entry is not
        // read at all, so it has no host and no port either.
        let flat = by_name(&config, "u-listflat");
        assert_eq!((flat.host.as_str(), flat.ssh_port), ("", 0));
        assert!(secure.uses_tls());
        // It holds both credential files, so nothing to enroll.
        assert!(!secure.needs_enrollment());

        // ABSENT auth_mode is token, not "unknown" — the legacy-entry rule.
        let mini2 = by_name(&config, "mini2");
        assert_eq!(mini2.auth_mode, "");
        assert_eq!(mini2.auth_mode(), AuthMode::Token);
        assert!(!mini2.uses_tls()); // plain http entry → open server, never enrolls
        assert!(!mini2.needs_enrollment());
    }

    /// The "no usable credential ⇒ enroll" discrimination (Go `4553cc8` parity).
    #[test]
    fn needs_enrollment_discriminates_open_from_stripped_secure_entries() {
        let base = ShedServerEntry {
            name: "s".into(),
            host: "h".into(),
            http_port: 8080,
            ssh_port: 2222,
            control_token: String::new(),
            api_url: "https://h:8443".into(),
            tls_cert_fingerprint: "sha256:aa".into(),
            auth_mode: String::new(),
            client_cert_file: String::new(),
            client_key_file: String::new(),
            client_cert_expires_at: String::new(),
            user: String::new(),
            identity_file: String::new(),
        };

        // (a) an https entry stripped of every credential field by an older
        // client — the reproduced-live case — enrolls.
        assert!(base.needs_enrollment());

        // (b) the same entry with an mtls mode but no files still enrolls...
        let mtls_stripped = ShedServerEntry {
            auth_mode: "mtls".into(),
            ..base.clone()
        };
        assert!(mtls_stripped.needs_enrollment());

        // ...and does not once both files are present.
        let mtls_ready = ShedServerEntry {
            client_cert_file: "/creds/c.crt".into(),
            client_key_file: "/creds/c.key".into(),
            ..mtls_stripped.clone()
        };
        assert!(!mtls_ready.needs_enrollment());
        // A half-written pair is as unusable as none.
        assert!(ShedServerEntry {
            client_key_file: String::new(),
            ..mtls_ready.clone()
        }
        .needs_enrollment());

        // (c) a token entry that still holds its token does not enroll.
        assert!(!ShedServerEntry {
            control_token: "shed_control_x".into(),
            ..base.clone()
        }
        .needs_enrollment());

        // (d) an OPEN server (plain http, no pin) never enrolls, credential or
        // not — it must not pay an SSH round-trip to discover it needs nothing.
        assert!(!ShedServerEntry {
            api_url: String::new(),
            tls_cert_fingerprint: String::new(),
            ..base.clone()
        }
        .needs_enrollment());

        // (e) no way to reach SSH → nothing to enroll WITH.
        assert!(!ShedServerEntry {
            ssh_port: 0,
            ..base.clone()
        }
        .needs_enrollment());
        assert!(!ShedServerEntry {
            host: String::new(),
            ..base.clone()
        }
        .needs_enrollment());
    }

    #[test]
    fn auth_mode_is_lowercased_and_unknown_values_read_as_token() {
        use crate::token::AuthMode;
        let yaml = "\
servers:
    a:
        host: a
        auth_mode: MTLS
    b:
        host: b
        auth_mode: something-new
";
        let config = ShedConfig::parse(yaml);
        assert_eq!(by_name(&config, "a").auth_mode(), AuthMode::Mtls);
        // Forward-compat: an unrecognized mode is the shape whose fields are
        // actually populated (Go `sdk.Bundle.Mode` parity), never mtls.
        assert_eq!(by_name(&config, "b").auth_mode(), AuthMode::Token);
    }

    /// `user: null` and `user: ~` are no user, as Go's reader and the broker's discovery
    /// read them (they used to be kept as the text "null" and "~"); quoted, they are text.
    #[test]
    fn a_null_user_is_no_user() {
        let yaml = "\
servers:
    a:
        user: null
    b:
        user: ~
    c:
        user: NULL
    d:
        user: \"null\"
    e:
        user: 'pat' # a comment
    f:
        user: \"pat
";
        let config = ShedConfig::parse(yaml);
        for name in ["a", "b", "c"] {
            assert_eq!(by_name(&config, name).user, "", "{name}");
        }
        assert_eq!(by_name(&config, "d").user, "null");
        assert_eq!(by_name(&config, "e").user, "pat");
        // A quoted value that never closes fails closed, never as no user.
        assert_eq!(by_name(&config, "f").user, USER_NOT_A_STRING);
    }

    /// The scalar forms `identity_file` arrives in (plan 037 A8): what the Go CLI's saver
    /// writes and what a person types, read as YAML reads them.
    #[test]
    fn identity_file_scalars_read_as_yaml_reads_them() {
        use crate::identityfile::IDENTITY_FILE_UNREADABLE;
        let yaml = "\
servers:
    plain:
        identity_file: /home/pat/.ssh/id_work
    hash-in-word:
        identity_file: /home/pat/.ssh/id#work
    comment:
        identity_file: /home/pat/.ssh/id_work  # the work key
    tab-comment:
        identity_file: /home/pat/.ssh/id_work\t# the work key
    double:
        identity_file: \"/home/pat/.ssh/id\\twork\"
    double-hash:
        identity_file: \"/home/pat/.ssh/id#work\" # text inside quotes
    escapes:
        identity_file: \"\\0\\a\\b\\v\\f\\r\\n\\e\\ \\\"\\/\\\\\\N\\_\\L\\P\\x41\\u00e9\\U0001F600\"
    single:
        identity_file: '/home/pat/.ssh/pat''s key'
    single-backslash:
        identity_file: '/home/pat/.ssh/id\\work'
    nbsp-trailing:
        identity_file: /home/pat/.ssh/id_work\u{a0}
    null:
        identity_file: null
    tilde:
        identity_file: ~
    bare:
        identity_file:
    absent:
        host: a
    list:
        identity_file:
            - /home/pat/.ssh/id_work
    map:
        identity_file: {}
    nested:
        identity_file:
            path: /home/pat/.ssh/id_work
    unclosed:
        identity_file: \"/home/pat/.ssh/id_work
    bad-escape:
        identity_file: \"/home/pat/.ssh/\\q\"
    trailing-text:
        identity_file: \"/home/pat/.ssh/id_work\" more
";
        use crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE;
        let config = ShedConfig::parse(yaml);
        let got = |name: &str| by_name(&config, name).identity_file.clone();
        assert_eq!(got("plain"), "/home/pat/.ssh/id_work");
        assert_eq!(got("hash-in-word"), "/home/pat/.ssh/id#work");
        assert_eq!(got("comment"), "/home/pat/.ssh/id_work");
        assert_eq!(got("tab-comment"), "/home/pat/.ssh/id_work");
        assert_eq!(got("double"), "/home/pat/.ssh/id\twork");
        assert_eq!(got("double-hash"), "/home/pat/.ssh/id#work");
        assert_eq!(
            got("escapes"),
            "\0\u{7}\u{8}\u{b}\u{c}\r\n\u{1b} \"/\\\u{85}\u{a0}\u{2028}\u{2029}A\u{e9}\u{1F600}"
        );
        assert_eq!(got("single"), "/home/pat/.ssh/pat's key");
        assert_eq!(got("single-backslash"), "/home/pat/.ssh/id\\work");
        assert_eq!(got("nbsp-trailing"), "/home/pat/.ssh/id_work\u{a0}");
        for name in ["null", "tilde", "bare", "absent"] {
            assert_eq!(got(name), "", "{name}");
        }
        // A nested mapping is a form the reader understands, but no string: the value
        // fails closed, the entry is read.
        assert_eq!(got("nested"), IDENTITY_FILE_UNREADABLE);
        // Every other shape is a line the whitelist does not accept: the whole entry.
        for name in ["list", "map", "unclosed", "bad-escape", "trailing-text"] {
            assert_eq!(got(name), IDENTITY_FILE_ENTRY_UNREADABLE, "{name}");
        }
    }

    /// A key is decoded as a value is: a quoted `identity_file` key, escapes and all, is the
    /// field (Go reads it as the field; reading it as an unknown key would run ssh with no
    /// `-i`).
    #[test]
    fn a_quoted_key_is_decoded_as_a_value_is() {
        let yaml = "\
servers:
    escaped:
        \"identity\\u005ffile\": /home/pat/.ssh/id_work
        \"us\\x65r\": pat
    single:
        'identity_file' : /home/pat/.ssh/id_single
    quoted-colon:
        \"a:b\": x
        identity_file: /home/pat/.ssh/id_after
";
        let config = ShedConfig::parse(yaml);
        assert_eq!(
            by_name(&config, "escaped").identity_file,
            "/home/pat/.ssh/id_work"
        );
        assert_eq!(by_name(&config, "escaped").user, "pat");
        assert_eq!(
            by_name(&config, "single").identity_file,
            "/home/pat/.ssh/id_single"
        );
        // A colon inside a quoted key does not end it.
        assert_eq!(
            by_name(&config, "quoted-colon").identity_file,
            "/home/pat/.ssh/id_after"
        );
    }

    /// A value that runs on to a following line indented under its key is not cut at its
    /// first line (YAML folds the lines into one value, or refuses the file): the line is
    /// a continuation, which the whitelist does not accept, so the whole entry is refused,
    /// whichever field it follows. A comment under a value is no continuation.
    #[test]
    fn a_value_that_runs_on_is_not_cut_at_its_first_line() {
        use crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE;
        let yaml = "\
servers:
    folded:
        host: f.example
        identity_file: /home/pat/.ssh/first
          second
        user: pat
          more
    nested:
        identity_file: /home/pat/.ssh/first
            port: 1
    null-runs-on:
        identity_file: null
          /home/pat/.ssh/id_work
        user: ~
          pat
    next-line:
        identity_file:
            /home/pat/.ssh/id_work
        user:
            pat
    text-at-key-indent:
        identity_file: /home/pat/.ssh/first
        second
    host-runs-on:
        host: h.example
          tail
    fine:
        identity_file: /home/pat/.ssh/id_work
        # a comment under it is not a continuation
        user: pat
";
        let config = ShedConfig::parse(yaml);
        for name in [
            "folded",
            "nested",
            "null-runs-on",
            "next-line",
            "text-at-key-indent",
            "host-runs-on",
        ] {
            let entry = by_name(&config, name);
            assert_eq!(
                entry.identity_file, IDENTITY_FILE_ENTRY_UNREADABLE,
                "{name}"
            );
            assert_eq!(entry.user, USER_NOT_A_STRING, "{name}");
            // Refused before any field is read: not the first line of a folded host.
            assert_eq!(entry.host, "", "{name}");
        }
        assert_eq!(
            by_name(&config, "fine").identity_file,
            "/home/pat/.ssh/id_work"
        );
        assert_eq!(by_name(&config, "fine").user, "pat");
    }

    /// A tag, an anchor, an alias, a block scalar, a flow collection or a mapping inside a
    /// plain value: YAML reads something else than the text (`!!str null` is the string
    /// "null", not no value), so the line is not one the whitelist accepts and the whole
    /// entry is refused, whichever field it is in (`host` included: it is never read as
    /// its text either).
    #[test]
    fn a_value_with_a_tag_or_an_anchor_is_not_exact() {
        use crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE;
        let mut yaml = String::from("servers:\n");
        let values = [
            "!!str null",
            "!!str /home/pat/.ssh/id_work",
            "&key /home/pat/.ssh/id_work",
            "*key",
            "|",
            ">-",
            "{a: b}",
            "/home/pat/.ssh/id: work",
            "/home/pat/.ssh/id:",
            "!!str \"pat\"",
            "[pat]",
            "{}",
            "- pat",
            "-",
            "? pat",
            "%pat",
            "@pat",
            "`pat",
            ", pat",
        ];
        for (i, value) in values.iter().enumerate() {
            // Each value in one field only, the others plain: the one line refuses all.
            for (field, other) in [
                ("host", "identity_file"),
                ("identity_file", "user"),
                ("user", "host"),
            ] {
                yaml.push_str(&format!(
                    "    e{i}-{field}:\n        {field}: {value}\n        {other}: plain\n"
                ));
            }
        }
        let config = ShedConfig::parse(&yaml);
        for (i, value) in values.iter().enumerate() {
            for field in ["host", "identity_file", "user"] {
                let entry = by_name(&config, &format!("e{i}-{field}"));
                assert_eq!(
                    entry.identity_file, IDENTITY_FILE_ENTRY_UNREADABLE,
                    "{field}: {value}"
                );
                assert_eq!(entry.user, USER_NOT_A_STRING, "{field}: {value}");
                assert_eq!(entry.host, "", "{field}: {value}");
            }
        }
    }

    /// **The whitelist decides before any field is read** (plan 037 A8): the review's
    /// shapes that once read as an entry with NO identity file (an explicit `?` key and
    /// `:` value, the fields as a flow mapping on the following line) and the one that
    /// was dropped silently (the entry as a flow mapping on its own line) are each the
    /// whole entry refused, by name, with no host fallback and no user; and an entry of
    /// plain lines, nested mappings included, is still read.
    #[test]
    fn an_entry_is_read_only_when_every_line_is_understood() {
        use crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE;
        let yaml = "\
servers:
    explicit:
        host: prod.example
        ? identity_file
        : /keys/prod
    explicit-first:
        ? identity_file
        : /keys/prod
        host: prod.example
    flow-next-line:
        {host: prod.example, identity_file: /keys/prod}
    inline: {host: prod.example, identity_file: /keys/prod}
    scalar-entry: prod.example
    list-entry:
        - host
    tab-indent:
        host: prod.example
\t\tidentity_file: /keys/prod
    colon-in-key:
        host: prod.example
        a:b: c
    no-space-after-colon:
        host: prod.example
        identity_file:/keys/prod
    dedent-between:
        host: prod.example
          extra:
         x: y
    empty-map-with-fields: {}
        host: prod.example
    fine:
        host: prod.example   # the host
        'identity_file': \"/keys/prod\"
        user: pat
        extra:
            nested: value
            deeper:
                a: b
        ssh_port: 2222
    bare-entry:
    empty-entry: {}
    fine-2:
        host: other.example
";
        let config = ShedConfig::parse(yaml);
        for name in [
            "explicit-first",
            "explicit",
            "flow-next-line",
            "inline",
            "scalar-entry",
            "list-entry",
            "tab-indent",
            "no-space-after-colon",
            "dedent-between",
            "empty-map-with-fields",
        ] {
            let entry = by_name(&config, name);
            assert_eq!(
                *entry,
                ShedServerEntry {
                    name: name.to_string(),
                    user: USER_NOT_A_STRING.to_string(),
                    identity_file: IDENTITY_FILE_ENTRY_UNREADABLE.to_string(),
                    ..Default::default()
                },
                "{name}"
            );
            let err =
                crate::identityfile::check_entry_file(name, &entry.identity_file).unwrap_err();
            assert_eq!(
                err.to_string(),
                format!(
                    "entry {name:?} cannot be used: {}",
                    crate::identityfile::ValueRule::EntryUnreadable.reason()
                )
            );
        }
        let fine = by_name(&config, "fine");
        assert_eq!(fine.host, "prod.example");
        assert_eq!(fine.identity_file, "/keys/prod");
        assert_eq!(fine.user, "pat");
        assert_eq!(fine.ssh_port, 2222);
        // `a:b: c` is the key `a:b` (a field this reader does not use), as in YAML.
        assert_eq!(by_name(&config, "colon-in-key").host, "prod.example");
        // No entry is made of a line inside another one (the tab-indented
        // `identity_file`, the `? identity_file` key).
        assert!(config
            .servers
            .iter()
            .all(|s| s.name != "identity_file" && !s.name.is_empty()));
        // A bare entry and an empty one are every default, as they always were.
        for name in ["bare-entry", "empty-entry"] {
            let entry = by_name(&config, name);
            assert_eq!((entry.host.as_str(), entry.ssh_port), (name, 22));
            assert_eq!(entry.identity_file, "");
        }
        assert_eq!(by_name(&config, "fine-2").host, "other.example");
    }

    /// **A plain key ends at the first colon followed by a space, a tab or the end of the
    /// line** (plan 037 A8, third review, finding 3): the entry yaml.v3 writes for the name
    /// `prod:blue` (byte for byte the review's output) is read under its own name with its
    /// own fields, as are a name with two colons and an address with a port; `a:b: c`
    /// inside an entry is the key `a:b`. A key followed by a no-break space before its
    /// colon is not the key `host`.
    #[test]
    fn a_plain_key_ends_at_a_colon_followed_by_a_space() {
        let yaml = "\
servers:
    prod:blue:
        host: prod.example
        ssh_port: 22
        added_at: 0001-01-01T00:00:00Z
    a:b:c:
        host: abc.example
        user: pat
        identity_file: /keys/abc
    10.0.0.5:8443:
        host: 10.0.0.5
        a:b: c
        ssh_port: 2222
    nbsp:
        host\u{a0}: other.example
        ssh_port: 2222
default_server: \"\"
sheds: {}
";
        let config = ShedConfig::parse(yaml);
        let names: Vec<&str> = config.servers.iter().map(|s| s.name.as_str()).collect();
        assert_eq!(names, ["10.0.0.5:8443", "a:b:c", "nbsp", "prod:blue"]);
        let blue = by_name(&config, "prod:blue");
        assert_eq!((blue.host.as_str(), blue.ssh_port), ("prod.example", 22));
        assert_eq!(blue.identity_file, "");
        let abc = by_name(&config, "a:b:c");
        assert_eq!(abc.host, "abc.example");
        assert_eq!(abc.user, "pat");
        assert_eq!(abc.identity_file, "/keys/abc");
        let addr = by_name(&config, "10.0.0.5:8443");
        assert_eq!((addr.host.as_str(), addr.ssh_port), ("10.0.0.5", 2222));
        // `host\u{a0}` is a key of its own, not `host`: the entry has no `host`, and reads
        // as an entry with none does.
        let nbsp = by_name(&config, "nbsp");
        assert_eq!((nbsp.host.as_str(), nbsp.ssh_port), ("nbsp", 2222));
    }

    /// **Only the first YAML document is read** (finding 1): a second document can neither
    /// replace an entry, add one, nor remove one; a `---` before content and a `...` at the
    /// end change nothing.
    #[test]
    fn only_the_first_document_is_read() {
        let first = "servers:\n  prod:\n    host: prod.example\n    user: alice\n    \
                     identity_file: /keys/prod\n";
        for (shape, yaml) in [
            (
                "a second document",
                format!("{first}---\nservers:\n  prod:\n    host: other.example\n  extra:\n    host: x\n"),
            ),
            (
                "a document end, then more",
                format!("{first}...\nservers:\n  prod:\n    host: other.example\n"),
            ),
            (
                "a second document with a comment on its marker",
                format!("{first}--- # two\nservers:\n  prod:\n    host: other.example\n"),
            ),
            ("framed", format!("---\n{first}...\n")),
            ("framed, CRLF", format!("---\r\n{first}...\r\n").replace("\n    ", "\r\n    ")),
        ] {
            let config = ShedConfig::parse(&yaml);
            assert_eq!(config.refused, None, "{shape}");
            let names: Vec<&str> = config.servers.iter().map(|s| s.name.as_str()).collect();
            assert_eq!(names, ["prod"], "{shape}");
            let prod = by_name(&config, "prod");
            assert_eq!(prod.host, "prod.example", "{shape}");
            assert_eq!(prod.user, "alice", "{shape}");
            assert_eq!(prod.identity_file, "/keys/prod", "{shape}");
        }
    }

    /// **A file is refused whole** when this reader would read its entries differently
    /// from Go in a way no entry refusal catches (finding 2 and the review's table): a raw
    /// line break it does not split on (each of the four inside an apparent comment, the
    /// review's input), a second `servers:`, `servers:` in flow form or with a tag or an
    /// anchor, or content on the first `---`. Nothing is read then. Escaped forms inside a
    /// double-quoted value are text, and a CRLF file is read as usual.
    #[test]
    fn a_file_read_otherwise_by_go_is_refused_whole() {
        let entry = |br: &str| {
            format!(
                "servers:\n  prod:\n    host: prod.example\n    # note{br}    identity_file: \
                 /keys/prod\nmachines:\n  box:\n    host: box\ndefault_server: prod\n"
            )
        };
        let base = "servers:\n  prod:\n    host: prod.example\n    identity_file: /keys/prod\n";
        let rows: Vec<(String, &str)> = vec![
            (entry("\r"), "carriage return"),
            (entry("\u{85}"), "U+0085"),
            (entry("\u{2028}"), "U+2028"),
            (entry("\u{2029}"), "U+2029"),
            (format!("{base}\r"), "carriage return"),
            (
                format!("{base}servers:\n  prod:\n    host: other.example\n"),
                "more than once",
            ),
            (
                "servers: {prod: {host: prod.example, identity_file: /keys/prod}}\n".to_string(),
                "not a block",
            ),
            ("servers: []\n".to_string(), "not a block"),
            (base.replace("servers:", "servers: &s"), "not a block"),
            (base.replace("servers:", "servers: !!map"), "not a block"),
            (base.replace("servers:", "!!str servers:"), "tag"),
            (base.replace("servers:", "&s servers:"), "tag"),
            (
                "{servers: {prod: {host: prod.example}}}\n".to_string(),
                "flow form",
            ),
            (format!("--- !!map\n{base}"), "`---` line"),
            (
                "servers:\n\t\tprod:\n    host: prod.example\n".to_string(),
                "tab",
            ),
        ];
        for (yaml, why) in rows {
            let config = ShedConfig::parse(&yaml);
            let reason = config
                .refused
                .clone()
                .unwrap_or_else(|| panic!("{yaml:?} is read: {config:?}"));
            assert!(reason.contains(why), "{yaml:?}: {reason}");
            assert_eq!(
                config,
                ShedConfig {
                    refused: Some(reason),
                    ..ShedConfig::default()
                },
                "{yaml:?}"
            );
        }
        // The escaped forms are text inside a quoted value (the value rule refuses them
        // in `identity_file` at use), and CRLF line endings are line endings.
        for yaml in [
            "servers:\n  prod:\n    host: prod.example\n    identity_file: \"/k\\r\\N\\L\\P\"\n"
                .to_string(),
            base.replace('\n', "\r\n"),
            "servers:\n  prod:\n    host: \"prod.example\"\n\"default_server\": prod\n".to_string(),
        ] {
            let config = ShedConfig::parse(&yaml);
            assert_eq!(config.refused, None, "{yaml:?}");
            assert_eq!(by_name(&config, "prod").host, "prod.example");
        }
    }

    /// A UTF-8 byte-order mark at the start is skipped (the review's table: it used to
    /// hide every server).
    #[test]
    fn a_byte_order_mark_is_skipped() {
        let config = ShedConfig::parse(
            "\u{feff}servers:\n  prod:\n    host: prod.example\n    identity_file: /keys/prod\n",
        );
        assert_eq!(config.refused, None);
        let prod = by_name(&config, "prod");
        assert_eq!(prod.host, "prod.example");
        assert_eq!(prod.identity_file, "/keys/prod");
    }

    /// **A tab in an entry's indentation refuses that entry, cleanly** (the review's
    /// table): the entry is not read without the field the line held, no entry is made of
    /// that line, and the entries around it are read.
    #[test]
    fn a_tab_indented_line_refuses_its_entry_cleanly() {
        use crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE;
        let yaml = "\
servers:
  before:
    host: before.example
  prod:
    host: prod.example
    user: alice
\t\tidentity_file: /keys/prod
  header-tab:
\t\thost: tab.example
    user: alice
  after:
    host: after.example
";
        let config = ShedConfig::parse(yaml);
        assert_eq!(config.refused, None);
        let names: Vec<&str> = config.servers.iter().map(|s| s.name.as_str()).collect();
        assert_eq!(names, ["after", "before", "header-tab", "prod"]);
        for name in ["prod", "header-tab"] {
            let entry = by_name(&config, name);
            assert_eq!(
                entry.identity_file, IDENTITY_FILE_ENTRY_UNREADABLE,
                "{name}"
            );
            assert_eq!(entry.host, "", "{name}");
        }
        assert_eq!(by_name(&config, "before").host, "before.example");
        assert_eq!(by_name(&config, "after").host, "after.example");
    }

    /// An entry whose name yaml.v3 writes as an explicit key (`? name` then `: …`, which it
    /// does for a name over 128 bytes) is refused under that name, and the entry before it
    /// is read.
    #[test]
    fn an_explicit_key_entry_is_refused_by_its_name() {
        use crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE;
        let long = "x".repeat(130);
        let yaml = format!(
            "servers:\n    before:\n        host: before.example\n    ? {long}\n    :   host: \
             b.example\n        ssh_port: 0\n    after:\n        host: after.example\n"
        );
        let config = ShedConfig::parse(&yaml);
        let names: Vec<&str> = config.servers.iter().map(|s| s.name.as_str()).collect();
        assert_eq!(names, ["after", "before", long.as_str()]);
        let entry = by_name(&config, &long);
        assert_eq!(entry.identity_file, IDENTITY_FILE_ENTRY_UNREADABLE);
        assert_eq!(entry.host, "");
        assert_eq!(by_name(&config, "before").host, "before.example");
        assert_eq!(by_name(&config, "after").host, "after.example");
    }

    /// **The promise** (plan 037 A8): a config.yaml written by the shed CLI's own save
    /// path, from real structs (`crates/fixtures/clientconfig/`, written and kept fresh by
    /// `internal/config/clientconfig_fixture_test.go`), is read with every server's host,
    /// ssh_port, user and identity_file as the CLI holds them, every entry read (none
    /// refused), and its default server as the CLI reads it. A legacy `machines:`
    /// section in the file is ignored here; the CLI's save preserves it opaquely.
    #[test]
    fn the_cli_written_config_is_read_as_the_cli_reads_it() {
        let config = ShedConfig::parse(include_str!("../../fixtures/clientconfig/config.yaml"));
        let expected: serde_json::Value =
            serde_json::from_str(include_str!("../../fixtures/clientconfig/expected.json"))
                .unwrap();
        assert_eq!(config.refused, None);
        let want = expected["servers"].as_array().unwrap();
        assert_eq!(config.servers.len(), want.len());
        for w in want {
            let name = w["name"].as_str().unwrap();
            let got = by_name(&config, name);
            assert_eq!(got.host, w["host"].as_str().unwrap(), "{name} host");
            assert_eq!(
                u64::from(got.ssh_port),
                w["ssh_port"].as_u64().unwrap(),
                "{name} ssh_port"
            );
            assert_eq!(got.user, w["user"].as_str().unwrap(), "{name} user");
            assert_eq!(
                got.identity_file,
                w["identity_file"].as_str().unwrap(),
                "{name} identity_file"
            );
        }
        assert_eq!(
            config.default_server.as_deref(),
            expected["default_server"].as_str()
        );
    }

    /// An explicit null host (`host: null`, `host: ~`, a bare `host:`) is no destination:
    /// the entry is refused, never sent to a host named after the entry (an absent `host`
    /// still is the entry's name) nor to one named "null". A quoted "null" is text.
    #[test]
    fn a_null_host_refuses_the_entry() {
        use crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE;
        let yaml = "\
servers:
    null-host:
        host: null
    tilde-host:
        host: ~
    bare-host:
        host:
        ssh_port: 2222
    nested-host:
        host:
            name: prod.example
    absent:
        ssh_port: 2222
    quoted:
        host: \"null\"
";
        let config = ShedConfig::parse(yaml);
        for name in ["null-host", "tilde-host", "bare-host", "nested-host"] {
            let entry = by_name(&config, name);
            assert_eq!(entry.host, "", "{name}");
            assert_eq!(
                entry.identity_file, IDENTITY_FILE_ENTRY_UNREADABLE,
                "{name}"
            );
            assert_eq!(entry.user, USER_NOT_A_STRING, "{name}");
        }
        assert_eq!(by_name(&config, "absent").host, "absent");
        assert_eq!(by_name(&config, "quoted").host, "null");
    }

    /// An entry this reader cannot read exactly as a whole: a merge key (its merged
    /// mapping could carry `identity_file` or `user`), or a key with a tag, an anchor or an
    /// alias, or a quoted key that does not close, or a key repeated in the entry (Go
    /// refuses the file). Every field fails closed whatever the entry says, and the
    /// refusal says to write the fields one per line.
    #[test]
    fn an_entry_that_cannot_be_read_exactly_is_refused() {
        use crate::identityfile::IDENTITY_FILE_ENTRY_UNREADABLE;
        let yaml = "\
servers:
    merged:
        <<: {identity_file: /home/pat/.ssh/id_work}
        host: m.example
    merged-quoted:
        \"<<\": x
        identity_file: /home/pat/.ssh/id_work
        user: pat
    tagged-key:
        !!str identity_file: /home/pat/.ssh/id_work
    anchored-key:
        &k user: pat
    unclosed-key:
        \"identity_file: /home/pat/.ssh/id_work
    twice:
        identity_file: /home/pat/.ssh/id_a
        identity_file: /home/pat/.ssh/id_b
        user: pat
        user: admin
";
        let config = ShedConfig::parse(yaml);
        for name in [
            "merged",
            "merged-quoted",
            "tagged-key",
            "anchored-key",
            "unclosed-key",
            "twice",
        ] {
            let entry = by_name(&config, name);
            assert_eq!(
                entry.identity_file, IDENTITY_FILE_ENTRY_UNREADABLE,
                "{name}"
            );
            assert_eq!(entry.user, USER_NOT_A_STRING, "{name}");
            assert_eq!(entry.host, "", "{name}: no field is read, the host neither");
            let err =
                crate::identityfile::check_entry_file(name, &entry.identity_file).unwrap_err();
            assert!(
                err.to_string()
                    .ends_with("write its fields one per line, as key: value"),
                "{err}"
            );
        }
    }
}
