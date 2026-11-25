//! `github`: the `gh` sign-in. On a machine it runs `gh auth token` and
//! `gh api user` (for the login); from [`Inputs`] (a token, its login and,
//! when known, when it expires) it delivers `~/.config/gh/hosts.yml`
//! (`0600`), with a git config fragment that makes `gh` git's credential
//! helper for github.com. A token past its expiry is `expired` and nothing
//! is delivered; one with an expiry gives its item that `expires_at`.

use std::fmt;

use shed_provision::{
    format_time, Base, Command, Context, Contribution, Draft, GitSection, Mode, ProblemCode, Reader,
};

use crate::common::{desktop_provider, file_draft, gathered, one_line, run, Redacted};
use crate::settings::GithubSettings;
use crate::Gathered;

/// The provider's name.
pub const NAME: &str = "github";

/// The host the sign-in is for.
const HOST: &str = "github.com";

/// What the provider delivers from.
#[derive(Clone, PartialEq, Eq)]
pub struct Inputs {
    /// The token.
    pub token: String,
    /// The GitHub login the token is for.
    pub login: String,
    /// When the token expires (Unix seconds), when known.
    pub expires_at: Option<i64>,
}

impl fmt::Debug for Inputs {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Inputs")
            .field("token", &Redacted(self.token.len()))
            .field("login", &self.login)
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

desktop_provider!(
    /// The `github` provider.
    Github,
    GithubSettings
);

/// A token as gh prints one, and as GitHub issues one: letters, digits, `_` and `-`, so it is a plain
/// YAML scalar.
fn valid_token(t: &str) -> bool {
    !t.is_empty()
        && t.len() <= 1024
        && t.bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'_' || c == b'-')
}

/// A GitHub login: letters, digits, `-` and `_`, starting with a letter or
/// digit.
fn valid_login(l: &str) -> bool {
    let b = l.as_bytes();
    !b.is_empty()
        && b.len() <= 100
        && b[0].is_ascii_alphanumeric()
        && b.iter()
            .all(|c| c.is_ascii_alphanumeric() || *c == b'-' || *c == b'_')
}

/// gh's `hosts.yml` for one signed-in account, its token stored in the
/// file.
fn hosts_yml(login: &str, token: &str) -> String {
    format!(
        "{HOST}:\n    users:\n        {login}:\n            oauth_token: {token}\n    git_protocol: https\n    oauth_token: {token}\n    user: {login}\n"
    )
}

/// Asks `gh` for its token and then for the login, stopping at the first
/// problem: no second command runs after a token that is not one.
pub fn gather(
    settings: &GithubSettings,
    ctx: &Context<'_>,
    reader: &dyn Reader,
) -> Gathered<Inputs> {
    gathered(ask_gh(settings, ctx, reader))
}

fn ask_gh(
    settings: &GithubSettings,
    ctx: &Context<'_>,
    reader: &dyn Reader,
) -> Result<Inputs, Contribution> {
    let gh = settings.gh.as_deref().unwrap_or("gh");
    let token_cmd = Command::new(gh, ["auth", "token", "--hostname", HOST]);
    let out = run(ctx, reader, &token_cmd, "gh auth token")?;
    if !out.success() {
        return Err(Contribution::problem(
            ProblemCode::SignedOut,
            "gh is not signed in to github.com",
        ));
    }
    let token = one_line(&out.stdout)
        .filter(|t| valid_token(t))
        .ok_or_else(|| {
            Contribution::problem(
                ProblemCode::CommandFailed,
                "gh auth token printed something that is not a token",
            )
        })?;
    let user_cmd = Command::new(gh, ["api", "user", "--hostname", HOST, "--jq", ".login"]);
    let out = run(ctx, reader, &user_cmd, "gh api user")?;
    if !out.success() {
        return Err(Contribution::problem(
            ProblemCode::CommandFailed,
            "gh api user failed",
        ));
    }
    let login = one_line(&out.stdout)
        .filter(|l| valid_login(l))
        .ok_or_else(|| {
            Contribution::problem(
                ProblemCode::CommandFailed,
                "gh api user printed something that is not a login",
            )
        })?;
    Ok(Inputs {
        token: token.to_string(),
        login: login.to_string(),
        expires_at: None,
    })
}

/// `hosts.yml` and the credential helper for `inputs`. The token and the
/// login are held to the forms `hosts.yml` can carry, and a token whose
/// expiry is not after `ctx.now` is `expired`, with no item.
pub fn build(inputs: &Inputs, ctx: &Context<'_>) -> Contribution {
    if !valid_token(&inputs.token) {
        return Contribution::problem(
            ProblemCode::Unreadable,
            "the GitHub token is not letters, digits, _ and - of at most 1024 bytes",
        );
    }
    if !valid_login(&inputs.login) {
        return Contribution::problem(
            ProblemCode::MissingInput,
            "the GitHub login is not a GitHub login",
        );
    }
    if inputs.expires_at.is_some_and(|t| t <= ctx.now) {
        return Contribution::problem(ProblemCode::Expired, "the GitHub token has expired");
    }
    let helper = [("helper", ""), ("helper", "!gh auth git-credential")];
    Contribution {
        items: vec![Draft {
            expires_at: inputs.expires_at.map(format_time),
            ..file_draft(
                NAME,
                Base::Home,
                ".config/gh/hosts.yml",
                Mode::M0600,
                hosts_yml(&inputs.login, &inputs.token).as_bytes(),
            )
        }],
        git_config: vec![
            GitSection::sub("credential", "https://github.com", &helper),
            GitSection::sub("credential", "https://gist.github.com", &helper),
        ],
        problems: Vec::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn inputs(expires_at: Option<i64>) -> Inputs {
        Inputs {
            token: "github_pat_SENTINEL".into(),
            login: "ada-example".into(),
            expires_at,
        }
    }

    fn at(now: i64) -> Context<'static> {
        Context::preview(now)
    }

    #[test]
    fn an_expired_token_is_expired_and_delivers_nothing() {
        let c = build(&inputs(Some(1_000)), &at(1_001));
        assert!(c.items.is_empty() && c.git_config.is_empty());
        assert_eq!(c.problems.len(), 1);
        assert_eq!(c.problems[0].code, ProblemCode::Expired);
        // At the instant itself it has expired.
        assert_eq!(
            build(&inputs(Some(1_000)), &at(1_000)).problems[0].code,
            ProblemCode::Expired
        );
    }

    #[test]
    fn a_later_expiry_reaches_the_item() {
        let c = build(&inputs(Some(1_795_431_600)), &at(1_795_428_000));
        assert!(c.problems.is_empty());
        assert_eq!(
            c.items[0].expires_at.as_deref(),
            Some("2026-11-23T11:00:00Z")
        );
        let c = build(&inputs(None), &at(1_795_428_000));
        assert_eq!(c.items[0].expires_at, None);
    }

    #[test]
    fn build_repeats_the_checks_gather_makes() {
        let mut i = inputs(None);
        i.token = "not a token!".into();
        assert_eq!(build(&i, &at(0)).problems[0].code, ProblemCode::Unreadable);
        let mut i = inputs(None);
        i.login = String::new();
        assert_eq!(
            build(&i, &at(0)).problems[0].code,
            ProblemCode::MissingInput
        );
        let shown = format!("{:?}", inputs(None));
        assert!(
            !shown.contains("SENTINEL") && shown.contains("ada-example"),
            "{shown}"
        );
    }
}
