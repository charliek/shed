package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// Cloud sheds in the CLI (plan 041 criterion 5, and criterion 4's "every
// new code reaches the CLI with fixed words and its reason").

func cloudShed(name, server string, left time.Duration, now time.Time) listedShed {
	started := now.Add(-3 * time.Hour)
	return listedShed{server: server, shed: config.Shed{
		Name: name, Status: config.StatusRunning, Backend: config.BackendRemote, CreatedAt: now.Add(-4 * time.Hour),
		Cloud: &config.ShedCloud{
			Provider: "gce", Size: config.CloudSize{Name: "standard", CPUs: 2, MemoryMB: 8192}, DiskGB: 50, Image: "shed-remote-full-1",
			CreatedAt: now.Add(-4 * time.Hour), StartedAt: &started,
			Lease: &config.CloudLease{ExpiresAt: now.Add(left), MaxExpiresAt: now.Add(100 * time.Hour), Extendable: true, PresetsH: []int{6, 12}},
		},
	}}
}

// emptyClientConfig gives the test a client config with no servers.
func emptyClientConfig(t *testing.T) {
	t.Helper()
	orig := clientConfig
	t.Cleanup(func() { clientConfig = orig })
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{}}
}

func localShed(name string) listedShed {
	return listedShed{server: "home", shed: config.Shed{Name: name, Status: config.StatusStopped, Backend: config.BackendFirecracker, CreatedAt: time.Now()}}
}

// Every refusal of the account service reaches a person in fixed words with
// its reason, on a JSON answer and on a create's stream.
func TestComputeRefusalWords(t *testing.T) {
	cases := []struct {
		status        int
		code, reason  string
		words, notHas string
	}{
		{503, config.ErrComputeLimitReached, "person", "COMPUTE_LIMIT_REACHED (person): you hold as many cloud machines as the account service allows a person", ""},
		{503, config.ErrComputeLimitReached, "organization", "COMPUTE_LIMIT_REACHED (organization): the organization holds as many cloud machines", ""},
		{422, config.ErrComputePolicyRefused, "size", "COMPUTE_POLICY_REFUSED (size): the account service does not allow this size", ""},
		{422, config.ErrComputePolicyRefused, "lease", "COMPUTE_POLICY_REFUSED (lease): the account service does not allow this lease", ""},
		{422, config.ErrComputePolicyRefused, "image", "COMPUTE_POLICY_REFUSED (image): the account service does not offer this image", ""},
		{422, config.ErrComputePolicyRefused, "access", "COMPUTE_POLICY_REFUSED (access): the account service offers no way to reach", ""},
		{422, config.ErrComputePolicyRefused, "person", "COMPUTE_POLICY_REFUSED (person): the account service makes cloud machines only for its own people", ""},
		{422, config.ErrComputePolicyRefused, "client", "COMPUTE_POLICY_REFUSED (client): this service client is not registered", ""},
		{403, config.ErrComputeDenied, "", "COMPUTE_DENIED: the account service does not let this server make cloud machines", "certificate"},
		{503, config.ErrComputeUnavailable, "", "COMPUTE_UNAVAILABLE: the account service could not be reached", ""},
		{409, config.ErrComputeDeleting, "", "COMPUTE_DELETING: the account service is deleting this cloud shed's machine", ""},
		{403, config.ErrExtendNotAllowed, "", "EXTEND_NOT_ALLOWED: this cloud shed's lease cannot be extended", ""},
		{409, config.ErrLeaseAtCap, "", "LEASE_AT_CAP: the extend would take the lease past its cap", ""},
		{409, config.ErrLeaseEnded, "", "LEASE_ENDED: this cloud shed's lease has ended", ""},
		{403, config.ErrPersonInactive, "", "PERSON_INACTIVE: the shed's person is no longer active", ""},
		// The cloud's own refusals keep their names and the server's words.
		{503, config.ErrCloudQuotaExceeded, "", "CLOUD_QUOTA_EXCEEDED: server words", ""},
		{503, config.ErrZoneCapacityUnavailable, "", "ZONE_CAPACITY_UNAVAILABLE: server words", ""},
	}
	for _, tc := range cases {
		t.Run(tc.code+"/"+tc.reason, func(t *testing.T) {
			body, _ := json.Marshal(config.APIError{Error: config.APIErrorDetail{Code: tc.code, Message: "server words", Reason: tc.reason}})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			_, err := newAPIClient(srv.URL, "", "", DefaultTimeout).ExtendShed("widgets", config.ExtendShedRequest{Hours: 6})
			var se *apiStatusError
			if !errors.As(err, &se) || se.Status != tc.status || se.Code != tc.code || !strings.HasPrefix(err.Error(), tc.words) {
				t.Fatalf("JSON answer: %v, want it to start %q", err, tc.words)
			}
			if tc.notHas != "" && strings.Contains(err.Error(), tc.notHas) {
				t.Fatalf("JSON answer: %v says %q", err, tc.notHas)
			}
			stream := "event: error\ndata: " + string(body) + "\n\n"
			if _, err := (&APIClient{}).readSSEStream(strings.NewReader(stream), nil); err == nil || !strings.HasPrefix(err.Error(), tc.words) {
				t.Fatalf("stream: %v, want it to start %q", err, tc.words)
			}
		})
	}
	// A stream error in the bare shape is read too, never printed as ": ".
	bare := `event: error` + "\n" + `data: {"code":"COMPUTE_LIMIT_REACHED","message":"m","reason":"person"}` + "\n\n"
	if _, err := (&APIClient{}).readSSEStream(strings.NewReader(bare), nil); err == nil || !strings.HasPrefix(err.Error(), "COMPUTE_LIMIT_REACHED (person): ") {
		t.Fatalf("bare stream error: %v", err)
	}
	// Any other code keeps its message, and a reason is not dropped.
	if got := apiErrorText(config.APIErrorDetail{Code: "SHED_NOT_FOUND", Message: "shed not found: x"}); got != "SHED_NOT_FOUND: shed not found: x" {
		t.Fatalf("an ordinary error: %q", got)
	}
}

