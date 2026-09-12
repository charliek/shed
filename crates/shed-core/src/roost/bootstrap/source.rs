//! Where the bytes come from — shed's own source ladder, over roost's pure
//! helpers (plan 019 §3.5, S5).
//!
//! An install streams a `roost-session` binary at a host. This module decides
//! *which* binary, produces the opened, verified [`SourceHandle`] the machine
//! will take, and — when nothing can supply one — says so in the sentence plan
//! 019 §3.5 pinned.
//!
//! ## The ladder
//!
//! 1. **Override** — the file `ROOST_SESSION_INSTALL_BIN` names. roost's own
//!    seam; shed reads the variable itself. A hard error rather than a
//!    fall-through, for roost's reason: an explicit override that cannot be used
//!    means the user asked for a *specific* binary, and quietly installing a
//!    different one is worse than failing.
//! 2. **Sibling** — the `roost-session` beside this client, **desktop only**
//!    (pin P3). Requires [`client_arch()`] to equal the remote's (it is `None`
//!    off Linux, so a Mac never takes this rung) and a local `identify` that
//!    says session protocol [`SESSION_PROTOCOL_VERSION`].
//! 3. **Release asset** — behind [`RELEASE_PIN`], fetched and checksum-verified
//!    by [`fetch_release`].
//! 4. **Nothing** — [`unavailable`], and the host is left untouched.
//!
//! ## The pin is `None`, and that is the state of the world
//!
//! [`RELEASE_PIN`] is `None` **because no roost release speaks session protocol
//! [`SESSION_PROTOCOL_VERSION`]** — the latest is [`LATEST_KNOWN_RELEASE`],
//! which speaks 2. That is an external gate, not an unfinished branch: rung 3 is
//! implemented and tested against a loopback fixture, and the only thing missing
//! is a release to point it at. Flipping the pin is a one-line edit *here*,
//! which is the whole reason the version and the protocol number live in
//! constants rather than in the sentence that mentions them.
//!
//! ## Why shed fetches the asset itself, on the desktop too
//!
//! roost has a download rung of its own, and shed does not use it. The reason is
//! one property, and it is the property the whole install rests on:
//!
//! > **The descriptor shed hashed must be the descriptor shed streams.**
//!
//! [`ResolvedSource::verified`](roost_ipc::bootstrap::ResolvedSource) — roost's
//! open, hashed handle — is **private**, and its accessor is private too. What
//! roost's API will hand a caller is a `Path`. Hashing a path and re-opening it
//! to stream is a window a local attacker writes through: the bytes that were
//! checked and the bytes that get executed on someone else's machine are two
//! lookups of one name, not one file. shed cannot close that window from
//! outside roost, so it does the fetch itself and keeps the descriptor —
//! [`SourceHandle`] owns it, [`Stdin::Source`](super::Stdin::Source) streams it,
//! and nothing in between ever names a path again.
//!
//! Two more things follow from doing it here rather than there, and both are
//! reasons rather than consolations:
//!
//! * **Mobile needs this code anyway.** Pin P4 says the phone fetches with its
//!   own HTTP stack — it has no `curl` to shell out to and no `ssh` to run one
//!   over. One implementation both clients share is a property; two that agree
//!   today is a coincidence (the module doc's own rule, applied to the source
//!   ladder instead of the choreography).
//! * **One fetch tested once.** The contract below — https-only, redirects
//!   https-only, caps, a private directory, partial files removed, the checksum
//!   read from a published sibling — is asserted against a loopback fixture in
//!   this file's tests. A second path through `curl` would need the same table
//!   and would not get it.
//!
//! This is one of the two decisions plan 019 records as not yet confirmed by the
//! owner; it is implemented as written, and it is a cheap thing to reverse (the
//! desktop would call roost's `resolve_source` and give up the descriptor
//! property — which is exactly the trade being made).
//!
//! ## The downloaded file has no name
//!
//! Rung 3 writes the asset into a fresh 0700 directory, and **unlinks it the
//! instant it is open** — before a byte is downloaded, long before it is hashed.
//! What survives the `unlink` is the descriptor, which is the only thing
//! [`SourceHandle`] ever wanted; nothing downstream needs the name (the desktop
//! takes the descriptor and plan 019 §3.5 pins that mobile never re-opens a
//! path). The published `.sha256` never touches disk at all —
//! [`download_to_string`] holds those four kilobytes in memory.
//!
//! **What that buys, precisely:** between the hash and the stream there is no
//! pathname a second process could open, so "the descriptor shed hashed is the
//! descriptor shed streams" is a property of the filesystem rather than of a
//! race being lost. Writing to a predictable name in a scratch directory and
//! removing the directory afterwards *narrows* that window; unlinking at open
//! removes it.
//!
//! **What it does not buy:** anything against a same-UID process. That is not a
//! boundary shed defends anywhere — a process running as this user can already
//! rewrite `~/.shed/config.yaml`, the user's ssh keys, and the shed binary
//! itself, and none of those have a lock on them either. The point is narrower
//! and worth keeping anyway: a security property the module *states* should not
//! quietly depend on timing.
//!
//! ## What is verified, where
//!
//! Only rung 3 is sha256-verified locally, and that is roost's design kept
//! rather than a gap: there is no published checksum for a file on this machine,
//! so there is nothing rungs 1 and 2 could be checked *against*. What gates them
//! is rung 1's [`sniff_binary`] (a wrong-arch ELF, an ELF for a machine roost
//! publishes nothing for, a Mach-O — the Mac-developer mistake) and rung 2's
//! local `identify`. What gates **every** rung alike is the far side's staged
//! verify before the commit and the post-commit identify after it, which are the
//! two checks that decide what actually runs over there.

use std::ffi::OsString;
use std::io::{Read as _, Seek as _, SeekFrom, Write as _};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use futures_util::StreamExt as _;
use roost_ipc::bootstrap::{
    asset_name, check_asset_base, checksum_name, client_arch, default_asset_base, map_arch,
    parse_checksum_file, sniff_binary, BootstrapError, RemoteArch, Sniff, INSTALL_BIN_ENV,
};
use roost_ipc::messages::SESSION_PROTOCOL_VERSION;
use roost_ipc::session_launch::locate_session_binary;
use sha2::{Digest, Sha256};

use super::copy::{BootstrapFailure, Stage};
use super::{Identity, SourceHandle, IDENTITY_STDOUT_CAP};

// ============================================================================
// The pin
// ============================================================================

