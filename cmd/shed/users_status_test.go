package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// statusNow is when the golden outputs count the trust time left from.
var statusNow = time.Date(2026, 11, 17, 10, 30, 0, 0, time.UTC)

func statusAt(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

const statusVersion = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

// statusFixtures are GET /api/users/status answers: a directory server in
// each state of its source, a config server and a server with no list. A
// skipped entry's name carries control characters, as a hostile account
// service could send.
func statusFixtures() map[string]config.UsersStatusResponse {
	local := config.UserStatus{Name: "ops", Admin: true, Source: config.UserFromLocal}
	listed := []config.UserStatus{
		{Name: "ada", Admin: true, Source: config.UserFromDirectory},
		{Name: "bob", Source: config.UserFromDirectory},
		local,
	}
	skipped := []config.SkippedEntry{
		{Name: "ops", Reason: "local-admin-name", Description: "its name is a local admin's (the local admin is a separate, local-only identity)"},
		{Name: "evil\x1b[2J\nname", Reason: "invalid-name", Description: "its name is not a valid user name"},
	}
	base := func(state, reason string) config.DirectoryStatus {
		return config.DirectoryStatus{URL: "https://accounts.example.test", Organization: "example", ClientID: "srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a",
			State: state, Reason: reason, Copy: "none", Skipped: []config.SkippedEntry{}}
	}
	trusted := base(config.DirectoryStateTrusted, "")
	trusted.LastGoodPull, trusted.Version, trusted.TrustedUntil = statusAt("2026-11-17T09:00:00Z"), statusVersion, statusAt("2026-11-20T09:00:00Z")
	trusted.LastAttempt, trusted.NextAttempt, trusted.Copy, trusted.Skipped = statusAt("2026-11-17T09:00:00Z"), statusAt("2026-11-18T09:00:00Z"), "kept", skipped

	failing := trusted
	failing.LastAttempt, failing.LastError, failing.NextAttempt = statusAt("2026-11-17T10:00:00Z"), `the account service answered HTTP 503 UNAVAILABLE: "try later"`, statusAt("2026-11-17T10:15:00Z")
	failing.Copy, failing.CopyError = "not-written", "writing users.json: no space left on device"

	expired := base(config.DirectoryStateExpired, "the list's trust time ended at 2026-11-17T09:00:00Z")
	expired.LastGoodPull, expired.Version, expired.TrustedUntil, expired.Skipped = statusAt("2026-11-14T09:00:00Z"), statusVersion, statusAt("2026-11-17T09:00:00Z"), skipped
	expired.LastAttempt, expired.LastError, expired.NextAttempt = statusAt("2026-11-17T10:20:00Z"), "the account service did not answer", statusAt("2026-11-17T10:35:00Z")
	expired.CopyError = "removed: the trust time of its list ended at 2026-11-17T09:00:00Z"
	expired.ExpiredLocalAdmins = []string{"ops"}

	noCopy := base(config.DirectoryStateNoCopy, "no list yet: no usable kept copy and no good pull")
	noCopy.Copy, noCopy.CopyError = "unused", "/srv/state/directory/users.json: is a symbolic link"

	notEnrolled := base(config.DirectoryStateNotEnrolled, "no enrolment: /srv/state/directory does not exist")
	notEnrolled.Organization, notEnrolled.ClientID = "", ""
	notEnrolled.LastAttempt, notEnrolled.LastError, notEnrolled.NextAttempt = statusAt("2026-11-17T10:20:00Z"), "not enrolled: no enrolment: /srv/state/directory does not exist", statusAt("2026-11-17T10:35:00Z")

	unusable := base(config.DirectoryStateEnrolmentUnusable, "/srv/state/directory/enrolment.json: group or others may use it")
	unusable.ClientID = ""

	dir := func(d config.DirectoryStatus, us []config.UserStatus) config.UsersStatusResponse {
		return config.UsersStatusResponse{Source: config.UsersSourceDirectory, Directory: &d, Users: us}
	}
	return map[string]config.UsersStatusResponse{
		"trusted":            dir(trusted, listed),
		"trusted-failing":    dir(failing, listed),
		"expired":            dir(expired, []config.UserStatus{local}),
		"no-copy":            dir(noCopy, []config.UserStatus{local}),
		"not-enrolled":       dir(notEnrolled, []config.UserStatus{local}),
		"enrolment-unusable": dir(unusable, []config.UserStatus{local}),
		"config": {Source: config.UsersSourceConfig, Users: []config.UserStatus{
			{Name: "alice", Source: config.UserFromConfig}, {Name: "root", Admin: true, Source: config.UserFromConfig}}},
		"none": {Source: config.UsersSourceNone, Users: []config.UserStatus{}},
	}
}

// The golden outputs of `shed users status`, one per fixture.
const (
	goldenDirectoryHeader = "Source: directory (an account service's list, beside the local admins)\n" +
		"Account service:  https://accounts.example.test\n"
	goldenListedUsers = `
Users:
  USER   ADMIN  SOURCE
  "ada"  yes    directory
  "bob"  no     directory
  ops    yes    local

Skipped entries (2):
  "ops": its name is a local admin's (the local admin is a separate, local-only identity)
  "evil\x1b[2J\nname": its name is not a valid user name
`
	goldenLocalOnly = `
Users:
  USER  ADMIN  SOURCE
  ops   yes    local
`
)

var usersStatusGolden = map[string]string{
	"trusted": goldenDirectoryHeader + `Organization:     example
Client id:        srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a
State:            trusted
Last good pull:   2026-11-17T09:00:00Z
Version:          0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0
Trusted until:    2026-11-20T09:00:00Z (about 2d 22h 30m left, by this machine's clock)
Last attempt:     2026-11-17T09:00:00Z, good
Next attempt:     2026-11-18T09:00:00Z
Kept copy:        kept
` + goldenListedUsers,
	"trusted-failing": goldenDirectoryHeader + `Organization:     example
Client id:        srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a
State:            trusted
Last good pull:   2026-11-17T09:00:00Z
Version:          0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0
Trusted until:    2026-11-20T09:00:00Z (about 2d 22h 30m left, by this machine's clock)
Last attempt:     2026-11-17T10:00:00Z, failed
Last error:       the account service answered HTTP 503 UNAVAILABLE: "try later"
Next attempt:     2026-11-17T10:15:00Z
Kept copy:        not-written (writing users.json: no space left on device)
` + goldenListedUsers,
	"expired": goldenDirectoryHeader + `Organization:     example
Client id:        srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a
State:            expired (the list's trust time ended at 2026-11-17T09:00:00Z)
Last good pull:   2026-11-14T09:00:00Z
Version:          0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0
Trusted until:    2026-11-17T09:00:00Z (ended)
Last attempt:     2026-11-17T10:20:00Z, failed
Last error:       the account service did not answer
Next attempt:     2026-11-17T10:35:00Z
Kept copy:        none (removed: the trust time of its list ended at 2026-11-17T09:00:00Z)
WARNING: every key of every local admin has expired (ops): no local admin can log in, and with the account service unreachable nobody can
` + goldenLocalOnly + `
Skipped entries (2):
  "ops": its name is a local admin's (the local admin is a separate, local-only identity)
  "evil\x1b[2J\nname": its name is not a valid user name
`,
	"no-copy": goldenDirectoryHeader + `Organization:     example
Client id:        srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a
State:            no-copy (no list yet: no usable kept copy and no good pull)
Last good pull:   never
Version:          none
Trusted until:    no list held
Last attempt:     none yet
Next attempt:     due now
Kept copy:        unused (/srv/state/directory/users.json: is a symbolic link)
` + goldenLocalOnly + `
Skipped entries: none
`,
	"not-enrolled": goldenDirectoryHeader + `Organization:     not known until enrolment
Client id:        none (not enrolled)
State:            not-enrolled (no enrolment: /srv/state/directory does not exist)
Last good pull:   never
Version:          none
Trusted until:    no list held
Last attempt:     2026-11-17T10:20:00Z, failed
Last error:       not enrolled: no enrolment: /srv/state/directory does not exist
Next attempt:     2026-11-17T10:35:00Z
Kept copy:        none
` + goldenLocalOnly + `
Skipped entries: none
`,
	"enrolment-unusable": goldenDirectoryHeader + `Organization:     example
Client id:        none (not enrolled)
State:            enrolment-unusable (/srv/state/directory/enrolment.json: group or others may use it)
Last good pull:   never
Version:          none
Trusted until:    no list held
Last attempt:     none yet
Next attempt:     due now
Kept copy:        none
` + goldenLocalOnly + `
Skipped entries: none
`,
	"config": `Source: config (auth.users)

Users:
  USER   ADMIN  SOURCE
  alice  no     config
  root   yes    config
`,
	"none": "Source: none (no user list: every accepted key is the implicit user, an admin)\n",
}

// TestRenderUsersStatusGolden: `shed users status` prints, for each state of
// a directory server's source and for a config server and a server with no
// list, exactly its golden output: the state and its reason, the last good
// pull, the version, the trust time left, the last error, the next attempt,
// the kept copy, the users and the entries skipped, with every name from the
// account service quoted (control characters escaped).
func TestRenderUsersStatusGolden(t *testing.T) {
	fixtures := statusFixtures()
	if len(fixtures) != len(usersStatusGolden) {
		t.Fatalf("%d fixtures, %d golden outputs", len(fixtures), len(usersStatusGolden))
	}
	for name, st := range fixtures {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			renderUsersStatus(&buf, st, statusNow)
			if got, want := buf.String(), usersStatusGolden[name]; got != want {
				t.Errorf("output:\n%s\nwant:\n%s", got, want)
			}
			if strings.ContainsAny(buf.String(), "\x1b\r") {
				t.Errorf("output holds a control character: %q", buf.String())
			}
		})
	}
}

