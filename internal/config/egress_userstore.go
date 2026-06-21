package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/charliek/shed/internal/egress"
	"gopkg.in/yaml.v3"
)

// userProfileNameRe constrains user-profile names to lowercase so that names can
// be safe filenames AND can't case-collide on a case-insensitive filesystem
// (APFS): "Foo" and "foo" would otherwise share one <name>.yaml.
var userProfileNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// UserProfileStore is the runtime, user-editable egress-profile store — a second
// source of named profiles alongside the read-only server.yaml ones. Each profile
// is one whole-document <name>.yaml file, CRUD'd via the API/CLI. It is the FIRST
// writer into the egress resolution read path (server.yaml is immutable after
// load), so List/Get return deep copies and all access is RWMutex-guarded.
type UserProfileStore struct {
	dir string
	mu  sync.RWMutex
	m   map[string]EgressProfile
}

// OpenUserProfileStore loads every <name>.yaml under dir into memory. It FAILS
// HARD on a malformed file or an invalid profile spec — a bad runtime policy file
// must not be silently skipped (a profile the user thought they had, gone after a
// restart) nor produce a half-valid effective policy.
func OpenUserProfileStore(dir string) (*UserProfileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("egress profile store %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("egress profile store %s: %w", dir, err)
	}
	s := &UserProfileStore{dir: dir, m: make(map[string]EgressProfile)}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".yaml")
		if !userProfileNameRe.MatchString(name) {
			return nil, fmt.Errorf("egress profile file %q: name must match %s", e.Name(), userProfileNameRe.String())
		}
		if IsReservedEgressName(name) {
			return nil, fmt.Errorf("egress profile file %q: %q is a reserved name", e.Name(), name)
		}
		p, err := loadUserProfile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("egress profile %q: %w", name, err)
		}
		s.m[name] = p
	}
	return s, nil
}

func loadUserProfile(path string) (EgressProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return EgressProfile{}, err
	}
	var p EgressProfile
	// Strict decode: a typo'd key (e.g. "allowed:") fails the load rather than
	// silently dropping the rule.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return EgressProfile{}, err
	}
	if err := egress.ValidateProfile(p.spec()); err != nil {
		return EgressProfile{}, err
	}
	return p, nil
}

// List returns a deep copy of every user profile (map + Allow/Deny slices cloned)
// so a caller can't mutate store state or race a concurrent Put. nil receiver →
// nil (lets callers pass store.List() unconditionally).
func (s *UserProfileStore) List() map[string]EgressProfile {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]EgressProfile, len(s.m))
	for k, v := range s.m {
		out[k] = v.clone()
	}
	return out
}

// Get returns a deep copy of one profile.
func (s *UserProfileStore) Get(name string) (EgressProfile, bool) {
	if s == nil {
		return EgressProfile{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.m[name]
	if !ok {
		return EgressProfile{}, false
	}
	return p.clone(), true
}

// Names returns the user-profile names, sorted (deterministic CLI/UX output).
func (s *UserProfileStore) Names() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.m))
	for k := range s.m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Put validates then atomically writes a profile (<name>.yaml, temp+rename) and
// updates the in-memory map. On a validation or write error the map is unchanged.
// Config-name collisions are the API layer's job (the store has no config view).
func (s *UserProfileStore) Put(name string, p EgressProfile) error {
	if !userProfileNameRe.MatchString(name) {
		return fmt.Errorf("profile name %q must match %s", name, userProfileNameRe.String())
	}
	if IsReservedEgressName(name) {
		return fmt.Errorf("profile name %q is reserved", name)
	}
	if err := egress.ValidateProfile(p.spec()); err != nil {
		return err
	}
	data, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := atomicWriteFile(filepath.Join(s.dir, name+".yaml"), data); err != nil {
		return err
	}
	s.m[name] = p.clone()
	return nil
}

// Delete removes a user profile (file + map). Returns an error if it doesn't exist.
func (s *UserProfileStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[name]; !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	if err := os.Remove(filepath.Join(s.dir, name+".yaml")); err != nil && !os.IsNotExist(err) {
		return err
	}
	delete(s.m, name)
	return nil
}

func atomicWriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
