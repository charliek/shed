// Package state is the remote backend's local cache: one JSON record per shed
// and one delete marker per shed whose delete was acknowledged and whose
// machine may still exist. The provider's machines (each with its spec and
// working labels) are the record of which sheds exist; this cache is what
// GetShed and ListSheds read, and what lets a restarted server pick up the
// work it was doing.
//
// Layout under the state directory:
//
//	sheds/<name>.json     one Record
//	deleting/<id>.json    one Marker
//
// Every write is a temp file in the same directory, fsync, rename.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/remote/provider"
)

// Job kinds a record may have in flight.
const (
	JobCreate = "create"
	JobStart  = "start"
	JobStop   = "stop"
	// JobProbe waits for the agent of an adopted, running shed to answer.
	// It never starts, stops or changes the instance.
	JobProbe = "probe"
)

// Queued actions: what runs when the job in flight ends.
const (
	QueuedStart = "start"
	QueuedStop  = "stop"
)

// Schema is the version of the record and marker files this build writes
// and reads. A file without it, or with another value, was written by an
// earlier server (the in-tree cloud provider's, or before the compute
// contract) and is never read as a record: the backend refuses to start
// while one is there (plan 041 R15).
const Schema = 2

// Mutation is the request id a job's call that changes the machine sends,
// stored before it is sent so that a retry, in this process or after a
// restart, sends the same one (the compute contract's idempotency).
type Mutation struct {
	// Call is the call it is for ("start", "stop", "labels:ready").
	Call string `json:"call"`
	// Ref is the machine it is for.
	Ref provider.Ref `json:"ref"`
	// RequestID is the id sent.
	RequestID string `json:"request_id"`
}

// ExtendRequest is an extend's request id, stored before it is sent.
type ExtendRequest struct {
	// Ref is the machine it is for.
	Ref provider.Ref `json:"ref"`
	// RequestID is the id sent.
	RequestID string `json:"request_id"`
	// Hours is how much later it moves the lease.
	Hours int `json:"hours"`
	// At is when it was first sent.
	At time.Time `json:"at"`
}

