//! **The craze ladder's behaviour, run for real** (plan 025 §3.4, C5).
//!
//! `shed_core::craze` composes `sh -c '<ladder>'` and hands it to a transport;
//! these tests run that script with a local `sh` — no sshd, no root — and watch
//! which `craze` it execs, with what arguments, under what `PATH`.
//!
//! **The production tables, re-rooted.** [`rerooted`] maps the production
//! [`LADDER`] (and [`JAILED_LADDER`]) onto a scratch dir: every absolute rung
//! moves under `<root>` (`<root>/opt/homebrew/bin`, …), the `$HOME`-relative
//! ones follow a scratch `HOME`, and `command -v` searches a scratch original
//! `PATH`. So the rung ORDER and the exec-PATH ORDER under test are the
//! production constants' own, and a swap there turns these tests red. The
//! production constants un-rooted run in two places: here, through rung 1 only
//! (a scratch `HOME` needs no root — [`the_production_forms_take_rung_one_with_the_production_exec_path`]),
//! and in `tests/machine-transport`'s Docker recipe, where the container can put
//! stubs at the real `/usr/local/bin` and `/opt/homebrew/bin`.
//!
//! **The expectations are written out here**, from craze's published contract
//! (`docs/reference/protocol.md`, "SSH exec"), never derived from the tables
//! under test — an expectation computed from the table would agree with any
//! reordering of it.
//!
//! Every run is `env -i` with exactly `HOME`, `PATH` and (where a test says)
//! `USER` — plus, in the one test about it, an exported `cz_path` the caller
//! already had. A stub `craze` on a rung records which rung it is, the argv it
//! was exec'd with, the `PATH` it saw, and whether `cz_path` leaked into its
//! environment (it must not: the prologue computes it, never exports it). Each
//! case runs under every distinct POSIX `sh` this host has among `/bin/sh`,
//! `/bin/dash`, `/bin/bash` (as `sh`, so in POSIX mode — macOS's `/bin/sh`) and
//! `/bin/busybox` (as `sh`).
//!
//! **One lock for the whole binary.** Exec'ing a file this process just wrote
//! while another test thread forks is the `ETXTBSY` race
//! `shed_core::roost::testing::write_exec` documents (a forked child briefly
//! holds the writer's descriptor). Every test here takes [`LOCK`] across its
//! stub writes and its runs, and this file is its own test binary so no other
//! suite's threads fork beside it.
#![cfg(unix)]

use std::os::unix::fs::PermissionsExt as _;
use std::os::unix::process::CommandExt as _;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Mutex, MutexGuard};

use shed_core::craze::{
    attach_argv, bridge_hub_argv_jailed, providers_hub_argv, Dir, Ladder, Rung, JAILED_LADDER,
    LADDER,
};
use shed_core::machine::display_line;

static LOCK: Mutex<()> = Mutex::new(());

fn lock() -> MutexGuard<'static, ()> {
    LOCK.lock().unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// The `USER` the per-user Nix rung is exercised with.
const USER: &str = "crazetest";

/// A valid craze host id: twelve lowercase hex digits.
const HOST_ID: &str = "0123456789ab";

/// Where a rung looks, test-side — craze's published order, restated.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Loc {
    HomeLocal,
    Path,
    OptHomebrew,
    UsrLocal,
    Linuxbrew,
    UsrBin,
    NixProfile,
    NixPerUser,
    NixosSystem,
}

/// craze's published ladder order (`protocol.md`, "SSH exec", rungs 1–9).
const ORDER: [Loc; 9] = [
    Loc::HomeLocal,
    Loc::Path,
    Loc::OptHomebrew,
    Loc::UsrLocal,
    Loc::Linuxbrew,
    Loc::UsrBin,
    Loc::NixProfile,
    Loc::NixPerUser,
    Loc::NixosSystem,
];

impl Loc {
    fn label(self) -> &'static str {
        match self {
            Loc::HomeLocal => "home-local-bin",
            Loc::Path => "path",
            Loc::OptHomebrew => "opt-homebrew",
            Loc::UsrLocal => "usr-local",
            Loc::Linuxbrew => "linuxbrew",
            Loc::UsrBin => "usr-bin",
            Loc::NixProfile => "nix-profile",
            Loc::NixPerUser => "nix-per-user",
            Loc::NixosSystem => "nixos-system",
        }
    }
}

