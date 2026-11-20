package main

// A refetch keeps the HTTPS endpoint the operator chose (an add's
// --https-port, for a server reached through a port mapping) while it still
// answers, and moves to the port the server reports only when the entry's own
// endpoint does not answer (verifyRefetchedEndpoint). The automatic
// re-enrolments (a certificate renewal, a token refresh) never rebuild the
// api_url at all.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
)

// fileBytes is what snapshotConfigDir records of each path.
type fileBytes struct {
	mode fs.FileMode
	data string
}

// snapshotConfigDir records every path under ~/.shed with its mode and, for a
// regular file, its bytes.
func snapshotConfigDir(t *testing.T) map[string]fileBytes {
	t.Helper()
	root := config.GetClientConfigDir()
	out := map[string]fileBytes{}
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
		fb := fileBytes{mode: info.Mode()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fb.data = string(data)
		}
		out[p] = fb
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertConfigDirUnchanged fails for any path under ~/.shed added, removed, or
// changed in mode or bytes since before.
func assertConfigDirUnchanged(t *testing.T, before map[string]fileBytes) {
	t.Helper()
	after := snapshotConfigDir(t)
	for p, b := range before {
		a, ok := after[p]
		switch {
		case !ok:
			t.Errorf("%s was removed", p)
		case a.mode != b.mode:
			t.Errorf("%s changed mode: %v, was %v", p, a.mode, b.mode)
		case a.data != b.data:
			t.Errorf("%s changed", p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s was created", p)
		}
	}
}

func httpsURL(host string, port int) string {
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// TestRefetchKeepsTheEntrysEndpoint drives `shed server update --refetch`
// against an mtls test server listening on one port, with entries and bundles
// that name it or a closed port.
func TestRefetchKeepsTheEntrysEndpoint(t *testing.T) {
	const (
		real   = "real"   // the port the test server listens on
		closed = "closed" // a port nothing listens on
		other  = "other"  // a second port nothing listens on
	)
	cases := []struct {
		name string
		// addOverride: the entry is added with --https-port at the real port
		// while the bundle reports the closed one; otherwise it is added at the
		// port the bundle reports (real).
		addOverride bool
		// storedAPIURL, when set, replaces the entry's api_url on disk after
		// the add (a server that moved; a plain http or portless entry).
		storedAPIURL func(host string, ports map[string]int) string
		// reported is the port the refetch's bundle reports.
		reported string
		json     bool

		wantErr     []string
		wantAPIURL  func(host string, ports map[string]int) string
		wantMoved   string // the text line, "" for none
		wantJSONOld func(host string, ports map[string]int) string
	}{
		{
			name:        "an entry added with --https-port keeps it across a refetch",
			addOverride: true,
			reported:    closed,
			wantAPIURL:  func(h string, p map[string]int) string { return httpsURL(h, p[real]) },
		},
		{
			name:        "the kept endpoint is reported in JSON with no previous_api_url",
			addOverride: true,
			reported:    closed,
			json:        true,
			wantAPIURL:  func(h string, p map[string]int) string { return httpsURL(h, p[real]) },
		},
		{
			name:         "a server that moved its HTTPS port heals and says so",
			storedAPIURL: func(h string, p map[string]int) string { return httpsURL(h, p[closed]) },
			reported:     real,
			wantAPIURL:   func(h string, p map[string]int) string { return httpsURL(h, p[real]) },
			wantMoved:    "  HTTPS port changed from {closed} to {real}: https://{host}:{closed} did not answer, and the server reports https://{host}:{real}",
		},
		{
			name:         "a move is reported in JSON as api_url and previous_api_url",
			storedAPIURL: func(h string, p map[string]int) string { return httpsURL(h, p[closed]) },
			reported:     real,
			json:         true,
			wantAPIURL:   func(h string, p map[string]int) string { return httpsURL(h, p[real]) },
			wantJSONOld:  func(h string, p map[string]int) string { return httpsURL(h, p[closed]) },
		},
		{
			name:         "neither endpoint answering refuses, naming both, and writes nothing",
			storedAPIURL: func(h string, p map[string]int) string { return httpsURL(h, p[closed]) },
			reported:     other,
			wantErr: []string{
				"answered at neither the entry's endpoint https://{host}:{closed}",
				"nor the endpoint the server reports, https://{host}:{other}",
				"nothing was saved",
			},
		},
		{
			name:         "the entry's endpoint not answering, at the port the server reports, refuses",
			storedAPIURL: func(h string, p map[string]int) string { return httpsURL(h, p[closed]) },
			reported:     closed,
			wantErr:      []string{"the server's API at https://{host}:{closed} did not answer"},
		},
		{
			name:         "a plain http entry takes the reported endpoint, as before",
			storedAPIURL: func(h string, p map[string]int) string { return "http" + httpsURL(h, p[closed])[5:] },
			reported:     real,
			json:         true,
			wantAPIURL:   func(h string, p map[string]int) string { return httpsURL(h, p[real]) },
		},
		{
			name:         "an https entry with no port takes the reported endpoint, as before",
			storedAPIURL: func(h string, p map[string]int) string { return "https://" + h },
			reported:     real,
			wantAPIURL:   func(h string, p map[string]int) string { return httpsURL(h, p[real]) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			withCleanAddFlags(t)
			serverAddTrustTOFU = true

			m := newMTLSServer(t)
			host, realPort := hostPort(t, m.srv.URL)
			ports := map[string]int{real: realPort, closed: freeClosedPort(t), other: freeClosedPort(t)}
			expand := func(s string) string {
				return strings.NewReplacer("{host}", host,
					"{real}", strconv.Itoa(ports[real]),
					"{closed}", strconv.Itoa(ports[closed]),
					"{other}", strconv.Itoa(ports[other])).Replace(s)
			}
			hostKey, _ := testHostKey(t)
			stubHostKeyScan(t, hostKey, nil)

			addCred := mtlsCredentialFor(t, m)
			if tc.addOverride {
				addCred.Bundle.HTTPSPort = ports[closed]
				serverAddHTTPSPort = ports[real]
			}
			stubBootstrapResult(addCred, nil)
			if err := runServerAdd(nil, []string{host}); err != nil {
				t.Fatalf("add: %v", err)
			}
			serverAddHTTPSPort = 0
			if tc.storedAPIURL != nil {
				e := clientConfig.Servers["srv"]
				e.APIURL = tc.storedAPIURL(host, ports)
				putServerEntry(t, "srv", e)
			}
			stored := clientConfig.Servers["srv"].APIURL

			serverUpdateRefetch = true
			serverUpdateTrustTOFU = true
			jsonFlag = tc.json
			refetch := mtlsCredentialFor(t, m)
			refetch.Bundle.HTTPSPort = ports[tc.reported]
			stubBootstrapResult(refetch, nil)

			before := snapshotConfigDir(t)
			var err error
			out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"srv"}) })

			if tc.wantErr != nil {
				if err == nil {
					t.Errorf("refetch succeeded, want a refusal; output:\n%s", out)
				}
				for _, want := range tc.wantErr {
					if err != nil && !strings.Contains(err.Error(), expand(want)) {
						t.Errorf("error %q must contain %q", err, expand(want))
					}
				}
				assertConfigDirUnchanged(t, before)
				if got := clientConfig.Servers["srv"].APIURL; got != stored {
					t.Errorf("in-memory api_url = %q, want the stored %q", got, stored)
				}
				return
			}
			if err != nil {
				t.Fatalf("refetch: %v\n%s", err, out)
			}

			want := tc.wantAPIURL(host, ports)
			for _, cfg := range loadedAndInMemory(t, cfgPath) {
				if got := cfg.Servers["srv"].APIURL; got != want {
					t.Errorf("api_url = %q, want %q", got, want)
				}
			}
			e := clientConfig.Servers["srv"]
			if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
				t.Errorf("the refetched entry does not work: %v", err)
			}

			if tc.json {
				var got map[string]any
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("decode %q: %v", out, err)
				}
				if got["api_url"] != want {
					t.Errorf("JSON api_url = %v, want %q", got["api_url"], want)
				}
				prev, has := got["previous_api_url"]
				switch {
				case tc.wantJSONOld == nil && has:
					t.Errorf("JSON previous_api_url = %v, want none", prev)
				case tc.wantJSONOld != nil && prev != tc.wantJSONOld(host, ports):
					t.Errorf("JSON previous_api_url = %v, want %q", prev, tc.wantJSONOld(host, ports))
				}
				return
			}
			if tc.wantMoved != "" {
				if !strings.Contains(out, expand(tc.wantMoved)+"\n") {
					t.Errorf("the move was not printed as %q:\n%s", expand(tc.wantMoved), out)
				}
			} else if strings.Contains(out, "HTTPS port changed") {
				t.Errorf("a refetch that kept its endpoint printed a move:\n%s", out)
			}
		})
	}
}