// Record is everything the server keeps about one shed.
type Record struct {
	// Schema is Schema; SaveRecord writes it.
	Schema int `json:"schema"`

	// Shed is what the API reports. Shed.Status is "starting" or
	// "stopping" exactly while Job is set.
	Shed config.Shed `json:"shed"`

	// Env is the environment of a record the in-tree cloud provider wrote
	// (before the compute service). This build never writes it: a record
	// that has it and no Machine is refused at start-up (Legacy).
	Env string `json:"env,omitempty"`

	// Ref is the provider's handle (the shed id), empty until the provider
	// has accepted the create.
	Ref provider.Ref `json:"ref,omitempty"`
	// Pending is the operation the job in flight is waiting on, when the
	// provider gave one, with what it waits for (Pending.Want).
	Pending *provider.Pending `json:"pending,omitempty"`
	// Mutation is the request id of the job's call in flight, kept from
	// before the call is sent until the job's step ends.
	Mutation *Mutation `json:"mutation,omitempty"`
	// Extend is the request id of the extend in flight, written durably
	// before it is sent, and kept while its outcome is not known (its
	// reply was lost, every retry too). A known outcome clears it. Only the
	// extend's own retries send it again: a caller who retries its request
	// sends its own id (config.ExtendShedRequest.RequestID).
	Extend *ExtendRequest `json:"extend,omitempty"`
	// Spec is the create, request id included. It is kept so a create
	// resumed after a restart sends the same request.
	Spec *provider.Spec `json:"spec,omitempty"`
	// Machine is the machine as the provider last answered it: filled from
	// the create's answer at once, and from every read since. Its access
	// options are what a reconnect dials.
	Machine *provider.Instance `json:"machine,omitempty"`

	// Job is the work in flight: JobCreate, JobStart, JobStop or empty.
	Job string `json:"job,omitempty"`
	// Queued is what to do when Job ends: QueuedStart, QueuedStop or empty.
	Queued string `json:"queued,omitempty"`

	// Ready is true once the machine carries shed_state=ready: the host key
	// is pinned and the agent answered. A shed that is Ready is never
	// re-learned and never has its create steps run again.
	Ready bool `json:"ready,omitempty"`
	// Delivery is the shed's delivery record: the four tracked steps
	// (stage 1, clone, stage 2, install hook), saved after each, the last
	// delivery and the delivered items, never a value. Nil for a shed
	// created before delivery existed, which reports {"state": "none"}.
	Delivery *delivery.Record `json:"delivery,omitempty"`
	// Cloned and Provisioned are what a server from before delivery
	// recorded of a create's clone and hooks. They are read, never
	// written: a record with no Delivery reads cloned as the clone done and
	// provisioned as the install hook done (LegacySteps).
	Cloned      bool `json:"cloned,omitempty"`
	Provisioned bool `json:"provisioned,omitempty"`
	// NoProvision is the create request's no_provision.
	NoProvision bool `json:"no_provision,omitempty"`
	// NoServerEnv is the create request's NoServerEnv: the server's env_file
	// variables are kept out of the shed's repo clone (a shed of a user who
	// is not an admin). It is kept so a create resumed after a restart
	// withholds them too.
	NoServerEnv bool `json:"no_server_env,omitempty"`
	// ScriptKey says the create was called with a script key, for which no
	// delivery applies. It is kept so a create resumed after a restart
	// knows no source would be asked, and runs its clone and hooks.
	ScriptKey bool `json:"script_key,omitempty"`

	// UserOrigin is where the shed's user came from when it was created:
	// "local" (a server with no directory, or a local admin) or "directory"
	// (a person of the account service), by the rule of plan 040 section
	// 3.2. A delivery to the shed is refused (SHED_ORIGIN_CHANGED) when it
	// is not the current origin of the user's name. A shed from before it
	// is stamped once, at the server's first start that knows it.
	UserOrigin string `json:"user_origin,omitempty"`

	// HostKeyPin is the one host key the server accepts from the machine:
	// the key the service attested, compared with the key sshd presented
	// at first contact, in authorized_keys form with no comment. A later
	// change is refused.
	HostKeyPin string `json:"host_key_pin,omitempty"`
	// Layout is the guest layout the agent reported.
	Layout int `json:"layout,omitempty"`

	// UserConflict is the owner a read of the shed's machine found when it
	// named another user or client than the one Shed.User (or Shed.Owner)
	// records, or a user that is not a valid user name at all. While it is
	// set the shed is reported with config.UserUnresolved, so only an admin
	// can reach it. A later read that finds the machine in line with the
	// record clears it.
	UserConflict string `json:"user_conflict,omitempty"`
}

// Legacy says why a record was written by an earlier server, or "" when it
// was not: it does not carry this build's Schema, its Ref has the in-tree
// cloud provider's shape (a zone and a name, not a shed id), or it has an
// environment and no machine.
func (r *Record) Legacy() string {
	switch {
	case r.Schema != Schema:
		return "it was written by an earlier server"
	case r.Ref != "" && string(r.Ref) != r.Shed.ID:
		return fmt.Sprintf("its cloud reference %q is the old provider's", r.Ref)
	case r.Env != "" && r.Machine == nil:
		return fmt.Sprintf("it belongs to environment %q of the old provider", r.Env)
	}
	return ""
}

// Clone returns a deep copy.
func (r *Record) Clone() *Record {
	if r == nil {
		return nil
	}
	out := *r
	out.Delivery = r.Delivery.Clone()
	if r.Pending != nil {
		p := *r.Pending
		out.Pending = &p
	}
	if r.Mutation != nil {
		m := *r.Mutation
		out.Mutation = &m
	}
	if r.Extend != nil {
		x := *r.Extend
		out.Extend = &x
	}
	out.Spec = r.Spec.Clone()
	if r.Machine != nil {
		m := r.Machine.Clone()
		out.Machine = &m
	}
	return &out
}

