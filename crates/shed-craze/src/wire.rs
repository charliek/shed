//! Protocol 1 on the wire — the JSON-RPC envelope, its ids and its error
//! object, and the params and results of every method this crate composes,
//! as serde types (craze `docs/reference/protocol.md` at the pinned sha, "PM";
//! the published schema under `docs/reference/protocol/schema/`).
//!
//! **Tolerant of what arrives, strict about what leaves** (PM "Tolerant
//! inbound, strict outbound"): every result and notification type here
//! ignores a member it does not know and defaults one that is absent, and every
//! params type carries exactly the members it names — a host refuses an
//! unknown params field (`-32602`, `unknown_field`), and a swallowed option is
//! worse than a refusal. Tolerance is about VALUES: a known member whose JSON
//! TYPE is wrong fails its decode, and the caller decides what that costs (a
//! roster row's host-written `row` is the one place a bad type keeps the rest —
//! [`RosterRow::from_value`]).
//!
//! **Ids are strings**, counted from `"1"` per connection, exactly as the
//! vendored WIRE fixtures write them, so a request this crate composes is the
//! fixture's `c2s` line field for field (`tests/wire.rs`).
//!
//! This module composes `hello` (the hub's), `sessions.subscribe`,
//! `sessions.createOptions` and `session.create` — the source's half (plan 025
//! C7). The lane's methods (`session.connect`, the host `hello`, `attach`, the
//! verbs) join it with the lane (C8).

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

/// The one protocol version this crate speaks.
pub const PROTOCOL: u32 = 1;
/// The event codec version this crate folds. A `hello` naming another is
/// `TooOld` (PM "Versioning": a client that does not recognize a codec version
/// must not attempt to fold that stream).
pub const CODEC_EVENT: u32 = 1;
/// The snapshot codec version this crate folds (same rule as [`CODEC_EVENT`]).
pub const CODEC_SNAPSHOT: u32 = 1;

/// The longest line this client may WRITE: craze's `inboundLine`, 4 MiB of
/// content (the `\n` not counted). A host discards a longer line and answers
/// it `-32600` with `id: null` — which a client cannot match to the request
/// it lost — so this crate refuses to write one at all.
pub const INBOUND_LINE_MAX: usize = 4 << 20;
/// The longest line this client must READ: craze's `outboundLine`, 16 MiB of
/// content (the `\n`, and a `\r` before it, not counted). Every line a host or
/// the hub writes is bounded under it by construction (PM "Framing"); a longer
/// one is a protocol fault that ends the connection.
pub const OUTBOUND_LINE_MAX: usize = 16 << 20;

/// `hello`'s `client.kind` for every shed client.
pub const CLIENT_KIND: &str = "shed";

/// The methods this crate composes.
pub mod method {
    pub const HELLO: &str = "hello";
    pub const SESSIONS_SUBSCRIBE: &str = "sessions.subscribe";
    pub const SESSIONS_CREATE_OPTIONS: &str = "sessions.createOptions";
    pub const SESSION_CREATE: &str = "session.create";
}

/// The notifications the source reads.
pub mod notify {
    pub const ROSTER: &str = "roster";
    pub const RESET: &str = "reset";
}

/// A roster subscription's `reset` reasons (PM "The hub's roster").
pub mod reset {
    /// A notification's write blocked for 10 s: subscribe again, same
    /// connection.
    pub const SLOW_CONSUMER: &str = "slow_consumer";
    /// The roster's completeness changed (it crossed 512 rows): subscribe
    /// again, same connection, and read `truncated` from the new reply.
    pub const OMITTED: &str = "omitted";
    /// The hub is shutting down and closes the connection: reconnect, and the
    /// new hub's `epoch` reseeds.
    pub const HUB_CLOSING: &str = "hub_closing";
}

