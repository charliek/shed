//! The host-agent's **agent-forward** SSH backend — the Rust port of the Go daemon's
//! `ssh_backend_agent.go`. It proxies the `list`/`sign` ops to the host's existing
//! SSH agent (Secretive, 1Password, `ssh-agent`, …) reachable at `$SSH_AUTH_SOCK`, by
//! speaking the **ssh-agent wire protocol** directly (a minimal hand-rolled client —
//! no new dependency).
//!
//! **Fresh connection per operation** (Go's `connect`: `net.Dial("unix", path)` per
//! `List`/`Sign`). Framing is `uint32-BE length ‖ payload`, where `payload[0]` is the
//! message type:
//!
//! - **`list`** → `SSH_AGENTC_REQUEST_IDENTITIES(11)`; reply
//!   `SSH_AGENT_IDENTITIES_ANSWER(12)`: `uint32 nkeys` then per key `string keyblob`,
//!   `string comment`. The `format` is the algorithm name at the head of the keyblob
//!   (the first SSH `string` inside it — exactly what Go's `agent.Key.Format`/`Type()`
//!   returns via `parseKey`). Returns `SshKeyInfo{format, blob: keyblob, comment}`.
//! - **`sign`** → `SSH_AGENTC_SIGN_REQUEST(13)`: `string keyblob`, `string data`,
//!   `uint32 flags`. The `flags` word passes through **verbatim** — Go sends the same
//!   wire bytes whether it calls `Sign` (flags 0) or `SignWithFlags` (flags != 0).
//!   Reply `SSH_AGENT_SIGN_RESPONSE(14)`: `string sigblob`, where `sigblob = string
//!   format ‖ string blob ‖ rest` (x/crypto's `ssh.Signature` encoding). Parsed into
//!   `SshSignature{format, blob}`; any trailing `rest` is dropped (Go keeps it in
//!   `Signature.Rest`, which the bus response pins to `""`).
//! - `SSH_AGENT_FAILURE(5)` → `Err`. An unexpected type byte → `Err`.
//!
//! **Error-string parity (best-effort, op-log/unit-owned):** the strings mirror
//! x/crypto/ssh/agent's client (`golang.org/x/crypto@v0.53.0/ssh/agent/client.go`):
//! `"agent: failed to list keys"` (client.go:476), `"agent: failed to sign challenge"`
//! (client.go:509), `"agent: client error: response too large"` (the oversize guard,
//! client.go:402-403 wrapped by `clientErr`, client.go:248), `"agent: client error:
//! agent: empty packet"` (client.go:519), `"agent: client error: agent: unknown type
//! tag N"` (client.go:534). These only surface in the operational log and the
//! **list-error audit detail**, which the plan assigns to unit parity (not the live
//! differential) — error-string parity across live agent failures is brittle, same
//! split as the minter's error strings. Two documented residual divergences: (1) the
//! `connect` error's wrapped OS text (`connecting to SSH agent at <path>: <err>`)
//! differs between Go's `%w` and Rust's `io::Error` Display — not wire-visible; (2) a
//! valid-but-wrong *known* reply tag prints the numeric tag here vs Go's Go-type name
//! (`%T`) in `List`/`Sign`'s `default` arm.
//!
//! **Rust-only safety bounds** (labeled — Go's client relies on OS/socket behavior):
//! - a reply length prefix over [`MAX_AGENT_RESPONSE_BYTES`] is rejected **before** the
//!   body is read (this cap value DOES mirror x/crypto's `maxAgentResponseBytes = 16 <<
//!   20`, client.go:157 — the *early-reject-without-reading-the-body* framing is the
//!   Rust-specific part);
//! - a per-op read/write [`DEFAULT_IO_TIMEOUT`] so a wedged agent can't hang the bus
//!   handler forever (no Go mirror; the timeout is a struct field so tests inject a
//!   short one).
//!
//! **Feature posture / concurrency:** no `desktop-forwarding` gate — the bus (always
//! compiled, even headless) owns `sign`/`list`, so this module is always built and the
//! `--no-default-features` build compiles it. The trait is **sync/blocking** like the
//! local-keys backend; the bus calls it directly from its async handler (same pattern
//! as `LocalKeysBackend::sign`), so blocking UDS I/O here is consistent with the
//! existing call site.

