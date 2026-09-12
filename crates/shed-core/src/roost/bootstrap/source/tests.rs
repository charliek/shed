//! The source ladder, against a real loopback HTTP server and real files.
//!
//! **Nothing here is mocked that could be real**, the same rule the machines'
//! rig follows. The fetch runs through a real `reqwest` client over a real
//! TCP connection to a real HTTP/1.1 server this module writes byte by byte —
//! which is what makes "a redirect to `http://` is refused" and "a body with no
//! `Content-Length` is still capped" assertions about the code rather than about
//! a mock's configuration. The ladder's local rungs run against real files and,
//! for the sibling, a real child process.
//!
//! Writing the server by hand rather than reaching for `httpmock` buys the three
//! shapes the contract is actually about and a request mocker will not produce:
//! a response that declares no length at all, a response that declares one and
//! then stops mid-body forever (the cancellation case), and a `Location` header
//! the client must refuse to follow.
//!
//! **What is NOT covered, and why:** rung 3 against the real
//! `github.com/charliek/roost` release. [`RELEASE_PIN`] is `None` because no
//! published roost release speaks session protocol
//! [`SESSION_PROTOCOL_VERSION`], so there is no live asset to fetch and no
//! honest way to fetch one. That is plan 019's stated external gate. Every
//! clause of the fetch contract is exercised here against the fixture; what is
//! untested is one URL.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::{TcpListener, TcpStream};

use roost_ipc::bootstrap::{asset_name, checksum_name, client_arch, BootstrapError, RemoteArch};
use roost_ipc::messages::SESSION_PROTOCOL_VERSION;
use sha2::Sha256;

use crate::roost::bootstrap::hex;
use crate::roost::testing::{write_exec, ScratchDir};

use super::*;

const TARGET: &str = "roost:popos/p019-a";

/// A version that is not [`LATEST_KNOWN_RELEASE`]'s — the tests that reach rung
/// 3 are about a pin that does not exist yet, and borrowing today's version
/// number would read like a claim that it does.
const VERSION: &str = "0.0.20";

// ============================================================================
// The fixture server
// ============================================================================

/// One canned answer. `Clone` because a route may be asked for twice.
#[derive(Clone)]
enum Reply {
    /// A body with a truthful `Content-Length`.
    Body(Vec<u8>),
    /// A body framed by the connection closing, with **no** `Content-Length` —
    /// the shape a declared-length check cannot bound, and the reason there is a
    /// counting check behind it.
    BodyUnframed(Vec<u8>),
    /// A status and nothing else.
    Status(u16),
    /// A `302` somewhere.
    Redirect { location: String },
    /// Headers, a few bytes of body, and then nothing, ever.
    Stall,
}

/// Something to do the moment a request arrives, before it is answered.
///
/// The fetch's own progress is what drives the fixture, so a hook on a known
/// route is a **synchronisation point** rather than a sleep: "the checksum is
/// being asked for" means the asset has been created, written and closed, and
/// nothing has been hashed yet. Two tests need to look at — or meddle with —
/// the filesystem at exactly that instant.
type Hook = Box<dyn Fn(&str) + Send + Sync>;

struct Fixture {
    base: String,
    routes: Arc<Mutex<HashMap<String, Reply>>>,
    hits: Arc<Mutex<Vec<String>>>,
    hook: Arc<Mutex<Option<Hook>>>,
    task: tokio::task::JoinHandle<()>,
    /// Every per-connection `serve` task, so `Drop` can abort them alongside
    /// the accept loop. `Reply::Stall` never returns on its own, and the
    /// accept loop's own `abort()` only ever reached itself — a connection
    /// already handed off to `serve` kept running until the test binary's
    /// runtime tore down, not until this fixture went out of scope.
    connections: Arc<Mutex<tokio::task::JoinSet<()>>>,
}

impl Drop for Fixture {
    fn drop(&mut self) {
        self.task.abort();
        self.connections
            .lock()
            .expect("the fixture's connection set")
            .abort_all();
    }
}

impl Fixture {
    async fn start() -> Fixture {
        let listener = TcpListener::bind("127.0.0.1:0")
            .await
            .expect("binding the fixture");
        let addr: SocketAddr = listener.local_addr().expect("the fixture's address");
        let routes: Arc<Mutex<HashMap<String, Reply>>> = Arc::default();
        let hits: Arc<Mutex<Vec<String>>> = Arc::default();
        let hook: Arc<Mutex<Option<Hook>>> = Arc::default();
        let connections: Arc<Mutex<tokio::task::JoinSet<()>>> = Arc::default();
        let task = tokio::spawn({
            let routes = Arc::clone(&routes);
            let hits = Arc::clone(&hits);
            let hook = Arc::clone(&hook);
            let connections = Arc::clone(&connections);
            async move {
                while let Ok((stream, _)) = listener.accept().await {
                    connections
                        .lock()
                        .expect("the fixture's connection set")
                        .spawn(serve(
                            stream,
                            Arc::clone(&routes),
                            Arc::clone(&hits),
                            Arc::clone(&hook),
                        ));
                }
            }
        });
        Fixture {
            base: format!("http://127.0.0.1:{}", addr.port()),
            routes,
            hits,
            hook,
            task,
            connections,
        }
    }

    /// Run `hook` on every request, with the path it asked for.
    fn on_hit(&self, hook: impl Fn(&str) + Send + Sync + 'static) {
        *self.hook.lock().expect("the fixture's hook") = Some(Box::new(hook));
    }

    fn route(&self, path: &str, reply: Reply) {
        self.routes
            .lock()
            .expect("the fixture's routes")
            .insert(path.to_string(), reply);
    }

    fn hits(&self) -> Vec<String> {
        self.hits.lock().expect("the fixture's hit log").clone()
    }

    /// The published names, exactly as [`asset_name`] builds them.
    fn asset_path(&self) -> String {
        format!("/{}", asset_name(VERSION, RemoteArch::Amd64))
    }

    fn checksum_path(&self) -> String {
        format!(
            "/{}",
            checksum_name(&asset_name(VERSION, RemoteArch::Amd64))
        )
    }

    /// An asset and a `.sha256` that covers it — the good release.
    fn publish(&self, bytes: &[u8]) {
        self.route(&self.asset_path(), Reply::Body(bytes.to_vec()));
        self.publish_checksum(&format!(
            "{}  {}\n",
            hex(Sha256::digest(bytes).as_slice()),
            asset_name(VERSION, RemoteArch::Amd64)
        ));
    }

    fn publish_checksum(&self, body: &str) {
        self.route(&self.checksum_path(), Reply::Body(body.as_bytes().to_vec()));
    }
}

