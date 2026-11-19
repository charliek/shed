package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
	"github.com/charliek/shed/sdk/creds"
)

// ---------------------------------------------------------------------------
// Who an entry is (plan 037 §3.3): the `User:` line of `shed server add`, the
// `user` and `user_implicit` of its JSON, and the USER column (and the same
// two JSON members) of `shed server list`. The JSON never carries the text's
// `(implicit)` or `-`.
// ---------------------------------------------------------------------------

// runAddCapturing runs `shed server add` against an mtls fixture whose
// bootstrap issues a certificate for user ("" for the implicit user) and
// returns what it printed, with the fixture's endpoint replaced by URL.
func runAddCapturing(t *testing.T, m *mtlsServer, user string) string {
	t.Helper()
	host, port := hostPort(t, m.srv.URL)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	cred := m.credentialFor(t, "cli-key", user, farFromExpiry)
	cred.Bundle.HTTPSPort = port
	stubBootstrapResult(cred, nil)
	var err error
	out := captureStdout(t, func() { err = runServerAdd(nil, []string{host}) })
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return strings.ReplaceAll(out, m.srv.URL, "URL")
}

// checkJSONUser checks the two user members of one JSON object: `user` is
// wantUser, or absent when wantUser is ""; `user_implicit` is present and
// equal to wantImplicit.
func checkJSONUser(t *testing.T, what string, obj map[string]any, wantUser string, wantImplicit bool) {
	t.Helper()
	got, has := obj["user"]
	switch {
	case wantUser == "" && has:
		t.Errorf("%s: user = %#v, want no user member", what, got)
	case wantUser != "" && got != wantUser:
		t.Errorf("%s: user = %#v, want %q", what, got, wantUser)
	}
	if imp, ok := obj["user_implicit"].(bool); !ok || imp != wantImplicit {
		t.Errorf("%s: user_implicit = %#v, want %v", what, obj["user_implicit"], wantImplicit)
	}
}

// TestServerAddPrintsUser: the text and JSON of `shed server add` for an mtls
// server with a user list (the named user), one without (the implicit user),
// and an open server (no credential, no user).
func TestServerAddPrintsUser(t *testing.T) {
	t.Run("named, text", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		out := runAddCapturing(t, newMTLSServer(t), "pat")
		want := "✓ Added server srv (URL, TLS pinned)\n" +
			"  Enrolled a client certificate over SSH (mtls mode)\n" +
			"  User: pat\n" +
			"  Set as default server\n"
		if !strings.HasSuffix(out, want) {
			t.Errorf("add output:\n%s\nwant it to end with:\n%s", out, want)
		}
	})

	t.Run("implicit, text", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		out := runAddCapturing(t, newMTLSServer(t), "")
		want := "✓ Added server srv (URL, TLS pinned)\n" +
			"  Enrolled a client certificate over SSH (mtls mode)\n" +
			"  User: (implicit)\n" +
			"  Set as default server\n"
		if !strings.HasSuffix(out, want) {
			t.Errorf("add output:\n%s\nwant it to end with:\n%s", out, want)
		}
	})

	for _, tc := range []struct {
		name, user, wantUser string
		wantImplicit         bool
	}{
		{"named, JSON", "pat", "pat", false},
		{"implicit, JSON", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testClientConfig(t)
			withCleanAddFlags(t)
			serverAddTrustTOFU = true
			jsonFlag = true
			m := newMTLSServer(t)
			out := runAddCapturing(t, m, tc.user)
			var got struct {
				Status  string         `json:"status"`
				Action  string         `json:"action"`
				Name    string         `json:"name"`
				Details map[string]any `json:"details"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("decode %q: %v", out, err)
			}
			if got.Status != "ok" || got.Action != "added" || got.Name != "srv" {
				t.Errorf("result = %+v", got)
			}
			checkJSONUser(t, "details", got.Details, tc.wantUser, tc.wantImplicit)
			if got.Details["auth_mode"] != config.AuthModeMTLS || got.Details["api_url"] != "URL" ||
				got.Details["tls_cert_fingerprint"] != m.pin || got.Details["default"] != true {
				t.Errorf("details = %v", got.Details)
			}
		})
	}

	openAdd := func(t *testing.T, asJSON bool) string {
		t.Helper()
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		jsonFlag = asJSON
		hostKey, _ := testHostKey(t)
		stubHostKeyScan(t, hostKey, nil)
		stubBootstrapResult(sdk.Credential{}, errors.New("sdk/bootstrap: ssh exited 1: bootstrap requires auth.ssh.mode: enforce"))
		plain := httptest.NewServer(infoHandler())
		t.Cleanup(plain.Close)
		plainHost, plainPort := hostPort(t, plain.URL)
		serverAddPort = plainPort
		var err error
		out := captureStdout(t, func() { err = runServerAdd(nil, []string{plainHost}) })
		if err != nil {
			t.Fatalf("open-mode add: %v", err)
		}
		return out
	}

	t.Run("not mtls, text: no User line", func(t *testing.T) {
		out := openAdd(t, false)
		// The entry's endpoint is built from the HTTP port the server
		// reports (infoHandler: 8080).
		want := "✓ Added server test-server (http://127.0.0.1:8080)\n" +
			"  Set as default server\n"
		if !strings.HasSuffix(out, want) {
			t.Errorf("add output:\n%s\nwant it to end with:\n%s", out, want)
		}
		if strings.Contains(out, "User:") {
			t.Errorf("an open server's add printed a user:\n%s", out)
		}
	})

	t.Run("not mtls, JSON: no user", func(t *testing.T) {
		out := openAdd(t, true)
		var got struct {
			Details map[string]any `json:"details"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		checkJSONUser(t, "details", got.Details, "", false)
	})
}