/// A scratch tree that removes itself: `root/` (the re-rooted absolute rungs),
/// `home/` (the scratch `HOME`), `orig/` (the original `PATH`'s one directory),
/// `cwd/` (where every shell starts) and the stub's `record` file.
struct Rig {
    dir: PathBuf,
}

impl Rig {
    fn new() -> Rig {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let dir = std::env::temp_dir().join(format!(
            "shed-craze-ladder-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let _ = std::fs::remove_dir_all(&dir);
        for sub in ["root", "home", "orig", "cwd"] {
            std::fs::create_dir_all(dir.join(sub)).expect("creating the scratch tree");
        }
        Rig { dir }
    }

    fn root(&self) -> String {
        path_str(&self.dir.join("root"))
    }

    fn home(&self) -> String {
        path_str(&self.dir.join("home"))
    }

    fn orig(&self) -> String {
        path_str(&self.dir.join("orig"))
    }

    fn cwd(&self) -> PathBuf {
        self.dir.join("cwd")
    }

    fn record_path(&self) -> PathBuf {
        self.dir.join("record")
    }

    /// The `craze` a rung finds — written out per craze's contract, not read
    /// back from the table under test.
    fn at(&self, loc: Loc) -> PathBuf {
        let (root, home) = (self.root(), self.home());
        PathBuf::from(match loc {
            Loc::HomeLocal => format!("{home}/.local/bin/craze"),
            Loc::Path => format!("{}/craze", self.orig()),
            Loc::OptHomebrew => format!("{root}/opt/homebrew/bin/craze"),
            Loc::UsrLocal => format!("{root}/usr/local/bin/craze"),
            Loc::Linuxbrew => format!("{root}/home/linuxbrew/.linuxbrew/bin/craze"),
            Loc::UsrBin => format!("{root}/usr/bin/craze"),
            Loc::NixProfile => format!("{home}/.nix-profile/bin/craze"),
            Loc::NixPerUser => format!("{root}/etc/profiles/per-user/{USER}/bin/craze"),
            Loc::NixosSystem => format!("{root}/run/current-system/sw/bin/craze"),
        })
    }

    /// The enhanced PATH a stub must see: the ladder's directories in the
    /// ladder's order — each under its own guard — then the original `PATH`.
    fn enhanced_path(&self, home_rungs: bool, user: bool, original: &str) -> String {
        let (root, home) = (self.root(), self.home());
        let mut dirs = Vec::new();
        if home_rungs {
            dirs.push(format!("{home}/.local/bin"));
        }
        dirs.push(format!("{root}/opt/homebrew/bin"));
        dirs.push(format!("{root}/usr/local/bin"));
        dirs.push(format!("{root}/home/linuxbrew/.linuxbrew/bin"));
        dirs.push(format!("{root}/usr/bin"));
        if home_rungs {
            dirs.push(format!("{home}/.nix-profile/bin"));
        }
        if user {
            dirs.push(format!("{root}/etc/profiles/per-user/{USER}/bin"));
        }
        dirs.push(format!("{root}/run/current-system/sw/bin"));
        if !original.is_empty() {
            dirs.push(original.to_string());
        }
        dirs.join(":")
    }

    /// A stub `craze` at `path` that records itself (see the module doc).
    fn stub_file(&self, path: &Path, label: &str) {
        std::fs::create_dir_all(path.parent().unwrap()).expect("mkdir for a stub");
        let record = path_str(&self.record_path());
        std::fs::write(
            path,
            format!(
                "#!/bin/sh\n\
                 {{\n\
                 printf 'rung %s\\n' '{label}'\n\
                 printf 'path %s\\n' \"${{PATH-<unset>}}\"\n\
                 printf 'cz_path %s\\n' \"${{cz_path-<unset>}}\"\n\
                 for a in \"$@\"; do printf 'arg %s\\n' \"$a\"; done\n\
                 }} >'{record}'\n"
            ),
        )
        .expect("writing a stub craze");
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o755))
            .expect("chmod +x a stub craze");
    }