/// craze's closed `data.code` set (PM "Codes and retry") — what a client
/// decides from, and the ONLY thing [`crate::errors`] keys on.
pub mod code {
    pub const BAD_REQUEST: &str = "bad_request";
    pub const STALE_VERSION: &str = "stale_version";
    pub const STALE_TURN: &str = "stale_turn";
    pub const UNKNOWN_SESSION: &str = "unknown_session";
    pub const UNKNOWN_ASK: &str = "unknown_ask";
    pub const ALREADY_SUBMITTED: &str = "already_submitted";
    pub const ALREADY_RESOLVED: &str = "already_resolved";
    pub const NOT_ACCEPTING: &str = "not_accepting";
    pub const FOREIGN_TURN: &str = "foreign_turn";
    pub const IN_PROGRESS: &str = "in_progress";
    pub const STALE_MODEL: &str = "stale_model";
    pub const UNAVAILABLE: &str = "unavailable";
    pub const UNSUPPORTED: &str = "unsupported";
    pub const QUEUE_FULL: &str = "queue_full";
    pub const TEXT_TOO_LONG: &str = "text_too_long";
    pub const PROMPT_IN_FLIGHT: &str = "prompt_in_flight";
    pub const PROMPT_CANCELLED: &str = "prompt_cancelled";
    pub const UNKNOWN_ROW: &str = "unknown_row";
    pub const UNKNOWN_COMMAND: &str = "unknown_command";
    pub const UNKNOWN_SUBAGENT: &str = "unknown_subagent";
    pub const ABORTED: &str = "aborted";
    pub const FAILED: &str = "failed";
    pub const INDEX_WRITE: &str = "index_write";
}

/// The two `data.reason`s this crate reads — never to decide an error's
/// [`shed_core::lane::LaneError`] variant (that is `data.code` alone), only
/// for the two places plan 025 pins a reason: a `hello` refused for want of a
/// shared protocol is `TooOld` (§3.3.2), and a create whose session failed to
/// start carries its cause (P14).
pub mod reason {
    pub const PROTOCOL_VERSION: &str = "protocol_version";
    pub const START_FAILED: &str = "start_failed";
}

// ---- outbound ----

/// The envelope of one request line, in the fixtures' member order.
#[derive(Serialize)]
struct RequestLine<'a, P: Serialize> {
    jsonrpc: &'static str,
    id: &'a str,
    method: &'a str,
    params: &'a P,
}

/// One request line, WITHOUT its `\n`: `{"jsonrpc":"2.0","id":…,"method":…,"params":…}`.
pub fn request_line<P: Serialize>(
    id: &str,
    method: &str,
    params: &P,
) -> Result<Vec<u8>, serde_json::Error> {
    serde_json::to_vec(&RequestLine {
        jsonrpc: "2.0",
        id,
        method,
        params,
    })
}

/// `{}` — the params of a method that takes none (`sessions.subscribe`,
/// `sessions.createOptions`). PM: "`params` is always an object".
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize)]
pub struct Empty {}

/// `hello`'s params. Only what protocol 1 defines and shed sets: no `resume`
/// (the hub mints no client ids), no `auth`, no `via` (both reserved).
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct HelloParams {
    pub protocols: Vec<u32>,
    pub client: ClientInfo,
}

impl HelloParams {
    /// Protocol 1, as `client`.
    pub fn new(client: ClientInfo) -> HelloParams {
        HelloParams {
            protocols: vec![PROTOCOL],
            client,
        }
    }
}

/// Who is saying `hello` (`client.kind` is required; `name` and `version` are
/// free-form and logged by nobody, PM "`hello`").
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct ClientInfo {
    pub kind: String,
    pub name: String,
    pub version: String,
}

impl ClientInfo {
    /// `{kind: "shed", name, version: <this crate's version>}` — `name` is the
    /// caller's (`"shed-desktop"`, `"shed-mobile"`), plan 025 §3.3.2.
    pub fn shed(name: &str) -> ClientInfo {
        ClientInfo {
            kind: CLIENT_KIND.to_string(),
            name: name.to_string(),
            version: env!("CARGO_PKG_VERSION").to_string(),
        }
    }
}