// listRow is one row of `shed server list`'s text table.
type listRow struct{ endpoint, ssh, security, user, status, def string }

// parseServerList reads the text table: its header must be exactly the one
// below, and each row is split on runs of spaces (no cell holds a space).
func parseServerList(t *testing.T, out string) map[string]listRow {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if got := strings.Join(strings.Fields(lines[0]), " "); got != "NAME ENDPOINT SSH SECURITY USER STATUS DEFAULT" {
		t.Fatalf("header = %q", lines[0])
	}
	rows := map[string]listRow{}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) < 6 || len(f) > 7 {
			t.Fatalf("row of %d cells %q", len(f), l)
		}
		r := listRow{endpoint: f[1], ssh: f[2], security: f[3], user: f[4], status: f[5]}
		if len(f) == 7 {
			r.def = f[6]
		}
		rows[f[0]] = r
	}
	return rows
}

// listOutput runs `shed server list` in the current output mode and returns
// its stdout (stderr, where refusals go, is captured and dropped).
func listOutput(t *testing.T) string {
	t.Helper()
	var out string
	var err error
	captureStderr(t, func() {
		out = captureStdout(t, func() { err = runServerList(nil, nil) })
	})
	if err != nil {
		t.Fatalf("server list: %v", err)
	}
	return out
}

// listJSON runs `shed --json server list` and returns its objects by name.
func listJSON(t *testing.T) map[string]map[string]any {
	t.Helper()
	jsonFlag = true
	out := listOutput(t)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	byName := map[string]map[string]any{}
	for _, r := range rows {
		name, _ := r["name"].(string)
		byName[name] = r
	}
	if len(byName) != len(rows) {
		t.Fatalf("duplicate names in %q", out)
	}
	return byName
}

// listWant is what `shed server list` shows for one entry: the text USER and
// STATUS, and the JSON `user` ("" for absent) and `user_implicit`.
type listWant struct {
	user, status string
	jsonUser     string
	implicit     bool
}

// checkListRows runs the listing in text and in JSON (each over a fresh setup)
// and checks every entry of want, and that there are no others.
func checkListRows(t *testing.T, setup func(t *testing.T), want map[string]listWant) {
	t.Helper()
	t.Run("text", func(t *testing.T) {
		setup(t)
		out := listOutput(t)
		rows := parseServerList(t, out)
		if len(rows) != len(want) {
			t.Errorf("rows = %d, want %d:\n%s", len(rows), len(want), out)
		}
		for name, w := range want {
			r, ok := rows[name]
			if !ok {
				t.Errorf("no row for %s:\n%s", name, out)
				continue
			}
			if r.user != w.user || r.status != w.status {
				t.Errorf("%s: USER %q STATUS %q, want %q %q", name, r.user, r.status, w.user, w.status)
			}
		}
	})
	t.Run("JSON", func(t *testing.T) {
		setup(t)
		rows := listJSON(t)
		if len(rows) != len(want) {
			t.Errorf("rows = %d, want %d", len(rows), len(want))
		}
		for name, w := range want {
			r, ok := rows[name]
			if !ok {
				t.Errorf("no object for %s", name)
				continue
			}
			if r["status"] != w.status {
				t.Errorf("%s: status %v, want %q", name, r["status"], w.status)
			}
			checkJSONUser(t, name, r, w.jsonUser, w.implicit)
		}
	})
}

