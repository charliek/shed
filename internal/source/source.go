// Package source keeps the pin: for each user, the one machine that answers
// that user's credential requests on this server.
//
// # The record
//
// A Binding names, for one user, the machine in each of two slots: Delivery
// (the machine the bus delivers the user's requests to) and Signing (the
// machine whose answers it accepts). Both are lists so a later cut can name
// more than one; in this cut each holds exactly one Ref{Kind: "machine"} and
// both always name the same machine. A stored record that breaks that (an
// empty slot, two refs, two different refs, an unknown kind) is refused when
// the store is loaded, never repaired.
//
// Beside the bindings the store remembers, per user, the machines that have
// presented themselves: id, name, the fingerprint of the key that enrolled it
// and when it was first seen. When it was last seen is kept in memory only,
// so a machine that keeps retrying causes no disk write. At most
// MaxMachinesPerUser are remembered per user: when a new one arrives at the
// limit the least recently seen machine that is not the bound one is
// forgotten (a broker with a fresh state directory is a new machine every
// time, so an unbounded list would be a slow leak).
//
// # Rules
//
//   - Admission (Admit): the first machine to connect for a user is bound.
//     After that a machine is admitted only when its id is the bound one; the
//     name and fingerprint do not take part in the match. A new name or
//     fingerprint for a known id is recorded and reported in the Decision so
//     the caller can log it. A machine that is not admitted is still
//     remembered (so the user can move the binding to it), and the Decision
//     names the bound machine for the refusal.
//   - Move (Use): both slots are set to a machine already remembered for the
//     user. Moving to the machine already bound changes nothing and writes
//     nothing. Who may move is the caller's decision; the store records the
//     mover string it is given in BoundBy.
//   - Generation: every bind and every move (and the adoption of the implicit
//     user's binding) takes the next value of one store-wide counter that is
//     saved with the records and never goes back, so a user's generation
//     increases on every bind and move and never repeats for that user, across
//     restarts and after the binding is dropped and made again. (It is not
//     consecutive per user.) Generations live in memory only outside this
//     store; if an operator removes the file, the counter starts again, which
//     is harmless because nothing in a new process remembers an old one. The
//     counter never wraps: at its maximum every operation that needs a new
//     generation fails with ErrGenerationExhausted (a file holding that
//     counter still loads, and everything else keeps working).
//
// # Users
//
// Records are keyed by the user's name. The implicit user of a server with no
// user list, ImplicitUser (""), is a user like any other here.
//
// When a user list is added to a server that had none, the implicit user's
// binding and machines either become the one configured user's
// (AdoptImplicit) or are dropped (Drop(ImplicitUser)); the caller decides
// which under the same one-time rule as the upgrade marker
// (backend.UsersUpgrade). Nothing in this package calls either.
//
// A user removed from the configuration keeps their records until
// RetainUsers drops them. Because the key is a name, a name configured again
// later (the same person or someone else) would inherit those records: their
// machines would be refused, naming the old bound machine, until they moved
// the binding to one of their own. The old machine itself can never be served
// for the new owner unless it authenticates with a key of the new owner,
// because admission is always asked for the caller's own user. To make a
// reused name start clean, whoever opens the store for the bus must call
// RetainUsers at every start, after the one-time adoption step, with the
// configured user names (or only ImplicitUser when there is no user list), so
// the records of a name that is no longer configured are gone before the name
// can come back. RetainUsers refuses an empty list (ErrNoUsersToKeep), so a
// caller that builds the list from a configuration that failed to load cannot
// wipe the store.
//
// # Concurrency and the locking contract
//
// Every method is safe for concurrent use and atomic: it sees one consistent
// state and leaves one. Two first admissions for one user from two ids bind
// exactly one.
//
//   - Reads (Get, Machines, Current) read an immutable snapshot through an
//     atomic pointer: they take no lock, never wait for a writer or for the
//     disk, and may be called while the caller holds any lock (the bus
//     registry's lock included).
//   - Writes (Admit, Use, AdoptImplicit, Drop, RetainUsers) are serialized by
//     one store-wide mutex that is held across the save. A save that fails
//     fails the operation and leaves the in-memory state as it was. Writes
//     must not be called while holding a lock that a path without a deadline
//     waits for (the registry's r.mu, the bridge's b.mu): they may wait for
//     the disk. Seen changes only the in-memory last-seen time and never
//     saves, but it takes the same mutex (so it cannot undo a concurrent
//     change), and so it follows the same rule.
//   - The store takes none of the caller's locks and calls nothing of the
//     caller's, so the caller's own locks always come first.
//
// The per-user critical section (admission, listener registration, moves
// and response acceptance for one user, under one per-user lock) is the
// caller's: the store cannot hold a lock across the caller's own steps.
// The store supports it with the generation. Admit and Use return the
// binding, whose Generation the caller keeps with whatever it registers or
// stamps; Current(user, machineID, generation) reports whether that is still
// the user's binding. The caller holds its per-user lock across Admit and
// registration (and across Use and closing the other machine's listeners),
// and also checks Current under the lock that publishes each effect, so an
// effect that passed admission before a move can never take effect after it.
//
// Two further duties fall on the caller:
//
//   - Every write a remote party can trigger (a new machine id, or a changed
//     name or fingerprint for a known one) is a full durable rewrite of the
//     file under the one store-wide lock, and a refused machine is remembered
//     too. Storage is bounded (MaxMachinesPerUser), write volume is not. The
//     caller rate-limits, per user, the admissions that would write (Admit
//     reports NewMachine, NameChanged and KeyFingerprintChanged), so one
//     user's broker cycling ids or names cannot keep the disk and every other
//     user's admission waiting.
//   - Drop, AdoptImplicit and RetainUsers change bindings like a move does,
//     but return no generation to compare. They run either at start-up,
//     before any listener can register, or under the caller's per-user lock
//     for every user they touch.
//
// # The file
//
// FileBindings keeps the records in one JSON file (in shed-server, the
// backend state directory's source-bindings.json), mode 0600, written whole,
// atomically and durably (a synced temporary file renamed into place and its
// directory synced). The format is versioned (FileVersion); a change of
// format, an added field included, bumps the version.
//
// One store owns the file: OpenFileBindings takes an exclusive advisory lock
// (flock on the file's name plus LockSuffix, a lock file that is never
// removed) for the life of the store, and a second open of the same path, in
// the same process or another, fails with ErrLocked. Close releases it.
//
// Loading is strict, and a damaged file is never repaired or rewritten:
// starting with no bindings instead would let the next machine to connect
// become each user's source. A file that cannot be read, is not valid UTF-8,
// does not parse, lacks any field (or holds null for one), holds an invalid
// record, holds a user's machines without that user's binding, holds more
// than MaxMachinesPerUser machines for a user, or has a newer version fails
// OpenFileBindings. The store never writes such a file: every save is
// checked by decoding it as a load would, before it is written. See
// OpenFileBindings for what an operator does to recover.
//
// A missing file is the first start: OpenFileBindings writes an empty store
// at once and reports it. The store cannot tell a first start from a file
// that was deleted, or left out of a restore of the state directory: in both
// cases no user has a binding, and the first machine to connect for each
// user becomes its source. So the caller logs one warning when the store was
// newly started (StartedEmptyWarning), saying that no source bindings exist,
// that the first machine to connect for each user will become its source,
// and that if the server had bindings before, every user should check
// `shed source status`. Starting empty on purpose is also how an operator
// discards the pins.
package source

