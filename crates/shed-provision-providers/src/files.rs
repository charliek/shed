//! `files`: files copied as they are, each `{from, to, base, mode, only}`.
//! `base = "project"` makes an entry stage 2 (written into the project
//! directory after the clone). The settings file takes the modes `0600`
//! (default) and `0400`; [`Inputs`] also carries `0644`, which a hosted
//! source may set.

use std::fmt;

use shed_provision::{
    limits::MAX_FILE_BYTES, valid_name, Base, Context, Contribution, Mode, Reader,
};

use crate::common::{desktop_provider, file_draft, read_setting_file, Redacted};
use crate::settings::FilesSettings;
use crate::Gathered;

/// The provider's name.
pub const NAME: &str = "files";

/// One file to deliver.
#[derive(Clone, PartialEq, Eq)]
pub struct FileInput {
    /// The item's name.
    pub name: String,
    /// Where it lands, relative to `base`.
    pub target: String,
    /// `home` (stage 1) or `project` (stage 2).
    pub base: Base,
    /// Its mode: `0400`, `0600` or `0644`.
    pub mode: Mode,
    /// Its content.
    pub bytes: Vec<u8>,
}

impl fmt::Debug for FileInput {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("FileInput")
            .field("name", &self.name)
            .field("target", &self.target)
            .field("base", &self.base)
            .field("mode", &self.mode)
            .field("bytes", &Redacted(self.bytes.len()))
            .finish()
    }
}

/// What the provider delivers from: the files, in order.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Inputs {
    /// The files, in the order they are delivered.
    pub files: Vec<FileInput>,
}

desktop_provider!(
    /// The `files` provider.
    Files,
    FilesSettings
);

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

/// Reads the entries `settings` lists. An entry its own `only` leaves out is
/// not read at all; one that cannot be read is a problem and the others are
/// kept. Each is named from its target, in order after each read: the
/// entry's index when that name is not valid or an entry read before took
/// it.
pub fn gather(
    settings: &FilesSettings,
    ctx: &Context<'_>,
    reader: &dyn Reader,
) -> Gathered<Inputs> {
    let mut inputs = Inputs::default();
    let mut problems = Vec::new();
    for (i, e) in settings.files.iter().enumerate() {
        if e.only.as_ref().is_some_and(|o| !ctx.allows(o)) {
            continue;
        }
        let f = match read_setting_file(
            ctx,
            reader,
            &e.from,
            MAX_FILE_BYTES,
            &format!("files.file[{i}].from"),
        ) {
            Ok(f) => f,
            Err(p) => {
                problems.extend(p.problems);
                continue;
            }
        };
        let mut name = item_name(&e.to);
        if !valid_name(&name) || inputs.files.iter().any(|d| d.name == name) {
            name = format!("{NAME}.{i}");
        }
        inputs.files.push(FileInput {
            name,
            target: e.to.clone(),
            base: e.base,
            mode: if e.mode.as_deref() == Some("0400") {
                Mode::M0400
            } else {
                Mode::M0600
            },
            bytes: f.bytes,
        });
    }
    Gathered {
        inputs: Some(inputs),
        problems,
    }
}

/// The items `inputs` make: one file each, a `project` file at stage 2. The
/// target, the name and the size are held to the contract's rules where the
/// answer is assembled.
pub fn build(inputs: &Inputs, _ctx: &Context<'_>) -> Contribution {
    Contribution {
        items: inputs
            .files
            .iter()
            .map(|f| file_draft(&f.name, f.base, &f.target, f.mode, &f.bytes))
            .collect(),
        ..Contribution::default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_provision::ItemBody;

    #[test]
    fn names() {
        assert_eq!(item_name(".npmrc"), "files..npmrc");
        assert_eq!(
            item_name("Config/App Settings.json"),
            "files.config-app-settings.json"
        );
        assert_eq!(item_name(&"a".repeat(100)).len(), 64);
    }

    #[test]
    fn debug_carries_no_content() {
        let i = Inputs {
            files: vec![FileInput {
                name: "files.x".into(),
                target: "x".into(),
                base: Base::Home,
                mode: Mode::M0644,
                bytes: b"SENTINEL".to_vec(),
            }],
        };
        let shown = format!("{i:?}");
        assert!(
            !shown.contains("SENTINEL") && shown.contains("<8 bytes>"),
            "{shown}"
        );
        // 0644 is carried through.
        let c = build(&i, &Context::preview(0));
        let ItemBody::File(f) = &c.items[0].body else {
            panic!()
        };
        assert_eq!(f.mode, Mode::M0644);
    }
}