/// A roost release shed would fetch a `roost-session` from.
///
/// One field today. It is a struct rather than a bare `&str` so that the
/// pin-flip PR adds whatever else a real pin turns out to need (an asset base
/// override, a second architecture's version) in the place the pin already is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RoostRelease {
    /// The release's version, **without** the `v`. [`default_asset_base`]
    /// prefixes it to build the tag, and refuses anything that is not three
    /// plain numeric components — a prerelease client must never guess at a tag
    /// spelling that may not exist, or may exist holding a different build.
    pub version: &'static str,
}

/// The release rung 3 fetches from — **`None` today**.
///
/// See the module doc: no published roost release speaks session protocol
/// [`SESSION_PROTOCOL_VERSION`], so there is nothing honest to point this at.
/// A `Some(RoostRelease { version: "0.0.20" })` here is the entire pin-flip.
pub const RELEASE_PIN: Option<RoostRelease> = None;

/// The newest roost release shed knows of, and the session protocol it speaks —
/// **`("0.0.19", 2)`**.
///
/// Its only job is [`unavailable`]'s parenthesis. It is a constant so that the
/// sentence a user reads ("the latest, 0.0.19, speaks 2") cannot drift away from
/// the pin above it: one PR edits both, in one file, and this module's tests
/// assert the sentence is built from them rather than typed out.
pub const LATEST_KNOWN_RELEASE: (&str, u32) = ("0.0.19", 2);

// ============================================================================
// Caps and budgets — roost's numbers, restated
// ============================================================================
//
// Private constants of roost's own runtime, restated here with the reasons
// roost gives, exactly as [`super`] restates the exec budgets: shed does not use
// roost's runtime, and a number copied without its reason is a number that
// drifts silently.

/// Cap on a downloaded release asset. `roost-session` is ~10 MiB stripped; this
/// leaves two orders of magnitude of headroom and still bounds a server that
/// answers a 404 page with a chunked infinity.
pub const ASSET_MAX_BYTES: u64 = 256 * 1024 * 1024;

/// Cap on the `.sha256` sibling: one 64-hex record and a filename.
pub const CHECKSUM_MAX_BYTES: u64 = 4 * 1024;

/// The local `<candidate> identify` the sibling rung runs. A local process that
/// prints one line; anything slower is not answering.
const LOCAL_IDENTIFY_BUDGET: Duration = Duration::from_secs(10);

/// How long a fetch waits for the far side to *start* answering.
const CONNECT_TIMEOUT: Duration = Duration::from_secs(15);

/// How long a fetch waits between two bytes of a body it is already reading.
///
/// A read timeout rather than a whole-request one on purpose: a 256 MiB ceiling
/// over a link nobody promised anything about has no honest total, and a total
/// that is generous enough for the ceiling is no bound at all on a server that
/// dribbles. What is actually pathological is a connection that stops producing,
/// and that is what this catches.
const READ_TIMEOUT: Duration = Duration::from_secs(60);

/// How many redirects are followed before the fetch gives up. GitHub's release
/// downloads are one hop to an object store; five is slack, not a design.
const MAX_REDIRECTS: usize = 5;

/// One read's worth of bytes while hashing.
const HASH_CHUNK: usize = 64 * 1024;

/// How much of a body accumulates before it crosses [`write_batch`]'s blocking
/// thread. A `spawn_blocking` per streamed chunk would be a task per few
/// kilobytes — tens of thousands of them at [`ASSET_MAX_BYTES`] — and a batch
/// per megabyte is the same syscall count with a thousandth of the scheduling.
/// It is the accumulator that bounds what the batching costs in memory; the cap
/// still bounds the file.
const WRITE_BATCH: usize = 1024 * 1024;

/// How much of a candidate binary [`sniff_binary`] is shown. A whole ELF64
/// header, which is more than it reads but is the natural unit to lift off disk
/// in one go.
const HEADER_PEEK: usize = 64;

/// The caps a fetch enforces. Split out of the constants **only** so the tests
/// can drive the oversize paths with a few kilobytes instead of a few hundred
/// megabytes; [`Limits::DEFAULT`] is what every caller gets, and a test pins
/// that it is roost's two numbers.
#[derive(Debug, Clone, Copy)]
struct Limits {
    asset_max: u64,
    checksum_max: u64,
}

impl Limits {
    const DEFAULT: Limits = Limits {
        asset_max: ASSET_MAX_BYTES,
        checksum_max: CHECKSUM_MAX_BYTES,
    };
}

// ============================================================================
// What the ladder would use
// ============================================================================

/// Which rung the ladder lands on, decided without running or fetching
/// anything.
///
/// The FRB-mirror rule (`crates/CLAUDE.md`): owned `String`s, a fielded enum
/// that becomes a Dart sealed class.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Source {
    /// The file `ROOST_SESSION_INSTALL_BIN` names.
    Override { path: String },
    /// The `roost-session` beside this client.
    Sibling { path: String },
    /// A release asset, to be downloaded and checksum-verified.
    Asset { base: String, version: String },
    /// Nothing applies. The button is replaced by [`unavailable`]'s sentence
    /// (plan 019 §3.4's sixth plan-matrix row).
    None,
}

impl Source {
    /// A stable kebab name, for an IPC payload and a log line.
    pub fn as_str(&self) -> &'static str {
        match self {
            Source::Override { .. } => "override",
            Source::Sibling { .. } => "sibling",
            Source::Asset { .. } => "asset",
            Source::None => "none",
        }
    }

    /// The "from where" phrase, in the **one** place both the consent card's
    /// prediction and the resolved handle's
    /// [`origin`](SourceHandle::origin) read it from — so a card cannot promise
    /// one origin in wording the log then reports differently. roost's own rule,
    /// and its reason: an overridden asset base is named as itself, because
    /// rendering a fixture server or a mirror as github.com would be a lie in
    /// exactly the situation where the user most needs the truth.
    ///
    /// `target` is used by [`Source::None`] alone, whose sentence ends by saying
    /// what was *not* done to it.
    pub fn describe(&self, target: &str) -> String {
        match self {
            Source::Override { path } => override_origin(Path::new(path)),
            Source::Sibling { path } => sibling_origin(path),
            Source::Asset { base, version } => asset_origin(base, version),
            Source::None => unavailable(target),
        }
    }

    /// Whether there is anything here to install from.
    pub fn available(&self) -> bool {
        !matches!(self, Source::None)
    }
}

