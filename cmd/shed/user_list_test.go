package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func TestMixedUsers(t *testing.T) {
	tests := []struct {
		users []string
		want  bool
	}{
		{nil, false},
		{[]string{""}, false},
		{[]string{"", ""}, false},
		{[]string{"alice"}, false},
		{[]string{"alice", "alice"}, false},
		{[]string{"", "alice"}, true},
		{[]string{"alice", ""}, true},
		{[]string{"alice", "bob"}, true},
	}
	for _, tt := range tests {
		if got := mixedUsers(tt.users); got != tt.want {
			t.Errorf("mixedUsers(%q) = %v, want %v", tt.users, got, tt.want)
		}
	}
}

func userListSheds(users ...string) []listedShed {
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	out := make([]listedShed, len(users))
	for i, u := range users {
		out[i] = listedShed{shed: config.Shed{Name: "shed-" + string(rune('a'+i)), Status: config.StatusStopped, Backend: "firecracker", CreatedAt: at, User: u}, server: "s1"}
	}
	return out
}

// headerCells is the first line of a table, split on its column gaps.
func headerCells(out string) []string {
	return strings.Fields(strings.SplitN(out, "\n", 2)[0])
}

// The default table gains USER only when the sheds belong to more than one
// user; with every shed the implicit user's it is as before.
func TestListDefaultTableUserColumn(t *testing.T) {
	var buf bytes.Buffer
	renderShedTable(&buf, userListSheds("", ""), false)
	if got := strings.Join(headerCells(buf.String()), " "); got != "NAME BACKEND STATUS SSH CREATED" {
		t.Errorf("implicit-user header = %q, want the table unchanged", got)
	}

	buf.Reset()
	renderShedTable(&buf, userListSheds("alice", "alice"), false)
	if strings.Contains(buf.String(), "USER") {
		t.Errorf("one user's sheds got a USER column:\n%s", buf.String())
	}

	buf.Reset()
	renderShedTable(&buf, userListSheds("alice", ""), true)
	out := buf.String()
	if got := strings.Join(headerCells(out), " "); got != "NAME SERVER BACKEND STATUS SSH USER CREATED" {
		t.Errorf("mixed header = %q", got)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || strings.Fields(lines[1])[5] != "alice" || strings.Fields(lines[2])[5] != "-" {
		t.Errorf("USER cells wrong (want alice, then - for the implicit user):\n%s", out)
	}
}

// `shed list -v` puts USER beside OWNER when a listed shed has a user.
func TestListVerboseUserColumn(t *testing.T) {
	var buf bytes.Buffer
	renderShedTableVerbose(&buf, userListSheds("", ""), false)
	if strings.Contains(buf.String(), "USER") {
		t.Errorf("implicit-user verbose table has a USER column:\n%s", buf.String())
	}

	buf.Reset()
	renderShedTableVerbose(&buf, userListSheds("alice", ""), false)
	header := headerCells(buf.String())
	owner := -1
	for i, h := range header {
		if h == "OWNER" {
			owner = i
		}
	}
	if owner < 0 || owner+1 >= len(header) || header[owner+1] != "USER" {
		t.Fatalf("header %v: want USER right after OWNER", header)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if cells := strings.Fields(lines[1]); cells[owner+1] != "alice" {
		t.Errorf("USER cell = %q, want alice:\n%s", cells[owner+1], buf.String())
	}
}

// `shed list -vv` shows a User line only for a shed that has one.
func TestListDetailsUserLine(t *testing.T) {
	var buf bytes.Buffer
	renderShedDetails(&buf, userListSheds("alice"), false)
	if !strings.Contains(buf.String(), "User:           alice\n") {
		t.Errorf("details lack the user:\n%s", buf.String())
	}
	buf.Reset()
	renderShedDetails(&buf, userListSheds(""), false)
	if strings.Contains(buf.String(), "User:") {
		t.Errorf("details of an implicit-user shed show a User line:\n%s", buf.String())
	}
}

// `shed list --json` carries the user, and no key for the implicit user.
func TestListJSONUser(t *testing.T) {
	b, err := json.Marshal(shedListJSON(userListSheds("alice", "")))
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got[0]["user"] != "alice" {
		t.Errorf("first shed user = %v, want alice", got[0]["user"])
	}
	if _, ok := got[1]["user"]; ok {
		t.Errorf("implicit-user shed carries a user key: %v", got[1])
	}
}

// `shed list --json` carries the record's isolation and arch, under the
// names `shed --json create` and the API use, and omits each when the server
// sent none (an older server).
func TestListJSONIsolationAndArch(t *testing.T) {
	sheds := []listedShed{
		{shed: config.Shed{Name: "fc", Status: config.StatusStopped, NetworkProfile: "isolated", Isolation: "firewall", Arch: "amd64"}, server: "srv"},
		{shed: config.Shed{Name: "old", Status: config.StatusStopped, NetworkProfile: "open"}, server: "srv"},
	}
	b, err := json.Marshal(shedListJSON(sheds))
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got[0]["isolation"] != "firewall" || got[0]["arch"] != "amd64" {
		t.Errorf("first shed: isolation %v, arch %v; want firewall, amd64", got[0]["isolation"], got[0]["arch"])
	}
	for _, key := range []string{"isolation", "arch"} {
		if v, ok := got[1][key]; ok {
			t.Errorf("a shed with no %s carries %v", key, v)
		}
	}
	// The same names as the record the API and `--json create` carry.
	created, err := json.Marshal(sheds[0].shed)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(created, &record); err != nil {
		t.Fatal(err)
	}
	if record["isolation"] != got[0]["isolation"] || record["arch"] != got[0]["arch"] {
		t.Errorf("list JSON %v disagrees with the record %v", got[0], record)
	}
}

// `shed snapshot list` follows the default table's rule.
func TestSnapshotTableUserColumn(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	snaps := []config.Snapshot{{Name: "one", Backend: "firecracker", CreatedAt: at}, {Name: "two", Backend: "firecracker", CreatedAt: at}}
	var buf bytes.Buffer
	renderSnapshotTable(&buf, snaps)
	if got := strings.Join(headerCells(buf.String()), " "); got != "NAME BACKEND SOURCE SIZE CREATED COMMENT" {
		t.Errorf("implicit-user header = %q, want the table unchanged", got)
	}

	snaps[0].User = "alice"
	buf.Reset()
	renderSnapshotTable(&buf, snaps)
	if got := strings.Join(headerCells(buf.String()), " "); got != "NAME BACKEND SOURCE USER SIZE CREATED COMMENT" {
		t.Errorf("mixed header = %q", got)
	}
	if !strings.Contains(buf.String(), "alice") {
		t.Errorf("the user is not shown:\n%s", buf.String())
	}
}