async fn serve(
    mut stream: TcpStream,
    routes: Arc<Mutex<HashMap<String, Reply>>>,
    hits: Arc<Mutex<Vec<String>>>,
    hook: Arc<Mutex<Option<Hook>>>,
) {
    let mut head = Vec::new();
    let mut buffer = [0u8; 1024];
    loop {
        let Ok(read) = stream.read(&mut buffer).await else {
            return;
        };
        if read == 0 {
            return;
        }
        head.extend_from_slice(&buffer[..read]);
        if head.windows(4).any(|window| window == b"\r\n\r\n") {
            break;
        }
        if head.len() > 8 * 1024 {
            return;
        }
    }
    let request = String::from_utf8_lossy(&head).into_owned();
    let path = request.split_whitespace().nth(1).unwrap_or("/").to_string();
    hits.lock()
        .expect("the fixture's hit log")
        .push(path.clone());
    if let Some(hook) = hook.lock().expect("the fixture's hook").as_ref() {
        hook(&path);
    }
    let reply = routes
        .lock()
        .expect("the fixture's routes")
        .get(&path)
        .cloned()
        .unwrap_or(Reply::Status(404));
    let _ = write_reply(&mut stream, reply).await;
}

async fn write_reply(stream: &mut TcpStream, reply: Reply) -> std::io::Result<()> {
    match reply {
        Reply::Body(body) => {
            let head = format!(
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            );
            stream.write_all(head.as_bytes()).await?;
            stream.write_all(&body).await?;
        }
        Reply::BodyUnframed(body) => {
            stream
                .write_all(b"HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n")
                .await?;
            stream.write_all(&body).await?;
        }
        Reply::Status(code) => {
            let head = format!(
                "HTTP/1.1 {code} Refused\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
            );
            stream.write_all(head.as_bytes()).await?;
        }
        Reply::Redirect { location } => {
            let head = format!(
                "HTTP/1.1 302 Found\r\nLocation: {location}\r\nContent-Length: 0\r\n\
                 Connection: close\r\n\r\n"
            );
            stream.write_all(head.as_bytes()).await?;
        }
        Reply::Stall => {
            stream
                .write_all(
                    b"HTTP/1.1 200 OK\r\nContent-Length: 65536\r\nConnection: close\r\n\r\nhalf a ",
                )
                .await?;
            stream.flush().await?;
            // Never answers, never closes. The only way out is the caller
            // giving up, which is the case under test.
            std::future::pending::<()>().await;
        }
    }
    stream.shutdown().await
}

// ============================================================================
// Helpers
// ============================================================================

/// A scratch directory, and a path *inside* it for a fetch to create itself.
fn scratch() -> (ScratchDir, std::path::PathBuf) {
    let dir = ScratchDir::with_prefix("shed-bootstrap-source");
    let target = dir.0.join("fetch");
    (dir, target)
}

fn small_limits() -> Limits {
    Limits {
        asset_max: 1024,
        checksum_max: 1024,
    }
}

async fn fetch(
    fixture: &Fixture,
    dir: &Path,
    limits: Limits,
) -> Result<SourceHandle, BootstrapError> {
    fetch_release_limited(&fixture.base, VERSION, RemoteArch::Amd64, dir, limits).await
}

/// Everything a handle holds, read back through the descriptor it owns.
fn drain(handle: &SourceHandle) -> Vec<u8> {
    let mut out = Vec::new();
    loop {
        let chunk = handle.read_chunk(4096).expect("reading the source handle");
        if chunk.is_empty() {
            return out;
        }
        out.extend_from_slice(&chunk);
    }
}

fn write_file(path: &Path, bytes: &[u8]) {
    std::fs::create_dir_all(path.parent().expect("a parent")).expect("mkdir");
    std::fs::write(path, bytes).expect("writing a test file");
}

/// A minimal ELF header `sniff_binary` can read: `e_machine` at offset 18, in
/// the endianness `EI_DATA` declares.
fn elf_header(machine: u16) -> Vec<u8> {
    let mut bytes = vec![0u8; 64];
    bytes[..4].copy_from_slice(&[0x7f, b'E', b'L', b'F']);
    bytes[4] = 2; // ELFCLASS64
    bytes[5] = 1; // ELFDATA2LSB
    bytes[18..20].copy_from_slice(&machine.to_le_bytes());
    bytes
}

const EM_X86_64: u16 = 0x3e;
const EM_AARCH64: u16 = 0xb7;
/// 32-bit ARM — a real machine number roost publishes no build for.
const EM_ARM: u16 = 0x28;

/// A `roost-session` stand-in that answers `identify` with one JSON line, the
/// way the real binary does.
fn fake_session(app_version: &str, protocol: u32) -> String {
    format!(
        r#"#!/bin/sh
case "$1" in
  identify)
    printf '%s\n' '{{"app_version":"{app_version}","session_protocol":{protocol},"libghostty_build":"ghostty-f2d5758f6305867d+snapshot.v1"}}'
    ;;
  *) exit 2 ;;
esac
"#
    )
}

/// A `roost-session` stand-in that writes 256 KiB of nothing at `identify`
/// before it says anything true — the hostile-or-broken local binary the
/// sibling rung executes *before* anything has verified it.
///
/// It touches `finished` only if it is allowed to write the whole flood, which
/// is what makes "the read stopped at the cap" observable from outside the
/// process rather than inferred from a timing.
fn flooding_session(finished: &Path) -> String {
    let chunk = "x".repeat(4096);
    format!(
        r#"#!/bin/sh
case "$1" in
  identify)
    i=0
    while [ "$i" -lt 64 ]; do
      printf '%s' '{chunk}' || exit 3
      i=$((i+1))
    done
    : > '{finished}'
    printf '%s\n' '{{"app_version":"0.0.19","session_protocol":4,"libghostty_build":"x"}}'
    ;;
  *) exit 2 ;;
esac
"#,
        finished = finished.display()
    )
}

/// A `roost-session` stand-in that never answers `identify` and never exits,
/// touching `heartbeat` every 100 ms for as long as it is alive.
///
/// The heartbeat is how "the child was killed" is asserted without racing a
/// zombie reaper: delete the file, wait, and see whether anything recreates it.
fn heartbeat_session(heartbeat: &Path) -> String {
    format!(
        r#"#!/bin/sh
case "$1" in
  identify)
    while : ; do
      : > '{heartbeat}'
      sleep 0.1
    done
    ;;
  *) exit 2 ;;
esac
"#,
        heartbeat = heartbeat.display()
    )
}

/// What a directory holds right now, or one entry saying why that could not be
/// read — never a panic, because this runs inside the fixture's connection task.
fn list_dir(dir: &Path) -> Vec<String> {
    match std::fs::read_dir(dir) {
        Ok(entries) => entries
            .map(|entry| match entry {
                Ok(entry) => entry.file_name().to_string_lossy().into_owned(),
                Err(error) => format!("<unreadable entry: {error}>"),
            })
            .collect(),
        Err(error) => vec![format!("<unreadable directory: {error}>")],
    }
}

/// `mkfifo(3)` — the one file type that makes a plain `open(2)` block forever.
fn make_fifo(path: &Path) {
    use std::os::unix::ffi::OsStrExt as _;

    let raw = std::ffi::CString::new(path.as_os_str().as_bytes()).expect("a path with no NUL");
    // SAFETY: `raw` is a NUL-terminated path that outlives the call, and
    // `mkfifo` reads nothing else.
    let made = unsafe { libc::mkfifo(raw.as_ptr(), 0o600) };
    assert_eq!(
        made,
        0,
        "mkfifo {}: {}",
        path.display(),
        std::io::Error::last_os_error()
    );
}

