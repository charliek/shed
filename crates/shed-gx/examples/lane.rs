//! `shed-gx-lane` — a tiny CLI over [`shed_core::lane::AgentLane`], for driving
//! a real gx leader by hand.
//!
//! It exists for three reasons: it is the manual verification tool plan 017 §8
//! drives a roost tab's gx session with; it is the smallest possible proof that
//! the contract is usable from outside this crate (past construction it calls
//! nothing but `AgentLane`); and it is the one place the **whole credential
//! seam** is exercised end to end by a human — the local file reader, the SSH
//! probe, and the two-URL split between what a record REPORTS and where HTTP
//! actually goes.
//!
//! ```text
//! cargo run -p shed-gx --example lane -- --session <id> watch
//! cargo run -p shed-gx --example lane -- --ssh mini3 --dial http://127.0.0.1:9431 sessions
//! ```
//!
//! # The two URLs, on the command line
//!
//! `--url` is the **reported** one: what a discovery record is matched against,
//! and what roost stamps on a tab as `gx.remote`. `--dial` is where requests
//! go. On this machine they are the same and `--dial` is unnecessary. Over an
//! `ssh -N -L` forward they differ, and conflating them is exactly the bug the
//! split exists to make unrepresentable — so this tool keeps them as two flags
//! rather than one.

use std::process::ExitCode;
use std::sync::Arc;

use shed_core::lane::{AgentLane, LaneAnswer, LaneDecision, LaneEvent, SendMode};
use shed_gx::discovery::{
    parse_probe, GxDiscovery, GxRecord, GxToken, StaticCredentials, DEFAULT_GROK_HOME, PROBE_SCRIPT,
};
use shed_gx::transport::FixedDial;
use shed_gx::{GxClient, GxTimings};

const USAGE: &str = "\
usage: shed-gx-lane [options] <verb> [args]

options:
  --session <id>        the session most verbs act on
  --url <reported-url>  the lane's REPORTED url (default: whichever record looks newest)
  --dial <url>          where HTTP actually goes (default: the reported url)
  --grok-home <dir>     where records live (default: $GROK_HOME, else ~/.grok)
  --ssh <host>          discover over ssh with the POSIX probe instead of locally
  --token-file <path>   read the token from here and skip discovery entirely
  --instance <id>       the instanceId to pin (required with --token-file)

verbs:
  sessions                          every session the leader knows
  session                           one session's row
  history [--cursor <event-id>]     the transcript, refolded
  watch                             the live stream until ^C
  send <text>                       queue a prompt
  interject <text>                  interrupt the turn in flight
  cancel                            stop the turn in flight
  approvals                         what is waiting on you
  answer <id> <allow-once|allow-always|reject>
  answer <id> --choice <option-id>  the option gx offered, by its own id
";

