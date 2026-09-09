//! **The credential seam** — where gx's bearer token comes from, and the checks
//! that decide whether it may be read at all.
//!
//! The agent-lane contract has no credential type and will not grow one
//! ([`shed_core::lane`], correction 5): roost carries only WHERE an agent is,
//! never HOW to be let in. So the adapter's crate defines the SOURCE
//! ([`GxCredentialSource`]) and the client — the desktop today, the phone next —
//! decides how to read it. This module is everything both readers share:
//!
//! - [`PROBE_SCRIPT`], one POSIX `sh -c` string, for the far side of an SSH
//!   reach;
//! - [`parse_probe`], which reads its output back;
//! - [`token_file_refusal`] and [`read_token_file`], the LOCAL reader's checks —
//!   pure functions over facts, so the Tauri reader (C4) calls them instead of
//!   re-deriving what "eligible" means;
//! - [`GxToken`], which exists so a token cannot be printed by accident.
//!
//! # Two URLs, and this module only ever sees one of them
//!
//! A gx lane has a **reported** URL (roost's `gx.remote`, and the discovery
//! record's own `url`) and a **dial** URL (where HTTP actually goes — the same
//! address locally, a forwarded `127.0.0.1:<local>` over SSH). They are never
//! conflated. Discovery matches the **reported** one, through
//! [`same_reported_url`]; the dial URL belongs to [`crate::transport`] and is
//! never compared with a record.
//!
//! # What the real artifacts look like
//!
//! Read off a live `gx 1.0.16+gx.12` leader, because three of these are the
//! difference between a parser that works and one that refuses every real
//! token:
//!
//! - the record is **pretty-printed, multi-line** JSON, so the probe's `---`
//!   delimiter has to be line-oriented;
//! - the token file is **65 bytes** — 64 lowercase hex plus a trailing newline —
//!   so the hex check runs on the TRIMMED text or every real token is refused as
//!   malformed;
//! - the record's filename is `gx-remote-<16 hex>.json` whenever the leader is
//!   not on the default socket (a scratch leader, any non-default deployment),
//!   so the `gx-remote*.json` glob is the PRIMARY path, not a fallback after
//!   `gx-remote.json`;
//! - the **token is not suffixed even when the record is** — one token per
//!   `$GROK_HOME`, shared by every leader on it. The token's path comes from the
//!   record's own `tokenFile`, or from [`DEFAULT_TOKEN_FILE`] under the home; it
//!   is NEVER derived from the record's filename.
//! - the record's `url` is `"http://127.0.0.1:2431"` — **no trailing slash**,
//!   which is exactly what [`shed_core::roost::loopback_base_url`] accepts (it
//!   rejects one).

use std::fmt;
use std::path::{Path, PathBuf};

use shed_core::lane::LaneError;
use shed_core::roost::loopback_base_url;

/// The default filename of the shared token under `$GROK_HOME`.
///
/// Shared by every leader on that home — a leader restart changes its
/// `instanceId`, never the token — which is why it carries no per-leader suffix
/// even when the RECORD does.
pub const DEFAULT_TOKEN_FILE: &str = "gx-remote.token";

/// `$GROK_HOME`'s default, relative to the caller's home directory.
pub const DEFAULT_GROK_HOME: &str = ".grok";

/// The record glob's prefix and suffix: `gx-remote*.json`, which matches both
/// the default-socket `gx-remote.json` and the suffixed
/// `gx-remote-<16 hex>.json` a non-default socket writes.
pub const RECORD_PREFIX: &str = "gx-remote";
/// See [`RECORD_PREFIX`].
pub const RECORD_SUFFIX: &str = ".json";

/// A gx token's grammar, in one place: exactly [`TOKEN_HEX_LEN`] LOWERCASE hex
/// digits.
///
/// [`GxToken::parse`] is not its only reader — [`crate::client::redact_hex64`],
/// the belt to this type's braces, must recognize EXACTLY what `parse` accepts
/// or the redactor quietly stops matching the thing it exists to backstop. Two
/// spellings of "lowercase hex" is how those two drift.
pub(crate) const TOKEN_HEX_LEN: usize = 64;

