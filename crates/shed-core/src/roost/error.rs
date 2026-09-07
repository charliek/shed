//! What a roost exchange can go wrong with.
//!
//! Split the way `HubError` is split, and for the same reason: a client has to
//! tell "nothing is listening" (degrade quietly, keep the row, say why) from
//! "something answered, and answered badly" (say so loudly). The two extra
//! variants here — [`RoostError::ProtocolMismatch`] and
//! [`RoostError::NotASession`] — are the compatibility gate's, and they exist as
//! named variants precisely so a caller never has to string-match a refusal to
//! decide whether the thing on the other end is a roost-session at all.

use roost_ipc::client::ServerCode;
use roost_ipc::ClientError;

/// A failure from one roost request, or from the dial that preceded it.
#[derive(Debug, thiserror::Error)]
pub enum RoostError {
    /// Nothing to talk to: no socket at the path, the dial was refused, or the
    /// connection ended before the first reply. **Quiet** — a caller shows this
    /// as an unreachable row with a reason, not as an error dialog. The string
    /// names what was tried, because "no roost-session" with no path in it is
    /// the single least actionable message this module could produce.
    #[error("{0}")]
    Unavailable(String),
    /// The session speaks a different `session.identify` protocol than this
    /// build. Refused by name rather than limped through: the lease semantics
    /// and the lease-gated op set are exactly what the number covers.
    #[error(
        "this roost-session speaks session protocol {theirs}, this build speaks {ours} \
         (upgrade whichever is older; shed pins roost-ipc by rev)"
    )]
    ProtocolMismatch { theirs: u32, ours: u32 },
    /// The socket answered `unknown-op` to `session.identify` — roost's own
    /// documented way of saying "I am a UI socket". A UI socket must never be
    /// read as session inventory: it serves no event stream, its `tab.list`
    /// carries no `revision`, and its tabs are somebody's desktop window.
    #[error("the socket is a roost UI socket, not a roost-session (it has no session.identify)")]
    NotASession,
    /// A refusal the session minted, kept whole. `code` is a stable kebab-case
    /// string; map it with [`roost_ipc::client::ServerCode`] rather than
    /// comparing spellings at each decision point.
    #[error("roost refused: {code} — {message}")]
    Server { code: String, message: String },
    /// The event stream skipped a revision. The only loss signal the protocol
    /// offers, and the cue to resync (fresh `tab.list`, re-subscribe, re-fence)
    /// rather than to guess at what was missed.
    #[error("the roost event stream skipped a revision: expected {expected}, got {got}")]
    RevisionGap { expected: u64, got: u64 },
    /// Something arrived that could not be understood: a decode failure, a
    /// mismatched response id, an oversized frame. Client/server schema drift,
    /// not a dead wire.
    #[error("roost wire error: {0}")]
    Wire(String),
}

impl RoostError {
    /// The typed refusal, when this error is a server-minted one.
    pub fn server_code(&self) -> Option<ServerCode> {
        match self {
            RoostError::Server { code, .. } => Some(ServerCode::from_wire(code)),
            _ => None,
        }
    }

    /// Whether this is the quiet "nothing there" case a watcher backs off on
    /// without shouting.
    pub fn is_unavailable(&self) -> bool {
        matches!(self, RoostError::Unavailable(_))
    }
}

impl From<ClientError> for RoostError {
    fn from(e: ClientError) -> RoostError {
        match e {
            // A transport-level failure and a mid-request close are the same
            // thing to a caller: there is nothing to talk to right now.
            ClientError::Disconnected => {
                RoostError::Unavailable("the roost connection closed before the reply".into())
            }
            ClientError::Io(roost_ipc::Error::Io(io)) => {
                RoostError::Unavailable(format!("roost connection: {io}"))
            }
            ClientError::Io(roost_ipc::Error::UnexpectedEof) => {
                RoostError::Unavailable("the roost connection ended mid-frame".into())
            }
            ClientError::Io(other) => RoostError::Wire(other.to_string()),
            ClientError::Protocol(e) => RoostError::Wire(e.to_string()),
            ClientError::Server { code, message } => RoostError::Server { code, message },
            ClientError::IdMismatch { expected, got } => RoostError::Wire(format!(
                "response id mismatch: expected {expected}, got {got}"
            )),
            ClientError::RevisionGap { expected, got } => RoostError::RevisionGap { expected, got },
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_dead_wire_is_quiet_and_a_bad_reply_is_not() {
        let dead: RoostError = ClientError::Disconnected.into();
        assert!(dead.is_unavailable(), "a mid-request close is Unavailable");

        let refused: RoostError = ClientError::Io(roost_ipc::Error::Io(std::io::Error::from(
            std::io::ErrorKind::ConnectionRefused,
        )))
        .into();
        assert!(refused.is_unavailable(), "a refused dial is Unavailable");

        let drifted: RoostError = ClientError::Protocol(roost_ipc::Error::UnexpectedEof).into();
        assert!(!drifted.is_unavailable(), "schema drift is not Unavailable");
        assert!(matches!(drifted, RoostError::Wire(_)));
    }

    #[test]
    fn a_server_refusal_keeps_its_typed_code() {
        let err: RoostError = ClientError::Server {
            code: "not-found".into(),
            message: "no such tab".into(),
        }
        .into();
        assert_eq!(err.server_code(), Some(ServerCode::NotFound));
        assert_eq!(RoostError::NotASession.server_code(), None);
    }

    #[test]
    fn a_revision_gap_survives_the_mapping() {
        let err: RoostError = ClientError::RevisionGap {
            expected: 43,
            got: 46,
        }
        .into();
        match err {
            RoostError::RevisionGap { expected, got } => {
                assert_eq!((expected, got), (43, 46));
            }
            other => panic!("expected RevisionGap, got {other:?}"),
        }
    }
}
