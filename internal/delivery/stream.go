package delivery

import "time"

// The server's side of the transport, as the Service sees it: the pin (which
// machine is a user's source, and its open delivery stream) and the stream
// itself. internal/plugin's registry implements both; the Service holds no
// stream of its own and admits nobody.

// Pin is the credential source binding as a delivery needs it: who a user's
// bound machine is, whether it holds its delivery stream now, and whether a
// stamp (machine, generation) is still the user's binding.
// *plugin.Registry satisfies it.
type Pin interface {
	// DeliveryBound returns user's bound machine; false when the user has
	// none, or the server keeps no bindings.
	DeliveryBound(user string) (SourceMachine, bool)
	// DeliveryStream returns user's open delivery stream: the bound
	// machine's, registered at the binding's current generation and not
	// ended. False when there is none.
	DeliveryStream(user string) (Stream, bool)
	// DeliveryCurrent reports whether machineID at generation is still
	// user's binding.
	DeliveryCurrent(user, machineID string, generation uint64) bool
}

// Stream is one open delivery stream: one connection of a user's bound
// machine, on which requests are sent and to which their answers are bound.
type Stream interface {
	// ID is the stream's id, sent to the source in the stream's
	// X-Shed-Delivery-Stream header and echoed on each answer.
	ID() string
	// Machine is the machine that opened the stream.
	Machine() SourceMachine
	// Generation is the binding generation the stream was admitted under.
	Generation() uint64
	// Serial is the client certificate's serial that opened the stream
	// (lowercase hex; empty in open mode, where the machine alone binds).
	Serial() string
	// Done is closed when the stream ends, for any reason: its connection
	// went, a write stalled, the binding moved, the caller no longer
	// authenticates, or the server is shutting down.
	Done() <-chan struct{}
	// Offer queues o for the stream to write, without blocking. It reports
	// false when the stream has ended or its queue is full.
	Offer(o Outgoing) bool
}

// Outgoing is one request on its way to a source: its id, its line (the
// request as compact JSON, the data of one event), and Live, which the
// stream asks just before it writes the line: a request that has ended
// since it was queued (answered, expired, failed) is not sent.
type Outgoing struct {
	ID   string
	Line []byte
	Live func() bool
}

// Answerer is who sent an answer: the caller's user, the machine its headers
// name, its certificate's serial ("" in open mode) and the stream id it
// echoed ("" when it named none).
type Answerer struct {
	User    string
	Machine string
	Serial  string
	Stream  string
}

// AnswerReadTimeout bounds how long the transport reads an answer's body
// once it has claimed the request. A Service waiting for that request waits
// for the judgement at most this long (plus a margin) past its deadline.
const AnswerReadTimeout = 30 * time.Second