/// The other architecture from this build's own — the arch-mismatch row.
fn other_arch(arch: RemoteArch) -> RemoteArch {
    match arch {
        RemoteArch::Amd64 => RemoteArch::Arm64,
        RemoteArch::Arm64 => RemoteArch::Amd64,
    }
}

/// A `SourceEnv` that reads **nothing** from the real environment.
///
/// Load-bearing: this developer's machine has a real `/usr/bin/roost-session`
/// (protocol 2), so a ladder handed the real `PATH` would find it and a test
/// about a cold client would quietly be a test about this laptop.
fn sealed_env() -> SourceEnv {
    SourceEnv::default()
}

// ============================================================================
// The pin
// ============================================================================

/// The external gate, asserted rather than described.
#[test]
fn nothing_is_pinned_until_a_protocol_4_release_ships() {
    assert!(
        RELEASE_PIN.is_none(),
        "plan 019 §3.5: the asset rung stays behind a pin until a roost release speaks \
         protocol {SESSION_PROTOCOL_VERSION}"
    );
    assert_eq!(LATEST_KNOWN_RELEASE, ("0.0.19", 2));
    assert_ne!(
        LATEST_KNOWN_RELEASE.1, SESSION_PROTOCOL_VERSION,
        "if these ever agree, the pin above is what needs editing — not this test"
    );
}

/// Plan 019 §3.5's sentence, word for word, and built from the constants beside
/// it rather than typed out.
#[test]
fn the_no_source_copy_is_the_one_the_plan_pinned() {
    assert_eq!(
        unavailable(TARGET),
        "no roost release speaking session protocol 4 is published yet (the latest, 0.0.19, \
         speaks 2). On a Linux machine with a protocol-4 roost installed the desktop uses that \
         roost-session; otherwise point ROOST_SESSION_INSTALL_BIN at a protocol-4 build. \
         roost:popos/p019-a was left untouched."
    );

    let failure = no_source(TARGET);
    assert_eq!(failure.stage, Stage::Source);
    assert_eq!(failure.message, unavailable(TARGET));

    // The numbers are read, not written: a pin-flip PR that edits the constants
    // and forgets the copy is the failure this catches.
    let (version, protocol) = LATEST_KNOWN_RELEASE;
    assert!(unavailable(TARGET).contains(&format!("the latest, {version}, speaks {protocol}")));
    assert!(unavailable(TARGET).contains(&format!("session protocol {SESSION_PROTOCOL_VERSION}")));
}

/// With no pin there is no asset rung, so a client with nothing else lands on
/// rung 4 — and the preview says why.
#[tokio::test]
async fn a_client_with_no_rungs_lands_on_no_source() {
    let (_dir, scratch_path) = scratch();
    let env = sealed_env();

    let previewed = preview(&env, TARGET, "amd64");
    assert_eq!(previewed.source, Source::None);
    assert!(!previewed.available());
    assert_eq!(previewed.describe(TARGET), unavailable(TARGET));
    assert!(
        previewed
            .skipped
            .iter()
            .any(|why| why.contains("no roost release is pinned")),
        "the preview says the asset rung is gated: {:?}",
        previewed.skipped
    );

    let failure = resolve(&env, TARGET, "amd64", &scratch_path)
        .await
        .expect_err("nothing can supply these bytes");
    assert_eq!(failure.stage, Stage::Source);
    assert_eq!(failure.message, unavailable(TARGET));
}

// ============================================================================
// Rung 1 — the override
// ============================================================================

#[tokio::test]
async fn the_override_rung_takes_the_file_it_is_pointed_at() {
    let (dir, scratch_path) = scratch();
    let bin = dir.0.join("a-roost-session");
    let bytes = elf_header(EM_X86_64);
    write_file(&bin, &bytes);

    let env = SourceEnv {
        install_bin: Some(bin.clone()),
        ..sealed_env()
    };
    let handle = resolve(&env, TARGET, "amd64", &scratch_path)
        .await
        .expect("the override rung");

    assert_eq!(
        handle.origin(),
        format!("{} (ROOST_SESSION_INSTALL_BIN)", bin.display())
    );
    assert_eq!(handle.len(), bytes.len() as u64);
    assert_eq!(
        handle.sha256(),
        None,
        "no published checksum exists for a local file"
    );
    // The descriptor was rewound after the sniff: a handle that starts 64 bytes
    // in would stream a truncated binary and the far side would refuse it with
    // a mystifying error.
    assert_eq!(drain(&handle), bytes);
}

/// An override wins over a rung that would otherwise have answered.
#[tokio::test]
async fn the_override_rung_wins_over_the_sibling() {
    let Some(local) = client_arch() else {
        return; // off Linux there is no sibling rung to lose. See below.
    };
    let (dir, scratch_path) = scratch();
    let app = dir.0.join("app");
    write_exec(&app.join("roost-session"), &fake_session("0.0.19", 4));
    let override_bin = dir.0.join("chosen");
    let machine = match local {
        RemoteArch::Amd64 => EM_X86_64,
        RemoteArch::Arm64 => EM_AARCH64,
    };
    write_file(&override_bin, &elf_header(machine));

    let env = SourceEnv {
        install_bin: Some(override_bin.clone()),
        caller_exe: Some(app.join("shed-desktop")),
        ..sealed_env()
    };
    let handle = resolve(&env, TARGET, local.as_str(), &scratch_path)
        .await
        .expect("the override rung");
    assert!(handle.origin().contains("ROOST_SESSION_INSTALL_BIN"));
}

/// Every refusal [`sniff_binary`] can produce, plus the unreadable file, and the
/// one promise each of them has to keep.
#[tokio::test]
async fn the_override_rung_refuses_what_sniff_binary_can_see_is_wrong() {
    let (dir, scratch_path) = scratch();

    let cases: Vec<(&str, Vec<u8>, &str)> = vec![
        (
            "mach-o",
            vec![0xcf, 0xfa, 0xed, 0xfe, 0, 0, 0, 0],
            "is a macOS binary, not a Linux one",
        ),
        (
            "wrong-arch",
            elf_header(EM_AARCH64),
            "is an arm64 Linux binary but roost:popos/p019-a is amd64",
        ),
        (
            "other-machine",
            elf_header(EM_ARM),
            "is an ELF for machine 0x28, not amd64/arm64",
        ),
    ];
    for (name, bytes, expected) in cases {
        let bin = dir.0.join(name);
        write_file(&bin, &bytes);
        let env = SourceEnv {
            install_bin: Some(bin),
            ..sealed_env()
        };
        let failure = resolve(&env, TARGET, "amd64", &scratch_path)
            .await
            .expect_err("the sniff refuses this");
        assert_eq!(failure.stage, Stage::Source, "{name}");
        assert!(
            failure.message.contains(expected),
            "{name}: {}",
            failure.message
        );
        // roost's Source copy, kept: the one thing a user needs from a refusal
        // is that the host is untouched.
        assert!(
            failure
                .message
                .contains("roost:popos/p019-a was left untouched."),
            "{name}: {}",
            failure.message
        );
    }

    // A named override that cannot be read is a hard error, never a
    // fall-through — the user asked for a specific binary.
    let env = SourceEnv {
        install_bin: Some(dir.0.join("not-there")),
        ..sealed_env()
    };
    let failure = resolve(&env, TARGET, "amd64", &scratch_path)
        .await
        .expect_err("a missing override is a refusal");
    assert!(
        failure.message.contains("could not be read"),
        "{}",
        failure.message
    );

    // A directory is not a file, however readable it is.
    let env = SourceEnv {
        install_bin: Some(dir.0.clone()),
        ..sealed_env()
    };
    let failure = resolve(&env, TARGET, "amd64", &scratch_path)
        .await
        .expect_err("a directory is not a roost-session");
    assert!(
        failure.message.contains("is not a regular file"),
        "{}",
        failure.message
    );
}

