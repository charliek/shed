//! Discovery's tests, including a REAL local run of [`PROBE_SCRIPT`] against a
//! fixture `$GROK_HOME`.
//!
//! Running the script rather than asserting on its text is the point: the three
//! things it must get right — always exit 0, print the token last, withhold a
//! symlinked or wrongly-moded token — are properties of `sh` and `find`, not of
//! a string, and a golden of the string would pass while the script did the
//! opposite.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use super::*;

/// The crate's one fixture token, under this module's shorter name.
///
/// It is [`crate::testing::SENTINEL_TOKEN`] rather than a second copy of the
/// literal: the whole value of a sentinel is that ONE grep finds every use of
/// it, and two definitions is how a rotated fixture leaves half the suite
/// asserting against a string no code produces any more.
use crate::testing::SENTINEL_TOKEN as SENTINEL;

// ---------------------------------------------------------------------------
// a throwaway $GROK_HOME
// ---------------------------------------------------------------------------

struct TempHome(PathBuf);

impl TempHome {
    fn new(tag: &str) -> TempHome {
        static N: AtomicU64 = AtomicU64::new(0);
        let p = std::env::temp_dir().join(format!(
            "shed-gx-{tag}-{}-{}",
            std::process::id(),
            N.fetch_add(1, Ordering::SeqCst)
        ));
        let _ = std::fs::remove_dir_all(&p);
        std::fs::create_dir_all(&p).expect("creating the fixture home");
        TempHome(p)
    }

    fn path(&self) -> &Path {
        &self.0
    }

    fn write(&self, name: &str, body: &str) -> PathBuf {
        let p = self.0.join(name);
        std::fs::write(&p, body).expect("writing the fixture file");
        p
    }

    /// Write the token file with an exact mode — the whole point of the
    /// eligibility rule.
    fn write_token(&self, name: &str, body: &str, mode: u32) -> PathBuf {
        use std::os::unix::fs::PermissionsExt as _;
        let p = self.write(name, body);
        std::fs::set_permissions(&p, std::fs::Permissions::from_mode(mode))
            .expect("setting the fixture token's mode");
        p
    }
}