/// See [`TOKEN_HEX_LEN`]. Uppercase is deliberately absent: gx mints lowercase,
/// and folding another spelling in would mean this reader and gx's disagree
/// about what a token is.
pub(crate) fn is_lower_hex(b: u8) -> bool {
    matches!(b, b'0'..=b'9' | b'a'..=b'f')
}

/// The line the probe prints after each discovery record.
pub const PROBE_RECORD_DELIMITER: &str = "---";
/// The line the probe prints before the token — and prints whether or not a
/// token follows, so "no eligible token" and "the probe died early" are
/// distinguishable.
pub const PROBE_TOKEN_SENTINEL: &str = "===token===";

/// The remote half of discovery: one POSIX `sh -c` string, run on the host that
/// runs gx.
///
/// # The four rules it exists to keep
///
/// 1. **It always exits 0 and reports in band.** A non-zero exit over SSH is
///    indistinguishable from ssh's own failures, and
///    `shed_app::machine::exec` builds its error string from the remote's
///    stdout when stderr is empty — which is precisely where the token would
///    be. Reporting in band means a failed probe never has a reason to quote
///    what the remote printed.
/// 2. **The token is printed LAST**, after everything that can fail. Nothing
///    after it can turn into an error message that carries it.
/// 3. **`find` both checks AND reads**, in one invocation:
///    `find "$t" -prune -type f -perm 0600 -user "$(id -un)" -exec cat {} \;`
///    checks the three things gx's own reader checks — `-type f` under find's
///    default `-P` is false for a symlink, `-perm 0600` is an EXACT mode match
///    (no `-`/`/` prefix), and `-user` is the owner — and `-exec` runs only if
///    all of them held. A file that fails any of them prints nothing, and the
///    sentinel above it says the probe still ran.
///
///    **`-prune`, not `-maxdepth`.** `-maxdepth` is a GNU/BSD extension: a
///    strictly POSIX `find` errors on it, and the probe would then silently
///    yield no token at all — a hard failure of the SSH path on any host whose
///    `find` is strict. `-prune` is POSIX, evaluates true, and stops descent,
///    which is all that is wanted for a single-file argument.
///
///    **Residual, stated because it is not zero.** Folding the read into `find`
///    removes the wide window the previous shape had — a `find` that only
///    tested, a command substitution, a `[ -n … ]`, and then a *separate* `cat`
///    process resolving the path a second time. What remains is that `-exec cat
///    {}` hands `cat` a PATHNAME, so the bytes are opened once more inside
///    find's own exec. POSIX `find` has no read-through-descriptor primitive, so
///    that last window cannot be closed from a shell script; it is sub-process
///    -spawn wide, and the trust boundary here is already "same UID on that
///    host", the same one roost's socket has.
/// 4. **No single quote appears in it.** The script crosses SSH as one
///    shell-quoted argument (`shed_core::rc_agents::shell_quote_always`
///    single-quote-wraps and rewrites `'` as `'\''`), which survives a quote
///    fine — but a golden that pins this string, in `tests/machine-transport`'s
///    `gx-probe` scenario and in shed-mobile's Dart composer after it, is worth
///    keeping legible.
///
/// The `[ -f "$f" ]` guard is what makes an unmatched glob (which POSIX sh
/// leaves as the literal pattern) print nothing instead of a `cat` error.
pub const PROBE_SCRIPT: &str = r#"h=${GROK_HOME:-$HOME/.grok}
for f in "$h"/gx-remote*.json; do
  [ -f "$f" ] || continue
  cat "$f" 2>/dev/null
  printf "\n---\n"
done
printf "===token===\n"
t="$h/gx-remote.token"
find "$t" -prune -type f -perm 0600 -user "$(id -un)" -exec cat {} \; 2>/dev/null
exit 0
"#;

// ---------------------------------------------------------------------------
// the token
// ---------------------------------------------------------------------------

