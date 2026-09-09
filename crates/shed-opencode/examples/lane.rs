//! `shed-opencode-lane` — a tiny CLI over [`shed_core::lane::AgentLane`], for
//! driving a real opencode server by hand.
//!
//! It exists for two reasons: it is the manual verification tool plan 015 §8
//! drives a roost tab's session with, and it is the smallest possible proof
//! that the contract is usable from outside this crate (it calls nothing but
//! `AgentLane`).
//!
//! ```text
//! shed-opencode-lane <base_url> <session_id> <verb> [args]
//!
//!   sessions                                     every root session on the server
//!   history                                      the transcript, refolded from the top
//!   watch                                        the live stream until ^C
//!   send <text>                                  queue a prompt
//!   cancel                                       stop the turn in flight
//!   approvals                                    what is waiting on you
//!   answer <id> allow-once|allow-always|reject   answer one approval
//!
//! `sessions` takes no session, so pass `-` in that slot.
//! `OPENCODE_SERVER_PASSWORD` (username `opencode`) is honored if set.
//! ```

use std::process::ExitCode;

use shed_core::lane::{AgentLane, LaneAnswer, LaneDecision, LaneEvent, SendMode};
use shed_opencode::{BasicAuth, OpencodeClient};

const USAGE: &str = "\
usage: shed-opencode-lane <base_url> <session_id> <verb> [args]

verbs:
  sessions                                     every root session on the server
  history                                      the transcript, refolded from the top
  watch                                        the live stream until ^C
  send <text>                                  queue a prompt
  cancel                                       stop the turn in flight
  approvals                                    what is waiting on you
  answer <id> allow-once|allow-always|reject   answer one approval

`sessions` takes no session id — pass `-` in that slot.
OPENCODE_SERVER_PASSWORD (username `opencode`) is honored if set.
";

#[tokio::main]
async fn main() -> ExitCode {
    match run().await {
        Ok(()) => ExitCode::SUCCESS,
        Err(message) => {
            eprintln!("shed-opencode-lane: {message}");
            ExitCode::FAILURE
        }
    }
}

async fn run() -> Result<(), String> {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.len() < 3 {
        return Err(format!("too few arguments\n\n{USAGE}"));
    }
    let base = args[0]
        .parse::<reqwest::Url>()
        .map_err(|e| format!("{}: not a URL ({e})", args[0]))?;
    let session = args[1].as_str();
    let verb = args[2].as_str();

    let auth = std::env::var("OPENCODE_SERVER_PASSWORD")
        .ok()
        .filter(|p| !p.is_empty())
        .map(BasicAuth::password);
    let lane = OpencodeClient::new(base, auth).map_err(|e| e.to_string())?;

    match verb {
        "sessions" => {
            for s in lane.sessions().await.map_err(|e| e.to_string())? {
                println!(
                    "{}  {:<12} {:>2} waiting  {}  {}",
                    s.id,
                    s.activity.as_str(),
                    s.pending_approvals,
                    s.cwd,
                    s.title
                );
            }
        }
        "history" => {
            let page = lane
                .history(session, None, 200)
                .await
                .map_err(|e| e.to_string())?;
            for m in &page.messages {
                println!(
                    "#{:<4} {:<9} {:<16} {}",
                    m.seq,
                    m.role,
                    m.msg_type,
                    m.text.as_deref().unwrap_or_default()
                );
            }
            if page.truncated {
                println!("… (truncated: this is the tail, not the whole history)");
            }
        }
        "watch" => {
            // BOTH halves are kept alive: dropping `stop` aborts the pump, and
            // the receiver then yields nothing forever. See `LaneSubscription`.
            let (mut rx, _stop) = lane
                .subscribe(session, None)
                .await
                .map_err(|e| e.to_string())?
                .into_parts();
            while let Some(event) = rx.recv().await {
                print_event(&event);
                if matches!(event, LaneEvent::Down { .. }) {
                    break; // a Down ENDS the subscription
                }
            }
        }
        "send" => {
            let text = args
                .get(3)
                .ok_or_else(|| format!("send needs text\n\n{USAGE}"))?;
            lane.send(session, text, SendMode::Queue)
                .await
                .map_err(|e| e.to_string())?;
            println!("queued");
        }
        "cancel" => {
            lane.cancel(session).await.map_err(|e| e.to_string())?;
            println!("cancelled");
        }
        "approvals" => {
            for a in lane.approvals(session).await.map_err(|e| e.to_string())? {
                println!(
                    "{}  {:<12} {:<10} {}  [{}]",
                    a.id,
                    a.kind.as_str(),
                    a.status.as_str(),
                    a.title,
                    a.session_id
                );
                for q in &a.questions {
                    let options: Vec<&str> = q.options.iter().map(|o| o.id.as_str()).collect();
                    println!("      {} — {}  {:?}", q.header, q.question, options);
                }
            }
        }
        "answer" => {
            let id = args
                .get(3)
                .ok_or_else(|| format!("answer needs an approval id\n\n{USAGE}"))?;
            let decision = args
                .get(4)
                .ok_or_else(|| format!("answer needs a decision\n\n{USAGE}"))?;
            let decision = match decision.as_str() {
                "allow-once" => LaneDecision::AllowOnce,
                "allow-always" => LaneDecision::AllowAlways,
                "reject" => LaneDecision::Reject,
                other => return Err(format!("unknown decision {other:?}\n\n{USAGE}")),
            };
            lane.answer(session, id, LaneAnswer::Permission { decision })
                .await
                .map_err(|e| e.to_string())?;
            println!("answered");
        }
        other => return Err(format!("unknown verb {other:?}\n\n{USAGE}")),
    }
    Ok(())
}

fn print_event(event: &LaneEvent) {
    match event {
        LaneEvent::Reset { reason, generation } => {
            println!("--- reset ({reason}) generation {generation}: stage from here");
        }
        LaneEvent::Ready { generation } => {
            println!("--- ready generation {generation}: swap the staged view in");
        }
        LaneEvent::Message { message, .. } => println!(
            "#{:<4} {:<9} {:<16} {}",
            message.seq,
            message.role,
            message.msg_type,
            message.text.as_deref().unwrap_or_default()
        ),
        LaneEvent::Session { session } => println!(
            "=== {} [{}] {} waiting  {}",
            session.id,
            session.activity.as_str(),
            session.pending_approvals,
            session.title
        ),
        LaneEvent::Approval { approval } => println!(
            "!!! {}  {:<12} {:<10} {}",
            approval.id,
            approval.kind.as_str(),
            approval.status.as_str(),
            approval.title
        ),
        LaneEvent::Down { reason } => println!("--- down ({reason}): the subscription ended"),
        LaneEvent::Unknown => println!("--- an event kind this build does not know (ignored)"),
    }
}
