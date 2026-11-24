package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// FileVersion is the version of the stored format this build reads and
// writes.
const FileVersion = 1

// Store is the binding rules over an in-memory state that is saved, whole,
// through a persist function before every change takes effect. FileBindings
// is a Store saved to a file; the test double (fake.Bindings) is a Store
// saved to memory. See the package comment for the rules and the locking
// contract.
type Store struct {
	// writeMu serializes every change, across its save.
	writeMu sync.Mutex
	// cur is the current state. A state is never changed once stored: a
	// change builds a copy, saves it, and stores the copy.
	cur     atomic.Pointer[state]
	persist func(data []byte) error
	now     func() time.Time
}

var _ Bindings = (*Store)(nil)

// state is everything the store holds.
type state struct {
	// lastGeneration is the last generation handed out.
	lastGeneration uint64
	users          map[string]*userState
}

// userState is one user's records. A user with neither a binding nor a
// machine has no entry.
type userState struct {
	binding  *Binding
	machines map[string]Machine
}

// NewStore returns an empty Store saved through persist, which receives the
// whole encoded state and must return nil only once it is kept.
func NewStore(persist func(data []byte) error, opts Options) *Store {
	s := &Store{persist: persist, now: opts.Now}
	if s.now == nil {
		s.now = time.Now
	}
	s.cur.Store(&state{users: map[string]*userState{}})
	return s
}

// LoadStore returns a Store holding the state encoded in data (as persist
// last received it), saved through persist. Data that is not a valid store
// of a known version is refused with ErrCorrupt or ErrNewerVersion; nothing
// is repaired.
func LoadStore(data []byte, persist func(data []byte) error, opts Options) (*Store, error) {
	st, err := decode(data)
	if err != nil {
		return nil, err
	}
	s := NewStore(persist, opts)
	s.cur.Store(st)
	return s, nil
}

