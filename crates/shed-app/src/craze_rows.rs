//! **The craze row merge** (plan 025 §3.6.3, D4) — one rule, shared by every
//! client that shows a machine's roost tabs beside its craze hub's sessions.
//!
//! A craze session can be seen twice on one machine: as a row of craze's hub
//! roster (the session itself, with craze's status), and as the roost tab a
//! craze TUI runs in — which roost reports as owned `(craze, X)`, `X` being the
//! provider session id the TUI claimed. D4 says which one is the row, literally:
//!
//! * **for craze sessions, status is craze's** — the hub row IS the row;
//! * a roost tab craze owns **folds into the hub row it names** (the row gains
//!   the tab's id, so its terminal actions have a tab to act on);
//! * roost's row **stands alone only when the hub feed is down**.
//!
//! [`fold_plan`] is that rule as a pure function over one machine: the craze
//! tabs roost lists ([`RoostTabRef`]) and the hub's rows, or `None` while the
//! hub feed is not live (dormant, offline, never reached). It answers which
//! tabs are absorbed — attached to a hub row, or hidden — and every client
//! applies the same answer. It lives here, ungated, for the reason
//! [`crate::lane_view`] does: shed-mobile links this crate with default
//! features and folds the same two feeds, and two copies of "which roost tab
//! is a craze session" are how the desktop and the phone come to show
//! different rows.
//!
//! ## The rule, as implemented
//!
//! * **The feed down (`hub: None`) absorbs nothing.** roost's craze rows show
//!   exactly as they would with no craze source at all.
//! * **The feed live (`hub: Some`) absorbs EVERY craze-owned tab** — D4 applied
//!   literally (plan 025 panel, Codex B). A tab owned `(craze, X)` attaches to
//!   the hub row whose `provider_session_id` is `X`; one with no such row is
//!   HIDDEN, not shown as a roost row (the ~1 s race while the hub polls a new
//!   host, or a TUI run with craze's control socket off — plan 025 §9).
//! * **Only craze ownership folds.** A tab of another source whose session id
//!   happens to equal `X` is never absorbed — [`RoostTabRef::craze_owner`] is
//!   `Some` only for a craze-owned tab.
//! * **A fold never crosses machines** — the plan is computed per machine, from
//!   that machine's tabs and that machine's hub, and nothing here can see a
//!   second one.
//! * **Determinism.** Two craze tabs owned by one `X` (a session opened in a
//!   terminal twice): the NEWEST tab id attaches and the others are hidden. Two
//!   hub rows claiming one `X` (a re-hosted session, both briefly listed): the
//!   tab attaches to the row with the newer `since`, else `startedAt`
//!   ([`LaneSession::last_change_unix_ms`], which is exactly "since, else
//!   startedAt"), ties broken by the greater hostId. A row with no time at all
//!   is older than any row with one.
//!
//! Every craze-owned tab therefore lands in exactly one of
//! [`FoldPlan::absorbed`] and [`FoldPlan::hidden`] while the feed is live, and
//! `absorbed` attaches at most one tab to any hub row.

use std::collections::{BTreeMap, BTreeSet};

use shed_core::lane::LaneSession;

/// One roost tab on the machine being folded, as the merge needs it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RoostTabRef {
    /// roost's tab id — what an absorbed tab hands its hub row.
    pub tab_id: i64,
    /// `Some(X)` iff the tab is owned by craze (`ownership.source == "craze"`,
    /// [`shed_core::rc::RcKind::Craze`]) and `X` is its ownership session id —
    /// the provider session id the TUI claimed. `None` for every other tab,
    /// whatever its session id says.
    pub craze_owner: Option<String>,
}

/// What [`fold_plan`] decided for one machine.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct FoldPlan {
    /// roost tab id → the hub row (its `hostId`) the tab attaches to. At most
    /// one tab per hub row; that row is shown with the tab's id, and the tab is
    /// not shown as a roost row.
    pub absorbed: BTreeMap<i64, String>,
    /// Craze tabs absorbed with no hub row to attach to: not shown at all.
    pub hidden: BTreeSet<i64>,
}