/// `session.create`'s params — **only** `cwd`, `prompt?`, `provider?` and
/// `requestId` (plan 025 D6: no model, no effort, no fast, craze's default
/// permission mode), in the fixtures' member order.
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateParams {
    pub cwd: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub prompt: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub provider: Option<String>,
    pub request_id: String,
}

// ---- inbound: the envelope ----

/// One line a peer wrote, sorted: a reply (by its id) or a notification.
#[derive(Debug, Clone, PartialEq)]
pub enum Incoming {
    /// A reply to the request whose id this is: its `result`, or its `error`.
    /// `id` is the JSON value exactly as the peer echoed it — a string or a
    /// number, never converted (PM "The envelope": "echoed back verbatim"), so
    /// a numeric `1` never answers a request whose id was the string `"1"`.
    Reply {
        id: Value,
        outcome: Result<Value, RpcError>,
    },
    /// A notification (no `id`).
    Notification { method: String, params: Value },
}

/// Why a line is not an [`Incoming`].
#[derive(Debug, Clone, PartialEq)]
pub enum LineError {
    /// Not a JSON object at all — a login profile's chatter before craze
    /// speaks (the bounded preamble, plan 025 §3.3.2), or garbage after.
    NotJson,
    /// A JSON object that is not a JSON-RPC message this client can file.
    NotJsonRpc(String),
    /// An error reply with `id: null` — the peer could not read a request's id
    /// (a line too long, or not JSON). It cannot be matched to the request it
    /// lost, so the connection that carries one cannot be trusted further.
    NullId(Box<RpcError>),
}

/// Sort one line (its `\n` and any `\r` already stripped).
///
/// Every message must say `"jsonrpc":"2.0"` (every `s2c` line of the vendored
/// fixtures does); an object that does not is a [`LineError::NotJsonRpc`].
pub fn parse_line(line: &[u8]) -> Result<Incoming, LineError> {
    // Anything but a JSON OBJECT is `NotJson` — a valid JSON value included
    // (a bare `42`, a `[1,2]`), on purpose: before the first reply that is a
    // login profile's noise, which the bounded preamble exists to skip (a
    // profile that prints a number prints valid JSON); after it, `NotJson` is
    // a protocol fault like any other non-object line (`conn.rs`). (A c7 sol
    // review finding, weighed and not adopted.)
    let Ok(Value::Object(mut obj)) = serde_json::from_slice::<Value>(line) else {
        return Err(LineError::NotJson);
    };
    if obj.get("jsonrpc").and_then(Value::as_str) != Some("2.0") {
        return Err(LineError::NotJsonRpc(format!(
            "a message whose jsonrpc is {}, not \"2.0\"",
            obj.get("jsonrpc")
                .map_or_else(|| "absent".to_string(), Value::to_string)
        )));
    }
    // The line is owned: its members are moved out, never copied (a reply's
    // `result` can be the whole roster, up to 16 MiB).
    let outcome = match (obj.remove("result"), obj.remove("error")) {
        (None, None) => None,
        (Some(result), None) => Some(Ok(result)),
        (None, Some(error)) => Some(Err(RpcError::from_value(&error))),
        (Some(_), Some(_)) => {
            return Err(LineError::NotJsonRpc(
                "a reply carrying both result and error".to_string(),
            ))
        }
    };
    if let Some(outcome) = outcome {
        return match obj.remove("id") {
            Some(id @ (Value::String(_) | Value::Number(_))) => Ok(Incoming::Reply { id, outcome }),
            None | Some(Value::Null) => match outcome {
                Err(e) => Err(LineError::NullId(Box::new(e))),
                Ok(_) => Err(LineError::NotJsonRpc("a result with no id".to_string())),
            },
            Some(other) => Err(LineError::NotJsonRpc(format!("a reply id {other}"))),
        };
    }
    match obj.remove("method") {
        Some(Value::String(method)) if !obj.contains_key("id") => Ok(Incoming::Notification {
            method,
            params: obj.remove("params").unwrap_or(Value::Null),
        }),
        // Protocol 1 servers send no requests; a line with both is nothing a
        // client can answer.
        Some(_) => Err(LineError::NotJsonRpc(
            "a request from the server, which protocol 1 never sends".to_string(),
        )),
        None => Err(LineError::NotJsonRpc(
            "an object with no result, error or method".to_string(),
        )),
    }
}