fn override_origin(path: &Path) -> String {
    format!("{} ({INSTALL_BIN_ENV})", path.display())
}

fn sibling_origin(path: &str) -> String {
    format!("the roost-session beside this app ({path})")
}

fn asset_origin(base: &str, version: &str) -> String {
    format!("roost-session {version} from {base}, checksum-verified")
}

/// What a consent card says about where the bytes will come from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SourcePreview {
    /// The rung the ladder will try first.
    pub source: Source,
    /// Where it lands if `source` turns out not to be usable.
    ///
    /// Only ever set for the **sibling** rung, because its local `identify` is
    /// the one check a preview deliberately does not run (plan 019 §3.5:
    /// nothing is resolved before consent). Stating the fall-through is longer
    /// than a single claim and it is the only version of the sentence that stays
    /// true whichever way the resolution goes — roost's rule, and its wording.
    pub fallback: Option<Source>,
    /// Rungs the ladder will not try, and why.
    ///
    /// Pin P3 requires one of these by name: a client whose own architecture is
    /// not the remote's is skipped, **and the copy names the arch**, because
    /// "shed won't use the roost-session next to it" is baffling without it.
    pub skipped: Vec<String>,
}

impl SourcePreview {
    /// Whether the plan matrix gets a button at all.
    pub fn available(&self) -> bool {
        self.source.available()
    }

    /// The consent card's "from where" line.
    pub fn describe(&self, target: &str) -> String {
        let first = self.source.describe(target);
        match &self.fallback {
            None => first,
            // A fall-through to nothing is still a fall-through, and saying so
            // is the difference between a card that was honest and a card that
            // promised a sibling and delivered a refusal.
            Some(Source::None) => format!(
                "{first} — and nothing else, if it turns out not to speak session protocol \
                 {SESSION_PROTOCOL_VERSION}"
            ),
            Some(fallback) => format!(
                "{} — or {}, if it turns out not to speak session protocol \
                 {SESSION_PROTOCOL_VERSION}",
                first,
                fallback.describe(target)
            ),
        }
    }
}

// ============================================================================
// The inputs, injected rather than read
// ============================================================================

/// Everything the ladder reads from the process environment, lifted into
/// arguments.
///
/// roost's own reason, applied here: the precedence between these is the thing
/// worth testing, and it is not testable without mutating process-global state
/// that every other test in the binary also reads. [`SourceEnv::from_env`] is
/// the one place that touches `std::env`.
#[derive(Debug, Clone, Default)]
pub struct SourceEnv {
    /// `ROOST_SESSION_INSTALL_BIN` — rung 1.
    pub install_bin: Option<PathBuf>,
    /// `ROOST_SESSION_BIN` — roost's "which daemon do I run" override, which
    /// rung 2 borrows to find the sibling.
    pub session_bin: Option<OsString>,
    /// This process's own path, so rung 2 can look beside it.
    pub caller_exe: Option<PathBuf>,
    /// `PATH`, rung 2's last resort.
    pub path: Option<OsString>,
    /// `ROOST_SESSION_ASSET_BASE` — roost's override for the release base, which
    /// is what points rung 3 at a fixture server or a mirror.
    pub asset_base: Option<String>,
}

impl SourceEnv {
    /// Read the real environment.
    pub fn from_env() -> SourceEnv {
        fn non_empty(value: Option<OsString>) -> Option<OsString> {
            value.filter(|value| !value.is_empty())
        }
        SourceEnv {
            install_bin: non_empty(std::env::var_os(INSTALL_BIN_ENV)).map(PathBuf::from),
            session_bin: non_empty(std::env::var_os(roost_ipc::session_launch::BIN_ENV)),
            caller_exe: std::env::current_exe().ok(),
            path: std::env::var_os("PATH"),
            asset_base: non_empty(std::env::var_os(roost_ipc::bootstrap::ASSET_BASE_ENV))
                .map(|value| value.to_string_lossy().into_owned()),
        }
    }
}

// ============================================================================
// Rung 4's sentence
// ============================================================================

/// The copy plan 019 §3.5 pinned for "nothing can supply these bytes", word for
/// word — built from [`RELEASE_PIN`]'s neighbours so that a pin-flip PR editing
/// the constants above cannot leave this sentence claiming something that has
/// stopped being true.
pub fn unavailable(target: &str) -> String {
    let (version, protocol) = LATEST_KNOWN_RELEASE;
    format!(
        "no roost release speaking session protocol {SESSION_PROTOCOL_VERSION} is published yet \
         (the latest, {version}, speaks {protocol}). On a Linux machine with a protocol-\
         {SESSION_PROTOCOL_VERSION} roost installed the desktop uses that roost-session; \
         otherwise point {INSTALL_BIN_ENV} at a protocol-{SESSION_PROTOCOL_VERSION} build. \
         {target} was left untouched."
    )
}

/// [`unavailable`] as the failure an install refuses with.
pub fn no_source(target: &str) -> BootstrapFailure {
    BootstrapFailure::new(Stage::Source, unavailable(target))
}

// ============================================================================
// Preview
// ============================================================================

/// Which rung a bootstrap of `target` would use, without running or fetching
/// anything.
///
/// `arch` is [`Probe::arch`](super::Probe) — roost's release spelling (`amd64` /
/// `arm64`), which [`map_arch`] also accepts in its `uname -m` spellings.
pub fn preview(env: &SourceEnv, target: &str, arch: &str) -> SourcePreview {
    preview_with(env, target, arch, RELEASE_PIN.as_ref())
}

fn preview_with(
    env: &SourceEnv,
    target: &str,
    arch: &str,
    pin: Option<&RoostRelease>,
) -> SourcePreview {
    let mut skipped = Vec::new();

    let Ok(remote) = map_arch(arch) else {
        // The plan matrix refused before this — an architecture roost publishes
        // no build for never reaches a consent card. Answered rather than
        // panicked because a preview is a read.
        skipped.push(format!(
            "{target}'s architecture ({arch}) has no roost-session build"
        ));
        return SourcePreview {
            source: Source::None,
            fallback: None,
            skipped,
        };
    };

    if let Some(path) = &env.install_bin {
        return SourcePreview {
            source: Source::Override {
                path: path.display().to_string(),
            },
            fallback: None,
            skipped,
        };
    }

    // Both rungs are decided before either is reported, so `skipped` reads in
    // ladder order however the two turn out.
    let sibling = sibling_candidate(env, target, remote);
    let asset = asset_source(env, pin);
    if let Err(why) = &sibling {
        skipped.push(why.clone());
    }
    if let Err(why) = &asset {
        skipped.push(why.clone());
    }
    let asset = asset.unwrap_or(Source::None);

    match sibling {
        Ok(path) => SourcePreview {
            source: Source::Sibling { path },
            fallback: Some(asset),
            skipped,
        },
        Err(_) => SourcePreview {
            source: asset,
            fallback: None,
            skipped,
        },
    }
}

