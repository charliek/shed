//! This machine's identity: a stable id and a display name, kept in one small file that
//! every broker on the machine reads.
//!
//! A shed-server pins one machine per user as the source that answers credential
//! requests (plan 035 §3.4). For that it needs to tell machines apart, and two keys or
//! two certificates are not a machine: the same person's laptop and desktop can share an
//! SSH key, and one machine enrols a fresh certificate whenever its old one lapses. So a
//! machine names itself, once, here:
//!
//! ```text
//! <state>/broker/machine.json   0600  {"schema":1,"id":"<32 hex>","name":"<name>","created_at":"<RFC3339>"}
//! <state>/broker/machine.lock   0600  the advisory lock that serializes a create or repair
//! ```
//!
//! `<state>` is [`crate::paths::state_dir`], the directory the credential store already
//! lives under, so the broker reads the same file and presents the same id on every start.
//! (A process started with a different `SHED_BROKER_STATE_DIR` is, by construction, a
//! different machine.)
//!
//! * **id** — 32 lowercase hexadecimal characters, no hyphens (a v4 UUID's simple form).
//!   Never derived from anything on the machine, so a cloned disk or a restored backup is
//!   the one way two machines can share it; the upgrade guide says to delete the file on
//!   a machine restored from another's backup.
//! * **name** — what a person sees and types (`shed source use <name>`). Derived from the
//!   hostname ONCE, at create, and then only ever read: renaming the host does not rename
//!   the machine, and a hand-edited `name` is honoured. It is 1 to 63 bytes of UTF-8 with
//!   no control characters ([`valid_name`]), which is what the server accepts in its
//!   machine-name header.
//!
//! ## What happens on each kind of file
//!
//! The id is what a server pins, so the one rule above all the others: **an id that can
//! be read is never thrown away.** The id and the schema are each read on their own,
//! before anything else in the file is looked at, so no other field — missing, `null`,
//! the wrong type, malformed — can cost the machine its id.
//!
//! | On disk | Result |
//! |---|---|
//! | missing | created ([`LoadOutcome::Created`]) |
//! | valid | read as is ([`LoadOutcome::Existing`]); never rewritten |
//! | a valid id, and anything else wrong: `name` not a [`valid_name`] string, `created_at` not an RFC3339 string, `schema` missing or not a number above 0 | the id is kept, the rest repaired (a name re-derived from the hostname, `created_at` set to now, schema 1) and written back ([`LoadOutcome::Repaired`]) |
//! | `schema` above [`MACHINE_SCHEMA`] (a newer build wrote it) | **never rewritten, moved or replaced.** A valid id is used, with the file's name if it is a valid one, else a name derived for this run only ([`LoadOutcome::Repaired`] with `written: false`). No valid id: `Err` naming the schema, and the broker runs without an identity |
//! | no valid id can be read (not JSON, not an object, too large, `id` missing, not a string, or not 32 lowercase hex) | copied aside to `machine.json.corrupt`, then a new identity is written over it in one atomic rename ([`LoadOutcome::Replaced`]) — the credstore's "stored but unusable → enrol afresh". It is LOUD: the caller logs it, because the server will see a new machine that has to be chosen again. If the copy or the new write fails, `Err`; `machine.json` is never left missing, so a later start never mistakes this for a first run |
//! | cannot be read (permissions, I/O), or a missing/corrupt file that cannot be written | `Err`: the caller runs WITHOUT an identity and reports why in status. A file that exists but cannot be read is never overwritten, since it may be perfectly good |
//!
//! Failing the daemon's start on an unusable file was considered and rejected: the
//! credential store never fails a start (an unusable credential is re-enrolled, an
//! unwritable one is kept in memory), so this module does not either. A broker with no
//! identity keeps brokering, and the status report carries the error. What a server does
//! with a broker that presents no identity is the server's rule, not this file's.
//!
//! ## Durability
//!
//! A write here (create, repair, the copy aside, the replacement) counts as done only
//! once the new file is fsynced, the directory it was renamed into is fsynced, and so is
//! the parent of every directory this code created on the way. A sync that fails fails
//! the write, so the caller runs without an identity and says why, rather than handing a
//! server an id that a power loss could take back (the next start would mint another).
//! The one exception is a filesystem that cannot sync a directory at all (`EINVAL` or
//! `ENOTSUP`): there is nothing more to do, and refusing would leave such a machine
//! without an identity for good. (The credential store keeps its own best-effort
//! directory sync for its files.)
//!
//! ## Two processes creating it at once
//!
//! Reads take no lock: the file is only ever replaced by an atomic rename
//! ([`crate::credstore::atomic_write_durable`]: temp file in the same directory, fsynced,
//! renamed), so a reader sees a whole old file or a whole new one. Anything that WRITES —
//! a create, a repair, a replacement — takes an exclusive `flock` on the sibling
//! `machine.lock` and then reads the file AGAIN under the lock, and writes only if it is
//! still missing or still broken. So when the daemon and the desktop app start together
//! on a fresh machine, one of them creates the identity and the other, blocked on the
//! lock, finds it already there and reads it: one machine, one id.

use std::fs;
use std::io::Read as _;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

use crate::credstore::{atomic_write_durable, ensure_dir, FileLock};

/// The identity file's basename, under `<state>/broker/`.
pub const MACHINE_FILE_NAME: &str = "machine.json";
/// The lock serializing a create, repair or replacement of [`MACHINE_FILE_NAME`]. Never
/// removed once created (deleting a lock file someone holds breaks the mutex).
const LOCK_FILE_NAME: &str = "machine.lock";
/// Where an identity file with no readable id is copied before a new one replaces it, so
/// what was there can still be looked at.
const CORRUPT_SUFFIX: &str = ".corrupt";
/// The on-disk format version this build writes.
pub const MACHINE_SCHEMA: u32 = 1;
/// The longest machine name, in bytes of UTF-8 (plan 035 §3.4).
pub const MAX_NAME_BYTES: usize = 63;
/// The length of a machine id: 32 lowercase hexadecimal characters.
pub const ID_LEN: usize = 32;
/// An identity file is a few hundred bytes; anything larger than this is not one.
const MAX_FILE_BYTES: u64 = 64 * 1024;
/// How much of an unexpected field value a reason quotes.
const QUOTE_CHARS: usize = 80;

/// One machine's identity: what it presents to every server it brokers for.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct MachineIdentity {
    /// 32 lowercase hex characters ([`valid_id`]).
    pub id: String,
    /// 1–63 bytes of UTF-8, no control characters ([`valid_name`]).
    pub name: String,
}