impl Drop for TempHome {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// The record shape a real leader writes: pretty-printed, multi-line, and with
/// a **slash-free** url.
fn record_json(url: &str, instance: &str, token_file: &str) -> String {
    format!(
        "{{\n  \"url\": \"{url}\",\n  \"pid\": 4242,\n  \"instanceId\": \"{instance}\",\n  \
         \"socketPath\": \"/tmp/gx-fixture-leader.sock\",\n  \"tokenFile\": \"{token_file}\",\n  \
         \"version\": \"1.0.16+gx.12\",\n  \"startedAt\": 1788939060616\n}}\n"
    )
}

fn run_probe(home: &Path) -> (i32, String) {
    let out = std::process::Command::new("sh")
        .arg("-c")
        .arg(PROBE_SCRIPT)
        .env("GROK_HOME", home)
        .output()
        .expect("running the probe");
    (
        out.status.code().unwrap_or(-1),
        String::from_utf8_lossy(&out.stdout).into_owned(),
    )
}

// ---------------------------------------------------------------------------
// the token type
// ---------------------------------------------------------------------------

#[test]
fn a_real_token_carries_a_trailing_newline_and_is_still_accepted() {
    // 65 bytes on disk, verified on a live gx 1.0.16+gx.12 leader. A validator
    // that ran on the raw bytes would refuse every real token.
    let raw = format!("{SENTINEL}\n");
    assert_eq!(raw.len(), 65);
    let token = GxToken::parse(&raw).expect("a trailing newline is trimmed");
    assert_eq!(token.expose(), SENTINEL);
    // Surrounding whitespace of any shape.
    assert!(GxToken::parse(&format!("  {SENTINEL}  \r\n")).is_some());
}

#[test]
fn a_token_that_is_not_64_lowercase_hex_is_refused() {
    for bad in [
        "",
        "short",
        // 63 and 65 hex digits.
        &SENTINEL[..63],
        &format!("{SENTINEL}a"),
        // Uppercase is refused rather than folded: gx mints lowercase, and
        // accepting another spelling means the two readers disagree.
        &SENTINEL.to_ascii_uppercase(),
        // 64 characters, one of them not hex.
        &format!("z{}", &SENTINEL[1..]),
        // Interior whitespace is not trimmed away.
        &format!("{} {}", &SENTINEL[..32], &SENTINEL[33..]),
    ] {
        assert!(
            GxToken::parse(bad).is_none(),
            "accepted a malformed token: {bad:?}"
        );
    }
}

#[test]
fn the_token_never_appears_in_a_debug_string() {
    let token = GxToken::parse(SENTINEL).expect("parses");
    let discovery = GxDiscovery {
        token: token.clone(),
        instance_id: "facade00facade00facade00facade00".to_string(),
    };
    let creds = StaticCredentials::new(discovery.clone());
    let probe = Probe {
        records: vec![],
        token: Some(token.clone()),
        unreadable_records: 0,
    };

    for (what, rendered) in [
        ("GxToken", format!("{token:?}")),
        ("GxDiscovery", format!("{discovery:?}")),
        ("StaticCredentials", format!("{creds:?}")),
        ("Probe", format!("{probe:?}")),
        // The alternate (pretty) formatter is a separate code path in `derive`.
        ("GxDiscovery pretty", format!("{discovery:#?}")),
    ] {
        assert!(
            !rendered.contains(SENTINEL),
            "{what} leaked the token: {rendered}"
        );
    }
    assert_eq!(format!("{token:?}"), "GxToken(<redacted>)");
    // The instance id is NOT a secret and stays visible — it is what a
    // diagnostic needs.
    assert!(format!("{discovery:?}").contains("facade00facade00facade00facade00"));
}

#[test]
fn the_authorization_header_is_marked_sensitive() {
    let token = GxToken::parse(SENTINEL).expect("parses");
    let value = token
        .authorization()
        .expect("64 hex digits are legal header bytes");
    assert!(
        value.is_sensitive(),
        "the Authorization value must be sensitive so hyper never logs it"
    );
    // A sensitive HeaderValue also refuses to print itself.
    assert!(!format!("{value:?}").contains(SENTINEL));
}

// ---------------------------------------------------------------------------
// the probe script, run for real
// ---------------------------------------------------------------------------

#[test]
fn the_probe_exits_zero_and_prints_the_token_last() {
    let home = TempHome::new("probe-ok");
    let token_path = home.path().join(DEFAULT_TOKEN_FILE);
    home.write(
        "gx-remote-52e273e2a1d25203.json",
        &record_json(
            "http://127.0.0.1:2431",
            "facade00facade00facade00facade00",
            &token_path.display().to_string(),
        ),
    );
    // The real file: 64 hex plus a newline, mode 0600.
    home.write_token(DEFAULT_TOKEN_FILE, &format!("{SENTINEL}\n"), 0o600);

    let (code, stdout) = run_probe(home.path());
    assert_eq!(code, 0, "the probe must always exit 0");

    let sentinel_at = stdout
        .find(PROBE_TOKEN_SENTINEL)
        .expect("the sentinel line");
    let token_at = stdout.find(SENTINEL).expect("the token");
    assert!(
        token_at > sentinel_at,
        "the token must be printed LAST, after everything that can fail"
    );

    let probe = parse_probe(&stdout).expect("parses");
    assert_eq!(probe.records.len(), 1);
    assert_eq!(probe.records[0].url, "http://127.0.0.1:2431");
    assert_eq!(
        probe.records[0].instance_id,
        "facade00facade00facade00facade00"
    );
    // The tokenFile field names the UNSUFFIXED token beside the SUFFIXED
    // record — one token per $GROK_HOME.
    assert_eq!(
        probe.records[0].token_file,
        token_path.display().to_string()
    );
    assert_eq!(
        probe.token.as_ref().map(GxToken::expose),
        Some(SENTINEL),
        "an eligible 0600 token is read"
    );
}

#[test]
fn the_probe_withholds_a_symlinked_or_world_readable_token() {
    for (tag, setup) in [
        ("symlink", 0u32),
        ("mode-0644", 0o644),
        ("mode-0640", 0o640),
        ("mode-0700", 0o700),
    ] {
        let home = TempHome::new(&format!("probe-{tag}"));
        home.write(
            "gx-remote.json",
            &record_json("http://127.0.0.1:2431", "abc", ""),
        );
        if tag == "symlink" {
            // A symlink to a perfectly eligible file: `find -type f` under its
            // default -P is false for the LINK, which is the whole check.
            let real = home.write_token("real.token", &format!("{SENTINEL}\n"), 0o600);
            std::os::unix::fs::symlink(&real, home.path().join(DEFAULT_TOKEN_FILE))
                .expect("creating the symlink");
        } else {
            home.write_token(DEFAULT_TOKEN_FILE, &format!("{SENTINEL}\n"), setup);
        }

        let (code, stdout) = run_probe(home.path());
        assert_eq!(code, 0, "{tag}: the probe must always exit 0");
        assert!(
            !stdout.contains(SENTINEL),
            "{tag}: an ineligible token must never be printed"
        );
        assert!(
            stdout.contains(PROBE_TOKEN_SENTINEL),
            "{tag}: the sentinel still says the probe ran"
        );
        let probe = parse_probe(&stdout).expect("parses");
        assert!(probe.token.is_none(), "{tag}");
        assert_eq!(probe.records.len(), 1, "{tag}: the record still came back");
    }
}

/// A `$GROK_HOME` whose path contains a space and a single quote.
///
/// Every path in the script is double-quoted for exactly this, and the glob
/// still has to expand inside those quotes.
#[test]
fn the_probe_handles_a_home_with_a_space_and_a_quote_in_its_path() {
    let home = TempHome::new("probe with a space and a ' quote");
    home.write(
        "gx-remote.json",
        &record_json("http://127.0.0.1:2431", "quoted", ""),
    );
    home.write_token(DEFAULT_TOKEN_FILE, &format!("{SENTINEL}\n"), 0o600);

    let (code, stdout) = run_probe(home.path());
    assert_eq!(code, 0);
    let probe = parse_probe(&stdout).expect("parses");
    assert_eq!(
        probe.records.len(),
        1,
        "the glob expanded inside the quotes"
    );
    assert_eq!(probe.records[0].instance_id, "quoted");
    assert_eq!(probe.token.as_ref().map(GxToken::expose), Some(SENTINEL));
}

/// A `$GROK_HOME` that does not exist at all.
#[test]
fn the_probe_survives_a_missing_grok_home() {
    let missing = std::env::temp_dir().join("shed-gx-no-such-home-ever");
    let _ = std::fs::remove_dir_all(&missing);
    let (code, stdout) = run_probe(&missing);
    assert_eq!(code, 0, "an absent home is still exit 0");
    let probe = parse_probe(&stdout).expect("parses");
    assert!(probe.records.is_empty());
    assert!(probe.token.is_none());
}

/// The probe uses only POSIX `find` predicates.
///
/// `-maxdepth` is a GNU/BSD extension: a strictly POSIX `find` errors on it and
/// the probe then yields NO token at all — a silent, total failure of the SSH
/// path on any host whose `find` is strict. The probe runs on an arbitrary
/// remote host, so this must not depend on GNU `find`.
#[test]
fn the_probe_avoids_non_posix_find_flags() {
    assert!(
        !PROBE_SCRIPT.contains("-maxdepth"),
        "-maxdepth is not POSIX; -prune is",
    );
    assert!(PROBE_SCRIPT.contains("-prune"));
    // find both checks AND reads, so the path is resolved once rather than
    // tested and then `cat`-ed separately.
    assert!(PROBE_SCRIPT.contains("-exec cat {}"));
}

#[test]
fn the_probe_survives_a_home_with_no_records_and_no_token() {
    let home = TempHome::new("probe-empty");
    let (code, stdout) = run_probe(home.path());
    assert_eq!(code, 0);
    let probe = parse_probe(&stdout).expect("parses");
    assert!(probe.records.is_empty());
    assert!(probe.token.is_none());
    assert_eq!(probe.unreadable_records, 0);
}

#[test]
fn the_probe_finds_several_records_and_the_glob_is_the_primary_path() {
    let home = TempHome::new("probe-many");
    // The suffixed form is the COMMON case: any leader not on the default
    // socket writes it, which includes every scratch leader.
    home.write(
        "gx-remote-52e273e2a1d25203.json",
        &record_json("http://127.0.0.1:2431", "scratch", ""),
    );
    home.write(
        "gx-remote.json",
        &record_json("http://127.0.0.1:2421", "default", ""),
    );
    home.write(
        "gx-remote-aaaaaaaaaaaaaaaa.json",
        &record_json("http://127.0.0.1:2441", "third", ""),
    );
    // Neither of these is a record: the token has no `.json`, and the other is
    // not `gx-remote*`.
    home.write_token(DEFAULT_TOKEN_FILE, &format!("{SENTINEL}\n"), 0o600);
    home.write("config.json", "{}");

    let (code, stdout) = run_probe(home.path());
    assert_eq!(code, 0);
    let probe = parse_probe(&stdout).expect("parses");
    assert_eq!(probe.records.len(), 3, "all three records, no config.json");

    // One of them matches the reported URL, and it is the one that is picked.
    let matched = records_for(&probe.records, "http://127.0.0.1:2431");
    assert_eq!(matched.len(), 1);
    assert_eq!(matched[0].instance_id, "scratch");
}

// ---------------------------------------------------------------------------
// the parser
// ---------------------------------------------------------------------------

#[test]
fn parse_probe_handles_zero_one_and_several_records() {
    let one = format!(
        "{}\n---\n{}\n",
        record_json("http://127.0.0.1:2431", "a", ""),
        PROBE_TOKEN_SENTINEL
    );
    assert_eq!(parse_probe(&one).expect("parses").records.len(), 1);

    let none = format!("{PROBE_TOKEN_SENTINEL}\n");
    assert!(parse_probe(&none).expect("parses").records.is_empty());

    let two = format!(
        "{}\n---\n{}\n---\n{}\n",
        record_json("http://127.0.0.1:2431", "a", ""),
        record_json("http://127.0.0.1:2432", "b", ""),
        PROBE_TOKEN_SENTINEL
    );
    let p = parse_probe(&two).expect("parses");
    assert_eq!(p.records.len(), 2);
    assert_eq!(p.records[1].instance_id, "b");
}

#[test]
fn parse_probe_skips_an_unreadable_record_and_keeps_the_rest() {
    let mixed = format!(
        "not json at all\n---\n{}\n---\n{}\n",
        record_json("http://127.0.0.1:2431", "good", ""),
        PROBE_TOKEN_SENTINEL
    );
    let p = parse_probe(&mixed).expect("parses");
    assert_eq!(p.records.len(), 1);
    assert_eq!(p.records[0].instance_id, "good");
    assert_eq!(p.unreadable_records, 1);
}

#[test]
fn parse_probe_reports_a_truncated_run_and_a_malformed_token() {
    // No sentinel at all: the probe did not run to completion.
    let truncated = format!("{}\n---\n", record_json("http://127.0.0.1:2431", "a", ""));
    assert_eq!(parse_probe(&truncated).unwrap_err(), ProbeError::Truncated);

    let malformed = format!("{PROBE_TOKEN_SENTINEL}\nnot-a-token\n");
    assert_eq!(
        parse_probe(&malformed).unwrap_err(),
        ProbeError::MalformedToken
    );
}

#[test]
fn probe_errors_never_echo_the_probes_output() {
    // Every failure path, given input that CONTAINS the sentinel token.
    let with_token = format!("{PROBE_TOKEN_SENTINEL}\n{SENTINEL} and some trailing junk\n");
    let err = parse_probe(&with_token).expect_err("a token with trailing junk is malformed");
    let rendered = format!("{err}{err:?}");
    assert!(!rendered.contains(SENTINEL), "leaked: {rendered}");

    let truncated = format!("{SENTINEL}\n---\n");
    let err = parse_probe(&truncated).expect_err("no sentinel");
    let rendered = format!("{err}{err:?}");
    assert!(!rendered.contains(SENTINEL), "leaked: {rendered}");
}

#[test]
fn parse_probe_reads_a_token_with_or_without_a_trailing_newline() {
    for tail in ["", "\n", "\r\n", "\n\n"] {
        let out = format!("{PROBE_TOKEN_SENTINEL}\n{SENTINEL}{tail}");
        let p = parse_probe(&out).expect("parses");
        assert_eq!(p.token.as_ref().map(GxToken::expose), Some(SENTINEL));
    }
}

// ---------------------------------------------------------------------------
// URL matching
// ---------------------------------------------------------------------------

#[test]
fn same_reported_url_is_loopback_clean_and_exact() {
    // The shape gx actually writes, and roost actually forwards.
    assert!(same_reported_url(
        "http://127.0.0.1:2431",
        "http://127.0.0.1:2431"
    ));
    assert!(same_reported_url(
        " http://127.0.0.1:2431\n",
        "http://127.0.0.1:2431"
    ));

    for (a, b, why) in [
        (
            "http://127.0.0.1:2431/",
            "http://127.0.0.1:2431",
            "a trailing slash is a DIAL url and loopback_base_url rejects it",
        ),
        (
            "http://localhost:2431",
            "http://127.0.0.1:2431",
            "both are loopback-clean, but they are not the same string",
        ),
        (
            "http://127.0.0.1:2431",
            "http://127.0.0.1:2432",
            "different ports",
        ),
        (
            "https://127.0.0.1:2431",
            "https://127.0.0.1:2431",
            "https is not the rule roost applied",
        ),
        (
            "http://10.0.0.4:2431",
            "http://10.0.0.4:2431",
            "not loopback",
        ),
        ("http://127.0.0.1", "http://127.0.0.1", "no explicit port"),
    ] {
        assert!(!same_reported_url(a, b), "{why}: {a} vs {b}");
    }
}

#[test]
fn records_for_matches_on_the_reported_url_and_ignores_a_dead_pid() {
    let records = vec![
        GxRecord {
            url: "http://127.0.0.1:2421".to_string(),
            pid: 1,
            instance_id: "other".to_string(),
            socket_path: String::new(),
            token_file: String::new(),
            version: String::new(),
            started_at: 0,
        },
        GxRecord {
            // pid 1 is certainly not this leader, and it is still the match:
            // the instanceId pin is the real test, and it does not race a
            // restart the way a pid check does.
            url: "http://127.0.0.1:2431".to_string(),
            pid: 1,
            instance_id: "wanted".to_string(),
            socket_path: String::new(),
            token_file: String::new(),
            version: String::new(),
            started_at: 0,
        },
    ];
    let got = records_for(&records, "http://127.0.0.1:2431");
    assert_eq!(got.len(), 1);
    assert_eq!(got[0].instance_id, "wanted");
    assert!(records_for(&records, "http://127.0.0.1:9999").is_empty());
}

// ---------------------------------------------------------------------------
// the local reader's checks
// ---------------------------------------------------------------------------

#[test]
fn token_file_refusal_is_gxs_own_rule() {
    let ok = TokenFileFacts {
        is_symlink: false,
        is_regular: true,
        mode: 0o600,
        uid: 1000,
    };
    assert_eq!(token_file_refusal(&ok, 1000), None);

    for (facts, uid, want) in [
        (
            TokenFileFacts {
                is_symlink: true,
                ..ok
            },
            1000,
            TokenRefusal::Symlink,
        ),
        (
            TokenFileFacts {
                is_regular: false,
                ..ok
            },
            1000,
            TokenRefusal::NotRegular,
        ),
        // Exact mode: a token another user can read is one the caller cannot
        // claim sole authority over, and that authority IS the bearer's meaning.
        (
            TokenFileFacts { mode: 0o644, ..ok },
            1000,
            TokenRefusal::Mode,
        ),
        (
            TokenFileFacts { mode: 0o640, ..ok },
            1000,
            TokenRefusal::Mode,
        ),
        (
            TokenFileFacts { mode: 0o400, ..ok },
            1000,
            TokenRefusal::Mode,
        ),
        (
            TokenFileFacts { mode: 0o660, ..ok },
            1000,
            TokenRefusal::Mode,
        ),
        (ok, 1001, TokenRefusal::Owner),
    ] {
        assert_eq!(token_file_refusal(&facts, uid), Some(want), "{facts:?}");
    }

    // The high bits (setuid/sticky) are part of the exact match, not masked
    // away — `04600` is not `0600`.
    assert_eq!(
        token_file_refusal(&TokenFileFacts { mode: 0o4600, ..ok }, 1000),
        Some(TokenRefusal::Mode)
    );
    // The file-TYPE bits above 0o7777 are, though: `mode()` returns them and
    // they are not permissions.
    assert_eq!(
        token_file_refusal(
            &TokenFileFacts {
                mode: 0o100_600,
                ..ok
            },
            1000
        ),
        None
    );
}

#[test]
fn read_token_file_applies_the_rule_to_the_file_it_opened() {
    let home = TempHome::new("local-read");
    let uid = std::fs::metadata(home.path())
        .map(|m| {
            use std::os::unix::fs::MetadataExt as _;
            m.uid()
        })
        .expect("the fixture home's uid");

    let good = home.write_token("good.token", &format!("{SENTINEL}\n"), 0o600);
    assert_eq!(
        read_token_file(&good, uid).expect("eligible").expose(),
        SENTINEL
    );

    let loose = home.write_token("loose.token", &format!("{SENTINEL}\n"), 0o644);
    assert_eq!(read_token_file(&loose, uid), Err(TokenRefusal::Mode));

    let link = home.path().join("link.token");
    std::os::unix::fs::symlink(&good, &link).expect("symlink");
    assert_eq!(read_token_file(&link, uid), Err(TokenRefusal::Symlink));

    let junk = home.write_token("junk.token", "not a token\n", 0o600);
    assert_eq!(read_token_file(&junk, uid), Err(TokenRefusal::Malformed));

    assert_eq!(
        read_token_file(&home.path().join("absent.token"), uid),
        Err(TokenRefusal::Unreadable)
    );

    // A directory named like a token: refused as not-regular rather than read.
    let dir = home.path().join("dir.token");
    std::fs::create_dir(&dir).expect("dir");
    assert!(matches!(
        read_token_file(&dir, uid),
        Err(TokenRefusal::NotRegular) | Err(TokenRefusal::Unreadable)
    ));

    // Wrong owner. uid 0 is not the test runner (and if it were, the test is
    // meaningless rather than wrong, so it is skipped).
    if uid != 0 {
        assert_eq!(read_token_file(&good, 0), Err(TokenRefusal::Owner));
    }
}

/// The token's bytes come from the descriptor the checks ran on.
///
/// The function used to validate a descriptor and then `read_to_string(path)` —
/// a fourth resolution nothing had checked, so a file replaced in between was
/// read unvalidated and shipped as a bearer token. A gx leader rewriting its
/// token file on restart lands in that window with no attacker involved.
///
/// **What this test can and cannot show.** The window is closed by construction
/// — there is exactly one `File::open`, and both the `fstat` checks and the read
/// use that descriptor — which is verifiable by reading the function. Actually
/// racing it would need the path swapped between the open and the read, and
/// there is no portable hook to suspend the function there, so this pins the
/// observable half: the content is exactly the validated file's, and every
/// refusal still fires on a file that is ineligible BEFORE the open.
#[test]
fn the_token_is_read_from_the_descriptor_that_was_validated() {
    let home = TempHome::new("fd-read");
    let uid = {
        use std::os::unix::fs::MetadataExt as _;
        std::fs::metadata(home.path()).expect("stat").uid()
    };
    let token = home.write_token("t.token", &format!("{SENTINEL}\n"), 0o600);
    assert_eq!(
        read_token_file(&token, uid).expect("eligible").expose(),
        SENTINEL,
        "exactly the validated file's content",
    );

    // A hard link to the same inode reads identically — the checks and the read
    // are about the OBJECT, not the name it was reached by.
    let link = home.path().join("hard.token");
    std::fs::hard_link(&token, &link).expect("hard link");
    assert_eq!(
        read_token_file(&link, uid).expect("same inode").expose(),
        SENTINEL,
    );

    // Ineligible before the open is still refused, on the fd's own facts.
    let loose = home.write_token("loose.token", &format!("{SENTINEL}\n"), 0o644);
    assert_eq!(read_token_file(&loose, uid), Err(TokenRefusal::Mode));
}

#[test]
fn a_token_refusal_never_names_the_token_or_the_path() {
    for r in [
        TokenRefusal::Symlink,
        TokenRefusal::NotRegular,
        TokenRefusal::Mode,
        TokenRefusal::Owner,
        TokenRefusal::Unreadable,
        TokenRefusal::Malformed,
    ] {
        let display = format!("{r}");
        let debug = format!("{r:?}");
        assert!(!display.contains(SENTINEL), "{display}");
        assert!(!debug.contains(SENTINEL), "{debug}");
        // A refusal says WHAT was wrong; the caller already knows where it
        // looked, and a path in an error is one copy-paste from a log.
        assert!(
            !display.contains('/') && !debug.contains('/'),
            "a refusal names no path: {display}"
        );
    }
}

#[test]
fn record_files_globs_gx_remote_json_and_never_the_token() {
    let home = TempHome::new("glob");
    home.write("gx-remote.json", "{}");
    home.write("gx-remote-52e273e2a1d25203.json", "{}");
    home.write_token(DEFAULT_TOKEN_FILE, SENTINEL, 0o600);
    home.write("gx-remote.json.bak", "{}");
    home.write("other.json", "{}");

    let names: Vec<String> = record_files(home.path())
        .iter()
        .map(|p| p.file_name().unwrap().to_string_lossy().into_owned())
        .collect();
    assert_eq!(
        names,
        vec!["gx-remote-52e273e2a1d25203.json", "gx-remote.json"],
        "sorted, .json only, and never the token"
    );
    // A home that does not exist is empty, not an error.
    assert!(record_files(Path::new("/nonexistent/shed-gx")).is_empty());

    for (name, want) in [
        ("gx-remote.json", true),
        ("gx-remote-52e273e2a1d25203.json", true),
        ("gx-remote.token", false),
        ("gx-remote.json.bak", false),
        ("remote.json", false),
    ] {
        assert_eq!(is_record_file_name(name), want, "{name}");
    }
}

/// The token path comes from the record — and only when it stays inside
/// `$GROK_HOME`.
#[test]
fn the_token_path_comes_from_the_record_and_is_bounded_to_the_home() {
    let home = TempHome::new("token-path");
    let real = home.write_token(DEFAULT_TOKEN_FILE, &format!("{SENTINEL}\n"), 0o600);

    // The real pairing: a SUFFIXED record naming an UNSUFFIXED token, inside
    // the home.
    let inside = GxRecord {
        url: "http://127.0.0.1:2431".to_string(),
        pid: 1,
        instance_id: "a".to_string(),
        socket_path: String::new(),
        token_file: real.display().to_string(),
        version: String::new(),
        started_at: 0,
    };
    assert_eq!(
        token_path_for(&inside, home.path()).expect("inside the home"),
        real.canonicalize().expect("canonicalizes"),
    );

    // No `tokenFile`: the default under the home, still unsuffixed.
    let bare = GxRecord {
        token_file: String::new(),
        ..inside.clone()
    };
    assert_eq!(
        token_path_for(&bare, home.path()).expect("the default"),
        real.canonicalize().expect("canonicalizes"),
    );

    // OUTSIDE the home: refused, even though the file is real, caller-owned,
    // 0600 and a perfectly valid 64-hex token. This is the arbitrary-path read.
    let elsewhere = TempHome::new("token-path-elsewhere");
    let planted = elsewhere.write_token("stolen.token", &format!("{SENTINEL}\n"), 0o600);
    let uid = {
        use std::os::unix::fs::MetadataExt as _;
        std::fs::metadata(home.path()).expect("stat").uid()
    };
    assert!(
        read_token_file(&planted, uid).is_ok(),
        "the planted file really is eligible on its own merits — which is \
         exactly why the containment bound has to be what refuses it"
    );
    let outside = GxRecord {
        token_file: planted.display().to_string(),
        ..inside.clone()
    };
    assert_eq!(
        token_path_for(&outside, home.path()),
        Err(TokenRefusal::OutsideHome),
    );

    // `..` traversal that escapes, spelled to defeat a string-prefix test.
    let traversal = GxRecord {
        token_file: format!(
            "{}/../{}/stolen.token",
            home.path().display(),
            elsewhere.path().file_name().unwrap().to_string_lossy()
        ),
        ..inside.clone()
    };
    assert_eq!(
        token_path_for(&traversal, home.path()),
        Err(TokenRefusal::OutsideHome),
        "canonicalized comparison, not a string prefix",
    );

    // A sibling whose name merely STARTS with the home's — the case a string
    // prefix would wave through and `Path::starts_with` refuses.
    let sibling = std::path::PathBuf::from(format!("{}-evil", home.path().display()));
    std::fs::create_dir_all(&sibling).expect("sibling dir");
    let sibling_token = sibling.join("gx-remote.token");
    std::fs::write(&sibling_token, format!("{SENTINEL}\n")).expect("write");
    let sibling_rec = GxRecord {
        token_file: sibling_token.display().to_string(),
        ..inside
    };
    assert_eq!(
        token_path_for(&sibling_rec, home.path()),
        Err(TokenRefusal::OutsideHome),
    );
    let _ = std::fs::remove_dir_all(&sibling);
}

#[test]
fn gx_home_prefers_the_explicit_override_then_the_env_then_the_default() {
    let explicit = Path::new("/tmp/harness-home");
    let env = Path::new("/opt/grok");
    let home = Path::new("/home/u");

    // The test-mode override wins outright, so a hermetic harness cannot read
    // the developer's real ~/.grok by accident.
    assert_eq!(gx_home(Some(explicit), Some(env), home), explicit);
    assert_eq!(gx_home(None, Some(env), home), env);
    assert_eq!(gx_home(None, None, home), PathBuf::from("/home/u/.grok"));
}

// ---------------------------------------------------------------------------
// the static source
// ---------------------------------------------------------------------------

#[tokio::test]
async fn static_credentials_answer_whatever_url_they_are_asked_about() {
    let creds = StaticCredentials::from_parts(SENTINEL, "inst-1").expect("a valid token");
    let got = creds
        .discover("http://127.0.0.1:2431")
        .await
        .expect("static credentials never fail");
    assert_eq!(got.instance_id, "inst-1");
    assert_eq!(got.token.expose(), SENTINEL);

    assert!(
        StaticCredentials::from_parts("nope", "inst-1").is_none(),
        "a fixture that plants a bad token fails at construction"
    );
}

#[test]
fn the_probe_script_carries_no_single_quote() {
    // It crosses SSH as one shell-quoted argument. A quote survives the
    // quoter, but a golden that pins this string — the `gx-probe` scenario in
    // tests/machine-transport, and shed-mobile's Dart composer after it — is
    // worth keeping legible.
    assert!(!PROBE_SCRIPT.contains('\''), "{PROBE_SCRIPT}");
    // The properties the script's shape depends on — and the two delimiters
    // the PARSER spells as constants, asserted here because the script
    // hardcodes them and a drift between the two would be silent.
    assert!(PROBE_SCRIPT.contains("gx-remote*.json"));
    assert!(PROBE_SCRIPT.contains("-type f -perm 0600 -user"));
    assert!(PROBE_SCRIPT.contains(DEFAULT_TOKEN_FILE));
    assert!(PROBE_SCRIPT.contains(&format!("{PROBE_RECORD_DELIMITER}\\n")));
    assert!(PROBE_SCRIPT.contains(&format!("{PROBE_TOKEN_SENTINEL}\\n")));
    assert!(PROBE_SCRIPT.trim_end().ends_with("exit 0"));
}

#[test]
fn local_discovery_reads_the_record_and_its_token_without_shelling_out() {
    let home = TempHome::new("local-discovery");
    let uid = {
        use std::os::unix::fs::MetadataExt as _;
        std::fs::metadata(home.path()).expect("stat").uid()
    };
    // The real pairing: a SUFFIXED record naming an UNSUFFIXED token.
    let token_path = home.path().join(DEFAULT_TOKEN_FILE);
    home.write(
        "gx-remote-52e273e2a1d25203.json",
        &record_json(
            "http://127.0.0.1:2431",
            "facade00facade00facade00facade00",
            &token_path.display().to_string(),
        ),
    );
    // A second leader on this home, on a different port.
    home.write(
        "gx-remote.json",
        &record_json("http://127.0.0.1:2421", "the-other-one", ""),
    );
    home.write_token(DEFAULT_TOKEN_FILE, &format!("{SENTINEL}\n"), 0o600);

    let got = local_discovery(home.path(), "http://127.0.0.1:2431", uid).expect("discovers");
    assert_eq!(got.instance_id, "facade00facade00facade00facade00");
    assert_eq!(got.token.expose(), SENTINEL);

    // The other record's URL resolves to the other leader, off the SAME token.
    let other = local_discovery(home.path(), "http://127.0.0.1:2421", uid).expect("discovers");
    assert_eq!(other.instance_id, "the-other-one");
    assert_eq!(other.token.expose(), SENTINEL, "one token per $GROK_HOME");

    // A URL no record names, and an ineligible token: both quiet, and neither
    // message carries a path or the token.
    for err in [
        local_discovery(home.path(), "http://127.0.0.1:9999", uid).expect_err("no record"),
        local_discovery(home.path(), "http://127.0.0.1:2431", uid + 1).expect_err("not our token"),
    ] {
        assert!(matches!(err, LaneError::Unavailable(_)), "{err:?}");
        let rendered = format!("{err}{err:?}");
        assert!(!rendered.contains(SENTINEL), "leaked: {rendered}");
        assert!(
            !rendered.contains(&home.path().display().to_string()),
            "names no path: {rendered}"
        );
    }
}