// ============================================================================
// Resolve
// ============================================================================

/// Climb the ladder for real and hand back an opened, verified
/// [`SourceHandle`].
///
/// `scratch` is used by the asset rung alone: a **fresh** directory this
/// function creates, fills and removes before it returns. Nothing survives it —
/// see [`fetch_release`].
pub async fn resolve(
    env: &SourceEnv,
    target: &str,
    arch: &str,
    scratch: &Path,
) -> Result<SourceHandle, BootstrapFailure> {
    resolve_with(
        env,
        target,
        arch,
        RELEASE_PIN.as_ref(),
        scratch,
        Limits::DEFAULT,
    )
    .await
}

async fn resolve_with(
    env: &SourceEnv,
    target: &str,
    arch: &str,
    pin: Option<&RoostRelease>,
    scratch: &Path,
    limits: Limits,
) -> Result<SourceHandle, BootstrapFailure> {
    let remote = map_arch(arch).map_err(|_| super::copy::unsupported_arch(target, arch))?;

    // Rung 1. A hard error, never a fall-through: see the module doc.
    if let Some(path) = &env.install_bin {
        return open_override(path, remote, target);
    }

    // Rung 2. A fall-through, because it is a guess about this machine rather
    // than an instruction from the user.
    //
    // The reason it fell through is dropped here rather than logged: shed-core
    // installs no logging facade, and rung 4's sentence is pinned word for word
    // by plan 019 §3.5 and may not grow a clause. What a *user* needs is on the
    // consent card they already read — [`preview`] records every skip in
    // [`SourcePreview::skipped`], including pin P3's arch sentence, and the one
    // skip a preview cannot foresee (a sibling that will not identify as
    // protocol 4) is exactly what its `fallback` clause warned about.
    if let Ok(handle) = sibling_handle(env, target, remote).await {
        return Ok(handle);
    }

    // Rung 3.
    if let Ok(Source::Asset { base, version }) = asset_source(env, pin) {
        return fetch_release_limited(&base, &version, remote, scratch, limits)
            .await
            .map_err(|error| BootstrapFailure::from_roost(Stage::Source, &error, target));
    }

    // Rung 4.
    Err(no_source(target))
}

/// Rung 1: the file [`INSTALL_BIN_ENV`] names, exactly as it is.
///
/// What is checked here is only what the file's own first bytes can settle
/// ([`sniff_binary`]): its architecture class against the remote's, and formats
/// that are provably not a Linux binary for that host. Anything else — a shell
/// script, a short file, an unrecognized format — passes through untouched, and
/// the far side's staged verify is what decides.
///
/// **Opened once.** roost sniffs a path and lets the streamer reopen it, and
/// accepts the resulting window on a developer's own machine. shed keeps the
/// descriptor it sniffed, so there is no second lookup to race — the same
/// property the asset rung exists to preserve, applied to a rung that gets it
/// for free.
fn open_override(
    path: &Path,
    arch: RemoteArch,
    target: &str,
) -> Result<SourceHandle, BootstrapFailure> {
    let refuse = |detail: String| {
        BootstrapFailure::from_roost(Stage::Source, &BootstrapError::Source(detail), target)
    };
    let unreadable = |error: std::io::Error| {
        refuse(format!(
            "{INSTALL_BIN_ENV}={} could not be read: {error}",
            path.display()
        ))
    };

    let mut file = open_without_blocking(path).map_err(unreadable)?;
    // The **descriptor's** metadata, never the path's: what is checked and what
    // is streamed are then one file by construction, with no second lookup to
    // race.
    let metadata = file.metadata().map_err(unreadable)?;
    if !metadata.is_file() {
        return Err(refuse(format!(
            "{INSTALL_BIN_ENV}={} is not a regular file",
            path.display()
        )));
    }

    let mut head = Vec::with_capacity(HEADER_PEEK);
    (&mut file)
        .take(HEADER_PEEK as u64)
        .read_to_end(&mut head)
        .map_err(unreadable)?;
    match sniff_binary(&head) {
        Sniff::Elf(found) if found != arch => {
            return Err(refuse(format!(
                "{INSTALL_BIN_ENV}={} is an {found} Linux binary but {target} is {arch}",
                path.display()
            )))
        }
        Sniff::ElfOther(machine) => {
            return Err(refuse(format!(
                "{INSTALL_BIN_ENV}={} is an ELF for machine 0x{machine:x}, not amd64/arm64",
                path.display()
            )))
        }
        Sniff::MachO => {
            return Err(refuse(format!(
                "{INSTALL_BIN_ENV}={} is a macOS binary, not a Linux one",
                path.display()
            )))
        }
        Sniff::Elf(_) | Sniff::Unknown => {}
    }

    file.seek(SeekFrom::Start(0)).map_err(unreadable)?;
    Ok(SourceHandle::from_open_file(
        override_origin(path),
        file,
        metadata.len(),
        None,
    ))
}

/// `open(2)` with `O_NONBLOCK`, so that opening the file can never be the thing
/// that hangs.
///
/// A plain `File::open` on a **FIFO** blocks until somebody opens the other end
/// — which would happen one line before [`open_override`]'s `is_file()` check,
/// i.e. the guard that exists to refuse a non-regular file would never get to
/// run. `ROOST_SESSION_INSTALL_BIN` is the user's own variable pointing at the
/// user's own file, so this is a self-inflicted hang rather than an attack; it
/// is still the one case the guard did not actually answer. `O_NONBLOCK` on a
/// regular file is a no-op, and on a directory the open succeeds and
/// `is_file()` does the refusing, exactly as before.
fn open_without_blocking(path: &Path) -> std::io::Result<std::fs::File> {
    use std::os::unix::fs::OpenOptionsExt as _;

    std::fs::OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_NONBLOCK)
        .open(path)
}

