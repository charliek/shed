//! shed-app — the display-free shed-server application layer: the
//! shed-core-backed [`Backend`] (one HTTP client per configured host + the
//! pull-based create store) and the [`AuthModeRegistry`].

pub mod auth_modes;
pub mod backend;
pub mod mock_url;
pub mod timefmt;

pub use auth_modes::{AuthModeRegistry, AuthModeState};
pub use backend::{
    Backend, HostDiskUsage, HostEgressProfiles, HostFailure, HostFailureKind, HostSource, RcTarget,
    Reachability, SourceMachineRow,
};
pub use mock_url::mock_url_for;
