//! `delivery.toml` on disk, for a shell: read it as a JSON view (`delivery.settings`)
//! and change one provider's table (`delivery.set`), writing the whole file back
//! atomically, mode `0600`.
//!
//! A provider's view is `{name, configured, enabled, only, config}`, where `config` is
//! the provider's table as flat JSON, exactly the shape `delivery.set` takes. None of
//! the settings is a secret value: they name files and commands, never their contents.
//!
//! A change is rendered as a TOML table and read by the same schema walk as the file
//! ([`Settings::parse`]), so every rule (unknown keys, types, paths, `only` lists) holds
//! for it, and its error says where in fixed words, never quoting the value.

use std::io::Write as _;
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::Path;

use serde_json::{json, Map, Value};
use shed_provision::{Base, Only};
use shed_provision_providers::settings::FileEntry;
use shed_provision_providers::Settings;

/// The five providers, in the one order they are asked.
pub const PROVIDERS: [&str; 5] = ["ssh-key", "github", "api-keys", "envsecrets-login", "files"];

/// What reading the file gave.
pub struct Loaded {
    pub exists: bool,
    /// The settings, or why they could not be used (fixed text, never the file's).
    pub settings: Result<Settings, String>,
}

/// The settings file at `path`. A missing file is no providers.
pub fn load(path: &Path) -> Loaded {
    match std::fs::read(path) {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Loaded {
            exists: false,
            settings: Ok(Settings::default()),
        },
        Err(e) => Loaded {
            exists: true,
            settings: Err(format!(
                "{} could not be read: {}",
                shed_provision_providers::SETTINGS_FILE_NAME,
                e.kind()
            )),
        },
        Ok(bytes) => Loaded {
            exists: true,
            settings: match String::from_utf8(bytes) {
                Ok(text) => Settings::parse(&text).map_err(|e| e.to_string()),
                Err(_) => Err(format!(
                    "{} is not UTF-8 text",
                    shed_provision_providers::SETTINGS_FILE_NAME
                )),
            },
        },
    }
}

fn only_json(o: Option<&Only>) -> Value {
    match o {
        None => Value::Null,
        Some(o) => {
            let mut m = Map::new();
            if let Some(s) = &o.servers {
                m.insert("servers".into(), json!(s));
            }
            if let Some(b) = &o.backends {
                m.insert(
                    "backends".into(),
                    json!(b.iter().map(|b| b.as_str()).collect::<Vec<_>>()),
                );
            }
            if let Some(r) = &o.repos {
                m.insert("repos".into(), json!(r));
            }
            Value::Object(m)
        }
    }
}

fn file_json(e: &FileEntry) -> Value {
    let mut m = Map::new();
    m.insert("from".into(), json!(e.from));
    m.insert("to".into(), json!(e.to));
    m.insert(
        "base".into(),
        json!(match e.base {
            Base::Home => "home",
            Base::Project => "project",
        }),
    );
    if let Some(mode) = &e.mode {
        m.insert("mode".into(), json!(mode));
    }
    if e.only.is_some() {
        m.insert("only".into(), only_json(e.only.as_ref()));
    }
    Value::Object(m)
}

/// One provider's table as flat JSON (the shape [`set`] takes), or `None` when the file
/// has no table for it.
fn provider_config(s: &Settings, name: &str) -> Option<(bool, Option<Only>, Map<String, Value>)> {
    let mut m = Map::new();
    let (enabled, only) = match name {
        "ssh-key" => {
            let p = s.ssh_key.as_ref()?;
            if let Some(k) = &p.key_file {
                m.insert("key_file".into(), json!(k));
            }
            m.insert("sign_commits".into(), json!(p.sign_commits));
            (p.enabled, p.only.clone())
        }
        "github" => {
            let p = s.github.as_ref()?;
            if let Some(g) = &p.gh {
                m.insert("gh".into(), json!(g));
            }
            (p.enabled, p.only.clone())
        }
        "api-keys" => {
            let p = s.api_keys.as_ref()?;
            m.insert("craze_providers".into(), json!(p.craze_providers));
            if !p.env_files.is_empty() {
                m.insert("env_files".into(), json!(p.env_files));
            }
            (p.enabled, p.only.clone())
        }
        "envsecrets-login" => {
            let p = s.envsecrets_login.as_ref()?;
            if let Some(f) = &p.passphrase_file {
                m.insert("passphrase_file".into(), json!(f));
            }
            if let Some(c) = &p.passphrase_command {
                m.insert("passphrase_command".into(), json!(c));
            }
            (p.enabled, p.only.clone())
        }
        "files" => {
            let p = s.files.as_ref()?;
            m.insert(
                "file".into(),
                Value::Array(p.files.iter().map(file_json).collect()),
            );
            (p.enabled, p.only.clone())
        }
        _ => return None,
    };
    Some((enabled, only, m))
}