/// Rung 2, the half a preview can answer: is there a sibling that *could* run
/// over there at all?
///
/// Pin P3's arch gate lives here because it is free — [`client_arch`] is a
/// compile-time fact, not an environment one: it answers "could the binary
/// sitting next to me run over there", and only the target triple this was built
/// for can say. `None` off Linux, so a Mac desktop never offers this rung.
fn sibling_candidate(env: &SourceEnv, target: &str, arch: RemoteArch) -> Result<String, String> {
    let Some(local) = client_arch() else {
        return Err(
            "this shed is not a Linux build, so the roost-session beside it can't run on a Linux \
             host"
                .to_string(),
        );
    };
    if local != arch {
        return Err(format!(
            "this shed is an {local} build and {target} is {arch}"
        ));
    }
    let located = locate_session_binary(
        env.session_bin.as_deref(),
        env.caller_exe.as_deref(),
        env.path.as_deref(),
    )
    .map_err(|_| "this shed has no roost-session beside it or on its PATH".to_string())?;
    Ok(located.path.display().to_string())
}

/// Rung 2 in full: the candidate, plus the local `identify` a preview will not
/// run.
///
/// The protocol gate is shed's — [`Identity::compatible`] — and not roost's
/// exact triple, for the reason the module doc gives. A stale
/// `target/debug/roost-session` fails it and the ladder falls through: it is a
/// real binary, it is simply not one shed can talk to, and installing it would
/// put a session on the host that shed then refuses to read.
async fn sibling_handle(
    env: &SourceEnv,
    target: &str,
    arch: RemoteArch,
) -> Result<SourceHandle, String> {
    let path = sibling_candidate(env, target, arch)?;
    let identity = local_identify(Path::new(&path)).await?;
    if !identity.compatible() {
        return Err(format!(
            "{path} speaks session protocol {} and this shed speaks {SESSION_PROTOCOL_VERSION}",
            identity.session_protocol
        ));
    }
    let file = std::fs::File::open(&path).map_err(|error| format!("{path}: {error}"))?;
    let len = file
        .metadata()
        .map_err(|error| format!("{path}: {error}"))?
        .len();
    Ok(SourceHandle::from_open_file(
        sibling_origin(&path),
        file,
        len,
        None,
    ))
}

/// Run `<path> identify` here, bounded in **time and in memory**, and read the
/// one line back.
async fn local_identify(path: &Path) -> Result<Identity, String> {
    local_identify_within(path, LOCAL_IDENTIFY_BUDGET).await
}

/// [`local_identify`] with the budget supplied — the seam the timeout row in
/// this module's tests drives, so that asserting "the budget has teeth" costs a
/// third of a second rather than [`LOCAL_IDENTIFY_BUDGET`].
async fn local_identify_within(path: &Path, budget: Duration) -> Result<Identity, String> {
    // Scoped to this function: `ChildStdout` is the only `AsyncRead` in the
    // module, and importing the extension trait at the top would shadow
    // `std::io::Read`'s `take`/`read_to_end` for every `File` below it.
    use tokio::io::AsyncReadExt as _;

    let mut child = tokio::process::Command::new(path)
        .arg("identify")
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        // The budget's teeth: a timeout drops this future, which drops the
        // child, which kills it. Without this a binary that hangs on `identify`
        // outlives the answer nobody is waiting for any more.
        .kill_on_drop(true)
        .spawn()
        .map_err(|error| format!("running {} identify: {error}", path.display()))?;

    let Some(mut stdout) = child.stdout.take() else {
        return Err(format!(
            "running {} identify: it has no stdout",
            path.display()
        ));
    };

    // **At most the cap, off the pipe, while the child is still running.**
    // `wait_with_output()` reads to EOF first and caps the buffer afterwards,
    // which caps nothing: a broken or hostile `roost-session` writing
    // continuously grows that allocation at pipe throughput for the whole
    // budget, and this rung *executes* the candidate before anything has
    // verified it. Closing the read end after the cap is what turns the number
    // into a bound rather than a slice — the child's next write gets EPIPE (and
    // SIGPIPE, which `Command` restores to its default in the child), so it
    // stops instead of filling the pipe until the clock runs out.
    let (seen, status) = tokio::time::timeout(budget, async move {
        let mut seen = Vec::new();
        (&mut stdout)
            .take(IDENTITY_STDOUT_CAP as u64)
            .read_to_end(&mut seen)
            .await?;
        drop(stdout);
        let status = child.wait().await?;
        Ok::<_, std::io::Error>((seen, status))
    })
    .await
    .map_err(|_| format!("{} identify timed out", path.display()))?
    .map_err(|error| format!("running {} identify: {error}", path.display()))?;

    if !status.success() {
        return Err(format!(
            "{} would not identify itself: {status}",
            path.display(),
        ));
    }

    Identity::parse(&String::from_utf8_lossy(&seen))
        .ok_or_else(|| format!("{} would not identify itself", path.display()))
}

/// Rung 3, as far as it can be decided without fetching: is there a pin, and is
/// the base it points at one shed will talk to?
///
/// [`check_asset_base`] runs **here**, at preview time, and not only inside the
/// fetch: a consent card that names a base the fetch is going to refuse has
/// asked the user to approve something that cannot happen.
fn asset_source(env: &SourceEnv, pin: Option<&RoostRelease>) -> Result<Source, String> {
    let pin = pin.ok_or_else(|| {
        let (version, protocol) = LATEST_KNOWN_RELEASE;
        format!(
            "no roost release is pinned — the latest, {version}, speaks session protocol \
             {protocol} and this shed speaks {SESSION_PROTOCOL_VERSION}"
        )
    })?;
    let base = match &env.asset_base {
        Some(base) => base.clone(),
        // A `NoSource` from here means the pinned version is not a plain stable
        // `x.y.z`, so `v<version>` is not guaranteed to be a real release tag.
        // roost's own message for that is addressed to a *host*; this one is
        // about the pin, so it says that instead.
        None => default_asset_base(pin.version).map_err(|_| {
            format!(
                "the pinned roost release ({}) is not a plain x.y.z version, so there is no \
                     release tag to download from",
                pin.version
            )
        })?,
    };
    check_asset_base(&base).map_err(|error| match error {
        BootstrapError::Download(detail) => detail,
        // `check_asset_base` has exactly one refusal (`refuse()`, in
        // `roost_ipc::bootstrap`) and it always builds `Download` — named
        // rather than silently handled, because `BootstrapError::message`'s
        // other variants are phrased at a remote target ("downloading
        // roost-session for {target} failed…") and would read as nonsense
        // here, where there is no target yet, only a base URL.
        other => unreachable!("check_asset_base only ever returns Download: {other:?}"),
    })?;
    Ok(Source::Asset {
        base,
        version: pin.version.to_string(),
    })
}

