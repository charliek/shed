// Package state is the remote backend's local cache: one JSON record per shed
// and one delete marker per shed whose delete was acknowledged and whose VM
// may still exist. The cloud's labels and metadata are the record of which
// sheds exist; this cache is what GetShed and ListSheds read, and what lets a
// restarted server pick up the work it was doing.
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

// Record is everything the server keeps about one shed.
type Record struct {
	// Shed is what the API reports. Shed.Status is "starting" or
	// "stopping" exactly while Job is set.
	Shed config.Shed `json:"shed"`

	// Env and Zone are the environment and zone the shed was created in.
	Env  string `json:"env"`
	Zone string `json:"zone,omitempty"`

	// Ref is the provider's handle, empty until the cloud has accepted the
	// create.
	Ref provider.Ref `json:"ref,omitempty"`
	// Pending is the cloud operation the job in flight is waiting on, when
	// the provider gave one.
	Pending *provider.Pending `json:"pending,omitempty"`
	// Spec is what the instance is created from. It is kept so a create
	// resumed after a restart sends the same request.
	Spec *provider.Spec `json:"spec,omitempty"`

	// Job is the work in flight: JobCreate, JobStart, JobStop or empty.
	Job string `json:"job,omitempty"`
	// Queued is what to do when Job ends: QueuedStart, QueuedStop or empty.
	Queued string `json:"queued,omitempty"`

	// Ready is true once the instance carries shed_state=ready: the host
	// key is pinned and the agent answered. A shed that is Ready is never
	// re-learned and never has its create steps run again.
	Ready bool `json:"ready,omitempty"`
	// Cloned and Provisioned record which create steps have run, so a
	// resumed create does not repeat them.
	Cloned      bool `json:"cloned,omitempty"`
	Provisioned bool `json:"provisioned,omitempty"`
	// NoProvision is the create request's no_provision.
	NoProvision bool `json:"no_provision,omitempty"`

	// Address is the last address the cloud reported; AddressAt is when.
	Address   string    `json:"address,omitempty"`
	AddressAt time.Time `json:"address_at,omitempty"`
	// HostKeyPin is a copy of the pinned host key in instance metadata.
	HostKeyPin string `json:"host_key_pin,omitempty"`
	// Layout is the guest layout the agent reported.
	Layout int `json:"layout,omitempty"`
	// Size and DiskGB are the named size and boot disk.
	Size   string `json:"size,omitempty"`
	DiskGB int    `json:"disk_gb,omitempty"`
	// MaxRun is the run limit applied at each start.
	MaxRun time.Duration `json:"max_run,omitempty"`
}

// Clone returns a deep copy.
func (r *Record) Clone() *Record {
	if r == nil {
		return nil
	}
	out := *r
	if r.Pending != nil {
		p := *r.Pending
		out.Pending = &p
	}
	if r.Spec != nil {
		s := *r.Spec
		s.Labels = cloneMap(r.Spec.Labels)
		s.Metadata = cloneMap(r.Spec.Metadata)
		out.Spec = &s
	}
	return &out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Marker says a delete was acknowledged for a shed and its VM may still
// exist. It is removed once the VM is gone.
type Marker struct {
	ID   string       `json:"id"`
	Name string       `json:"name"`
	Env  string       `json:"env"`
	Ref  provider.Ref `json:"ref,omitempty"`
	// At is when the delete was acknowledged.
	At time.Time `json:"at"`
	// Owner and CreatedAt are the deleted shed's, so a caller waiting for
	// the VM to go can be checked against them once the record is gone.
	Owner     string    `json:"owner,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	// KeepRecord marks the clean-up of a failed create: the VM is to be
	// destroyed, and the shed's record (in status "error") stays until the
	// shed itself is deleted.
	KeepRecord bool `json:"keep_record,omitempty"`
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

// SaveRecord writes a record, replacing any record of the same name.
func (s *Store) SaveRecord(r *Record) error {
	if err := safeName(r.Shed.Name); err != nil {
		return err
	}
	return writeJSON(s.recordPath(r.Shed.Name), r)
}

// DeleteRecord removes a record. A missing record is not an error.
func (s *Store) DeleteRecord(name string) error {
	if err := safeName(name); err != nil {
		return err
	}
	return removeIfExists(s.recordPath(name))
}

// Records returns every record, ordered by name. A file that cannot be
// parsed is returned in bad, by path, and left in place.
func (s *Store) Records() (records []*Record, bad []string, err error) {
	paths, err := jsonFiles(s.shedsDir())
	if err != nil {
		return nil, nil, err
	}
	for _, p := range paths {
		var r Record
		if err := readJSON(p, &r); err != nil || r.Shed.Name == "" {
			bad = append(bad, p)
			continue
		}
		records = append(records, &r)
	}
	sort.Slice(records, func(a, b int) bool { return records[a].Shed.Name < records[b].Shed.Name })
	return records, bad, nil
}

// SaveMarker writes a delete marker.
func (s *Store) SaveMarker(m *Marker) error {
	if err := safeName(m.ID); err != nil {
		return err
	}
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
		if err := readJSON(p, &m); err != nil || m.ID == "" {
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

// writeJSON writes v to path atomically and durably: a temp file beside it,
// fsync, rename, fsync of the directory.
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
		return fmt.Errorf("remote state: %w", err)
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
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