// LegacySteps is how a record written before delivery existed reads as
// steps: nil when it has a delivery record or recorded nothing; otherwise
// every step owed but the clone (done when Cloned) and the install hook
// (done when Provisioned).
func (r *Record) LegacySteps() *delivery.Steps {
	if r == nil || r.Delivery != nil || (!r.Cloned && !r.Provisioned) {
		return nil
	}
	s := *delivery.NewRecord().Steps
	if r.Cloned {
		s.Clone = delivery.Step{State: delivery.StepDone}
	}
	if r.Provisioned {
		s.Install = delivery.Step{State: delivery.StepDone}
	}
	return &s
}

// Marker says a delete was acknowledged for a shed and its machine may still
// exist. It is removed once the machine is gone.
type Marker struct {
	// Schema is Schema; SaveMarker writes it.
	Schema int    `json:"schema"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	// Env is a marker the in-tree cloud provider wrote; this build never
	// writes it (Legacy).
	Env string       `json:"env,omitempty"`
	Ref provider.Ref `json:"ref,omitempty"`
	// At is when the delete was acknowledged.
	At time.Time `json:"at"`
	// Owner and CreatedAt are the deleted shed's, so a caller waiting for
	// the VM to go can be checked against them once the record is gone.
	// Owner is the service client the shed was reported with
	// (config.ShedOwnership), not the raw stored one, so a record that
	// named both a client and a user is reached by no client here either.
	// Markers written before this carry the raw owner.
	Owner     string    `json:"owner,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	// KeepRecord marks the clean-up of a failed create: the VM is to be
	// destroyed, and the shed's record (in status "error") stays until the
	// shed itself is deleted.
	KeepRecord bool `json:"keep_record,omitempty"`
	// LabelID and DestroyID are the request ids of the delete's label
	// write and its destroy, stored before each is sent so that a retry,
	// or the delete resumed after a restart, sends the same one.
	LabelID   string `json:"label_request_id,omitempty"`
	DestroyID string `json:"destroy_request_id,omitempty"`
}

// Legacy reports whether the marker was written by an earlier server: it
// does not carry this build's Schema, it names an environment, or a Ref of
// the in-tree cloud provider's shape.
func (m *Marker) Legacy() bool {
	return m.Schema != Schema || m.Env != "" || (m.Ref != "" && string(m.Ref) != m.ID)
}

// Store reads and writes records and markers. It does no locking: the
// backend serialises access.
type Store struct {
	dir string
}

// Open returns a store rooted at dir, creating the directories it needs.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("remote state: no state directory")
	}
	s := &Store{dir: dir}
	for _, d := range []string{s.shedsDir(), s.markersDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("remote state: %w", err)
		}
	}
	return s, nil
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) shedsDir() string   { return filepath.Join(s.dir, "sheds") }
func (s *Store) markersDir() string { return filepath.Join(s.dir, "deleting") }

func (s *Store) recordPath(name string) string {
	return filepath.Join(s.shedsDir(), name+".json")
}

func (s *Store) markerPath(id string) string {
	return filepath.Join(s.markersDir(), id+".json")
}

// safeName refuses a name that could leave the directory.
func safeName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("remote state: unsafe name %q", name)
	}
	return nil
}

// SaveRecord writes a record, replacing any record of the same name. See
// ErrNotDurable for the one error after which the record is in place.
func (s *Store) SaveRecord(r *Record) error {
	if err := safeName(r.Shed.Name); err != nil {
		return err
	}
	r.Schema = Schema
	return writeJSON(s.recordPath(r.Shed.Name), r)
}

// DeleteRecord removes a record. A missing record is not an error.
func (s *Store) DeleteRecord(name string) error {
	if err := safeName(name); err != nil {
		return err
	}
	return removeIfExists(s.recordPath(name))
}

// probe is the part of a record or marker file read before the rest: its
// schema, and what names it. An earlier server's file may not decode as
// this build's types, so it is recognised by these alone.
type probe struct {
	Schema int    `json:"schema"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Shed   struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"shed"`
}

// Stale is a record or marker file an earlier server wrote (its schema is
// not Schema): what names it, and its path.
type Stale struct {
	Name, ID, Path string
	// Marker says it is a delete marker, not a record.
	Marker bool
}