/// How [`load_or_create_at`] came by the identity it returns. Everything but
/// [`LoadOutcome::Existing`] is worth a log line.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LoadOutcome {
    /// The file was there and valid.
    Existing,
    /// There was no file; this call created it.
    Created,
    /// The file held a valid id but something else was wrong (`reason`); the id was kept
    /// and the rest repaired. `written` is false when the file was left alone because a
    /// newer build wrote it (its schema is above [`MACHINE_SCHEMA`]): the repaired name is
    /// then this run's only.
    Repaired { reason: String, written: bool },
    /// No id could be read from the file; it was copied to `old_file` and a new identity
    /// written in its place. The machine now has a NEW id.
    Replaced { reason: String, old_file: PathBuf },
}

/// An identity plus how it was obtained.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LoadedIdentity {
    pub identity: MachineIdentity,
    pub outcome: LoadOutcome,
}

/// The identity file's path: `<state>/broker/machine.json`, beside the broker's
/// credential store (`<state>/broker/creds/`).
pub fn machine_state_path() -> PathBuf {
    crate::paths::state_dir()
        .join("broker")
        .join(MACHINE_FILE_NAME)
}

/// Load this machine's identity from [`machine_state_path`], creating it on first use.
/// See the module doc for what each kind of file yields.
pub fn load_or_create() -> Result<LoadedIdentity, String> {
    load_or_create_at(&machine_state_path())
}

/// [`load_or_create`] at an explicit path (tests, and an embedder with its own root).
pub fn load_or_create_at(path: &Path) -> Result<LoadedIdentity, String> {
    load_or_create_io(
        path,
        &Io {
            hostname: &system_hostname,
            before_lock: &|| {},
            sync_dir: &sync_dir,
        },
    )
}

/// The outside inputs of a load, injectable for tests: the hostname; a hook run between
/// the lock-free read and taking the lock (the seam the race tests use to force the
/// interleaving where another process writes in between); and the directory sync (the
/// seam the durability tests use to record or fail it).
struct Io<'a> {
    hostname: &'a dyn Fn() -> Option<String>,
    before_lock: &'a dyn Fn(),
    sync_dir: &'a dyn Fn(&Path) -> std::io::Result<()>,
}

/// The body of [`load_or_create_at`].
fn load_or_create_io(path: &Path, io: &Io) -> Result<LoadedIdentity, String> {
    match read(path)? {
        OnDisk::Valid(identity) => return Ok(existing(identity)),
        OnDisk::Newer { id, reason } => return Ok(newer(io, id, reason)),
        OnDisk::Missing | OnDisk::Repair { .. } | OnDisk::Corrupt(_) => {}
    }
    (io.before_lock)();

    let dir = parent_dir(path);
    ensure_dir_durable(dir, io.sync_dir)?;
    let _lock = FileLock::acquire(&dir.join(LOCK_FILE_NAME))?;

    // Read again under the lock: another process may have created or repaired the file
    // since the read above, and its identity must win over a second fresh one.
    match read(path)? {
        OnDisk::Valid(identity) => Ok(existing(identity)),
        OnDisk::Newer { id, reason } => Ok(newer(io, id, reason)),
        OnDisk::Missing => {
            let identity = new_identity(io.hostname);
            write(path, &identity, &crate::time::now_rfc3339(), io.sync_dir)?;
            Ok(LoadedIdentity {
                identity,
                outcome: LoadOutcome::Created,
            })
        }
        OnDisk::Repair {
            id,
            name,
            created_at,
            reason,
        } => {
            let name = name.unwrap_or_else(|| derive_name(io.hostname, &id));
            let created_at = created_at.unwrap_or_else(crate::time::now_rfc3339);
            let identity = MachineIdentity { id, name };
            write(path, &identity, &created_at, io.sync_dir)?;
            Ok(LoadedIdentity {
                identity,
                outcome: LoadOutcome::Repaired {
                    reason,
                    written: true,
                },
            })
        }
        OnDisk::Corrupt(reason) => {
            // Copy, then replace in one rename — never move the file away first. A
            // rename-aside leaves `machine.json` missing until the new file lands, and a
            // failure or a power cut in between would make the next start look like an
            // ordinary first run, with the cause gone from view.
            let old_file = corrupt_path(path);
            copy_aside(path, &old_file, io.sync_dir).map_err(|e| {
                format!(
                    "machine identity {} has no usable id ({reason}) and could not be copied \
                     aside to {}, so it was left as it was: {e}",
                    path.display(),
                    old_file.display()
                )
            })?;
            let identity = new_identity(io.hostname);
            write(path, &identity, &crate::time::now_rfc3339(), io.sync_dir).map_err(|e| {
                format!(
                    "machine identity {} has no usable id ({reason}); it was copied to {}, \
                     but writing a new identity in its place failed: {e}",
                    path.display(),
                    old_file.display()
                )
            })?;
            Ok(LoadedIdentity {
                identity,
                outcome: LoadOutcome::Replaced { reason, old_file },
            })
        }
    }
}

fn existing(identity: MachineIdentity) -> LoadedIdentity {
    LoadedIdentity {
        identity,
        outcome: LoadOutcome::Existing,
    }
}

/// A newer build's file with a valid id but no usable name: the id is used, and a name
/// derived for this run only. The file is not touched (no lock is needed to not write).
fn newer(io: &Io, id: String, reason: String) -> LoadedIdentity {
    let name = derive_name(io.hostname, &id);
    LoadedIdentity {
        identity: MachineIdentity { id, name },
        outcome: LoadOutcome::Repaired {
            reason,
            written: false,
        },
    }
}