/// gx's bearer token, in the one type that cannot print it.
///
/// **No `Debug` derive, no `Display`, no `Serialize`.** [`fmt::Debug`] is
/// implemented by hand to print `GxToken(<redacted>)`, so every struct that
/// holds one — [`GxDiscovery`], [`crate::GxClient`] — can derive `Debug` and
/// still be safe to log. [`GxToken::authorization`] marks the header value
/// `set_sensitive(true)`, which is what keeps it out of reqwest's own tracing.
///
/// The only way to see the secret is [`GxToken::expose`], which is named to be
/// greppable.
#[derive(Clone, PartialEq, Eq)]
pub struct GxToken(String);

impl fmt::Debug for GxToken {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("GxToken(<redacted>)")
    }
}

impl GxToken {
    /// Parse a token, **trimming surrounding whitespace first**.
    ///
    /// The trim is not politeness: gx writes the file with a trailing newline
    /// (65 bytes for a 64-hex token, verified on a live leader), so a validator
    /// that ran on the raw bytes would refuse every real token.
    ///
    /// The grammar is exactly 64 LOWERCASE hex digits. Uppercase is refused
    /// rather than folded — gx mints lowercase, and quietly accepting another
    /// spelling would mean this reader and gx's disagree about what a token is.
    pub fn parse(raw: &str) -> Option<GxToken> {
        let t = raw.trim();
        let ok = t.len() == TOKEN_HEX_LEN && t.bytes().all(is_lower_hex);
        ok.then(|| GxToken(t.to_string()))
    }

    /// The secret itself. Named so `grep -rn expose` finds every place a token
    /// leaves this type.
    pub fn expose(&self) -> &str {
        &self.0
    }

    /// The `Authorization` header value, marked **sensitive** so reqwest and
    /// hyper never log it.
    ///
    /// Infallible in practice — the value is `Bearer ` plus 64 hex digits, all
    /// of which are legal header bytes — and a `None` here would be a
    /// programming error, so the caller treats it as one.
    pub fn authorization(&self) -> Option<reqwest::header::HeaderValue> {
        let mut v = reqwest::header::HeaderValue::from_str(&format!("Bearer {}", self.0)).ok()?;
        v.set_sensitive(true);
        Some(v)
    }
}

/// Replaces every run of **64 or more** lowercase hex digits with
/// `<redacted>`.
///
/// The belt to [`GxToken`]'s braces. The type makes it impossible to print a
/// token this adapter HOLDS; this makes it impossible to forward one that
/// arrived from somewhere else — a gx error message quoting the credential it
/// just refused, a server-supplied session id that IS the token echoed back
/// into a decode failure, a probe's stderr.
///
/// It lives here, beside the token, because every caller that needs it is
/// reasoning about tokens: the client's error boundary and (C4) the Tauri
/// credential reader, which owes the same guarantee on the probe's stderr.
///
/// Exactly a gx token's grammar, so the false positives are the adjacent
/// things: a sha256 digest, a long hex id. Redacting one of those inside an
/// error message costs a debugging detail; forwarding a token costs the
/// credential. The threshold is `>=` rather than `==` so a token with hex glued
/// to either end is still caught.
pub fn redact_hex64(s: &str) -> String {
    const MIN: usize = TOKEN_HEX_LEN;
    let bytes = s.as_bytes();
    let mut out = String::with_capacity(s.len());
    let mut i = 0usize;
    while i < bytes.len() {
        if !is_lower_hex(bytes[i]) {
            // Not the start of a run: copy this whole character (which may be
            // several bytes) and move past it.
            let ch = s[i..].chars().next().unwrap_or('\u{fffd}');
            out.push(ch);
            i += ch.len_utf8();
            continue;
        }
        let start = i;
        while i < bytes.len() && is_lower_hex(bytes[i]) {
            i += 1;
        }
        if i - start >= MIN {
            out.push_str("<redacted>");
        } else {
            out.push_str(&s[start..i]);
        }
    }
    out
}

// ---------------------------------------------------------------------------
// the discovery record
// ---------------------------------------------------------------------------