    fn stub(&self, loc: Loc) {
        self.stub_file(&self.at(loc), loc.label());
    }

    /// An executable DIRECTORY named `craze` where a rung looks: `[ -x ]`
    /// alone would accept it, and the `exec` would then fail at 126.
    fn exec_dir(&self, loc: Loc) {
        let path = self.at(loc);
        std::fs::create_dir_all(&path).expect("mkdir an executable directory");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755))
            .expect("chmod 0755 a directory");
    }

    /// Run `argv` (`["sh", "-c", <script>]`) under `shell`, `env -i` with
    /// exactly `HOME`, `PATH`, (when `env.user`) `USER` and (when
    /// `env.inherited_cz_path`) an exported `cz_path`, from [`Rig::cwd`] unless
    /// `env.cwd` says otherwise.
    fn run(&self, shell: &Path, argv: &[String], env: &Env) -> Run {
        assert_eq!(argv[0], "sh", "every form runs under sh");
        let _ = std::fs::remove_file(self.record_path());
        let mut cmd = Command::new(shell);
        cmd.arg0("sh")
            .args(&argv[1..])
            .env_clear()
            .current_dir(env.cwd.clone().unwrap_or_else(|| self.cwd()));
        // PATH is always passed — possibly empty — so the shell's own default
        // PATH (which on a dev box reaches a real `craze`) never applies.
        cmd.env("PATH", &env.path);
        cmd.env("HOME", &env.home);
        if env.user {
            cmd.env("USER", USER);
        }
        if let Some(value) = &env.inherited_cz_path {
            cmd.env("cz_path", value);
        }
        let out = cmd.output().expect("spawning a shell");
        Run {
            status: out.status.code(),
            stdout: String::from_utf8_lossy(&out.stdout).into_owned(),
            stderr: String::from_utf8_lossy(&out.stderr).into_owned(),
            record: std::fs::read_to_string(self.record_path())
                .ok()
                .map(|text| Record::parse(&text)),
        }
    }

    /// The ordinary environment: the scratch `HOME`, `USER` set, and the
    /// original `PATH` = `orig/` alone.
    fn env(&self) -> Env {
        Env {
            home: self.home(),
            path: self.orig(),
            user: true,
            cwd: None,
            inherited_cz_path: None,
        }
    }
}

impl Drop for Rig {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

struct Env {
    home: String,
    path: String,
    user: bool,
    cwd: Option<PathBuf>,
    /// A `cz_path` the CALLER's environment already exports — what an
    /// `env -i` run never has, and the one way an assignment-only prologue
    /// would hand the computed value on to craze.
    inherited_cz_path: Option<String>,
}

#[derive(Debug)]
struct Run {
    status: Option<i32>,
    stdout: String,
    stderr: String,
    record: Option<Record>,
}

#[derive(Debug, PartialEq, Eq)]
struct Record {
    rung: String,
    path: String,
    cz_path: String,
    args: Vec<String>,
}

impl Record {
    fn parse(text: &str) -> Record {
        let mut record = Record {
            rung: String::new(),
            path: String::new(),
            cz_path: String::new(),
            args: Vec::new(),
        };
        for line in text.lines() {
            let (key, value) = line.split_once(' ').unwrap_or((line, ""));
            match key {
                "rung" => record.rung = value.to_string(),
                "path" => record.path = value.to_string(),
                "cz_path" => record.cz_path = value.to_string(),
                "arg" => record.args.push(value.to_string()),
                other => panic!("unexpected stub record line {other:?} in {text:?}"),
            }
        }
        record
    }
}

impl Run {
    /// The run exec'd the stub at `loc`, cleanly, with `args`; returns its
    /// record for further checks.
    fn exec_d(&self, loc: Loc, args: &[&str], what: &str) -> &Record {
        let record = self
            .record
            .as_ref()
            .unwrap_or_else(|| panic!("{what}: no craze ran: {self:?}"));
        assert_eq!(
            record.rung,
            loc.label(),
            "{what}: the wrong rung won: {self:?}"
        );
        assert_eq!(record.args, args, "{what}: craze got the wrong arguments");
        assert_eq!(self.status, Some(0), "{what}: {self:?}");
        assert_eq!(self.stderr, "", "{what}: {self:?}");
        assert_eq!(
            record.cz_path, "<unset>",
            "{what}: cz_path must be computed, never exported"
        );
        record
    }