/// A JSON-RPC error object (PM "Errors and retry"), read tolerantly: a member
/// of the wrong type reads as absent rather than losing the whole refusal.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RpcError {
    /// The JSON-RPC integer (`-32000` for every craze-level refusal).
    pub code: i64,
    /// For people; never matched on.
    pub message: String,
    /// `data.code` — the closed set a client decides from.
    pub data_code: Option<String>,
    /// `data.reason` — finer, for wording; never for retry.
    pub reason: Option<String>,
    /// `data.cause` — a wrapped failure's own text (a start failure's).
    pub cause: Option<String>,
    /// `data.result` — a result carried beside the error, compact JSON
    /// (`hello`'s `{supported}` on `protocol_version`).
    pub result: Option<String>,
}

impl RpcError {
    /// Read an `error` member, tolerantly.
    pub fn from_value(v: &Value) -> RpcError {
        let str_at =
            |m: &Map<String, Value>, k: &str| m.get(k).and_then(Value::as_str).map(str::to_string);
        let Value::Object(obj) = v else {
            return RpcError {
                message: v.to_string(),
                ..RpcError::default()
            };
        };
        let data = obj.get("data").and_then(Value::as_object);
        RpcError {
            code: obj.get("code").and_then(Value::as_i64).unwrap_or(0),
            message: str_at(obj, "message").unwrap_or_default(),
            data_code: data.and_then(|d| str_at(d, "code")),
            reason: data.and_then(|d| str_at(d, "reason")),
            cause: data.and_then(|d| str_at(d, "cause")),
            result: data.and_then(|d| d.get("result")).map(Value::to_string),
        }
    }

    /// The message, else the code — never empty, so a caller always has
    /// words to show.
    pub fn text(&self) -> String {
        if !self.message.is_empty() {
            return self.message.clone();
        }
        match &self.data_code {
            Some(code) => code.clone(),
            None => format!("JSON-RPC error {}", self.code),
        }
    }
}

impl std::fmt::Display for RpcError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.text())?;
        if let Some(code) = &self.data_code {
            write!(f, " ({code}")?;
            if let Some(reason) = &self.reason {
                write!(f, "/{reason}")?;
            }
            f.write_str(")")?;
        }
        Ok(())
    }
}

// ---- inbound: results ----

/// `hello`'s result — a hub's or a host's (`endpoint.kind` says which, PM "A
/// host's result", "The hub's result"). Only what this crate reads; every
/// member defaults, so an absent one reads as the older peer's answer.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct HelloResult {
    #[serde(default)]
    pub protocol: u32,
    #[serde(default)]
    pub endpoint: Endpoint,
    #[serde(default)]
    pub capabilities: ConnCapabilities,
    /// Absent reads as no codec this client knows.
    #[serde(default)]
    pub codecs: Option<Codecs>,
}

/// Who answered `hello`.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Endpoint {
    /// `"hub"` or `"host"`.
    #[serde(default)]
    pub kind: String,
    /// The hub's own id — the roster's `epoch` — or the host's.
    #[serde(default)]
    pub host_id: String,
    #[serde(default)]
    pub craze_version: String,
    #[serde(default)]
    pub pid: Option<i64>,
}