// TestRefetchMoveLearnsTheKeyAgain: a refetch with an identity file whose
// entry's own endpoint does not answer learns the key again before it
// presents the new certificate at the port the server reports. A key replaced
// at its path while the first attempt ran is refused there: the server is
// never shown the certificate and nothing is saved. The negative control
// leaves the key alone, and the entry moves.
func TestRefetchMoveLearnsTheKeyAgain(t *testing.T) {
	for _, replace := range []bool{true, false} {
		name := "the key replaced while the entry's endpoint is tried"
		if !replace {
			name = "negative control: the key left alone"
		}
		t.Run(name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			withCleanAddFlags(t)
			serverAddTrustTOFU = true
			m := newMTLSServer(t)
			addedMTLSServer(t, m)

			key := writeKey(t, t.TempDir(), "id_work")
			// The entry's own endpoint: a listener that, when the first
			// attempt reaches it, replaces the key (or not) and hangs up.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					if replace {
						writeKeyAt(t, key, nil)
					}
					_ = conn.Close()
				}
			}()
			e := clientConfig.Servers["srv"]
			e.APIURL = "https://" + ln.Addr().String()
			putServerEntry(t, "srv", e)

			serverUpdateRefetch, serverUpdateTrustTOFU = true, true
			serverUpdateIdentityFile = optionalString{value: key, set: true}
			bootstrapDuring(credentialForKey(t, m, keyFingerprint(t, key), testUser), func() {})
			before := snapshotConfigDir(t)
			accepted, shown := m.accepted.Load(), len(m.shownCNs())

			var rerr error
			captureStdout(t, func() { rerr = runServerUpdate(nil, []string{"srv"}) })
			if !replace {
				if rerr != nil {
					t.Fatalf("refetch: %v", rerr)
				}
				if got, want := onDisk(t, cfgPath).Servers["srv"].APIURL, m.srv.URL; got != want {
					t.Errorf("api_url = %q, want the reported %q", got, want)
				}
				return
			}
			r := asRefusal(t, rerr, config.RefusedEntryUnreadable)
			if !strings.Contains(r.Error(), "the key in its identity_file changed while this command ran") {
				t.Errorf("refusal = %v", r)
			}
			if n, s := m.accepted.Load()-accepted, len(m.shownCNs())-shown; n != 0 || s != 0 {
				t.Errorf("the reported endpoint accepted %d connection(s) and was shown %d certificate(s); want none", n, s)
			}
			assertConfigDirUnchanged(t, before)
		})
	}
}