/// A script, a short file, an unrecognized format — roost's rule is that these
/// pass through and the far side's staged verify decides.
#[tokio::test]
async fn the_override_rung_passes_through_what_it_cannot_identify() {
    let (dir, scratch_path) = scratch();
    let bin = dir.0.join("a-script");
    write_file(&bin, b"#!/bin/sh\necho hello\n");
    let env = SourceEnv {
        install_bin: Some(bin),
        ..sealed_env()
    };
    let handle = resolve(&env, TARGET, "amd64", &scratch_path)
        .await
        .expect("an unrecognized format is the far side's problem");
    assert_eq!(drain(&handle), b"#!/bin/sh\necho hello\n");
}

/// The file types the override can be pointed at that are not files — and the
/// one indirection that is fine.
///
/// The FIFO row is the one that used to **hang**: the guard checks
/// `metadata()` on the open descriptor rather than on the path, which is right,
/// but `File::open` on a FIFO blocks until a writer appears — one line before
/// the `is_file()` that exists to refuse it. It is the user's own variable
/// pointing at the user's own FIFO, so it is a self-inflicted hang rather than
/// an attack; it was still the one case the guard did not actually answer.
/// `O_NONBLOCK` is what lets the open return so the refusal can happen.
#[tokio::test]
async fn the_override_rung_answers_a_fifo_a_directory_and_a_symlink() {
    let (dir, scratch_path) = scratch();

    // A FIFO nobody will ever write to.
    let fifo = dir.0.join("a-fifo");
    make_fifo(&fifo);
    let env = SourceEnv {
        install_bin: Some(fifo),
        ..sealed_env()
    };
    // **On a thread of its own, with a channel deadline** rather than a
    // `tokio::time::timeout`: a blocking `open(2)` blocks the *task*, so the
    // timeout future wrapped around it can never be polled to fire, and even a
    // multi-threaded runtime then waits for that worker at shutdown. A detached
    // thread is the only shape in which the regression is a failure at a
    // deadline instead of a suite that never finishes.
    let (answered, answer) = std::sync::mpsc::channel();
    std::thread::spawn({
        let env = env.clone();
        let scratch_path = scratch_path.clone();
        move || {
            let runtime = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("a runtime for the blocking-open probe");
            let _ = answered.send(runtime.block_on(resolve(&env, TARGET, "amd64", &scratch_path)));
        }
    });
    let failure = answer
        .recv_timeout(Duration::from_secs(5))
        .expect("opening the override must not block on a FIFO")
        .expect_err("a FIFO is not a roost-session");
    assert!(
        failure.message.contains("is not a regular file"),
        "{}",
        failure.message
    );

    // A directory — openable, readable, still not a file.
    let env = SourceEnv {
        install_bin: Some(dir.0.clone()),
        ..sealed_env()
    };
    let failure = resolve(&env, TARGET, "amd64", &scratch_path)
        .await
        .expect_err("a directory is not a roost-session");
    assert!(
        failure.message.contains("is not a regular file"),
        "{}",
        failure.message
    );

    // A symlink to a real binary is **accepted**. The hazard the guard is
    // about is a file that is not a file; an indirection to a regular one is
    // how half the packaging layouts on a Linux box ship a binary.
    let bytes = elf_header(EM_X86_64);
    let real = dir.0.join("a-real-one");
    write_file(&real, &bytes);
    let link = dir.0.join("a-link");
    std::os::unix::fs::symlink(&real, &link).expect("symlink");
    let env = SourceEnv {
        install_bin: Some(link.clone()),
        ..sealed_env()
    };
    let handle = resolve(&env, TARGET, "amd64", &scratch_path)
        .await
        .expect("a symlink to a regular file is a source");
    assert_eq!(
        handle.origin(),
        format!("{} (ROOST_SESSION_INSTALL_BIN)", link.display())
    );
    assert_eq!(drain(&handle), bytes);
}

// ============================================================================
// Rung 2 — the sibling
// ============================================================================

/// The rung P3 puts the Linux desktop on.
///
/// Skipped off Linux **by the code under test**: [`client_arch`] is a
/// compile-time fact, so a Mac cannot take this rung and the assertion below is
/// that it says so rather than that it works.
#[tokio::test]
async fn the_sibling_rung_is_taken_when_it_speaks_protocol_4() {
    let (dir, scratch_path) = scratch();
    let app = dir.0.join("app");
    let session = app.join("roost-session");
    write_exec(&session, &fake_session("0.0.19", 4));
    let env = SourceEnv {
        caller_exe: Some(app.join("shed-desktop")),
        ..sealed_env()
    };

    let Some(local) = client_arch() else {
        let previewed = preview(&env, TARGET, "amd64");
        assert_eq!(previewed.source, Source::None);
        assert!(
            previewed
                .skipped
                .iter()
                .any(|why| why.contains("this shed is not a Linux build")),
            "{:?}",
            previewed.skipped
        );
        return;
    };

    let previewed = preview(&env, TARGET, local.as_str());
    assert_eq!(
        previewed.source,
        Source::Sibling {
            path: session.display().to_string()
        }
    );
    // No pin, so the fall-through is nothing — and the card says so rather than
    // promising a sibling it has not yet run.
    assert_eq!(previewed.fallback, Some(Source::None));
    assert_eq!(
        previewed.describe(TARGET),
        format!(
            "the roost-session beside this app ({}) — and nothing else, if it turns out not to \
             speak session protocol 4",
            session.display()
        )
    );

    let handle = resolve(&env, TARGET, local.as_str(), &scratch_path)
        .await
        .expect("the sibling rung");
    assert_eq!(
        handle.origin(),
        format!("the roost-session beside this app ({})", session.display())
    );
    assert_eq!(handle.sha256(), None);
    assert_eq!(drain(&handle), fake_session("0.0.19", 4).into_bytes());
}

/// Pin P3's arch gate, and the copy that names the arch.
#[tokio::test]
async fn a_sibling_for_another_architecture_is_skipped_and_the_copy_names_it() {
    let (dir, scratch_path) = scratch();
    let app = dir.0.join("app");
    write_exec(&app.join("roost-session"), &fake_session("0.0.19", 4));
    let env = SourceEnv {
        caller_exe: Some(app.join("shed-desktop")),
        ..sealed_env()
    };

    let expected = match client_arch() {
        Some(local) => format!(
            "this shed is an {local} build and {TARGET} is {}",
            other_arch(local)
        ),
        None => "this shed is not a Linux build, so the roost-session beside it can't run on a \
                 Linux host"
            .to_string(),
    };
    let remote = client_arch().map_or(RemoteArch::Arm64, other_arch);

    let previewed = preview(&env, TARGET, remote.as_str());
    assert_eq!(previewed.source, Source::None);
    assert!(
        previewed.skipped.contains(&expected),
        "the skip names the arch: {:?}",
        previewed.skipped
    );

    // And the ladder really does fall past it, rather than merely saying so.
    let failure = resolve(&env, TARGET, remote.as_str(), &scratch_path)
        .await
        .expect_err("a sibling for another arch is not a source");
    assert_eq!(failure.message, unavailable(TARGET));
}