// TestServerListShowsUser: the USER column and the JSON user members of `shed
// server list`, for a named entry, the implicit user's, a named entry holding
// the implicit user's certificate, an entry that is not mtls, and entries the
// CLI refuses. An entry that records no user and holds a certificate for a
// named user never shows that name: it is `refused`, and its USER is `-`. An
// entry that records a user shows it whatever its certificate says (the only
// user the CLI acts as for it). An unreadable certificate shows `-`, never a
// name.
func TestServerListShowsUser(t *testing.T) {
	setup := func(t *testing.T) {
		t.Helper()
		testClientConfig(t)
		withCleanAddFlags(t)
		m := newMTLSServer(t)

		putServerEntry(t, "named", mtlsEntry(t, m, "named", m.credential(t, "cli-key", farFromExpiry)))
		implicit := mtlsEntry(t, m, "implicit", m.credentialFor(t, "cli-key", "", farFromExpiry))
		if implicit.User != "" {
			t.Fatalf("test setup: an implicit entry records %q", implicit.User)
		}
		putServerEntry(t, "implicit", implicit)
		// A named entry whose server dropped its user list: it records pat
		// and holds the implicit user's certificate.
		namedImplicit := mtlsEntry(t, m, "named-implicit", m.credentialFor(t, "cli-key", "", farFromExpiry))
		namedImplicit.User = testUser
		putServerEntry(t, "named-implicit", namedImplicit)
		putServerEntry(t, "plain", config.ServerEntry{Host: "127.0.0.1", HTTPPort: freeClosedPort(t), SSHPort: 2222})
		// A stored certificate for pat beside a row that records no user.
		unrecorded := mtlsEntry(t, m, "unrecorded", m.credential(t, "cli-key", farFromExpiry))
		unrecorded.User = ""
		putServerEntry(t, "unrecorded", unrecorded)
		// A stored certificate that names no user, beside a row that records none.
		putServerEntry(t, "nouser", mtlsEntry(t, m, "nouser", selfSignedCredential(t, m.pin)))
		// A row that records pat beside a certificate for admin.
		mismatch := mtlsEntry(t, m, "mismatch", m.credentialFor(t, "admin-key", "admin", farFromExpiry))
		mismatch.User = testUser
		putServerEntry(t, "mismatch", mismatch)
		// A certificate file that cannot be read as one, beside a row that records no user.
		garbled := mtlsEntry(t, m, "garbled", m.credential(t, "cli-key", farFromExpiry))
		garbled.User = ""
		if err := os.WriteFile(garbled.ClientCertFile, []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		putServerEntry(t, "garbled", garbled)

		// The garbled entry re-enrols (its pair does not load); the server
		// cannot be reached over SSH here, so it stays without a credential.
		stubBootstrapResult(sdk.Credential{}, errors.New("test: ssh is not reachable"))
	}
	checkListRows(t, setup, map[string]listWant{
		"named":          {"pat", "online", "pat", false},
		"implicit":       {"(implicit)", "online", "", true},
		"named-implicit": {"pat", "online", "pat", true},
		"plain":          {"-", "offline", "", false},
		"unrecorded":     {"-", "refused", "", false},
		"nouser":         {"-", "refused", "", false},
		"mismatch":       {"pat", "refused", "pat", false},
		"garbled":        {"-", "offline", "", false},
	})

	t.Run("the refused entry's certificate user appears nowhere on its row", func(t *testing.T) {
		setup(t)
		for _, l := range strings.Split(listOutput(t), "\n") {
			if strings.HasPrefix(l, "unrecorded ") && strings.Contains(l, testUser) {
				t.Errorf("the refused entry shows its certificate's user: %q", l)
			}
		}
	})
}

// TestServerListNeverShowsAnInvalidUser: a `user` in config.yaml that is not
// a valid user name (a hand edit) is never printed and never taken as the
// label, in text or JSON: `(implicit)` (the text's own label), a name with a
// tab, and one with a newline each show `-`, and no control character reaches
// the terminal from the USER column. Those beside a certificate for pat
// disagree with it, so the existing rule refuses them; the one beside the
// implicit user's certificate is online and still shows `-`.
func TestServerListNeverShowsAnInvalidUser(t *testing.T) {
	invalid := map[string]string{
		"fake-implicit": serverUserImplicit,
		"tab":           "pa\tt",
		"newline":       "pat\nroot",
		"escape":        "pat\x1b[2J",
	}
	setup := func(t *testing.T) {
		t.Helper()
		testClientConfig(t)
		withCleanAddFlags(t)
		m := newMTLSServer(t)
		for name, user := range invalid {
			e := mtlsEntry(t, m, name, m.credential(t, "cli-key", farFromExpiry))
			e.User = user
			putServerEntry(t, name, e)
		}
		// Not mtls, and hand-edited to the text's own label.
		putServerEntry(t, "plain-implicit", config.ServerEntry{
			Host: "127.0.0.1", HTTPPort: freeClosedPort(t), SSHPort: 2222, User: serverUserImplicit})
		// Hand-edited to the text's own label beside the implicit user's
		// certificate, which every entry may present: online, and still `-`
		// (the recorded value is never the label), while the JSON says what
		// the certificate is.
		e := mtlsEntry(t, m, "label-beside-implicit", m.credentialFor(t, "cli-key", "", farFromExpiry))
		e.User = serverUserImplicit
		putServerEntry(t, "label-beside-implicit", e)
		stubBootstrapResult(sdk.Credential{}, errors.New("test: ssh is not reachable"))
	}
	want := map[string]listWant{
		"plain-implicit":        {"-", "offline", "", false},
		"label-beside-implicit": {"-", "online", "", true},
	}
	for name := range invalid {
		want[name] = listWant{"-", "refused", "", false}
	}
	checkListRows(t, setup, want)

	t.Run("no control character and no label from config.yaml in the table", func(t *testing.T) {
		setup(t)
		out := listOutput(t)
		for _, r := range out {
			if r < 0x20 && r != '\n' || r == 0x7f {
				t.Errorf("the table carries control character %U:\n%q", r, out)
			}
		}
		if strings.Contains(out, serverUserImplicit) {
			t.Errorf("a hand-edited %q reached the table:\n%s", serverUserImplicit, out)
		}
	})
}

// TestServerListShowsTheUserARefreshRecorded: the listing's probe can
// re-enrol an entry and so change its row (its first user, its certificate
// paths, its auth mode). USER is worked out from the whole row read again
// after the probe, in text and in JSON, never from the copy the listing
// started with:
//
//   - implicit to named: a server that gained a user list rejects the stored
//     implicit certificate; the re-enrolment records pat;
//   - implicit to implicit: the server rejects the stored implicit
//     certificate and issues another implicit one;
//   - no certificate yet: an mtls entry with no certificate paths enrols;
//   - no auth mode yet: an https entry with no auth mode and no credential
//     enrols, and the persist makes it mtls.
func TestServerListShowsTheUserARefreshRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		// entry returns the starting row; issue the user each mint is for.
		entry func(t *testing.T, m *mtlsServer) config.ServerEntry
		issue string
		want  listWant
	}{
		{
			name: "implicit to named",
			entry: func(t *testing.T, m *mtlsServer) config.ServerEntry {
				stored := m.credentialFor(t, "cli-key", "", farFromExpiry)
				m.revoke(stored.Bundle.CertSerial)
				return mtlsEntry(t, m, "srv", stored)
			},
			issue: testUser,
			want:  listWant{"pat", "online", "pat", false},
		},
		{
			name: "implicit to implicit",
			entry: func(t *testing.T, m *mtlsServer) config.ServerEntry {
				stored := m.credentialFor(t, "cli-key", "", farFromExpiry)
				m.revoke(stored.Bundle.CertSerial)
				return mtlsEntry(t, m, "srv", stored)
			},
			issue: "",
			want:  listWant{"(implicit)", "online", "", true},
		},
		{
			name: "no certificate yet",
			entry: func(t *testing.T, m *mtlsServer) config.ServerEntry {
				return config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: m.srv.URL,
					TLSCertFingerprint: m.pin, AuthMode: config.AuthModeMTLS}
			},
			issue: "",
			want:  listWant{"(implicit)", "online", "", true},
		},
		{
			name: "no auth mode yet",
			entry: func(t *testing.T, m *mtlsServer) config.ServerEntry {
				return config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: m.srv.URL,
					TLSCertFingerprint: m.pin}
			},
			issue: "",
			want:  listWant{"(implicit)", "online", "", true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkListRows(t, func(t *testing.T) {
				testClientConfig(t)
				withCleanAddFlags(t)
				m := newMTLSServer(t)
				putServerEntry(t, "srv", tc.entry(t, m))
				mints := stubBootstrap(func() sdk.Credential { return m.credentialFor(t, "cli-key", tc.issue, farFromExpiry) })
				// After the listing (cleanups run before testClientConfig's
				// restore): it enrolled once, and the row on disk is now an
				// mtls row with certificate paths.
				t.Cleanup(func() {
					if got := mints.Load(); got != 1 {
						t.Errorf("mints = %d, want 1 (the entry enrols once)", got)
					}
					disk, err := clientConfig.ReadOnDisk()
					if err != nil {
						t.Fatal(err)
					}
					if row := disk.Servers["srv"]; row.AuthMode != config.AuthModeMTLS || row.ClientCertFile == "" {
						t.Errorf("the row on disk after the enrolment: %+v", row)
					}
				})
			}, map[string]listWant{"srv": tc.want})
		})
	}
}