/// One `$GROK_HOME/gx-remote*.json` — where a leader says its lane is, and
/// which leader instance it is.
///
/// Every field is owned and scalar (the FRB-mirror rule): shed-mobile
/// hand-mirrors this along with the rest of the seam.
#[derive(Debug, Clone, PartialEq, Eq, serde::Deserialize)]
pub struct GxRecord {
    /// The **reported** URL, slash-free and `loopback_base_url`-clean. Never
    /// compared against a dial URL.
    #[serde(default)]
    pub url: String,
    #[serde(default)]
    pub pid: i64,
    #[serde(default, rename = "instanceId")]
    pub instance_id: String,
    #[serde(default, rename = "socketPath")]
    pub socket_path: String,
    /// The token's absolute path as the leader wrote it. **Not** derived from
    /// this record's own filename: the record may be suffixed
    /// (`gx-remote-<16 hex>.json`) while the token never is.
    #[serde(default, rename = "tokenFile")]
    pub token_file: String,
    #[serde(default)]
    pub version: String,
    #[serde(default, rename = "startedAt")]
    pub started_at: i64,
}

/// Whether two **reported** URLs name the same lane.
///
/// Both must satisfy [`loopback_base_url`] — roost validated `gx.remote` with
/// exactly that rule and gx writes its record's `url` in exactly that shape — and
/// then they must be byte-equal after trimming. Deliberately strict: the
/// predicate rejects a trailing slash, so a record carrying one does not match,
/// and `localhost` does not match `127.0.0.1`. Both sides come from the same
/// producer (gx writes the record; roost forwards the record's URL), so a
/// difference here means something re-wrote a URL, which is the case worth
/// refusing rather than papering over.
///
/// A **dial** URL is never an argument to this. See the module doc.
pub fn same_reported_url(a: &str, b: &str) -> bool {
    let (a, b) = (a.trim(), b.trim());
    loopback_base_url(a) && loopback_base_url(b) && a == b
}

// ---------------------------------------------------------------------------
// the probe's output
// ---------------------------------------------------------------------------

/// What [`PROBE_SCRIPT`] found: every discovery record on that host, and the
/// shared token when it was eligible to be read.
#[derive(Debug)]
pub struct Probe {
    pub records: Vec<GxRecord>,
    /// `None` when the token file failed one of the three checks (a symlink,
    /// not mode `0600`, not owned by the caller) or simply is not there. Not an
    /// error at parse time — the caller turns it into
    /// [`LaneError::Unavailable`], which is the quiet render.
    pub token: Option<GxToken>,
    /// How many record blocks did not parse as JSON. A COUNT, never the text:
    /// a record's contents include paths from the far host, and this number is
    /// enough to debug "the probe found three files and could read one".
    pub unreadable_records: usize,
}

/// Why a probe's output could not be read.
///
/// **Never carries raw probe output.** Every variant's message is a fixed
/// string, because the thing being parsed is one `cat` away from being the
/// token, and an error type that quoted its input would be one refactor away
/// from logging it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
pub enum ProbeError {
    /// The `===token===` line never appeared: the probe did not run to
    /// completion (no `sh`, the reach failed mid-stream, output truncated).
    #[error("the gx discovery probe did not run to completion")]
    Truncated,
    /// Something followed the sentinel, and it was not a 64-lowercase-hex
    /// token. Distinct from "no token": a malformed one means the file was
    /// readable and its CONTENTS are wrong, which no retry fixes.
    #[error("the gx token file did not contain a 64-character hex token")]
    MalformedToken,
}