// Admit implements Bindings.
func (s *Store) Admit(user string, m Machine) (Decision, error) {
	if err := checkUser(user); err != nil {
		return Decision{}, err
	}
	if err := checkMachine(m); err != nil {
		return Decision{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	now := s.now().UTC()
	st := s.cur.Load().clone()
	u := st.users[user]
	if u == nil {
		u = &userState{machines: map[string]Machine{}}
		st.users[user] = u
	}
	var d Decision
	dirty := false
	rec, known := u.machines[m.ID]
	if known {
		if rec.Name != m.Name {
			d.NameChanged, d.PreviousName = true, rec.Name
			rec.Name = m.Name
			dirty = true
		}
		if rec.KeyFingerprint != m.KeyFingerprint {
			d.KeyFingerprintChanged, d.PreviousKeyFingerprint = true, rec.KeyFingerprint
			rec.KeyFingerprint = m.KeyFingerprint
			dirty = true
		}
	} else {
		d.Evicted = u.makeRoom(m.ID)
		rec = Machine{ID: m.ID, Name: m.Name, KeyFingerprint: m.KeyFingerprint, FirstSeen: now}
		d.NewMachine = true
		dirty = true
	}
	rec.LastSeen = now
	u.machines[m.ID] = rec
	if u.binding == nil {
		gen, err := st.nextGeneration()
		if err != nil {
			return Decision{}, err
		}
		u.binding = newBinding(user, m.ID, now, BoundByFirstConnect, gen)
		d.NewlyBound = true
		dirty = true
	}
	if dirty {
		if err := s.save(st); err != nil {
			return Decision{}, err
		}
	}
	s.cur.Store(st)

	d.Binding = u.binding.clone()
	d.Bound = u.machines[d.Binding.MachineID()]
	d.Machine = rec
	d.Admitted = d.Binding.MachineID() == m.ID
	return d, nil
}

// Use implements Bindings.
func (s *Store) Use(user, machineID, by string) (Move, error) {
	if err := checkUser(user); err != nil {
		return Move{}, err
	}
	if !ValidMachineID(machineID) {
		return Move{}, fmt.Errorf("%w: id %q is not 32 lowercase hex digits", ErrInvalidMachine, machineID)
	}
	if !validMover(by) {
		return Move{}, fmt.Errorf("%w: %q", ErrInvalidMover, by)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cur := s.cur.Load()
	u := cur.users[user]
	if u == nil {
		return Move{}, fmt.Errorf("%w: %s", ErrUnknownMachine, machineID)
	}
	to, ok := u.machines[machineID]
	if !ok {
		return Move{}, fmt.Errorf("%w: %s", ErrUnknownMachine, machineID)
	}
	if u.binding != nil && u.binding.MachineID() == machineID {
		return Move{Binding: u.binding.clone(), From: to, To: to}, nil
	}

	st := cur.clone()
	u = st.users[user]
	var from Machine
	if u.binding != nil {
		from = u.machines[u.binding.MachineID()]
	}
	gen, err := st.nextGeneration()
	if err != nil {
		return Move{}, err
	}
	u.binding = newBinding(user, machineID, s.now().UTC(), by, gen)
	if err := s.save(st); err != nil {
		return Move{}, err
	}
	s.cur.Store(st)
	return Move{Moved: true, Binding: u.binding.clone(), From: from, To: to}, nil
}

// Get implements Bindings.
func (s *Store) Get(user string) (Binding, bool) {
	u := s.cur.Load().users[user]
	if u == nil || u.binding == nil {
		return Binding{}, false
	}
	return u.binding.clone(), true
}

// Bound implements Bindings: the binding and the bound machine from one
// snapshot. The bound machine is never evicted, so a valid store always
// remembers it; were it missing the user is reported as not bound.
func (s *Store) Bound(user string) (Binding, Machine, bool) {
	u := s.cur.Load().users[user]
	if u == nil || u.binding == nil {
		return Binding{}, Machine{}, false
	}
	m, ok := u.machines[u.binding.MachineID()]
	if !ok {
		return Binding{}, Machine{}, false
	}
	return u.binding.clone(), m, true
}

// Machines implements Bindings.
func (s *Store) Machines(user string) []Machine {
	u := s.cur.Load().users[user]
	if u == nil {
		return nil
	}
	return u.sortedMachines()
}

// Current implements Bindings.
func (s *Store) Current(user, machineID string, generation uint64) bool {
	u := s.cur.Load().users[user]
	return u != nil && u.binding != nil && machineID != "" &&
		u.binding.MachineID() == machineID && u.binding.Generation == generation
}

// Seen implements Bindings.
func (s *Store) Seen(user, machineID string) bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cur := s.cur.Load()
	u := cur.users[user]
	if u == nil {
		return false
	}
	rec, ok := u.machines[machineID]
	if !ok {
		return false
	}
	rec.LastSeen = s.now().UTC()
	st := cur.clone()
	st.users[user].machines[machineID] = rec
	s.cur.Store(st)
	return true
}

// AdoptImplicit implements Bindings. The adopted binding keeps when and by
// whom it was made and takes a new generation, as a bind for the named user.
func (s *Store) AdoptImplicit(user string) (Adoption, error) {
	if user == ImplicitUser {
		return Adoption{}, fmt.Errorf("%w: the implicit user cannot adopt its own records", ErrInvalidUser)
	}
	if err := checkUser(user); err != nil {
		return Adoption{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cur := s.cur.Load()
	if cur.users[ImplicitUser] == nil {
		return Adoption{}, nil
	}
	st := cur.clone()
	imp := st.users[ImplicitUser]
	delete(st.users, ImplicitUser)
	ad := Adoption{Machines: len(imp.machines)}
	if own := st.users[user]; own != nil {
		ad.Dropped = true
		if own.binding != nil {
			b := own.binding.clone()
			ad.Binding = &b
		}
	} else {
		ad.Adopted = true
		if imp.binding != nil {
			gen, err := st.nextGeneration()
			if err != nil {
				return Adoption{}, err
			}
			imp.binding.User = user
			imp.binding.Generation = gen
			b := imp.binding.clone()
			ad.Binding = &b
		}
		st.users[user] = imp
	}
	if err := s.save(st); err != nil {
		return Adoption{}, err
	}
	s.cur.Store(st)
	return ad, nil
}

// Drop implements Bindings.
func (s *Store) Drop(user string) (bool, error) {
	if err := checkUser(user); err != nil {
		return false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cur := s.cur.Load()
	if cur.users[user] == nil {
		return false, nil
	}
	st := cur.clone()
	delete(st.users, user)
	if err := s.save(st); err != nil {
		return false, err
	}
	s.cur.Store(st)
	return true, nil
}

// RetainUsers implements Bindings.
func (s *Store) RetainUsers(keep []string) ([]string, error) {
	if len(keep) == 0 {
		return nil, ErrNoUsersToKeep
	}
	kept := make(map[string]bool, len(keep))
	for _, user := range keep {
		if err := checkUser(user); err != nil {
			return nil, err
		}
		kept[user] = true
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cur := s.cur.Load()
	var dropped []string
	for user := range cur.users {
		if !kept[user] {
			dropped = append(dropped, user)
		}
	}
	if len(dropped) == 0 {
		return nil, nil
	}
	sort.Strings(dropped)
	st := cur.clone()
	for _, user := range dropped {
		delete(st.users, user)
	}
	if err := s.save(st); err != nil {
		return nil, err
	}
	s.cur.Store(st)
	return dropped, nil
}

// save encodes st and hands it to persist.
func (s *Store) save(st *state) error {
	data, err := encode(st)
	if err != nil {
		return fmt.Errorf("source: encoding bindings: %w", err)
	}
	if err := s.persist(data); err != nil {
		return fmt.Errorf("source: saving bindings: %w", err)
	}
	return nil
}

// nextGeneration takes the next value of the store-wide counter, refusing
// to wrap past its maximum.
func (st *state) nextGeneration() (uint64, error) {
	if st.lastGeneration == math.MaxUint64 {
		return 0, ErrGenerationExhausted
	}
	st.lastGeneration++
	return st.lastGeneration, nil
}

// makeRoom forgets machines until one more fits within MaxMachinesPerUser,
// least recently seen first, never the bound one or the arriving id, and
// returns them.
func (u *userState) makeRoom(arriving string) []Machine {
	var evicted []Machine
	for len(u.machines) >= MaxMachinesPerUser {
		bound := ""
		if u.binding != nil {
			bound = u.binding.MachineID()
		}
		var victim *Machine
		for id, m := range u.machines {
			if id == bound || id == arriving {
				continue
			}
			if victim == nil || olderThan(m, *victim) {
				m := m
				victim = &m
			}
		}
		if victim == nil {
			break
		}
		delete(u.machines, victim.ID)
		evicted = append(evicted, *victim)
	}
	return evicted
}

// olderThan orders machines for eviction: least recently seen first, then
// first seen, then id, so the choice never depends on map order.
func olderThan(a, b Machine) bool {
	if !a.seen().Equal(b.seen()) {
		return a.seen().Before(b.seen())
	}
	if !a.FirstSeen.Equal(b.FirstSeen) {
		return a.FirstSeen.Before(b.FirstSeen)
	}
	return a.ID < b.ID
}

// sortedMachines returns the user's machines, first seen first.
func (u *userState) sortedMachines() []Machine {
	out := make([]Machine, 0, len(u.machines))
	for _, m := range u.machines {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.Before(out[j].FirstSeen)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// clone deep-copies the state.
func (st *state) clone() *state {
	out := &state{lastGeneration: st.lastGeneration, users: make(map[string]*userState, len(st.users))}
	for user, u := range st.users {
		c := &userState{machines: make(map[string]Machine, len(u.machines))}
		if u.binding != nil {
			b := u.binding.clone()
			c.binding = &b
		}
		for id, m := range u.machines {
			c.machines[id] = m
		}
		out.users[user] = c
	}
	return out
}

// --- the stored form ---------------------------------------------------------

// fileData is the stored form.
type fileData struct {
	Version        int             `json:"version"`
	LastGeneration uint64          `json:"last_generation"`
	Bindings       []Binding       `json:"bindings"`
	Machines       []machineRecord `json:"machines"`
}

// machineRecord is a remembered machine as stored (LastSeen is not).
type machineRecord struct {
	User           string    `json:"user"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	KeyFingerprint string    `json:"key_fingerprint"`
	FirstSeen      time.Time `json:"first_seen"`
}

// encode renders st in the stored form, in a stable order, and checks the
// result by decoding it as a load would: the store never writes a file it
// would refuse to load.
func encode(st *state) ([]byte, error) {
	f := fileData{Version: FileVersion, LastGeneration: st.lastGeneration, Bindings: []Binding{}, Machines: []machineRecord{}}
	users := make([]string, 0, len(st.users))
	for user := range st.users {
		users = append(users, user)
	}
	sort.Strings(users)
	for _, user := range users {
		u := st.users[user]
		if u.binding != nil {
			f.Bindings = append(f.Bindings, u.binding.clone())
		}
		for _, m := range u.sortedMachines() {
			f.Machines = append(f.Machines, machineRecord{User: user, ID: m.ID, Name: m.Name, KeyFingerprint: m.KeyFingerprint, FirstSeen: m.FirstSeen})
		}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if _, err := decode(data); err != nil {
		return nil, fmt.Errorf("the state would not load again: %w", err)
	}
	return data, nil
}

// decode reads the stored form strictly: valid UTF-8 (the JSON decoder would
// otherwise replace a damaged byte and accept the result), the version first
// (a newer one is ErrNewerVersion), every field present and not null, then
// the whole document with no unknown field and nothing after it, then every
// record (ErrCorrupt names the first bad one), and last the invariants
// across records: no user has more than MaxMachinesPerUser machines, and a
// user with machines has a binding.
func decode(data []byte) (*state, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: not valid UTF-8", ErrCorrupt)
	}
	var probe struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	switch {
	case probe.Version == nil:
		return nil, fmt.Errorf("%w: no version", ErrCorrupt)
	case *probe.Version > FileVersion:
		return nil, fmt.Errorf("%w (version %d; this build reads version %d)", ErrNewerVersion, *probe.Version, FileVersion)
	case *probe.Version < 1:
		return nil, fmt.Errorf("%w: version %d", ErrCorrupt, *probe.Version)
	}
	if err := checkFieldsPresent(data); err != nil {
		return nil, err
	}
	var f fileData
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: data after the document", ErrCorrupt)
	}

	st := &state{lastGeneration: f.LastGeneration, users: map[string]*userState{}}
	userOf := func(user string) *userState {
		u := st.users[user]
		if u == nil {
			u = &userState{machines: map[string]Machine{}}
			st.users[user] = u
		}
		return u
	}
	for i, r := range f.Machines {
		bad := func(why string) error {
			return fmt.Errorf("%w: machine %d (user %q, id %q): %s", ErrCorrupt, i, r.User, r.ID, why)
		}
		if checkUser(r.User) != nil {
			return nil, bad("invalid user name")
		}
		if err := checkMachine(Machine{ID: r.ID, Name: r.Name, KeyFingerprint: r.KeyFingerprint}); err != nil {
			return nil, bad(err.Error())
		}
		if r.FirstSeen.IsZero() {
			return nil, bad("no first_seen")
		}
		u := userOf(r.User)
		if _, dup := u.machines[r.ID]; dup {
			return nil, bad("listed twice")
		}
		u.machines[r.ID] = Machine{ID: r.ID, Name: r.Name, KeyFingerprint: r.KeyFingerprint, FirstSeen: r.FirstSeen.UTC()}
	}
	for i, b := range f.Bindings {
		bad := func(why string) error {
			return fmt.Errorf("%w: binding %d (user %q): %s", ErrCorrupt, i, b.User, why)
		}
		if checkUser(b.User) != nil {
			return nil, bad("invalid user name")
		}
		if err := checkSlots(b); err != nil {
			return nil, bad(err.Error())
		}
		if b.BoundAt.IsZero() {
			return nil, bad("no bound_at")
		}
		if !validMover(b.BoundBy) {
			return nil, bad("invalid bound_by")
		}
		if b.Generation == 0 || b.Generation > f.LastGeneration {
			return nil, bad(fmt.Sprintf("generation %d is not between 1 and last_generation %d", b.Generation, f.LastGeneration))
		}
		u := st.users[b.User]
		if u == nil {
			return nil, bad("the bound machine is not among the user's machines")
		}
		if u.binding != nil {
			return nil, bad("a second binding for the user")
		}
		if _, ok := u.machines[b.MachineID()]; !ok {
			return nil, bad("the bound machine is not among the user's machines")
		}
		b = b.clone()
		b.BoundAt = b.BoundAt.UTC()
		u.binding = &b
	}
	users := make([]string, 0, len(st.users))
	for user := range st.users {
		users = append(users, user)
	}
	sort.Strings(users)
	for _, user := range users {
		u := st.users[user]
		if n := len(u.machines); n > MaxMachinesPerUser {
			return nil, fmt.Errorf("%w: user %q has %d machines; at most %d are kept", ErrCorrupt, user, n, MaxMachinesPerUser)
		}
		if u.binding == nil {
			return nil, fmt.Errorf("%w: user %q has machines but no binding", ErrCorrupt, user)
		}
	}
	return st, nil
}

// The fields every stored object must carry. A field that is absent or null
// would otherwise decode as its zero value, and for a user name that is the
// implicit user: an incomplete file must be refused, not read as fewer
// records.
var (
	topFields     = []string{"version", "last_generation", "bindings", "machines"}
	bindingFields = []string{"user", "delivery", "signing", "bound_at", "bound_by", "generation"}
	refFields     = []string{"kind", "id"}
	machineFields = []string{"user", "id", "name", "key_fingerprint", "first_seen"}
)

// checkFieldsPresent refuses a stored document in which any object lacks one
// of its fields or holds null for it.
func checkFieldsPresent(data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if err := requireFields("the document", top, topFields); err != nil {
		return err
	}
	var bindings, machines []map[string]json.RawMessage
	if err := json.Unmarshal(top["bindings"], &bindings); err != nil {
		return fmt.Errorf("%w: bindings: %v", ErrCorrupt, err)
	}
	if err := json.Unmarshal(top["machines"], &machines); err != nil {
		return fmt.Errorf("%w: machines: %v", ErrCorrupt, err)
	}
	for i, b := range bindings {
		what := fmt.Sprintf("binding %d", i)
		if err := requireFields(what, b, bindingFields); err != nil {
			return err
		}
		for _, slot := range []string{"delivery", "signing"} {
			var refs []map[string]json.RawMessage
			if err := json.Unmarshal(b[slot], &refs); err != nil {
				return fmt.Errorf("%w: %s %s: %v", ErrCorrupt, what, slot, err)
			}
			for j, r := range refs {
				if err := requireFields(fmt.Sprintf("%s %s ref %d", what, slot, j), r, refFields); err != nil {
					return err
				}
			}
		}
	}
	for i, m := range machines {
		if err := requireFields(fmt.Sprintf("machine %d", i), m, machineFields); err != nil {
			return err
		}
	}
	return nil
}

// requireFields refuses obj when it lacks one of names or holds null for it.
func requireFields(what string, obj map[string]json.RawMessage, names []string) error {
	if obj == nil {
		return fmt.Errorf("%w: %s is null", ErrCorrupt, what)
	}
	for _, name := range names {
		raw, ok := obj[name]
		if !ok {
			return fmt.Errorf("%w: %s has no %s", ErrCorrupt, what, name)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%w: %s has a null %s", ErrCorrupt, what, name)
		}
	}
	return nil
}

// checkSlots enforces this cut's shape: each slot exactly one machine ref,
// both the same.
func checkSlots(b Binding) error {
	if len(b.Delivery) != 1 || len(b.Signing) != 1 {
		return fmt.Errorf("delivery and signing must each hold exactly one ref (have %d and %d)", len(b.Delivery), len(b.Signing))
	}
	for _, r := range []Ref{b.Delivery[0], b.Signing[0]} {
		if r.Kind != KindMachine {
			return fmt.Errorf("ref kind %q is not %q", r.Kind, KindMachine)
		}
		if !ValidMachineID(r.ID) {
			return fmt.Errorf("ref id %q is not 32 lowercase hex digits", r.ID)
		}
	}
	if b.Delivery[0] != b.Signing[0] {
		return fmt.Errorf("delivery and signing name different machines")
	}
	return nil
}