    fn not_found(&self, what: &str) {
        assert!(self.record.is_none(), "{what}: a craze ran: {self:?}");
        assert_eq!(self.status, Some(127), "{what}: {self:?}");
        assert_eq!(
            self.stderr, "craze: command not found\n",
            "{what}: {self:?}"
        );
        assert_eq!(self.stdout, "", "{what}: {self:?}");
    }
}

fn path_str(path: &Path) -> String {
    path.to_str().expect("a UTF-8 scratch path").to_string()
}

fn leak(s: String) -> &'static str {
    Box::leak(s.into_boxed_str())
}

/// `ladder` with every absolute directory moved under `root` — the
/// `$HOME`-relative ones and `command -v` are left as they are (a scratch
/// `HOME` and a scratch `PATH` already jail them).
fn rerooted(ladder: &Ladder<'static>, root: &str) -> Ladder<'static> {
    let dir = |dir: Dir<'static>| match dir {
        Dir::Fixed(path) => Dir::Fixed(leak(format!("{root}{path}"))),
        Dir::PerUser { prefix, suffix } => Dir::PerUser {
            prefix: leak(format!("{root}{prefix}")),
            suffix,
        },
        home @ Dir::Home(_) => home,
    };
    let rungs: Vec<Rung<'static>> = ladder
        .rungs
        .iter()
        .map(|rung| match *rung {
            Rung::At(d) => Rung::At(dir(d)),
            Rung::PathLookup => Rung::PathLookup,
        })
        .collect();
    let exec_path: Vec<Dir<'static>> = ladder.exec_path.iter().map(|d| dir(*d)).collect();
    Ladder {
        rungs: Box::leak(rungs.into_boxed_slice()),
        exec_path: Box::leak(exec_path.into_boxed_slice()),
    }
}

/// The three forms on `ladder`, each with the argv craze must receive.
fn forms(ladder: &Ladder<'_>) -> Vec<(Vec<String>, Vec<&'static str>)> {
    vec![
        (ladder.bridge_hub_argv(), vec!["bridge", "--hub"]),
        (
            ladder.providers_hub_argv(),
            vec!["providers", "--hub", "--json"],
        ),
        (
            ladder.attach_argv(HOST_ID).expect("a valid host id"),
            vec!["attach", "--session", HOST_ID],
        ),
    ]
}

/// Every distinct POSIX `sh` on this host, each run as `sh`.
fn shells() -> Vec<PathBuf> {
    let mut seen = Vec::new();
    let mut shells = Vec::new();
    for candidate in ["/bin/sh", "/bin/dash", "/bin/bash", "/bin/busybox"] {
        let Ok(real) = std::fs::canonicalize(candidate) else {
            continue;
        };
        if !seen.contains(&real) {
            seen.push(real);
            shells.push(PathBuf::from(candidate));
        }
    }
    assert!(!shells.is_empty(), "no /bin/sh on this host");
    shells
}

/// Each rung, in turn, holds a stub — and so does every LATER rung: the one
/// that runs must be the earliest, exec'd with each form's arguments. A
/// rung list in the wrong order runs a later stub; a dropped rung runs the
/// next one.
#[test]
fn every_rung_is_exec_d_with_each_forms_arguments_and_the_earliest_wins() {
    let _guard = lock();
    for shell in shells() {
        for (i, loc) in ORDER.iter().enumerate() {
            let rig = Rig::new();
            for later in &ORDER[i..] {
                rig.stub(*later);
            }
            let ladder = rerooted(&LADDER, &rig.root());
            for (argv, args) in forms(&ladder) {
                let what = format!("{shell:?}, stubs from rung {} ({loc:?}), {args:?}", i + 1);
                rig.run(&shell, &argv, &rig.env())
                    .exec_d(*loc, &args, &what);
            }
        }
    }
}