/// shed's gate is the protocol number: today's released `roost-session` speaks
/// 2, and it is not a source however real a binary it is.
#[tokio::test]
async fn a_sibling_that_speaks_the_wrong_protocol_falls_through() {
    let Some(local) = client_arch() else {
        return; // covered by the arch row above.
    };
    let (dir, scratch_path) = scratch();
    let app = dir.0.join("app");
    write_exec(
        &app.join("roost-session"),
        &fake_session(LATEST_KNOWN_RELEASE.0, LATEST_KNOWN_RELEASE.1),
    );
    let env = SourceEnv {
        caller_exe: Some(app.join("shed-desktop")),
        ..sealed_env()
    };

    // The preview cannot know — it does not run the local `identify` — so it
    // offers the sibling with the fall-through clause, and the resolution is
    // what lands on rung 4.
    assert!(matches!(
        preview(&env, TARGET, local.as_str()).source,
        Source::Sibling { .. }
    ));
    let failure = resolve(&env, TARGET, local.as_str(), &scratch_path)
        .await
        .expect_err("protocol 2 is not a source");
    assert_eq!(failure.message, unavailable(TARGET));
}

/// A binary too old to know `identify` at all is the same answer.
#[tokio::test]
async fn a_sibling_that_will_not_identify_falls_through() {
    let Some(local) = client_arch() else {
        return;
    };
    let (dir, scratch_path) = scratch();
    let app = dir.0.join("app");
    write_exec(&app.join("roost-session"), "#!/bin/sh\nexit 2\n");
    let env = SourceEnv {
        caller_exe: Some(app.join("shed-desktop")),
        ..sealed_env()
    };
    let failure = resolve(&env, TARGET, local.as_str(), &scratch_path)
        .await
        .expect_err("a binary that says nothing is not a source");
    assert_eq!(failure.message, unavailable(TARGET));
}

/// A candidate that floods its stdout is read **to the cap and no further**.
///
/// This rung executes the candidate before anything has verified it, so the
/// flood precedes every compatibility check there is. `wait_with_output()` read
/// to EOF and capped the buffer *afterwards*, which caps nothing: a binary
/// writing continuously grew that allocation at pipe throughput for the whole
/// budget. The marker file is the assertion — the script can only touch it if
/// it was allowed to write all 256 KiB.
#[tokio::test]
async fn a_flooding_identify_is_read_only_to_the_cap() {
    let Some(local) = client_arch() else {
        return; // no sibling rung off Linux; see the arch row above.
    };
    let (dir, scratch_path) = scratch();
    let app = dir.0.join("app");
    let finished = dir.0.join("the-flood-ran-to-completion");
    write_exec(&app.join("roost-session"), &flooding_session(&finished));
    let env = SourceEnv {
        caller_exe: Some(app.join("shed-desktop")),
        ..sealed_env()
    };

    let failure = resolve(&env, TARGET, local.as_str(), &scratch_path)
        .await
        .expect_err("a binary that answers `identify` with noise is not a source");
    assert_eq!(failure.message, unavailable(TARGET));
    assert!(
        !finished.exists(),
        "the flood was read to the end: the cap slices a buffer instead of \
         bounding the read"
    );
}

/// A candidate that never answers is timed out — and the child does not outlive
/// the answer nobody is waiting for.
///
/// Driven through [`local_identify_within`] with a 300 ms budget rather than
/// [`LOCAL_IDENTIFY_BUDGET`]: the budget's *value* is a constant this file
/// already reads, and what is under test is that it has teeth.
#[tokio::test]
async fn an_identify_that_never_exits_is_timed_out_and_its_child_killed() {
    let dir = ScratchDir::with_prefix("shed-bootstrap-hang");
    let heartbeat = dir.0.join("heartbeat");
    let session = dir.0.join("roost-session");
    write_exec(&session, &heartbeat_session(&heartbeat));

    let why = local_identify_within(&session, Duration::from_millis(300))
        .await
        .expect_err("a binary that never answers has no identity");
    assert!(why.contains("identify timed out"), "{why}");

    // Not merely abandoned — gone. The script recreates the heartbeat every
    // 100 ms for as long as it lives, so deleting it and looking again is a
    // liveness question the process answers itself.
    std::fs::remove_file(&heartbeat).expect("the script beat at least once before the budget");
    tokio::time::sleep(Duration::from_millis(600)).await;
    assert!(
        !heartbeat.exists(),
        "the identify child outlived the budget that timed it out"
    );
}

// ============================================================================
// Rung 3 — the fetch
// ============================================================================

/// The property the whole rung exists for: the bytes that were hashed are the
/// bytes the handle streams, and there is no name left to re-open.
#[tokio::test]
async fn the_fetch_hands_back_the_descriptor_it_hashed() {
    let fixture = Fixture::start().await;
    let (_dir, scratch_path) = scratch();
    let bytes: Vec<u8> = (0..4096u32).map(|byte| byte as u8).collect();
    fixture.publish(&bytes);

    let handle = fetch(&fixture, &scratch_path, Limits::DEFAULT)
        .await
        .expect("the release fetches");

    assert_eq!(handle.len(), 4096);
    assert_eq!(
        handle.sha256(),
        Some(hex(Sha256::digest(&bytes).as_slice()).as_str())
    );
    assert_eq!(
        handle.origin(),
        format!(
            "roost-session {VERSION} from {}, checksum-verified",
            fixture.base
        )
    );
    assert_eq!(drain(&handle), bytes);
    // Read back *after* the directory is gone: the handle owns an inode, not a
    // path, which is what makes "what was checked is what is sent" true.
    assert!(
        !scratch_path.exists(),
        "the scratch directory outlived the fetch"
    );
    handle.rewind().expect("a handle rewinds");
    assert_eq!(drain(&handle), bytes);
}

