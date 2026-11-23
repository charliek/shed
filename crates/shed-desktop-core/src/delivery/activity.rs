//! `activity.server_list`: the activity feed (`GET /api/activity`, plan 039 §3.6) of
//! every configured server, merged newest first, with paging.
//!
//! Each server pages on its own cursor; the merged page's cursor is all of them, an
//! opaque JSON string naming, per server, where its next page starts (`since`) or that
//! it has nothing older (`null`). A page asks each server that is not done for `limit`
//! events, merges them by time (newest first, ties in server then page order), keeps the
//! first `limit`, and moves each server's cursor past what it kept of that server: to the
//! server's own `next` when every event it sent was kept, else to the last one kept (the
//! server's `since` starts just older than the event it names). A server that could not
//! be read keeps its cursor and is listed in `errors`; its events come on a later page.

use std::collections::BTreeMap;

use serde_json::{json, Value};
use shed_core::http::Client;

/// The largest page.
pub const MAX_LIMIT: u32 = 500;
/// A page's size when none is given.
pub const DEFAULT_LIMIT: u32 = 100;

/// Per server: where its next page starts (`Some("")` is the newest), or `None` when it
/// has nothing older.
pub type Cursor = BTreeMap<String, Option<String>>;

/// Reads a merged cursor. Every key must be one of `servers`.
pub fn parse_cursor(raw: &str, servers: &[String]) -> Result<Cursor, String> {
    let v: Value =
        serde_json::from_str(raw).map_err(|_| "the cursor is not one this app made".to_string())?;
    let Value::Object(m) = v else {
        return Err("the cursor is not one this app made".into());
    };
    let mut out = Cursor::new();
    for (k, v) in m {
        if !servers.contains(&k) {
            return Err("the cursor names a server that is not configured".into());
        }
        let since = match v {
            Value::Null => None,
            Value::String(s) if s.len() <= 256 => Some(s),
            _ => return Err("the cursor is not one this app made".into()),
        };
        out.insert(k, since);
    }
    Ok(out)
}

/// A sortable form of an RFC 3339 time: (unix seconds, nanoseconds). An unparseable one
/// sorts oldest.
fn time_key(at: &str) -> (i64, u32) {
    let secs = match shed_source::time::parse_rfc3339_to_unix(at) {
        Ok(Some(s)) => s,
        _ => return (i64::MIN, 0),
    };
    let nanos = at
        .split_once('.')
        .map(|(_, f)| {
            let digits: String = f.chars().take_while(char::is_ascii_digit).take(9).collect();
            format!("{digits:0<9}").parse().unwrap_or(0)
        })
        .unwrap_or(0);
    (secs, nanos)
}

/// One server's page, as it was read.
pub struct Page {
    pub server: String,
    pub result: Result<Value, Value>,
}

/// Merges `pages` (read at `cursor`) into one page of at most `limit` events.
pub fn merge(pages: Vec<Page>, cursor: &Cursor, limit: usize) -> Value {
    let mut errors = Vec::new();
    let mut next = cursor.clone();
    let mut all: Vec<(usize, usize, (i64, u32), Value)> = Vec::new();
    let mut per_server: Vec<(String, Vec<Value>, String)> = Vec::new();
    for page in pages {
        match page.result {
            Err(e) => {
                let mut e = e;
                if let Value::Object(m) = &mut e {
                    m.insert("server".into(), json!(page.server));
                }
                errors.push(e);
            }
            Ok(doc) => {
                let events: Vec<Value> = doc
                    .get("events")
                    .and_then(Value::as_array)
                    .cloned()
                    .unwrap_or_default();
                let server_next = doc
                    .get("next")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .to_string();
                let s = per_server.len();
                for (i, ev) in events.iter().enumerate() {
                    let at = ev.get("at").and_then(Value::as_str).unwrap_or("");
                    all.push((s, i, time_key(at), ev.clone()));
                }
                per_server.push((page.server, events, server_next));
            }
        }
    }
    all.sort_by(|a, b| b.2.cmp(&a.2).then(a.0.cmp(&b.0)).then(a.1.cmp(&b.1)));
    all.truncate(limit);
    let mut kept = vec![0usize; per_server.len()];
    let events: Vec<Value> = all
        .into_iter()
        .map(|(s, i, _, mut ev)| {
            kept[s] = kept[s].max(i + 1);
            if let Value::Object(m) = &mut ev {
                m.insert("server".into(), json!(per_server[s].0));
            }
            ev
        })
        .collect();
    for (s, (server, evs, server_next)) in per_server.iter().enumerate() {
        let k = kept[s];
        if k == evs.len() {
            next.insert(
                server.clone(),
                (!server_next.is_empty()).then(|| server_next.clone()),
            );
        } else if k > 0 {
            let id = evs[k - 1].get("id").and_then(Value::as_str).unwrap_or("");
            next.insert(server.clone(), Some(id.to_string()));
        }
    }
    let more = next.values().any(Option::is_some);
    json!({
        "events": events,
        "next": if more { Value::from(serde_json::to_string(&next).unwrap_or_default()) } else { Value::Null },
        "errors": errors,
    })
}

