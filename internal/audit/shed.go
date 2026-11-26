package audit

import "context"

// The shed lifecycle and delivery events (plan 039 §3.6): what was done to a
// shed and what became of its deliveries. Unlike the older kinds, every one
// names the shed by its permanent id as well as its name, and carries whose
// the shed was when the event was written (its user, or the service client
// that owns it), so the activity feed can show a person their own sheds'
// events by that, never by whose a shed with the name is now.
//
// None carries a delivered value or anything computed from one: a delivery
// event holds the record's states, reasons, the source machine, the item
// names with their results and the fingerprints the source chose.

// Event names of the lifecycle and delivery kinds.
const (
	EventShedCreate = "shed.create"
	EventShedStart  = "shed.start"
	EventShedStop   = "shed.stop"
	EventShedDelete = "shed.delete"
	EventShedReset  = "shed.reset"
	EventShedAssign = "shed.assign"
	// EventShedExtend is a cloud shed's lease moved later (plan 041).
	EventShedExtend     = "shed.extend"
	EventProvisionStart = "shed.provision.start"
	EventProvisionEnd   = "shed.provision.end"
	EventDelivery       = "delivery"
)

// ShedEventKinds is every kind that names its shed's id and whose it was.
var ShedEventKinds = []string{
	EventShedCreate, EventShedStart, EventShedStop, EventShedDelete, EventShedReset,
	EventShedAssign, EventShedExtend, EventProvisionStart, EventProvisionEnd, EventDelivery,
}

// IsShedEventKind reports whether event is one of ShedEventKinds.
func IsShedEventKind(event string) bool {
	for _, k := range ShedEventKinds {
		if k == event {
			return true
		}
	}
	return false
}

// Results of a lifecycle event: the action was carried out (or, on a
// backend whose lifecycle is asynchronous, accepted), or it failed. A
// failure's error text is not recorded: it can carry a repository URL.
const (
	ResultOK     = "ok"
	ResultFailed = "failed"
)

// ShedRef is the shed an event is about, as it was when the event was
// written: its name, its permanent id ("" for a shed made before ids, or a
// create that failed before one was given), its user ("" for a service
// client's shed, an unassigned one, or the implicit user's) and the service
// client that owns it.
type ShedRef struct {
	Shed      string `json:"shed"`
	ShedID    string `json:"shed_id,omitempty"`
	ShedUser  string `json:"shed_user,omitempty"`
	ShedOwner string `json:"shed_owner,omitempty"`
}

// ShedEvent is one lifecycle action on a shed, once the access rule let it
// through: Identity is who acted (the actor), Result how it ended. A call
// the access rule refused writes none.
type ShedEvent struct {
	Event    string   `json:"event"`
	Identity Identity `json:"identity"`
	ShedRef
	Result string `json:"result"`
	// Cause is, for a delete the server did not make (a cloud shed whose
	// machine the account service deleted), why: lease_ended, page,
	// external or server.
	Cause string `json:"cause,omitempty"`
}

// DeliveryEvent is the outcome of one delivery: a create's, or a
// provision's. Identity is who caused it.
type DeliveryEvent struct {
	Event    string   `json:"event"`
	Identity Identity `json:"identity"`
	ShedRef
	Delivery DeliveryOutcome `json:"delivery"`
}

// DeliveryOutcome is a delivery's record as an event carries it. Reason is
// create or provision. Interrupted is set when the run ended before its last
// step (the shed stopped, changed or went, or the server stopped): the rest
// is the record as it last saved it. Problems is how many problems the
// source reported (their text is not kept here).
type DeliveryOutcome struct {
	Reason      string           `json:"reason,omitempty"`
	State       string           `json:"state"`
	Outcome     string           `json:"outcome,omitempty"`
	Interrupted bool             `json:"interrupted,omitempty"`
	Failure     *DeliveryFailure `json:"failure,omitempty"`
	Source      *DeliverySource  `json:"source,omitempty"`
	Steps       *DeliverySteps   `json:"steps,omitempty"`
	Items       []DeliveryItem   `json:"items,omitempty"`
	Problems    int              `json:"problems,omitempty"`
}

// DeliveryFailure is why a delivery failed: the reason, the contract rule an
// answer broke, or the item a write failed at and its result.
type DeliveryFailure struct {
	Reason string `json:"reason"`
	Rule   string `json:"rule,omitempty"`
	Item   string `json:"item,omitempty"`
	Result string `json:"result,omitempty"`
}

// DeliverySource is the source a delivery's request went to: its kind
// (machine or service) and, for a machine, its id and name.
type DeliverySource struct {
	Kind        string `json:"kind"`
	MachineID   string `json:"machine_id,omitempty"`
	MachineName string `json:"machine_name,omitempty"`
}

// DeliverySteps is each tracked step's state.
type DeliverySteps struct {
	Stage1  DeliveryStep `json:"stage1"`
	Clone   DeliveryStep `json:"clone"`
	Stage2  DeliveryStep `json:"stage2"`
	Install DeliveryStep `json:"install"`
}

// DeliveryStep is one step's state and, when skipped or failed, why.
type DeliveryStep struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// DeliveryItem is one item of the record: its name and what became of it,
// never its content, value or target.
type DeliveryItem struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Stage       int    `json:"stage"`
	Kind        string `json:"kind"`
	Result      string `json:"result"`
	Fingerprint string `json:"fingerprint,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

// StateOnly is o with everything but its state withheld: what a caller who
// may not enter the shed sees, as the shed document shows it.
func (o DeliveryOutcome) StateOnly() DeliveryOutcome {
	return DeliveryOutcome{State: o.State}
}

type actorKey struct{}

// WithActor returns ctx carrying id as the actor of what is done with it: the
// front door that names the caller sets it, and the access guard and the
// delivery runner write it in their events.
func WithActor(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, actorKey{}, id)
}

// ActorFrom returns the actor on ctx.
func ActorFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(actorKey{}).(Identity)
	return id, ok
}
