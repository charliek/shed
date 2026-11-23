//! The provision contract's source side in the desktop (plan 039 §3.7, commit 3).
//!
//! * [`runtime`] — [`DeliveryRuntime`]: one delivery stream per configured server
//!   (`shed-source`), answered from `delivery.toml`'s providers, with its status.
//! * [`reader`] — [`DiskReader`], the `shed_provision::Reader` over the disk and the
//!   person's commands.
//! * [`key`] — the fingerprint key in the state directory.
//! * [`settings_file`] — `delivery.toml` read as a view and changed one provider at a
//!   time.
//! * [`preview`] — the request a shed's document would draw, for `delivery.preview`.
//! * [`activity`] — the servers' activity feeds merged, for `activity.server_list`.
//!
//! The contract's crates are re-exported as [`provision`] and [`providers`], and
//! `shed-source` as [`source`].

pub use shed_provision as provision;
pub use shed_provision_providers as providers;
pub use shed_source as source;

pub mod activity;
pub mod key;
pub mod preview;
pub mod reader;
pub mod runtime;
pub mod settings_file;

pub use reader::DiskReader;
pub use runtime::{DeliveryOptions, DeliveryRuntime};