/// Whether `id` is a machine id: exactly 32 lowercase hexadecimal characters.
pub fn valid_id(id: &str) -> bool {
    id.len() == ID_LEN && id.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

/// Whether `name` is a machine name: 1 to [`MAX_NAME_BYTES`] bytes of UTF-8 (a `&str`
/// is UTF-8 by construction) with no control character.
pub fn valid_name(name: &str) -> bool {
    !name.is_empty() && name.len() <= MAX_NAME_BYTES && !name.chars().any(char::is_control)
}

/// The machine name a hostname yields, or `None` when nothing usable is left.
///
/// The first DNS label is taken (`laptop.local` → `laptop`; `build-07.example.org` →
/// `build-07`) unless the hostname is an IP address, which is kept whole. Control
/// characters are dropped, surrounding whitespace trimmed, and the result cut to
/// [`MAX_NAME_BYTES`] at a character boundary. Case is kept.
pub fn name_from_hostname(hostname: &str) -> Option<String> {
    let host = hostname.trim();
    let label = if host.parse::<std::net::IpAddr>().is_ok() {
        host
    } else {
        match host.split('.').next() {
            Some(first) if !first.trim().is_empty() => first,
            _ => host,
        }
    };
    sanitize_name(label)
}

/// Make `raw` a [`valid_name`] if anything of it can be kept: drop control characters,
/// trim, and truncate to [`MAX_NAME_BYTES`] without splitting a character.
pub fn sanitize_name(raw: &str) -> Option<String> {
    let cleaned: String = raw.chars().filter(|c| !c.is_control()).collect();
    let mut end = 0;
    for (i, c) in cleaned.trim().char_indices() {
        if i + c.len_utf8() > MAX_NAME_BYTES {
            break;
        }
        end = i + c.len_utf8();
    }
    let name = cleaned.trim()[..end].trim_end();
    (!name.is_empty()).then(|| name.to_string())
}

/// The system hostname (`gethostname(2)`), lossily decoded; `None` if the call fails.
fn system_hostname() -> Option<String> {
    let mut buf = [0u8; 256];
    // SAFETY: the pointer and length describe `buf`, which outlives the call.
    let rc = unsafe { libc::gethostname(buf.as_mut_ptr().cast(), buf.len()) };
    if rc != 0 {
        return None;
    }
    let end = buf.iter().position(|&b| b == 0).unwrap_or(buf.len());
    Some(String::from_utf8_lossy(&buf[..end]).into_owned())
}

/// A fresh identity: a random id and a name derived from the hostname.
fn new_identity(hostname: &dyn Fn() -> Option<String>) -> MachineIdentity {
    let id = uuid::Uuid::new_v4().simple().to_string();
    let name = derive_name(hostname, &id);
    MachineIdentity { id, name }
}

/// The hostname's name, else `machine-<first 8 of the id>`.
fn derive_name(hostname: &dyn Fn() -> Option<String>, id: &str) -> String {
    hostname()
        .as_deref()
        .and_then(name_from_hostname)
        .unwrap_or_else(|| format!("machine-{}", &id[..8.min(id.len())]))
}

fn corrupt_path(path: &Path) -> PathBuf {
    let mut name = path
        .file_name()
        .map(|n| n.to_os_string())
        .unwrap_or_default();
    name.push(CORRUPT_SUFFIX);
    path.with_file_name(name)
}

/// `path`'s directory, `.` for a bare file name.
fn parent_dir(path: &Path) -> &Path {
    match path.parent() {
        Some(p) if !p.as_os_str().is_empty() => p,
        _ => Path::new("."),
    }
}

/// The production directory sync: fsync the directory, accepting a filesystem that
/// cannot sync a directory at all ([`tolerate_unsupported_sync`]).
fn sync_dir(dir: &Path) -> std::io::Result<()> {
    tolerate_unsupported_sync(fs::File::open(dir)?.sync_all())
}

/// `EINVAL` / `ENOTSUP` from a directory fsync mean the filesystem does not support
/// syncing a directory — not that a sync was attempted and failed — so there is nothing
/// further to do and it is not an error. Every other error is.
fn tolerate_unsupported_sync(r: std::io::Result<()>) -> std::io::Result<()> {
    match r {
        Err(e)
            if e.raw_os_error()
                .is_some_and(|c| [libc::EINVAL, libc::ENOTSUP, libc::EOPNOTSUPP].contains(&c)) =>
        {
            Ok(())
        }
        r => r,
    }
}

/// [`ensure_dir`] (create, tighten to 0700), and then make every directory it CREATED
/// durable by syncing that directory's parent, where its entry lives. The file's own
/// directory is synced by the write that follows.
fn ensure_dir_durable(
    dir: &Path,
    sync_dir: &dyn Fn(&Path) -> std::io::Result<()>,
) -> Result<(), String> {
    let mut created = Vec::new();
    let mut cur = Some(dir);
    while let Some(d) = cur {
        if d.as_os_str().is_empty() || d.exists() {
            break;
        }
        created.push(d);
        cur = d.parent();
    }
    ensure_dir(dir)?;
    for d in created {
        let parent = parent_dir(d);
        sync_dir(parent).map_err(|e| {
            format!(
                "sync dir {} after creating {}: {e}",
                parent.display(),
                d.display()
            )
        })?;
    }
    Ok(())
}

/// Copy the file at `path` to `to` (replacing any older copy), durably. `path` itself is
/// not touched.
fn copy_aside(
    path: &Path,
    to: &Path,
    sync_dir: &dyn Fn(&Path) -> std::io::Result<()>,
) -> Result<(), String> {
    let mut src = fs::File::open(path).map_err(|e| format!("open {}: {e}", path.display()))?;
    atomic_write_durable(to, &mut src, sync_dir)
}

/// The file's on-disk shape as this build writes it. Reading does NOT decode into this
/// (see [`classify`]): it would make every field's type a condition of keeping the id.
#[derive(Serialize)]
struct MachineFile<'a> {
    schema: u32,
    id: &'a str,
    name: &'a str,
    created_at: &'a str,
}

/// What [`read`] found.
#[derive(Debug)]
enum OnDisk {
    Missing,
    Valid(MachineIdentity),
    /// A valid id in a file this build may write, with something else wrong. `name` and
    /// `created_at` are the file's own when they are valid, kept by the repair.
    Repair {
        id: String,
        name: Option<String>,
        created_at: Option<String>,
        reason: String,
    },
    /// A newer build's file: a valid id, but no valid name. Used, never written.
    Newer {
        id: String,
        reason: String,
    },
    /// No id can be read at all.
    Corrupt(String),
}

/// Read and classify the file. `Err` for a file that exists but cannot be READ — one that
/// may be fine and so must not be replaced — and for a newer build's file with no id this
/// build can read.
fn read(path: &Path) -> Result<OnDisk, String> {
    let file = match fs::File::open(path) {
        Ok(f) => f,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(OnDisk::Missing),
        Err(e) => return Err(format!("read machine identity {}: {e}", path.display())),
    };
    let mut raw = Vec::new();
    file.take(MAX_FILE_BYTES + 1)
        .read_to_end(&mut raw)
        .map_err(|e| format!("read machine identity {}: {e}", path.display()))?;
    classify(&raw).map_err(|reason| format!("machine identity {}: {reason}", path.display()))
}

