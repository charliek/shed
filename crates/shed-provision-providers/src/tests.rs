//! The providers end to end, through the builder, with a hermetic home: a
//! key pair, a fake `gh`, craze's key file, an env file and an envsecrets
//! config. Every value is a sentinel, so a test can check none reaches a
//! problem message or the preview.

use base64::Engine as _;
use shed_provision::testing::FakeReader;
use shed_provision::{
    parse_request, Built, FingerprintKey, ItemBody, Mode, ProblemCode, ReadError, Request,
    RunError, Stage, GIT_CONFIG_NAME,
};

use crate::{build_answer, preview, Settings};

const HOME: &str = "/home/ada";
const TOKEN: &str = "gho_SENTINELtoken000";
const API_VALUE: &str = "sk-SENTINEL \"quoted\" $DOLLAR inner";
const PASSPHRASE: &str = "SENTINEL-passphrase";
const GCS: &str = "eyJTRU5USU5FTCI6MX0=";

fn b64(b: &[u8]) -> String {
    base64::engine::general_purpose::STANDARD.encode(b)
}

fn put(b: &mut Vec<u8>, s: &[u8]) {
    b.extend((s.len() as u32).to_be_bytes());
    b.extend_from_slice(s);
}

/// A made-up OpenSSH key pair (not a key): the container a real one has,
/// with `cipher` and a public blob of type ssh-ed25519.
fn key_pair(cipher: &str) -> (String, String) {
    let mut public = Vec::new();
    put(&mut public, b"ssh-ed25519");
    put(&mut public, &[0x5a; 32]);
    let mut blob = b"openssh-key-v1\0".to_vec();
    put(&mut blob, cipher.as_bytes());
    put(
        &mut blob,
        if cipher == "none" { b"none" } else { b"bcrypt" },
    );
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

fn home() -> FakeReader {
    let (private, public) = key_pair("none");
    FakeReader::new(HOME)
        .file("~/.ssh/id_ed25519", private, 0o600)
        .file("~/.ssh/id_ed25519.pub", public, 0o644)
        .command(&["gh", "auth", "token", "--hostname", "github.com"], 0, &format!("{TOKEN}\n"))
        .command(&["gh", "api", "user", "--hostname", "github.com", "--jq", ".login"], 0, "ada-example\n")
        .file("~/.craze/native/providers.toml", "[openai]\napi_key = \"SENTINEL-craze\"\n", 0o600)
        .file(
            "~/.config/shed/keys.env",
            format!("# a comment\n\nEXAMPLE_API_KEY={API_VALUE}\r\nOTHER_KEY=two\n"),
            0o600,
        )
        .file(
            "~/.envsecrets/config.yaml",
            format!("bucket: example\npassphrase_env: ENVSECRETS_PASSPHRASE  # or use passphrase_command\ngcs_credentials: {GCS}\n"),
            0o600,
        )
        .file("~/.config/envsecrets/passphrase", format!("{PASSPHRASE}\n"), 0o600)
        .file("~/.npmrc", "//registry.example.test/:_authToken=SENTINEL-npm\n", 0o600)
        .file("~/work/widgets.env", "DB=SENTINEL-db\n", 0o600)
}

const SETTINGS: &str = r#"
[ssh-key]
enabled = true
key_file = "~/.ssh/id_ed25519"
sign_commits = true

[github]
enabled = true

[api-keys]
enabled = true
env_files = ["~/.config/shed/keys.env"]

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
only = { repos = ["example/widgets"] }
"#;

fn request(extra: &str) -> Request {
    let body = format!(
        "{{\"version\":1,\"id\":\"0123456789abcdef0123456789abcdef\",\"reason\":\"create\",\"deadline\":\"2026-11-23T10:00:30Z\",\"user\":\"ada\",\"shed\":{{\"name\":\"widgets\",\"id\":\"q6c3\",\"backend\":\"remote\"{extra}}}}}"
    );
    parse_request(body.as_bytes()).unwrap()
}

fn widgets() -> Request {
    request(",\"repo\":{\"url\":\"git@git.example.test:example/widgets.git\",\"name\":\"example/widgets\"},\"project_dir\":\"/home/shed/widgets\"")
}

fn key() -> FingerprintKey {
    FingerprintKey::from_bytes([3; 32])
}

fn build_with(settings: &str, reader: &FakeReader, req: &Request) -> Built {
    let s = Settings::parse(settings).unwrap_or_else(|e| panic!("{e}"));
    build_answer(&s, req, "work", reader, &key()).unwrap()
}

/// (name, target, stage, mode) of each item.
fn shape(b: &Built) -> Vec<(String, String, u8, Option<Mode>)> {
    b.answer
        .items
        .iter()
        .map(|i| {
            let mode = match &i.body {
                ItemBody::File(f) => Some(f.mode),
                ItemBody::Env(_) => None,
            };
            (
                i.name.clone(),
                i.target().to_string(),
                i.stage().number(),
                mode,
            )
        })
        .collect()
}

fn problems(b: &Built) -> Vec<(String, ProblemCode)> {
    b.answer
        .problems
        .iter()
        .map(|p| (p.provider.clone(), p.code))
        .collect()
}

fn no_sentinel_outside_items(b: &Built) {
    let preview = serde_json::to_string(&b.preview).unwrap();
    let messages: String = b
        .answer
        .problems
        .iter()
        .map(|p| p.message.clone())
        .collect();
    for text in [preview, messages] {
        assert!(!text.contains("SENTINEL"), "a value escaped: {text}");
        assert!(!text.contains(GCS) && !text.contains(TOKEN));
    }
}

#[test]
fn every_provider_answers_a_request() {
    let r = home();
    let b = build_with(SETTINGS, &r, &widgets());
    let s = |n: &str, t: &str, st: u8, m: Option<Mode>| (n.to_string(), t.to_string(), st, m);
    assert_eq!(
        shape(&b),
        vec![
            s("ssh-key", ".ssh/id_ed25519", 1, Some(Mode::M0600)),
            s("ssh-key.pub", ".ssh/id_ed25519.pub", 1, Some(Mode::M0644)),
            s("github", ".config/gh/hosts.yml", 1, Some(Mode::M0600)),
            s(
                "api-keys.craze",
                ".craze/native/providers.toml",
                1,
                Some(Mode::M0600)
            ),
            s("api-keys.example_api_key", "EXAMPLE_API_KEY", 1, None),
            s("api-keys.other_key", "OTHER_KEY", 1, None),
            s(
                "envsecrets-login.config",
                ".envsecrets/config.yaml",
                1,
                Some(Mode::M0600)
            ),
            s(
                "envsecrets-login.passphrase",
                "ENVSECRETS_PASSPHRASE",
                1,
                None
            ),
            s("files..npmrc", ".npmrc", 1, Some(Mode::M0600)),
            s("files..env", ".env", 2, Some(Mode::M0600)),
            s(GIT_CONFIG_NAME, ".config/git/config", 1, Some(Mode::M0600)),
        ]
    );
    assert!(b.answer.problems.is_empty(), "{:?}", b.answer.problems);
    let item = |name: &str| b.answer.items.iter().find(|i| i.name == name).unwrap();
    // A value with quotes, a dollar sign and inner spaces is kept byte for
    // byte (the env file's CRLF is a line end, not part of it).
    let ItemBody::Env(e) = &item("api-keys.example_api_key").body else {
        panic!()
    };
    assert_eq!(e.value, API_VALUE);
    let ItemBody::Env(e) = &item("envsecrets-login.passphrase").body else {
        panic!()
    };
    assert_eq!(e.value, PASSPHRASE);
    let ItemBody::File(f) = &item("envsecrets-login.config").body else {
        panic!()
    };
    assert!(String::from_utf8_lossy(&f.content).starts_with("bucket: example\npassphrase_env:"));
    let ItemBody::File(f) = &item("github").body else {
        panic!()
    };
    let hosts = String::from_utf8(f.content.clone()).unwrap();
    assert!(
        hosts.contains("user: ada-example") && hosts.contains(&format!("oauth_token: {TOKEN}")),
        "{hosts}"
    );
    let ItemBody::File(f) = &item(GIT_CONFIG_NAME).body else {
        panic!()
    };
    let git = String::from_utf8(f.content.clone()).unwrap();
    for want in [
        "[gpg]\n\tformat = ssh\n",
        "[user]\n\tsigningkey = /home/shed/.ssh/id_ed25519\n",
        "[commit]\n\tgpgsign = true\n",
        "[credential \"https://github.com\"]\n\thelper =\n\thelper = !gh auth git-credential\n",
    ] {
        assert!(git.contains(want), "{git}");
    }
    assert_eq!(item("files..env").stage(), Stage::Two);
    // The public half is the one line, re-written.
    let ItemBody::File(f) = &item("ssh-key.pub").body else {
        panic!()
    };
    assert_eq!(f.content, key_pair("none").1.as_bytes());
    no_sentinel_outside_items(&b);
    // Commands run by argv, under a bound.
    for c in r.ran() {
        assert_eq!(c.program, "gh");
        assert!(c.max_output > 0 && !c.timeout.is_zero());
    }
}

#[test]
fn only_rules_per_provider_and_per_entry() {
    let r = home();
    // A repo the entry's only rule does not name: the entry stays home.
    let other = request(",\"repo\":{\"url\":\"git@git.example.test:example/gadgets.git\",\"name\":\"example/gadgets\"},\"project_dir\":\"/home/shed/gadgets\"");
    let b = build_with(SETTINGS, &r, &other);
    assert!(b.answer.items.iter().all(|i| i.name != "files..env"));
    // A provider limited to another server does not run.
    let limited = format!("{SETTINGS}\n[github.only]\nservers = [\"home\"]\n");
    let b = build_with(&limited, &r, &widgets());
    assert!(b.answer.items.iter().all(|i| i.name != "github"));
    let gh = b
        .preview
        .providers
        .iter()
        .find(|p| p.provider == "github")
        .unwrap();
    assert!(!gh.ran && gh.restricted);
    // By backend.
    let limited = format!("{SETTINGS}\n[ssh-key.only]\nbackends = [\"vz\"]\n");
    let b = build_with(&limited, &r, &widgets());
    assert!(b
        .answer
        .items
        .iter()
        .all(|i| !i.name.starts_with("ssh-key")));
    let git = b
        .answer
        .items
        .iter()
        .find(|i| i.name == GIT_CONFIG_NAME)
        .unwrap();
    let ItemBody::File(f) = &git.body else {
        panic!()
    };
    assert!(!String::from_utf8_lossy(&f.content).contains("gpgsign"));
}

#[test]
fn ssh_key_problems() {
    let one = "[ssh-key]\nenabled = true\nkey_file = \"~/.ssh/id_ed25519\"\n";
    let (locked, locked_pub) = key_pair("aes256-ctr");
    let r = FakeReader::new(HOME)
        .file("~/.ssh/id_ed25519", locked, 0o600)
        .file("~/.ssh/id_ed25519.pub", locked_pub, 0o644);
    let b = build_with(one, &r, &widgets());
    assert_eq!(
        problems(&b),
        vec![("ssh-key".into(), ProblemCode::PassphraseProtected)]
    );
    assert!(b.answer.items.is_empty());
    let (private, _) = key_pair("none");
    let r = FakeReader::new(HOME).file("~/.ssh/id_ed25519", private.clone(), 0o600);
    assert_eq!(
        problems(&build_with(one, &r, &widgets())),
        vec![("ssh-key".into(), ProblemCode::MissingInput)]
    );
    let r = FakeReader::new(HOME)
        .file("~/.ssh/id_ed25519", private, 0o600)
        .file(
            "~/.ssh/id_ed25519.pub",
            "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOther other\n",
            0o644,
        );
    assert_eq!(
        problems(&build_with(one, &r, &widgets())),
        vec![("ssh-key".into(), ProblemCode::Unreadable)]
    );
    let r = FakeReader::new(HOME);
    assert_eq!(
        problems(&build_with(one, &r, &widgets())),
        vec![("ssh-key".into(), ProblemCode::MissingInput)]
    );
    let unset = "[ssh-key]\nenabled = true\n";
    assert_eq!(
        problems(&build_with(unset, &r, &widgets())),
        vec![("ssh-key".into(), ProblemCode::MissingInput)]
    );
    let r = FakeReader::new(HOME).file_error("~/.ssh/id_ed25519", ReadError::Denied);
    assert_eq!(
        problems(&build_with(one, &r, &widgets())),
        vec![("ssh-key".into(), ProblemCode::Unreadable)]
    );
}

/// A public half with anything beside its one line is not delivered: the
/// world-readable item can carry nothing else.
#[test]
fn ssh_key_public_half_carries_nothing_else() {
    let one = "[ssh-key]\nenabled = true\nkey_file = \"~/.ssh/id_ed25519\"\n";
    let (private, public) = key_pair("none");
    let r = FakeReader::new(HOME)
        .file("~/.ssh/id_ed25519", private.clone(), 0o600)
        .file("~/.ssh/id_ed25519.pub", format!("{public}{private}"), 0o644);
    let b = build_with(one, &r, &widgets());
    assert!(b.answer.items.is_empty());
    assert_eq!(
        problems(&b),
        vec![("ssh-key".into(), ProblemCode::Unreadable)]
    );
    // Through a link, as a dotfile manager makes one, the key is delivered.
    let r = FakeReader::new(HOME)
        .file("~/dotfiles/ssh/id_ed25519", private, 0o600)
        .file("~/dotfiles/ssh/id_ed25519.pub", public, 0o644)
        .link("~/.ssh/id_ed25519", "~/dotfiles/ssh/id_ed25519")
        .link("~/.ssh/id_ed25519.pub", "~/dotfiles/ssh/id_ed25519.pub");
    assert_eq!(build_with(one, &r, &widgets()).answer.items.len(), 2);
    // A FIFO where the key should be is not a file.
    let r = FakeReader::new(HOME).special("~/.ssh/id_ed25519");
    assert_eq!(
        problems(&build_with(one, &r, &widgets())),
        vec![("ssh-key".into(), ProblemCode::Unreadable)]
    );
}

/// A 256 KiB env file of `A=1` lines: 65,536 drafts of one variable. They
/// all conflict, none is delivered, and it returns at once.
#[test]
fn a_large_env_file_of_one_variable() {
    let s = "[api-keys]\nenabled = true\ncraze_providers = false\nenv_files = [\"~/a.env\"]\n";
    let text = "A=1\n".repeat(65_536);
    assert_eq!(text.len(), 256 << 10);
    let r = FakeReader::new(HOME).file("~/a.env", text, 0o600);
    let start = std::time::Instant::now();
    let b = build_with(s, &r, &widgets());
    assert!(b.answer.items.is_empty());
    assert_eq!(
        problems(&b),
        vec![("api-keys".into(), ProblemCode::Conflict)]
    );
    assert!(
        start.elapsed() < std::time::Duration::from_secs(10),
        "{:?}",
        start.elapsed()
    );
}

#[test]
fn github_problems_carry_no_output() {
    let gh = "[github]\nenabled = true\ngh = \"/opt/example/gh\"\n";
    let token = [
        "/opt/example/gh",
        "auth",
        "token",
        "--hostname",
        "github.com",
    ];
    let cases: Vec<(FakeReader, ProblemCode)> = vec![
        (FakeReader::new(HOME), ProblemCode::MissingInput),
        (
            FakeReader::new(HOME).command(&token, 1, "SENTINEL no oauth token"),
            ProblemCode::SignedOut,
        ),
        (
            FakeReader::new(HOME).command_error(&token, RunError::TimedOut),
            ProblemCode::CommandFailed,
        ),
        (
            FakeReader::new(HOME).command(&token, 0, "SENTINEL not a token!\n"),
            ProblemCode::CommandFailed,
        ),
        (
            FakeReader::new(HOME)
                .command(&token, 0, "gho_SENTINEL\n")
                .command(
                    &[
                        "/opt/example/gh",
                        "api",
                        "user",
                        "--hostname",
                        "github.com",
                        "--jq",
                        ".login",
                    ],
                    1,
                    "SENTINEL",
                ),
            ProblemCode::CommandFailed,
        ),
    ];
    for (r, code) in cases {
        let b = build_with(gh, &r, &widgets());
        assert_eq!(problems(&b), vec![("github".into(), code)]);
        assert!(b.answer.items.is_empty());
        no_sentinel_outside_items(&b);
    }
}

#[test]
fn api_keys_env_files() {
    let s = "[api-keys]\nenabled = true\ncraze_providers = false\nenv_files = [\"~/a.env\", \"~/b.env\", \"/etc/c.env\"]\n";
    let r = FakeReader::new(HOME)
        .file(
            "~/a.env",
            "GOOD=1\nPATH=/SENTINEL\nlower=SENTINEL\nSPACE=SENTINEL \nnoequals\nDUP=SENTINEL-a\nTAB=SENTINEL\tx\n",
            0o600,
        )
        .file("~/b.env", "DUP=SENTINEL-b\n", 0o400)
        .file("/etc/c.env", "SHARED=SENTINEL\n", 0o644);
    let b = build_with(s, &r, &widgets());
    let names: Vec<&str> = b.answer.items.iter().map(|i| i.name.as_str()).collect();
    assert_eq!(names, vec!["api-keys.good"]);
    let messages: Vec<&str> = b
        .answer
        .problems
        .iter()
        .map(|p| p.message.as_str())
        .collect();
    assert_eq!(
        messages,
        vec![
            "api-keys.env_files[0] line 2: the variable is one a shed may not have set; it was not delivered",
            "api-keys.env_files[0] line 3: the name is not upper-case letters, digits and underscores; it was not delivered",
            "api-keys.env_files[0] line 4: the value starts or ends with a space; it was not delivered",
            "api-keys.env_files[0]: 2 more lines break the env rules and were not delivered",
            "api-keys.env_files[2] can be read by others; set its mode to 0600",
            "two items write the same target, or one inside the other; neither was delivered",
        ]
    );
    no_sentinel_outside_items(&b);
    // craze's file is never read when it is off, and only it is read when on.
    let s = "[api-keys]\nenabled = true\n";
    let b = build_with(s, &FakeReader::new(HOME), &widgets());
    assert_eq!(
        problems(&b),
        vec![("api-keys".into(), ProblemCode::MissingInput)]
    );
}

#[test]
fn envsecrets_login() {
    let by_command = "[envsecrets-login]\nenabled = true\npassphrase_command = [\"example-pass\", \"show\", \"envsecrets\"]\n";
    let config = format!("passphrase_env: ENVSECRETS_PASSPHRASE\ngcs_credentials: {GCS}\n");
    let r = FakeReader::new(HOME)
        .file("~/.envsecrets/config.yaml", config.clone(), 0o600)
        .command(
            &["example-pass", "show", "envsecrets"],
            0,
            "SENTINEL-from-command\n",
        );
    let b = build_with(by_command, &r, &widgets());
    assert_eq!(b.answer.items.len(), 2);
    let ItemBody::Env(e) = &b.answer.items[1].body else {
        panic!()
    };
    assert_eq!(
        (e.variable.as_str(), e.value.as_str()),
        ("ENVSECRETS_PASSPHRASE", "SENTINEL-from-command")
    );
    let ItemBody::File(f) = &b.answer.items[0].body else {
        panic!()
    };
    assert_eq!(f.content, config.as_bytes());

    let failing = FakeReader::new(HOME)
        .file("~/.envsecrets/config.yaml", config.clone(), 0o600)
        .command(
            &["example-pass", "show", "envsecrets"],
            2,
            "SENTINEL-partial\n",
        );
    let b = build_with(by_command, &failing, &widgets());
    assert_eq!(
        problems(&b),
        vec![("envsecrets-login".into(), ProblemCode::CommandFailed)]
    );
    no_sentinel_outside_items(&b);

    for (cfg, code) in [
        (
            format!("gcs_credentials: {GCS}\n"),
            ProblemCode::MissingInput,
        ),
        (
            "passphrase_env: ENVSECRETS_PASSPHRASE\n".to_string(),
            ProblemCode::MissingInput,
        ),
        (
            format!("passphrase_env: PATH\ngcs_credentials: {GCS}\n"),
            ProblemCode::Unreadable,
        ),
        (
            format!("passphrase_env: |\n  X\ngcs_credentials: {GCS}\n"),
            ProblemCode::Unreadable,
        ),
        (
            "passphrase_env: X\ngcs_credentials:\n  type: service_account\n".to_string(),
            ProblemCode::Unreadable,
        ),
    ] {
        let r = FakeReader::new(HOME)
            .file("~/.envsecrets/config.yaml", cfg.clone(), 0o600)
            .command(&["example-pass", "show", "envsecrets"], 0, "p\n");
        let b = build_with(by_command, &r, &widgets());
        assert_eq!(
            problems(&b),
            vec![("envsecrets-login".into(), code)],
            "{cfg:?}"
        );
        assert!(b.answer.items.is_empty());
    }
    let neither = "[envsecrets-login]\nenabled = true\n";
    let r = FakeReader::new(HOME).file("~/.envsecrets/config.yaml", config, 0o600);
    assert_eq!(
        problems(&build_with(neither, &r, &widgets())),
        vec![("envsecrets-login".into(), ProblemCode::MissingInput)]
    );
}

#[test]
fn files_conflicts_and_stage_2() {
    let s = "[ssh-key]\nenabled = true\nkey_file = \"~/.ssh/id_ed25519\"\nsign_commits = true\n\
             [github]\nenabled = true\n\
             [files]\nenabled = true\n\
             [[files.file]]\nfrom = \"~/.npmrc\"\nto = \".config/git/config\"\n\
             [[files.file]]\nfrom = \"~/.npmrc\"\nto = \".config/gh\"\n\
             [[files.file]]\nfrom = \"~/work/widgets.env\"\nto = \".env\"\nbase = \"project\"\nmode = \"0400\"\n";
    let b = build_with(s, &home(), &widgets());
    let names: Vec<&str> = b.answer.items.iter().map(|i| i.name.as_str()).collect();
    // The git config and the file on it are both left out, and so are
    // github's hosts.yml and the file that would be its directory.
    assert_eq!(names, vec!["ssh-key", "ssh-key.pub", "files..env"]);
    let mut who: Vec<(String, ProblemCode)> = problems(&b);
    who.sort_by(|a, b| a.0.cmp(&b.0));
    assert_eq!(
        who,
        vec![
            ("files".into(), ProblemCode::Conflict),
            ("github".into(), ProblemCode::Conflict),
            ("ssh-key".into(), ProblemCode::Conflict),
        ]
    );
    let ItemBody::File(f) = &b.answer.items[2].body else {
        panic!()
    };
    assert_eq!((f.stage, f.mode), (Stage::Two, Mode::M0400));
    // With a mounted host directory there is no stage 2, so no project item.
    let local = request(",\"local_dir\":\"widgets\",\"project_dir\":\"/home/shed/widgets\"");
    let b = build_with(s, &home(), &local);
    assert!(b.answer.items.iter().all(|i| i.name != "files..env"));
    // A missing source file is a problem, not a silent skip.
    let r = FakeReader::new(HOME);
    let one = "[files]\nenabled = true\n[[files.file]]\nfrom = \"~/nope\"\nto = \"x\"\n";
    assert_eq!(
        problems(&build_with(one, &r, &widgets())),
        vec![("files".into(), ProblemCode::MissingInput)]
    );
}

#[test]
fn preview_has_no_values_and_disabled_providers_do_not_run() {
    let r = home();
    let s = Settings::parse(SETTINGS).unwrap();
    let pv = preview(&s, &r, &key());
    let json = serde_json::to_string(&pv).unwrap();
    assert!(
        !json.contains("SENTINEL") && !json.contains(TOKEN) && !json.contains(PASSPHRASE),
        "{json}"
    );
    assert!(json.contains("\"target\":\"EXAMPLE_API_KEY\""));
    assert_eq!(pv.items.len(), 11);
    let off = SETTINGS.replace("[github]\nenabled = true", "[github]\nenabled = false");
    let r = home();
    let s = Settings::parse(&off).unwrap();
    let pv = preview(&s, &r, &key());
    assert!(pv.providers.iter().all(|p| p.provider != "github"));
    assert!(r.ran().is_empty(), "a disabled provider ran a command");
    // The same content gives the same fingerprint on one machine.
    let a = preview(&s, &home(), &key());
    let b = preview(&s, &home(), &key());
    assert_eq!(a, b);
}