// StaleFiles returns every record and marker file an earlier server wrote,
// recognised by its schema before anything else is decoded. A file that is
// not JSON at all is not one of them (Records reports it as bad).
func (s *Store) StaleFiles() ([]Stale, error) {
	var out []Stale
	for _, dir := range []struct {
		path   string
		marker bool
	}{{s.shedsDir(), false}, {s.markersDir(), true}} {
		paths, err := jsonFiles(dir.path)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			var pr probe
			if err := readJSON(p, &pr); err != nil || pr.Schema == Schema {
				continue
			}
			st := Stale{Name: pr.Shed.Name, ID: pr.Shed.ID, Path: p, Marker: dir.marker}
			if dir.marker {
				st.Name, st.ID = pr.Name, pr.ID
			}
			out = append(out, st)
		}
	}
	return out, nil
}

// Records returns every record of this build's Schema, ordered by name. A
// file that cannot be parsed is returned in bad, by path, and left in
// place; one an earlier server wrote is left out (StaleFiles).
func (s *Store) Records() (records []*Record, bad []string, err error) {
	paths, err := jsonFiles(s.shedsDir())
	if err != nil {
		return nil, nil, err
	}
	for _, p := range paths {
		var pr probe
		data, err := os.ReadFile(p)
		if err == nil {
			err = json.Unmarshal(data, &pr)
		}
		if err == nil && pr.Schema != Schema {
			continue
		}
		var r Record
		if err == nil {
			err = json.Unmarshal(data, &r)
		}
		if err != nil || r.Shed.Name == "" {
			bad = append(bad, p)
			continue
		}
		delivery.NormalizeLegacy(r.Delivery, data)
		records = append(records, &r)
	}
	sort.Slice(records, func(a, b int) bool { return records[a].Shed.Name < records[b].Shed.Name })
	return records, bad, nil
}

// SaveMarker writes a delete marker. See ErrNotDurable for the one error
// after which the marker is in place.
func (s *Store) SaveMarker(m *Marker) error {
	if err := safeName(m.ID); err != nil {
		return err
	}
	m.Schema = Schema
	return writeJSON(s.markerPath(m.ID), m)
}

// DeleteMarker removes a delete marker. A missing marker is not an error.
func (s *Store) DeleteMarker(id string) error {
	if err := safeName(id); err != nil {
		return err
	}
	return removeIfExists(s.markerPath(id))
}

// Markers returns every delete marker, ordered by ID.
func (s *Store) Markers() ([]*Marker, error) {
	paths, err := jsonFiles(s.markersDir())
	if err != nil {
		return nil, err
	}
	var out []*Marker
	for _, p := range paths {
		var m Marker
		if err := readJSON(p, &m); err != nil || m.ID == "" || m.Schema != Schema {
			continue
		}
		out = append(out, &m)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func jsonFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("remote state: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// ErrNotDurable is returned by a write that put its file in place and could
// not flush the directory afterwards. The file is there: this process reads
// it, and so does the next server, unless a power cut takes the rename back.
// So a caller must treat the write as made. Reporting it as failed would
// leave a file behind that says otherwise: a delete marker the periodic pass
// and the next start-up act on, a record the next start-up resumes. Every
// other error from a write means nothing was put in place.
var ErrNotDurable = errors.New("the file is in place but its directory could not be flushed")

// writeJSON writes v to path atomically and durably: a temp file beside it,
// fsync, rename, fsync of the directory. An error that is ErrNotDurable means
// the rename was made; any other error means path is as it was.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("remote state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("remote state: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("remote state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("remote state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("remote state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("remote state: %w", err)
	}
	// The rename is durable only once the directory is. Callers act in the
	// cloud on the strength of this write (a record before an insert, a
	// marker before a delete is acknowledged), so a power loss must not be
	// able to take it back.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("remote state: %s: %w: %w", filepath.Base(path), ErrNotDurable, err)
	}
	return nil
}

// removeIfExists removes a file and flushes its directory. A file that is not
// there is flushed for all the same: an earlier call may have removed it and
// failed at the flush, and reporting success for the retry would call a
// removal durable that a power cut can still undo.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remote state: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("remote state: %w", err)
	}
	return nil
}

// syncDir flushes a directory, which makes a rename or a removal inside it
// durable. A variable so a test can watch it.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
