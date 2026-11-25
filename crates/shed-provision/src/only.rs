//! The `only` rules: which servers, backends and repositories a provider (or
//! one of its entries) delivers to. Every list given must match, and a list
//! is never empty.

use serde::Deserialize;

use crate::request::{Backend, Request};

/// Where a provider delivers. An absent list does not restrict; a list
/// given must match.
#[derive(Debug, Clone, Default, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Only {
    /// The servers, by the name the person's config gives them.
    pub servers: Option<Vec<String>>,
    /// The backends.
    pub backends: Option<Vec<Backend>>,
    /// Repository patterns, `owner/name`, each part with `*` matching any
    /// run of characters but `/`. Compared as written (case matters). A shed
    /// with no repository, or whose URL gives no `owner/name`, matches none.
    pub repos: Option<Vec<String>>,
}

impl Only {
    /// Checks the rules are well formed: no empty list, and every repository
    /// pattern `owner/name`. The error names the list, never a value.
    pub fn validate(&self) -> Result<(), String> {
        if self.servers.as_ref().is_some_and(Vec::is_empty) {
            return Err("only.servers is an empty list".into());
        }
        if self.backends.as_ref().is_some_and(Vec::is_empty) {
            return Err("only.backends is an empty list".into());
        }
        if let Some(repos) = &self.repos {
            if repos.is_empty() {
                return Err("only.repos is an empty list".into());
            }
            if let Some(i) = repos.iter().position(|p| !valid_repo_pattern(p)) {
                return Err(format!("only.repos[{i}] is not an owner/name pattern"));
            }
        }
        Ok(())
    }

    /// Whether a delivery to `server` for `request` passes every list
    /// given.
    pub fn matches(&self, server: &str, request: &Request) -> bool {
        if let Some(servers) = &self.servers {
            if !servers.iter().any(|s| s == server) {
                return false;
            }
        }
        if let Some(backends) = &self.backends {
            if !backends.contains(&request.shed.backend) {
                return false;
            }
        }
        if let Some(repos) = &self.repos {
            let Some(name) = request.repo_name() else {
                return false;
            };
            if !repos.iter().any(|p| repo_pattern_matches(p, name)) {
                return false;
            }
        }
        true
    }
}

/// Whether `p` is an `owner/name` pattern: two non-empty parts and one
/// slash, with no control character or white space.
pub fn valid_repo_pattern(p: &str) -> bool {
    match p.split_once('/') {
        Some((owner, name)) => {
            !owner.is_empty()
                && !name.is_empty()
                && !name.contains('/')
                && !p.chars().any(|c| c.is_control() || c.is_whitespace())
        }
        None => false,
    }
}

/// Whether the repository `name` (`owner/name`) matches `pattern`: part by
/// part, `*` matching any run of characters (none included) within a part.
pub fn repo_pattern_matches(pattern: &str, name: &str) -> bool {
    match (pattern.split_once('/'), name.split_once('/')) {
        (Some((po, pn)), Some((no, nn))) => {
            glob(po.as_bytes(), no.as_bytes()) && glob(pn.as_bytes(), nn.as_bytes())
        }
        _ => false,
    }
}

/// `*`-only globbing, iterative with one backtrack point.
fn glob(p: &[u8], s: &[u8]) -> bool {
    let (mut pi, mut si) = (0, 0);
    let mut star: Option<(usize, usize)> = None;
    while si < s.len() {
        if pi < p.len() && p[pi] == b'*' {
            star = Some((pi, si));
            pi += 1;
        } else if pi < p.len() && p[pi] == s[si] {
            pi += 1;
            si += 1;
        } else if let Some((sp, ss)) = star {
            pi = sp + 1;
            si = ss + 1;
            star = Some((sp, ss + 1));
        } else {
            return false;
        }
    }
    p[pi..].iter().all(|&c| c == b'*')
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::request::parse_request;

    fn req(backend: &str, repo: Option<&str>) -> Request {
        let repo = repo
            .map(|n| {
                format!(",\"repo\":{{\"url\":\"git@git.example.test:{n}.git\",\"name\":\"{n}\"}}")
            })
            .unwrap_or_default();
        let body = format!(
            "{{\"version\":2,\"id\":\"0123456789abcdef0123456789abcdef\",\"reason\":\"create\",\"deadline\":\"2026-11-23T10:00:30Z\",\"user\":\"ada\",\"shed\":{{\"name\":\"w\",\"id\":\"q6c3\",\"backend\":\"{backend}\"{repo}}}}}"
        );
        parse_request(body.as_bytes()).unwrap()
    }

    #[test]
    fn globs() {
        assert!(repo_pattern_matches("example/*", "example/widgets"));
        assert!(repo_pattern_matches("*/*", "a/b"));
        assert!(repo_pattern_matches("ex*/w*ts", "example/widgets"));
        assert!(repo_pattern_matches("example/widgets", "example/widgets"));
        assert!(!repo_pattern_matches("example/widgets", "Example/widgets"));
        assert!(!repo_pattern_matches("example/*", "other/widgets"));
        assert!(!repo_pattern_matches("example/w*x", "example/widgets"));
        assert!(!valid_repo_pattern("example"));
        assert!(!valid_repo_pattern("a/b/c"));
        assert!(!valid_repo_pattern("/b"));
    }

    #[test]
    fn every_list_given_must_match() {
        let fc = req("firecracker", Some("example/widgets"));
        let none = Only::default();
        assert!(none.matches("work", &fc));
        let only = Only {
            servers: Some(vec!["work".into()]),
            backends: Some(vec![Backend::Firecracker, Backend::Remote]),
            repos: Some(vec!["example/*".into()]),
        };
        assert!(only.validate().is_ok());
        assert!(only.matches("work", &fc));
        assert!(!only.matches("home", &fc));
        assert!(!only.matches("work", &req("vz", Some("example/widgets"))));
        assert!(!only.matches("work", &req("firecracker", Some("other/widgets"))));
        assert!(!only.matches("work", &req("firecracker", None)));
        let empty = Only {
            servers: Some(vec![]),
            ..Only::default()
        };
        assert!(empty.validate().is_err());
    }
}