impl FoldPlan {
    /// Whether roost's row for `tab_id` is folded away (absorbed or hidden).
    pub fn folds(&self, tab_id: i64) -> bool {
        self.absorbed.contains_key(&tab_id) || self.hidden.contains(&tab_id)
    }

    /// The tab attached to the hub row `host_id`, if any.
    pub fn tab_of(&self, host_id: &str) -> Option<i64> {
        self.absorbed
            .iter()
            .find_map(|(tab, host)| (host == host_id).then_some(*tab))
    }
}

/// The merge for ONE machine (the module doc): its roost tabs, and its hub's
/// rows when — and only when — the hub feed is live.
pub fn fold_plan(roost: &[RoostTabRef], hub: Option<&[LaneSession]>) -> FoldPlan {
    let mut plan = FoldPlan::default();
    let Some(hub) = hub else {
        return plan;
    };

    // The craze tabs per owner, newest tab id first.
    let mut owned: BTreeMap<&str, Vec<i64>> = BTreeMap::new();
    for tab in roost {
        if let Some(owner) = tab.craze_owner.as_deref() {
            owned.entry(owner).or_default().push(tab.tab_id);
        }
    }

    for (owner, mut tabs) in owned {
        tabs.sort_unstable_by(|a, b| b.cmp(a));
        let target = hub
            .iter()
            .filter(|row| row.provider_session_id.as_deref() == Some(owner))
            .max_by(|a, b| {
                a.last_change_unix_ms
                    .cmp(&b.last_change_unix_ms)
                    .then_with(|| a.id.cmp(&b.id))
            });
        let mut tabs = tabs.into_iter();
        match target {
            Some(row) => {
                if let Some(newest) = tabs.next() {
                    plan.absorbed.insert(newest, row.id.clone());
                }
                plan.hidden.extend(tabs);
            }
            None => plan.hidden.extend(tabs),
        }
    }
    plan
}

#[cfg(test)]
mod tests {
    use super::*;
    use shed_core::rc::RcActivity;

    fn craze_tab(tab_id: i64, owner: &str) -> RoostTabRef {
        RoostTabRef {
            tab_id,
            craze_owner: Some(owner.to_string()),
        }
    }

    fn other_tab(tab_id: i64) -> RoostTabRef {
        RoostTabRef {
            tab_id,
            craze_owner: None,
        }
    }

    fn hub_row(host_id: &str, provider_session_id: &str, since: Option<i64>) -> LaneSession {
        LaneSession {
            id: host_id.to_string(),
            title: host_id.to_string(),
            cwd: "/work".to_string(),
            activity: RcActivity::Idle,
            pending_approvals: 0,
            approximate: false,
            parent_id: None,
            last_change_unix_ms: since,
            provider: Some("grok".to_string()),
            model: None,
            doing: None,
            head_ask_summary: None,
            last_reply: None,
            since_unix_ms: since,
            attached: None,
            start_error: None,
            provider_session_id: Some(provider_session_id.to_string()),
            permission_mode: None,
            tab_id: None,
        }
    }

    #[test]
    fn a_feed_that_is_not_live_absorbs_nothing() {
        let tabs = [craze_tab(4, "ses-a"), other_tab(5)];
        assert_eq!(fold_plan(&tabs, None), FoldPlan::default());
    }

    #[test]
    fn a_live_feed_attaches_a_craze_tab_to_the_hub_row_it_names() {
        let tabs = [craze_tab(4, "ses-a"), other_tab(5)];
        let hub = [hub_row("aaaaaaaaaaaa", "ses-a", Some(10))];
        let plan = fold_plan(&tabs, Some(&hub));
        assert_eq!(
            plan.absorbed,
            BTreeMap::from([(4, "aaaaaaaaaaaa".to_string())])
        );
        assert!(plan.hidden.is_empty());
        assert!(plan.folds(4));
        assert!(!plan.folds(5), "a plain tab is never folded");
        assert_eq!(plan.tab_of("aaaaaaaaaaaa"), Some(4));
    }

