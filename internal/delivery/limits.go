package delivery

import "time"

// Version is the contract number every request and answer carries.
const Version = 1

// Limits of the contract. Every size is in bytes; a size of content is of the
// decoded bytes.
const (
	// MaxBodyBytes is the largest answer body the transport reads: a body
	// over it is refused (413) before it is read.
	MaxBodyBytes = 6 << 20
	// MaxItems is the most items one answer may hold.
	MaxItems = 64
	// MaxProblems is the most problems one answer may hold.
	MaxProblems = 16
	// MaxFileBytes is the most decoded content one file item may hold.
	MaxFileBytes = 1 << 20
	// MaxTotalFileBytes is the most decoded content all of an answer's file
	// items may hold together.
	MaxTotalFileBytes = 4 << 20
	// MaxValueBytes is the longest env value.
	MaxValueBytes = 8192
	// MaxPathBytes is the longest file path.
	MaxPathBytes = 255
	// MaxMessageBytes is the longest problem message.
	MaxMessageBytes = 256
	// MaxFingerprintBytes is the longest fingerprint.
	MaxFingerprintBytes = 64
	// MaxNesting is the deepest a document may nest: each object and each
	// array is a level, the top-level object level 1.
	MaxNesting = 16

	// MaxNameBytes bounds a request's user, shed name and shed id.
	MaxNameBytes = 64
	// MaxURLBytes bounds a request's repo URL.
	MaxURLBytes = 2048
	// MaxRepoNameBytes bounds a request's repo owner/name.
	MaxRepoNameBytes = 255
	// MaxLocalDirBytes bounds a request's local_dir base name.
	MaxLocalDirBytes = 255
	// MaxProjectDirBytes bounds a request's project_dir.
	MaxProjectDirBytes = 1024
)

// KeepaliveInterval is how often the server writes a comment line on a
// delivery stream.
const KeepaliveInterval = 15 * time.Second

// RememberAnswered is the least time the server keeps an ended request's
// outcome, so a repeated POST is told already_answered or deadline_passed
// rather than unknown_request.
const RememberAnswered = 5 * time.Minute

// The transport's routes, header and event.
const (
	// StreamPath is the delivery stream: GET, server-sent events.
	StreamPath = "/api/delivery/requests"
	// AnswerPathPrefix is followed by a request id: POST, the answer.
	AnswerPathPrefix = "/api/delivery/requests/"
	// HeaderStream carries a stream's id: the server sets it on the stream's
	// response, and a source sends it back on every answer to a request
	// that stream carried.
	HeaderStream = "X-Shed-Delivery-Stream"
	// HeaderMachineID and HeaderMachineName name the source's machine on
	// both routes.
	HeaderMachineID   = "X-Shed-Machine-Id"
	HeaderMachineName = "X-Shed-Machine-Name"
	// EventRequest is the one event type a stream carries.
	EventRequest = "request"
	// KeepaliveComment is the comment line the server writes every
	// KeepaliveInterval.
	KeepaliveComment = ": keepalive\n\n"
)

// The transport's error codes. They are lowercase on purpose: they are this
// contract's own, unlike the API's UPPER_SNAKE codes.
const (
	CodeUnknownRequest  = "unknown_request"
	CodeAlreadyAnswered = "already_answered"
	CodeDeadlinePassed  = "deadline_passed"
	CodeNotStreamOwner  = "not_stream_owner"
	CodeAnswerRefused   = "answer_refused"
)