func TestListColumnsOnlyWithACloudShed(t *testing.T) {
	emptyClientConfig(t)
	now := time.Now()
	var out bytes.Buffer
	renderShedTable(&out, []listedShed{localShed("a"), localShed("b")}, false)
	if strings.Contains(out.String(), "SIZE") || strings.Contains(out.String(), "LEASE") {
		t.Fatalf("a listing with no cloud shed has SIZE or LEASE:\n%s", out.String())
	}
	out.Reset()
	renderShedTable(&out, []listedShed{localShed("a"), cloudShed("c", "home", 80*time.Minute, now), cloudShed("d", "home", 30*time.Hour, now)}, false)
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[0], "SIZE") || !strings.Contains(lines[0], "LEASE") {
		t.Fatalf("no SIZE and LEASE with a cloud shed:\n%s", out.String())
	}
	for _, l := range lines {
		f := strings.Fields(l)
		switch {
		case len(f) > 0 && f[0] == "a":
			if !strings.Contains(l, " - ") {
				t.Errorf("a local shed's cells: %q", l)
			}
		case len(f) > 0 && f[0] == "c":
			if !strings.Contains(l, "standard") || !strings.Contains(l, "1h 19m !") && !strings.Contains(l, "1h 20m !") {
				t.Errorf("a lease under 2 h is not marked: %q", l)
			}
		case len(f) > 0 && f[0] == "d":
			if !strings.Contains(l, "1d 5h") || strings.Contains(l, "!") {
				t.Errorf("a far lease: %q", l)
			}
		}
	}

	out.Reset()
	renderShedTableVerbose(&out, []listedShed{cloudShed("c", "home", 80*time.Minute, now)}, false)
	if !strings.Contains(out.String(), "IMAGE") || !strings.Contains(out.String(), "shed-remote-full-1") || !strings.Contains(out.String(), "3h 0m") {
		t.Fatalf("-v: no image or run time from the machine's start:\n%s", out.String())
	}
	out.Reset()
	renderShedDetails(&out, []listedShed{cloudShed("c", "home", 80*time.Minute, now)}, false)
	for _, want := range []string{"\nCloud:\n", "Size:         standard (2 vCPU, 8192 MB)", "Image:        shed-remote-full-1", "Running for:  3h 0m", "Cap:", "Extendable:   yes (+6h, +12h)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("-vv has no %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	renderShedDetails(&out, []listedShed{localShed("a")}, false)
	if strings.Contains(out.String(), "Cloud:") {
		t.Fatalf("-vv of a local shed has a Cloud block:\n%s", out.String())
	}
}

