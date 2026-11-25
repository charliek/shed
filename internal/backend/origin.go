package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// OriginStamper is a backend that records, per shed, where the shed's user
// came from when it was made (user_origin: "local" or "directory", plan
// 040 section 3.2). Each backend stamps it at create; a shed from before it
// is stamped from the one decision the first start recorded
// (MigrateUserOrigins), never from a later start's configuration.
type OriginStamper interface {
	// UnstampedSheds lists the sheds with no recorded user origin, each by
	// its identity and its user.
	UnstampedSheds(ctx context.Context) ([]OriginShed, error)
	// StampUserOrigins gives every shed with no recorded user origin the
	// origin decide returns for it, durably; a shed decide has no answer
	// for (false) is left unstamped. It returns the names it stamped. A
	// shed already stamped is never changed. A shed whose save was not
	// durable is left unstamped (and so refused deliveries) and named in
	// the error.
	StampUserOrigins(ctx context.Context, decide func(OriginShed) (string, bool)) ([]string, error)
}

// OriginShed is a shed as the origin migration knows it: its identity (name,
// permanent id, creation instant) and its user.
type OriginShed struct {
	Name      string
	ID        string
	CreatedAt time.Time
	User      string
}

// The origin migration file: <server state dir>/delivery/origin-migration.json.
//
// It is the one record of the identity decision of the first start that knew
// user_origin: for every shed that existed then with no user_origin, the
// origin of its user's name by THAT start's configuration (its local-admin
// set and whether it had a directory). It is written once, atomically and
// durably (a temporary file, fsync, rename, fsync of the directory), before
// any shed is stamped, and never rewritten. Every later start stamps an
// unstamped shed only from it, never from its own configuration, so a stamp
// whose save failed cannot be decided again after the name has moved between
// the local admins and the directory. A shed that is not in it (matched by
// name and creation instant, and by id when the file names one) and has no
// user_origin stays unstamped, and is refused deliveries (SHED_ORIGIN_UNKNOWN),
// as is every unstamped shed while the file cannot be written or read: no
// decision was recorded, and the next start tries again from the start.
//
//	{"version": 1, "decided_at": "<RFC 3339>",
//	 "sheds": [{"name", "id"?, "created_at", "user"?, "origin"}]}
const (
	OriginMigrationDir  = "delivery"
	OriginMigrationFile = "origin-migration.json"
	originMigrationV1   = 1
)

// OriginDecision is one shed's recorded origin.
type OriginDecision struct {
	Name      string    `json:"name"`
	ID        string    `json:"id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	User      string    `json:"user,omitempty"`
	Origin    string    `json:"origin"`
}

type originMigration struct {
	Version   int              `json:"version"`
	DecidedAt time.Time        `json:"decided_at"`
	Sheds     []OriginDecision `json:"sheds"`
}

// matches reports whether d is the decision for s: the same name and
// creation instant, and the same id when d names one (a shed from before
// delivery may be given its id later).
func (d OriginDecision) matches(s OriginShed) bool {
	return d.Name == s.Name && d.CreatedAt.Equal(s.CreatedAt) && (d.ID == "" || d.ID == s.ID)
}

// MigrateUserOrigins stamps the sheds with no user_origin from the origin
// migration file at path, writing it first when there is none (the first
// start): the origin of each such shed's user by origin, this start's
// configuration. A file that cannot be read or written stamps nothing (every
// unstamped shed stays refused) and is the error. It returns the names it
// stamped; a shed whose stamp was not saved, or that has no decision, stays
// unstamped and is logged by name.
func MigrateUserOrigins(ctx context.Context, st OriginStamper, path string, origin func(user string) string, now time.Time, logf func(string, ...any)) ([]string, error) {
	m, found, err := readOriginMigration(path)
	if err != nil {
		return nil, fmt.Errorf("origin migration file %s: %w; no shed is stamped, and every shed with no origin gets no delivery", path, err)
	}
	if !found {
		list, err := st.UnstampedSheds(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing the sheds with no origin: %w; no decision is recorded, and the next start tries again", err)
		}
		m = &originMigration{Version: originMigrationV1, DecidedAt: now.UTC(), Sheds: []OriginDecision{}}
		for _, s := range list {
			m.Sheds = append(m.Sheds, OriginDecision{Name: s.Name, ID: s.ID, CreatedAt: s.CreatedAt, User: s.User, Origin: origin(s.User)})
		}
		sort.Slice(m.Sheds, func(i, j int) bool { return m.Sheds[i].Name < m.Sheds[j].Name })
		if err := writeOriginMigration(path, m); err != nil {
			return nil, fmt.Errorf("writing the origin migration file %s: %w; no shed is stamped, every shed with no origin gets no delivery, and the next start decides again", path, err)
		}
	}
	decide := func(s OriginShed) (string, bool) {
		for _, d := range m.Sheds {
			if d.matches(s) {
				return d.Origin, true
			}
		}
		logf("Users: shed %s has no record of where its user came from, and none was decided at the first start (%s): it gets no delivery; recreate it", s.Name, path)
		return "", false
	}
	return st.StampUserOrigins(ctx, decide)
}

// readOriginMigration reads the file strictly; found is false when there is
// none.
func readOriginMigration(path string) (*originMigration, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m originMigration
	if err := dec.Decode(&m); err != nil {
		return nil, false, err
	}
	if m.Version != originMigrationV1 {
		return nil, false, fmt.Errorf("version %d is not one this server knows", m.Version)
	}
	for _, d := range m.Sheds {
		if d.Name == "" || (d.Origin != "local" && d.Origin != "directory") {
			return nil, false, fmt.Errorf("a decision for %q is malformed", d.Name)
		}
	}
	return &m, true, nil
}

// writeOriginMigration writes m to path atomically and durably.
func writeOriginMigration(path string, m *originMigration) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+OriginMigrationFile+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := originFileSync(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := originDirSync(dir); err != nil {
		// In place, not durable: removed, so no later start trusts a
		// decision that a power cut could undo (it decides again).
		_ = os.Remove(path)
		return err
	}
	return nil
}

// The syncs writeOriginMigration makes (tests make them fail).
var (
	originFileSync = func(f *os.File) error { return f.Sync() }
	originDirSync  = syncDir
)