/// Read [`PROBE_SCRIPT`]'s stdout.
///
/// The grammar, line-oriented because a record is pretty-printed across several
/// lines:
///
/// ```text
/// <record JSON>          ─┐ repeated, zero or more times
/// ---                    ─┘
/// ===token===
/// <64 hex>\n             ── optional
/// ```
///
/// A record block that does not parse is SKIPPED and counted
/// ([`Probe::unreadable_records`]) rather than failing the probe: one
/// unreadable file must not hide a live lane's record beside it.
///
/// # The token is deliberately NOT bound to a record
///
/// [`Probe`] carries many records and at most ONE token, and that asymmetry is
/// gx's, not an oversight worth "fixing": there is one token per `$GROK_HOME`,
/// shared by every leader running on it, and a leader restart changes its
/// `instanceId` and never the token. Verified live on two leaders — the record
/// was the suffixed `gx-remote-<16 hex>.json` while the token stayed
/// `gx-remote.token`. Which LEADER is answering is settled by the `instanceId`
/// pin, not by which file the token came from.
///
/// Relatedly, this parser does not judge a record's `url`: [`records_for`]
/// applies [`loopback_base_url`] downstream, and a parser that dropped
/// non-loopback records would hide them from a diagnostic that wants to say
/// "there is a record here, and it is not one shed will dial".
pub fn parse_probe(stdout: &str) -> Result<Probe, ProbeError> {
    let mut records = Vec::new();
    let mut unreadable_records = 0usize;
    let mut block = String::new();
    let mut lines = stdout.lines();
    let mut saw_sentinel = false;

    for line in lines.by_ref() {
        if line.trim_end() == PROBE_TOKEN_SENTINEL {
            saw_sentinel = true;
            break;
        }
        if line.trim_end() == PROBE_RECORD_DELIMITER {
            if !block.trim().is_empty() {
                match serde_json::from_str::<GxRecord>(&block) {
                    Ok(r) => records.push(r),
                    Err(_) => unreadable_records += 1,
                }
            }
            block.clear();
            continue;
        }
        block.push_str(line);
        block.push('\n');
    }

    if !saw_sentinel {
        // Whatever is in `block` is a record fragment at best; it is not
        // reported, and neither is the reason. See [`ProbeError`].
        return Err(ProbeError::Truncated);
    }

    // Everything after the sentinel is the token, or nothing at all.
    let rest: String = lines.collect::<Vec<_>>().join("\n");
    let token = if rest.trim().is_empty() {
        None
    } else {
        Some(GxToken::parse(&rest).ok_or(ProbeError::MalformedToken)?)
    };

    Ok(Probe {
        records,
        token,
        unreadable_records,
    })
}

// ---------------------------------------------------------------------------
// the local reader's checks, as pure functions
// ---------------------------------------------------------------------------

/// Where a local reader looks for `$GROK_HOME`, in the order it looks.
///
/// `explicit` is the test-mode override (`$SHED_TAURI_GX_HOME` in C4's Tauri
/// reader) — it wins outright so a hermetic harness cannot accidentally read the
/// developer's real `~/.grok`. Then `$GROK_HOME`, then `<home>/.grok`.
pub fn gx_home(explicit: Option<&Path>, grok_home_env: Option<&Path>, home: &Path) -> PathBuf {
    if let Some(p) = explicit {
        return p.to_path_buf();
    }
    if let Some(p) = grok_home_env {
        return p.to_path_buf();
    }
    home.join(DEFAULT_GROK_HOME)
}

/// Whether a filename is a discovery record — the `gx-remote*.json` glob
/// [`PROBE_SCRIPT`] uses, spelled once so the local reader and the remote one
/// cannot disagree about which files are records.
///
/// Note what it EXCLUDES: `gx-remote.token` does not end in `.json`, so the glob
/// that finds every record never finds the token.
pub fn is_record_file_name(name: &str) -> bool {
    name.starts_with(RECORD_PREFIX) && name.ends_with(RECORD_SUFFIX)
}

/// Every discovery record under `home`, sorted by filename so a reader's
/// behaviour does not depend on directory order.
///
/// A `home` that cannot be listed yields an empty vec rather than an error: "no
/// records here" and "no such directory" reach the caller as the same
/// [`LaneError::Unavailable`], and distinguishing them would only add a code
/// path that says the same thing.
pub fn record_files(home: &Path) -> Vec<PathBuf> {
    let Ok(entries) = std::fs::read_dir(home) else {
        return Vec::new();
    };
    let mut out: Vec<PathBuf> = entries
        .flatten()
        .filter(|e| e.file_name().to_str().is_some_and(is_record_file_name))
        .map(|e| e.path())
        .collect();
    out.sort();
    out
}