/// The CONNECTION's capability set (PM "Capabilities"), distinct from a
/// session's. Every member absent reads `false`: `createOptions` is omitted
/// when false by design, and an older hub lacks the newer ones.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ConnCapabilities {
    #[serde(default)]
    pub roster_subscribe: bool,
    #[serde(default)]
    pub session_create: bool,
    #[serde(default)]
    pub multiplex: bool,
    #[serde(default)]
    pub connect: bool,
    #[serde(default)]
    pub snapshot: bool,
    #[serde(default)]
    pub attach_when_now: bool,
    #[serde(default)]
    pub create_options: bool,
}

/// `hello`'s `codecs`.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Deserialize)]
pub struct Codecs {
    #[serde(default)]
    pub event: u32,
    #[serde(default)]
    pub snapshot: u32,
}

/// `sessions.subscribe`'s result. Rows stay raw JSON here: each is read on its
/// own by [`RosterRow::from_value`], so one bad row costs that row and not the
/// roster.
#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
pub struct SubscribeResult {
    pub subscription: String,
    #[serde(default)]
    pub epoch: String,
    #[serde(default)]
    pub cursor: u64,
    #[serde(default)]
    pub sessions: Vec<Value>,
    /// The roster was cut at 512 rows; absent otherwise.
    #[serde(default)]
    pub truncated: bool,
}

/// A `roster` notification's params: the net change since the cursor the
/// subscriber last had.
#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
pub struct RosterParams {
    #[serde(default)]
    pub subscription: String,
    /// The hub's epoch; when present, it must be the subscription's.
    #[serde(default)]
    pub epoch: Option<String>,
    #[serde(default)]
    pub cursor: u64,
    #[serde(default)]
    pub upserts: Vec<Value>,
    /// The host ids of rows that left.
    #[serde(default)]
    pub removes: Vec<String>,
}

/// A `reset` notification's params.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct ResetParams {
    #[serde(default)]
    pub subscription: String,
    #[serde(default)]
    pub reason: String,
}

/// `sessions.createOptions`' result (PM "`sessions.createOptions`").
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateOptionsResult {
    #[serde(default)]
    pub providers: Vec<ProviderOption>,
    #[serde(default)]
    pub default_provider: Option<String>,
    #[serde(default)]
    pub recent_dirs: Vec<RecentDir>,
}

/// One provider a create can name.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct ProviderOption {
    pub id: String,
    #[serde(default)]
    pub label: String,
    /// `ready` | `needs_setup` | `unavailable` — an open string here; the
    /// contract's tolerant enum reads it.
    #[serde(default)]
    pub state: String,
    #[serde(default)]
    pub reason: Option<String>,
    #[serde(default)]
    pub fix: Option<String>,
}

/// One recent directory.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RecentDir {
    pub dir: String,
    #[serde(default)]
    pub used_at: Option<String>,
}

/// `session.create`'s result: the new session's roster row, and what became of
/// its first prompt.
#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct CreateResult {
    pub session: Value,
    /// `none` | `accepted` | `unknown` | `refused`; absent reads `none`.
    #[serde(default)]
    pub prompt: Option<String>,
    #[serde(default)]
    pub prompt_error: Option<String>,
}

// ---- inbound: the roster row ----

/// One roster row (PM "The hub's roster"): what the hub knows about one host,
/// keyed by `hostId`, and the host's own `sessions.list` row, `row`, which a
/// host of ANY build wrote and the hub passed through untouched.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RosterRow {
    pub host_id: String,
    /// The host's craze session id — what every session call carries.
    pub session_id: String,
    pub host: RosterHost,
    /// `connecting` | `reachable` | `unreachable`.
    pub status: String,
    pub approximate: bool,
    /// The host's row, when there is one that reads.
    pub row: Option<HostRow>,
    /// The host half or the row was there and did not read: what did read is
    /// kept, and the mapped row says `approximate`.
    pub malformed: bool,
}

/// The row's `host` half.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RosterHost {
    #[serde(default)]
    pub pid: Option<i64>,
    #[serde(default)]
    pub craze_version: String,
    #[serde(default)]
    pub protocol: Option<u32>,
    /// The provider's name (`grok`, `cursor`, …); `""` where a host has none.
    #[serde(default)]
    pub provider: String,
    #[serde(default)]
    pub workspace: String,
    #[serde(default)]
    pub started_at: Option<String>,
    #[serde(default)]
    pub ready: bool,
}

