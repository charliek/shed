//! shed-app — the display-free shed-server application layer: the
//! shed-core-backed [`Backend`] (one HTTP client per configured host + the
//! pull-based create store) and the [`AuthModeRegistry`].

pub mod auth_modes;
pub mod backend;
pub mod cloud;
pub mod mock_url;
pub mod timefmt;

pub use auth_modes::{AuthModeRegistry, AuthModeState};
pub use backend::{
    source_error_class, source_error_words, Backend, HostDiskUsage, HostEgressProfiles,
    HostFailure, HostFailureKind, HostSource, RcTarget, Reachability, SourceMachineRow,
    SourceServiceRow, SOURCE_ERROR_CLASSES, SOURCE_SERVICE_URL_MAX_BYTES,
};
pub use cloud::{
    cloud_row, refusal_words, usage_words, CloudActions, CloudCaps, CloudFailure, CloudRow,
    LeaseState,
};
pub use mock_url::mock_url_for;
