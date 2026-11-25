package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// memStamper is an OriginStamper over sheds in memory; a save of a shed
// named in fail fails.
type memStamper struct {
	sheds   []OriginShed
	origins map[string]string
	fail    map[string]bool
}

func (m *memStamper) UnstampedSheds(context.Context) ([]OriginShed, error) {
	var out []OriginShed
	for _, s := range m.sheds {
		if m.origins[s.Name] == "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memStamper) StampUserOrigins(_ context.Context, decide func(OriginShed) (string, bool)) ([]string, error) {
	var stamped []string
	var errs []error
	for _, s := range m.sheds {
		if m.origins[s.Name] != "" {
			continue
		}
		o, ok := decide(s)
		if !ok {
			continue
		}
		if m.fail[s.Name] {
			errs = append(errs, errors.New("save of "+s.Name+" failed"))
			continue
		}
		m.origins[s.Name] = o
		stamped = append(stamped, s.Name)
	}
	return stamped, errors.Join(errs...)
}

// TestOriginMigrationKeepsTheFirstStartsDecision: the first start records
// every unstamped shed's origin by its configuration before it stamps any;
// a shed whose stamp failed is stamped at a later start from that record,
// never from the later start's configuration (alice has moved from the
// local admins to the directory meanwhile); a shed with no recorded
// decision (one not there at the first start, or a replacement of the name
// made at another instant) stays unstamped.
func TestOriginMigrationKeepsTheFirstStartsDecision(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	path := filepath.Join(t.TempDir(), OriginMigrationDir, OriginMigrationFile)
	m := &memStamper{
		sheds: []OriginShed{
			{Name: "old", ID: "idold", CreatedAt: at, User: "alice"},
			{Name: "other", ID: "idother", CreatedAt: at, User: "bob"},
		},
		origins: map[string]string{},
		fail:    map[string]bool{"old": true},
	}
	aliceLocal := func(string) string { return "local" }
	aliceDirectory := func(u string) string {
		if u == "alice" {
			return "directory"
		}
		return "local"
	}
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, f) }

	stamped, err := MigrateUserOrigins(context.Background(), m, path, aliceLocal, at, logf)
	if err == nil || !slices.Equal(stamped, []string{"other"}) || m.origins["old"] != "" {
		t.Fatalf("first start = %v, %v; origins %v", stamped, err, m.origins)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the decision was not recorded: %v", err)
	}

	// The next start, with alice moved to the directory: old is stamped as
	// the first start decided.
	m.fail = nil
	m.sheds = append(m.sheds,
		OriginShed{Name: "new", ID: "idnew", CreatedAt: at, User: "carol"},
	)
	stamped, err = MigrateUserOrigins(context.Background(), m, path, aliceDirectory, at.Add(time.Hour), logf)
	if err != nil || !slices.Equal(stamped, []string{"old"}) || m.origins["old"] != "local" {
		t.Fatalf("second start = %v, %v; old's origin %q, want local (the first start's)", stamped, err, m.origins["old"])
	}
	if m.origins["new"] != "" || len(logged) == 0 {
		t.Fatalf("a shed with no recorded decision was stamped %q (logged %v)", m.origins["new"], logged)
	}

	// A replacement of a name (another creation instant) is not the shed
	// the decision was for.
	m2 := &memStamper{sheds: []OriginShed{{Name: "old", ID: "idold2", CreatedAt: at.Add(time.Minute), User: "alice"}}, origins: map[string]string{}}
	if stamped, _ := MigrateUserOrigins(context.Background(), m2, path, aliceDirectory, at, logf); len(stamped) != 0 {
		t.Fatalf("a replacement was stamped from the old shed's decision: %v", stamped)
	}
}

// TestOriginMigrationFileNotWritten: when the decision cannot be recorded
// durably, no shed is stamped and no file is left, so the next start
// decides afresh (no decision was ever acted on).
func TestOriginMigrationFileNotWritten(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	path := filepath.Join(t.TempDir(), OriginMigrationDir, OriginMigrationFile)
	m := &memStamper{sheds: []OriginShed{{Name: "old", ID: "idold", CreatedAt: at, User: "alice"}}, origins: map[string]string{}}
	prev := originDirSync
	originDirSync = func(string) error { return errors.New("sync failed") }
	stamped, err := MigrateUserOrigins(context.Background(), m, path, func(string) string { return "local" }, at, t.Logf)
	originDirSync = prev
	if err == nil || len(stamped) != 0 || m.origins["old"] != "" {
		t.Fatalf("with the file not durable = %v, %v; %v", stamped, err, m.origins)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a decision that is not durable was left in place: %v", err)
	}
	// A file that does not parse stamps nothing either.
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stamped, err := MigrateUserOrigins(context.Background(), m, path, func(string) string { return "local" }, at, t.Logf); err == nil || len(stamped) != 0 {
		t.Fatalf("with a damaged file = %v, %v", stamped, err)
	}
	// The next start that can write decides, and stamps.
	_ = os.Remove(path)
	if stamped, err := MigrateUserOrigins(context.Background(), m, path, func(string) string { return "directory" }, at, t.Logf); err != nil || len(stamped) != 1 || m.origins["old"] != "directory" {
		t.Fatalf("the next start = %v, %v; %v", stamped, err, m.origins)
	}
}