/// One merged page across `clients` (name, client), starting at `cursor` (none: the
/// newest of every server), filtered to `shed` when given.
pub async fn server_list(
    clients: &[(String, Client)],
    cursor: Option<&str>,
    shed: Option<&str>,
    limit: u32,
) -> Result<Value, String> {
    let names: Vec<String> = clients.iter().map(|(n, _)| n.clone()).collect();
    let mut cur = match cursor {
        Some(c) => parse_cursor(c, &names)?,
        None => Cursor::new(),
    };
    for n in &names {
        cur.entry(n.clone()).or_insert_with(|| Some(String::new()));
    }
    let limit = limit.clamp(1, MAX_LIMIT);
    let reads = clients.iter().filter_map(|(name, client)| {
        let since = cur.get(name).cloned().flatten()?;
        let name = name.clone();
        Some(async move {
            let result = client
                .activity(Some(&since), shed, Some(limit))
                .await
                .map_err(|e| serde_json::to_value(e).unwrap_or(Value::Null));
            Page {
                server: name,
                result,
            }
        })
    });
    let pages = futures_util::future::join_all(reads).await;
    Ok(merge(pages, &cur, limit as usize))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ev(id: &str, at: &str) -> Value {
        json!({"id": id, "at": at, "kind": "shed.create", "shed": {"name": "x"}, "actor": {"kind": "user"}})
    }

    fn page(server: &str, events: &[Value], next: &str) -> Page {
        Page {
            server: server.into(),
            result: Ok(json!({"events": events, "next": next})),
        }
    }

    #[test]
    fn pages_merge_newest_first_and_each_cursor_moves_past_what_it_kept() {
        let cur: Cursor = [
            ("a".into(), Some(String::new())),
            ("b".into(), Some(String::new())),
        ]
        .into_iter()
        .collect();
        let out = merge(
            vec![
                page(
                    "a",
                    &[
                        ev("a3", "2026-11-23T10:00:05Z"),
                        ev("a2", "2026-11-23T10:00:02Z"),
                    ],
                    "a2",
                ),
                page(
                    "b",
                    &[
                        ev("b2", "2026-11-23T10:00:04.5Z"),
                        ev("b1", "2026-11-23T10:00:01Z"),
                    ],
                    "",
                ),
            ],
            &cur,
            3,
        );
        let ids: Vec<(&str, &str)> = out["events"]
            .as_array()
            .unwrap()
            .iter()
            .map(|e| (e["server"].as_str().unwrap(), e["id"].as_str().unwrap()))
            .collect();
        assert_eq!(ids, vec![("a", "a3"), ("b", "b2"), ("a", "a2")]);
        let next: Cursor = serde_json::from_str(out["next"].as_str().unwrap()).unwrap();
        // a: all kept, so its own next; b: one of two kept, so past b2.
        assert_eq!(next["a"].as_deref(), Some("a2"));
        assert_eq!(next["b"].as_deref(), Some("b2"));
    }

    #[test]
    fn a_done_server_is_null_and_all_done_is_no_next() {
        let cur: Cursor = [("a".into(), Some(String::new()))].into_iter().collect();
        let out = merge(
            vec![page("a", &[ev("a1", "2026-11-23T10:00:00Z")], "")],
            &cur,
            10,
        );
        assert_eq!(out["next"], Value::Null);
        let err = Page {
            server: "b".into(),
            result: Err(json!({"status": 500, "code": "", "message": "down"})),
        };
        let cur: Cursor = [("a".into(), None), ("b".into(), Some("c9".into()))]
            .into_iter()
            .collect();
        let out = merge(vec![err], &cur, 10);
        assert_eq!(out["errors"][0]["server"], "b");
        let next: Cursor = serde_json::from_str(out["next"].as_str().unwrap()).unwrap();
        assert_eq!(next["b"].as_deref(), Some("c9"), "kept for the next page");
        assert_eq!(next["a"], None);
    }

    #[test]
    fn a_cursor_names_only_configured_servers() {
        let servers = vec!["a".to_string()];
        assert!(parse_cursor(r#"{"a":"c1"}"#, &servers).is_ok());
        assert!(parse_cursor(r#"{"z":"c1"}"#, &servers).is_err());
        assert!(parse_cursor(r#"{"a":5}"#, &servers).is_err());
        assert!(parse_cursor("nope", &servers).is_err());
    }

    #[test]
    fn times_order_with_fractions() {
        assert!(time_key("2026-11-23T10:00:05Z") > time_key("2026-11-23T10:00:04.999Z"));
        assert!(time_key("2026-11-23T10:00:05.1Z") > time_key("2026-11-23T10:00:05Z"));
        assert!(time_key("garbage") < time_key("1970-01-01T00:00:00Z"));
    }
}