/// The token's path for a record — **only when it resolves inside `home`**.
///
/// The record's own `tokenFile` when it names one, else [`DEFAULT_TOKEN_FILE`]
/// under `home`. **Never** derived from the record's filename: a suffixed record
/// (`gx-remote-52e273e2a1d25203.json`) sits beside an UNsuffixed
/// `gx-remote.token`, because the token is per-`$GROK_HOME` and the record is
/// per-leader.
///
/// # Why the containment bound
///
/// A record is a FILE on disk, and its contents are attacker-influenced if the
/// host is compromised. Honouring `tokenFile` verbatim made it an arbitrary-path
/// read: a record naming any caller-owned `0600` file whose contents happen to
/// be 64 hex digits would have had those bytes sent as a bearer token to gx.
/// The SSH probe never had this exposure — it reads `$GROK_HOME/gx-remote.token`
/// and ignores `tokenFile` entirely — so the two readers also disagreed about
/// the same record, which is its own bug.
///
/// The bound costs nothing real: `tokenFile` was verified to be exactly
/// `$GROK_HOME/gx-remote.token` on two independent live leaders — one on a
/// non-default socket (a suffixed record) and one on the default. Keeping the
/// field honoured *within* the home tolerates a future gx that moves the file
/// inside its own directory, which a hardcoded filename would not.
///
/// # It resolves rather than compares strings
///
/// Both sides are canonicalized, so `..` cannot escape by spelling and a symlink
/// out of the home cannot either. The comparison is [`Path::starts_with`], which
/// is component-wise — a string prefix would accept `/home/u/.grok-evil` for a
/// home of `/home/u/.grok`.
///
/// The token file itself need not exist yet, so it is its PARENT that is
/// canonicalized and the file name re-attached. That makes this function do I/O,
/// which the rest of this module's checks deliberately do not — the alternative
/// is a purely textual containment test, and a textual test of a filesystem
/// property is exactly the kind that a symlink defeats.
pub fn token_path_for(record: &GxRecord, home: &Path) -> Result<PathBuf, TokenRefusal> {
    let raw = if record.token_file.trim().is_empty() {
        home.join(DEFAULT_TOKEN_FILE)
    } else {
        PathBuf::from(record.token_file.trim())
    };
    let home_real = home.canonicalize().map_err(|_| TokenRefusal::Unreadable)?;
    let (Some(parent), Some(name)) = (raw.parent(), raw.file_name()) else {
        // A path with no file name (`/`, or `..`) is not a token file.
        return Err(TokenRefusal::OutsideHome);
    };
    // A relative `tokenFile` is resolved against the home, not against the
    // process's cwd — the cwd is nothing to do with the record.
    let parent = if parent.as_os_str().is_empty() {
        home.to_path_buf()
    } else if parent.is_absolute() {
        parent.to_path_buf()
    } else {
        home.join(parent)
    };
    let parent_real = parent
        .canonicalize()
        .map_err(|_| TokenRefusal::Unreadable)?;
    if !parent_real.starts_with(&home_real) {
        return Err(TokenRefusal::OutsideHome);
    }
    Ok(parent_real.join(name))
}

/// The three facts a token file's eligibility turns on, gathered by the caller
/// so the RULE is a pure function.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TokenFileFacts {
    /// From an `lstat`, not a `stat`: a symlink must be refused, not followed.
    pub is_symlink: bool,
    /// A regular file — not a directory, fifo, socket or device.
    pub is_regular: bool,
    /// The permission bits, masked to `0o7777`.
    pub mode: u32,
    pub uid: u32,
}

/// Why a token file may not be read. The message never names the path — a
/// refusal's job is to say what was wrong, and the caller already knows where it
/// looked.
#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
pub enum TokenRefusal {
    #[error("the gx token file is a symlink")]
    Symlink,
    #[error("the gx token file is not a regular file")]
    NotRegular,
    #[error("the gx token file's mode is not 0600")]
    Mode,
    #[error("the gx token file is not owned by this user")]
    Owner,
    #[error("the gx token file could not be read")]
    Unreadable,
    /// The record named a token file outside `$GROK_HOME`.
    #[error("the gx token file is outside GROK_HOME")]
    OutsideHome,
    #[error("the gx token file did not contain a 64-character hex token")]
    Malformed,
}

