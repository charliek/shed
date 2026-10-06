//! `shed-craze-lane` — a tiny CLI over the agent-lane contract for driving
//! THIS machine's craze hub by hand (or a phone-style loopback port), and the
//! smallest proof the contract is usable from outside this crate: past
//! building the source it calls nothing but [`shed_core::lane::AgentSource`].
//!
//! ```text
//! shed-craze-lane [--port N] <verb> [args]
//!
//!   sessions                                   the hub's roster (the source's first seed)
//!   watch                                      the live roster until ^C
//!   options                                    what a create can start (sessions.createOptions)
//!   create <cwd> [--provider P] [--prompt T] [--request-id ID]
//!                                              start a session (a fresh request id unless given)
//!   probe                                      the find-only probe (never starts a hub)
//! ```
//!
//! By default it dials `/bin/sh -c '<craze's ladder> bridge --hub'` with this
//! process's environment — exactly the desktop's localhost dial — so `sessions`
//! STARTS a hub when none runs (`probe` never does). `--port N` dials
//! `127.0.0.1:N` instead (a tunnel to an `ssh … bridge --hub`, the phone's
//! shape). The session verbs (watch a transcript, send, answer) arrive with the
//! lane in plan 025 C8.

use std::process::ExitCode;
use std::sync::Arc;

use shed_core::lane::{AgentSource, LaneCreateRequest, SourceEvent};
use shed_craze::{
    new_request_id, probe_process, CrazeDial, CrazeSource, EnvPolicy, Probe, ProcessDial, TcpDial,
};

const USAGE: &str = "\
usage: shed-craze-lane [--port N] <verb> [args]

verbs:
  sessions                                   the hub's roster (the source's first seed)
  watch                                      the live roster until ^C
  options                                    what a create can start
  create <cwd> [--provider P] [--prompt T] [--request-id ID]
                                             start a session
  probe                                      the find-only probe (never starts a hub)

Without --port it runs craze's ladder locally with this environment, so
`sessions` starts a hub when none runs.
";

#[tokio::main]
async fn main() -> ExitCode {
    match run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(message) => {
            eprintln!("shed-craze-lane: {message}");
            ExitCode::FAILURE
        }
    }
}

fn flag(args: &[String], name: &str) -> Option<String> {
    args.iter()
        .position(|a| a == name)
        .and_then(|i| args.get(i + 1))
        .cloned()
}

async fn run() -> Result<(), String> {
    let mut args: Vec<String> = std::env::args().skip(1).collect();
    let port = match args.iter().position(|a| a == "--port") {
        Some(i) => {
            let p = args
                .get(i + 1)
                .ok_or_else(|| format!("--port needs a number\n\n{USAGE}"))?
                .parse::<u16>()
                .map_err(|e| format!("--port: {e}"))?;
            args.drain(i..=i + 1);
            Some(p)
        }
        None => None,
    };
    let verb = args
        .first()
        .ok_or_else(|| format!("no verb\n\n{USAGE}"))?
        .clone();
    let dial: Arc<dyn CrazeDial> = match port {
        Some(p) => Arc::new(TcpDial(p)),
        None => Arc::new(ProcessDial::bridge_hub(EnvPolicy::Inherit)),
    };
    let source = CrazeSource::new(dial, "shed-craze-lane");

    match verb.as_str() {
        "sessions" | "watch" => {
            // Both halves stay bound while the stream is read (see
            // `Subscription`).
            let (mut rx, _stop) = source
                .subscribe()
                .await
                .map_err(|e| e.to_string())?
                .into_parts();
            let once = verb == "sessions";
            while let Some(event) = rx.recv().await {
                match event {
                    SourceEvent::Reset { reason, generation } => {
                        println!("--- reset ({reason}) generation {generation}")
                    }
                    SourceEvent::Session { session: s } => println!(
                        "{}  {:<14} {:>2} waiting  {:<8} {}  {}",
                        s.id,
                        s.activity.as_str(),
                        s.pending_approvals,
                        s.provider.as_deref().unwrap_or("-"),
                        s.cwd,
                        s.doing.as_deref().unwrap_or(&s.title)
                    ),
                    SourceEvent::Removed { session_id } => println!("{session_id}  (gone)"),
                    SourceEvent::Capabilities { capabilities } => println!(
                        "+++ create={} create_options={}",
                        capabilities.create, capabilities.create_options
                    ),
                    SourceEvent::Ready { truncated, .. } => {
                        if truncated {
                            println!("… (truncated at craze's 512 rows)");
                        }
                        if once {
                            break;
                        }
                        println!("--- ready");
                    }
                    SourceEvent::Offline { reason, cause } => {
                        if once {
                            return Err(format!("craze is offline ({}): {reason}", cause.as_str()));
                        }
                        println!("--- offline ({}): {reason}", cause.as_str());
                    }
                    SourceEvent::Unknown => {}
                }
            }
        }
        "options" => {
            let o = source.create_options().await.map_err(|e| e.to_string())?;
            for p in &o.providers {
                println!(
                    "{:<8} {:<12} {}{}",
                    p.id,
                    p.state.as_str(),
                    p.reason.as_deref().unwrap_or(""),
                    p.fix
                        .as_deref()
                        .map(|f| format!(" — {f}"))
                        .unwrap_or_default()
                );
            }
            println!("default: {}", o.default_provider.as_deref().unwrap_or("-"));
            for d in &o.recent_dirs {
                println!("recent:  {d}");
            }
        }
        "create" => {
            let cwd = args
                .get(1)
                .filter(|a| !a.starts_with("--"))
                .ok_or_else(|| format!("create needs a directory\n\n{USAGE}"))?
                .clone();
            let request_id = flag(&args, "--request-id").unwrap_or_else(new_request_id);
            let created = source
                .create(LaneCreateRequest {
                    cwd,
                    provider: flag(&args, "--provider"),
                    prompt: flag(&args, "--prompt"),
                    request_id: request_id.clone(),
                })
                .await
                .map_err(|e| {
                    if shed_craze::is_outcome_unknown(&e) {
                        format!("{e} (request id {request_id})")
                    } else {
                        e.to_string()
                    }
                })?;
            println!(
                "{}  {}  prompt {}{}",
                created.session.id,
                created.session.cwd,
                created.prompt.as_str(),
                created
                    .prompt_error
                    .map(|e| format!(": {e}"))
                    .unwrap_or_default()
            );
        }
        "probe" => {
            if port.is_some() {
                return Err("probe runs locally; it has no --port form".to_string());
            }
            match probe_process(shed_core::craze::providers_hub_argv(), EnvPolicy::Inherit).await {
                Probe::Hub => println!("a hub is running"),
                Probe::NoHub => println!("craze is installed; no hub is running"),
                Probe::Offline { cause, reason } => {
                    println!("offline ({}): {reason}", cause.as_str())
                }
            }
        }
        other => return Err(format!("unknown verb {other:?}\n\n{USAGE}")),
    }
    Ok(())
}