// ============================================================================
// The fetch
// ============================================================================

/// Fetch a published `roost-session` asset and its checksum, verify the bytes
/// here, and hand back the **open descriptor they were verified through**.
///
/// Plan 019 §3.5's contract, clause by clause — every one of them is a
/// requirement and every one of them is asserted against a loopback fixture in
/// this module's tests:
///
/// * [`check_asset_base`] first, so a base that is not a usable URL costs no
///   request at all.
/// * **`https://` only.** The one exception is `http://` for `127.0.0.1`,
///   `localhost` or `[::1]` — roost's loopback carve-out, which exists so a test
///   fixture can serve assets over a local HTTP server. Confining it to loopback
///   is what stops the seam from becoming a way to downgrade a real download to
///   plaintext.
/// * **Redirects only to `https://`**, and the loopback exception does **not**
///   extend to them: a base is something a human typed, a `Location` is
///   something a server said. Five hops, then it stops.
/// * The status is checked before a single body byte is read.
/// * The asset is capped at [`ASSET_MAX_BYTES`] and the checksum at
///   [`CHECKSUM_MAX_BYTES`] — on the declared `Content-Length` *and* on the bytes
///   actually counted, because a limit can only be enforced in advance against a
///   length the server chose to declare. A response with no `Content-Length` is
///   exactly the shape that slips past the first check.
/// * `dir` is created **fresh and private** (0700) and is a hard error if it
///   already exists: the caller's contract is a fresh directory, and a
///   pre-existing one is either another resolution's or something planted.
/// * **Nothing is left behind, on any path** — success, failure, *or
///   cancellation*, which is the case a `remove_dir_all` at the end of the
///   happy path does not cover and a phone backgrounding mid-download produces
///   for real. The removal is by **identity**: the directory's `(dev, ino)` is
///   recorded at creation and re-checked before it goes, so a retargeted
///   ancestor symlink gets a refusal rather than somebody else's directory
///   deleted ([`remove_created_dir`]). A cleanup that fails is reported, not
///   dropped.
/// * [`parse_checksum_file`] reads the published sibling, with roost's refusals:
///   exactly one record, naming the asset that was actually fetched. It is held
///   in memory and never written to disk.
/// * The sha256 is computed over the **open descriptor** the bytes were written
///   through, which [`SourceHandle`] then owns. The file is never re-opened by
///   name, and there is no name to re-open: it is **unlinked the moment it is
///   created**, so from the first downloaded byte onwards the descriptor is the
///   only reference to the inode (see the module doc for what that does and
///   does not buy).
///
/// `arch` is roost's release spelling (`amd64` / `arm64`); [`map_arch`] also
/// accepts the `uname -m` spellings.
pub async fn fetch_release(
    base: &str,
    version: &str,
    arch: &str,
    dir: &Path,
) -> Result<SourceHandle, BootstrapError> {
    let arch = map_arch(arch)?;
    fetch_release_limited(base, version, arch, dir, Limits::DEFAULT).await
}

/// `BootstrapError::Download` naming the path an I/O step failed on — the one
/// shape `create_private_dir`, `ScratchGuard::arm`, `create_private_file` and
/// the unlink below all refuse with, differing only in which verb and which
/// path. The verb stays a distinct word at each call site; only the
/// `"{verb} {path}: {error}"` assembly is shared.
fn path_error(verb: &str, path: &Path, error: &std::io::Error) -> BootstrapError {
    BootstrapError::Download(format!("{verb} {}: {error}", path.display()))
}

async fn fetch_release_limited(
    base: &str,
    version: &str,
    arch: RemoteArch,
    dir: &Path,
    limits: Limits,
) -> Result<SourceHandle, BootstrapError> {
    check_asset_base(base)?;

    // **Created first, then guarded.** The `AlreadyExists` refusal is the whole
    // point of the fresh-directory rule, and a guard armed before the create
    // would answer that refusal by deleting whatever was already there.
    create_private_dir(dir).map_err(|error| path_error("creating", dir, &error))?;
    // Armed for the *cancellation* case, which is the one a cleanup at the end
    // of the function cannot cover: a cancelled future runs `Drop` and nothing
    // else, and a phone backgrounding mid-download produces that for real.
    let scratch =
        ScratchGuard::arm(dir).map_err(|error| path_error("reading back", dir, &error))?;

    let fetched = fetch_verified(base, version, arch, dir, limits).await;

    // Every path that *returns* sweeps here rather than in `Drop`, because a
    // `Drop` has nobody to report a failure to and the old `let _ =` reported
    // it to nobody: a cleanup that fails leaves files behind, and that is worth
    // saying out loud. When the fetch itself failed, its error is the one the
    // user needs and it wins — a sweep failure on top of it is noise about a
    // directory whose contents the failure already explains.
    match (fetched, scratch.sweep()) {
        (Ok(handle), Ok(())) => Ok(handle),
        (Ok(_), Err(why)) => Err(BootstrapError::Download(why)),
        (Err(error), _) => Err(error),
    }
}

/// The fetch proper: everything between a scratch directory that exists and a
/// verified handle. Split out so the cleanup above has exactly one place to run
/// for every way this can end.
async fn fetch_verified(
    base: &str,
    version: &str,
    arch: RemoteArch,
    dir: &Path,
    limits: Limits,
) -> Result<SourceHandle, BootstrapError> {
    let name = asset_name(version, arch);
    let asset_url = format!("{base}/{name}");
    let checksum_url = format!("{base}/{}", checksum_name(&name));

    let client = http_client()?;

    let asset_path = dir.join(&name);
    let file = create_private_file(&asset_path)
        .map_err(|error| path_error("creating", &asset_path, &error))?;
    // **The name goes away here** — before a byte is downloaded, and a long way
    // before one is hashed. See the module doc: what is left is the descriptor,
    // which is all anything downstream wants, and there is no longer a path a
    // second process could open between the hash and the stream. A failure to
    // unlink is a hard error rather than a shrug, because the property the rung
    // exists for is the thing that would be quietly lost.
    std::fs::remove_file(&asset_path)
        .map_err(|error| path_error("unlinking", &asset_path, &error))?;

    let (file, size) = download_into(&client, &asset_url, file, limits.asset_max).await?;
    if size == 0 {
        return Err(BootstrapError::Download(format!(
            "{asset_url} was served as an empty file"
        )));
    }

    // Held in memory, never written: four kilobytes with nothing to unlink.
    let published = download_to_string(&client, &checksum_url, limits.checksum_max).await?;
    let expected = parse_checksum_file(&published, &name)?;

    let (actual, file) = tokio::task::spawn_blocking(move || hash_open_file(file))
        .await
        .map_err(|error| BootstrapError::Checksum(format!("hashing was interrupted: {error}")))?
        .map_err(|error| {
            BootstrapError::Checksum(format!("the download could not be re-read: {error}"))
        })?;
    if !actual.eq_ignore_ascii_case(&expected) {
        return Err(BootstrapError::Checksum(format!(
            "the download hashes to {actual} and the published checksum is {expected}"
        )));
    }

    Ok(SourceHandle::from_open_file(
        asset_origin(base, version),
        file,
        size,
        Some(actual),
    ))
}