/// gx's own eligibility rule, as a pure function.
///
/// It is gx's, not a stricter or looser one of shed's: gx's reader opens the
/// token `O_NOFOLLOW`, validates mode `0600` and the owner from the fd, and
/// refuses otherwise. shed should not be the weaker reader of the two — a token
/// that gx would refuse to read is one shed must not send.
///
/// The mode check is **exact**. `0640` is refused even though the caller could
/// read it, because a token another user can read is one the caller cannot claim
/// sole authority over, and that authority is the whole meaning of the bearer
/// (gx records it as `Principal::Human`).
pub fn token_file_refusal(facts: &TokenFileFacts, caller_uid: u32) -> Option<TokenRefusal> {
    if facts.is_symlink {
        return Some(TokenRefusal::Symlink);
    }
    if !facts.is_regular {
        return Some(TokenRefusal::NotRegular);
    }
    if facts.mode & 0o7777 != 0o600 {
        return Some(TokenRefusal::Mode);
    }
    if facts.uid != caller_uid {
        return Some(TokenRefusal::Owner);
    }
    None
}

/// Read a token file locally, applying [`token_file_refusal`] to the file that
/// was actually OPENED — and reading the bytes **through that same descriptor**.
///
/// The order, and what each step is worth:
///
/// 1. `symlink_metadata` (an `lstat`) refuses a symlink **by name**. This is a
///    pre-check and it is advisory: nothing holds the path still afterwards.
/// 2. the file is opened — the ONE resolution of `path` that this function
///    performs;
/// 3. `File::metadata` (an `fstat`) re-reads the file type, mode and owner
///    **from the open descriptor**, and those are the facts
///    [`token_file_refusal`] runs on;
/// 4. the contents are read from the SAME descriptor, never by re-opening the
///    path.
///
/// Step 4 is the one that matters and it was previously wrong: the function used
/// to validate a descriptor and then call `read_to_string(path)`, so the bytes
/// that became the token came from a fourth resolution that nothing had checked.
/// A gx leader rewriting its token file — which happens on an ordinary restart,
/// no attacker required — could land in that window and yield an unvalidated
/// read. Now the checks and the read are the same object.
///
/// **The residual, stated exactly.** A path swapped between (1) and (2) is
/// still opened, and if the replacement is a symlink it IS followed — so the
/// symlink refusal in step 1 is not a guarantee. What IS guaranteed is that
/// whatever was opened must itself be a regular file, mode `0600`, owned by the
/// caller, and that the token comes from it: a swap can change WHICH eligible
/// file is read, never whether an ineligible one is accepted. An earlier version
/// of this comment claimed the race "could turn a refusal into a different
/// refusal, never into an acceptance"; that was true of the window it named and
/// false of the code, because the load-bearing read happened outside it.
///
/// Unix-only: the mode and owner this rule is about do not exist elsewhere, and
/// gx does not run elsewhere.
#[cfg(unix)]
pub fn read_token_file(path: &Path, caller_uid: u32) -> Result<GxToken, TokenRefusal> {
    use std::io::Read as _;
    use std::os::unix::fs::MetadataExt as _;

    let lst = std::fs::symlink_metadata(path).map_err(|_| TokenRefusal::Unreadable)?;
    if lst.file_type().is_symlink() {
        return Err(TokenRefusal::Symlink);
    }
    let mut file = std::fs::File::open(path).map_err(|_| TokenRefusal::Unreadable)?;
    let md = file.metadata().map_err(|_| TokenRefusal::Unreadable)?;
    let facts = TokenFileFacts {
        // The by-name refusal already happened; the OPENED object is what the
        // remaining checks — and the read below — are about.
        is_symlink: false,
        is_regular: md.file_type().is_file(),
        mode: md.mode(),
        uid: md.uid(),
    };
    if let Some(why) = token_file_refusal(&facts, caller_uid) {
        return Err(why);
    }
    let mut raw = String::new();
    file.read_to_string(&mut raw)
        .map_err(|_| TokenRefusal::Unreadable)?;
    GxToken::parse(&raw).ok_or(TokenRefusal::Malformed)
}