#[tokio::main]
async fn main() -> ExitCode {
    match run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(message) => {
            eprintln!("shed-gx-lane: {message}");
            ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<(), String> {
    let args = Args::parse(std::env::args().skip(1).collect())?;
    let lane = build(&args).await?;
    let session = || -> Result<&str, String> {
        args.session
            .as_deref()
            .ok_or_else(|| format!("this verb needs --session\n\n{USAGE}"))
    };

    match args.verb.as_str() {
        "sessions" => {
            for s in lane.sessions().await.map_err(|e| e.to_string())? {
                println!(
                    "{}  {:<13} {:>2} waiting  {:<5} {}  {}",
                    s.id,
                    s.activity.as_str(),
                    s.pending_approvals,
                    if s.approximate { "~" } else { "exact" },
                    s.cwd,
                    s.title
                );
            }
        }
        "session" => {
            let s = lane.session(session()?).await.map_err(|e| e.to_string())?;
            println!("{s:#?}");
        }
        "history" => {
            let page = lane
                .history(session()?, args.cursor.as_deref(), 200)
                .await
                .map_err(|e| e.to_string())?;
            for m in &page.messages {
                print_row(m);
            }
            if page.truncated {
                println!("… (truncated: this is a tail, not the whole history)");
            }
            if let Some(cursor) = &page.cursor {
                println!("--- cursor {cursor}");
            }
        }
        "watch" => {
            // BOTH halves are kept alive: dropping `stop` aborts the pump and
            // the receiver then yields nothing forever. See `LaneSubscription`.
            let (mut rx, _stop) = lane
                .subscribe(session()?, None)
                .await
                .map_err(|e| e.to_string())?
                .into_parts();
            // A readiness line on stdout, so a driving script can wait for the
            // watcher to be up instead of sleeping and hoping.
            println!("--- watching {}", session()?);
            while let Some(event) = rx.recv().await {
                print_event(&event);
                if matches!(event, LaneEvent::Down { .. }) {
                    break; // a Down ENDS the subscription
                }
            }
        }
        verb @ ("send" | "interject") => {
            let text = args
                .rest
                .first()
                .ok_or_else(|| format!("{verb} needs text\n\n{USAGE}"))?;
            let mode = if verb == "send" {
                SendMode::Queue
            } else {
                SendMode::Interject
            };
            lane.send(session()?, text, mode)
                .await
                .map_err(|e| e.to_string())?;
            println!(
                "{}",
                if verb == "send" {
                    "sent"
                } else {
                    "interjected"
                }
            );
        }
        "cancel" => {
            lane.cancel(session()?).await.map_err(|e| e.to_string())?;
            println!("cancelled");
        }
        "approvals" => {
            for a in lane
                .approvals(session()?)
                .await
                .map_err(|e| e.to_string())?
            {
                println!(
                    "{}  {:<16} {:<10} {}",
                    a.id,
                    a.kind.as_str(),
                    a.status.as_str(),
                    a.title
                );
                if let Some(detail) = &a.detail {
                    println!("      {detail}");
                }
                for o in &a.options {
                    // The id and the kind are printed SEPARATELY on purpose:
                    // an `optionId` is whatever the agent called it, and
                    // nothing here or in the adapter parses one.
                    println!(
                        "      [{}] {}   (kind: {})",
                        o.id,
                        o.label,
                        o.kind.as_deref().unwrap_or("—")
                    );
                }
                for q in &a.questions {
                    let options: Vec<&str> = q.options.iter().map(|o| o.label.as_str()).collect();
                    println!("      ? {}  {options:?}", q.question);
                }
            }
        }
        "answer" => {
            let id = args
                .rest
                .first()
                .ok_or_else(|| format!("answer needs an approval id\n\n{USAGE}"))?;
            let answer = match (&args.choice, args.rest.get(1).map(String::as_str)) {
                (Some(option_id), _) => LaneAnswer::Choice {
                    option_id: option_id.clone(),
                },
                (None, Some("allow-once")) => LaneAnswer::Permission {
                    decision: LaneDecision::AllowOnce,
                },
                (None, Some("allow-always")) => LaneAnswer::Permission {
                    decision: LaneDecision::AllowAlways,
                },
                (None, Some("reject")) => LaneAnswer::Reject,
                (None, Some(other)) => {
                    return Err(format!("unknown decision {other:?}\n\n{USAGE}"))
                }
                (None, None) => return Err(format!("answer needs a decision\n\n{USAGE}")),
            };
            lane.answer(session()?, id, answer)
                .await
                .map_err(|e| e.to_string())?;
            println!("answered");
        }
        other => return Err(format!("unknown verb {other:?}\n\n{USAGE}")),
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// building the client
// ---------------------------------------------------------------------------

async fn build(args: &Args) -> Result<GxClient, String> {
    let (reported, discovery) = credentials(args)?;
    let dial = args.dial.clone().unwrap_or_else(|| reported.clone());
    let transport = FixedDial::parse(&dial).map_err(|e| e.to_string())?;
    GxClient::new(
        reported,
        Arc::new(transport),
        Arc::new(StaticCredentials::new(discovery)),
        GxTimings::default(),
    )
    .map_err(|e| e.to_string())
}

/// The three ways a human can hand this tool a credential, in the order the
/// flags say.
fn credentials(args: &Args) -> Result<(String, GxDiscovery), String> {
    // 1. Explicit: a token file and the instance it belongs to. No discovery at
    //    all, which is what makes it usable against something this tool cannot
    //    see the records of.
    if let Some(path) = &args.token_file {
        let url = args
            .url
            .clone()
            .ok_or("--token-file needs --url: there is no record to read one from")?;
        let instance = args
            .instance
            .clone()
            .ok_or("--token-file needs --instance: a token alone does not say WHICH leader")?;
        let raw = std::fs::read_to_string(path).map_err(|e| format!("reading {path}: {e}"))?;
        let token = GxToken::parse(&raw).ok_or("that file does not hold a gx token")?;
        return Ok((
            url,
            GxDiscovery {
                token,
                instance_id: instance,
            },
        ));
    }

    // 2. Over SSH: the one POSIX probe string, run as one re-parsed command,
    //    exactly as the desktop runs it through the machine reach.
    if let Some(host) = &args.ssh {
        let out = std::process::Command::new("ssh")
            .arg(host)
            .arg("sh")
            .arg("-c")
            .arg(PROBE_SCRIPT)
            .output()
            .map_err(|e| format!("running the probe on {host}: {e}"))?;
        // The script always exits 0 and reports in band, so a non-zero status
        // is ssh's own failure and its stderr is the useful thing.
        if !out.status.success() {
            return Err(format!(
                "ssh {host} failed: {}",
                String::from_utf8_lossy(&out.stderr).trim()
            ));
        }
        let probe = parse_probe(&String::from_utf8_lossy(&out.stdout))
            .map_err(|e| format!("parsing the probe output from {host}: {e}"))?;
        let token = probe
            .token
            .ok_or_else(|| format!("{host} withheld the token: check its mode and owner"))?;
        let record = choose(&probe.records, args.url.as_deref())?;
        return Ok((
            record.url.clone(),
            GxDiscovery {
                token,
                instance_id: record.instance_id.clone(),
            },
        ));
    }

    // 3. Locally: read the record and the token file directly, with the same
    //    checks gx's own reader makes. Never by shelling out.
    let home = args.grok_home.clone().unwrap_or_else(default_grok_home);
    let records = local_records(&home);
    let record = choose(&records, args.url.as_deref())?;
    let url = record.url.clone();
    let discovery = shed_gx::local_discovery(std::path::Path::new(&home), &url, caller_uid()?)
        .map_err(|e| e.to_string())?;
    Ok((url, discovery))
}

/// The record to use: the one `--url` names, else the most recently started.
fn choose<'a>(records: &'a [GxRecord], url: Option<&str>) -> Result<&'a GxRecord, String> {
    match url {
        Some(url) => records
            .iter()
            .find(|r| r.url == url)
            .ok_or_else(|| format!("no discovery record reports {url}")),
        None => records
            .iter()
            .filter(|r| !r.url.is_empty())
            .max_by_key(|r| r.started_at)
            .ok_or_else(|| "no gx discovery record found; pass --url".to_string()),
    }
}

fn local_records(home: &str) -> Vec<GxRecord> {
    shed_gx::discovery::record_files(std::path::Path::new(home))
        .iter()
        .filter_map(|p| std::fs::read_to_string(p).ok())
        .filter_map(|s| serde_json::from_str::<GxRecord>(&s).ok())
        .collect()
}

fn default_grok_home() -> String {
    match std::env::var("GROK_HOME") {
        Ok(h) if !h.is_empty() => h,
        _ => format!(
            "{}/{DEFAULT_GROK_HOME}",
            std::env::var("HOME").unwrap_or_else(|_| ".".to_string())
        ),
    }
}

/// The caller's effective uid, for the token file's owner check — read back off
/// a file this process just created, because nothing in `std` reports it and
/// this crate does not take a `libc` dependency.
///
/// **A failure is propagated, never defaulted.** This value feeds
/// `local_discovery`'s owner check, so answering `0` on an unwritable `TMPDIR`
/// would silently claim the caller is ROOT — and a root-owned token file that
/// must be refused for a uid-1000 caller would sail through. A security check
/// may fail closed; it may not fail *privileged*.
fn caller_uid() -> Result<u32, String> {
    use std::os::unix::fs::MetadataExt as _;
    let probe = std::env::temp_dir().join(format!("shed-gx-lane-uid-{}", std::process::id()));
    let uid = std::fs::write(&probe, b"")
        .and_then(|()| std::fs::metadata(&probe))
        .map(|m| m.uid())
        .map_err(|e| {
            format!(
                "cannot determine this process's uid (probing {}): {e}",
                probe.display()
            )
        });
    let _ = std::fs::remove_file(&probe);
    uid
}

// ---------------------------------------------------------------------------
// arguments
// ---------------------------------------------------------------------------

#[derive(Debug, Default)]
struct Args {
    verb: String,
    rest: Vec<String>,
    session: Option<String>,
    url: Option<String>,
    dial: Option<String>,
    grok_home: Option<String>,
    ssh: Option<String>,
    token_file: Option<String>,
    instance: Option<String>,
    cursor: Option<String>,
    choice: Option<String>,
}

impl Args {
    fn parse(argv: Vec<String>) -> Result<Args, String> {
        let mut out = Args::default();
        let mut it = argv.into_iter();
        while let Some(arg) = it.next() {
            let mut value = |name: &str| -> Result<String, String> {
                it.next().ok_or_else(|| format!("{name} needs a value"))
            };
            match arg.as_str() {
                "--session" => out.session = Some(value("--session")?),
                "--url" => out.url = Some(value("--url")?),
                "--dial" => out.dial = Some(value("--dial")?),
                "--grok-home" => out.grok_home = Some(value("--grok-home")?),
                "--ssh" => out.ssh = Some(value("--ssh")?),
                "--token-file" => out.token_file = Some(value("--token-file")?),
                "--instance" => out.instance = Some(value("--instance")?),
                "--cursor" => out.cursor = Some(value("--cursor")?),
                "--choice" => out.choice = Some(value("--choice")?),
                "-h" | "--help" => return Err(USAGE.to_string()),
                other if other.starts_with("--") => {
                    return Err(format!("unknown option {other}\n\n{USAGE}"))
                }
                other if out.verb.is_empty() => out.verb = other.to_string(),
                other => out.rest.push(other.to_string()),
            }
        }
        if out.verb.is_empty() {
            return Err(format!("no verb\n\n{USAGE}"));
        }
        Ok(out)
    }
}

// ---------------------------------------------------------------------------
// printing
// ---------------------------------------------------------------------------

fn print_row(m: &shed_core::rc::RcFeedMessage) {
    let detail = m
        .tool
        .as_ref()
        .and_then(|t| t.detail.clone())
        .unwrap_or_default();
    println!(
        "#{:<4} {:<9} {:<16} {}{}",
        m.seq,
        m.role,
        m.msg_type,
        m.text
            .as_deref()
            .unwrap_or_default()
            .replace('\n', "\n           "),
        if detail.is_empty() {
            String::new()
        } else {
            format!("[{detail}]")
        }
    );
}

fn print_event(event: &LaneEvent) {
    match event {
        LaneEvent::Reset { reason, generation } => {
            println!("--- reset ({reason}) generation {generation}: stage from here");
        }
        LaneEvent::Ready { generation } => {
            println!("--- ready generation {generation}: swap the staged view in");
        }
        LaneEvent::Message { message, cursor } => {
            print_row(message);
            if let Some(cursor) = cursor {
                println!("           cursor {cursor}");
            }
        }
        LaneEvent::Session { session } => println!(
            "=== {} [{}] {} waiting  {}",
            session.id,
            session.activity.as_str(),
            session.pending_approvals,
            session.title
        ),
        LaneEvent::Approval { approval } => println!(
            "!!! {}  {:<16} {:<10} {}",
            approval.id,
            approval.kind.as_str(),
            approval.status.as_str(),
            approval.title
        ),
        LaneEvent::Down { reason } => println!("--- down ({reason}): the subscription ended"),
        LaneEvent::Unknown => println!("--- an event kind this build does not know (ignored)"),
    }
}