/// The asset is **unlinked the moment it is created**, so the window between
/// the hash and the stream has no name in it.
///
/// Written to a predictable path in a 0700 directory and removed afterwards,
/// the file is openable by a same-UID process for the whole download: it could
/// hold a writable descriptor and swap the contents after `fetch_release`
/// returned, and the handle would stream bytes nobody hashed. That is not a
/// boundary shed defends elsewhere — but "hashed equals streamed" should not
/// rest on winning a race, and unlinking at open removes the window instead of
/// narrowing it.
///
/// The checksum request is the synchronisation point: by then the asset has
/// been created, written and closed, and nothing has been hashed.
#[tokio::test]
async fn the_downloaded_asset_has_no_name_while_it_is_being_fetched() {
    let fixture = Fixture::start().await;
    let (_dir, scratch_path) = scratch();
    let bytes = vec![11u8; 2048];
    fixture.publish(&bytes);

    let seen: Arc<Mutex<Option<Vec<String>>>> = Arc::default();
    fixture.on_hit({
        let seen = Arc::clone(&seen);
        let scratch_path = scratch_path.clone();
        let checksum = fixture.checksum_path();
        move |path| {
            if path == checksum {
                *seen.lock().expect("the listing") = Some(list_dir(&scratch_path));
            }
        }
    });

    let handle = fetch(&fixture, &scratch_path, Limits::DEFAULT)
        .await
        .expect("the release fetches");

    let listing = seen
        .lock()
        .expect("the listing")
        .clone()
        .expect("the checksum was fetched, so the hook ran");
    assert!(
        listing.is_empty(),
        "the scratch directory still named the download while it was open: {listing:?}"
    );
    // And the handle is entirely usable without a name.
    assert_eq!(drain(&handle), bytes);
    assert_eq!(
        handle.sha256(),
        Some(hex(Sha256::digest(&bytes).as_slice()).as_str())
    );
}

/// A body that crosses [`WRITE_BATCH`] lands byte for byte.
///
/// The writes go across `spawn_blocking` in batches — a blocking `write_all`
/// per chunk on a tokio worker is what a 256 MiB transfer would otherwise hold
/// one for, in the same function that already knew to send its hashing to a
/// blocking thread. Batching is where that could go wrong: a boundary that
/// reorders, or a trailing partial batch that never gets written, produces a
/// file the checksum then refuses. Several megabytes are needed to reach a
/// second batch at all, which is why this row is fat and the others are not.
#[tokio::test]
async fn a_body_larger_than_one_write_batch_lands_intact() {
    let fixture = Fixture::start().await;
    let (_dir, scratch_path) = scratch();
    // Two full batches and a short one, with position-dependent bytes so a
    // reordering is a different hash rather than the same one.
    let bytes: Vec<u8> = (0..(2 * WRITE_BATCH + 7919))
        .map(|index| (index % 251) as u8)
        .collect();
    fixture.publish(&bytes);

    let handle = fetch(&fixture, &scratch_path, Limits::DEFAULT)
        .await
        .expect("a multi-batch body fetches");
    assert_eq!(handle.len(), bytes.len() as u64);
    assert_eq!(
        handle.sha256(),
        Some(hex(Sha256::digest(&bytes).as_slice()).as_str())
    );
    assert_eq!(drain(&handle), bytes);
}

/// Cleanup removes the directory it **created**, identified by `(dev, ino)` —
/// never whatever a pathname happens to name by the time it runs.
///
/// `remove_dir_all` takes a string. If an ancestor of the caller's `dir` is a
/// symlink somebody retargets after the create, that string names somebody
/// else's directory: the old cleanup deleted theirs and left the real one
/// behind, and discarded the result, so nothing said so. The retarget happens
/// here at the checksum request, which is inside the fetch.
#[tokio::test]
async fn cleanup_refuses_a_directory_it_did_not_create_and_says_so() {
    let fixture = Fixture::start().await;
    let dir = ScratchDir::with_prefix("shed-bootstrap-swap");
    let real = dir.0.join("real");
    let decoy = dir.0.join("decoy");
    std::fs::create_dir_all(&real).expect("mkdir");
    write_file(&decoy.join("fetch").join("somebody-elses-file"), b"mine");

    // `link` → `real` while `link/fetch` is created; `link` → `decoy` by the
    // time the cleanup looks at that same pathname.
    let link = dir.0.join("link");
    std::os::unix::fs::symlink(&real, &link).expect("symlink");
    let scratch_path = link.join("fetch");

    fixture.publish(&[5u8; 64]);
    fixture.on_hit({
        let link = link.clone();
        let decoy = decoy.clone();
        let checksum = fixture.checksum_path();
        move |path| {
            if path == checksum {
                let _ = std::fs::remove_file(&link);
                let _ = std::os::unix::fs::symlink(&decoy, &link);
            }
        }
    });

    let error = fetch(&fixture, &scratch_path, Limits::DEFAULT)
        .await
        .expect_err("a cleanup that cannot happen safely is reported, not swallowed");
    assert!(
        matches!(&error, BootstrapError::Download(detail)
            if detail.contains("no longer the directory this fetch created")),
        "{error:?}"
    );
    assert!(
        decoy.join("fetch").join("somebody-elses-file").exists(),
        "the cleanup deleted a directory it did not create"
    );
    // What it did create is still there — the honest trade. A leaked scratch
    // directory beats deleting somebody else's, and the refusal above is what
    // tells a caller it happened.
    assert!(real.join("fetch").exists());
}

/// `https://` only — and the loopback carve-out does **not** extend to a
/// `Location` header.
#[tokio::test]
async fn a_redirect_to_plain_http_is_refused() {
    let fixture = Fixture::start().await;
    let (_dir, scratch_path) = scratch();
    fixture.publish(&[1, 2, 3, 4]);
    fixture.route(
        &fixture.asset_path(),
        Reply::Redirect {
            location: format!("{}/elsewhere", fixture.base),
        },
    );
    fixture.route("/elsewhere", Reply::Body(vec![9, 9, 9, 9]));

    let error = fetch(&fixture, &scratch_path, Limits::DEFAULT)
        .await
        .expect_err("a plaintext redirect is a downgrade");
    let BootstrapError::Download(detail) = &error else {
        panic!("expected a download refusal, got {error:?}");
    };
    assert!(
        detail.contains("shed follows a redirect only to an https:// URL"),
        "{detail}"
    );
    assert!(
        !fixture.hits().iter().any(|path| path == "/elsewhere"),
        "the redirect was followed: {:?}",
        fixture.hits()
    );
    assert!(!scratch_path.exists());
}

/// The policy stops for **two** reasons, and a refusal must name its own.
///
/// Exceeding [`MAX_REDIRECTS`] and a next hop that is not `https://` used to
/// produce one sentence — so a base that redirected through six perfectly good
/// https hops told the user their URL was not https. The single redirect test
/// above is the plain-http one, which is exactly why the conflation went
/// unnoticed.
///
/// **Why this asserts the decision rather than a chain over the wire:** a hop
/// is only ever *followed* when it is `https://`, so a chain long enough to
/// reach the limit needs a TLS server — and there is no way here to hand the
/// real `reqwest` client a fixture certificate to trust (no test CA, and no
/// dependency may be added for one). So the two stop reasons are lifted into
/// [`redirect_verdict`], which is where the bug was, and the scheme half stays
/// asserted end-to-end against the live fixture in
/// [`a_redirect_to_plain_http_is_refused`]. What is not covered is the wire leg
/// of the hop-limit branch — stated, not glossed.
#[test]
fn the_two_redirect_stop_reasons_are_told_apart() {
    assert_eq!(redirect_verdict(0, "https"), Redirect::Follow);
    assert_eq!(
        redirect_verdict(MAX_REDIRECTS - 1, "https"),
        Redirect::Follow
    );
    assert_eq!(
        redirect_verdict(MAX_REDIRECTS, "https"),
        Redirect::TooManyHops
    );
    // The hop limit outranks the scheme: a chain already too long is too long
    // whatever the next hop would have been.
    assert_eq!(
        redirect_verdict(MAX_REDIRECTS, "http"),
        Redirect::TooManyHops
    );
    assert_eq!(redirect_verdict(0, "http"), Redirect::NotHttps);
    assert_eq!(redirect_verdict(0, "ftp"), Redirect::NotHttps);

    let hops = too_many_redirects("https://roost.example/download/asset");
    assert!(
        hops.contains(&MAX_REDIRECTS.to_string()),
        "the hop-limit refusal names the limit: {hops}"
    );
    assert!(
        !hops.contains("https:// URL"),
        "the hop-limit refusal accuses the URL of not being https: {hops}"
    );
    assert!(
        hops.contains("https://roost.example/download/asset"),
        "{hops}"
    );
}

