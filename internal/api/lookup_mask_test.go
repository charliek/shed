package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// unreadableBackend is userFakeBackend whose records cannot be read: every
// shed and snapshot lookup fails as a malformed record does, naming a path on
// the server.
type unreadableBackend struct{ *userFakeBackend }

const unreadablePath = "/var/lib/shed/instances/box/metadata.json"

func (unreadableBackend) GetShed(context.Context, string) (*config.Shed, error) {
	return nil, fmt.Errorf("failed to parse metadata %s: unexpected end of JSON input", unreadablePath)
}

func (unreadableBackend) GetSnapshot(context.Context, string) (*config.Snapshot, error) {
	return nil, fmt.Errorf("failed to parse snapshot metadata %s: unexpected end of JSON input", unreadablePath)
}

// TestAPIUnreadableRecordLooksAbsent: a shed or snapshot whose record cannot
// be read answers a user who is not an admin with the ordinary 404, with
// nothing of the error, on every route that names one; an admin gets the
// error as before (500, with its detail), to diagnose it.
func TestAPIUnreadableRecordLooksAbsent(t *testing.T) {
	_, h := usersTestServer(t, unreadableBackend{newUserFakeBackend()})
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/sheds/box", ""},
		{http.MethodPost, "/api/sheds/box/start", ""},
		{http.MethodPost, "/api/sheds/box/stop", ""},
		{http.MethodDelete, "/api/sheds/box", ""},
		{http.MethodGet, "/api/sheds/box/sessions", ""},
		{http.MethodGet, "/api/snapshots/snap", ""},
		{http.MethodDelete, "/api/snapshots/snap", ""},
	} {
		r := doAs(h, bobFP, rt.method, rt.path, rt.body)
		if r.code != http.StatusNotFound || strings.Contains(string(r.body), unreadablePath) || strings.Contains(string(r.body), "JSON") {
			t.Errorf("bob %s %s = %d %s, want a plain 404", rt.method, rt.path, r.code, r.body)
		}
		r = doAs(h, rootFP, rt.method, rt.path, rt.body)
		if r.code == http.StatusNotFound || !strings.Contains(string(r.body), "JSON") {
			t.Errorf("root %s %s = %d %s, want the error as it is", rt.method, rt.path, r.code, r.body)
		}
	}
}

// absentBackend is userFakeBackend with no shed and no snapshot at all.
func absentBackend() *userFakeBackend {
	b := newUserFakeBackend()
	b.sheds = map[string]config.Shed{}
	b.snaps = map[string]config.Snapshot{}
	return b
}

// TestAPIMaskedLookupIsByteIdenticalToAbsent: for a user who is not an admin,
// a name whose record cannot be read and a name that does not exist get the
// same response, byte for byte: status, body and every header. Anything that
// differed would tell the caller the name is taken.
func TestAPIMaskedLookupIsByteIdenticalToAbsent(t *testing.T) {
	_, masked := usersTestServer(t, unreadableBackend{newUserFakeBackend()})
	_, absent := usersTestServer(t, absentBackend())
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/sheds/box", ""},
		{http.MethodPost, "/api/sheds/box/start", ""},
		{http.MethodPost, "/api/sheds/box/stop", ""},
		{http.MethodDelete, "/api/sheds/box", ""},
		{http.MethodGet, "/api/sheds/box/sessions", ""},
		{http.MethodGet, "/api/snapshots/snap", ""},
		{http.MethodDelete, "/api/snapshots/snap", ""},
	} {
		got := doAs(masked, bobFP, rt.method, rt.path, rt.body)
		want := doAs(absent, bobFP, rt.method, rt.path, rt.body)
		if want.code != http.StatusNotFound {
			t.Fatalf("test set-up: %s %s on an absent name = %d %s, want 404", rt.method, rt.path, want.code, want.body)
		}
		if got.code != want.code || !bytes.Equal(got.body, want.body) || !reflect.DeepEqual(got.header, want.header) {
			t.Errorf("%s %s: unreadable record answered\n  %d %v %q\nan absent name\n  %d %v %q", rt.method, rt.path, got.code, got.header, got.body, want.code, want.header, want.body)
		}
	}
}