use std::ffi::OsString;
use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::time::Duration;

use crate::ssh_backend::{SshBackend, SshKeyInfo, SshSignature};

// ssh-agent protocol message types (draft-miller-ssh-agent §5.1 / x/crypto/ssh/agent
// client.go constants).
const SSH_AGENT_FAILURE: u8 = 5;
const SSH_AGENT_SUCCESS: u8 = 6;
const SSH_AGENTC_REQUEST_IDENTITIES: u8 = 11;
const SSH_AGENT_IDENTITIES_ANSWER: u8 = 12;
const SSH_AGENTC_SIGN_REQUEST: u8 = 13;
const SSH_AGENT_SIGN_RESPONSE: u8 = 14;

/// Max accepted agent reply size. Mirrors x/crypto/ssh/agent's `maxAgentResponseBytes
/// = 16 << 20` (client.go:157). A length prefix larger than this is rejected before
/// the body is read (the *early-reject* framing is the Rust-only part — see the module
/// doc), so a hostile/wedged agent can't make us allocate or block on an unbounded
/// read.
const MAX_AGENT_RESPONSE_BYTES: u32 = 16 << 20;

/// Default per-op read/write timeout. **Rust-only** (no Go mirror — Go's agent client
/// relies on the OS/socket default plus a caller-side deadline). Bounds a wedged agent
/// so a `sign`/`list` can't hang the bus handler forever. A struct field so tests
/// override it with a short value.
const DEFAULT_IO_TIMEOUT: Duration = Duration::from_secs(30);

/// The agent-forward backend (Go `agentForwardBackend`). Stores the `$SSH_AUTH_SOCK`
/// path resolved at construction; every op opens a fresh connection to it.
#[derive(Debug)]
pub struct AgentForwardBackend {
    socket_path: PathBuf,
    io_timeout: Duration,
}

impl AgentForwardBackend {
    /// Construct from an explicit `$SSH_AUTH_SOCK` value (Go's `newAgentForwardBackend`
    /// reads the env; here [`resolve_ssh_backend`](crate::ssh_backend::resolve_ssh_backend)
    /// does the `std::env::var_os` read and passes the value in). Taking the value as a
    /// parameter is the injectable seam the resolver and the unit tests use so they
    /// don't depend on — or race — the ambient developer agent. Mirrors Go: the path is
    /// only **stored** here — no dial happens until an op, so an explicit
    /// `agent-forward` with an unreachable socket still constructs (the error surfaces
    /// on the first `list`/`sign`).
    pub(crate) fn from_sock(sock: Option<OsString>) -> Result<AgentForwardBackend, String> {
        match sock.filter(|s| !s.is_empty()) {
            Some(s) => Ok(AgentForwardBackend {
                socket_path: PathBuf::from(s),
                io_timeout: DEFAULT_IO_TIMEOUT,
            }),
            None => Err("SSH_AUTH_SOCK not set".to_string()),
        }
    }

    /// Open a fresh connection to the agent (Go's `connect`: `net.Dial("unix", path)`
    /// per op). The error prefix mirrors Go's `fmt.Errorf("connecting to SSH agent at
    /// %s: %w", ...)`; the wrapped OS error text differs slightly Go-vs-Rust (op-log
    /// only, not wire-visible).
    fn connect(&self) -> Result<UnixStream, String> {
        let conn = UnixStream::connect(&self.socket_path).map_err(|e| {
            format!(
                "connecting to SSH agent at {}: {e}",
                self.socket_path.display()
            )
        })?;
        // Rust-only: bound a wedged agent (see DEFAULT_IO_TIMEOUT). set_*_timeout only
        // errors on a bad fd, which a just-connected stream isn't — ignore the Result.
        let _ = conn.set_read_timeout(Some(self.io_timeout));
        let _ = conn.set_write_timeout(Some(self.io_timeout));
        Ok(conn)
    }

