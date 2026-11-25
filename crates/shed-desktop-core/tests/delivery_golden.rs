//! The desktop source's answer over a hermetic home on disk, through the real
//! `DiskReader`: a key and its `.pub`, a fake `gh`, craze's key file, an env file and an
//! envsecrets config. Held byte for byte against `fixtures/delivery-golden-v0.11.0.json`,
//! which v0.11.0 wrote (plan 040 §3.4, criterion 6); only the answer's version number
//! is normalised. `SHED_GOLDEN_WRITE=1` writes the fixture instead of comparing.

use std::os::unix::fs::PermissionsExt as _;
use std::path::Path;

use base64::Engine as _;
use serde_json::{json, Value};
use shed_desktop_core::delivery::providers::Settings;
use shed_desktop_core::delivery::provision::{parse_request, FingerprintKey, Request};
use shed_desktop_core::delivery::DiskReader;

const FIXTURE: &str = concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/fixtures/delivery-golden-v0.11.0.json"
);

fn put(b: &mut Vec<u8>, s: &[u8]) {
    b.extend((s.len() as u32).to_be_bytes());
    b.extend_from_slice(s);
}

/// A made-up OpenSSH key pair (not a key): the container a real one has.
fn key_pair() -> (String, String) {
    let b64 = |b: &[u8]| base64::engine::general_purpose::STANDARD.encode(b);
    let mut public = Vec::new();
    put(&mut public, b"ssh-ed25519");
    put(&mut public, &[0x5a; 32]);
    let mut blob = b"openssh-key-v1\0".to_vec();
    put(&mut blob, b"none");
    put(&mut blob, b"none");
    put(&mut blob, b"");
    blob.extend(1u32.to_be_bytes());
    put(&mut blob, &public);
    put(&mut blob, b"SENTINEL-private-part");
    let body = b64(&blob);
    let lines: Vec<&str> = body
        .as_bytes()
        .chunks(70)
        .map(|c| std::str::from_utf8(c).unwrap())
        .collect();
    (
        format!(
            "-----BEGIN OPENSSH PRIVATE KEY-----\n{}\n-----END OPENSSH PRIVATE KEY-----\n",
            lines.join("\n")
        ),
        format!("ssh-ed25519 {} ada@example.test\n", b64(&public)),
    )
}

fn write(home: &Path, rel: &str, content: &[u8], mode: u32) {
    let p = home.join(rel);
    std::fs::create_dir_all(p.parent().unwrap()).unwrap();
    std::fs::write(&p, content).unwrap();
    std::fs::set_permissions(&p, std::fs::Permissions::from_mode(mode)).unwrap();
}

/// The hermetic home, and the settings that use it.
fn home(home: &Path) -> String {
    let (private, public) = key_pair();
    write(home, ".ssh/id_ed25519", private.as_bytes(), 0o600);
    write(home, ".ssh/id_ed25519.pub", public.as_bytes(), 0o644);
    write(
        home,
        "bin/gh",
        b"#!/bin/sh\ncase \"$1 $2\" in\n  \"auth token\") echo gho_SENTINELtoken000 ;;\n  \"api user\") echo ada-example ;;\n  *) exit 2 ;;\nesac\n",
        0o755,
    );
    write(
        home,
        ".craze/native/providers.toml",
        b"[openai]\napi_key = \"SENTINEL-craze\"\n",
        0o600,
    );
    write(
        home,
        ".config/shed/keys.env",
        b"# a comment\nEXAMPLE_API_KEY=sk-SENTINEL \"q\" $D\r\nOTHER_KEY=two\n",
        0o600,
    );
    write(
        home,
        ".envsecrets/config.yaml",
        b"bucket: example\npassphrase_env: ENVSECRETS_PASSPHRASE\ngcs_credentials: eyJTRU5USU5FTCI6MX0=\n",
        0o600,
    );
    write(
        home,
        ".config/envsecrets/passphrase",
        b"SENTINEL-passphrase\n",
        0o600,
    );
    write(
        home,
        ".npmrc",
        b"//registry.example.test/:_authToken=SENTINEL-npm\n",
        0o600,
    );
    write(home, "work/widgets.env", b"DB=SENTINEL-db\n", 0o600);
    format!(
        r#"
[ssh-key]
enabled = true
key_file = "~/.ssh/id_ed25519"
sign_commits = true

[github]
enabled = true
gh = "{gh}"

[api-keys]
enabled = true
env_files = ["~/.config/shed/keys.env", "~/missing.env"]

[envsecrets-login]
enabled = true
passphrase_file = "~/.config/envsecrets/passphrase"

[files]
enabled = true
[[files.file]]
from = "~/.npmrc"
to = ".npmrc"
[[files.file]]
from = "~/work/widgets.env"
to = ".env"
base = "project"
mode = "0400"
only = {{ repos = ["example/widgets"] }}
"#,
        gh = home.join("bin/gh").display()
    )
}

fn request() -> Request {
    let body = format!(
        "{{\"version\":{},\"id\":\"0123456789abcdef0123456789abcdef\",\"reason\":\"create\",\"deadline\":\"2026-11-23T10:00:30Z\",\"user\":\"ada\",\"shed\":{{\"name\":\"widgets\",\"id\":\"q6c3\",\"backend\":\"vz\",\"repo\":{{\"url\":\"git@git.example.test:example/widgets.git\",\"name\":\"example/widgets\"}},\"project_dir\":\"/home/shed/widgets\"}}}}",
        shed_desktop_core::delivery::provision::limits::VERSION
    );
    parse_request(body.as_bytes()).unwrap()
}

#[test]
fn the_desktop_answer_is_v0_11_0s() {
    let dir = tempfile::tempdir().unwrap();
    let settings = Settings::parse(&home(dir.path())).unwrap();
    let reader = DiskReader::new(dir.path(), std::env::var_os("PATH"));
    let key = FingerprintKey::from_bytes([9; 32]);
    let built = shed_desktop_core::delivery::providers::build_answer(
        &settings,
        &request(),
        "work",
        1_795_428_000,
        None,
        &reader,
        &key,
    )
    .unwrap();
    let text = String::from_utf8(built.answer.to_json()).unwrap();
    let (_, rest) = text
        .strip_prefix("{\"version\":")
        .and_then(|r| r.split_once(','))
        .unwrap();
    let got = json!({
        "answer": format!("{{\"version\":V,{rest}"),
        "preview": built.preview,
    });
    if std::env::var_os("SHED_GOLDEN_WRITE").is_some() {
        std::fs::create_dir_all(Path::new(FIXTURE).parent().unwrap()).unwrap();
        std::fs::write(FIXTURE, serde_json::to_string_pretty(&got).unwrap() + "\n").unwrap();
        return;
    }
    let want: Value = serde_json::from_str(&std::fs::read_to_string(FIXTURE).unwrap()).unwrap();
    assert_eq!(got, want);
}
