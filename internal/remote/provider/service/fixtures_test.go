package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/provider"
)

// The fixtures, classified: every machine fixture accepted or refused
// under its rule, every reply fixture classified as it expects and as
// fixtures/mapping.json says, mapping.json equal to Codes, and every error
// an exchange answers at the status mapping.json gives its code.

// kinds names every provider error kind mapping.json may name.
var kinds = map[string]error{
	"ErrNotFound": provider.ErrNotFound, "ErrExists": provider.ErrExists, "ErrQuota": provider.ErrQuota,
	"ErrCapacity": provider.ErrCapacity, "ErrBusy": provider.ErrBusy, "ErrInvalid": provider.ErrInvalid,
	"ErrLabelTaken": provider.ErrLabelTaken, "ErrSpecMismatch": provider.ErrSpecMismatch,
	"ErrComputeLimit": provider.ErrComputeLimit, "ErrPolicy": provider.ErrPolicy, "ErrLease": provider.ErrLease,
	"ErrDeleting": provider.ErrDeleting, "ErrIngress": provider.ErrIngress, "ErrDenied": provider.ErrDenied,
	"ErrUnavailable": provider.ErrUnavailable, "ErrInvalidReply": provider.ErrInvalidReply,
}

func kindName(err error) string {
	for n, k := range kinds {
		if k == err {
			return n
		}
	}
	return fmt.Sprint(err)
}

const (
	fixtureShedID = "q6c3g4ymb5m2dkxwe7kyz5hhzu"
	fixtureServer = "srv-a"
)

type machineFixture struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Context     struct {
		ShedID      string `json:"shed_id"`
		Server      string `json:"server"`
		TestDrivers bool   `json:"test_drivers"`
	} `json:"context"`
	Machine     json.RawMessage `json:"machine"`
	MachineText *string         `json:"machine_text"`
	Expect      struct {
		Result string `json:"result"`
		Rule   string `json:"rule"`
		// Decoded is, for an accepted machine, what a server reads of it.
		Decoded json.RawMessage `json:"decoded"`
	} `json:"expect"`
}

// renderMachine writes a decoded machine in the fixtures' "decoded" form:
// the members a server reads, the ssh options it keeps (in order, with the
// four members it uses), and how many options of other kinds it skipped.
func renderMachine(m *Machine) map[string]any {
	opt := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	tm := func(t time.Time) any {
		if t.IsZero() {
			return nil
		}
		return t.UTC().Format(timeLayout)
	}
	access := []any{}
	for _, a := range m.Access {
		access = append(access, map[string]any{"host": a.Host.String(), "port": a.Port, "user": a.User, "host_key": a.HostKey})
	}
	return map[string]any{
		"shed_id": m.ShedID, "name": m.Name, "server": m.Server, "user": opt(m.User), "client": opt(m.Client),
		"state": m.State, "size": map[string]any{"name": m.Size.Name, "cpus": m.Size.CPUs, "memory_mb": m.Size.MemoryMB, "arch": m.Size.Arch},
		"disk_gb": m.DiskGB, "image": map[string]any{"requested": m.Image.Requested, "resolved": m.Image.Resolved},
		"provider": m.Provider, "created_at": tm(m.CreatedAt), "started_at": tm(m.StartedAt), "stopped_at": tm(m.StoppedAt),
		"lease": map[string]any{"expires_at": tm(m.Lease.ExpiresAt), "max_expires_at": tm(m.Lease.MaxExpiresAt),
			"extendable": m.Lease.Extendable, "presets_h": m.Lease.PresetsH},
		"access": access, "ignored_access": m.Ignored, "labels": m.Labels, "spec": m.Spec,
	}
}

