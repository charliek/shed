package main

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// statusAs is GET /api/users/status with id's certificate: the status code
// and the body.
func (s *e2eServer) statusAs(id *e2eIdentity) (int, []byte) {
	s.t.Helper()
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{Certificates: []tls.Certificate{id.cert}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // the test pins nothing; the server's check is what is tested
	}}
	resp, err := hc.Get(s.apiURL + "/api/users/status")
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body
}

// refreshAs is POST /api/users/refresh with id's certificate, decoded.
func (s *e2eServer) refreshAs(id *e2eIdentity) config.UsersRefreshResponse {
	s.t.Helper()
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{Certificates: []tls.Certificate{id.cert}, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // as above
	}}
	resp, err := hc.Post(s.apiURL+"/api/users/refresh", "application/json", nil)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out config.UsersRefreshResponse
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("refresh: HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// TestDirectoryServerUsersStatus: a directory server, as serve builds it,
// answers GET /api/users/status from its runtime: to its local admin, the
// directory's state, the users with their sources and the entry skipped for
// the local admin's name, with no key; to a listed user who is not an admin,
// 403. Its refresh answers the pull's row, good, then failed (an outage) with
// 200 and the list held.
func TestDirectoryServerUsersStatus(t *testing.T) {
	captureEnrolLog(t)
	opsSigner, opsLine := e2eKey(t)
	adaSigner, adaLine := e2eKey(t)
	_, shadowLine := e2eKey(t)
	ops := &e2eIdentity{name: "ops", signer: opsSigner, line: opsLine}
	ada := &e2eIdentity{name: "ada", signer: adaSigner, line: adaLine}
	shadow := &e2eIdentity{name: "ops", line: shadowLine}

	s, _ := startE2EServer(t, ops)
	opsHeld := s.enrol(ops)
	defer opsHeld.Close()

	s.listUsers(map[*e2eIdentity]bool{ada: false, shadow: true})
	if got := s.refreshAs(ops); got.Directory == nil || got.Directory.Status != config.DirectoryPullGood || got.Directory.List != config.DirectoryListInForce ||
		got.Directory.Users != 1 || got.Directory.Skipped != 1 {
		t.Fatalf("the first pull's row: %+v", got.Directory)
	}
	adaHeld := s.enrol(ada)
	defer adaHeld.Close()

	if code, body := s.statusAs(ada); code != http.StatusForbidden {
		t.Errorf("ada (not an admin) = %d %s, want 403", code, body)
	}
	code, body := s.statusAs(ops)
	if code != http.StatusOK {
		t.Fatalf("the local admin = %d %s", code, body)
	}
	var st config.UsersStatusResponse
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	d := st.Directory
	if st.Source != config.UsersSourceDirectory || d == nil || d.State != config.DirectoryStateTrusted || d.ClientID == "" ||
		d.LastGoodPull == nil || d.TrustedUntil == nil || d.Copy != "kept" || len(d.Version) != 64 {
		t.Fatalf("status = %s", body)
	}
	if len(st.Users) != 2 || st.Users[0] != (config.UserStatus{Name: "ada", Source: config.UserFromDirectory}) ||
		st.Users[1] != (config.UserStatus{Name: "ops", Admin: true, Source: config.UserFromLocal}) {
		t.Errorf("users = %+v", st.Users)
	}
	if len(d.Skipped) != 1 || d.Skipped[0].Name != "ops" || d.Skipped[0].Reason != "local-admin-name" {
		t.Errorf("skipped = %+v", d.Skipped)
	}
	for _, k := range []string{opsLine, adaLine, shadowLine, "SHA256:", "laptop", "desk"} {
		if strings.Contains(string(body), k) {
			t.Errorf("the status holds a key or a label (%q)", k)
		}
	}

	// An outage: the pull fails, as a row with 200; the list held stays.
	s.writeControl("outage", "")
	got := s.refreshAs(ops)
	if got.Directory == nil || got.Directory.Status != config.DirectoryPullFailed || got.Directory.Error == "" ||
		got.Directory.List != config.DirectoryListInForce || got.Directory.Version != d.Version || got.Directory.Users != 1 {
		t.Fatalf("the failed pull's row: %+v", got.Directory)
	}
	if _, body := s.statusAs(ops); !strings.Contains(string(body), `"last_error"`) || !strings.Contains(string(body), `"state":"trusted"`) {
		t.Errorf("status after a failed pull = %s", body)
	}
}
