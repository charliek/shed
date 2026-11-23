//! `github`: the `gh` sign-in. It runs `gh auth token` and `gh api user`
//! (for the login) and delivers `~/.config/gh/hosts.yml` (`0600`), with a
//! git config fragment that makes `gh` git's credential helper for
//! github.com.

use shed_provision::{
    Base, Command, Context, Contribution, Draft, FileItem, GitSection, ItemBody, Mode, Only,
    ProblemCode, Provider, Reader, Stage,
};

use crate::common::{one_line, run};
use crate::settings::GithubSettings;

/// The provider's name.
pub const NAME: &str = "github";

/// The host the sign-in is for.
const HOST: &str = "github.com";

/// The `github` provider.
#[derive(Debug, Clone)]
pub struct Github {
    settings: GithubSettings,
}

impl Github {
    /// The provider for these settings.
    pub fn new(settings: GithubSettings) -> Github {
        Github { settings }
    }

    fn gh(&self) -> &str {
        self.settings.gh.as_deref().unwrap_or("gh")
    }
}

/// A token as gh prints one: letters, digits, `_` and `-`, so it is a plain
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

impl Provider for Github {
    fn name(&self) -> &str {
        NAME
    }

    fn only(&self) -> Option<&Only> {
        self.settings.only.as_ref()
    }

    fn provide(&self, _ctx: &Context<'_>, reader: &dyn Reader) -> Contribution {
        let token_cmd = Command::new(self.gh(), ["auth", "token", "--hostname", HOST]);
        let out = match run(reader, &token_cmd, "gh auth token") {
            Ok(o) => o,
            Err(c) => return c,
        };
        if !out.success() {
            return Contribution::problem(
                ProblemCode::SignedOut,
                "gh is not signed in to github.com",
            );
        }
        let Some(token) = one_line(&out.stdout).filter(|t| valid_token(t)) else {
            return Contribution::problem(
                ProblemCode::CommandFailed,
                "gh auth token printed something that is not a token",
            );
        };
        let user_cmd = Command::new(
            self.gh(),
            ["api", "user", "--hostname", HOST, "--jq", ".login"],
        );
        let out = match run(reader, &user_cmd, "gh api user") {
            Ok(o) => o,
            Err(c) => return c,
        };
        if !out.success() {
            return Contribution::problem(ProblemCode::CommandFailed, "gh api user failed");
        }
        let Some(login) = one_line(&out.stdout).filter(|l| valid_login(l)) else {
            return Contribution::problem(
                ProblemCode::CommandFailed,
                "gh api user printed something that is not a login",
            );
        };
        let helper = [("helper", ""), ("helper", "!gh auth git-credential")];
        Contribution {
            items: vec![Draft {
                name: NAME.into(),
                body: ItemBody::File(FileItem {
                    stage: Stage::One,
                    base: Base::Home,
                    path: ".config/gh/hosts.yml".into(),
                    mode: Mode::M0600,
                    content: hosts_yml(login, token).into_bytes(),
                }),
                expires_at: None,
            }],
            git_config: vec![
                GitSection::sub("credential", "https://github.com", &helper),
                GitSection::sub("credential", "https://gist.github.com", &helper),
            ],
            problems: Vec::new(),
        }
    }
}