/// The whole LOCAL read, assembled: glob the records under `home`, keep the ones
/// naming `reported_url`, resolve that record's token path, and read it under
/// [`token_file_refusal`].
///
/// This exists so C4's Tauri reader CALLS the sequence rather than reimplementing
/// it — the ordering here is load-bearing (the token's path comes from the
/// record, never from the record's filename) and a second implementation is how
/// the two would drift.
///
/// **Never shells out.** The `ReachKind::Local` path reads files directly; the
/// probe is the SSH path's business.
///
/// When several records name one URL — which means a stale record, since a URL
/// is a port and two leaders cannot bind one — the first in filename order wins
/// and the client's `instanceId` pin is what catches a wrong pick. A caller that
/// wants to choose for itself uses [`record_files`] and [`records_for`] directly.
///
/// Both failures are [`LaneError::Unavailable`]: no record and no readable token
/// are the quiet, render-the-row-stale cases, and neither message carries a path
/// or a refusal's detail (those go to a `tracing::debug` at the call site).
#[cfg(unix)]
pub fn local_discovery(
    home: &Path,
    reported_url: &str,
    caller_uid: u32,
) -> Result<GxDiscovery, LaneError> {
    let records: Vec<GxRecord> = record_files(home)
        .iter()
        .filter_map(|p| std::fs::read_to_string(p).ok())
        .filter_map(|s| serde_json::from_str::<GxRecord>(&s).ok())
        .collect();
    let record = records_for(&records, reported_url)
        .into_iter()
        .next()
        .ok_or_else(|| {
            LaneError::Unavailable(format!("no gx discovery record for {reported_url}"))
        })?;
    let token = token_path_for(record, home)
        .and_then(|p| read_token_file(&p, caller_uid))
        .map_err(|_| LaneError::Unavailable("the gx token could not be read".to_string()))?;
    Ok(GxDiscovery {
        token,
        instance_id: record.instance_id.clone(),
    })
}

// ---------------------------------------------------------------------------
// the source
// ---------------------------------------------------------------------------

/// What a client hands the adapter: the token, and the leader instance it was
/// read beside.
///
/// The `instance_id` is half of the pinning rule ([`crate::GxClient`]'s
/// `ensure_pinned`): a token alone proves nothing about WHICH leader is
/// answering on that port.
#[derive(Debug, Clone)]
pub struct GxDiscovery {
    pub token: GxToken,
    pub instance_id: String,
}

/// Where credentials come from — the seam the contract deliberately does not
/// have.
///
/// The argument is the **reported** URL, because that is what a discovery
/// record is matched against. An implementation that dialled it would be
/// wrong over SSH; dialling is [`crate::transport::GxTransport`]'s job.
#[async_trait::async_trait]
pub trait GxCredentialSource: Send + Sync {
    async fn discover(&self, reported_url: &str) -> Result<GxDiscovery, LaneError>;
}

/// A credential source that already knows the answer — tests, the example, and
/// the phone's first cut, which reads the record and the token itself and then
/// has nothing left to discover.
#[derive(Debug, Clone)]
pub struct StaticCredentials(GxDiscovery);

impl StaticCredentials {
    pub fn new(discovery: GxDiscovery) -> StaticCredentials {
        StaticCredentials(discovery)
    }

    /// The common test shape: a token string and an instance id.
    ///
    /// `None` when the token is not 64 lowercase hex — a fixture that plants a
    /// bad token should fail at construction, not at the first request.
    pub fn from_parts(token: &str, instance_id: &str) -> Option<StaticCredentials> {
        Some(StaticCredentials(GxDiscovery {
            token: GxToken::parse(token)?,
            instance_id: instance_id.to_string(),
        }))
    }
}

#[async_trait::async_trait]
impl GxCredentialSource for StaticCredentials {
    async fn discover(&self, _reported_url: &str) -> Result<GxDiscovery, LaneError> {
        Ok(self.0.clone())
    }
}

/// The records on a host that name `reported_url`, in the order the probe found
/// them.
///
/// A **dead `pid`** is deliberately NOT filtered here: the caller pins the
/// record whose `instanceId` matches what `healthz` just answered, which is a
/// stronger test than a pid check and does not race a leader that restarted
/// between the probe and the health check. `pid` is kept on [`GxRecord`] for a
/// human reading a log, not for a decision.
pub fn records_for<'a>(records: &'a [GxRecord], reported_url: &str) -> Vec<&'a GxRecord> {
    records
        .iter()
        .filter(|r| same_reported_url(&r.url, reported_url))
        .collect()
}

#[cfg(test)]
mod tests;
