//! The porcelain's view of the **local activity hub** client.
//!
//! The client itself graduated into [`shed_core::hub_client`] in plan 012
//! (roadmap R4) — the desktop and mobile clients read the same four routes off
//! the same wire, and mobile can only reach shed-core's surface, so a
//! porcelain-local copy would have become the odd one out immediately.
//!
//! Re-exported here rather than replaced at the call sites: `sx watch` was its
//! only caller for two plans, so this keeps the graduation a pure move.

pub use shed_core::hub_client::{HubClient, HubError, HUB_PORT};
