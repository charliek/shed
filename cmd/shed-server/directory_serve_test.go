package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/source"
)

// directoryConfigs are servers with the same local admins, with and without
// a directory: ops alone, and ops and root.
func directoryConfigs(t *testing.T) (oneDir, twoDir, oneUsers *config.ServerConfig) {
	t.Helper()
	opsLine, _ := wiringKey(t)
	rootLine, _ := wiringKey(t)
	ops := "    ops:\n      admin: true\n      keys:\n        - {key: \"" + opsLine + "\", label: laptop}\n"
	root := "    root:\n      admin: true\n      keys:\n        - {key: \"" + rootLine + "\", label: desk}\n"
	dir := "  directory:\n    url: https://accounts.example.test\n"
	oneDir = loadWiringConfig(t, "auth:\n  mode: mtls\n  users:\n"+ops+dir)
	twoDir = loadWiringConfig(t, "auth:\n  mode: mtls\n  users:\n"+ops+root+dir)
	oneUsers = loadWiringConfig(t, "auth:\n  mode: mtls\n  users:\n"+ops)
	return oneDir, twoDir, oneUsers
}

// TestDirectoryServerRemovesDirectoryUsersBindings: on a directory server
// the start-up step removes the credential source binding (and remembered
// machines) of every user who is not a local admin, logging each, since the
// account service answers for them and no machine does (plan 040, section
// 3.2); a local admin's binding is kept. With auth.users and no directory,
// the same user's records are dropped as not configured, as before.
func TestDirectoryServerRemovesDirectoryUsersBindings(t *testing.T) {
	oneDir, _, oneUsers := directoryConfigs(t)
	for _, tc := range []struct {
		name string
		cfg  *config.ServerConfig
		line string
	}{
		{"directory", oneDir, `Credential source: removed the binding of "ada": the account service answers for this user on this server`},
		{"auth.users", oneUsers, `Users: dropped the credential source records of "ada", which is not a configured user`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			state := t.TempDir()
			fb := bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
			admitT(t, fb, "ada", smachine(1))
			admitT(t, fb, "ada", smachine(2))
			admitT(t, fb, "ops", smachine(3))
			for i := 0; i < 2; i++ {
				stamp(t, tc.name, func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
					return stampUnassignedIn(tc.cfg, newListOnlyBackend(), state, fb)
				})
				fb = restart(t, fb)
			}
			if boundTo(fb, "ada") != "" || len(fb.Machines("ada")) != 0 {
				t.Fatalf("ada's records survived the start: bound to %q, %d machines", boundTo(fb, "ada"), len(fb.Machines("ada")))
			}
			if boundTo(fb, "ops") != smachine(3).ID {
				t.Fatalf("the local admin's binding was not kept: %q", boundTo(fb, "ops"))
			}
			if !strings.Contains(logs.String(), tc.line) {
				t.Fatalf("the removal was not logged as %q:\n%s", tc.line, logs.String())
			}
		})
	}
}

// TestDirectoryServerUpgradeStep: the one-time upgrade step reads the local
// admins (auth.users), never the list: one local admin on a server that had
// no list receives the sheds with no user and the implicit credential
// source; two receive nothing; the same as auth.users with those names.
func TestDirectoryServerUpgradeStep(t *testing.T) {
	oneDir, twoDir, oneUsers := directoryConfigs(t)
	for _, tc := range []struct {
		name     string
		cfg      *config.ServerConfig
		assigned map[string]string
		source   string // the user the implicit binding went to, "" none
	}{
		{"directory, one local admin", oneDir, map[string]string{"legacy": "ops"}, "ops"},
		{"auth.users, the same admin", oneUsers, map[string]string{"legacy": "ops"}, "ops"},
		{"directory, two local admins", twoDir, map[string]string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := bindingsStore(t, filepath.Join(t.TempDir(), source.FileName))
			admitT(t, fb, source.ImplicitUser, smachine(1))
			be := newListOnlyBackend()
			stamp(t, tc.name, func() (backend.UpgradeReport, *backend.UsersUpgrade, error) {
				return stampUnassignedIn(tc.cfg, be, t.TempDir(), fb)
			})
			if !reflect.DeepEqual(be.assigned, tc.assigned) {
				t.Errorf("assigned %v, want %v", be.assigned, tc.assigned)
			}
			for _, u := range []string{"ops", "root"} {
				if got := boundTo(fb, u) == smachine(1).ID; got != (u == tc.source) {
					t.Errorf("the implicit source is %s's: %v", u, got)
				}
			}
			if _, ok := fb.Get(source.ImplicitUser); ok {
				t.Error("the implicit binding was kept")
			}
		})
	}
}

// TestDirectoryStartupLines: serve's start-up lines on a directory server:
// the source and its state, the local admins, and the warning when every
// local admin key has expired (and only then).
func TestDirectoryStartupLines(t *testing.T) {
	line, _ := wiringKey(t)
	old, _ := wiringKey(t)
	body := "auth:\n  mode: mtls\n  users:\n    ops:\n      admin: true\n      keys:\n" +
		"        - {key: \"" + line + "\", label: laptop, expires: \"2026-01-01\"}\n" +
		"        - {key: \"" + old + "\", label: desk, expires: \"2027-01-01\"}\n" +
		"  directory:\n    url: https://accounts.example.test\n    organization: example\n"
	cfg := loadWiringConfig(t, body)
	rt, err := newDirectoryUsers(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Users: from the directory at https://accounts.example.test (organization example, client id none)",
		"Users: not enrolled (no enrolment: ",
		"Local admins: ops",
	}
	got := directoryStartupLines(cfg, rt, time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC))
	if len(got) != 3 {
		t.Fatalf("lines:\n%s", strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("line %d = %q, want it to start %q", i, got[i], want[i])
		}
	}
	if !strings.Contains(got[1], "): run 'shed-server directory enroll'; only the local admins can log in") {
		t.Errorf("not-enrolled line %q", got[1])
	}
	got = directoryStartupLines(cfg, rt, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC))
	if len(got) != 4 || !strings.HasPrefix(got[3], "WARNING: every local admin key has expired (ops (laptop), ops (desk))") {
		t.Errorf("lines once every local admin key has expired:\n%s", strings.Join(got, "\n"))
	}
}

// TestServeLookupInterfaceUnchangedWithoutDirectory: without auth.directory
// newUserLookup is the config lookup exactly as before, with its own
// StartRefresh (a no-op with no GitHub logins).
func TestServeLookupInterfaceUnchangedWithoutDirectory(t *testing.T) {
	_, _, oneUsers := directoryConfigs(t)
	l, err := newUserLookup(context.Background(), oneUsers, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, isDir := l.(*directory.Runtime); isDir {
		t.Fatal("a server without auth.directory built the directory's lookup")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.StartRefresh(ctx)
	if u, ok := l.User("ops"); !ok || !u.Admin {
		t.Fatalf("ops = %+v %v", u, ok)
	}
}