// TestListedEntryUserVouchesForNothingOnAFailedReread: after the probe, the
// USER of an entry comes from its row read again. When that read fails, the
// row is gone, or it no longer names the endpoint the listing shows, nothing
// is vouched for: no name, not implicit (`-`, and no `user` in JSON), even
// though the copy the listing started with names a user or holds the implicit
// user's certificate.
func TestListedEntryUserVouchesForNothingOnAFailedReread(t *testing.T) {
	setup := func(t *testing.T) (named, implicit config.ServerEntry) {
		t.Helper()
		testClientConfig(t)
		m := newMTLSServer(t)
		named = mtlsEntry(t, m, "named", m.credential(t, "cli-key", farFromExpiry))
		implicit = mtlsEntry(t, m, "implicit", m.credentialFor(t, "cli-key", "", farFromExpiry))
		putServerEntry(t, "named", named)
		putServerEntry(t, "implicit", implicit)
		return named, implicit
	}
	check := func(t *testing.T, what string, got entryUser, want entryUser) {
		t.Helper()
		if got != want {
			t.Errorf("%s: %+v, want %+v", what, got, want)
		}
	}

	t.Run("control: the row as listed", func(t *testing.T) {
		named, implicit := setup(t)
		check(t, "named", listedEntryUser("named", named), entryUser{name: testUser, recorded: true})
		check(t, "implicit", listedEntryUser("implicit", implicit), entryUser{implicit: true})
	})

	t.Run("config.yaml cannot be read", func(t *testing.T) {
		named, implicit := setup(t)
		if err := os.WriteFile(config.GetClientConfigPath(), []byte("servers: [this is not\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		check(t, "named", listedEntryUser("named", named), entryUser{})
		check(t, "implicit", listedEntryUser("implicit", implicit), entryUser{})
	})

	t.Run("the row is gone", func(t *testing.T) {
		named, implicit := setup(t)
		if err := clientConfig.Update(func(c *config.ClientConfig) {
			delete(c.Servers, "named")
			delete(c.Servers, "implicit")
		}); err != nil {
			t.Fatal(err)
		}
		check(t, "named", listedEntryUser("named", named), entryUser{})
		check(t, "implicit", listedEntryUser("implicit", implicit), entryUser{})
	})

	t.Run("the row names another endpoint", func(t *testing.T) {
		named, implicit := setup(t)
		if err := clientConfig.Update(func(c *config.ClientConfig) {
			for _, n := range []string{"named", "implicit"} {
				e := c.Servers[n]
				e.Host = "build-1.example.test"
				e.User = "admin"
				c.Servers[n] = e
			}
		}); err != nil {
			t.Fatal(err)
		}
		check(t, "named", listedEntryUser("named", named), entryUser{})
		check(t, "implicit", listedEntryUser("implicit", implicit), entryUser{})
	})
}

// treeState is what TestServerUserWritesNothing compares of each path.
type treeState struct {
	mode  fs.FileMode
	mtime time.Time
	size  int64
}

// snapshotTree records every path under root (none when root does not exist).
func snapshotTree(t *testing.T, root string) map[string]treeState {
	t.Helper()
	out := map[string]treeState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[p] = treeState{mode: info.Mode(), mtime: info.ModTime(), size: info.Size()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestServerUserWritesNothing: working out who an entry is (the listing's
// USER, read again after the probe, and the add's report) writes nothing under
// ~/.shed: no directory or lock file is created, no permission is changed, no
// file is touched. Shown for entries with stored material (named, implicit)
// whose lock files and config lock are absent (credentials restored without
// them) and whose store root is left group-readable, and for entries with no
// stored material at all, where the store does not exist.
func TestServerUserWritesNothing(t *testing.T) {
	removeLocks := func(t *testing.T) {
		t.Helper()
		for _, p := range []string{
			filepath.Join(config.GetCredsDir(), creds.LockDirName),
			filepath.Join(config.GetClientConfigDir(), "%config.lock"),
		} {
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
		}
	}
	whoAll := func(names []string) map[string]entryUser {
		got := map[string]entryUser{}
		for _, n := range names {
			e := clientConfig.Servers[n]
			got[n] = listedEntryUser(n, e)
			if add := entryUserOf(n, e); add != got[n] {
				got[n+" (add)"] = add
			}
		}
		return got
	}
	unchanged := func(t *testing.T, before map[string]treeState) {
		t.Helper()
		after := snapshotTree(t, config.GetClientConfigDir())
		for p, b := range before {
			if a, ok := after[p]; !ok {
				t.Errorf("%s was removed", p)
			} else if a != b {
				t.Errorf("%s changed: %+v, was %+v", p, a, b)
			}
		}
		for p := range after {
			if _, ok := before[p]; !ok {
				t.Errorf("%s was created", p)
			}
		}
	}

	t.Run("with stored material", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)
		putServerEntry(t, "named", mtlsEntry(t, m, "named", m.credential(t, "cli-key", farFromExpiry)))
		putServerEntry(t, "implicit", mtlsEntry(t, m, "implicit", m.credentialFor(t, "cli-key", "", farFromExpiry)))
		removeLocks(t)
		if err := os.Chmod(config.GetCredsDir(), 0o750); err != nil {
			t.Fatal(err)
		}
		before := snapshotTree(t, config.GetClientConfigDir())

		got := whoAll([]string{"named", "implicit"})
		want := map[string]entryUser{
			"named":    {name: testUser, recorded: true},
			"implicit": {implicit: true},
		}
		for n, w := range want {
			if got[n] != w {
				t.Errorf("%s: %+v, want %+v", n, got[n], w)
			}
		}
		if len(got) != len(want) {
			t.Errorf("the add's report and the listing disagree: %+v", got)
		}
		unchanged(t, before)
	})

	t.Run("without stored material", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)
		certPath, keyPath := config.ClientCredentialPaths("bare")
		putServerEntry(t, "bare", config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: m.srv.URL,
			TLSCertFingerprint: m.pin, AuthMode: config.AuthModeMTLS,
			ClientCertFile: certPath, ClientKeyFile: keyPath, User: testUser})
		putServerEntry(t, "plain", config.ServerEntry{Host: "127.0.0.1", HTTPPort: freeClosedPort(t), SSHPort: 2222})
		removeLocks(t)
		if _, err := os.Stat(config.GetCredsDir()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("test setup: the store exists (%v)", err)
		}
		before := snapshotTree(t, config.GetClientConfigDir())

		got := whoAll([]string{"bare", "plain"})
		want := map[string]entryUser{
			"bare":  {name: testUser, recorded: true},
			"plain": {},
		}
		for n, w := range want {
			if got[n] != w {
				t.Errorf("%s: %+v, want %+v", n, got[n], w)
			}
		}
		unchanged(t, before)
	})
}
