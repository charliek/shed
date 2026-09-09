//! **The transport hook** — the thing that keeps forward repair out of the
//! event stream.
//!
//! A gx lane has two URLs (the module doc on [`crate::discovery`] says why they
//! are never conflated). The **reported** one is fixed for the life of the
//! adapter; the **dial** one is not. Over SSH the client runs an `ssh -N -L`
//! forward, and a forward dies — the child is killed, the network blips, the
//! local port is re-reserved. When it does, the adapter must be able to
//! reconnect to the NEW local port without the contract growing a frame for it.
//!
//! [`shed_core::lane`]'s correction 1 is the rule: *transport repair is not a
//! `LaneEvent`*. An adapter that reconnects calls a client-supplied hook before
//! every connect attempt, and the client re-`ensure`s whatever it owns inside
//! that hook. The desktop's implementation (C4) re-reserves the forward and
//! answers `http://127.0.0.1:<local>/`; a local lane's answers the reported URL
//! unchanged; [`FixedDial`] answers a constant.
//!
//! # Called before every connect — and every request
//!
//! Plan 017 §3.3 requires `dial()` before every connect attempt: the first
//! verb, every SSE (re)connect, and every verb after a failure. This crate
//! calls it before **every** request instead, because that is strictly more
//! often, needs no bookkeeping to get right, and is what makes a moved forward
//! detectable at the moment it moves rather than at the next failure. A dial URL
//! that differs from the pinned epoch's opens a new epoch (`ensure_pinned`
//! re-pins), which is exactly the behaviour a re-established forward wants.
//!
//! The cost is one `dial()` per request. The desktop's is a cache read behind a
//! lock when nothing is wrong, which is what it must be — a hook that did real
//! work per call would make every verb pay for a tunnel that is fine.

use shed_core::lane::LaneError;

/// Where HTTP actually goes, resolved fresh before each connect.
///
/// Implementations are cheap and idempotent: the adapter calls this a lot.
/// Failure is [`LaneError::Unavailable`] — a tunnel that will not come up is
/// the quiet, render-the-row-stale case, not a loud one.
#[async_trait::async_trait]
pub trait GxTransport: Send + Sync {
    /// The base URL to dial. A trailing slash is normal here and meaningless —
    /// `reqwest::Url` normalises an empty path to `/` — and it is the visible
    /// difference from the slash-free REPORTED URL, which is never a URL this
    /// method returns.
    async fn dial(&self) -> Result<reqwest::Url, LaneError>;
}

/// A transport that always answers the same URL: a lane on this machine, the
/// example, and every test that is not about forward repair.
#[derive(Debug, Clone)]
pub struct FixedDial(reqwest::Url);

impl FixedDial {
    pub fn new(url: reqwest::Url) -> FixedDial {
        FixedDial(url)
    }

    /// Parse a base URL. The error is `Unavailable` rather than `BadRequest`
    /// because the only caller that hits it is a client wiring up a lane whose
    /// URL came off a roost tab — an unusable address there means the lane
    /// cannot be reached, which is what the quiet variant says.
    pub fn parse(url: &str) -> Result<FixedDial, LaneError> {
        reqwest::Url::parse(url)
            .map(FixedDial)
            .map_err(|e| LaneError::Unavailable(format!("gx lane URL {url}: {e}")))
    }

    pub fn url(&self) -> &reqwest::Url {
        &self.0
    }
}

#[async_trait::async_trait]
impl GxTransport for FixedDial {
    async fn dial(&self) -> Result<reqwest::Url, LaneError> {
        Ok(self.0.clone())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn fixed_dial_answers_the_url_it_was_built_on() {
        let t = FixedDial::parse("http://127.0.0.1:2431").expect("parses");
        // `reqwest::Url` normalises the empty path to `/` — which is exactly
        // the visible difference from a REPORTED URL, and why the two are
        // never compared.
        assert_eq!(
            t.dial().await.expect("dials").as_str(),
            "http://127.0.0.1:2431/"
        );
    }

    #[tokio::test]
    async fn an_unparseable_url_is_unavailable_not_failed() {
        let err = FixedDial::parse("not a url").expect_err("refuses");
        assert!(matches!(err, LaneError::Unavailable(_)), "{err:?}");
    }
}