/// Classify a file's bytes. The id and the schema are each read on their own, as JSON
/// values, before anything else — so a field of the wrong type elsewhere never stops a
/// valid id from being kept, and a newer schema is recognised whatever its other fields.
/// `Err` is a newer build's file with no id this build can read: left alone, and no
/// identity.
fn classify(raw: &[u8]) -> Result<OnDisk, String> {
    if raw.len() as u64 > MAX_FILE_BYTES {
        return Ok(OnDisk::Corrupt(format!(
            "larger than {MAX_FILE_BYTES} bytes"
        )));
    }
    let value: Value = match serde_json::from_slice(raw) {
        Ok(v) => v,
        Err(e) => return Ok(OnDisk::Corrupt(format!("not JSON: {e}"))),
    };
    let Some(obj) = value.as_object() else {
        return Ok(OnDisk::Corrupt(format!(
            "not a JSON object: {}",
            quote(Some(&value))
        )));
    };
    let id = read_id(obj);
    let name = obj
        .get("name")
        .and_then(Value::as_str)
        .filter(|n| valid_name(n))
        .map(str::to_string);

    Ok(match read_schema(obj.get("schema")) {
        Schema::Newer(schema) => match (id, name) {
            (Ok(id), Some(name)) => OnDisk::Valid(MachineIdentity { id, name }),
            (Ok(id), None) => OnDisk::Newer {
                id,
                reason: format!(
                    "written by a newer build (schema {schema}) and {}; a name is derived \
                     for this run and the file is left as it is",
                    name_problem(obj.get("name"))
                ),
            },
            (Err(id_problem), _) => {
                return Err(format!(
                    "written by a newer build (schema {schema}), and {id_problem}, so this \
                     build cannot read its id; the file is left as it is (upgrade this broker)"
                ))
            }
        },
        schema => {
            let id = match id {
                Ok(id) => id,
                Err(id_problem) => return Ok(OnDisk::Corrupt(id_problem)),
            };
            let mut problems = Vec::new();
            if let Schema::Bad(problem) = schema {
                problems.push(problem);
            }
            if name.is_none() {
                problems.push(name_problem(obj.get("name")));
            }
            let created_at = match obj.get("created_at") {
                Some(Value::String(s))
                    if matches!(crate::time::parse_rfc3339_to_unix(s), Ok(Some(_))) =>
                {
                    Some(s.clone())
                }
                other => {
                    problems.push(format!(
                        "created_at {} is not an RFC3339 time",
                        quote(other)
                    ));
                    None
                }
            };
            match (problems.is_empty(), name) {
                (true, Some(name)) => OnDisk::Valid(MachineIdentity { id, name }),
                (_, name) => OnDisk::Repair {
                    id,
                    name,
                    created_at,
                    reason: problems.join("; "),
                },
            }
        }
    })
}

/// The file's id, or why there is none.
fn read_id(obj: &Map<String, Value>) -> Result<String, String> {
    match obj.get("id") {
        Some(Value::String(id)) if valid_id(id) => Ok(id.clone()),
        Some(Value::String(_)) | None | Some(_) => Err(format!(
            "id {} is not {ID_LEN} lowercase hex characters",
            quote(obj.get("id"))
        )),
    }
}

enum Schema {
    /// [`MACHINE_SCHEMA`].
    Current,
    /// A number above [`MACHINE_SCHEMA`], as written.
    Newer(String),
    /// Missing, or anything else: repaired to [`MACHINE_SCHEMA`] when the id is valid.
    Bad(String),
}

fn read_schema(v: Option<&Value>) -> Schema {
    match v {
        Some(Value::Number(n)) if n.as_u64() == Some(u64::from(MACHINE_SCHEMA)) => Schema::Current,
        Some(Value::Number(n)) if n.as_f64().is_some_and(|f| f > f64::from(MACHINE_SCHEMA)) => {
            Schema::Newer(n.to_string())
        }
        other => Schema::Bad(format!("schema {} is not {MACHINE_SCHEMA}", quote(other))),
    }
}

fn name_problem(v: Option<&Value>) -> String {
    format!(
        "name {} is not 1 to {MAX_NAME_BYTES} bytes of text without control characters",
        quote(v)
    )
}

/// A field's value for a reason: its JSON (cut to [`QUOTE_CHARS`] characters), or
/// `missing`.
fn quote(v: Option<&Value>) -> String {
    match v {
        None => "missing".into(),
        Some(v) => {
            let s = v.to_string();
            if s.chars().count() > QUOTE_CHARS {
                format!("{}…", s.chars().take(QUOTE_CHARS).collect::<String>())
            } else {
                s
            }
        }
    }
}

