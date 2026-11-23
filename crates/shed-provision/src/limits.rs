//! The contract's limits and transport names (`CONTRACT.md`, "Limits" and
//! section 3). Every size is in bytes; a size of content is of the decoded
//! bytes.

/// The contract number every request and answer carries.
pub const VERSION: u32 = 1;

/// The largest answer body the server reads: a body over it is refused
/// (`413`, `body_too_large`) before it is read.
pub const MAX_BODY_BYTES: usize = 6 << 20;
/// The most items one answer may hold.
pub const MAX_ITEMS: usize = 64;
/// The most problems one answer may hold.
pub const MAX_PROBLEMS: usize = 16;
/// The most decoded content one file item may hold.
pub const MAX_FILE_BYTES: usize = 1 << 20;
/// The most decoded content all of an answer's file items may hold together.
pub const MAX_TOTAL_FILE_BYTES: usize = 4 << 20;
/// The most an answer's env items may take written as the env file: one
/// `VARIABLE=value` line each, with its line feed.
pub const MAX_ENV_FILE_BYTES: usize = 256 << 10;
/// The longest env value.
pub const MAX_VALUE_BYTES: usize = 8192;
/// The longest file path.
pub const MAX_PATH_BYTES: usize = 255;
/// The longest problem message.
pub const MAX_MESSAGE_BYTES: usize = 256;
/// The longest fingerprint.
pub const MAX_FINGERPRINT_BYTES: usize = 64;
/// The deepest a document may nest: each object and each array is a level,
/// the top-level object level 1.
pub const MAX_NESTING: usize = 16;

/// Bounds a request's user, shed name and shed id.
pub const MAX_NAME_BYTES: usize = 64;
/// Bounds a request's repository URL.
pub const MAX_URL_BYTES: usize = 2048;
/// Bounds a request's repository owner/name.
pub const MAX_REPO_NAME_BYTES: usize = 255;
/// Bounds a request's local directory base name.
pub const MAX_LOCAL_DIR_BYTES: usize = 255;
/// Bounds a request's project directory.
pub const MAX_PROJECT_DIR_BYTES: usize = 1024;

/// The shed user's home directory: where a `base: home` item lands.
pub const HOME_DIR: &str = "/home/shed";

/// The delivery stream: `GET`, server-sent events.
pub const STREAM_PATH: &str = "/api/delivery/requests";
/// Followed by a request id: `POST`, the answer.
pub const ANSWER_PATH_PREFIX: &str = "/api/delivery/requests/";
/// Carries a stream's id: set on the stream's response, sent back on every
/// answer to a request that stream carried.
pub const HEADER_STREAM: &str = "X-Shed-Delivery-Stream";
/// Names the source's machine on both routes.
pub const HEADER_MACHINE_ID: &str = "X-Shed-Machine-Id";
/// The machine's display name, percent-encoded UTF-8.
pub const HEADER_MACHINE_NAME: &str = "X-Shed-Machine-Name";
/// The one event type a stream carries.
pub const EVENT_REQUEST: &str = "request";
/// How often the server writes a keep-alive comment, in seconds.
pub const KEEPALIVE_SECONDS: u64 = 15;
/// A stream that hears nothing for this long may be taken as dead, in
/// seconds (three keep-alives).
pub const STREAM_SILENCE_SECONDS: u64 = 45;

/// The transport's own error codes (lower case on purpose).
pub const CODE_UNKNOWN_REQUEST: &str = "unknown_request";
/// The request was answered already.
pub const CODE_ALREADY_ANSWERED: &str = "already_answered";
/// The request's deadline passed before the answer came.
pub const CODE_DEADLINE_PASSED: &str = "deadline_passed";
/// The answer came from another certificate, machine or stream.
pub const CODE_NOT_STREAM_OWNER: &str = "not_stream_owner";
/// The answer broke a rule; the error names it in `rule`.
pub const CODE_ANSWER_REFUSED: &str = "answer_refused";