// TestRefetchMoveLineEscapesTheEndpoint: the line naming a moved endpoint
// shows api_urls read from config.yaml, so a control character in one is
// escaped, as the other lines that show a value from config escape it.
func TestRefetchMoveLineEscapesTheEndpoint(t *testing.T) {
	got := movedEndpointLine("https://h\x1b[2J:18443", "https://h:8443")
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("an escape character reached the line raw: %q", got)
	}
	if !strings.Contains(got, `\x1b[2J`) {
		t.Errorf("line = %q, want the control character shown escaped", got)
	}
}

// TestAutomaticReEnrolmentKeepsTheAPIURL: a certificate renewal and a token
// refresh re-stamp an existing entry from a fresh bundle that reports another
// HTTPS port than the one the entry uses (an add's --https-port). Neither
// rebuilds the api_url: it stays the operator's, on disk and in memory, and
// the entry keeps working.
func TestAutomaticReEnrolmentKeepsTheAPIURL(t *testing.T) {
	t.Run("a certificate renewal", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		m := newMTLSServer(t)
		entry := expiredEntry(t, m, "srv", m.credential(t, "cli-key", farFromExpiry))
		putServerEntry(t, "srv", entry)

		renewal := m.credential(t, "cli-key", farFromExpiry)
		renewal.Bundle.HTTPSPort = freeClosedPort(t)
		mints := stubBootstrap(func() sdk.Credential { return renewal })

		e := clientConfig.Servers["srv"]
		if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
			t.Fatalf("GetInfo after the renewal: %v", err)
		}
		if got := mints.Load(); got != 1 {
			t.Fatalf("mints = %d, want 1 (the renewal under test)", got)
		}
		for _, cfg := range loadedAndInMemory(t, cfgPath) {
			got := cfg.Servers["srv"]
			if got.APIURL != entry.APIURL {
				t.Errorf("api_url = %q after a renewal, want the entry's %q", got.APIURL, entry.APIURL)
			}
			if got.ClientCertExpiresAt.Equal(entry.ClientCertExpiresAt) {
				t.Error("the renewal was not persisted; the test proves nothing")
			}
		}
	})

	t.Run("a token refresh", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		_, host, port, pin := tokenServer(t, "tok", "shed_ctl_fresh")
		entry := config.ServerEntry{
			Host: host, SSHPort: 2222,
			APIURL:                httpsURL(host, port),
			TLSCertFingerprint:    pin,
			AuthMode:              config.AuthModeToken,
			ControlToken:          "shed_ctl_stale",
			ControlTokenExpiresAt: time.Now().Add(-time.Hour),
		}
		putServerEntry(t, "tok", entry)

		mints := stubBootstrap(func() sdk.Credential {
			return tokenBundle("shed_ctl_fresh", pin, freeClosedPort(t), time.Now().Add(time.Hour))
		})
		e := clientConfig.Servers["tok"]
		if _, err := NewAPIClientFromNamedEntry("tok", &e, DefaultTimeout).GetInfo(); err != nil {
			t.Fatalf("GetInfo after the refresh: %v", err)
		}
		if got := mints.Load(); got != 1 {
			t.Fatalf("mints = %d, want 1 (the refresh under test)", got)
		}
		for _, cfg := range loadedAndInMemory(t, cfgPath) {
			got := cfg.Servers["tok"]
			if got.APIURL != entry.APIURL {
				t.Errorf("api_url = %q after a token refresh, want the entry's %q", got.APIURL, entry.APIURL)
			}
			if got.ControlToken != "shed_ctl_fresh" {
				t.Errorf("token = %q: the refresh was not persisted; the test proves nothing", got.ControlToken)
			}
		}
	})
}