// sameJSON compares two values as JSON.
func sameJSON(t *testing.T, a any, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(mustJSON(a), &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

// TestMachineFixtures: each accepted fixture validates; each refused one
// is refused under its rule, and every rule has a refused fixture.
func TestMachineFixtures(t *testing.T) {
	ruled := map[string]bool{}
	for _, rel := range contractFiles(t, "fixtures/machines") {
		var f machineFixture
		decodeClosed(t, rel, readContract(t, rel), &f)
		t.Run(f.Name, func(t *testing.T) {
			if f.Name != strings.TrimSuffix(filepath.Base(rel), ".json") {
				t.Fatalf("name %q is not the file's", f.Name)
			}
			raw := []byte(f.Machine)
			if f.MachineText != nil {
				if f.Machine != nil {
					t.Fatal("both machine and machine_text")
				}
				raw = []byte(*f.MachineText)
			}
			c := Context{ShedID: f.Context.ShedID, Server: f.Context.Server, TestDrivers: f.Context.TestDrivers}
			m, err := DecodeMachine(raw, c)
			switch f.Expect.Result {
			case "accepted":
				if f.Expect.Rule != "" {
					t.Fatal("an accepted fixture names a rule")
				}
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if f.Expect.Decoded == nil {
					t.Fatal("an accepted fixture without decoded")
				}
				if !sameJSON(t, renderMachine(m), f.Expect.Decoded) {
					t.Errorf("decoded\n%s\nwant\n%s", mustJSON(renderMachine(m)), f.Expect.Decoded)
				}
			case "refused":
				if f.Expect.Decoded != nil {
					t.Fatal("a refused fixture with decoded")
				}
				var ie *InvalidError
				if !errors.As(err, &ie) {
					t.Fatalf("accepted (or %v), want refused under %s", err, f.Expect.Rule)
				}
				if ie.Rule != f.Expect.Rule {
					t.Fatalf("refused under %s (%v), want %s", ie.Rule, ie, f.Expect.Rule)
				}
				ruled[ie.Rule] = true
			default:
				t.Fatalf("result %q", f.Expect.Result)
			}
		})
	}
	for _, r := range MachineRules {
		if !ruled[r] {
			t.Errorf("no refused machine fixture for the rule %s", r)
		}
	}
}

type replyFixture struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Call        Call       `json:"call"`
	Replies     []fixReply `json:"replies"`
	Expect      struct {
		Result      string `json:"result"`
		Code        string `json:"code"`
		Kind        string `json:"kind"`
		RetryAfterS *int   `json:"retry_after_s"`
		Operation   string `json:"operation"`
	} `json:"expect"`
}

type fixReply struct {
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers"`
	Body        json.RawMessage   `json:"body"`
	Text        *string           `json:"text"`
	Generate    string            `json:"generate"`
	Silent      bool              `json:"silent"`
	Unreachable bool              `json:"unreachable"`
}

// The credentials the decoder test presents: the first attempt's, and,
// after a 401, the retry's (a new token, obtained with a new assertion). A
// fixture reply's {{bearer}} and {{assertion}} stand for its own attempt's.
var attemptCredentials = [2][2]string{
	{"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl", "eyJhbGciOiJFZERTQSJ9.eyJpc3MiOiJzcnZfdGVzdCJ9.YXNzZXJ0aW9u"},
	{"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyZXRyeSJ9.c2Vjb25k", "eyJhbGciOiJFZERTQSJ9.eyJqdGkiOiJyZXRyeSJ9.YmlzYmlz"},
}

// escapeFirst writes a credential with its first character as a \u
// escape: the same string once decoded, not the same bytes.
func escapeFirst(s string) string { return fmt.Sprintf(`\u%04x%s`, s[0], s[1:]) }

// fillCredentials puts an attempt's credentials in for their
// placeholders.
func fillCredentials(b []byte, bearer, assertion string) []byte {
	return []byte(strings.NewReplacer(
		"{{bearer}}", bearer, "{{assertion}}", assertion,
		"{{bearer_escaped}}", escapeFirst(bearer), "{{assertion_escaped}}", escapeFirst(assertion),
	).Replace(string(b)))
}

var errTimeout = errors.New("the 12 s deadline passed")

// sampleMachine is a valid machine for the generated bodies.
func sampleMachine(shedID, spec string) map[string]any {
	return map[string]any{
		"shed_id": shedID, "name": "sample", "server": fixtureServer, "user": "ada", "client": nil, "state": "running",
		"size": map[string]any{"name": "standard", "cpus": 2, "memory_mb": 8192, "arch": "amd64"}, "disk_gb": 50,
		"image": map[string]any{"requested": "default", "resolved": "shed-remote-full"}, "provider": "ref",
		"created_at": "2026-11-16T06:00:00Z", "started_at": "2026-11-16T06:00:00Z", "stopped_at": nil,
		"lease":  map[string]any{"expires_at": "2026-11-17T06:00:00Z", "max_expires_at": "2026-11-21T06:00:00Z", "extendable": true, "presets_h": []int{6}},
		"access": []any{}, "labels": map[string]string{}, "spec": spec,
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// generated builds a generated body, the decoder test's own (the
// reference service's sending test checks the service's).
func generated(gen, bearer string) (status int, body []byte, err error) {
	pad := func(head []byte, total int) []byte {
		head = append(head, `,"pad":"`...)
		n := total - len(head) - 2
		return append(append(head, bytes.Repeat([]byte("a"), n)...), '"', '}')
	}
	switch gen {
	case "oversize":
		head := mustJSON(map[string]any{"machine": sampleMachine(fixtureShedID, "")})
		return 200, pad(head[:len(head)-1], MaxReply+1), nil
	case "oversize_list":
		return 200, pad([]byte(`{"machines":[],"deleted":[],"complete":true`), MaxListReply+1), nil
	case "list_501":
		ms := []any{}
		for i := 0; i <= MaxListMachines; i++ {
			id := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaa")
			for j, n := 25, i; j >= 0 && n > 0; j, n = j-1, n/26 {
				id[j] = byte('a' + n%26)
			}
			ms = append(ms, sampleMachine(string(id), ""))
		}
		return 200, mustJSON(map[string]any{"machines": ms, "deleted": []any{}, "complete": true}), nil
	case "truncated":
		b := mustJSON(map[string]any{"machine": sampleMachine(fixtureShedID, "")})
		return 200, b[:len(b)/2], io.ErrUnexpectedEOF
	case "echo_bearer":
		return 200, mustJSON(map[string]any{"machine": sampleMachine(fixtureShedID, bearer)}), nil
	case "echo_bearer_escaped":
		b := mustJSON(map[string]any{"machine": sampleMachine(fixtureShedID, "SPEC-HERE")})
		return 200, bytes.Replace(b, []byte(`"SPEC-HERE"`), []byte(`"`+escapeFirst(bearer)+`"`), 1), nil
	case "oversize_error_echo_bearer":
		head := mustJSON(map[string]any{"error": map[string]string{"code": "BUSY", "message": "token " + bearer}})
		return 409, append(head, bytes.Repeat([]byte(" "), MaxErrorBody)...), nil
	case "oversize_error":
		return 409, mustJSON(map[string]any{"error": map[string]string{"code": "BUSY", "message": strings.Repeat("busy ", 1000)}}), nil
	}
	return 0, nil, fmt.Errorf("no generator %q", gen)
}

// asReply makes a fixture reply into what a client would have read.
func asReply(t *testing.T, call Call, fr fixReply, attempt int) Reply {
	t.Helper()
	switch {
	case fr.Silent:
		return Reply{Err: errTimeout, Bearer: attemptCredentials[attempt][0], Assertion: attemptCredentials[attempt][1]}
	case fr.Unreachable:
		return Reply{Err: syscall.ECONNREFUSED, Bearer: attemptCredentials[attempt][0], Assertion: attemptCredentials[attempt][1]}
	}
	bearer, assertion := attemptCredentials[attempt][0], attemptCredentials[attempt][1]
	h := http.Header{}
	for k, v := range fr.Headers {
		h.Set(k, v)
	}
	r := Reply{Status: fr.Status, Header: h, Bearer: bearer, Assertion: assertion}
	switch {
	case fr.Generate != "":
		status, body, err := generated(fr.Generate, bearer)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
		r.Status, r.Err = status, err
		// What a client reads, bounded as ReadBody bounds it.
		b, rerr := ReadBody(bytes.NewReader(body), BodyLimit(call, status))
		r.Body, r.TooLarge = b, errors.Is(rerr, ErrTooLarge)
	case fr.Text != nil:
		r.Body = fillCredentials([]byte(*fr.Text), bearer, assertion)
	case fr.Body != nil:
		r.Body = fillCredentials(fr.Body, bearer, assertion)
	}
	return r
}

// TestReplyFixtures classifies every reply fixture.
func TestReplyFixtures(t *testing.T) {
	statuses := map[int]bool{}
	for _, rel := range contractFiles(t, "fixtures/replies") {
		var f replyFixture
		decodeClosed(t, rel, readContract(t, rel), &f)
		t.Run(f.Name, func(t *testing.T) {
			if f.Name != strings.TrimSuffix(filepath.Base(rel), ".json") {
				t.Fatalf("name %q is not the file's", f.Name)
			}
			if !containsCall(f.Call) {
				t.Fatalf("call %q is not a call", f.Call)
			}
			if len(f.Replies) == 0 || len(f.Replies) > 2 {
				t.Fatalf("%d replies", len(f.Replies))
			}
			var rs []Reply
			for i, fr := range f.Replies {
				rs = append(rs, asReply(t, f.Call, fr, i))
				statuses[fr.Status] = true
			}
			c := Context{ShedID: fixtureShedID, Server: fixtureServer}
			if f.Call == CallList {
				c.ShedID = ""
			}
			got := Resolve(f.Call, rs, c)
			if string(got.Result) != f.Expect.Result {
				t.Fatalf("result %s (kind %s, code %q), want %s", got.Result, kindName(got.Kind), got.Code, f.Expect.Result)
			}
			e := f.Expect
			switch got.Result {
			case ResultRefused:
				if got.Code != e.Code {
					t.Errorf("code %q, want %q", got.Code, e.Code)
				}
				info, ok := Codes[e.Code]
				if !ok {
					t.Fatalf("%s is not in the code list", e.Code)
				}
				if kindName(info.Kind) != e.Kind || got.Kind != info.Kind {
					t.Errorf("kind %s, mapping %s, fixture %s", kindName(got.Kind), kindName(info.Kind), e.Kind)
				}
			case ResultUnknown, ResultFailed:
				if kindName(got.Kind) != e.Kind || e.Code != "" {
					t.Errorf("kind %s, want %s", kindName(got.Kind), e.Kind)
				}
			case ResultRetry:
				if e.RetryAfterS == nil || got.RetryAfter != time.Duration(*e.RetryAfterS)*time.Second {
					t.Errorf("retry after %v, want %v s", got.RetryAfter, e.RetryAfterS)
				}
			case ResultOK:
				if f.Call == CallOperation {
					op := got.Value.(*Operation)
					var what string
					switch {
					case !op.Done:
						what = "pending"
					case op.Gone:
						what = "gone"
					case op.Machine != nil:
						what = "machine"
					default:
						what = op.ErrorCode
						if kindName(OperationKind(op.ErrorCode)) != e.Kind {
							t.Errorf("operation kind %s, want %s", kindName(OperationKind(op.ErrorCode)), e.Kind)
						}
					}
					if what != e.Operation {
						t.Errorf("operation %q, want %q", what, e.Operation)
					}
				} else if e.Operation != "" || e.Kind != "" {
					t.Error("operation and kind are for an operation's reply")
				}
			}
		})
	}
	for _, s := range []int{200, 202, 204, 401, 403, 404, 409, 413, 422, 429, 500, 502, 503} {
		if !statuses[s] {
			t.Errorf("no reply fixture with status %d", s)
		}
	}
}

func containsCall(c Call) bool {
	for _, x := range Calls {
		if x == c {
			return true
		}
	}
	return false
}

type mappingFile struct {
	Codes          map[string]mappingEntry `json:"codes"`
	StatusDefaults map[string]string       `json:"status_defaults"`
	Failures       map[string]mappingEntry `json:"failures"`
}

type mappingEntry struct {
	Status   int    `json:"status"`
	Provider string `json:"provider"`
	Refusal  bool   `json:"refusal"`
	ShedAPI  struct {
		Status int     `json:"status"`
		Code   string  `json:"code"`
		Reason *string `json:"reason"`
	} `json:"shed_api"`
}

func (e mappingEntry) info(t *testing.T, name string) CodeInfo {
	t.Helper()
	k, ok := kinds[e.Provider]
	if !ok {
		t.Fatalf("%s: %q is not a provider kind", name, e.Provider)
	}
	if provider.IsRefusal(k) != e.Refusal {
		t.Errorf("%s: refusal %v, but provider.IsRefusal(%s) is %v", name, e.Refusal, e.Provider, provider.IsRefusal(k))
	}
	reason := ""
	if e.ShedAPI.Reason != nil {
		reason = *e.ShedAPI.Reason
	}
	return CodeInfo{Status: e.Status, Kind: k, ShedAPI: ShedAPI{e.ShedAPI.Status, e.ShedAPI.Code, reason}}
}

// TestMapping holds Codes, StatusDefaults and Failures equal to
// mapping.json.
func TestMapping(t *testing.T) {
	var m mappingFile
	decodeClosed(t, "fixtures/mapping.json", readContract(t, "fixtures/mapping.json"), &m)
	got := map[string]CodeInfo{}
	for code, e := range m.Codes {
		got[code] = e.info(t, code)
	}
	if !reflect.DeepEqual(got, Codes) {
		for code := range Codes {
			if !reflect.DeepEqual(got[code], Codes[code]) {
				t.Errorf("%s: mapping.json %+v, Codes %+v", code, got[code], Codes[code])
			}
		}
		for code := range got {
			if _, ok := Codes[code]; !ok {
				t.Errorf("mapping.json has %s, Codes does not", code)
			}
		}
	}
	defaults := map[int]string{}
	for s, code := range m.StatusDefaults {
		var n int
		fmt.Sscan(s, &n)
		defaults[n] = code
		if _, ok := Codes[code]; !ok {
			t.Errorf("status_defaults %s: %s is not a code", s, code)
		}
	}
	if !reflect.DeepEqual(defaults, StatusDefaults) {
		t.Errorf("status_defaults %v, StatusDefaults %v", defaults, StatusDefaults)
	}
	fails := map[string]CodeInfo{}
	for name, e := range m.Failures {
		fails[name] = e.info(t, name)
	}
	if !reflect.DeepEqual(fails, Failures) {
		t.Errorf("failures %+v, Failures %+v", fails, Failures)
	}
}

// TestCodeListIsTheContracts holds the code list in CONTRACT.md's section
// "Codes" equal to mapping.json's codes.
func TestCodeListIsTheContracts(t *testing.T) {
	doc := string(readContract(t, "CONTRACT.md"))
	start := strings.Index(doc, "\n## Codes")
	if start < 0 {
		t.Fatal("CONTRACT.md has no section Codes")
	}
	section := doc[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	listed := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^\\| `([A-Z_]+)` \\|").FindAllStringSubmatch(section, -1) {
		listed[m[1]] = true
	}
	for code := range Codes {
		if !listed[code] {
			t.Errorf("CONTRACT.md's Codes table does not list %s", code)
		}
	}
	for code := range listed {
		if _, ok := Codes[code]; !ok {
			t.Errorf("CONTRACT.md's Codes table lists %s, which mapping.json does not have", code)
		}
	}
}

// exchangeFile is what this test reads of an exchange: every response's
// status and body.
type exchangeStep struct {
	Response struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	} `json:"response"`
	Request struct {
		Path string `json:"path"`
	} `json:"request"`
}

// otherCodes are the codes an exchange may answer that are not in the
// code list: the directory contract's 401 and the body limit's 413, which
// a server classifies by its status alone.
var otherCodes = map[string]int{"invalid_token": 401, "PAYLOAD_TOO_LARGE": 413}

// TestExchangeCodes holds every /v1/compute error an exchange answers to
// mapping.json: its code is in the list (or one of otherCodes) and its
// status is the code's.
func TestExchangeCodes(t *testing.T) {
	seen := map[string]bool{}
	for _, rel := range contractFiles(t, "fixtures/exchanges") {
		var ex struct {
			exchangeStep
			Then []exchangeStep `json:"then"`
		}
		if err := json.Unmarshal(readContract(t, rel), &ex); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		for i, st := range append([]exchangeStep{ex.exchangeStep}, ex.Then...) {
			if !strings.HasPrefix(st.Request.Path, "/v1/compute/") {
				continue
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(st.Response.Body, &body)
			code := body.Error.Code
			if st.Response.Status == 200 && code != "" {
				// An operation that ended with an error: its code is listed.
				if _, listed := Codes[code]; !listed {
					t.Errorf("%s step %d: the operation error %s is not in mapping.json", rel, i, code)
				}
				seen[code] = true
				continue
			}
			if st.Response.Status < 400 {
				continue
			}
			if code == "" {
				t.Errorf("%s step %d: an error with no code", rel, i)
				continue
			}
			want, ok := otherCodes[code]
			if info, listed := Codes[code]; listed {
				want, ok = info.Status, true
			}
			switch {
			case !ok:
				t.Errorf("%s step %d: %s is not in mapping.json", rel, i, code)
			case want != st.Response.Status:
				t.Errorf("%s step %d: %s at %d, mapping.json gives %d", rel, i, code, st.Response.Status, want)
			}
			seen[code] = true
		}
	}
	// COMPUTE_DENIED is for a service that refuses compute by policy, which
	// the reference never does.
	var unseen []string
	for code := range Codes {
		if !seen[code] && code != "COMPUTE_DENIED" {
			unseen = append(unseen, code)
		}
	}
	sort.Strings(unseen)
	if len(unseen) > 0 {
		t.Errorf("no exchange answers %v", unseen)
	}
}

// TestFixturesAreJSON checks every fixture reads as JSON (a fixture's own
// text, not a body it carries).
func TestFixturesAreJSON(t *testing.T) {
	for _, dir := range []string{"fixtures/exchanges", "fixtures/machines", "fixtures/replies"} {
		for _, rel := range contractFiles(t, dir) {
			if !json.Valid(readContract(t, rel)) {
				t.Errorf("%s is not JSON", rel)
			}
		}
	}
	for _, rel := range []string{"fixtures/world.json", "fixtures/mapping.json"} {
		if _, err := os.Stat(filepath.Join(contractDir, rel)); err != nil {
			t.Error(err)
		}
	}
}

// planShedAPI is the shed API's answer for each code the plan's section
// 3.4 pins, written from that text and not from mapping.json or Codes, so
// that the two copies cannot share one mistake: the new codes with their
// reasons, the extend errors, the cloud refusals that keep their names, and
// COMPUTE_DENIED for a denied grant (S0 ruling 7).
var planShedAPI = map[string]ShedAPI{
	"QUOTA_PERSON":       {503, "COMPUTE_LIMIT_REACHED", "person"},
	"QUOTA_ORG":          {503, "COMPUTE_LIMIT_REACHED", "organization"},
	"SIZE_NOT_ALLOWED":   {422, "COMPUTE_POLICY_REFUSED", "size"},
	"SIZE_UNKNOWN":       {422, "COMPUTE_POLICY_REFUSED", "size"},
	"LEASE_TOO_LONG":     {422, "COMPUTE_POLICY_REFUSED", "lease"},
	"IMAGE_UNKNOWN":      {422, "COMPUTE_POLICY_REFUSED", "image"},
	"ACCESS_UNSUPPORTED": {422, "COMPUTE_POLICY_REFUSED", "access"},
	"PERSON_UNKNOWN":     {422, "COMPUTE_POLICY_REFUSED", "person"},
	"CLIENT_UNKNOWN":     {422, "COMPUTE_POLICY_REFUSED", "client"},
	"COMPUTE_DENIED":     {403, "COMPUTE_DENIED", ""},
	"INSUFFICIENT_SCOPE": {403, "COMPUTE_DENIED", ""},
	"UNAVAILABLE":        {503, "COMPUTE_UNAVAILABLE", ""},
	"DELETING":           {409, "COMPUTE_DELETING", ""},
	"EXTEND_NOT_ALLOWED": {403, "EXTEND_NOT_ALLOWED", ""},
	"LEASE_AT_CAP":       {409, "LEASE_AT_CAP", ""},
	"LEASE_ENDED":        {409, "LEASE_ENDED", ""},
	"PERSON_INACTIVE":    {403, "PERSON_INACTIVE", ""},
	"CLOUD_QUOTA":        {503, "CLOUD_QUOTA_EXCEEDED", ""},
	"CLOUD_CAPACITY":     {503, "ZONE_CAPACITY_UNAVAILABLE", ""},
}

// TestShedAPIIsThePlans holds both copies of the shed API answers
// (mapping.json and Codes) to the plan's own table, for every code it pins.
func TestShedAPIIsThePlans(t *testing.T) {
	var m mappingFile
	decodeClosed(t, "fixtures/mapping.json", readContract(t, "fixtures/mapping.json"), &m)
	for code, want := range planShedAPI {
		if got := Codes[code].ShedAPI; got != want {
			t.Errorf("Codes %s: %+v, the plan says %+v", code, got, want)
		}
		e, ok := m.Codes[code]
		if !ok {
			t.Errorf("mapping.json has no %s", code)
			continue
		}
		if got := e.info(t, code).ShedAPI; got != want {
			t.Errorf("mapping.json %s: %+v, the plan says %+v", code, got, want)
		}
	}
}

// TestEchoScanFailsClosed: a body the strict reader takes whose string
// scan fails is treated as an echo; a body that is not JSON is judged by
// its bytes alone. And the scan reads a number too large for a float64
// without stopping, to the credential after it.
func TestEchoScanFailsClosed(t *testing.T) {
	bearer := attemptCredentials[0][0]
	clean := []byte(`{"machine":{"spec":"nothing here"}}`)
	if echoes(clean, bearer) {
		t.Fatal("a clean body is an echo")
	}
	big := []byte(`{"future":1e400,"machine":{"spec":"` + escapeFirst(bearer) + `"}}`)
	if !echoes(big, bearer) {
		t.Fatal("the escaped bearer after 1e400 was missed")
	}
	saved := scanStrings
	defer func() { scanStrings = saved }()
	scanStrings = func([]byte, func(string)) error { return errors.New("the scan broke") }
	if !echoes(clean, bearer) {
		t.Error("a scan error on a strict body did not fail closed")
	}
	if echoes([]byte("not json at all"), bearer) {
		t.Error("a body that is not JSON, without the bearer in its bytes, is an echo")
	}
}