import (
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/shed/internal/config"
)

// ImplicitUser is the user of a server with no user list.
const ImplicitUser = ""

// KindMachine is the only Ref kind in this cut.
const KindMachine = "machine"

// BoundByFirstConnect is BoundBy for a binding made by the first machine of
// a user to connect.
const BoundByFirstConnect = "first-connect"

// MaxMachinesPerUser is how many machines the store remembers for one user.
const MaxMachinesPerUser = 32

// MaxMachineNameBytes is the longest machine name, in bytes of UTF-8. It
// matches the broker's limit (crates/shed-broker/src/machine.rs).
const MaxMachineNameBytes = 63

// MaxTextBytes bounds the other strings the store keeps: a key fingerprint
// and the mover recorded in BoundBy.
const MaxTextBytes = 256

var (
	// ErrInvalidMachine: a machine id, name or key fingerprint is not well
	// formed, or the id is empty (the caller answers a missing id itself,
	// before it asks the store).
	ErrInvalidMachine = errors.New("source: invalid machine")
	// ErrInvalidUser: a user name is neither ImplicitUser nor a valid
	// configured user name (AdoptImplicit's target must be a named user).
	ErrInvalidUser = errors.New("source: invalid user")
	// ErrInvalidMover: the mover given to Use is empty or not well formed.
	ErrInvalidMover = errors.New("source: invalid mover")
	// ErrUnknownMachine: Use names a machine this user has never presented
	// (or one forgotten since).
	ErrUnknownMachine = errors.New("source: unknown machine")
	// ErrCorrupt: a stored file does not parse or holds an invalid record.
	ErrCorrupt = errors.New("source: binding store is damaged")
	// ErrNewerVersion: a stored file has a version this build does not know.
	ErrNewerVersion = errors.New("source: binding store was written by a newer shed-server")
	// ErrLocked: another open store (in this process or another) holds the
	// file's lock.
	ErrLocked = errors.New("source: binding store is already open")
	// ErrClosed: a change was asked of a FileBindings after Close.
	ErrClosed = errors.New("source: binding store is closed")
	// ErrGenerationExhausted: the store-wide generation counter is at its
	// maximum, so no bind, move or adoption can take a new generation.
	ErrGenerationExhausted = errors.New("source: binding generation counter is exhausted")
	// ErrNoUsersToKeep: RetainUsers was given no user to keep (which would
	// drop every record). With no user list the caller keeps ImplicitUser.
	ErrNoUsersToKeep = errors.New("source: RetainUsers needs at least one user to keep")
)