    /// D4 literally: with the feed live, EVERY craze tab is absorbed — one the
    /// hub has no row for is hidden, never shown as roost's row.
    #[test]
    fn an_unmatched_craze_tab_is_hidden_while_the_feed_is_live() {
        let tabs = [craze_tab(4, "ses-unknown")];
        let hub = [hub_row("aaaaaaaaaaaa", "ses-a", Some(10))];
        let plan = fold_plan(&tabs, Some(&hub));
        assert!(plan.absorbed.is_empty());
        assert_eq!(plan.hidden, BTreeSet::from([4]));
        // And with an EMPTY live roster too: live is live.
        let plan = fold_plan(&tabs, Some(&[]));
        assert_eq!(plan.hidden, BTreeSet::from([4]));
    }

    #[test]
    fn a_tab_of_another_source_with_the_same_session_id_is_never_absorbed() {
        // An opencode tab whose session id happens to be "ses-a": not craze's.
        let tabs = [other_tab(7)];
        let hub = [hub_row("aaaaaaaaaaaa", "ses-a", Some(10))];
        let plan = fold_plan(&tabs, Some(&hub));
        assert_eq!(plan, FoldPlan::default());
    }

    /// Open in terminal twice: the newest tab attaches, the others are
    /// absorbed silently.
    #[test]
    fn two_tabs_of_one_session_attach_the_newest_and_hide_the_rest() {
        let tabs = [
            craze_tab(3, "ses-a"),
            craze_tab(9, "ses-a"),
            craze_tab(6, "ses-a"),
        ];
        let hub = [hub_row("aaaaaaaaaaaa", "ses-a", Some(10))];
        let plan = fold_plan(&tabs, Some(&hub));
        assert_eq!(
            plan.absorbed,
            BTreeMap::from([(9, "aaaaaaaaaaaa".to_string())])
        );
        assert_eq!(plan.hidden, BTreeSet::from([3, 6]));
    }

    /// A re-hosted session, both hosts briefly listed: the tab attaches to the
    /// row with the newer `since`/`startedAt`.
    #[test]
    fn two_hub_rows_claiming_one_session_take_the_newer() {
        let tabs = [craze_tab(4, "ses-a")];
        let old = hub_row("ffffffffffff", "ses-a", Some(10));
        let new = hub_row("111111111111", "ses-a", Some(20));
        for hub in [[old.clone(), new.clone()], [new.clone(), old.clone()]] {
            let plan = fold_plan(&tabs, Some(&hub));
            assert_eq!(
                plan.absorbed,
                BTreeMap::from([(4, "111111111111".to_string())]),
                "the newer row wins, whatever the order"
            );
        }
        // No time beats nothing: a row with a time is newer than one without.
        let timeless = hub_row("ffffffffffff", "ses-a", None);
        let plan = fold_plan(&tabs, Some(&[timeless, new.clone()]));
        assert_eq!(plan.absorbed[&4], "111111111111");
    }

    /// Equal times: the greater hostId, whichever order the rows came in.
    #[test]
    fn a_tie_is_broken_by_the_greater_host_id() {
        let tabs = [craze_tab(4, "ses-a")];
        let a = hub_row("aaaaaaaaaaaa", "ses-a", Some(10));
        let b = hub_row("bbbbbbbbbbbb", "ses-a", Some(10));
        for hub in [[a.clone(), b.clone()], [b.clone(), a.clone()]] {
            let plan = fold_plan(&tabs, Some(&hub));
            assert_eq!(plan.absorbed[&4], "bbbbbbbbbbbb");
        }
    }

    /// Several sessions at once: each tab to its own row, and a hub row no tab
    /// names keeps none.
    #[test]
    fn each_tab_attaches_to_its_own_row() {
        let tabs = [craze_tab(1, "ses-a"), craze_tab(2, "ses-b"), other_tab(3)];
        let hub = [
            hub_row("aaaaaaaaaaaa", "ses-a", Some(1)),
            hub_row("bbbbbbbbbbbb", "ses-b", Some(1)),
            hub_row("cccccccccccc", "ses-c", Some(1)),
        ];
        let plan = fold_plan(&tabs, Some(&hub));
        assert_eq!(
            plan.absorbed,
            BTreeMap::from([
                (1, "aaaaaaaaaaaa".to_string()),
                (2, "bbbbbbbbbbbb".to_string())
            ])
        );
        assert_eq!(plan.tab_of("cccccccccccc"), None);
    }
}