/// The `delivery.settings` view of `loaded`, read from `path`.
pub fn view(path: &Path, loaded: &Loaded) -> Value {
    let (error, providers) = match &loaded.settings {
        Err(e) => (Value::from(e.clone()), Value::Array(Vec::new())),
        Ok(s) => (
            Value::Null,
            Value::Array(
                PROVIDERS
                    .iter()
                    .map(|name| match provider_config(s, name) {
                        None => json!({
                            "name": name, "configured": false, "enabled": false,
                            "only": null, "config": null,
                        }),
                        Some((enabled, only, mut config)) => {
                            config.insert("enabled".into(), json!(enabled));
                            if only.is_some() {
                                config.insert("only".into(), only_json(only.as_ref()));
                            }
                            json!({
                                "name": name, "configured": true, "enabled": enabled,
                                "only": only_json(only.as_ref()), "config": config,
                            })
                        }
                    })
                    .collect(),
            ),
        ),
    };
    json!({
        "path": path.to_string_lossy(),
        "exists": loaded.exists,
        "error": error,
        "providers": providers,
    })
}

/// Why a change was refused: a code and fixed words.
pub type SetError = (String, String);

fn bad(msg: impl Into<String>) -> SetError {
    ("bad_request".to_string(), msg.into())
}

fn key(k: &str) -> String {
    if !k.is_empty()
        && k.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
    {
        k.to_string()
    } else {
        toml_string(k)
    }
}