func TestListJSONCarriesCloud(t *testing.T) {
	emptyClientConfig(t)
	now := time.Now()
	raw, _ := json.Marshal(shedListJSON([]listedShed{cloudShed("c", "home", time.Hour, now), localShed("a")}))
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	var c config.ShedCloud
	if err := json.Unmarshal(rows[0]["cloud"], &c); err != nil || c.Size.Name != "standard" || c.Lease == nil || c.Lease.ExpiresAt.IsZero() {
		t.Fatalf("--json's cloud: %s", rows[0]["cloud"])
	}
	if _, ok := rows[1]["cloud"]; ok {
		t.Fatalf("a local shed's row has cloud: %s", raw)
	}
}

func TestCloudFooters(t *testing.T) {
	now := time.Now()
	sheds := []listedShed{cloudShed("c", "srv-b", time.Hour, now), cloudShed("d", "srv-a", time.Hour, now), cloudShed("e", "srv-a", time.Hour, now), localShed("a"), cloudShed("f", "down", time.Hour, now)}
	asked := map[string]int{}
	usage := func(server string) (*config.CloudUsage, error) {
		asked[server]++
		switch server {
		case "srv-a":
			return &config.CloudUsage{Person: &config.CloudCounts{Machines: 3, Running: 2, Stopped: 1, Max: 4}, Org: config.CloudCounts{Machines: 7, Max: 10}}, nil
		case "srv-b":
			return &config.CloudUsage{Org: config.CloudCounts{Machines: 2, Max: 10}}, nil
		}
		return nil, &apiStatusError{Status: http.StatusNotImplemented}
	}
	got := cloudFooters(sheds, false, usage)
	want := []string{
		"Cloud: 3 of 4 machines (2 running, 1 stopped) · organization 7 of 10",
		"Cloud: organization 2 of 10",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("footers:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if asked["home"] != 0 || asked["srv-a"] != 1 {
		t.Fatalf("usage asked %v: a server with no cloud shed, or one twice", asked)
	}
	if got := cloudFooters(sheds, true, usage); got[0] != "Cloud (srv-a): 3 of 4 machines (2 running, 1 stopped) · organization 7 of 10" {
		t.Fatalf("--all footer: %q", got[0])
	}
	if got := cloudFooters([]listedShed{localShed("a")}, false, usage); len(got) != 0 {
		t.Fatalf("a listing with no cloud shed has footers: %v", got)
	}
}

func TestExtendHours(t *testing.T) {
	// Each flag counts by whether it was given, whatever its value.
	for _, tc := range []struct {
		hoursSet bool
		hours    int
		bySet    bool
		by       time.Duration
		want     int
		ok       bool
	}{
		{true, 6, false, 0, 6, true},
		{false, 0, true, 12 * time.Hour, 12, true},
		{false, 0, false, 0, 0, false},
		{true, 6, true, 6 * time.Hour, 0, false},
		{true, 0, true, 6 * time.Hour, 0, false},
		{true, 6, true, 0, 0, false},
		{true, 0, false, 0, 0, false},
		{false, 0, true, 0, 0, false},
		{false, 0, true, 90 * time.Minute, 0, false},
		{true, 721, false, 0, 0, false},
		{true, -1, false, 0, 0, false},
		{true, 720, false, 0, 720, true},
	} {
		got, err := extendHoursOf(tc.hoursSet, tc.hours, tc.bySet, tc.by)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("extendHoursOf(%v %d, %v %v) = %d, %v", tc.hoursSet, tc.hours, tc.bySet, tc.by, got, err)
		}
	}
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	shed := cloudShed("widgets", "home", 30*time.Hour, now).shed
	if got := extendedLine(&shed, now); got != "✓ Extended shed widgets: it is deleted at 2026-11-27 15:00 UTC (in 1d 6h); the cap is 2026-11-30 13:00 UTC" {
		t.Fatalf("extend's line: %q", got)
	}
}