/// The host's own `sessions.list` row — the members this crate maps, every one
/// optional (a host without `rowFacts` has none of the facts; one from before
/// a member existed lacks it).
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct HostRow {
    #[serde(default)]
    pub title: Option<String>,
    /// `starting` | `replaying` | `idle` | `working` | `error` | `closing`.
    #[serde(default)]
    pub activity: Option<String>,
    #[serde(default)]
    pub foreign_turn: bool,
    #[serde(default)]
    pub pending_asks: u32,
    #[serde(default)]
    pub head_ask: Option<HeadAsk>,
    #[serde(default)]
    pub doing: Option<String>,
    #[serde(default)]
    pub last_reply: Option<String>,
    #[serde(default)]
    pub since: Option<String>,
    #[serde(default)]
    pub start_failed: bool,
    #[serde(default)]
    pub start_err: Option<String>,
    #[serde(default)]
    pub model: Option<String>,
    #[serde(default)]
    pub attached: Option<u32>,
    #[serde(default)]
    pub provider_session_id: Option<String>,
    /// The info document's `bypass` | `prompt`.
    #[serde(default)]
    pub permission_mode: Option<String>,
}

/// The first open ask, as a row carries it.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
pub struct HeadAsk {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub kind: String,
    #[serde(default)]
    pub label: String,
    #[serde(default)]
    pub summary: Option<String>,
}

/// The members a roster row MUST carry to be a row at all.
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct RosterRowWire {
    host_id: String,
    session_id: String,
    #[serde(default)]
    host: Option<Value>,
    #[serde(default)]
    status: String,
    #[serde(default)]
    approximate: bool,
    #[serde(default)]
    row: Option<Value>,
}