fn write(
    path: &Path,
    identity: &MachineIdentity,
    created_at: &str,
    sync_dir: &dyn Fn(&Path) -> std::io::Result<()>,
) -> Result<(), String> {
    let file = MachineFile {
        schema: MACHINE_SCHEMA,
        id: &identity.id,
        name: &identity.name,
        created_at,
    };
    let mut data =
        serde_json::to_vec_pretty(&file).map_err(|e| format!("encode machine identity: {e}"))?;
    data.push(b'\n');
    atomic_write_durable(path, &mut &data[..], sync_dir)
        .map_err(|e| format!("write machine identity: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::os::unix::fs::PermissionsExt as _;
    use std::sync::{mpsc, Arc, Barrier};
    use std::thread::JoinHandle;
    use std::time::{Duration, Instant};

    /// Every wait in these tests is bounded: a locking regression must FAIL, not hang.
    const WAIT: Duration = Duration::from_secs(30);

    fn host(name: &'static str) -> impl Fn() -> Option<String> {
        move || Some(name.to_string())
    }

    fn no_host() -> Option<String> {
        None
    }

    /// A load with the real directory sync.
    fn load_or_create_with(
        path: &Path,
        hostname: &dyn Fn() -> Option<String>,
        before_lock: &dyn Fn(),
    ) -> Result<LoadedIdentity, String> {
        load_or_create_io(
            path,
            &Io {
                hostname,
                before_lock,
                sync_dir: &sync_dir,
            },
        )
    }

    /// A load whose directory syncs go through `sync`.
    fn load_with_sync(
        path: &Path,
        sync: &dyn Fn(&Path) -> std::io::Result<()>,
    ) -> Result<LoadedIdentity, String> {
        load_or_create_io(
            path,
            &Io {
                hostname: &host("desk"),
                before_lock: &|| {},
                sync_dir: sync,
            },
        )
    }

    fn join_within<T>(h: JoinHandle<T>, what: &str) -> T {
        let deadline = Instant::now() + WAIT;
        while !h.is_finished() {
            assert!(
                Instant::now() < deadline,
                "{what} did not finish within {WAIT:?}: a lock was never released"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        h.join().unwrap()
    }

    fn state() -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir
            .path()
            .join("state")
            .join("broker")
            .join(MACHINE_FILE_NAME);
        (dir, path)
    }

    /// A state path whose `broker/` dir already exists, holding `contents`.
    fn state_with(contents: &[u8]) -> (tempfile::TempDir, PathBuf) {
        let (d, path) = state();
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, contents).unwrap();
        (d, path)
    }

    fn mode_of(p: &Path) -> u32 {
        fs::metadata(p).unwrap().permissions().mode() & 0o777
    }

    fn on_disk(path: &Path) -> serde_json::Value {
        serde_json::from_slice(&fs::read(path).unwrap()).unwrap()
    }

    const ID: &str = "0123456789abcdef0123456789abcdef";

    #[test]
    fn create_then_reload_returns_the_same_identity() {
        let (_d, path) = state();
        let first = load_or_create_with(&path, &host("laptop.local"), &|| {}).unwrap();
        assert_eq!(first.outcome, LoadOutcome::Created);
        assert!(valid_id(&first.identity.id), "{}", first.identity.id);
        assert_eq!(first.identity.name, "laptop");

        // A different hostname on the second start changes nothing: the name is
        // derived once, at create.
        let second = load_or_create_with(&path, &host("renamed"), &|| {}).unwrap();
        assert_eq!(second.outcome, LoadOutcome::Existing);
        assert_eq!(second.identity, first.identity);

        let v = on_disk(&path);
        assert_eq!(v["schema"], 1);
        assert_eq!(v["id"], first.identity.id.as_str());
        assert_eq!(v["name"], "laptop");
        crate::time::parse_rfc3339_to_unix(v["created_at"].as_str().unwrap())
            .unwrap()
            .unwrap();
    }

    #[test]
    fn the_id_is_32_lowercase_hex_with_no_hyphens() {
        let (_d, path) = state();
        let id = load_or_create_with(&path, &no_host, &|| {})
            .unwrap()
            .identity
            .id;
        assert_eq!(id.len(), 32);
        assert!(!id.contains('-'));
        assert!(id
            .bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase()));
    }

    #[test]
    fn file_is_0600_in_a_0700_dir_and_leaves_no_temp_files() {
        let (_d, path) = state();
        load_or_create_with(&path, &host("h"), &|| {}).unwrap();
        let dir = path.parent().unwrap();
        assert_eq!(mode_of(&path), 0o600);
        assert_eq!(mode_of(dir), 0o700);
        let mut names: Vec<String> = fs::read_dir(dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        assert_eq!(names, vec![MACHINE_FILE_NAME, LOCK_FILE_NAME]);
    }

    #[test]
    fn a_hand_edited_name_is_honoured() {
        let (_d, path) = state();
        let id = load_or_create_with(&path, &host("h"), &|| {})
            .unwrap()
            .identity
            .id;
        let edited = format!(
            r#"{{"schema":1,"id":"{id}","name":"Charlie's desk 🖥","created_at":"2026-01-01T00:00:00Z"}}"#
        );
        fs::write(&path, &edited).unwrap();
        let got = load_or_create_with(&path, &host("h"), &|| {}).unwrap();
        assert_eq!(got.outcome, LoadOutcome::Existing);
        assert_eq!(got.identity.name, "Charlie's desk 🖥");
        assert_eq!(got.identity.id, id);
        assert_eq!(fs::read_to_string(&path).unwrap(), edited, "rewritten");
    }

    /// No id can be read: the file is copied aside and replaced, loudly — never a silent
    /// new id, and never an error that would leave the broker without one.
    #[test]
    fn a_file_with_no_readable_id_is_copied_aside_and_replaced() {
        for (raw, why) in [
            (&b"{not json"[..], "not JSON"),
            (b"[]", "not a JSON object"),
            (br#"{"schema":1,"name":"n"}"#, "id missing"),
            (br#"{"schema":1,"id":17,"name":"n"}"#, "id 17"),
            (br#"{"schema":1,"id":null,"name":"n"}"#, "id null"),
        ] {
            let (_d, path) = state_with(raw);
            let got = load_or_create_with(&path, &host("h"), &|| {}).unwrap();
            match &got.outcome {
                LoadOutcome::Replaced { reason, old_file } => {
                    assert!(reason.contains(why), "{reason}");
                    assert_eq!(old_file, &corrupt_path(&path));
                    assert_eq!(fs::read(old_file).unwrap(), raw);
                    assert_eq!(mode_of(old_file), 0o600);
                }
                other => panic!("{why}: expected Replaced, got {other:?}"),
            }
            assert!(valid_id(&got.identity.id));
            assert_eq!(on_disk(&path)["id"], got.identity.id.as_str());
        }
    }

    #[test]
    fn invalid_ids_are_corrupt() {
        for id in [
            "",
            "0123456789ABCDEF0123456789abcdef",     // upper case
            "01234567-89ab-cdef-0123-456789abcdef", // UUID form
            "0123456789abcdef0123456789abcde",      // 31
            "0123456789abcdef0123456789abcdef0",    // 33
            "0123456789abcdef0123456789abcdeg",     // not hex
            "0123456789abcdef0123456789abcdef\n",   // a trailing newline
        ] {
            let raw = serde_json::json!({"schema": 1, "id": id, "name": "n"}).to_string();
            assert!(
                matches!(classify(raw.as_bytes()), Ok(OnDisk::Corrupt(_))),
                "{id:?} accepted"
            );
        }
        for raw in [r#"[]"#, "", "null", r#""text""#] {
            assert!(
                matches!(classify(raw.as_bytes()), Ok(OnDisk::Corrupt(_))),
                "{raw:?}"
            );
        }
        let big = vec![b' '; MAX_FILE_BYTES as usize + 1];
        assert!(matches!(classify(&big), Ok(OnDisk::Corrupt(_))));
    }

    /// A good id with a bad name keeps the id: a new id would strand the machine's
    /// binding on every server for nothing more than a typo in the name.
    #[test]
    fn an_invalid_name_keeps_the_id_and_is_repaired() {
        let long = "x".repeat(64);
        for bad in ["", "tab\there", long.as_str()] {
            let raw = serde_json::json!({
                "schema": 1, "id": ID, "name": bad, "created_at": "2026-01-01T00:00:00Z"
            });
            let (_d, path) = state_with(raw.to_string().as_bytes());
            let got = load_or_create_with(&path, &host("desk.lan"), &|| {}).unwrap();
            assert!(
                matches!(got.outcome, LoadOutcome::Repaired { written: true, .. }),
                "{bad:?}: {:?}",
                got.outcome
            );
            assert_eq!(got.identity.id, ID);
            assert_eq!(got.identity.name, "desk");
            let v = on_disk(&path);
            assert_eq!(v["name"], "desk");
            assert_eq!(v["created_at"], "2026-01-01T00:00:00Z");
            assert!(!corrupt_path(&path).exists());
        }
    }

    /// The destructive-classification cases: a valid id with any OTHER field missing,
    /// `null`, the wrong type, or malformed keeps the id and repairs the rest. Each of
    /// these was once read as wholly corrupt and given a new id.
    #[test]
    fn a_valid_id_is_kept_whatever_else_is_wrong() {
        let ts = "2026-01-01T00:00:00Z";
        // (file, the name it must end with, whether the file's created_at survives)
        let cases: Vec<(serde_json::Value, &str, bool)> = vec![
            (
                serde_json::json!({"schema":1,"id":ID,"name":null,"created_at":ts}),
                "desk",
                true,
            ),
            (
                serde_json::json!({"schema":1,"id":ID,"created_at":ts}),
                "desk",
                true,
            ),
            (
                serde_json::json!({"schema":1,"id":ID,"name":7,"created_at":ts}),
                "desk",
                true,
            ),
            (
                serde_json::json!({"schema":1,"id":ID,"name":["a"],"created_at":ts}),
                "desk",
                true,
            ),
            (
                serde_json::json!({"schema":1,"id":ID,"name":{"n":"a"},"created_at":ts}),
                "desk",
                true,
            ),
            (
                serde_json::json!({"schema":1,"id":ID,"name":"kept","created_at":"yesterday"}),
                "kept",
                false,
            ),
            (
                serde_json::json!({"schema":1,"id":ID,"name":"kept","created_at":17}),
                "kept",
                false,
            ),
            (
                serde_json::json!({"schema":1,"id":ID,"name":"kept","created_at":null}),
                "kept",
                false,
            ),
            (
                serde_json::json!({"schema":1,"id":ID,"name":"kept"}),
                "kept",
                false,
            ),
            (
                serde_json::json!({"id":ID,"name":"kept","created_at":ts}),
                "kept",
                true,
            ),
            (
                serde_json::json!({"schema":0,"id":ID,"name":"kept","created_at":ts}),
                "kept",
                true,
            ),
            (
                serde_json::json!({"schema":"1","id":ID,"name":"kept","created_at":ts}),
                "kept",
                true,
            ),
            (
                serde_json::json!({"schema":null,"id":ID,"name":"kept","created_at":ts}),
                "kept",
                true,
            ),
            (
                serde_json::json!({"schema":-3,"id":ID,"name":"kept","created_at":ts}),
                "kept",
                true,
            ),
            (
                serde_json::json!({"schema":[1],"id":ID,"name":false,"created_at":{}}),
                "desk",
                false,
            ),
        ];
        for (raw, want_name, keeps_ts) in cases {
            let (_d, path) = state_with(raw.to_string().as_bytes());
            let got = load_or_create_with(&path, &host("desk"), &|| {})
                .unwrap_or_else(|e| panic!("{raw}: {e}"));
            assert!(
                matches!(got.outcome, LoadOutcome::Repaired { written: true, .. }),
                "{raw}: {:?}",
                got.outcome
            );
            assert_eq!(got.identity.id, ID, "{raw}");
            assert_eq!(got.identity.name, want_name, "{raw}");
            assert!(!corrupt_path(&path).exists(), "{raw}: moved aside");
            let v = on_disk(&path);
            assert_eq!(v["schema"], 1, "{raw}");
            assert_eq!(v["id"], ID, "{raw}");
            assert_eq!(v["name"], want_name, "{raw}");
            let at = v["created_at"].as_str().unwrap();
            crate::time::parse_rfc3339_to_unix(at).unwrap().unwrap();
            assert_eq!(at == ts, keeps_ts, "{raw}: created_at {at}");
            // And the repaired file now reads as is.
            let again = load_or_create_with(&path, &host("other"), &|| {}).unwrap();
            assert_eq!(again.outcome, LoadOutcome::Existing, "{raw}");
            assert_eq!(again.identity, got.identity, "{raw}");
        }
    }

    /// A newer build's file is never rewritten, moved or replaced, whatever the types of
    /// its other fields: its id is used whenever it can be read.
    #[test]
    fn a_newer_schema_is_never_rewritten_whatever_its_fields() {
        // Readable id and name: used as is.
        for raw in [
            serde_json::json!({"schema":2,"id":ID,"name":"desk","created_at":{"unix":1}}),
            serde_json::json!({"schema":2,"id":ID,"name":"desk","created_at":null}),
            serde_json::json!({"schema":7,"id":ID,"name":"desk","more":[1,2]}),
            serde_json::json!({"schema":4294967296u64,"id":ID,"name":"desk"}),
            serde_json::json!({"schema":2.5,"id":ID,"name":"desk"}),
        ] {
            let text = raw.to_string();
            let (_d, path) = state_with(text.as_bytes());
            let got = load_or_create_with(&path, &host("other"), &|| {}).unwrap();
            assert_eq!(got.outcome, LoadOutcome::Existing, "{text}");
            assert_eq!(got.identity.id, ID, "{text}");
            assert_eq!(got.identity.name, "desk", "{text}");
            assert_eq!(fs::read_to_string(&path).unwrap(), text);
            assert!(!corrupt_path(&path).exists(), "{text}");
        }
        // Readable id, no usable name: the id is used, a name derived for this run only.
        for raw in [
            serde_json::json!({"schema":2,"id":ID,"name":"","extra":true}),
            serde_json::json!({"schema":2,"id":ID,"name":null}),
            serde_json::json!({"schema":3,"id":ID,"name":{"display":"x"},"created_at":5}),
            serde_json::json!({"schema":2,"id":ID}),
        ] {
            let text = raw.to_string();
            let (_d, path) = state_with(text.as_bytes());
            let got = load_or_create_with(&path, &host("desk.lan"), &|| {}).unwrap();
            match &got.outcome {
                LoadOutcome::Repaired { reason, written } => {
                    assert!(!written, "{text}");
                    assert!(reason.contains("newer build"), "{reason}");
                }
                other => panic!("{text}: {other:?}"),
            }
            assert_eq!(got.identity.id, ID, "{text}");
            assert_eq!(got.identity.name, "desk", "{text}");
            assert_eq!(fs::read_to_string(&path).unwrap(), text);
            assert!(!corrupt_path(&path).exists(), "{text}");
        }
        // No readable id: no identity, an error naming the schema, the file untouched.
        for (raw, schema) in [
            (
                serde_json::json!({"schema":2,"id":{"hex":ID},"name":"desk"}),
                "schema 2",
            ),
            (serde_json::json!({"schema":2,"id":17}), "schema 2"),
            (serde_json::json!({"schema":5,"name":"desk"}), "schema 5"),
            (
                serde_json::json!({"schema":2,"id":"0123456789ABCDEF0123456789ABCDEF"}),
                "schema 2",
            ),
        ] {
            let text = raw.to_string();
            let (_d, path) = state_with(text.as_bytes());
            let err = load_or_create_with(&path, &host("h"), &|| {}).unwrap_err();
            assert!(err.contains(schema) && err.contains("newer build"), "{err}");
            assert!(err.contains("left as it is"), "{err}");
            assert_eq!(fs::read_to_string(&path).unwrap(), text);
            assert!(!corrupt_path(&path).exists(), "{text}");
        }
    }

    /// When a replacement cannot be completed, `machine.json` is never left missing
    /// (which the next start would read as an ordinary first run, hiding the cause): fail
    /// each directory sync of the replacement in turn and look at what is left.
    #[test]
    fn a_failed_replacement_never_leaves_the_path_empty() {
        let garbage = b"{not json";
        let mut fail_at = 0;
        loop {
            let (_d, path) = state_with(garbage);
            let calls = RefCell::new(0);
            let sync = |dir: &Path| {
                let n = *calls.borrow();
                *calls.borrow_mut() += 1;
                if n == fail_at {
                    Err(std::io::Error::from_raw_os_error(libc::EIO))
                } else {
                    sync_dir(dir)
                }
            };
            let got = load_with_sync(&path, &sync);
            let made = *calls.borrow();
            assert!(path.exists(), "fail_at {fail_at}: machine.json is gone");
            if fail_at >= made {
                // Every sync passed: the replacement completed.
                assert!(matches!(got.unwrap().outcome, LoadOutcome::Replaced { .. }));
                assert_eq!(fail_at, 2, "a replacement makes two directory syncs");
                break;
            }
            let err = got.unwrap_err();
            assert!(err.contains("has no usable id (not JSON"), "{err}");
            assert!(err.contains(".corrupt"), "{err}");
            if fail_at == 0 {
                // The copy aside failed: the file is exactly as it was.
                assert_eq!(fs::read(&path).unwrap(), garbage);
                assert!(err.contains("left as it was"), "{err}");
            } else {
                // The new file's rename landed but could not be made durable: reported as
                // a failure, and the path holds a whole identity, not nothing.
                assert!(valid_id(on_disk(&path)["id"].as_str().unwrap()));
                assert_eq!(fs::read(corrupt_path(&path)).unwrap(), garbage);
            }
            fail_at += 1;
        }
    }

    /// A create syncs the rename's directory and the parent of every directory it made,
    /// so a power loss cannot take the identity back after it was reported.
    #[test]
    fn a_create_syncs_the_rename_and_every_directory_it_made() {
        let (d, path) = state();
        let synced = RefCell::new(Vec::<PathBuf>::new());
        let sync = |dir: &Path| {
            synced.borrow_mut().push(dir.to_path_buf());
            sync_dir(dir)
        };
        let got = load_with_sync(&path, &sync).unwrap();
        assert_eq!(got.outcome, LoadOutcome::Created);
        let mut synced = synced.into_inner();
        synced.sort();
        let state_dir = d.path().join("state");
        let mut want = vec![
            d.path().to_path_buf(),   // holds the new `state/`
            state_dir.clone(),        // holds the new `broker/`
            state_dir.join("broker"), // holds the renamed `machine.json`
        ];
        want.sort();
        assert_eq!(synced, want);

        // A second start with everything in place syncs nothing.
        let again = RefCell::new(0);
        let count = |_: &Path| {
            *again.borrow_mut() += 1;
            Ok(())
        };
        assert_eq!(
            load_with_sync(&path, &count).unwrap().outcome,
            LoadOutcome::Existing
        );
        assert_eq!(*again.borrow(), 0);
    }

    /// A sync that fails is a failed create: the caller runs without an identity and
    /// says why, rather than presenting an id a power loss could take back.
    #[test]
    fn a_failed_sync_fails_the_create() {
        for fail_on in ["state", "broker"] {
            let (_d, path) = state();
            let sync = |dir: &Path| {
                if dir.ends_with(fail_on) {
                    Err(std::io::Error::from_raw_os_error(libc::EIO))
                } else {
                    Ok(())
                }
            };
            let err = load_with_sync(&path, &sync).unwrap_err();
            assert!(err.contains("sync dir"), "{fail_on}: {err}");
        }
        // The same for a repair.
        let raw = serde_json::json!({"schema":1,"id":ID,"name":null});
        let (_d, path) = state_with(raw.to_string().as_bytes());
        let eio = |_: &Path| Err(std::io::Error::from_raw_os_error(libc::EIO));
        let err = load_with_sync(&path, &eio).unwrap_err();
        assert!(
            err.contains("write machine identity") && err.contains("sync dir"),
            "{err}"
        );
    }

    /// A filesystem that cannot sync a directory at all is not a failure; every real
    /// error still is.
    #[test]
    fn an_unsupported_directory_sync_is_not_an_error() {
        for code in [libc::EINVAL, libc::ENOTSUP, libc::EOPNOTSUPP] {
            let r = Err(std::io::Error::from_raw_os_error(code));
            assert!(tolerate_unsupported_sync(r).is_ok(), "{code}");
        }
        for code in [libc::EIO, libc::ENOSPC, libc::EROFS, libc::EBADF] {
            let r = Err(std::io::Error::from_raw_os_error(code));
            assert!(tolerate_unsupported_sync(r).is_err(), "{code}");
        }
        assert!(tolerate_unsupported_sync(Ok(())).is_ok());
    }

    /// A file that exists but cannot be read is an error, and is left exactly as it was:
    /// it may well be a good identity.
    #[test]
    fn an_unreadable_file_is_an_error_and_is_not_replaced() {
        if unsafe { libc::geteuid() } == 0 {
            return; // root reads through any mode
        }
        let (_d, path) = state();
        let id = load_or_create_with(&path, &host("h"), &|| {})
            .unwrap()
            .identity
            .id;
        fs::set_permissions(&path, fs::Permissions::from_mode(0o000)).unwrap();
        let err = load_or_create_with(&path, &host("h"), &|| {}).unwrap_err();
        assert!(err.contains("read machine identity"), "{err}");
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600)).unwrap();
        assert_eq!(on_disk(&path)["id"], id.as_str());
    }

    /// No file and nowhere to write one is an error, not an identity that would change
    /// on every start.
    #[test]
    fn a_missing_file_in_an_unwritable_dir_is_an_error() {
        if unsafe { libc::geteuid() } == 0 {
            return;
        }
        // The state dir exists but is read-only and `broker/` is not there yet, so
        // nothing can be created. (Making `broker/` itself 0500 would not do: the
        // owner may chmod it back, and the create tightens it to 0700 as the credential
        // store does.)
        let (_d, path) = state();
        let state_dir = path.parent().unwrap().parent().unwrap();
        fs::create_dir_all(state_dir).unwrap();
        fs::set_permissions(state_dir, fs::Permissions::from_mode(0o500)).unwrap();
        let got = load_or_create_with(&path, &host("h"), &|| {});
        fs::set_permissions(state_dir, fs::Permissions::from_mode(0o700)).unwrap();
        assert!(got.is_err(), "{got:?}");
        assert!(!path.exists());
    }

    /// The forced interleaving: this caller's lock-free read finds nothing, and before it
    /// takes the lock another process creates the file. The caller must read that file
    /// under the lock and return ITS id, not write a second one over it.
    #[test]
    fn a_create_that_lands_between_read_and_lock_wins() {
        let (_d, path) = state();
        let dir = path.parent().unwrap().to_path_buf();
        fs::create_dir_all(&dir).unwrap();
        let other = MachineIdentity {
            id: "fedcba9876543210fedcba9876543210".into(),
            name: "other-process".into(),
        };

        // Hold the lock as "the other process" before the caller starts.
        let held = FileLock::acquire(&dir.join(LOCK_FILE_NAME)).unwrap();
        let (reached_tx, reached_rx) = mpsc::channel();
        let caller = {
            let path = path.clone();
            std::thread::spawn(move || {
                load_or_create_with(&path, &host("mine"), &|| {
                    reached_tx.send(()).unwrap();
                })
            })
        };
        // The caller has done its lock-free read (it found nothing) and is about to block
        // on the lock we hold. Now "the other process" creates the file and lets go.
        reached_rx
            .recv_timeout(WAIT)
            .expect("the caller never reached the lock");
        write(&path, &other, "2026-01-01T00:00:00Z", &sync_dir).unwrap();
        drop(held);

        let got = join_within(caller, "the caller").unwrap();
        assert_eq!(got.identity, other);
        assert_eq!(got.outcome, LoadOutcome::Existing);
        assert_eq!(on_disk(&path)["id"], other.id.as_str());
    }

    /// The same interleaving for a corrupt file: if another process already replaced it,
    /// the caller must not replace it a second time (that would give the two processes two
    /// different ids).
    #[test]
    fn a_repair_that_lands_between_read_and_lock_wins() {
        let (_d, path) = state_with(b"garbage");
        let dir = path.parent().unwrap().to_path_buf();
        let other = MachineIdentity {
            id: "00000000000000000000000000000001".into(),
            name: "repaired-elsewhere".into(),
        };
        let held = FileLock::acquire(&dir.join(LOCK_FILE_NAME)).unwrap();
        let (reached_tx, reached_rx) = mpsc::channel();
        let caller = {
            let path = path.clone();
            std::thread::spawn(move || {
                load_or_create_with(&path, &host("mine"), &|| {
                    reached_tx.send(()).unwrap();
                })
            })
        };
        reached_rx
            .recv_timeout(WAIT)
            .expect("the caller never reached the lock");
        write(&path, &other, "2026-01-01T00:00:00Z", &sync_dir).unwrap();
        drop(held);

        let got = join_within(caller, "the caller").unwrap();
        assert_eq!(got.identity, other);
        assert_eq!(got.outcome, LoadOutcome::Existing);
        assert!(!corrupt_path(&path).exists());
    }

    /// Many concurrent first starts on one path end with one id.
    #[test]
    fn concurrent_creates_agree_on_one_id() {
        let (_d, path) = state();
        let n = 8;
        let barrier = Arc::new(Barrier::new(n));
        // Every thread is spawned before any is joined (a lazy iterator would join each
        // one before spawning the next, and the barrier would never open).
        let mut handles = Vec::with_capacity(n);
        for _ in 0..n {
            let path = path.clone();
            let barrier = barrier.clone();
            handles.push(std::thread::spawn(move || {
                barrier.wait();
                load_or_create_at(&path).unwrap()
            }));
        }
        let results: Vec<LoadedIdentity> = handles
            .into_iter()
            .map(|h| join_within(h, "a concurrent create"))
            .collect();
        let first = &results[0].identity;
        assert!(results.iter().all(|r| &r.identity == first), "{results:?}");
        let created = results
            .iter()
            .filter(|r| r.outcome == LoadOutcome::Created)
            .count();
        assert_eq!(created, 1, "{results:?}");
        assert_eq!(on_disk(&path)["id"], first.id.as_str());
    }

    #[test]
    fn hostname_to_name() {
        let cases: &[(&str, Option<&str>)] = &[
            ("laptop", Some("laptop")),
            ("Charlies-MacBook-Pro.local", Some("Charlies-MacBook-Pro")),
            ("build-07.example.org", Some("build-07")),
            ("  spaced  ", Some("spaced")),
            ("ctl\u{7}\u{1b}name", Some("ctlname")),
            ("192.168.1.5", Some("192.168.1.5")),
            ("::1", Some("::1")),
            (".leading-dot", Some(".leading-dot")),
            ("", None),
            ("   ", None),
            ("\u{0}\u{1}", None),
            ("überhost.lan", Some("überhost")),
        ];
        for (input, want) in cases {
            assert_eq!(
                name_from_hostname(input).as_deref(),
                *want,
                "hostname {input:?}"
            );
        }
    }

    #[test]
    fn long_hostnames_truncate_at_a_character_boundary() {
        let ascii = "a".repeat(80);
        assert_eq!(name_from_hostname(&ascii).unwrap(), "a".repeat(63));
        // 62 ASCII bytes then a 2-byte character: the character does not fit and is not
        // split.
        let mixed = format!("{}é{}", "a".repeat(62), "b".repeat(5));
        let got = sanitize_name(&mixed).unwrap();
        assert_eq!(got, "a".repeat(62));
        assert!(valid_name(&got));
        // 4-byte characters: 15 fit in 60 bytes, the 16th would make 64.
        let emoji = "🖥".repeat(20);
        let got = sanitize_name(&emoji).unwrap();
        assert_eq!(got.len(), 60);
        assert!(valid_name(&got));
    }

    #[test]
    fn fallback_name_when_the_hostname_gives_nothing() {
        let (_d, path) = state();
        let got = load_or_create_with(&path, &no_host, &|| {}).unwrap();
        assert_eq!(
            got.identity.name,
            format!("machine-{}", &got.identity.id[..8])
        );
        let (_d2, path2) = state();
        let got = load_or_create_with(&path2, &host("\u{1}"), &|| {}).unwrap();
        assert!(got.identity.name.starts_with("machine-"));
    }

    #[test]
    fn name_and_id_validators() {
        assert!(valid_name("a"));
        assert!(valid_name(&"x".repeat(63)));
        assert!(!valid_name(&"x".repeat(64)));
        assert!(!valid_name(""));
        assert!(!valid_name("new\nline"));
        assert!(!valid_name("del\u{7f}"));
        assert!(valid_name("with space"));
        assert!(valid_id(ID));
        assert!(!valid_id("0123456789ABCDEF0123456789abcdef"));
        assert!(!valid_id("0123456789abcdef0123456789abcde\n"));
    }

    #[test]
    fn the_production_path_is_under_the_state_dir() {
        let _guard = crate::env_lock();
        std::env::set_var("SHED_BROKER_STATE_DIR", "/tmp/shed-state-test");
        let p = machine_state_path();
        std::env::remove_var("SHED_BROKER_STATE_DIR");
        // Beside the credential store (`broker/creds/`).
        assert_eq!(p, Path::new("/tmp/shed-state-test/broker/machine.json"));
    }

    /// The real `gethostname` path: it returns a hostname, and that hostname yields a
    /// valid name. (Fails, rather than passing vacuously, if the lookup returns nothing.)
    #[test]
    fn the_system_hostname_yields_a_valid_name() {
        let h = system_hostname().expect("gethostname returned nothing");
        assert!(!h.trim().is_empty(), "gethostname returned an empty name");
        let n = name_from_hostname(&h).unwrap_or_else(|| panic!("no name from {h:?}"));
        assert!(valid_name(&n), "{n:?} from {h:?}");
    }
}
