//! The golden test: every provider case's answer, byte for byte, its redacted
//! preview and the commands it ran, held against `fixtures/golden-v0.11.0.json`, which
//! was written by v0.11.0's providers (plan 040 §3.4, criterion 6). Only the answer's
//! version number is normalised (v0.11.0 spoke contract version 1).
//!
//! `SHED_GOLDEN_WRITE=1` writes the fixture instead of comparing; it was run once, on
//! the v0.11.0 tree.

use std::collections::BTreeMap;

use serde_json::{json, Value};
use shed_provision::testing::FakeReader;
use shed_provision::{Built, Preview, ReadError, Request, RunError};

use super::{home, key, key_pair, request, widgets, HOME, NOW, SETTINGS};
use crate::Settings;

const FIXTURE: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/fixtures/golden-v0.11.0.json");

fn build(settings: &str, reader: &FakeReader, req: &Request) -> Built {
    let s = Settings::parse(settings).unwrap_or_else(|e| panic!("{e}"));
    crate::build_answer(&s, req, "work", NOW, None, reader, &key()).unwrap()
}

fn preview(settings: &str, reader: &FakeReader) -> Preview {
    let s = Settings::parse(settings).unwrap_or_else(|e| panic!("{e}"));
    crate::preview(&s, NOW, reader, &key())
}

/// The answer's text with its version number normalised.
fn answer_text(b: &Built) -> String {
    let text = String::from_utf8(b.answer.to_json()).unwrap();
    let rest = text
        .strip_prefix("{\"version\":")
        .and_then(|r| r.split_once(','))
        .map(|(_, r)| r)
        .unwrap_or_else(|| panic!("no version: {text}"));
    format!("{{\"version\":V,{rest}")
}

fn ran(r: &FakeReader) -> Value {
    json!(r
        .ran()
        .iter()
        .map(|c| std::iter::once(c.program.clone())
            .chain(c.args.iter().cloned())
            .collect::<Vec<_>>()
            .join(" "))
        .collect::<Vec<_>>())
}

// Each case owns its reader, so the cases read as one list.
#[allow(clippy::needless_pass_by_value)]
fn case(
    out: &mut BTreeMap<String, Value>,
    name: &str,
    settings: &str,
    r: FakeReader,
    req: &Request,
) {
    let b = build(settings, &r, req);
    let prev = out.insert(
        name.to_string(),
        json!({"answer": answer_text(&b), "preview": b.preview, "ran": ran(&r)}),
    );
    assert!(prev.is_none(), "case {name} twice");
}

#[allow(clippy::needless_pass_by_value)]
fn preview_case(out: &mut BTreeMap<String, Value>, name: &str, settings: &str, r: FakeReader) {
    let p = preview(settings, &r);
    out.insert(name.to_string(), json!({"preview": p, "ran": ran(&r)}));
}

const ONE_KEY: &str = "[ssh-key]\nenabled = true\nkey_file = \"~/.ssh/id_ed25519\"\n";
const GH: &str = "[github]\nenabled = true\ngh = \"/opt/example/gh\"\n";
const GH_TOKEN: [&str; 5] = [
    "/opt/example/gh",
    "auth",
    "token",
    "--hostname",
    "github.com",
];
const GH_USER: [&str; 7] = [
    "/opt/example/gh",
    "api",
    "user",
    "--hostname",
    "github.com",
    "--jq",
    ".login",
];
const GCS: &str = "eyJTRU5USU5FTCI6MX0=";