fn toml_string(s: &str) -> String {
    let mut out = String::from("\"");
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            c if (c as u32) < 0x20 || c == '\u{7f}' => {
                out.push_str(&format!("\\u{:04X}", c as u32));
            }
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// A JSON value as a TOML value; nulls are refused (a key is left out instead).
fn toml_value(v: &Value) -> Result<String, SetError> {
    Ok(match v {
        Value::Null => return Err(bad("a setting is null; leave the key out instead")),
        Value::Bool(b) => b.to_string(),
        Value::Number(n) => n.to_string(),
        Value::String(s) => toml_string(s),
        Value::Array(a) => format!(
            "[{}]",
            a.iter()
                .map(toml_value)
                .collect::<Result<Vec<_>, _>>()?
                .join(", ")
        ),
        Value::Object(o) => format!(
            "{{ {} }}",
            o.iter()
                .map(|(k, v)| Ok(format!("{} = {}", key(k), toml_value(v)?)))
                .collect::<Result<Vec<_>, SetError>>()?
                .join(", ")
        ),
    })
}

/// `provider`'s table from `config` (flat JSON) as TOML text.
fn render_table(provider: &str, config: &Map<String, Value>) -> Result<String, SetError> {
    let mut out = format!("[{provider}]\n");
    let mut files = None;
    for (k, v) in config {
        if provider == "files" && k == "file" {
            files = Some(v);
            continue;
        }
        out.push_str(&format!("{} = {}\n", key(k), toml_value(v)?));
    }
    if let Some(entries) = files {
        let Value::Array(entries) = entries else {
            return Err(bad("files.file is not a list"));
        };
        for e in entries {
            let Value::Object(e) = e else {
                return Err(bad("a files.file entry is not an object"));
            };
            out.push_str("[[files.file]]\n");
            for (k, v) in e {
                out.push_str(&format!("{} = {}\n", key(k), toml_value(v)?));
            }
        }
    }
    Ok(out)
}

/// Replaces `provider`'s table in the settings file at `path` with `config` (flat JSON,
/// as [`view`] shows it), or removes it when `config` is null, and writes the whole
/// file back. Refused, with the file unchanged, for an unknown provider, a change the
/// schema refuses, or a file that cannot be read as settings now.
pub fn set(path: &Path, provider: &str, config: &Value) -> Result<(), SetError> {
    if !PROVIDERS.contains(&provider) {
        return Err(bad(format!(
            "unknown provider; one of {}",
            PROVIDERS.join(", ")
        )));
    }
    // The whole read-modify-write is one transaction: in this process under one mutex,
    // and across processes under a lock file beside the settings (the app is single
    // instance, but a second writer must never interleave with it either).
    static SETTINGS_WRITE: std::sync::Mutex<()> = std::sync::Mutex::new(());
    let _in_process = SETTINGS_WRITE.lock().unwrap_or_else(|e| e.into_inner());
    let _across = FileLock::acquire(path).map_err(|e| ("write_failed".to_string(), e))?;
    let mut settings = match load(path).settings {
        Ok(s) => s,
        Err(e) => {
            return Err((
                "settings_unreadable".into(),
                format!("the settings file cannot be changed until it is fixed: {e}"),
            ))
        }
    };
    let new = match config {
        Value::Null => Settings::default(),
        Value::Object(o) => {
            let text = render_table(provider, o)?;
            Settings::parse(&text).map_err(|e| bad(format!("the new settings: {e}")))?
        }
        _ => return Err(bad("config is not an object or null")),
    };
    match provider {
        "ssh-key" => settings.ssh_key = new.ssh_key,
        "github" => settings.github = new.github,
        "api-keys" => settings.api_keys = new.api_keys,
        "envsecrets-login" => settings.envsecrets_login = new.envsecrets_login,
        _ => settings.files = new.files,
    }
    write(path, settings.to_toml().as_bytes()).map_err(|e| ("write_failed".to_string(), e))
}

/// An exclusive `flock` on `<settings>.lock` beside the settings file, held until drop.
struct FileLock(std::fs::File);

impl FileLock {
    fn acquire(settings: &Path) -> Result<FileLock, String> {
        let dir = settings
            .parent()
            .ok_or_else(|| "the settings path has no directory".to_string())?;
        std::fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
        let name = settings
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        let lock = dir.join(format!(".{name}.lock"));
        let f = std::fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .mode(0o600)
            .open(&lock)
            .map_err(|e| format!("open {}: {e}", lock.display()))?;
        use std::os::unix::io::AsRawFd as _;
        // SAFETY: `f` owns a live descriptor for the whole call.
        if unsafe { libc::flock(f.as_raw_fd(), libc::LOCK_EX) } != 0 {
            return Err(format!(
                "lock {}: {}",
                lock.display(),
                std::io::Error::last_os_error()
            ));
        }
        Ok(FileLock(f))
    }
}

impl Drop for FileLock {
    fn drop(&mut self) {
        use std::os::unix::io::AsRawFd as _;
        // SAFETY: the descriptor is owned by `self.0` until this returns.
        unsafe { libc::flock(self.0.as_raw_fd(), libc::LOCK_UN) };
    }
}

/// A temporary file of its own beside `path`, created exclusively with mode `0600`:
/// its name is the process, a counter and the clock, and an existing name is never
/// reused or removed.
fn unique_temp(path: &Path) -> Result<(std::path::PathBuf, std::fs::File), String> {
    use std::sync::atomic::{AtomicU64, Ordering};
    static NEXT: AtomicU64 = AtomicU64::new(0);
    let dir = path
        .parent()
        .ok_or_else(|| "the settings path has no directory".to_string())?;
    for _ in 0..16 {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.subsec_nanos())
            .unwrap_or(0);
        let tmp = dir.join(format!(
            ".{}.{}.{}.{nanos}.tmp",
            shed_provision_providers::SETTINGS_FILE_NAME,
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        match std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp)
        {
            Ok(f) => return Ok((tmp, f)),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(format!("create {}: {e}", tmp.display())),
        }
    }
    Err("no unique temporary file could be made".into())
}

/// Writes `data` to `path` whole: a temporary file of its own in the same directory,
/// `0600`, synced, renamed into place.
fn write(path: &Path, data: &[u8]) -> Result<(), String> {
    let dir = path
        .parent()
        .ok_or_else(|| "the settings path has no directory".to_string())?;
    std::fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
    let (tmp, file) = unique_temp(path)?;
    let mut f = file;
    let result = f
        .write_all(data)
        .and_then(|()| f.set_permissions(std::fs::Permissions::from_mode(0o600)))
        .and_then(|()| f.sync_all())
        .and_then(|()| std::fs::rename(&tmp, path));
    if let Err(e) = result {
        let _ = std::fs::remove_file(&tmp);
        return Err(format!("write {}: {e}", path.display()));
    }
    if let Ok(d) = std::fs::File::open(dir) {
        let _ = d.sync_all();
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn provider<'a>(v: &'a Value, name: &str) -> &'a Value {
        v["providers"]
            .as_array()
            .unwrap()
            .iter()
            .find(|p| p["name"] == name)
            .unwrap()
    }

    #[test]
    fn a_change_round_trips_and_keeps_the_other_providers() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("shed").join("delivery.toml");
        let v = view(&path, &load(&path));
        assert_eq!(v["exists"], false);
        assert_eq!(provider(&v, "github")["configured"], false);

        set(
            &path,
            "github",
            &json!({"enabled": true, "gh": "/opt/gh", "only": {"servers": ["work"], "backends": ["vz"]}}),
        )
        .unwrap();
        set(
            &path,
            "files",
            &json!({"enabled": true, "file": [{"from": "~/.npmrc", "to": ".npmrc"},
                {"from": "~/w.env", "to": ".env", "base": "project", "mode": "0400",
                 "only": {"repos": ["example/*"]}}]}),
        )
        .unwrap();
        assert_eq!(
            std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        let v = view(&path, &load(&path));
        let gh = provider(&v, "github");
        assert_eq!(gh["enabled"], true);
        assert_eq!(gh["only"], json!({"servers": ["work"], "backends": ["vz"]}));
        assert_eq!(gh["config"]["gh"], "/opt/gh");
        let files = provider(&v, "files");
        assert_eq!(files["config"]["file"][1]["base"], "project");
        assert_eq!(files["config"]["file"][1]["only"]["repos"][0], "example/*");

        // Setting a provider's own view back changes nothing.
        let before = std::fs::read_to_string(&path).unwrap();
        set(&path, "files", &files["config"]).unwrap();
        set(&path, "github", &gh["config"]).unwrap();
        assert_eq!(std::fs::read_to_string(&path).unwrap(), before);

        // Null removes the table.
        set(&path, "github", &Value::Null).unwrap();
        let v = view(&path, &load(&path));
        assert_eq!(provider(&v, "github")["configured"], false);
        assert_eq!(provider(&v, "files")["configured"], true);
    }

    #[test]
    fn a_change_the_schema_refuses_leaves_the_file_alone_and_quotes_nothing() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("delivery.toml");
        set(
            &path,
            "ssh-key",
            &json!({"enabled": true, "key_file": "~/.ssh/id"}),
        )
        .unwrap();
        let before = std::fs::read_to_string(&path).unwrap();
        for (provider, config) in [
            (
                "ssh-key",
                json!({"enabled": true, "key_file": "relative/SENTINEL"}),
            ),
            ("ssh-key", json!({"enabled": "SENTINEL"})),
            ("ssh-key", json!({"enabled": true, "SENTINEL": 1})),
            ("github", json!({"gh": "gh"})),
            (
                "github",
                json!({"enabled": true, "only": {"backends": ["SENTINEL"]}}),
            ),
            ("github", json!({"enabled": true, "gh": null})),
            ("files", json!({"enabled": true, "file": "SENTINEL"})),
            ("nope", json!({"enabled": true})),
            ("github", json!(["SENTINEL"])),
        ] {
            let (code, msg) = set(&path, provider, &config).unwrap_err();
            assert_eq!(code, "bad_request", "{provider} {config}: {msg}");
            assert!(!msg.contains("SENTINEL"), "{msg}");
        }
        assert_eq!(std::fs::read_to_string(&path).unwrap(), before);
    }

    /// Changes made at once to different providers all land: each read-modify-write is
    /// whole, and no writer's temporary file is another's.
    #[test]
    fn concurrent_changes_are_not_lost() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("delivery.toml");
        let configs = [
            ("ssh-key", json!({"enabled": true, "key_file": "~/.ssh/id"})),
            ("github", json!({"enabled": true})),
            ("api-keys", json!({"enabled": false})),
            (
                "envsecrets-login",
                json!({"enabled": true, "passphrase_file": "~/p"}),
            ),
            ("files", json!({"enabled": true})),
        ];
        for _ in 0..5 {
            std::thread::scope(|scope| {
                for (provider, config) in &configs {
                    let path = &path;
                    scope.spawn(move || set(path, provider, config).unwrap());
                }
            });
            let v = view(&path, &load(&path));
            for (name, _) in &configs {
                assert_eq!(provider(&v, name)["configured"], true, "{name}");
            }
            std::fs::remove_file(&path).unwrap();
        }
        let left: Vec<_> = std::fs::read_dir(dir.path())
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .filter(|n| n.ends_with(".tmp"))
            .collect();
        assert!(left.is_empty(), "{left:?}");
    }

    #[test]
    fn a_broken_file_is_reported_and_not_overwritten() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("delivery.toml");
        std::fs::write(&path, "[github\nSENTINEL").unwrap();
        let v = view(&path, &load(&path));
        assert!(v["error"].as_str().unwrap().contains("not valid TOML"));
        assert!(!v.to_string().contains("SENTINEL"));
        let (code, _) = set(&path, "github", &json!({"enabled": true})).unwrap_err();
        assert_eq!(code, "settings_unreadable");
        assert_eq!(std::fs::read_to_string(&path).unwrap(), "[github\nSENTINEL");
    }
}