/// The PATH the exec'd craze sees is the ladder's directories, in the
/// ladder's order, then the original PATH — whichever rung ran. A guard that
/// fails drops only its own entry, and an empty original PATH leaves no empty
/// entry behind (an empty PATH entry means the current directory).
#[test]
fn the_exec_path_is_the_ladders_directories_then_the_original_path() {
    let _guard = lock();
    for shell in shells() {
        let rig = Rig::new();
        let ladder = rerooted(&LADDER, &rig.root());
        // The last rung, so every guard and every rung before it has run.
        rig.stub(Loc::NixosSystem);
        let argv = ladder.bridge_hub_argv();
        let orig = rig.orig();
        let two_entries = format!("{orig}:/nonexistent/second");

        for (what, env, want) in [
            (
                "the ordinary env",
                rig.env(),
                rig.enhanced_path(true, true, &orig),
            ),
            (
                "USER unset",
                Env {
                    user: false,
                    ..rig.env()
                },
                rig.enhanced_path(true, false, &orig),
            ),
            (
                "a two-entry original PATH",
                Env {
                    path: two_entries.clone(),
                    ..rig.env()
                },
                rig.enhanced_path(true, true, &two_entries),
            ),
            (
                "an empty original PATH",
                Env {
                    path: String::new(),
                    ..rig.env()
                },
                rig.enhanced_path(true, true, ""),
            ),
        ] {
            let what = format!("{shell:?}, {what}");
            let run = rig.run(&shell, &argv, &env);
            let record = run.exec_d(Loc::NixosSystem, &["bridge", "--hub"], &what);
            assert_eq!(record.path, want, "{what}: the exec PATH");
            assert!(
                !record.path.split(':').any(str::is_empty),
                "{what}: an empty PATH entry is the current directory: {:?}",
                record.path
            );
        }

        // Rung 1 sees the same PATH as rung 9: the PATH is the exec's, not
        // the rung's.
        rig.stub(Loc::HomeLocal);
        let run = rig.run(&shell, &argv, &rig.env());
        let record = run.exec_d(Loc::HomeLocal, &["bridge", "--hub"], "rung 1");
        assert_eq!(record.path, rig.enhanced_path(true, true, &orig));
    }
}

/// Rung 2 resolves `command -v` against the ORIGINAL PATH: a craze the user
/// put on their own PATH wins over a stub in every directory the exec PATH
/// injects (Homebrew, linuxbrew, Nix …).
#[test]
fn an_original_path_craze_beats_a_stub_in_the_enhanced_dirs() {
    let _guard = lock();
    for shell in shells() {
        let rig = Rig::new();
        for loc in &ORDER[1..] {
            rig.stub(*loc);
        }
        let ladder = rerooted(&LADDER, &rig.root());
        let what = format!("{shell:?}");
        let run = rig.run(&shell, &ladder.bridge_hub_argv(), &rig.env());
        let record = run.exec_d(Loc::Path, &["bridge", "--hub"], &what);
        // …and it is still exec'd with the enhanced PATH.
        assert_eq!(record.path, rig.enhanced_path(true, true, &rig.orig()));
    }
}

/// An executable DIRECTORY named `craze` on any rung is skipped, not exec'd
/// (which would fail at 126 and end the ladder there).
#[test]
fn an_executable_directory_at_a_rung_is_skipped() {
    let _guard = lock();
    for shell in shells() {
        for loc in &ORDER[..ORDER.len() - 1] {
            let rig = Rig::new();
            rig.exec_dir(*loc);
            rig.stub(Loc::NixosSystem);
            let ladder = rerooted(&LADDER, &rig.root());
            let what = format!("{shell:?}, a directory at {loc:?}");
            rig.run(&shell, &ladder.bridge_hub_argv(), &rig.env())
                .exec_d(Loc::NixosSystem, &["bridge", "--hub"], &what);
        }
    }
}