    /// One request/response exchange on a fresh connection: frame + send `payload`,
    /// then read and return the framed reply payload (leading type byte included).
    fn round_trip(&self, payload: &[u8]) -> Result<Vec<u8>, String> {
        let mut conn = self.connect()?;
        write_message(&mut conn, payload)?;
        read_message(&mut conn)
    }
}

impl SshBackend for AgentForwardBackend {
    fn list(&self) -> Result<Vec<SshKeyInfo>, String> {
        // REQUEST_IDENTITIES is a bare type byte (Go: `req := []byte{agentRequestIdentities}`).
        let reply = self.round_trip(&[SSH_AGENTC_REQUEST_IDENTITIES])?;
        match reply_type(&reply)? {
            SSH_AGENT_IDENTITIES_ANSWER => parse_identities(&reply),
            SSH_AGENT_FAILURE => Err("agent: failed to list keys".to_string()),
            other => Err(unexpected_type("agent: failed to list keys", other)),
        }
    }

    fn sign(&self, public_key: &[u8], data: &[u8], flags: u32) -> Result<SshSignature, String> {
        // SIGN_REQUEST(13): type ‖ string(keyblob) ‖ string(data) ‖ uint32(flags).
        let mut payload = Vec::with_capacity(1 + 8 + public_key.len() + data.len() + 4);
        payload.push(SSH_AGENTC_SIGN_REQUEST);
        write_string(&mut payload, public_key);
        write_string(&mut payload, data);
        payload.extend_from_slice(&flags.to_be_bytes());

        let reply = self.round_trip(&payload)?;
        match reply_type(&reply)? {
            SSH_AGENT_SIGN_RESPONSE => parse_signature(&reply),
            SSH_AGENT_FAILURE => Err("agent: failed to sign challenge".to_string()),
            other => Err(unexpected_type("agent: failed to sign challenge", other)),
        }
    }

    fn mode(&self) -> &str {
        "agent-forward"
    }
}

/// Frame and send a request: `uint32-BE len ‖ payload` (Go's `serialCall`). Agent
/// requests are tiny, so `len` never approaches `u32::MAX`.
fn write_message(conn: &mut UnixStream, payload: &[u8]) -> Result<(), String> {
    conn.write_all(&(payload.len() as u32).to_be_bytes())
        .map_err(|e| io_err(&e))?;
    conn.write_all(payload).map_err(|e| io_err(&e))?;
    conn.flush().map_err(|e| io_err(&e))
}

/// Read a framed reply: the `uint32-BE` length prefix, then exactly that many bytes.
/// An oversize prefix is rejected **before** the body is read (Go's `respSize >
/// maxAgentResponseBytes` guard), so a hostile length can't drive an unbounded
/// allocation or a never-ending read.
fn read_message(conn: &mut UnixStream) -> Result<Vec<u8>, String> {
    let mut len_buf = [0u8; 4];
    conn.read_exact(&mut len_buf).map_err(|e| io_err(&e))?;
    let len = u32::from_be_bytes(len_buf);
    if len > MAX_AGENT_RESPONSE_BYTES {
        // Go: `clientErr(errors.New("response too large"))` → "agent: client error: response too large".
        return Err("agent: client error: response too large".to_string());
    }
    let mut buf = vec![0u8; len as usize];
    conn.read_exact(&mut buf).map_err(|e| io_err(&e))?;
    Ok(buf)
}

/// The reply's message-type byte, or Go's empty-packet error (`unmarshal`,
/// client.go:519, wrapped by `clientErr`).
fn reply_type(reply: &[u8]) -> Result<u8, String> {
    reply
        .first()
        .copied()
        .ok_or_else(|| "agent: client error: agent: empty packet".to_string())
}

/// The error for a reply whose type is neither the expected answer nor
/// `SSH_AGENT_FAILURE`, mirroring Go's two shapes as closely as is cheap:
/// - a valid-but-wrong *known* tag (SUCCESS/IDENTITIES_ANSWER/SIGN_RESPONSE in the
///   other direction) → `"<prefix>, unexpected message type N"` (Go's `List`/`Sign`
///   `default` arm — Go prints the Go type name via `%T`; we print the numeric tag, a
///   documented op-log-only divergence);
/// - a truly unknown tag → `"agent: client error: agent: unknown type tag N"`
///   (x/crypto's `unmarshal` default, client.go:534, wrapped by `clientErr`).
fn unexpected_type(prefix: &str, type_byte: u8) -> String {
    match type_byte {
        SSH_AGENT_SUCCESS | SSH_AGENT_IDENTITIES_ANSWER | SSH_AGENT_SIGN_RESPONSE => {
            format!("{prefix}, unexpected message type {type_byte}")
        }
        other => format!("agent: client error: agent: unknown type tag {other}"),
    }
}

