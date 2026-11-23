//! shed-lane — pure agent-lane, RC DTO, slash-menu, craze command ladder,
//! shell quoting, and text/time leaf crate.
//!
//! Zero `shed-core`, zero `reqwest`, zero `roost-ipc`, zero `chrono` —
//! Android-clean and shared by every shed client and adapter.

pub mod backoff;
pub mod craze;
pub mod craze_rows;
pub mod lane;
pub mod lane_view;
pub mod rc;
pub mod shell_quote;
pub mod slash_menu;
pub mod text;
pub mod time;

pub use craze_rows::{fold_plan, FoldPlan, RoostTabRef};
pub use lane_view::{LaneView, LaneViewSnapshot, MAX_VIEW_MESSAGES};
pub use shell_quote::{display_line, shell_quote_always};
pub use slash_menu::{
    SlashAccept, SlashAcceptKey, SlashAction, SlashDispatch, SlashFilter, SlashFilterRow,
    SlashHidden, SlashMenu, SlashMenuRow,
};