/// A relative or empty `HOME` makes neither `$HOME` rung (nor either `$HOME`
/// exec-PATH entry) relative to wherever the shell started — even with a craze
/// sitting exactly where the relative path would land. (An UNSET `HOME` is not
/// a portable case: bash fills it in from the password database.)
#[test]
fn a_relative_home_never_makes_a_home_rung_relative() {
    let _guard = lock();
    for shell in shells() {
        let rig = Rig::new();
        let cwd = rig.dir.join("relative");
        for rel in [
            ".local/bin/craze",
            ".nix-profile/bin/craze",
            "home/.local/bin/craze",
        ] {
            rig.stub_file(&cwd.join(rel), "relative-home");
        }
        rig.stub(Loc::NixosSystem);
        let ladder = rerooted(&LADDER, &rig.root());
        for home in [".", "home", ""] {
            let what = format!("{shell:?}, HOME={home:?}");
            let env = Env {
                home: home.to_string(),
                cwd: Some(cwd.clone()),
                ..rig.env()
            };
            let run = rig.run(&shell, &ladder.bridge_hub_argv(), &env);
            let record = run.exec_d(Loc::NixosSystem, &["bridge", "--hub"], &what);
            assert_eq!(
                record.path,
                rig.enhanced_path(false, true, &rig.orig()),
                "{what}"
            );
        }
    }
}

/// **A `cz_path` the caller already exports never reaches craze.** Every
/// other test here starts from `env -i`, where the prologue's `cz_path` is
/// born unexported — so their "never exported" check cannot see this case. An
/// inherited `cz_path` keeps its export attribute through a bare `cz_path=`
/// in dash, bash and busybox `sh` alike; the prologue's `unset` is what drops
/// it. The inherited value must not leak into the computed PATH either.
#[test]
fn an_inherited_exported_cz_path_never_reaches_craze() {
    let _guard = lock();
    for shell in shells() {
        let rig = Rig::new();
        rig.stub(Loc::NixosSystem);
        let env = Env {
            inherited_cz_path: Some("/inherited/sentinel".to_string()),
            ..rig.env()
        };
        let want = rig.enhanced_path(true, true, &rig.orig());
        for (argv, args) in forms(&rerooted(&LADDER, &rig.root())) {
            let what = format!("{shell:?}, inherited cz_path, {args:?}");
            let run = rig.run(&shell, &argv, &env);
            // `exec_d` asserts the stub saw no `cz_path` at all.
            let record = run.exec_d(Loc::NixosSystem, &args, &what);
            assert_eq!(record.path, want, "{what}: the exec PATH");
        }

        // The production forms, through rung 1, the same.
        rig.stub(Loc::HomeLocal);
        for (argv, args) in forms(&LADDER) {
            let what = format!("{shell:?}, inherited cz_path, production {args:?}");
            rig.run(&shell, &argv, &env)
                .exec_d(Loc::HomeLocal, &args, &what);
        }
    }
}

/// No craze on any rung: `craze: command not found` on stderr, nothing on
/// stdout, exit 127 — for every form, on the full ladder and the jailed one.
#[test]
fn no_craze_anywhere_is_command_not_found_and_127() {
    let _guard = lock();
    for shell in shells() {
        let rig = Rig::new();
        for ladder in [
            rerooted(&LADDER, &rig.root()),
            rerooted(&JAILED_LADDER, &rig.root()),
        ] {
            for (argv, args) in forms(&ladder) {
                rig.run(&shell, &argv, &rig.env())
                    .not_found(&format!("{shell:?}, {args:?}"));
            }
        }
    }
}

/// **The jailed variant** reaches neither an absolute rung nor the
/// `$HOME/.nix-profile` one, and passes `PATH` through exactly as it was.
#[test]
fn the_jailed_variant_never_reaches_an_absolute_rung_and_passes_path_through() {
    let _guard = lock();
    for shell in shells() {
        let rig = Rig::new();
        let jailed = rerooted(&JAILED_LADDER, &rig.root());
        // Every rung but the two the jail keeps holds a stub.
        for loc in &ORDER[2..] {
            rig.stub(*loc);
        }
        for (argv, args) in forms(&jailed) {
            rig.run(&shell, &argv, &rig.env()).not_found(&format!(
                "{shell:?}, jailed {args:?} with only outside stubs"
            ));
        }

        rig.stub(Loc::Path);
        for (argv, args) in forms(&jailed) {
            let what = format!("{shell:?}, jailed {args:?}, rung 2");
            let run = rig.run(&shell, &argv, &rig.env());
            assert_eq!(
                run.exec_d(Loc::Path, &args, &what).path,
                rig.orig(),
                "{what}"
            );
        }

        rig.stub(Loc::HomeLocal);
        let two_entries = format!("{}:/nonexistent/second", rig.orig());
        for (argv, args) in forms(&jailed) {
            let what = format!("{shell:?}, jailed {args:?}, rung 1");
            let env = Env {
                path: two_entries.clone(),
                ..rig.env()
            };
            let run = rig.run(&shell, &argv, &env);
            assert_eq!(
                run.exec_d(Loc::HomeLocal, &args, &what).path,
                two_entries,
                "{what}"
            );
        }

        // The production jailed form IS this ladder: it has nothing a
        // re-rooting could move, so it holds no absolute rung at all.
        assert_eq!(jailed, JAILED_LADDER);
        assert_eq!(bridge_hub_argv_jailed(), jailed.bridge_hub_argv());
    }
}