// The CLI prints the banner for `shed exec` only when the exec ran without
// a terminal (the server prints it for one with), stderr is a terminal and
// --json is off; never for a console, which always has a terminal.
func TestExecBannerRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hasCommand bool
		stdin      bool
		stdout     bool
		stderr     bool
		json       bool
		shown      bool
	}{
		{"console (the server prints it)", false, true, true, true, false, false},
		{"interactive exec (the server prints it)", true, true, true, true, false, false},
		{"exec piped, stderr a terminal", true, true, false, true, false, true},
		{"exec with stdin piped, stderr a terminal", true, false, true, true, false, true},
		{"exec with stderr not a terminal", true, false, false, false, false, false},
		{"exec --json", true, false, false, true, true, false},
	} {
		flag := ttyFlag(tc.hasCommand, tc.stdin, tc.stdout)
		if got := execBannerShown(tc.hasCommand, flag, tc.stderr, tc.json); got != tc.shown {
			t.Errorf("%s: shown %v, want %v", tc.name, got, tc.shown)
		}
	}
	now := time.Now()
	soon := cloudShed("widgets", "home", 80*time.Minute, now).shed
	var errOut bytes.Buffer
	writeCLIBanner(&errOut, &soon, now)
	if n := strings.Count(errOut.String(), "shed: this cloud shed is deleted at "); n != 1 || !strings.HasSuffix(errOut.String(), "; extend with: shed extend widgets --hours N\n") {
		t.Fatalf("the banner: %q", errOut.String())
	}
	errOut.Reset()
	far := cloudShed("widgets", "home", 30*time.Hour, now).shed
	writeCLIBanner(&errOut, &far, now)
	local := localShed("a").shed
	writeCLIBanner(&errOut, &local, now)
	if errOut.Len() != 0 {
		t.Fatalf("a banner for a far lease or a local shed: %q", errOut.String())
	}
}

// The server's banner is declined, with the terminal kept, when --json is
// on or stderr is not a terminal; raw ssh sends nothing of the kind.
func TestBannerOptOut(t *testing.T) {
	for _, tc := range []struct {
		stderrTTY, json bool
		want            []string
	}{
		{true, false, nil},
		{false, false, []string{"-o", "SetEnv=SHED_LEASE_BANNER=0"}},
		{true, true, []string{"-o", "SetEnv=SHED_LEASE_BANNER=0"}},
	} {
		if got := bannerOptOut(tc.stderrTTY, tc.json); strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("bannerOptOut(%v, %v) = %v", tc.stderrTTY, tc.json, got)
		}
	}
	entry := &config.ServerEntry{Host: "h", SSHPort: 2222}
	args, err := sshSessionArgs("-t", "srv", "box", entry, []string{"'ls'"}, bannerOptOut(false, true)...)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(joined, "ssh -t ") || !strings.Contains(joined, "-o SetEnv=SHED_LEASE_BANNER=0 box@h 'ls'") {
		t.Fatalf("argv: %s", joined)
	}
}

// After a roost attach (no terminal on the server, so no server banner) the
// CLI prints the banner once, when stderr is a terminal and --json is off.
func TestRoostAttachBanner(t *testing.T) {
	now := time.Now()
	soon := cloudShed("widgets", "home", 80*time.Minute, now).shed
	for _, tc := range []struct {
		stderrTTY, json bool
		count           int
	}{{true, false, 1}, {false, false, 0}, {true, true, 0}} {
		var errOut bytes.Buffer
		roostBanner(&errOut, &soon, now, tc.stderrTTY, tc.json)
		if n := strings.Count(errOut.String(), "shed: this cloud shed is deleted at "); n != tc.count {
			t.Errorf("tty %v json %v: %d banners, want %d", tc.stderrTTY, tc.json, n, tc.count)
		}
	}
}