/// The directory a fetch worked in, removed when this value goes out of scope —
/// and **only if it is still the directory the fetch created**.
struct ScratchGuard<'a> {
    dir: &'a Path,
    identity: (u64, u64),
    armed: bool,
}

impl<'a> ScratchGuard<'a> {
    /// Record what was just created, by identity rather than by name.
    ///
    /// Armed **after** a successful create, never before — the
    /// pre-existing-directory refusal must not be answered by deleting whatever
    /// was already there.
    fn arm(dir: &'a Path) -> std::io::Result<ScratchGuard<'a>> {
        Ok(ScratchGuard {
            dir,
            identity: dir_identity(dir)?,
            armed: true,
        })
    }

    /// Remove it now, and say whether that worked.
    fn sweep(mut self) -> Result<(), String> {
        self.armed = false;
        remove_created_dir(self.dir, self.identity)
    }
}

impl Drop for ScratchGuard<'_> {
    fn drop(&mut self) {
        if self.armed {
            // Cancellation only — every returning path sweeps explicitly. The
            // result is dropped here because the future that would have been
            // told is the one that went away.
            //
            // Blocking in an async drop, deliberately: it is a handful of
            // `unlink`s in a directory this function made, and the alternative
            // — spawning the cleanup — is a cleanup that may not have run when
            // the caller looks.
            let _ = remove_created_dir(self.dir, self.identity);
        }
    }
}

/// A directory's `(st_dev, st_ino)` — its identity, as opposed to its name.
///
/// `symlink_metadata`, so the final component is never followed: a `dir`
/// replaced by a symlink after the create reads as a mismatch rather than as a
/// redirection to wherever the symlink now points.
fn dir_identity(dir: &Path) -> std::io::Result<(u64, u64)> {
    use std::os::unix::fs::MetadataExt as _;

    let metadata = std::fs::symlink_metadata(dir)?;
    Ok((metadata.dev(), metadata.ino()))
}

/// `remove_dir_all`, but only if the path still names the directory `identity`
/// came from.
///
/// `remove_dir_all` takes a **pathname**, and a pathname is not an identity: if
/// an ancestor of `dir` is a symlink that gets retargeted after the create, the
/// same string names somebody else's directory — and the cleanup would delete
/// theirs and leave the real one behind. Re-`stat`ing first is what keeps the
/// guard removing what it made. It does not make the removal itself atomic
/// against a retarget racing the traversal; it makes the *ordinary* case a
/// checked one and the hostile case a refusal rather than a deletion.
fn remove_created_dir(dir: &Path, identity: (u64, u64)) -> Result<(), String> {
    let found = match dir_identity(dir) {
        Ok(found) => found,
        // Already gone: there is nothing left behind, which is the whole point
        // of removing it.
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => {
            return Err(format!(
                "the scratch directory {} could not be read back before removing it: {error}",
                dir.display()
            ))
        }
    };
    if found != identity {
        return Err(format!(
            "{} is no longer the directory this fetch created, so it was left alone",
            dir.display()
        ));
    }
    std::fs::remove_dir_all(dir)
        .map_err(|error| format!("removing the scratch directory {}: {error}", dir.display()))
}

/// roost's `create_private_dir`, restated: it is `pub(crate)` over there.
///
/// `create` rather than `create_dir_all` on purpose — this is the hard error on
/// `AlreadyExists` the contract promises.
fn create_private_dir(dir: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::DirBuilderExt as _;

    std::fs::DirBuilder::new().mode(0o700).create(dir)
}

/// A file only this user can read, opened read/write so the bytes can be hashed
/// back through the same descriptor they were written through.
fn create_private_file(path: &Path) -> std::io::Result<std::fs::File> {
    use std::os::unix::fs::OpenOptionsExt as _;

    std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
}

/// What the redirect policy does with one hop.
///
/// A pure decision, lifted out of the closure, because the policy stops for
/// **two** reasons and a refusal that names the wrong one is worse than no
/// detail at all: a base that redirects through six perfectly good `https` hops
/// used to be reported as not being https.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Redirect {
    /// An `https://` hop, inside the budget.
    Follow,
    /// The chain is longer than [`MAX_REDIRECTS`].
    TooManyHops,
    /// The next hop is not `https://`.
    NotHttps,
}

/// [`Redirect`], from the two facts the policy has: how many hops have already
/// been followed, and the scheme of the next one.
fn redirect_verdict(already_followed: usize, next_scheme: &str) -> Redirect {
    if already_followed >= MAX_REDIRECTS {
        Redirect::TooManyHops
    } else if next_scheme == "https" {
        Redirect::Follow
    } else {
        Redirect::NotHttps
    }
}

/// The marker [`Redirect::TooManyHops`] fails the request with, and the only
/// thing in this module that produces a `reqwest` error whose
/// [`is_redirect`](reqwest::Error::is_redirect) is true — which is what
/// [`get_checked`] keys the hop-limit refusal off.
const TOO_MANY_REDIRECTS: &str = "the redirect chain is longer than shed follows";

/// The hop-limit refusal, which **names the limit** rather than the scheme.
fn too_many_redirects(url: &str) -> String {
    format!("{url} redirects through more than the {MAX_REDIRECTS} hops shed follows")
}