// TestRenderUsersStatusServerText: text the server sends (a reason, an
// error) cannot forge a line or drive the terminal: its control characters,
// and the Unicode line separator, are shown escaped.
func TestRenderUsersStatusServerText(t *testing.T) {
	st := statusFixtures()["not-enrolled"]
	d := *st.Directory
	d.Reason = "forged\nState:            trusted"
	d.LastError = "boom\x1b[31m\r" + string(lineSeparator) + string(rune(0x9b))
	st.Directory = &d
	var buf bytes.Buffer
	renderUsersStatus(&buf, st, statusNow)
	out := buf.String()
	wantError := "Last error:       boom" + `\x1b[31m\r` + `\` + "u2028" + `\` + "u009b"
	if !strings.Contains(out, `State:            not-enrolled (forged\nState:            trusted)`) ||
		!strings.Contains(out, wantError) || strings.Count(out, "\nState:") != 1 {
		t.Errorf("server text not escaped:\n%s", out)
	}
	if strings.ContainsRune(out, lineSeparator) || strings.ContainsRune(out, 0x9b) || strings.ContainsRune(out, 0x1b) {
		t.Errorf("a control character or a separator reached the output: %q", out)
	}
}

// The golden outputs of `shed users refresh` on a directory server: the
// pull's row above any login rows.
var usersRefreshGolden = []struct {
	name string
	resp config.UsersRefreshResponse
	want string
}{
	{"good", config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "good", List: "in-force", Version: statusVersion, Users: 3, Skipped: 1}},
		"Directory pull: good: version " + statusVersion + ", 3 users (1 skipped)\n"},
	{"failed, a list held", config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "failed", List: "in-force", Version: statusVersion, Users: 1,
			Error: `the account service answered HTTP 403 SERVER_DISABLED: "this server is disabled"`}},
		`Directory pull: failed: the account service answered HTTP 403 SERVER_DISABLED: "this server is disabled"
The list held stays: version ` + statusVersion + `, 1 user (0 skipped)
`},
	{"failed, no list", config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "failed", List: "none", Error: "not enrolled: no enrolment\x1b[31m"}},
		`Directory pull: failed: not enrolled: no enrolment\x1b[31m
No list held: only the local admins can log in.
`},
	{"failed, the list's trust time ended", config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "failed", List: "trust-ended", Error: "the account service did not answer"}},
		`Directory pull: failed: the account service did not answer
The list held is past its trust time: only the local admins can log in.
`},
	{"failed, the list withdrawn", config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "failed", List: "withdrawn", Error: "not enrolled: no enrolment"}},
		`Directory pull: failed: not enrolled: no enrolment
The list held is withdrawn: only the local admins can log in.
`},
	{"good, its trust time already ended", config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "good", List: "trust-ended"}},
		`Directory pull: good
The list held is past its trust time: only the local admins can log in.
`},
	{"no list member: in force by its version", config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "failed", Version: statusVersion, Users: 2, Error: "boom"}},
		"Directory pull: failed: boom\nThe list held stays: version " + statusVersion + ", 2 users (0 skipped)\n"},
	{"above the login rows", config.UsersRefreshResponse{
		Logins:    []config.GitHubLoginRefresh{{Login: "octocat", Status: "ok", GitHubID: 583231, Keys: 2}},
		Directory: &config.DirectoryRefreshRow{Status: "good", List: "in-force", Version: statusVersion, Users: 1}},
		"Directory pull: good: version " + statusVersion + ", 1 user (0 skipped)\n" + `
USER  GITHUB   ACCOUNT ID  STATUS  KEYS  ADDED  REMOVED
-     octocat  583231      ok      2     0      0
`},
}

// TestDirectoryPullError: `shed users refresh` exits non-zero after a
// failed directory pull, saying whether the list in force is unchanged or
// only the local admins are admitted; a good pull, and a server with no
// directory, exit 0.
func TestDirectoryPullError(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  *config.DirectoryRefreshRow
		want string
	}{
		{"no directory", nil, ""},
		{"good", &config.DirectoryRefreshRow{Status: "good", List: "in-force", Version: statusVersion}, ""},
		{"good, trust ended", &config.DirectoryRefreshRow{Status: "good", List: "trust-ended"}, ""},
		{"failed, in force", &config.DirectoryRefreshRow{Status: "failed", List: "in-force", Version: statusVersion},
			"refresh users on srv: the directory pull failed; the list in force is unchanged"},
		{"failed, trust ended", &config.DirectoryRefreshRow{Status: "failed", List: "trust-ended"},
			"refresh users on srv: the directory pull failed; only the local admins are admitted"},
		{"failed, withdrawn", &config.DirectoryRefreshRow{Status: "failed", List: "withdrawn"},
			"refresh users on srv: the directory pull failed; only the local admins are admitted"},
		{"failed, none", &config.DirectoryRefreshRow{Status: "failed", List: "none"},
			"refresh users on srv: the directory pull failed; only the local admins are admitted"},
		{"failed, an unknown list", &config.DirectoryRefreshRow{Status: "failed", List: "something", Version: statusVersion},
			"refresh users on srv: the directory pull failed; only the local admins are admitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := directoryPullError("srv", tc.row)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("error %v, want none", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("error %v, want %q", err, tc.want)
			}
		})
	}
}

// TestTrustLine: the trust line beside each state, counted from statusNow.
// The server's state is authoritative, and the time left is an estimate by
// this machine's clock: beside a trusted state it is never "ended" (at or
// past the instant it is "due"); at equality, 59m59s and exactly 24h.
func TestTrustLine(t *testing.T) {
	at := func(d time.Duration) *time.Time { u := statusNow.Add(d); return &u }
	stamp := func(d time.Duration) string { return statusNow.Add(d).UTC().Format(time.RFC3339) }
	for _, tc := range []struct {
		name  string
		state string
		until *time.Time
		want  string
	}{
		{"trusted, at the instant", config.DirectoryStateTrusted, at(0), stamp(0) + " (due, by this machine's clock)"},
		{"trusted, this machine's clock ahead", config.DirectoryStateTrusted, at(-time.Hour), stamp(-time.Hour) + " (due, by this machine's clock)"},
		{"trusted, 30s", config.DirectoryStateTrusted, at(30 * time.Second), stamp(30*time.Second) + " (less than a minute left, by this machine's clock)"},
		{"trusted, 59m59s", config.DirectoryStateTrusted, at(59*time.Minute + 59*time.Second), stamp(59*time.Minute+59*time.Second) + " (about 59m left, by this machine's clock)"},
		{"trusted, exactly 24h", config.DirectoryStateTrusted, at(24 * time.Hour), stamp(24*time.Hour) + " (about 1d left, by this machine's clock)"},
		{"trusted, 25h1m", config.DirectoryStateTrusted, at(25*time.Hour + time.Minute), stamp(25*time.Hour+time.Minute) + " (about 1d 1h 1m left, by this machine's clock)"},
		{"expired", config.DirectoryStateExpired, at(-time.Hour), stamp(-time.Hour) + " (ended)"},
		{"expired, this machine's clock behind", config.DirectoryStateExpired, at(time.Hour), stamp(time.Hour) + " (ended)"},
		{"not enrolled, a deadline", config.DirectoryStateNotEnrolled, at(time.Hour), stamp(time.Hour) + " (not in force)"},
		{"no list", config.DirectoryStateNoCopy, nil, "no list held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := trustLine(config.DirectoryStatus{State: tc.state, TrustedUntil: tc.until}, statusNow)
			if got != tc.want {
				t.Errorf("trustLine = %q, want %q", got, tc.want)
			}
			if tc.state == config.DirectoryStateTrusted && strings.Contains(got, "ended") {
				t.Errorf("a trusted state printed as ended: %q", got)
			}
		})
	}
}

// hostileRunes are characters a server could send that must never reach a
// terminal as themselves: a C1 control (CSI), a bidirectional override and
// isolate, and a zero-width space.
var hostileRunes = []rune{0x9b, 0x202e, 0x2066, 0x200b}

// hostileStatus is a directory server's status whose service-sent and
// server-sent text carries hostileRunes.
func hostileStatus() config.UsersStatusResponse {
	st := statusFixtures()["trusted"]
	d := *st.Directory
	h := string(hostileRunes)
	d.Reason = "reason" + h
	d.LastError = "error" + h
	d.CopyError = "copy" + h
	d.Skipped = []config.SkippedEntry{
		{Name: "csi" + string(rune(0x9b)) + "2J", Reason: "invalid-name", Description: "its name is not a valid user name" + h},
		{Name: "rlo" + string(rune(0x202e)) + "nimda", Reason: "invalid-name"},
		{Name: "lri" + string(rune(0x2066)), Reason: "invalid-name"},
		{Name: "zw" + string(rune(0x200b)) + "sp", Reason: "invalid-name"},
	}
	st.Directory = &d
	st.Users = []config.UserStatus{
		{Name: "ada" + string(rune(0x202e)), Source: config.UserFromDirectory},
		{Name: "ops" + string(rune(0x200b)), Admin: true, Source: config.UserFromLocal},
		{Name: "x" + string(rune(0x9b)) + string(rune(0x2066)), Source: "weird" + h},
	}
	return st
}

// noHostileRunes fails the test when out holds any of hostileRunes, or any
// other character unsafeForTerminal reports but a newline.
func noHostileRunes(t *testing.T, what, out string) {
	t.Helper()
	for _, r := range out {
		if r != '\n' && unsafeForTerminal(r) {
			t.Errorf("%s holds U+%04X: %q", what, r, out)
		}
	}
	for _, r := range hostileRunes {
		if strings.ContainsRune(out, r) {
			t.Errorf("%s holds U+%04X: %q", what, r, out)
		}
	}
}

// TestUsersStatusJSONEscapesServerText: `shed users status --json` writes
// every C1 control, bidirectional control and zero-width or other format
// character from the server as a \uXXXX escape: the document is valid JSON
// that decodes to the same strings, and no such character reaches the
// terminal. The same holds for a directory refresh row.
func TestUsersStatusJSONEscapesServerText(t *testing.T) {
	st := hostileStatus()
	var buf bytes.Buffer
	if err := writeTerminalSafeJSON(&buf, st); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	noHostileRunes(t, "status JSON", out)
	if !json.Valid(buf.Bytes()) {
		t.Fatalf("not valid JSON: %s", out)
	}
	for _, esc := range []string{`\u009b`, `\u202e`, `\u2066`, `\u200b`} {
		if !strings.Contains(out, esc) {
			t.Errorf("status JSON lacks the escape %s: %s", esc, out)
		}
	}
	var back config.UsersStatusResponse
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil || !reflect.DeepEqual(back, st) {
		t.Errorf("decoded %+v, %v; want the same strings %+v", back, err, st)
	}

	// Beyond the BMP: a format character is a surrogate pair.
	buf.Reset()
	tag := map[string]string{"name": "tag" + string(rune(0xe0001))}
	if err := writeTerminalSafeJSON(&buf, tag); err != nil {
		t.Fatal(err)
	}
	var tagBack map[string]string
	if !strings.Contains(buf.String(), `\udb40\udc01`) || json.Unmarshal(buf.Bytes(), &tagBack) != nil || !reflect.DeepEqual(tagBack, tag) {
		t.Errorf("a format character beyond the BMP: %s", buf.String())
	}

	// Nothing to escape: the bytes are outputJSON's.
	buf.Reset()
	plain := statusFixtures()["config"]
	if err := writeTerminalSafeJSON(&buf, plain); err != nil {
		t.Fatal(err)
	}
	want, _ := json.MarshalIndent(plain, "", "  ")
	if buf.String() != string(want)+"\n" {
		t.Errorf("plain JSON = %q, want %q", buf.String(), want)
	}

	row := config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "failed", List: "none", Error: "boom" + string(hostileRunes)}}
	buf.Reset()
	if err := writeTerminalSafeJSON(&buf, row); err != nil {
		t.Fatal(err)
	}
	noHostileRunes(t, "refresh JSON", buf.String())
	var rowBack config.UsersRefreshResponse
	if err := json.Unmarshal(buf.Bytes(), &rowBack); err != nil || !reflect.DeepEqual(rowBack, row) {
		t.Errorf("refresh row decoded %+v, %v", rowBack, err)
	}
}

// TestUsersStatusTextEscapesServerText: in text mode the same characters,
// in names (quoted) and in the server's text, are shown escaped; so is a
// refresh row's.
func TestUsersStatusTextEscapesServerText(t *testing.T) {
	var buf bytes.Buffer
	renderUsersStatus(&buf, hostileStatus(), statusNow)
	out := buf.String()
	noHostileRunes(t, "status text", out)
	for _, esc := range []string{`"csi\u009b2J"`, `"rlo\u202enimda"`, `"lri\u2066"`, `"zw\u200bsp"`, `"ada\u202e"`, `ops\u200b`,
		`reason\u009b\u202e\u2066\u200b`, `error\u009b\u202e\u2066\u200b`} {
		if !strings.Contains(out, esc) {
			t.Errorf("status text lacks %s:\n%s", esc, out)
		}
	}

	buf.Reset()
	renderUsersRefresh(&buf, config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "failed" + string(hostileRunes), List: "none", Error: "boom" + string(hostileRunes)}})
	noHostileRunes(t, "refresh text", buf.String())
	if !strings.Contains(buf.String(), `boom\u009b\u202e\u2066\u200b`) {
		t.Errorf("refresh text: %q", buf.String())
	}
}

// TestRenderUsersRefreshDirectory: `shed users refresh` prints a directory
// server's pull as its golden output: good, or failed with why and the list
// still held; above any login rows; never "follows no GitHub logins".
func TestRenderUsersRefreshDirectory(t *testing.T) {
	for _, tc := range usersRefreshGolden {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderUsersRefresh(&buf, tc.resp)
			if got := buf.String(); got != tc.want {
				t.Errorf("output:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// statusTestClient is an API client of a test server that answers with
// handler.
func statusTestClient(t *testing.T, handler http.HandlerFunc) *APIClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return NewAPIClient(u.Hostname(), port, time.Second)
}

// usersRig is a client config whose default server "srv" answers POST
// /api/users/refresh with refresh and GET /api/users/status with status.
func usersRig(t *testing.T, refresh config.UsersRefreshResponse, status config.UsersStatusResponse) {
	t.Helper()
	testClientConfig(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/users/refresh":
			_ = json.NewEncoder(w).Encode(refresh)
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/status":
			_ = json.NewEncoder(w).Encode(status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	putServerEntry(t, "srv", config.ServerEntry{APIURL: srv.URL})
	if err := clientConfig.Update(func(c *config.ClientConfig) { c.DefaultServer = "srv" }); err != nil {
		t.Fatal(err)
	}
}

// TestUsersRefreshCommandExit: `shed users refresh` prints a failed
// directory pull's row, then exits non-zero saying what is in force; a good
// pull exits 0; a server with no directory prints and exits as before. In
// --json the same, with the row's server text escaped.
func TestUsersRefreshCommandExit(t *testing.T) {
	failedInForce := config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "failed", List: "in-force", Version: statusVersion, Users: 1, Error: "boom" + string(rune(0x202e))}}
	failedTrustEnded := config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "failed", List: "trust-ended", Error: "the account service did not answer"}}
	good := config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{},
		Directory: &config.DirectoryRefreshRow{Status: "good", List: "in-force", Version: statusVersion, Users: 1}}
	noDirectory := config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{}}
	for _, tc := range []struct {
		name    string
		resp    config.UsersRefreshResponse
		json    bool
		wantOut string
		wantErr string
	}{
		{"failed, in force", failedInForce, false, "The list held stays: version " + statusVersion, "the directory pull failed; the list in force is unchanged"},
		{"failed, trust ended", failedTrustEnded, false, "The list held is past its trust time: only the local admins can log in.", "the directory pull failed; only the local admins are admitted"},
		{"good", good, false, "Directory pull: good: version " + statusVersion, ""},
		{"no directory", noDirectory, false, "This server follows no GitHub logins.\n", ""},
		{"failed, in force, --json", failedInForce, true, `"error": "boom` + `\` + `u202e"`, "the directory pull failed; the list in force is unchanged"},
		{"good, --json", good, true, `"list": "in-force"`, ""},
		{"no directory, --json", noDirectory, true, "{\n  \"logins\": []\n}\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usersRig(t, tc.resp, config.UsersStatusResponse{})
			args := []string{"users", "refresh"}
			if tc.json {
				args = append(args, "--json")
			}
			out, err := runShed(t, args...)
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("output %q lacks %q", out, tc.wantOut)
			}
			noHostileRunes(t, "refresh output", out)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("exit error %v, want none", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("exit error %v, want %q", err, tc.wantErr)
			}
			if tc.json && !json.Valid([]byte(out)) {
				t.Errorf("--json is not JSON: %s", out)
			}
		})
	}
}

// TestUsersStatusCommandJSON: `shed users status --json` writes the
// server's answer with every control and format character escaped, as JSON
// that decodes to the same answer; text mode shows them escaped too.
func TestUsersStatusCommandJSON(t *testing.T) {
	st := hostileStatus()
	usersRig(t, config.UsersRefreshResponse{}, st)
	out, err := runShed(t, "users", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	noHostileRunes(t, "status --json", out)
	var back config.UsersStatusResponse
	if err := json.Unmarshal([]byte(out), &back); err != nil || !reflect.DeepEqual(back, st) {
		t.Errorf("--json decoded %+v, %v", back, err)
	}
	out, err = runShed(t, "users", "status")
	if err != nil {
		t.Fatal(err)
	}
	noHostileRunes(t, "status", out)
	if !strings.Contains(out, `"rlo`+`\`+`u202enimda"`) {
		t.Errorf("status text: %s", out)
	}
}

// TestUsersStatusClient: the client gets the status from GET
// /api/users/status; an older server, which has no such route, answers 404,
// and that is "this server has no status route", an error (so the command
// exits non-zero); a refusal carries the server's code and message.
func TestUsersStatusClient(t *testing.T) {
	want := statusFixtures()["trusted"]
	c := statusTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/users/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	})
	got, err := c.UsersStatus()
	if err != nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("UsersStatus = %+v, %v", got, err)
	}

	older := statusTestClient(t, http.NotFound)
	_, err = older.UsersStatus()
	if !errors.Is(err, errUsersStatusUnsupported) {
		t.Fatalf("an older server: %v, want errUsersStatusUnsupported", err)
	}
	if msg := usersStatusError("old", err).Error(); !strings.Contains(msg, "this server has no status route") || !strings.HasPrefix(msg, "old: ") {
		t.Errorf("an older server's error = %q", msg)
	}

	refused := statusTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(config.NewAPIError(config.ErrForbidden, "admin only: the status of the server's users"))
	})
	_, err = refused.UsersStatus()
	if err == nil || errors.Is(err, errUsersStatusUnsupported) || !strings.Contains(err.Error(), "admin only") {
		t.Errorf("a refusal: %v, want the server's 403", err)
	}
	if msg := usersStatusError("srv", err).Error(); strings.Contains(msg, "no status route") {
		t.Errorf("a refusal reads as an older server: %q", msg)
	}
}