/// Parse `SSH_AGENT_IDENTITIES_ANSWER(12)`: `uint32 nkeys` then per key `string
/// keyblob`, `string comment`. Mirrors Go's `List` + `parseKey`.
fn parse_identities(reply: &[u8]) -> Result<Vec<SshKeyInfo>, String> {
    let mut r = Reader::new(reply);
    r.u8()?; // type byte (already matched by the caller).
    let nkeys = r.u32()?;
    // Go's guard against an over-large count (client.go:461-462).
    if nkeys > MAX_AGENT_RESPONSE_BYTES / 8 {
        return Err("agent: too many keys in agent reply".to_string());
    }
    // Plain `Vec` (no `with_capacity(nkeys)`): `nkeys` is attacker-controlled up to
    // ~2M here, and the real reply body is already capped at 16 MiB, so pushing as we
    // parse avoids a speculative multi-hundred-MB reserve. The loop's `Reader::string`
    // fails cleanly (truncated) if `nkeys` overstates the actual data.
    let mut keys = Vec::new();
    for _ in 0..nkeys {
        let blob = r.string()?.to_vec();
        let comment = String::from_utf8_lossy(r.string()?).into_owned();
        let format = parse_key_format(&blob)?;
        keys.push(SshKeyInfo {
            format,
            blob,
            comment,
        });
    }
    Ok(keys)
}

/// The algorithm name at the head of a marshaled public-key blob — the first SSH
/// `string` inside it (Go's `wireKey.Format` from `parseKey`; also what
/// `agent.Key.Format`/`Type()` returns).
fn parse_key_format(blob: &[u8]) -> Result<String, String> {
    let mut r = Reader::new(blob);
    let algo = r
        .string()
        .map_err(|_| "agent: client error: malformed key blob".to_string())?;
    Ok(String::from_utf8_lossy(algo).into_owned())
}

/// Parse `SSH_AGENT_SIGN_RESPONSE(14)`: `string sigblob` where `sigblob = string
/// format ‖ string blob ‖ rest` (the `ssh.Signature` encoding). The trailing `rest`
/// INSIDE the sigblob is ignored (Go keeps it in `Signature.Rest` via the `ssh:"rest"`
/// tag; the bus response pins it to `""`) — but trailing bytes AFTER the sigblob
/// string are an error, matching Go's `ssh.Unmarshal(signResponseAgentMsg)`, which
/// rejects top-level trailing data (cursor review finding).
fn parse_signature(reply: &[u8]) -> Result<SshSignature, String> {
    let mut r = Reader::new(reply);
    r.u8()?; // type byte (already matched).
    let sigblob = r.string()?;
    if !r.exhausted() {
        return Err(truncated()); // trailing junk after the sigblob string
    }
    let mut sr = Reader::new(sigblob);
    let format = String::from_utf8_lossy(sr.string()?).into_owned();
    let blob = sr.string()?.to_vec();
    Ok(SshSignature { format, blob })
}

/// Append an SSH `string` (RFC 4251: `uint32-BE len ‖ bytes`) to `out`.
fn write_string(out: &mut Vec<u8>, bytes: &[u8]) {
    out.extend_from_slice(&(bytes.len() as u32).to_be_bytes());
    out.extend_from_slice(bytes);
}

/// Format an `io::Error` the way Go's `clientErr` wraps a transport error
/// (`fmt.Errorf("agent: client error: %v", err)`, client.go:248). The exact OS text
/// differs Go-vs-Rust (op-log only).
fn io_err(e: &std::io::Error) -> String {
    format!("agent: client error: {e}")
}