fn cases() -> BTreeMap<String, Value> {
    let mut out = BTreeMap::new();
    let o = &mut out;

    // every_provider_answers_a_request
    case(o, "every-provider", SETTINGS, home(), &widgets());
    preview_case(o, "every-provider-preview", SETTINGS, home());
    let off = SETTINGS.replace("[github]\nenabled = true", "[github]\nenabled = false");
    preview_case(o, "github-off-preview", &off, home());

    // only_rules_per_provider_and_per_entry
    let gadgets = request(",\"repo\":{\"url\":\"git@git.example.test:example/gadgets.git\",\"name\":\"example/gadgets\"},\"project_dir\":\"/home/shed/gadgets\"");
    case(o, "only-other-repo", SETTINGS, home(), &gadgets);
    let limited = format!("{SETTINGS}\n[github.only]\nservers = [\"home\"]\n");
    case(o, "only-github-other-server", &limited, home(), &widgets());
    let limited = format!("{SETTINGS}\n[ssh-key.only]\nbackends = [\"vz\"]\n");
    case(
        o,
        "only-ssh-key-other-backend",
        &limited,
        home(),
        &widgets(),
    );
    case(o, "no-repo", SETTINGS, home(), &request(""));
    let local = request(",\"local_dir\":\"widgets\",\"project_dir\":\"/home/shed/widgets\"");
    case(o, "local-dir", SETTINGS, home(), &local);

    // ssh_key_problems
    let (locked, locked_pub) = key_pair("aes256-ctr");
    case(
        o,
        "ssh-key-passphrase",
        ONE_KEY,
        FakeReader::new(HOME)
            .file("~/.ssh/id_ed25519", locked, 0o600)
            .file("~/.ssh/id_ed25519.pub", locked_pub, 0o644),
        &widgets(),
    );
    let (private, public) = key_pair("none");
    case(
        o,
        "ssh-key-no-pub",
        ONE_KEY,
        FakeReader::new(HOME).file("~/.ssh/id_ed25519", private.clone(), 0o600),
        &widgets(),
    );
    case(
        o,
        "ssh-key-other-pub",
        ONE_KEY,
        FakeReader::new(HOME)
            .file("~/.ssh/id_ed25519", private.clone(), 0o600)
            .file(
                "~/.ssh/id_ed25519.pub",
                "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOther other\n",
                0o644,
            ),
        &widgets(),
    );
    case(
        o,
        "ssh-key-none",
        ONE_KEY,
        FakeReader::new(HOME),
        &widgets(),
    );
    case(
        o,
        "ssh-key-unset",
        "[ssh-key]\nenabled = true\n",
        FakeReader::new(HOME),
        &widgets(),
    );
    case(
        o,
        "ssh-key-denied",
        ONE_KEY,
        FakeReader::new(HOME).file_error("~/.ssh/id_ed25519", ReadError::Denied),
        &widgets(),
    );
    case(
        o,
        "ssh-key-not-utf8",
        ONE_KEY,
        FakeReader::new(HOME)
            .file("~/.ssh/id_ed25519", vec![0xff, 0xfe], 0o600)
            .file("~/.ssh/id_ed25519.pub", public.clone(), 0o644),
        &widgets(),
    );
    case(
        o,
        "ssh-key-unknown-form",
        ONE_KEY,
        FakeReader::new(HOME)
            .file("~/.ssh/id_ed25519", "not a key\n", 0o600)
            .file("~/.ssh/id_ed25519.pub", public.clone(), 0o644),
        &widgets(),
    );
    case(
        o,
        "ssh-key-unsigned",
        "[ssh-key]\nenabled = true\nkey_file = \"~/.ssh/id_ed25519\"\nsign_commits = false\n",
        FakeReader::new(HOME)
            .file("~/.ssh/id_ed25519", private.clone(), 0o600)
            .file("~/.ssh/id_ed25519.pub", public.clone(), 0o644),
        &widgets(),
    );

    // ssh_key_public_half_carries_nothing_else
    case(
        o,
        "ssh-key-pub-carries-more",
        ONE_KEY,
        FakeReader::new(HOME)
            .file("~/.ssh/id_ed25519", private.clone(), 0o600)
            .file("~/.ssh/id_ed25519.pub", format!("{public}{private}"), 0o644),
        &widgets(),
    );
    case(
        o,
        "ssh-key-through-links",
        ONE_KEY,
        FakeReader::new(HOME)
            .file("~/dotfiles/ssh/id_ed25519", private, 0o600)
            .file("~/dotfiles/ssh/id_ed25519.pub", public, 0o644)
            .link("~/.ssh/id_ed25519", "~/dotfiles/ssh/id_ed25519")
            .link("~/.ssh/id_ed25519.pub", "~/dotfiles/ssh/id_ed25519.pub"),
        &widgets(),
    );
    case(
        o,
        "ssh-key-fifo",
        ONE_KEY,
        FakeReader::new(HOME).special("~/.ssh/id_ed25519"),
        &widgets(),
    );

    // a_large_env_file_of_one_variable
    case(
        o,
        "api-keys-large-env-file",
        "[api-keys]\nenabled = true\ncraze_providers = false\nenv_files = [\"~/a.env\"]\n",
        FakeReader::new(HOME).file("~/a.env", "A=1\n".repeat(65_536), 0o600),
        &widgets(),
    );

    // github_problems_carry_no_output
    let gh_cases: Vec<(&str, FakeReader)> = vec![
        ("github-no-gh", FakeReader::new(HOME)),
        (
            "github-signed-out",
            FakeReader::new(HOME).command(&GH_TOKEN, 1, "SENTINEL no oauth token"),
        ),
        (
            "github-token-timed-out",
            FakeReader::new(HOME).command_error(&GH_TOKEN, RunError::TimedOut),
        ),
        (
            "github-not-a-token",
            FakeReader::new(HOME).command(&GH_TOKEN, 0, "SENTINEL not a token!\n"),
        ),
        (
            "github-user-failed",
            FakeReader::new(HOME)
                .command(&GH_TOKEN, 0, "gho_SENTINEL\n")
                .command(&GH_USER, 1, "SENTINEL"),
        ),
        (
            "github-not-a-login",
            FakeReader::new(HOME)
                .command(&GH_TOKEN, 0, "gho_SENTINEL\n")
                .command(&GH_USER, 0, "-not a login\n"),
        ),
        (
            "github-user-too-much",
            FakeReader::new(HOME)
                .command(&GH_TOKEN, 0, "gho_SENTINEL\n")
                .command_error(&GH_USER, RunError::OutputTooLarge),
        ),
        (
            "github-ok",
            FakeReader::new(HOME)
                .command(&GH_TOKEN, 0, "gho_SENTINEL\r\n")
                .command(&GH_USER, 0, "ada-example\n"),
        ),
    ];
    for (name, r) in gh_cases {
        case(o, name, GH, r, &widgets());
    }

    // api_keys_env_files
    case(
        o,
        "api-keys-env-files",
        "[api-keys]\nenabled = true\ncraze_providers = false\nenv_files = [\"~/a.env\", \"~/b.env\", \"/etc/c.env\"]\n",
        FakeReader::new(HOME)
            .file(
                "~/a.env",
                "GOOD=1\nPATH=/SENTINEL\nlower=SENTINEL\nSPACE=SENTINEL \nnoequals\nDUP=SENTINEL-a\nTAB=SENTINEL\tx\n",
                0o600,
            )
            .file("~/b.env", "DUP=SENTINEL-b\n", 0o400)
            .file("/etc/c.env", "SHARED=SENTINEL\n", 0o644),
        &widgets(),
    );
    case(
        o,
        "api-keys-no-craze-file",
        "[api-keys]\nenabled = true\n",
        FakeReader::new(HOME),
        &widgets(),
    );
    case(
        o,
        "api-keys-not-utf8",
        "[api-keys]\nenabled = true\ncraze_providers = false\nenv_files = [\"~/a.env\"]\n",
        FakeReader::new(HOME).file("~/a.env", vec![b'A', b'=', 0xff], 0o600),
        &widgets(),
    );
    case(
        o,
        "api-keys-fallback-names",
        "[api-keys]\nenabled = true\ncraze_providers = false\nenv_files = [\"~/a.env\"]\n",
        FakeReader::new(HOME).file(
            "~/a.env",
            format!("A_{}=1\nB=2\n# c\n  \nC=3\r\n", "X".repeat(70)),
            0o600,
        ),
        &widgets(),
    );

    // envsecrets_login
    let by_command = "[envsecrets-login]\nenabled = true\npassphrase_command = [\"example-pass\", \"show\", \"envsecrets\"]\n";
    let by_file = "[envsecrets-login]\nenabled = true\npassphrase_file = \"~/p\"\n";
    let pass = ["example-pass", "show", "envsecrets"];
    let config = format!("passphrase_env: ENVSECRETS_PASSPHRASE\ngcs_credentials: {GCS}\n");
    case(
        o,
        "envsecrets-command",
        by_command,
        FakeReader::new(HOME)
            .file("~/.envsecrets/config.yaml", config.clone(), 0o600)
            .command(&pass, 0, "SENTINEL-from-command\n"),
        &widgets(),
    );
    case(
        o,
        "envsecrets-command-failed",
        by_command,
        FakeReader::new(HOME)
            .file("~/.envsecrets/config.yaml", config.clone(), 0o600)
            .command(&pass, 2, "SENTINEL-partial\n"),
        &widgets(),
    );
    case(
        o,
        "envsecrets-command-not-utf8",
        by_command,
        FakeReader::new(HOME)
            .file("~/.envsecrets/config.yaml", config.clone(), 0o600)
            .command(&pass, 0, "\u{0}\n"),
        &widgets(),
    );
    for (i, cfg) in [
        format!("gcs_credentials: {GCS}\n"),
        "passphrase_env: ENVSECRETS_PASSPHRASE\n".to_string(),
        format!("passphrase_env: PATH\ngcs_credentials: {GCS}\n"),
        format!("passphrase_env: |\n  X\ngcs_credentials: {GCS}\n"),
        "passphrase_env: X\ngcs_credentials:\n  type: service_account\n".to_string(),
    ]
    .into_iter()
    .enumerate()
    {
        case(
            o,
            &format!("envsecrets-config-{i}"),
            by_command,
            FakeReader::new(HOME)
                .file("~/.envsecrets/config.yaml", cfg, 0o600)
                .command(&pass, 0, "p\n"),
            &widgets(),
        );
    }
    case(
        o,
        "envsecrets-config-not-utf8",
        by_command,
        FakeReader::new(HOME)
            .file("~/.envsecrets/config.yaml", vec![0xff], 0o600)
            .command(&pass, 0, "p\n"),
        &widgets(),
    );
    case(
        o,
        "envsecrets-no-config",
        by_command,
        FakeReader::new(HOME).command(&pass, 0, "p\n"),
        &widgets(),
    );
    case(
        o,
        "envsecrets-neither",
        "[envsecrets-login]\nenabled = true\n",
        FakeReader::new(HOME).file("~/.envsecrets/config.yaml", config.clone(), 0o600),
        &widgets(),
    );
    case(
        o,
        "envsecrets-file",
        by_file,
        FakeReader::new(HOME)
            .file("~/.envsecrets/config.yaml", config.clone(), 0o600)
            .file("~/p", "SENTINEL-pass\r\n", 0o600),
        &widgets(),
    );
    case(
        o,
        "envsecrets-file-missing",
        by_file,
        FakeReader::new(HOME).file("~/.envsecrets/config.yaml", config.clone(), 0o600),
        &widgets(),
    );
    case(
        o,
        "envsecrets-passphrase-breaks-rule",
        by_file,
        FakeReader::new(HOME)
            .file("~/.envsecrets/config.yaml", config, 0o600)
            .file("~/p", " lead\n", 0o600),
        &widgets(),
    );

    // files_conflicts_and_stage_2
    let s = "[ssh-key]\nenabled = true\nkey_file = \"~/.ssh/id_ed25519\"\nsign_commits = true\n\
             [github]\nenabled = true\n\
             [files]\nenabled = true\n\
             [[files.file]]\nfrom = \"~/.npmrc\"\nto = \".config/git/config\"\n\
             [[files.file]]\nfrom = \"~/.npmrc\"\nto = \".config/gh\"\n\
             [[files.file]]\nfrom = \"~/work/widgets.env\"\nto = \".env\"\nbase = \"project\"\nmode = \"0400\"\n";
    case(o, "files-conflicts", s, home(), &widgets());
    case(o, "files-conflicts-local-dir", s, home(), &local);
    case(
        o,
        "files-missing",
        "[files]\nenabled = true\n[[files.file]]\nfrom = \"~/nope\"\nto = \"x\"\n",
        FakeReader::new(HOME),
        &widgets(),
    );

    // Added for plan 040: a partial read (one env file unreadable, one good).
    case(
        o,
        "partial-read",
        "[api-keys]\nenabled = true\ncraze_providers = false\nenv_files = [\"~/bad.env\", \"~/good.env\"]\n",
        FakeReader::new(HOME)
            .file_error("~/bad.env", ReadError::Denied)
            .file("~/good.env", "GOOD=SENTINEL-good\n", 0o600),
        &widgets(),
    );
    // A file scoped out by its own `only`: not read at all (it does not exist, so
    // a read would be a problem).
    case(
        o,
        "file-scoped-out",
        "[files]\nenabled = true\n\
         [[files.file]]\nfrom = \"~/absent\"\nto = \"absent\"\nonly = { servers = [\"home\"] }\n\
         [[files.file]]\nfrom = \"~/.npmrc\"\nto = \".npmrc\"\n",
        home(),
        &widgets(),
    );
    // Names that collide, the first entry unreadable: the second takes the name,
    // the third falls back to its index.
    case(
        o,
        "names-collide",
        "[files]\nenabled = true\n\
         [[files.file]]\nfrom = \"~/absent\"\nto = \"Dot\"\n\
         [[files.file]]\nfrom = \"~/.npmrc\"\nto = \"dot\"\n\
         [[files.file]]\nfrom = \"~/.npmrc\"\nto = \"DOT\"\n\
         [[files.file]]\nfrom = \"~/.npmrc\"\nto = \"bad name!\"\n",
        home(),
        &widgets(),
    );
    // craze's providers.toml.
    case(
        o,
        "craze-providers",
        "[api-keys]\nenabled = true\n",
        home(),
        &widgets(),
    );
    out
}

#[test]
fn the_answers_are_v0_11_0s() {
    let got = Value::Object(cases().into_iter().collect());
    if std::env::var_os("SHED_GOLDEN_WRITE").is_some() {
        let text = serde_json::to_string_pretty(&got).unwrap() + "\n";
        std::fs::create_dir_all(std::path::Path::new(FIXTURE).parent().unwrap()).unwrap();
        std::fs::write(FIXTURE, text).unwrap();
        return;
    }
    let want: Value = serde_json::from_str(&std::fs::read_to_string(FIXTURE).unwrap()).unwrap();
    let (Value::Object(got), Value::Object(want)) = (&got, &want) else {
        panic!()
    };
    assert_eq!(
        got.keys().collect::<Vec<_>>(),
        want.keys().collect::<Vec<_>>(),
        "the cases"
    );
    for (name, w) in want {
        assert_eq!(&got[name], w, "case {name}");
    }
}