/// The cap, on both halves of it: what the server declares, and what it
/// actually sends when it declares nothing.
#[tokio::test]
async fn an_asset_past_the_cap_is_refused_declared_or_not() {
    let oversize = vec![0u8; 4096];

    for (name, reply, expected) in [
        (
            "declared",
            Reply::Body(oversize.clone()),
            "declares 4096 bytes, past the 1024-byte limit",
        ),
        (
            "undeclared",
            Reply::BodyUnframed(oversize.clone()),
            "is more than the 1024-byte limit",
        ),
    ] {
        let fixture = Fixture::start().await;
        let (_dir, scratch_path) = scratch();
        fixture.publish(&oversize);
        fixture.route(&fixture.asset_path(), reply);

        let error = match fetch(&fixture, &scratch_path, small_limits()).await {
            Ok(handle) => panic!("{name}: expected a refusal, got {handle:?}"),
            Err(error) => error,
        };
        let BootstrapError::Download(detail) = &error else {
            panic!("{name}: expected a download refusal, got {error:?}");
        };
        assert!(detail.contains(expected), "{name}: {detail}");
        assert!(!scratch_path.exists(), "{name}: the partial file survived");
    }
}

/// The `.sha256` sibling is bounded too — a server answering it with an
/// infinity must not be held in memory — and, like the asset, on both halves of
/// the cap. A checksum file that declares no length at all is the shape that
/// gets past the declared-length check, and there is a counting check behind it
/// for exactly that reason.
#[tokio::test]
async fn an_oversize_checksum_file_is_refused_declared_or_not() {
    let oversize = "a".repeat(8192);

    for (name, reply, expected) in [
        (
            "declared",
            Reply::Body(oversize.as_bytes().to_vec()),
            "declares 8192 bytes, past the 1024-byte limit",
        ),
        (
            "undeclared",
            Reply::BodyUnframed(oversize.as_bytes().to_vec()),
            "is more than the 1024-byte limit",
        ),
    ] {
        let fixture = Fixture::start().await;
        let (_dir, scratch_path) = scratch();
        fixture.publish(&[1, 2, 3, 4]);
        fixture.route(&fixture.checksum_path(), reply);

        let error = match fetch(&fixture, &scratch_path, small_limits()).await {
            Ok(handle) => panic!("{name}: expected a refusal, got {handle:?}"),
            Err(error) => error,
        };
        let BootstrapError::Checksum(detail) = &error else {
            panic!("{name}: expected a checksum refusal, got {error:?}");
        };
        assert!(detail.contains(expected), "{name}: {detail}");
        assert!(!scratch_path.exists(), "{name}: the partial file survived");
    }
}

/// The checksum decides, and [`parse_checksum_file`]'s refusals are really in
/// the path rather than reimplemented beside it.
#[tokio::test]
async fn a_checksum_that_does_not_check_out_refuses_before_anything_is_sent() {
    let bytes = vec![7u8; 512];

    // The hash is for other bytes.
    {
        let fixture = Fixture::start().await;
        let (_dir, scratch_path) = scratch();
        fixture.publish(&bytes);
        fixture.publish_checksum(&format!(
            "{}  {}\n",
            hex(Sha256::digest(b"something else").as_slice()),
            asset_name(VERSION, RemoteArch::Amd64)
        ));

        let error = fetch(&fixture, &scratch_path, Limits::DEFAULT)
            .await
            .expect_err("a hash that does not match is a refusal");
        let BootstrapError::Checksum(detail) = &error else {
            panic!("expected a checksum refusal, got {error:?}");
        };
        assert!(detail.contains("the download hashes to"), "{detail}");
        assert!(!scratch_path.exists());
    }

    // The record covers a different file — roost's rule, composed not copied.
    {
        let fixture = Fixture::start().await;
        let (_dir, scratch_path) = scratch();
        fixture.publish(&bytes);
        fixture.publish_checksum(&format!(
            "{}  some-other-file\n",
            hex(Sha256::digest(&bytes).as_slice())
        ));

        let error = fetch(&fixture, &scratch_path, Limits::DEFAULT)
            .await
            .expect_err("a checksum for another file covers nobody's download");
        let BootstrapError::Checksum(detail) = &error else {
            panic!("expected a checksum refusal, got {error:?}");
        };
        assert!(detail.contains("some-other-file"), "{detail}");
        assert!(!scratch_path.exists());
    }
}

/// Every failure path removes what it wrote — including the two that never got
/// as far as writing anything.
#[tokio::test]
async fn every_failure_path_leaves_nothing_behind() {
    // A 404 on the asset.
    {
        let fixture = Fixture::start().await;
        let (_dir, scratch_path) = scratch();
        let error = fetch(&fixture, &scratch_path, Limits::DEFAULT)
            .await
            .expect_err("nothing is published");
        assert!(
            matches!(&error, BootstrapError::Download(detail) if detail.contains("answered 404")),
            "{error:?}"
        );
        assert!(!scratch_path.exists());
    }

    // A 404 on the checksum, after the asset has already landed on disk.
    {
        let fixture = Fixture::start().await;
        let (_dir, scratch_path) = scratch();
        fixture.route(&fixture.asset_path(), Reply::Body(vec![1, 2, 3]));
        let error = fetch(&fixture, &scratch_path, Limits::DEFAULT)
            .await
            .expect_err("no published checksum, no install");
        assert!(matches!(&error, BootstrapError::Checksum(_)), "{error:?}");
        assert!(
            !scratch_path.exists(),
            "the downloaded asset outlived the failure"
        );
    }

    // An asset served as nothing at all.
    {
        let fixture = Fixture::start().await;
        let (_dir, scratch_path) = scratch();
        fixture.publish(&[]);
        let error = fetch(&fixture, &scratch_path, Limits::DEFAULT)
            .await
            .expect_err("an empty file is not a roost-session");
        assert!(
            matches!(&error, BootstrapError::Download(detail) if detail.contains("empty file")),
            "{error:?}"
        );
        assert!(!scratch_path.exists());
    }
}