/// A minimal SSH-wire reader over a byte slice (RFC 4251 `byte`/`uint32`/`string`).
struct Reader<'a> {
    buf: &'a [u8],
    pos: usize,
}

impl<'a> Reader<'a> {
    fn new(buf: &'a [u8]) -> Reader<'a> {
        Reader { buf, pos: 0 }
    }

    fn u8(&mut self) -> Result<u8, String> {
        let b = *self.buf.get(self.pos).ok_or_else(truncated)?;
        self.pos += 1;
        Ok(b)
    }

    fn u32(&mut self) -> Result<u32, String> {
        let end = self.pos.checked_add(4).ok_or_else(truncated)?;
        let slice = self.buf.get(self.pos..end).ok_or_else(truncated)?;
        self.pos = end;
        Ok(u32::from_be_bytes(slice.try_into().unwrap()))
    }

    fn string(&mut self) -> Result<&'a [u8], String> {
        let len = self.u32()? as usize;
        let end = self.pos.checked_add(len).ok_or_else(truncated)?;
        let slice = self.buf.get(self.pos..end).ok_or_else(truncated)?;
        self.pos = end;
        Ok(slice)
    }

    /// True when every byte has been consumed (Go's `ssh.Unmarshal` errors on
    /// top-level trailing data; callers use this to mirror that).
    fn exhausted(&self) -> bool {
        self.pos == self.buf.len()
    }
}

