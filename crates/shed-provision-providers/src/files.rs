//! `files`: files copied as they are, each `{from, to, base, mode, only}`.
//! `base = "project"` makes an entry stage 2 (written into the project
//! directory after the clone). Modes are `0600` (default) or `0400`.

use shed_provision::{
    limits::MAX_FILE_BYTES, valid_name, Base, Context, Contribution, Draft, FileItem, ItemBody,
    Mode, Only, Provider, Reader, Stage,
};

use crate::common::read_setting_file;
use crate::settings::FilesSettings;

/// The provider's name.
pub const NAME: &str = "files";

/// The `files` provider.
#[derive(Debug, Clone)]
pub struct Files {
    settings: FilesSettings,
}

impl Files {
    /// The provider for these settings.
    pub fn new(settings: FilesSettings) -> Files {
        Files { settings }
    }
}

/// An item name from an entry's target: `files.` and the path lower-cased,
/// with every character outside the name alphabet made `-`, cut to the
/// name's length.
fn item_name(to: &str) -> String {
    let mut name = format!("{NAME}.");
    for c in to.chars() {
        let c = c.to_ascii_lowercase();
        name.push(
            if c.is_ascii_lowercase() || c.is_ascii_digit() || matches!(c, '.' | '_' | '-') {
                c
            } else {
                '-'
            },
        );
    }
    name.truncate(64);
    name
}

impl Provider for Files {
    fn name(&self) -> &str {
        NAME
    }

    fn only(&self) -> Option<&Only> {
        self.settings.only.as_ref()
    }

    fn provide(&self, ctx: &Context<'_>, reader: &dyn Reader) -> Contribution {
        let mut c = Contribution::default();
        for (i, e) in self.settings.files.iter().enumerate() {
            if e.only.as_ref().is_some_and(|o| !ctx.allows(o)) {
                continue;
            }
            let f = match read_setting_file(
                reader,
                &e.from,
                MAX_FILE_BYTES,
                &format!("files.file[{i}].from"),
            ) {
                Ok(f) => f,
                Err(p) => {
                    c.problems.extend(p.problems);
                    continue;
                }
            };
            let mut name = item_name(&e.to);
            if !valid_name(&name) || c.items.iter().any(|d| d.name == name) {
                name = format!("{NAME}.{i}");
            }
            c.items.push(Draft {
                name,
                body: ItemBody::File(FileItem {
                    stage: if e.base == Base::Project {
                        Stage::Two
                    } else {
                        Stage::One
                    },
                    base: e.base,
                    path: e.to.clone(),
                    mode: if e.mode.as_deref() == Some("0400") {
                        Mode::M0400
                    } else {
                        Mode::M0600
                    },
                    content: f.bytes,
                }),
                expires_at: None,
            });
        }
        c
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names() {
        assert_eq!(item_name(".npmrc"), "files..npmrc");
        assert_eq!(
            item_name("Config/App Settings.json"),
            "files.config-app-settings.json"
        );
        assert_eq!(item_name(&"a".repeat(100)).len(), 64);
    }
}