// `shed extend` sends one request id per invocation, and sends it again
// when no answer came or the server could not decide; a refusal is final.
func TestExtendRetriesWithOneID(t *testing.T) {
	var ids []string
	fail := 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req config.ExtendShedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		ids = append(ids, req.RequestID)
		switch fail {
		case 2:
			fail--
			// The answer is lost: the connection drops.
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
					return
				}
			}
		case 1:
			fail--
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"COMPUTE_UNAVAILABLE","message":"m"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"widgets","status":"running"}`))
	}))
	defer srv.Close()
	c := newAPIClient(srv.URL, "", "", DefaultTimeout)
	rid := newRequestID()
	shed, err := extendWithRetry(c, "widgets", 6, rid, time.Millisecond)
	if err != nil || shed == nil || shed.Name != "widgets" {
		t.Fatalf("extend: %+v, %v", shed, err)
	}
	if len(ids) != 3 || ids[0] != rid || ids[1] != rid || ids[2] != rid || !config.ValidUUID(rid) {
		t.Fatalf("sends carried %v, want %s three times", ids, rid)
	}
	if newRequestID() == rid {
		t.Fatal("two invocations made the same id")
	}

	// A refusal is not sent again.
	ids = nil
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, "x")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"LEASE_AT_CAP","message":"m"}}`))
	}))
	defer refuse.Close()
	if _, err := extendWithRetry(newAPIClient(refuse.URL, "", "", DefaultTimeout), "widgets", 6, rid, time.Millisecond); err == nil || len(ids) != 1 {
		t.Fatalf("a refusal: %v after %d sends", err, len(ids))
	}
}

// A remote shed is shown as a cloud shed even before its machine has
// answered (no lease yet), or from a server whose record has no cloud.
func TestCloudColumnsForAnyRemoteShed(t *testing.T) {
	emptyClientConfig(t)
	now := time.Now()
	noLease := cloudShed("c", "home", time.Hour, now)
	noLease.shed.Cloud.Lease = nil
	bare := listedShed{server: "home", shed: config.Shed{Name: "r", Backend: config.BackendRemote, Status: config.StatusStarting, CreatedAt: now}}
	var out bytes.Buffer
	renderShedTable(&out, []listedShed{noLease, bare}, false)
	if !strings.Contains(out.String(), "SIZE") || !strings.Contains(out.String(), "LEASE") {
		t.Fatalf("no cloud columns:\n%s", out.String())
	}
	if leaseCell(noLease.shed, now) != "-" || leaseCell(bare.shed, now) != "-" || sizeCell(bare.shed) != "-" {
		t.Fatal("an unknown lease or size is not -")
	}
	if !isCloud(bare.shed) || isCloud(localShed("a").shed) {
		t.Fatal("isCloud")
	}
	out.Reset()
	renderShedDetails(&out, []listedShed{noLease}, false)
	if !strings.Contains(out.String(), "Lease:        not known yet") {
		t.Fatalf("-vv with no lease:\n%s", out.String())
	}
	if line, ok := config.LeaseBanner(&noLease.shed, now); ok {
		t.Fatalf("a banner with no lease: %q", line)
	}
}

// The plain attach (`--tmux`, or the fallback when no roost runs) asks the
// server for no banner under --json or with stderr not a terminal, keeping
// its terminal; at a terminal its argv is the golden's.
func TestAttachPlainBannerOptOut(t *testing.T) {
	resetAttachState(t)
	t.Setenv("HOME", "/home/tester")
	entry := &config.ServerEntry{Host: "mini3", SSHPort: 2222}
	for _, tc := range []struct {
		tty, json bool
		optOut    bool
	}{{true, false, false}, {false, false, true}, {true, true, true}} {
		stderrIsTerminal, jsonFlag = func() bool { return tc.tty }, tc.json
		var captured []string
		execSSH = func(_ string, argv []string, _ []string) error { captured = argv; return nil }
		attachTmuxFlag = true
		if err := attachShed("myshed", "myserver", entry, &config.Shed{LandingDir: "/home/shed"}); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(captured, " ")
		if !strings.HasPrefix(joined, "ssh -t ") {
			t.Fatalf("the terminal was dropped: %s", joined)
		}
		if got := strings.Contains(joined, "-o SetEnv=SHED_LEASE_BANNER=0 myshed@mini3 --"); got != tc.optOut {
			t.Errorf("tty %v json %v: opt-out %v, want %v: %s", tc.tty, tc.json, got, tc.optOut, joined)
		}
	}
}