impl RosterRow {
    /// Read one roster row tolerantly. `Err` only when the row has no
    /// readable `hostId`/`sessionId` — it has no key, so there is nothing to
    /// list or remove. A host half or a `row` that does not read keeps
    /// everything else and sets [`RosterRow::malformed`] (plan 025 §3.3.3:
    /// "a malformed `row` keeps the host half and sets `approximate`").
    pub fn from_value(v: &Value) -> Result<RosterRow, String> {
        let wire = RosterRowWire::deserialize(v).map_err(|e| format!("a roster row: {e}"))?;
        if wire.host_id.is_empty() || wire.session_id.is_empty() {
            return Err("a roster row with an empty hostId or sessionId".to_string());
        }
        // The two halves are owned here: each deserializes by value, moving
        // its strings rather than copying them a second time.
        let mut malformed = false;
        let host = match wire.host {
            None => RosterHost::default(),
            Some(h) => RosterHost::deserialize(h).unwrap_or_else(|_| {
                malformed = true;
                RosterHost::default()
            }),
        };
        let row = match wire.row {
            None => None,
            Some(r) => match HostRow::deserialize(r) {
                Ok(row) => Some(row),
                Err(_) => {
                    malformed = true;
                    None
                }
            },
        };
        Ok(RosterRow {
            host_id: wire.host_id,
            session_id: wire.session_id,
            host,
            status: wire.status,
            approximate: wire.approximate,
            row,
            malformed,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn a_request_line_is_the_fixtures_shape() {
        let line = request_line("2", method::SESSIONS_SUBSCRIBE, &Empty {}).unwrap();
        assert_eq!(
            std::str::from_utf8(&line).unwrap(),
            r#"{"jsonrpc":"2.0","id":"2","method":"sessions.subscribe","params":{}}"#
        );
    }

    #[test]
    fn create_params_carry_only_what_is_set() {
        let p = CreateParams {
            cwd: "/w".into(),
            prompt: None,
            provider: None,
            request_id: "r-1".into(),
        };
        assert_eq!(
            serde_json::to_value(&p).unwrap(),
            json!({"cwd": "/w", "requestId": "r-1"})
        );
    }

    #[test]
    fn replies_and_notifications_sort_by_their_members() {
        assert_eq!(
            parse_line(br#"{"jsonrpc":"2.0","id":"7","result":{}}"#).unwrap(),
            Incoming::Reply {
                id: json!("7"),
                outcome: Ok(json!({}))
            }
        );
        // A numeric id stays a number: the id is the JSON value, verbatim.
        assert!(matches!(
            parse_line(br#"{"jsonrpc":"2.0","id":7,"result":null}"#).unwrap(),
            Incoming::Reply { id, outcome: Ok(Value::Null) } if id == json!(7)
        ));
        assert_eq!(
            parse_line(br#"{"jsonrpc":"2.0","method":"roster","params":{"cursor":1}}"#).unwrap(),
            Incoming::Notification {
                method: "roster".into(),
                params: json!({"cursor": 1})
            }
        );
        assert_eq!(parse_line(b"Welcome to bash"), Err(LineError::NotJson));
        assert_eq!(parse_line(b"[1,2]"), Err(LineError::NotJson));
        assert!(matches!(
            parse_line(br#"{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"line too long"}}"#),
            Err(LineError::NullId(e)) if e.code == -32600
        ));
        assert!(matches!(
            parse_line(br#"{"jsonrpc":"2.0","id":"1","method":"x","params":{}}"#),
            Err(LineError::NotJsonRpc(_))
        ));
    }

    /// Every reply and notification must say `"jsonrpc":"2.0"`: absent, `1.0`
    /// or a non-string is not protocol 1.
    #[test]
    fn a_message_without_jsonrpc_2_0_is_not_protocol_1() {
        for line in [
            &br#"{"id":"1","result":{}}"#[..],
            br#"{"jsonrpc":"1.0","id":"1","result":{}}"#,
            br#"{"jsonrpc":2,"id":"1","error":{"code":-32000,"message":"x"}}"#,
            br#"{"method":"roster","params":{}}"#,
            br#"{"jsonrpc":"2.1","method":"roster","params":{}}"#,
        ] {
            assert!(
                matches!(parse_line(line), Err(LineError::NotJsonRpc(_))),
                "{}",
                String::from_utf8_lossy(line)
            );
        }
    }

    #[test]
    fn an_error_object_reads_tolerantly() {
        let e = RpcError::from_value(&json!({
            "code": -32000, "message": "m",
            "data": {"code": "not_accepting", "reason": "start_failed", "cause": "c", "result": {"supported": [1]}}
        }));
        assert_eq!(e.code, -32000);
        assert_eq!(e.data_code.as_deref(), Some("not_accepting"));
        assert_eq!(e.reason.as_deref(), Some("start_failed"));
        assert_eq!(e.cause.as_deref(), Some("c"));
        assert_eq!(e.result.as_deref(), Some(r#"{"supported":[1]}"#));
        // A wrong-typed member reads as absent; the rest survives.
        let e = RpcError::from_value(&json!({"code": "x", "message": "m", "data": {"code": 5}}));
        assert_eq!((e.code, e.message.as_str(), e.data_code), (0, "m", None));
    }

    #[test]
    fn a_roster_row_keeps_its_host_half_when_its_row_does_not_read() {
        let v = json!({
            "hostId": "0123456789ab", "sessionId": "s", "status": "reachable", "approximate": false,
            "host": {"pid": 1, "crazeVersion": "v", "protocol": 1, "provider": "grok", "workspace": "/w",
                     "startedAt": "2026-01-01T00:00:00Z", "ready": true},
            "row": {"pendingAsks": "three"}
        });
        let row = RosterRow::from_value(&v).unwrap();
        assert!(row.malformed);
        assert!(row.row.is_none());
        assert_eq!(row.host.provider, "grok");
        assert!(RosterRow::from_value(&json!({"sessionId": "s"})).is_err());
    }
}