// Ref names one thing a slot points at.
type Ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Binding is one user's pin. Delivery and Signing each hold exactly one
// machine Ref, the same one.
type Binding struct {
	User     string    `json:"user"`
	Delivery []Ref     `json:"delivery"`
	Signing  []Ref     `json:"signing"`
	BoundAt  time.Time `json:"bound_at"`
	// BoundBy is BoundByFirstConnect, or the mover given to Use.
	BoundBy string `json:"bound_by"`
	// Generation is this binding's place in the store's counter: it
	// increases on every bind and move and never repeats for a user.
	Generation uint64 `json:"generation"`
}

// MachineID is the id of the bound machine.
func (b Binding) MachineID() string {
	if len(b.Signing) == 0 {
		return ""
	}
	return b.Signing[0].ID
}

// clone copies the binding, slots included.
func (b Binding) clone() Binding {
	b.Delivery = append([]Ref(nil), b.Delivery...)
	b.Signing = append([]Ref(nil), b.Signing...)
	return b
}

// newBinding makes a binding of user to machine id.
func newBinding(user, id string, at time.Time, by string, generation uint64) *Binding {
	return &Binding{
		User:       user,
		Delivery:   []Ref{{Kind: KindMachine, ID: id}},
		Signing:    []Ref{{Kind: KindMachine, ID: id}},
		BoundAt:    at,
		BoundBy:    by,
		Generation: generation,
	}
}

// Machine is a machine as a user's broker presented it.
type Machine struct {
	// ID is 32 lowercase hex digits (ValidMachineID).
	ID string
	// Name is 1 to 63 bytes of UTF-8 without control characters
	// (ValidMachineName), already decoded from its header by the caller.
	Name string
	// KeyFingerprint is the fingerprint of the key the machine's credential
	// was enrolled with (empty when the server cannot tell, as in open mode).
	KeyFingerprint string
	// FirstSeen is when the store first remembered it. Set by the store.
	FirstSeen time.Time
	// LastSeen is when it last presented itself to Admit; kept in memory
	// only, so it is zero after a restart until the machine connects again.
	// Set by the store.
	LastSeen time.Time
}

// seen is the time eviction orders machines by.
func (m Machine) seen() time.Time {
	if m.LastSeen.After(m.FirstSeen) {
		return m.LastSeen
	}
	return m.FirstSeen
}

// Decision is the outcome of Admit.
type Decision struct {
	// Admitted: the machine is the user's bound machine.
	Admitted bool
	// Binding is the user's binding after the call; its Generation is what
	// the caller checks with Current.
	Binding Binding
	// Bound is the bound machine (for a refusal, the one to name).
	Bound Machine
	// Machine is the presenting machine as the store now remembers it.
	Machine Machine
	// NewlyBound: this call made the binding (the user had none).
	NewlyBound bool
	// NewMachine: the store had not remembered this id for the user.
	NewMachine bool
	// NameChanged and KeyFingerprintChanged report a change for a known id;
	// the Previous fields hold what was remembered before.
	NameChanged            bool
	PreviousName           string
	KeyFingerprintChanged  bool
	PreviousKeyFingerprint string
	// Evicted are the machines forgotten to stay within MaxMachinesPerUser.
	Evicted []Machine
}