/// The parse-underrun error (Go's `ssh.Unmarshal` failures flow through `clientErr`).
fn truncated() -> String {
    "agent: client error: malformed agent response".to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    // `UnixStream` is already in scope via `use super::*` (top-level import); only
    // `UnixListener` is new to the test module.
    use std::os::unix::net::UnixListener;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    use std::sync::{Arc, Mutex};
    use std::thread::JoinHandle;

    // ---- An in-Rust fake ssh-agent on a UDS (the unit-test seam; the python harness
    //      fake in commit 3 must replicate the SAME wire behavior for the ops it
    //      serves). Fresh-connection-per-op is exercised by counting accepts. ----

    /// What the fake replies with for each request it accepts.
    #[derive(Clone)]
    enum Behavior {
        /// Serve `identities` on REQUEST_IDENTITIES and a canned `(sig_format,
        /// sig_blob)` signature on SIGN_REQUEST (FAILURE for any other request type).
        Serve {
            identities: Vec<(Vec<u8>, String)>,
            sig_format: String,
            sig_blob: Vec<u8>,
        },
        /// Reply SSH_AGENT_FAILURE(5) to everything.
        Failure,
        /// Reply with an unknown/garbage message-type byte.
        GarbageType,
        /// Reply with a length prefix over the 16 MiB cap, then send no body.
        Oversize,
        /// Read the request, then sleep `d` before dropping the connection (never
        /// replies) — models a wedged agent past the client's read timeout.
        Hang(Duration),
    }

    struct FakeAgent {
        path: PathBuf,
        _tmp: tempfile::TempDir,
        conns: Arc<AtomicUsize>,
        requests: Arc<Mutex<Vec<Vec<u8>>>>,
        shutdown: Arc<AtomicBool>,
        handle: Option<JoinHandle<()>>,
    }

    impl FakeAgent {
        fn start(behavior: Behavior) -> FakeAgent {
            let tmp = tempfile::tempdir().unwrap();
            let path = tmp.path().join("agent.sock");
            let listener = UnixListener::bind(&path).unwrap();

            let conns = Arc::new(AtomicUsize::new(0));
            let requests = Arc::new(Mutex::new(Vec::new()));
            let shutdown = Arc::new(AtomicBool::new(false));

            let handle = std::thread::spawn({
                let conns = conns.clone();
                let requests = requests.clone();
                let shutdown = shutdown.clone();
                move || {
                    for stream in listener.incoming() {
                        let stream = match stream {
                            Ok(s) => s,
                            Err(_) => break,
                        };
                        // Drop's wake-connection arrives after `shutdown` is set — bail
                        // before counting/handling it so the accept count stays true.
                        if shutdown.load(Ordering::SeqCst) {
                            break;
                        }
                        conns.fetch_add(1, Ordering::SeqCst);
                        handle_conn(stream, &behavior, &requests);
                    }
                }
            });

            FakeAgent {
                path,
                _tmp: tmp,
                conns,
                requests,
                shutdown,
                handle: Some(handle),
            }
        }

        fn backend(&self, timeout: Duration) -> AgentForwardBackend {
            AgentForwardBackend {
                socket_path: self.path.clone(),
                io_timeout: timeout,
            }
        }

        fn conn_count(&self) -> usize {
            self.conns.load(Ordering::SeqCst)
        }

        fn requests(&self) -> Vec<Vec<u8>> {
            self.requests.lock().unwrap().clone()
        }
    }

    impl Drop for FakeAgent {
        fn drop(&mut self) {
            self.shutdown.store(true, Ordering::SeqCst);
            // Wake the blocking `accept` so the thread observes `shutdown` and exits.
            let _ = UnixStream::connect(&self.path);
            if let Some(h) = self.handle.take() {
                let _ = h.join();
            }
        }
    }

    fn handle_conn(mut stream: UnixStream, behavior: &Behavior, requests: &Mutex<Vec<Vec<u8>>>) {
        let req = match read_request(&mut stream) {
            Some(r) => r,
            None => return,
        };
        let msg_type = req.first().copied().unwrap_or(0);
        requests.lock().unwrap().push(req);
        match behavior {
            Behavior::Serve {
                identities,
                sig_format,
                sig_blob,
            } => match msg_type {
                SSH_AGENTC_REQUEST_IDENTITIES => {
                    write_reply(&mut stream, &encode_identities(identities))
                }
                SSH_AGENTC_SIGN_REQUEST => {
                    write_reply(&mut stream, &encode_signature(sig_format, sig_blob))
                }
                _ => write_reply(&mut stream, &[SSH_AGENT_FAILURE]),
            },
            Behavior::Failure => write_reply(&mut stream, &[SSH_AGENT_FAILURE]),
            Behavior::GarbageType => write_reply(&mut stream, &[99u8]),
            Behavior::Oversize => {
                // A length prefix over the cap, no body (the client must reject on the
                // header without reading — and without hanging).
                let _ = stream.write_all(&(MAX_AGENT_RESPONSE_BYTES + 1).to_be_bytes());
                let _ = stream.flush();
            }
            Behavior::Hang(d) => std::thread::sleep(*d),
        }
    }

    fn read_request(stream: &mut UnixStream) -> Option<Vec<u8>> {
        let mut len_buf = [0u8; 4];
        stream.read_exact(&mut len_buf).ok()?;
        let len = u32::from_be_bytes(len_buf) as usize;
        let mut payload = vec![0u8; len];
        stream.read_exact(&mut payload).ok()?;
        Some(payload)
    }

    fn write_reply(stream: &mut UnixStream, payload: &[u8]) {
        let _ = stream.write_all(&(payload.len() as u32).to_be_bytes());
        let _ = stream.write_all(payload);
        let _ = stream.flush();
    }

    fn encode_identities(identities: &[(Vec<u8>, String)]) -> Vec<u8> {
        let mut out = vec![SSH_AGENT_IDENTITIES_ANSWER];
        out.extend_from_slice(&(identities.len() as u32).to_be_bytes());
        for (blob, comment) in identities {
            write_string(&mut out, blob);
            write_string(&mut out, comment.as_bytes());
        }
        out
    }

    fn encode_signature(format: &str, blob: &[u8]) -> Vec<u8> {
        // sigblob = string(format) ‖ string(blob); reply = type ‖ string(sigblob).
        let mut inner = Vec::new();
        write_string(&mut inner, format.as_bytes());
        write_string(&mut inner, blob);
        let mut out = vec![SSH_AGENT_SIGN_RESPONSE];
        write_string(&mut out, &inner);
        out
    }

    /// A marshaled `ssh-ed25519` public-key blob (`string("ssh-ed25519") ‖ string(32
    /// bytes)`) — enough for `parse_key_format` to read `"ssh-ed25519"`.
    fn ed25519_blob(seed: u8) -> Vec<u8> {
        let mut out = Vec::new();
        write_string(&mut out, b"ssh-ed25519");
        write_string(&mut out, &[seed; 32]);
        out
    }

    /// Decode a captured SIGN_REQUEST payload into `(keyblob, data, flags)`.
    fn decode_sign_request(payload: &[u8]) -> (Vec<u8>, Vec<u8>, u32) {
        let mut r = Reader::new(payload);
        assert_eq!(r.u8().unwrap(), SSH_AGENTC_SIGN_REQUEST);
        let keyblob = r.string().unwrap().to_vec();
        let data = r.string().unwrap().to_vec();
        let flags = r.u32().unwrap();
        (keyblob, data, flags)
    }

    #[test]
    fn list_roundtrip() {
        let ed = ed25519_blob(1);
        let fake = FakeAgent::start(Behavior::Serve {
            identities: vec![(ed.clone(), "id_ed25519".to_string())],
            sig_format: String::new(),
            sig_blob: Vec::new(),
        });
        let be = fake.backend(DEFAULT_IO_TIMEOUT);
        let keys = be.list().unwrap();
        assert_eq!(keys.len(), 1);
        assert_eq!(keys[0].format, "ssh-ed25519");
        assert_eq!(keys[0].blob, ed);
        assert_eq!(keys[0].comment, "id_ed25519");
        assert_eq!(be.mode(), "agent-forward");
    }

    #[test]
    fn sign_roundtrip_flags() {
        // Nonzero flags must reach the wire verbatim; the canned signature parses.
        let key = ed25519_blob(2);
        let fake = FakeAgent::start(Behavior::Serve {
            identities: vec![(key.clone(), "id_rsa".to_string())],
            sig_format: "rsa-sha2-256".to_string(),
            sig_blob: b"canned-signature-bytes".to_vec(),
        });
        let be = fake.backend(DEFAULT_IO_TIMEOUT);
        let data = b"challenge-to-sign";
        let sig = be.sign(&key, data, 6).unwrap();
        assert_eq!(sig.format, "rsa-sha2-256");
        assert_eq!(sig.blob, b"canned-signature-bytes");

        // The fake recorded the raw request — assert keyblob/data/flags on the wire.
        let reqs = fake.requests();
        assert_eq!(reqs.len(), 1);
        let (keyblob, wire_data, flags) = decode_sign_request(&reqs[0]);
        assert_eq!(keyblob, key);
        assert_eq!(wire_data, data);
        assert_eq!(flags, 6, "flags must pass through verbatim");
    }

    #[test]
    fn sign_roundtrip_flags_zero() {
        // flags=0 sends the same wire form (uint32 0), matching Go's plain `Sign`.
        let key = ed25519_blob(3);
        let fake = FakeAgent::start(Behavior::Serve {
            identities: vec![(key.clone(), "id_ed25519".to_string())],
            sig_format: "ssh-ed25519".to_string(),
            sig_blob: vec![7u8; 64],
        });
        let be = fake.backend(DEFAULT_IO_TIMEOUT);
        let sig = be.sign(&key, b"data", 0).unwrap();
        assert_eq!(sig.format, "ssh-ed25519");
        assert_eq!(sig.blob, vec![7u8; 64]);
        let (_, _, flags) = decode_sign_request(&fake.requests()[0]);
        assert_eq!(flags, 0);
    }

    #[test]
    fn sign_response_trailing_data_rejected() {
        // Go's ssh.Unmarshal(signResponseAgentMsg) rejects top-level trailing bytes
        // after the sigblob string; trailing bytes INSIDE the sigblob are the
        // `ssh:"rest"` field and stay accepted.
        let mut sigblob = Vec::new();
        write_string(&mut sigblob, b"ssh-ed25519");
        write_string(&mut sigblob, &[7u8; 64]);
        let mut ok_reply = vec![SSH_AGENT_SIGN_RESPONSE];
        write_string(&mut ok_reply, &sigblob);
        assert!(parse_signature(&ok_reply).is_ok());

        // Trailing junk after the sigblob string → Err (the cursor-review fix).
        let mut trailing = ok_reply.clone();
        trailing.extend_from_slice(b"junk");
        assert_eq!(parse_signature(&trailing).unwrap_err(), truncated());

        // Rest bytes inside the sigblob are still fine (Go Signature.Rest).
        let mut sigblob_rest = sigblob.clone();
        sigblob_rest.extend_from_slice(b"rest-bytes");
        let mut rest_reply = vec![SSH_AGENT_SIGN_RESPONSE];
        write_string(&mut rest_reply, &sigblob_rest);
        let sig = parse_signature(&rest_reply).unwrap();
        assert_eq!(sig.format, "ssh-ed25519");
        assert_eq!(sig.blob, vec![7u8; 64]);
    }

    #[test]
    fn agent_failure_maps_to_err() {
        let fake = FakeAgent::start(Behavior::Failure);
        let be = fake.backend(DEFAULT_IO_TIMEOUT);
        assert_eq!(be.list().unwrap_err(), "agent: failed to list keys");
        assert_eq!(
            be.sign(&ed25519_blob(4), b"d", 0).unwrap_err(),
            "agent: failed to sign challenge"
        );
    }

    #[test]
    fn garbage_type_maps_to_err() {
        let fake = FakeAgent::start(Behavior::GarbageType);
        let be = fake.backend(DEFAULT_IO_TIMEOUT);
        // Type 99 is an unknown tag → Go's `unknown type tag` shape.
        assert_eq!(
            be.list().unwrap_err(),
            "agent: client error: agent: unknown type tag 99"
        );
        assert_eq!(
            be.sign(&ed25519_blob(5), b"d", 0).unwrap_err(),
            "agent: client error: agent: unknown type tag 99"
        );
    }

    #[test]
    fn fresh_connection_per_op() {
        let key = ed25519_blob(6);
        let fake = FakeAgent::start(Behavior::Serve {
            identities: vec![(key.clone(), "id_ed25519".to_string())],
            sig_format: "ssh-ed25519".to_string(),
            sig_blob: vec![0u8; 64],
        });
        let be = fake.backend(DEFAULT_IO_TIMEOUT);
        be.list().unwrap();
        be.sign(&key, b"d", 0).unwrap();
        assert_eq!(fake.conn_count(), 2, "each op opens its own connection");
    }

    #[test]
    fn oversize_reply_rejected() {
        let fake = FakeAgent::start(Behavior::Oversize);
        // A short timeout proves the reject happens on the header (not by hanging on a
        // never-arriving body — the fake sends only the oversize length prefix).
        let be = fake.backend(Duration::from_secs(5));
        assert_eq!(
            be.list().unwrap_err(),
            "agent: client error: response too large"
        );
    }

    #[test]
    fn wedged_agent_times_out() {
        // The fake reads the request then withholds the reply past the client timeout.
        let fake = FakeAgent::start(Behavior::Hang(Duration::from_millis(600)));
        let be = fake.backend(Duration::from_millis(120));
        let start = std::time::Instant::now();
        let err = be.list().unwrap_err();
        assert!(
            start.elapsed() < Duration::from_millis(500),
            "must time out well before the fake's hang ({:?})",
            start.elapsed()
        );
        assert!(
            err.starts_with("agent: client error:"),
            "timeout surfaces as a client error: {err}"
        );
    }

    #[test]
    fn from_sock_requires_env() {
        assert_eq!(
            AgentForwardBackend::from_sock(None).unwrap_err(),
            "SSH_AUTH_SOCK not set"
        );
        assert_eq!(
            AgentForwardBackend::from_sock(Some(OsString::new())).unwrap_err(),
            "SSH_AUTH_SOCK not set"
        );
        // A non-empty value constructs without dialing (an unreachable path is fine).
        let be = AgentForwardBackend::from_sock(Some(OsString::from("/nonexistent/agent.sock")))
            .unwrap();
        assert_eq!(be.mode(), "agent-forward");
    }

    #[test]
    fn unreachable_socket_errors_on_op() {
        // Construction stores the path; the dial failure surfaces on the first op.
        let be =
            AgentForwardBackend::from_sock(Some(OsString::from("/nonexistent/agent.sock"))).unwrap();
        let err = be.list().unwrap_err();
        assert!(
            err.starts_with("connecting to SSH agent at /nonexistent/agent.sock:"),
            "{err}"
        );
    }
}
