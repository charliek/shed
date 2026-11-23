//! What a named shed would be delivered (`delivery.preview`): the request its server
//! would send, rebuilt from the shed document as the server builds it
//! (`internal/config/delivery_record.go`: `DeliveryRequestShed`, `repoOwnerName`), and
//! the answer the providers would give, shown redacted: names, targets, stages, sizes,
//! fingerprints and problems, never a value.

use serde_json::Value;
use shed_provision::{Backend, Reason, Request, RequestRepo, RequestShed};

/// The shed user's home, which the server never sends as a project directory.
const HOME: &str = "/home/shed";

/// The id a preview's request carries: never one a server made.
pub const PREVIEW_REQUEST_ID: &str = "00000000000000000000000000000000";

/// The `owner/name` a repository URL parses to: its last two path segments, the second
/// without `.git`, or `None` when it has not exactly two.
pub fn repo_owner_name(url: &str) -> Option<String> {
    let rest = if let Some((_, after)) = url.split_once("://") {
        after.split_once('/')?.1
    } else if let Some(i) = url.find(':') {
        &url[i + 1..]
    } else {
        url
    };
    let rest = rest.trim_matches('/');
    let rest = rest.strip_suffix(".git").unwrap_or(rest);
    let parts: Vec<&str> = rest.split('/').collect();
    if parts.len() != 2 || parts.iter().any(|p| p.is_empty()) || rest.contains(['?', '#', '@']) {
        return None;
    }
    Some(rest.to_string())
}

/// A URL with any `user[:password]@` removed from its authority.
fn strip_userinfo(url: &str) -> String {
    if let Some((scheme, after)) = url.split_once("://") {
        let (authority, path) = match after.find('/') {
            Some(i) => (&after[..i], &after[i..]),
            None => (after, ""),
        };
        let host = authority.rsplit_once('@').map_or(authority, |(_, h)| h);
        return format!("{scheme}://{host}{path}");
    }
    url.to_string()
}

fn text<'a>(doc: &'a Value, key: &str) -> Option<&'a str> {
    doc.get(key)
        .and_then(Value::as_str)
        .filter(|s| !s.is_empty())
}

/// The request the shed of `doc` (its server's document) would draw for `reason`.
pub fn request_for(doc: &Value, reason: Reason, deadline: &str) -> Result<Request, String> {
    let name = text(doc, "name").ok_or("the shed document has no name")?;
    let backend = text(doc, "backend")
        .and_then(Backend::from_name)
        .ok_or("the shed document names no backend delivery knows")?;
    let landing = text(doc, "landing_dir").unwrap_or("");
    let repo = text(doc, "repo").map(|url| {
        let url = strip_userinfo(url);
        RequestRepo {
            name: repo_owner_name(&url),
            url,
        }
    });
    let local_dir = doc
        .get("project_mounts")
        .and_then(Value::as_array)
        .and_then(|mounts| {
            mounts
                .iter()
                .find(|m| !landing.is_empty() && text(m, "target") == Some(landing))
        })
        .and_then(|m| text(m, "source"))
        .map(|s| {
            let s = s.trim_end_matches('/');
            s.rsplit('/').next().unwrap_or(s).to_string()
        })
        .filter(|s| !s.is_empty());
    let project_dir = (!landing.is_empty() && landing != HOME).then(|| landing.to_string());
    Ok(Request {
        id: PREVIEW_REQUEST_ID.to_string(),
        reason,
        deadline: deadline.to_string(),
        user: text(doc, "user").unwrap_or("").to_string(),
        shed: RequestShed {
            name: name.to_string(),
            id: text(doc, "id").unwrap_or("preview").to_string(),
            backend,
            repo,
            local_dir,
            project_dir,
        },
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn owner_name_as_the_server_parses_it() {
        for (url, want) in [
            (
                "git@git.example.test:example/widgets.git",
                Some("example/widgets"),
            ),
            (
                "https://git.example.test/example/widgets",
                Some("example/widgets"),
            ),
            (
                "https://git.example.test/example/widgets.git/",
                Some("example/widgets"),
            ),
            ("https://git.example.test/group/sub/widgets", None),
            ("https://git.example.test", None),
            ("example/widgets", Some("example/widgets")),
        ] {
            assert_eq!(repo_owner_name(url).as_deref(), want, "{url}");
        }
        assert_eq!(
            strip_userinfo("https://ada:pw@git.example.test/a/b"),
            "https://git.example.test/a/b"
        );
    }

    #[test]
    fn the_request_is_rebuilt_from_the_document() {
        let doc = json!({
            "name": "widgets", "id": "q6c3g4ymb5m2dkxwe7kyz5hhzu", "backend": "vz",
            "repo": "git@git.example.test:example/widgets.git", "user": "ada",
            "landing_dir": "/home/shed/widgets",
            "project_mounts": [{"source": "/Users/ada/src/widgets/", "target": "/home/shed/widgets"}],
        });
        let r = request_for(&doc, Reason::Provision, "2999-01-01T00:00:00Z").unwrap();
        assert_eq!(r.shed.backend, Backend::Vz);
        assert_eq!(r.repo_name(), Some("example/widgets"));
        assert_eq!(r.shed.local_dir.as_deref(), Some("widgets"));
        assert_eq!(r.shed.project_dir.as_deref(), Some("/home/shed/widgets"));
        assert_eq!(r.stage2_dir(), None, "a mounted project gets no stage 2");
        assert_eq!(r.user, "ada");
        let home = request_for(
            &json!({"name": "a", "backend": "remote", "landing_dir": "/home/shed"}),
            Reason::Create,
            "2999-01-01T00:00:00Z",
        )
        .unwrap();
        assert_eq!(home.shed.project_dir, None);
        assert!(request_for(
            &json!({"name": "a", "backend": "docker"}),
            Reason::Create,
            "x"
        )
        .is_err());
    }
}
