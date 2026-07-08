//! shedctl — a headless command-line driver for the shed-desktop JSON IPC socket.
//! The Linux sibling of the macOS `shedctl`: no GUI toolkit, no display — just a
//! blocking Unix-socket client that speaks the newline-delimited
//! `{id, op, params}` → `{id, ok, result}` / `{id, ok:false, error:{code,message}}`
//! envelope the shed-desktop app (and the pytest harness) use. Making the client
//! drivable and observable over IPC is the North Star; this is the human/script
//! front door to that socket. Shipped in the .deb next to `shed-desktop`, mirroring
//! how roost ships `roostctl` alongside `roost`.
//!
//! Socket resolution mirrors the Tauri app's `env::default_socket_path` (duplicated,
//! not depended on, to keep this crate dependency-light): `--socket` >
//! `$SHED_TAURI_SOCKET` > `$XDG_RUNTIME_DIR/shed-tauri.sock` >
//! `/tmp/shed-tauri-<uid>/shed-tauri.sock`.

use std::io::{self, BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::process;

use base64::Engine as _;
use serde_json::{json, Value};

/// A parsed command: the op + params to send, plus (screenshot only) a PNG
/// output path that turns the reply into a decoded-to-file write instead of a
/// pretty-printed JSON dump.
struct Plan {
    op: String,
    params: Value,
    /// `Some` only for `screenshot --out FILE`: decode `result.png` → this path.
    write_png_to: Option<PathBuf>,
}

impl Plan {
    fn call(op: &str, params: Value) -> Self {
        Self {
            op: op.to_string(),
            params,
            write_png_to: None,
        }
    }
}

/// The request envelope the socket expects. Kept pure (arg → frame) so the
/// mapping is unit-testable without a socket. Takes `params` by value — it's
/// moved into the frame, not copied.
fn build_request(id: &str, op: &str, params: Value) -> Value {
    // Build via a Map (not `json!`) so `params` is *moved* into the frame: `json!`
    // would borrow it, tripping clippy's needless_pass_by_value under -D warnings.
    let mut frame = serde_json::Map::new();
    frame.insert("id".to_string(), Value::String(id.to_string()));
    frame.insert("op".to_string(), Value::String(op.to_string()));
    frame.insert("params".to_string(), params);
    Value::Object(frame)
}

/// Parse one reply line: an `ok` envelope yields its `result`, an error envelope
/// yields `Err((code, message))`. A malformed line is itself an error envelope
/// (`bad_reply`) so the caller can report it uniformly. Pure → unit-testable.
fn parse_reply(line: &str) -> Result<Value, (String, String)> {
    let v: Value = serde_json::from_str(line.trim())
        .map_err(|e| ("bad_reply".to_string(), format!("invalid JSON reply: {e}")))?;
    if v.get("ok").and_then(Value::as_bool) == Some(true) {
        return Ok(v.get("result").cloned().unwrap_or(Value::Null));
    }
    let field = |k| {
        v.get("error")
            .and_then(|e| e.get(k))
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_string()
    };
    let code = match field("code").as_str() {
        "" => "error".to_string(),
        c => c.to_string(),
    };
    Err((code, field("message")))
}

/// Parse a `--param k=v` pair: split on the first `=`, then interpret the value
/// as JSON when it parses (`3` → number, `true` → bool, `{"a":1}` → object),
/// else treat it as a plain string. Pure → unit-testable.
fn parse_param(pair: &str) -> Result<(String, Value), String> {
    let (key, raw) = pair
        .split_once('=')
        .ok_or_else(|| format!("--param expects k=v, got '{pair}'"))?;
    let value = serde_json::from_str::<Value>(raw).unwrap_or_else(|_| Value::String(raw.to_string()));
    Ok((key.to_string(), value))
}

/// Remove the first `--name VALUE` pair from `args`, returning VALUE (or `Err`
/// if the flag is present but has no following value).
fn take_opt(args: &mut Vec<String>, name: &str) -> Result<Option<String>, String> {
    match args.iter().position(|a| a.as_str() == name) {
        Some(i) => {
            if i + 1 >= args.len() {
                return Err(format!("{name} requires a value"));
            }
            let value = args.remove(i + 1);
            args.remove(i);
            Ok(Some(value))
        }
        None => Ok(None),
    }
}

/// Drain every `--param k=v` occurrence from `args`, parsing each value.
fn take_params(args: &mut Vec<String>) -> Result<Vec<(String, Value)>, String> {
    let mut out = Vec::new();
    while let Some(i) = args.iter().position(|a| a.as_str() == "--param") {
        if i + 1 >= args.len() {
            return Err("--param requires a k=v argument".to_string());
        }
        let pair = args.remove(i + 1);
        args.remove(i);
        out.push(parse_param(&pair)?);
    }
    Ok(out)
}

/// Map argv (with `--socket` already stripped) to a [`Plan`]. `Err` is a usage
/// message (exit 2). Pure → unit-testable.
fn parse_command(args: &[String]) -> Result<Plan, String> {
    let mut rest = args.to_vec();
    if rest.is_empty() {
        return Err("no command given".to_string());
    }
    let cmd = rest.remove(0);
    match cmd.as_str() {
        "identify" => Ok(Plan::call("identify", json!({}))),
        "sheds" => match rest.first().map(String::as_str) {
            Some("list") => Ok(Plan::call("sheds.list", json!({}))),
            _ => Err("usage: shedctl sheds list".to_string()),
        },
        "dashboard" => match rest.first().map(String::as_str) {
            Some("dump") => Ok(Plan::call("dashboard.dump", json!({}))),
            _ => Err("usage: shedctl dashboard dump".to_string()),
        },
        "screenshot" => {
            let out = take_opt(&mut rest, "--out")?.map(PathBuf::from);
            let scale = match take_opt(&mut rest, "--scale")? {
                Some(s) => s
                    .parse::<u64>()
                    .map_err(|_| format!("--scale must be a positive integer, got '{s}'"))?,
                None => 1,
            };
            Ok(Plan {
                op: "app.screenshot".to_string(),
                params: json!({ "scale": scale }),
                write_png_to: out,
            })
        }
        "shed" => {
            let host = take_opt(&mut rest, "--host")?;
            let action = rest
                .first()
                .ok_or("shed requires an action (start|stop|reset|delete)")?
                .clone();
            if !matches!(action.as_str(), "start" | "stop" | "reset" | "delete") {
                return Err(format!("unknown shed action: {action}"));
            }
            let name = rest.get(1).ok_or("shed <action> requires a NAME")?.clone();
            let mut params = json!({ "name": name });
            if let Some(h) = host {
                params["host"] = Value::String(h);
            }
            Ok(Plan::call(&format!("shed.{action}"), params))
        }
        "raw" => {
            let pairs = take_params(&mut rest)?;
            let op = rest.first().ok_or("raw requires an <op>")?.clone();
            let mut map = serde_json::Map::new();
            for (k, v) in pairs {
                map.insert(k, v);
            }
            Ok(Plan::call(&op, Value::Object(map)))
        }
        other => Err(format!("unknown command: {other}")),
    }
}

/// `--socket` flag > `$SHED_TAURI_SOCKET` > `default_socket_path()`.
fn resolve_socket(flag: Option<String>) -> PathBuf {
    flag.or_else(|| std::env::var("SHED_TAURI_SOCKET").ok().filter(|v| !v.is_empty()))
        .map(PathBuf::from)
        .unwrap_or_else(default_socket_path)
}

/// `$XDG_RUNTIME_DIR/shed-tauri.sock`, falling back to
/// `/tmp/shed-tauri-<uid>/shed-tauri.sock` when `XDG_RUNTIME_DIR` is unset —
/// matching the Tauri app's `env::default_socket_path` (flat, no nested subdir; a
/// duplicate, not a dependency, to keep shedctl dependency-light).
fn default_socket_path() -> PathBuf {
    socket_path_from(std::env::var_os("XDG_RUNTIME_DIR").as_deref(), current_uid())
}

/// Pure socket-path resolution (no env reads), so the flat `shed-tauri.sock`
/// layout + the `/tmp` fallback are unit-testable.
fn socket_path_from(xdg_runtime_dir: Option<&std::ffi::OsStr>, uid: u32) -> PathBuf {
    let dir = match xdg_runtime_dir {
        Some(x) if !x.is_empty() => PathBuf::from(x),
        _ => PathBuf::from(format!("/tmp/shed-tauri-{uid}")),
    };
    dir.join("shed-tauri.sock")
}

fn current_uid() -> u32 {
    // getuid() is infallible and has no safety preconditions.
    unsafe { libc::getuid() }
}

/// Write one request frame + read exactly one reply line back.
fn exchange(stream: &UnixStream, frame: &Value) -> io::Result<String> {
    let mut bytes = serde_json::to_vec(frame).expect("serialize request frame");
    bytes.push(b'\n');
    let mut writer = stream;
    writer.write_all(&bytes)?;
    let mut reader = BufReader::new(stream);
    let mut line = String::new();
    reader.read_line(&mut line)?;
    Ok(line)
}

/// Decode the screenshot reply's base64 `png` and write it to `out`, printing a
/// `wrote <FILE> (<w>x<h>)` line.
fn write_png(result: &Value, out: &Path) -> Result<(), String> {
    let b64 = result
        .get("png")
        .and_then(Value::as_str)
        .ok_or("screenshot reply had no 'png' field")?;
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(b64)
        .map_err(|e| format!("failed to decode png base64: {e}"))?;
    std::fs::write(out, &bytes).map_err(|e| format!("failed to write {}: {e}", out.display()))?;
    let dim = |k| result.get(k).and_then(Value::as_u64).unwrap_or(0);
    println!("wrote {} ({}x{})", out.display(), dim("width"), dim("height"));
    Ok(())
}

const USAGE: &str = "\
shedctl — drive the shed-desktop IPC socket

USAGE:
  shedctl [--socket PATH] <command>

COMMANDS:
  identify                              identify the running client
  sheds list                           list sheds across configured hosts
  dashboard dump                       dump the dashboard's rendered sheds
  screenshot [--out FILE] [--scale N]  render the window (PNG → FILE, else JSON)
  shed <start|stop|reset|delete> NAME [--host H]
                                       run a shed lifecycle action
  raw <op> [--param k=v ...]           call an arbitrary op (values parsed as JSON)

Socket: --socket > $SHED_TAURI_SOCKET > $XDG_RUNTIME_DIR/shed-tauri.sock > /tmp/shed-tauri-<uid>/shed-tauri.sock";

fn fail_usage(reason: &str) -> ! {
    eprintln!("shedctl: {reason}\n\n{USAGE}");
    process::exit(2);
}

fn main() {
    let mut args: Vec<String> = std::env::args().skip(1).collect();
    // `--socket` is global (highest-precedence socket source); pull it wherever
    // it appears before interpreting the command.
    let socket_flag = take_opt(&mut args, "--socket").unwrap_or_else(|e| fail_usage(&e));
    let plan = parse_command(&args).unwrap_or_else(|e| fail_usage(&e));
    let socket_path = resolve_socket(socket_flag);
    let frame = build_request("1", &plan.op, plan.params);

    let stream = UnixStream::connect(&socket_path).unwrap_or_else(|_| {
        eprintln!(
            "shedctl: cannot reach the IPC socket — is shed-desktop running? (socket: {})",
            socket_path.display()
        );
        process::exit(1);
    });
    let line = exchange(&stream, &frame).unwrap_or_else(|e| {
        eprintln!("shedctl: IPC error: {e}");
        process::exit(1);
    });

    match parse_reply(&line) {
        Ok(result) => {
            if let Some(out) = plan.write_png_to {
                write_png(&result, &out).unwrap_or_else(|e| {
                    eprintln!("shedctl: {e}");
                    process::exit(1);
                });
            } else {
                let pretty =
                    serde_json::to_string_pretty(&result).unwrap_or_else(|_| result.to_string());
                println!("{pretty}");
            }
        }
        Err((code, message)) => {
            eprintln!("error {code}: {message}");
            process::exit(1);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn svec(a: &[&str]) -> Vec<String> {
        a.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn socket_path_uses_flat_tauri_layout() {
        use std::ffi::OsStr;
        // XDG set → flat shed-tauri.sock directly under it (no nested subdir).
        assert_eq!(
            socket_path_from(Some(OsStr::new("/run/user/1000")), 1000),
            PathBuf::from("/run/user/1000/shed-tauri.sock")
        );
        // XDG unset or empty → the /tmp/shed-tauri-<uid> fallback.
        assert_eq!(
            socket_path_from(None, 501),
            PathBuf::from("/tmp/shed-tauri-501/shed-tauri.sock")
        );
        assert_eq!(
            socket_path_from(Some(OsStr::new("")), 501),
            PathBuf::from("/tmp/shed-tauri-501/shed-tauri.sock")
        );
    }

    #[test]
    fn build_request_shapes_the_frame() {
        let f = build_request("7", "sheds.list", json!({ "host": "h1" }));
        assert_eq!(f["id"], "7");
        assert_eq!(f["op"], "sheds.list");
        assert_eq!(f["params"], json!({ "host": "h1" }));
    }

    #[test]
    fn parse_reply_unwraps_ok_result() {
        let line = r#"{"id":"1","ok":true,"result":{"sheds":[{"name":"alpha"}]}}"#;
        let result = parse_reply(line).expect("ok envelope");
        assert_eq!(result["sheds"][0]["name"], "alpha");
    }

    #[test]
    fn parse_reply_maps_error_envelope() {
        let line = r#"{"id":"1","ok":false,"error":{"code":"bad_request","message":"missing 'name'"}}"#;
        let (code, message) = parse_reply(line).unwrap_err();
        assert_eq!(code, "bad_request");
        assert_eq!(message, "missing 'name'");
    }

    #[test]
    fn parse_param_parses_json_scalars_else_string() {
        assert_eq!(parse_param("n=3").unwrap(), ("n".to_string(), json!(3)));
        assert_eq!(parse_param("s=hi").unwrap(), ("s".to_string(), json!("hi")));
        assert_eq!(parse_param("b=true").unwrap(), ("b".to_string(), json!(true)));
    }

    #[test]
    fn parse_command_shed_action_carries_name_and_host() {
        let plan = parse_command(&svec(&["shed", "start", "alpha", "--host", "h1"])).unwrap();
        assert_eq!(plan.op, "shed.start");
        assert_eq!(plan.params["name"], "alpha");
        assert_eq!(plan.params["host"], "h1");
        assert!(plan.write_png_to.is_none());
    }

    #[test]
    fn parse_command_raw_collects_params() {
        let plan =
            parse_command(&svec(&["raw", "create.status", "--param", "create_id=abc"])).unwrap();
        assert_eq!(plan.op, "create.status");
        assert_eq!(plan.params["create_id"], "abc");
    }

    #[test]
    fn parse_command_screenshot_out_sets_write_target() {
        let plan =
            parse_command(&svec(&["screenshot", "--out", "/tmp/x.png", "--scale", "2"])).unwrap();
        assert_eq!(plan.op, "app.screenshot");
        assert_eq!(plan.params["scale"], 2);
        assert_eq!(plan.write_png_to, Some(PathBuf::from("/tmp/x.png")));
    }

    #[test]
    fn parse_command_unknown_is_usage_error() {
        assert!(parse_command(&svec(&["bogus"])).is_err());
        assert!(parse_command(&svec(&["shed", "frobnicate", "x"])).is_err());
    }

    #[test]
    fn parse_param_without_equals_is_error() {
        // No '=' → a usage error, not a silent empty-key/whole-string pair.
        assert!(parse_param("noequals").is_err());
    }

    #[test]
    fn write_png_without_png_field_is_error() {
        // A screenshot reply missing `png` is reported, and nothing is written.
        let dir = tempfile::tempdir().unwrap();
        let out = dir.path().join("out.png");
        assert!(write_png(&json!({ "width": 1, "height": 1 }), &out).is_err());
        assert!(!out.exists()); // the error is raised before any file write
    }

    #[test]
    fn take_opt_flag_without_value_is_error() {
        // `--out` present but trailing (no following VALUE) → error, not a silent None.
        assert!(take_opt(&mut vec!["--out".into()], "--out").is_err());
    }

    #[test]
    fn take_params_param_without_pair_is_error() {
        // `--param` with no k=v following it → error.
        assert!(take_params(&mut vec!["--param".into()]).is_err());
    }
}