/// A fetch that is dropped part-way — a phone backgrounding, a user clicking
/// cancel — leaves no more behind than one that failed.
///
/// This is the case a `remove_dir_all` at the end of the function does not
/// cover: a cancelled future runs `Drop` and nothing else.
#[tokio::test]
async fn a_cancelled_fetch_leaves_nothing_behind() {
    let fixture = Fixture::start().await;
    let (_dir, scratch_path) = scratch();
    fixture.publish(&[1, 2, 3, 4]);
    fixture.route(&fixture.asset_path(), Reply::Stall);

    let outcome = tokio::time::timeout(
        Duration::from_millis(300),
        fetch(&fixture, &scratch_path, Limits::DEFAULT),
    )
    .await;
    assert!(
        outcome.is_err(),
        "the stalled fetch should not have finished"
    );
    assert!(
        !scratch_path.exists(),
        "cancellation left the partial download at {}",
        scratch_path.display()
    );
}

/// The base is checked before a socket is opened, and a directory that already
/// exists is refused **without being deleted**.
#[tokio::test]
async fn the_base_and_the_directory_are_checked_before_anything_is_written() {
    let (dir, scratch_path) = scratch();

    for base in [
        "http://roost.example/download",
        "ftp://127.0.0.1/download",
        "https://roost.example/download?token=x",
        "https://user@roost.example/download",
    ] {
        let error = match fetch_release_limited(
            base,
            VERSION,
            RemoteArch::Amd64,
            &scratch_path,
            Limits::DEFAULT,
        )
        .await
        {
            Ok(handle) => panic!("{base}: expected a refusal, got {handle:?}"),
            Err(error) => error,
        };
        assert!(
            matches!(&error, BootstrapError::Download(detail) if detail.contains("usable https:// URL")),
            "{base}: {error:?}"
        );
        assert!(
            !scratch_path.exists(),
            "{base}: a refused base created a directory"
        );
    }

    // A pre-existing directory is the caller's bug, and its contents are not
    // this function's to delete.
    let occupied = dir.0.join("occupied");
    write_file(&occupied.join("somebody-elses-file"), b"mine");
    let error = fetch_release_limited(
        "https://roost.example/download",
        VERSION,
        RemoteArch::Amd64,
        &occupied,
        Limits::DEFAULT,
    )
    .await
    .expect_err("a directory that already exists is a refusal");
    assert!(
        matches!(&error, BootstrapError::Download(detail) if detail.contains("creating")),
        "{error:?}"
    );
    assert!(
        occupied.join("somebody-elses-file").exists(),
        "the refusal deleted a directory it did not create"
    );
}

/// The caps every real caller gets are roost's two numbers.
#[test]
fn the_default_limits_are_roosts_numbers() {
    assert_eq!(Limits::DEFAULT.asset_max, 256 * 1024 * 1024);
    assert_eq!(Limits::DEFAULT.checksum_max, 4 * 1024);
}

/// [`fetch_release`] itself — the public signature, with roost's arch spellings.
#[tokio::test]
async fn the_public_fetch_takes_roosts_arch_spellings() {
    let fixture = Fixture::start().await;
    let (_dir, scratch_path) = scratch();
    fixture.publish(&[4, 3, 2, 1]);

    let handle = fetch_release(&fixture.base, VERSION, "x86_64", &scratch_path)
        .await
        .expect("uname -m spellings map to the release spelling");
    assert_eq!(drain(&handle), vec![4, 3, 2, 1]);

    let error = fetch_release(&fixture.base, VERSION, "riscv64", &scratch_path)
        .await
        .expect_err("an architecture with no build has no asset");
    assert!(
        matches!(error, BootstrapError::UnsupportedArch(_)),
        "{error:?}"
    );
}

// ============================================================================
// The pin flip, rehearsed
// ============================================================================

/// **The one-line follow-up, proven to be one line.**
///
/// Everything below runs the real ladder with a pin supplied, which is exactly
/// what `RELEASE_PIN = Some(...)` will do — so the day a protocol-4 release
/// ships, the change is the constant and nothing else. This is the closest an
/// automated test can get to the live asset path while the external gate is
/// shut, and it is deliberately not described as covering it.
#[tokio::test]
async fn the_asset_rung_is_one_pin_away() {
    let fixture = Fixture::start().await;
    let (_dir, scratch_path) = scratch();
    let bytes = vec![42u8; 2048];
    fixture.publish(&bytes);

    let pin = RoostRelease { version: VERSION };
    let env = SourceEnv {
        asset_base: Some(fixture.base.clone()),
        ..sealed_env()
    };

    let previewed = preview_with(&env, TARGET, "amd64", Some(&pin));
    assert_eq!(
        previewed.source,
        Source::Asset {
            base: fixture.base.clone(),
            version: VERSION.to_string(),
        }
    );
    assert!(previewed.available());
    assert_eq!(
        previewed.describe(TARGET),
        format!(
            "roost-session {VERSION} from {}, checksum-verified",
            fixture.base
        )
    );

    let handle = resolve_with(
        &env,
        TARGET,
        "amd64",
        Some(&pin),
        &scratch_path,
        Limits::DEFAULT,
    )
    .await
    .expect("the asset rung");
    assert_eq!(drain(&handle), bytes);
    assert_eq!(
        handle.sha256(),
        Some(hex(Sha256::digest(&bytes).as_slice()).as_str())
    );
}

/// A pinned version that is not a plain `x.y.z` has no guaranteed release tag,
/// so the ladder refuses to guess at one rather than downloading whatever is
/// behind a tag that may not be that build.
#[test]
fn a_prerelease_pin_builds_no_url() {
    let pin = RoostRelease {
        version: "0.0.20-rc1",
    };
    let why = asset_source(&sealed_env(), Some(&pin)).expect_err("no tag can be derived");
    assert!(why.contains("not a plain x.y.z version"), "{why}");
}

/// An asset base the fetch would refuse is refused at **preview** time, so a
/// consent card never names one.
#[test]
fn a_base_the_fetch_would_refuse_never_reaches_a_consent_card() {
    let pin = RoostRelease { version: VERSION };
    let env = SourceEnv {
        asset_base: Some("http://roost.example/download".to_string()),
        ..sealed_env()
    };
    let previewed = preview_with(&env, TARGET, "amd64", Some(&pin));
    assert_eq!(previewed.source, Source::None);
    assert!(
        previewed
            .skipped
            .iter()
            .any(|why| why.contains("usable https:// URL")),
        "{:?}",
        previewed.skipped
    );
}

/// An architecture roost publishes nothing for has no rungs at all, and the
/// preview says which architecture.
#[test]
fn an_architecture_with_no_build_has_no_rungs() {
    let previewed = preview(&sealed_env(), TARGET, "riscv64");
    assert_eq!(previewed.source, Source::None);
    assert!(
        previewed
            .skipped
            .iter()
            .any(|why| why.contains("(riscv64) has no roost-session build")),
        "{:?}",
        previewed.skipped
    );
}

/// The kebab names an IPC payload and a log line use.
#[test]
fn the_rungs_have_stable_names() {
    assert_eq!(
        Source::Override {
            path: "/x".to_string()
        }
        .as_str(),
        "override"
    );
    assert_eq!(
        Source::Sibling {
            path: "/x".to_string()
        }
        .as_str(),
        "sibling"
    );
    assert_eq!(
        Source::Asset {
            base: "https://x".to_string(),
            version: "1.2.3".to_string()
        }
        .as_str(),
        "asset"
    );
    assert_eq!(Source::None.as_str(), "none");
}