// Move is the outcome of Use.
type Move struct {
	// Moved is false when the machine was already bound: nothing changed.
	Moved bool
	// Binding is the user's binding after the call.
	Binding Binding
	// From is the machine bound before (zero if the user had no binding).
	From Machine
	// To is the machine bound now.
	To Machine
}

// Adoption is the outcome of AdoptImplicit.
type Adoption struct {
	// Adopted: the implicit user's records became the named user's.
	Adopted bool
	// Dropped: the implicit user's records were dropped because the named
	// user already had records of their own, which are kept as they were.
	Dropped bool
	// Binding is the named user's binding after the call, if any.
	Binding *Binding
	// Machines is how many machines were adopted or dropped.
	Machines int
}

// Bindings is the seam the bus and the source routes use.
type Bindings interface {
	// Admit records machine m for user and decides whether it is the user's
	// source, binding it if the user has none. m.ID must be a valid,
	// non-empty id; m.FirstSeen and m.LastSeen are ignored.
	Admit(user string, m Machine) (Decision, error)
	// Use moves user's binding to a machine already remembered for them;
	// by is recorded in BoundBy.
	Use(user, machineID, by string) (Move, error)
	// Get returns the user's binding.
	Get(user string) (Binding, bool)
	// Machines returns the machines remembered for user, oldest first.
	Machines(user string) []Machine
	// Current reports whether user's binding is still to machineID at
	// generation.
	Current(user, machineID string, generation uint64) bool
	// Seen notes that user's machine machineID, which the store already
	// remembers, presented itself now: its last-seen time (kept in memory
	// only, and what eviction orders by) becomes now, and nothing else
	// changes. It never saves, so it cannot fail and writes nothing to disk;
	// it reports whether the store remembers the machine (when it does not,
	// nothing changes). Unlike Admit it records nothing the machine presents
	// (its name and key fingerprint stay as they were). It serializes with the
	// writes, so the calling rule for writes applies to it.
	Seen(user, machineID string) bool
	// AdoptImplicit makes the implicit user's binding and machines the named
	// user's, or drops them when that user already has records.
	AdoptImplicit(user string) (Adoption, error)
	// Drop forgets user's binding and machines; it reports whether there
	// were any.
	Drop(user string) (bool, error)
	// RetainUsers forgets the records of every user not in keep and returns
	// their names. An empty keep is refused with ErrNoUsersToKeep.
	RetainUsers(keep []string) ([]string, error)
}

// Options configure a store.
type Options struct {
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

var machineIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidMachineID reports whether id is 32 lowercase hex digits.
func ValidMachineID(id string) bool { return machineIDRe.MatchString(id) }

// ValidMachineName reports whether name is 1 to MaxMachineNameBytes bytes of
// UTF-8 with no control character.
func ValidMachineName(name string) bool {
	return name != "" && len(name) <= MaxMachineNameBytes && validText(name)
}

// validText reports whether s is UTF-8 with no control character.
func validText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validKeyFingerprint reports whether fp is empty or short text.
func validKeyFingerprint(fp string) bool {
	return len(fp) <= MaxTextBytes && validText(fp)
}

// validMover reports whether by can be recorded in BoundBy.
func validMover(by string) bool {
	return by != "" && len(by) <= MaxTextBytes && validText(by)
}

// checkUser accepts ImplicitUser or a valid user name.
func checkUser(user string) error {
	if user == ImplicitUser || config.ValidUserName(user) {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrInvalidUser, user)
}

// checkMachine validates what a caller presents to Admit.
func checkMachine(m Machine) error {
	switch {
	case !ValidMachineID(m.ID):
		return fmt.Errorf("%w: id %q is not 32 lowercase hex digits", ErrInvalidMachine, m.ID)
	case !ValidMachineName(m.Name):
		return fmt.Errorf("%w: name is not 1 to %d bytes of UTF-8 without control characters", ErrInvalidMachine, MaxMachineNameBytes)
	case !validKeyFingerprint(m.KeyFingerprint):
		return fmt.Errorf("%w: key fingerprint is not up to %d bytes of UTF-8 without control characters", ErrInvalidMachine, MaxTextBytes)
	}
	return nil
}