/// The production constants themselves, un-rooted, through rung 1 (a scratch
/// `HOME` needs no root): every form reaches craze with the production exec
/// PATH, in the production order. A backstop stub on the original PATH means a
/// broken rung 1 shows up as the wrong rung rather than falling through to a
/// real `/usr/local/bin/craze` on a dev box.
#[test]
fn the_production_forms_take_rung_one_with_the_production_exec_path() {
    let _guard = lock();
    for shell in shells() {
        let rig = Rig::new();
        rig.stub(Loc::HomeLocal);
        rig.stub(Loc::Path);
        let (home, orig) = (rig.home(), rig.orig());
        let want = format!(
            "{home}/.local/bin:/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:\
             /usr/bin:{home}/.nix-profile/bin:/etc/profiles/per-user/{USER}/bin:\
             /run/current-system/sw/bin:{orig}"
        );
        for (argv, args) in forms(&LADDER) {
            let what = format!("{shell:?}, production {args:?}");
            let run = rig.run(&shell, &argv, &rig.env());
            assert_eq!(
                run.exec_d(Loc::HomeLocal, &args, &what).path,
                want,
                "{what}"
            );
        }
        assert_eq!(providers_hub_argv(), LADDER.providers_hub_argv());
    }
}

/// **Open in terminal's argv, raw and displayed.** The raw `-c` element — what
/// roost's `tab.open` runs, no ssh — carries the id in one plain pair of single
/// quotes; the displayed line is `display_line`'s rendering of it, the one an
/// ssh transport sends. Both, run through a real shell, hand craze the same
/// three arguments.
#[test]
fn attach_argv_raw_and_displayed_both_hand_craze_the_same_arguments() {
    let _guard = lock();
    let argv = attach_argv(HOST_ID).expect("a valid host id");
    let raw = &argv[2];
    assert_eq!(
        raw.matches("attach --session '0123456789ab'").count(),
        9,
        "{raw}"
    );
    assert!(
        !raw.contains(r#"'"'"'"#),
        "a display escape in the raw argv: {raw}"
    );
    assert!(
        !raw.contains(r"'\''"),
        "a display escape in the raw argv: {raw}"
    );
    let line = display_line(&argv);
    assert!(line.starts_with("'sh' '-c' '"), "{line}");
    assert!(
        line.contains(r"attach --session '\''0123456789ab'\''"),
        "{line}"
    );

    let want = ["attach", "--session", HOST_ID];
    for shell in shells() {
        let rig = Rig::new();
        rig.stub(Loc::HomeLocal);
        rig.stub(Loc::Path);

        let what = format!("{shell:?}, raw");
        rig.run(&shell, &argv, &rig.env())
            .exec_d(Loc::HomeLocal, &want, &what);

        // The displayed line, parsed by an outer shell the way sshd's login
        // shell parses a remote command: it needs `sh` on its PATH.
        let what = format!("{shell:?}, displayed");
        let displayed = ["sh".to_string(), "-c".to_string(), line.clone()];
        let env = Env {
            path: format!("{}:/usr/bin:/bin", rig.orig()),
            ..rig.env()
        };
        rig.run(&shell, &displayed, &env)
            .exec_d(Loc::HomeLocal, &want, &what);
    }
}