fn http_client() -> Result<reqwest::Client, BootstrapError> {
    reqwest::Client::builder()
        .user_agent(crate::http::USER_AGENT)
        .connect_timeout(CONNECT_TIMEOUT)
        .read_timeout(READ_TIMEOUT)
        .redirect(reqwest::redirect::Policy::custom(|attempt| {
            // No loopback carve-out here — see [`fetch_release`].
            match redirect_verdict(attempt.previous().len(), attempt.url().scheme()) {
                Redirect::Follow => attempt.follow(),
                // `error()` rather than `stop()`, and that is the whole fix: a
                // `stop()` surfaces the 3xx as the *response*, and every 3xx
                // reaching the status check then reads as the scheme refusal.
                // An error is a second channel, and it carries the one cause a
                // response cannot.
                Redirect::TooManyHops => attempt.error(TOO_MANY_REDIRECTS),
                // Surfaces the 3xx as the response, which the status check
                // below turns into the refusal a user reads.
                Redirect::NotHttps => attempt.stop(),
            }
        }))
        .build()
        .map_err(|error| BootstrapError::Download(format!("building an HTTP client: {error}")))
}

/// GET `url` and check the status, before any body is read.
async fn get_checked(client: &reqwest::Client, url: &str) -> Result<reqwest::Response, String> {
    let response = client.get(url).send().await.map_err(|error| {
        // The policy above is the only redirect error this client can produce:
        // a `Location` reqwest cannot parse ends the chain by handing back the
        // 3xx response, and nothing here uses `Policy::limited`.
        if error.is_redirect() {
            too_many_redirects(url)
        } else {
            format!("{url}: {error}")
        }
    })?;
    let status = response.status();
    if status.is_redirection() {
        return Err(format!(
            "{url} answered {status}, and shed follows a redirect only to an https:// URL"
        ));
    }
    if !status.is_success() {
        return Err(format!("{url} answered {status}"));
    }
    Ok(response)
}

/// Stream a body into `file`, refusing anything past `max`, and hand the file
/// back.
///
/// **The writes cross `spawn_blocking`, like the hash does.** `std::fs::File`'s
/// `write_all` is a blocking syscall, and this function is `async`: a transfer
/// near [`ASSET_MAX_BYTES`] would otherwise occupy a tokio worker for its whole
/// duration, inside the one function that already knew to send its hashing to a
/// blocking thread.
async fn download_into(
    client: &reqwest::Client,
    url: &str,
    file: std::fs::File,
    max: u64,
) -> Result<(std::fs::File, u64), BootstrapError> {
    let response = get_checked(client, url)
        .await
        .map_err(BootstrapError::Download)?;
    if let Some(declared) = response.content_length() {
        if declared > max {
            return Err(BootstrapError::Download(format!(
                "{url} declares {declared} bytes, past the {max}-byte limit"
            )));
        }
    }

    let mut file = file;
    let mut written: u64 = 0;
    let mut batch = Vec::new();
    let mut batched: usize = 0;
    let mut body = response.bytes_stream();
    while let Some(chunk) = body.next().await {
        let chunk = chunk.map_err(|error| BootstrapError::Download(format!("{url}: {error}")))?;
        written += chunk.len() as u64;
        // The counting half of the cap. A server that declares nothing, or
        // declares a lie, is stopped here rather than at the check above — and
        // before its bytes reach the batch, so the refusal costs no write.
        if written > max {
            return Err(BootstrapError::Download(format!(
                "{url} is more than the {max}-byte limit"
            )));
        }
        batched += chunk.len();
        batch.push(chunk);
        if batched >= WRITE_BATCH {
            file = write_batch(file, std::mem::take(&mut batch), false).await?;
            batched = 0;
        }
    }
    let file = write_batch(file, batch, true).await?;
    Ok((file, written))
}

/// Write the accumulated chunks on a blocking thread and take the file back.
///
/// Generic over the chunk type so that the body's `Bytes` go across without a
/// copy and without this module naming `bytes` — the `Vec` is moved into the
/// task, which is also what keeps the batch's lifetime obvious.
async fn write_batch<B: AsRef<[u8]> + Send + 'static>(
    file: std::fs::File,
    batch: Vec<B>,
    flush: bool,
) -> Result<std::fs::File, BootstrapError> {
    tokio::task::spawn_blocking(move || {
        let mut file = file;
        for chunk in &batch {
            file.write_all(chunk.as_ref())?;
        }
        if flush {
            file.flush()?;
        }
        Ok::<_, std::io::Error>(file)
    })
    .await
    .map_err(|error| {
        BootstrapError::Download(format!("writing the download was interrupted: {error}"))
    })?
    .map_err(|error| BootstrapError::Download(format!("writing the download: {error}")))
}

/// The checksum sibling: small enough to hold in memory, and bounded so that a
/// server answering it with an infinity cannot be held in memory.
async fn download_to_string(
    client: &reqwest::Client,
    url: &str,
    max: u64,
) -> Result<String, BootstrapError> {
    let response = get_checked(client, url).await.map_err(|detail| {
        BootstrapError::Checksum(format!("its .sha256 could not be read: {detail}"))
    })?;
    if let Some(declared) = response.content_length() {
        if declared > max {
            return Err(BootstrapError::Checksum(format!(
                "the published checksum file declares {declared} bytes, past the {max}-byte limit"
            )));
        }
    }

    let mut text = Vec::new();
    let mut body = response.bytes_stream();
    while let Some(chunk) = body.next().await {
        let chunk = chunk.map_err(|error| {
            BootstrapError::Checksum(format!("its .sha256 could not be read: {error}"))
        })?;
        text.extend_from_slice(&chunk);
        if text.len() as u64 > max {
            return Err(BootstrapError::Checksum(format!(
                "the published checksum file is more than the {max}-byte limit"
            )));
        }
    }
    String::from_utf8(text).map_err(|_| {
        BootstrapError::Checksum("the published checksum file is not UTF-8".to_string())
    })
}

/// Hash what the descriptor holds and hand the descriptor back, rewound.
///
/// The file is neither named nor re-opened: what was hashed and what gets
/// streamed are provably one open file.
fn hash_open_file(mut file: std::fs::File) -> std::io::Result<(String, std::fs::File)> {
    file.seek(SeekFrom::Start(0))?;
    let mut hasher = Sha256::new();
    let mut buffer = vec![0u8; HASH_CHUNK];
    loop {
        let read = file.read(&mut buffer)?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    file.seek(SeekFrom::Start(0))?;
    Ok((super::hex(&hasher.finalize()), file))
}

#[cfg(test)]
mod tests;
