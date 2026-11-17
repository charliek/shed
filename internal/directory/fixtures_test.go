package directory

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charliek/shed/internal/config"
	gossh "golang.org/x/crypto/ssh"
)

// unknownMembersFixture is the one fixture that carries a github member: it
// shows that a user carrying members this contract does not specify is read
// as if they were absent.
const unknownMembersFixture = "fixtures/client/unknown-members.json"

// TestNoGitHubMember checks that no fixture but client/unknown-members.json
// mentions a github member: a user is a name, an admin flag and keys.
func TestNoGitHubMember(t *testing.T) {
	var files []string
	for _, d := range []struct{ dir, ext string }{
		{"fixtures", ".json"},
		{"fixtures/exchanges", ".json"},
		{"fixtures/client", ".json"},
		{"fixtures/test-keys", ".json"},
		{"fixtures/test-keys", ".pem"},
		{"fixtures/keys/accept", ".json"},
		{"fixtures/keys/refuse", ".json"},
		{"fixtures/keys/sources", ".pub"},
	} {
		files = append(files, fixtureFiles(t, d.dir, d.ext)...)
	}
	seen := false
	for _, rel := range files {
		has := bytes.Contains(bytes.ToLower(readFixture(t, rel)), []byte("github"))
		if rel == unknownMembersFixture {
			seen = has
			continue
		}
		if has {
			t.Errorf("%s mentions github; only %s may", rel, unknownMembersFixture)
		}
	}
	if !seen {
		t.Errorf("%s no longer carries a github member", unknownMembersFixture)
	}
}

// The harness language CONTRACT.md's "The fixtures" section documents. It
// is closed: a member, set-up step or placeholder outside it is an error,
// never a default (a misspelt nbf_ofset read as zero would sign a valid
// token for a case that expects a refusal).
var (
	exchangeMembers       = memberSet("name", "description", "before", "assertion", "token", "request", "response", "then")
	stepMembers           = memberSet("before", "assertion", "token", "request", "response")
	requestMembers        = memberSet("method", "path", "headers", "body")
	responseMembers       = memberSet("status", "headers", "body", "text")
	assertionMembers      = memberSet("key", "client_id", "typ", "aud", "lifetime", "iat_offset", "nbf_offset", "duplicate_claim")
	tokenMembers          = memberSet("client_id", "scope", "nbf_offset", "sub", "aud", "gty", "iss", "typ")
	comparedHeaders       = memberSet("Cache-Control", "Content-Type", "ETag", "Pragma", "Retry-After", "WWW-Authenticate")
	placeholderNames      = memberSet("client_id", "assertion", "access_token", "message", "description", "version")
	clientMembers         = memberSet("name", "description", "response", "expect")
	clientResponseMembers = memberSet("status", "headers", "body", "text", "body_base64")
	goodExpectMembers     = memberSet("result", "users", "skipped")
	failedExpectMembers   = memberSet("result", "retry_after_seconds")
	expectUserMembers     = memberSet("name", "admin", "keys")
	expectKeyMembers      = memberSet("key", "label", "expires")
	skippedMembers        = memberSet("entry", "reason")
	skipReasons           = memberSet("duplicate-member", "malformed", "invalid-name", "duplicate-name", "duplicate-key", "no-usable-key")
	exhaustibleLimits     = memberSet("enroll", "token", "directory")
)

// beforeSteps are the six set-up steps, each with a check of its value.
var beforeSteps = map[string]func(v any) bool{
	"users.remove":    isNonEmptyString,
	"servers.disable": isNonEmptyString,
	"users.set_admin": func(v any) bool {
		a, ok := v.([]any)
		if !ok || len(a) != 2 || !isNonEmptyString(a[0]) {
			return false
		}
		_, ok = a[1].(bool)
		return ok
	},
	"clock.set": func(v any) bool {
		s, ok := v.(string)
		if !ok {
			return false
		}
		_, err := time.Parse(time.RFC3339, s)
		return err == nil
	},
	"lookups.fail":   func(v any) bool { return v == true },
	"limits.exhaust": func(v any) bool { s, ok := v.(string); return ok && exhaustibleLimits[s] },
}

var placeholderRx = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

func memberSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func isNonEmptyString(v any) bool { s, ok := v.(string); return ok && s != "" }

func isInteger(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	_, err := n.Int64()
	return err == nil
}

// decodeAny decodes one JSON value, numbers as json.Number.
func decodeAny(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("data after the JSON value")
	}
	return v, nil
}

// shapeChecker collects the problems of one fixture.
type shapeChecker struct{ problems []string }

func (c *shapeChecker) addf(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

// object returns v as an object whose members are all in allowed, noting
// every other member as a problem.
func (c *shapeChecker) object(where string, v any, allowed map[string]bool) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		c.addf("%s is not an object", where)
		return nil
	}
	for _, k := range sortedKeys(m) {
		if !allowed[k] {
			c.addf("%s has the member %q, which the contract does not document", where, k)
		}
	}
	return m
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// strings visits every string in v (members' values, array elements), with
// its path.
func visitStrings(where string, v any, fn func(where, s string)) {
	switch x := v.(type) {
	case string:
		fn(where, x)
	case []any:
		for i, e := range x {
			visitStrings(fmt.Sprintf("%s[%d]", where, i), e, fn)
		}
	case map[string]any:
		for _, k := range sortedKeys(x) {
			visitStrings(where+"."+k, x[k], fn)
		}
	}
}

// placeholders checks every placeholder in s is a documented one, and that
// no stray brace pair is left, and returns the names used.
func (c *shapeChecker) placeholders(where, s string) []string {
	var names []string
	for _, m := range placeholderRx.FindAllStringSubmatch(s, -1) {
		if !placeholderNames[m[1]] {
			c.addf("%s uses the placeholder {{%s}}, which the contract does not document", where, m[1])
		}
		names = append(names, m[1])
	}
	if rest := placeholderRx.ReplaceAllString(s, ""); strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		c.addf("%s holds a malformed placeholder: %q", where, s)
	}
	return names
}

// exchangeProblems lists everything in an exchange fixture that the harness
// language does not allow. testKeys are the stems of test-keys/*.pem.
func exchangeProblems(raw []byte, testKeys map[string]bool) []string {
	var c shapeChecker
	v, err := decodeAny(raw)
	if err != nil {
		return []string{"not one JSON value: " + err.Error()}
	}
	top := c.object("the scenario", v, exchangeMembers)
	if top == nil {
		return c.problems
	}
	if !isNonEmptyString(top["name"]) {
		c.addf("the scenario has no name")
	}
	if !isNonEmptyString(top["description"]) {
		c.addf("the scenario has no description")
	} else {
		c.placeholders("description", top["description"].(string))
	}
	steps := []map[string]any{top}
	if then, ok := top["then"]; ok {
		arr, ok := then.([]any)
		if !ok || len(arr) == 0 {
			c.addf("then is not a non-empty array")
		}
		for i, s := range arr {
			if m := c.object(fmt.Sprintf("then[%d]", i), s, stepMembers); m != nil {
				steps = append(steps, m)
			}
		}
	}
	for i, s := range steps {
		c.step(fmt.Sprintf("step %d", i), s, testKeys)
	}
	return c.problems
}

func (c *shapeChecker) step(where string, s map[string]any, testKeys map[string]bool) {
	if b, ok := s["before"]; ok {
		arr, ok := b.([]any)
		if !ok || len(arr) == 0 {
			c.addf("%s: before is not a non-empty array", where)
		}
		for i, e := range arr {
			m, ok := e.(map[string]any)
			if !ok || len(m) != 1 {
				c.addf("%s: before[%d] is not an object with one member", where, i)
				continue
			}
			for name, val := range m {
				check, known := beforeSteps[name]
				switch {
				case !known:
					c.addf("%s: before[%d] is the set-up step %q, which the contract does not document", where, i, name)
				case !check(val):
					c.addf("%s: before[%d] %s has the value %v, which the contract does not allow", where, i, name, val)
				}
			}
		}
	}

	_, hasAssertion := s["assertion"]
	_, hasToken := s["token"]
	if hasAssertion && hasToken {
		c.addf("%s has both an assertion and a token", where)
	}
	if hasAssertion {
		c.assertion(where+": assertion", s["assertion"], testKeys)
	}
	if hasToken {
		c.token(where+": token", s["token"])
	}

	req := c.object(where+": request", s["request"], requestMembers)
	resp := c.object(where+": response", s["response"], responseMembers)
	if req == nil || resp == nil {
		return
	}

	// The request.
	method, _ := req["method"].(string)
	reqPath, _ := req["path"].(string)
	if method == "" || !strings.HasPrefix(reqPath, "/") {
		c.addf("%s: the request has no method or path", where)
	}
	if _, ok := req["body"]; !ok {
		c.addf("%s: the request has no body member (null for none)", where)
	}
	usesAssertion, usesToken := false, false
	if h, ok := req["headers"].(map[string]any); ok {
		for _, k := range sortedKeys(h) {
			hv, ok := h[k].(string)
			if !ok {
				c.addf("%s: request header %s is not a string", where, k)
				continue
			}
			for _, n := range c.placeholders(where+": request header "+k, hv) {
				switch n {
				case "access_token":
					usesToken = true
				case "client_id":
				default:
					c.addf("%s: request header %s holds {{%s}}, which belongs elsewhere", where, k, n)
				}
			}
		}
	} else {
		c.addf("%s: the request's headers are not an object", where)
	}
	visitStrings(where+": request body", req["body"], func(w, str string) {
		for _, n := range c.placeholders(w, str) {
			switch n {
			case "assertion":
				usesAssertion = true
			case "client_id":
			default:
				c.addf("%s holds {{%s}}, which belongs elsewhere", w, n)
			}
		}
	})
	if usesAssertion != hasAssertion {
		c.addf("%s: {{assertion}} in the request body and an assertion member must come together", where)
	}
	if hasToken && !usesToken {
		c.addf("%s: a token member without {{access_token}} in the request headers", where)
	}

	// The response.
	if !isInteger(resp["status"]) {
		c.addf("%s: the response has no integer status", where)
	}
	if h, ok := resp["headers"].(map[string]any); ok {
		for name := range comparedHeaders {
			if _, ok := h[name]; !ok {
				c.addf("%s: the response does not list %s", where, name)
			}
		}
		for _, k := range sortedKeys(h) {
			if !comparedHeaders[k] {
				c.addf("%s: the response lists %s, which is not one of the six compared headers", where, k)
			}
			if _, ok := h[k].(string); !ok && h[k] != nil {
				c.addf("%s: response header %s is neither a string nor null", where, k)
			}
		}
	} else {
		c.addf("%s: the response's headers are not an object", where)
	}
	if text, ok := resp["text"]; ok {
		if _, isString := text.(string); !isString || resp["body"] != nil {
			c.addf("%s: response text must be a string, with body null", where)
		}
		visitStrings(where+": response text", text, func(w, str string) {
			if len(c.placeholders(w, str)) > 0 {
				c.addf("%s holds a placeholder; text is compared exactly", w)
			}
		})
	}
	// A placeholder in a response stands only where the table puts it, as
	// a whole string.
	allowedAt := map[string]string{
		"access_token": "access_token", "client_id": "client_id",
		"error.message": "message", "error_description": "description", "version": "version",
	}
	visitStrings("", resp["body"], func(w, str string) {
		names := c.placeholders(where+": response body"+w, str)
		if len(names) == 0 {
			return
		}
		if want, ok := allowedAt[strings.TrimPrefix(w, ".")]; !ok || str != "{{"+want+"}}" {
			c.addf("%s: response body%s holds %q, where the contract allows no such placeholder", where, w, str)
		}
	})
	// And where the table puts one, it must be, so its rule always runs.
	if body, ok := resp["body"].(map[string]any); ok {
		if e, ok := body["error"].(map[string]any); ok && e["message"] != "{{message}}" {
			c.addf("%s: a /v1 error's message is not {{message}}", where)
		}
		if d, ok := body["error_description"]; ok && d != "{{description}}" {
			c.addf("%s: error_description is not {{description}}", where)
		}
		if reqPath == "/v1/directory" && fmt.Sprint(resp["status"]) == "200" && body["version"] != "{{version}}" {
			c.addf("%s: a directory 200's version is not {{version}}", where)
		}
	}
}

func (c *shapeChecker) assertion(where string, v any, testKeys map[string]bool) {
	if m, ok := v.(map[string]any); ok {
		if _, reuse := m["reuse"]; reuse {
			if len(m) != 1 || m["reuse"] != true {
				c.addf("%s: a reuse assertion is exactly {\"reuse\": true}", where)
			}
			return
		}
	}
	m := c.object(where, v, assertionMembers)
	if m == nil {
		return
	}
	if k, _ := m["key"].(string); !testKeys[k] {
		c.addf("%s: key %v is not a test-keys/*.pem stem", where, m["key"])
	}
	if !isNonEmptyString(m["client_id"]) {
		c.addf("%s: no client_id", where)
	} else {
		c.placeholders(where+" client_id", m["client_id"].(string))
	}
	if typ, ok := m["typ"]; ok && typ != nil {
		if _, ok := typ.(string); !ok {
			c.addf("%s: typ is neither a string nor null", where)
		}
	}
	for _, n := range []string{"lifetime", "iat_offset", "nbf_offset"} {
		if val, ok := m[n]; ok && !isInteger(val) {
			c.addf("%s: %s is not an integer", where, n)
		}
	}
	if val, ok := m["duplicate_claim"]; ok && !isNonEmptyString(val) {
		c.addf("%s: duplicate_claim is not a claim name", where)
	}
}

func (c *shapeChecker) token(where string, v any) {
	m := c.object(where, v, tokenMembers)
	if m == nil {
		return
	}
	if !isNonEmptyString(m["client_id"]) {
		c.addf("%s: no client_id", where)
	}
	scope, ok := m["scope"].([]any)
	if !ok {
		c.addf("%s: scope is not an array", where)
	}
	for _, s := range scope {
		if !isNonEmptyString(s) {
			c.addf("%s: a scope is not a string", where)
		}
	}
	if val, ok := m["nbf_offset"]; ok && !isInteger(val) {
		c.addf("%s: nbf_offset is not an integer", where)
	}
	for _, n := range []string{"sub", "gty", "iss", "typ"} {
		if val, ok := m[n]; ok && !isNonEmptyString(val) {
			c.addf("%s: %s is not a string", where, n)
		}
	}
}

// clientProblems lists everything in a client fixture that its language
// does not allow, and the ways its expectation cannot match its body.
func clientProblems(raw []byte) []string {
	var c shapeChecker
	v, err := decodeAny(raw)
	if err != nil {
		return []string{"not one JSON value: " + err.Error()}
	}
	top := c.object("the fixture", v, clientMembers)
	if top == nil {
		return c.problems
	}
	if !isNonEmptyString(top["name"]) || !isNonEmptyString(top["description"]) {
		c.addf("the fixture has no name or description")
	}

	resp := c.object("response", top["response"], clientResponseMembers)
	var entries []any // the body's users, when it has a readable list
	hasEntries := false
	if resp != nil {
		if !isInteger(resp["status"]) {
			c.addf("response has no integer status")
		}
		if h, ok := resp["headers"].(map[string]any); ok {
			for _, k := range sortedKeys(h) {
				switch hv := h[k].(type) {
				case string:
				case []any:
					if len(hv) == 0 {
						c.addf("response header %s is an empty array", k)
					}
					for _, e := range hv {
						if _, ok := e.(string); !ok {
							c.addf("response header %s holds a value that is not a string", k)
						}
					}
				default:
					c.addf("response header %s is neither a string nor an array of strings", k)
				}
			}
		} else if resp["headers"] != nil {
			c.addf("response headers are not an object")
		}
		bodies := 0
		for _, k := range []string{"body", "text", "body_base64"} {
			if val, ok := resp[k]; ok && val != nil {
				bodies++
			}
		}
		if bodies > 1 {
			c.addf("response has more than one of body, text and body_base64")
		}
		var bodyValue any
		if val, ok := resp["text"]; ok {
			s, isString := val.(string)
			if !isString {
				c.addf("response text is not a string")
			} else if dv, err := decodeAny([]byte(s)); err == nil {
				bodyValue = dv
			}
		}
		if val, ok := resp["body_base64"]; ok {
			s, _ := val.(string)
			if _, err := base64.StdEncoding.Strict().DecodeString(s); err != nil || s == "" {
				c.addf("response body_base64 is not padded standard base64")
			}
		}
		if val, ok := resp["body"]; ok && val != nil {
			bodyValue = val
		}
		if b, ok := bodyValue.(map[string]any); ok {
			entries, hasEntries = b["users"].([]any)
		}
	}

	expect, _ := top["expect"].(map[string]any)
	switch result := expect["result"]; result {
	case "good":
		c.object("expect", expect, goodExpectMembers)
		users, ok := expect["users"].([]any)
		if !ok {
			c.addf("expect.users is not an array")
		}
		prev := ""
		for i, u := range users {
			um := c.object(fmt.Sprintf("expect.users[%d]", i), u, expectUserMembers)
			if um == nil {
				continue
			}
			name, _ := um["name"].(string)
			if name == "" || name <= prev && i > 0 {
				c.addf("expect.users[%d]: names are not unique and in order", i)
			}
			prev = name
			if _, ok := um["admin"].(bool); !ok {
				c.addf("expect.users[%d]: admin is not a boolean", i)
			}
			keys, ok := um["keys"].([]any)
			if !ok || len(keys) == 0 {
				c.addf("expect.users[%d]: keys is not a non-empty array", i)
			}
			for j, k := range keys {
				km := c.object(fmt.Sprintf("expect.users[%d].keys[%d]", i, j), k, expectKeyMembers)
				if km != nil && (!isNonEmptyString(km["key"]) || !isNonEmptyString(km["label"])) {
					c.addf("expect.users[%d].keys[%d] has no key or label", i, j)
				}
			}
		}
		skipped, ok := expect["skipped"].([]any)
		if !ok {
			c.addf("expect.skipped is not an array of {entry, reason}")
		}
		last := int64(-1)
		for i, s := range skipped {
			sm := c.object(fmt.Sprintf("expect.skipped[%d]", i), s, skippedMembers)
			if sm == nil {
				continue
			}
			n, ok := sm["entry"].(json.Number)
			idx, err := n.Int64()
			if !ok || err != nil || idx <= last {
				c.addf("expect.skipped[%d]: entry is not an index greater than the one before", i)
			} else {
				last = idx
			}
			if hasEntries && idx >= int64(len(entries)) {
				c.addf("expect.skipped[%d]: entry %d is past the body's %d users", i, idx, len(entries))
			}
			if r, _ := sm["reason"].(string); !skipReasons[r] {
				c.addf("expect.skipped[%d]: reason %v is not one the contract documents", i, sm["reason"])
			}
		}
		if hasEntries && len(users)+len(skipped) != len(entries) {
			c.addf("expect admits %d users and skips %d entries, but the body has %d", len(users), len(skipped), len(entries))
		}
	case "failed":
		c.object("expect", expect, failedExpectMembers)
		ra, ok := expect["retry_after_seconds"]
		if !ok {
			c.addf("expect.retry_after_seconds is missing (null for absent)")
		} else if ra != nil {
			n, isNumber := ra.(json.Number)
			secs, err := n.Int64()
			if !isNumber || err != nil || secs < 0 || secs > 3600 {
				c.addf("expect.retry_after_seconds %v is not a number of seconds from 0 to 3600", ra)
			}
		}
	default:
		c.addf("expect.result %v is neither good nor failed", result)
	}
	return c.problems
}

// testKeyStems are the stems of the committed test-keys/*.pem files.
func testKeyStems(t *testing.T) map[string]bool {
	t.Helper()
	stems := map[string]bool{}
	for _, rel := range fixtureFiles(t, "fixtures/test-keys", ".pem") {
		stems[strings.TrimSuffix(path.Base(rel), ".pem")] = true
	}
	return stems
}

// TestFixtureShapes holds every exchange and client fixture to the harness
// language CONTRACT.md documents: its name is its file name, and every
// member, set-up step and placeholder is one the contract names, where the
// contract puts it.
func TestFixtureShapes(t *testing.T) {
	stems := testKeyStems(t)
	for _, rel := range fixtureFiles(t, "fixtures/exchanges", ".json") {
		raw := readFixture(t, rel)
		for _, p := range exchangeProblems(raw, stems) {
			t.Errorf("%s: %s", rel, p)
		}
		var named struct{ Name string }
		decodeFixture(t, rel, &named)
		if want := strings.TrimSuffix(path.Base(rel), ".json"); named.Name != want {
			t.Errorf("%s: name %q, want %q", rel, named.Name, want)
		}
	}
	for _, rel := range fixtureFiles(t, "fixtures/client", ".json") {
		for _, p := range clientProblems(readFixture(t, rel)) {
			t.Errorf("%s: %s", rel, p)
		}
		var named struct{ Name string }
		decodeFixture(t, rel, &named)
		if want := strings.TrimSuffix(path.Base(rel), ".json"); named.Name != want {
			t.Errorf("%s: name %q, want %q", rel, named.Name, want)
		}
	}
}

const (
	shapeHeaders  = `{"Cache-Control":"no-store","Content-Type":"application/json","ETag":null,"Pragma":null,"Retry-After":null,"WWW-Authenticate":null}`
	shapeExchange = `{"name":"x","description":"d","assertion":{"key":"server-ed25519","client_id":"srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a","lifetime":60},"request":{"method":"POST","path":"/oauth2/token","headers":{"Content-Type":"application/x-www-form-urlencoded"},"body":"client_assertion={{assertion}}"},"response":{"status":200,"headers":` + shapeHeaders + `,"body":{"access_token":"{{access_token}}"}},"then":[{"before":[{"clock.set":"2026-11-16T06:01:00Z"}],"token":{"client_id":"srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a","scope":["directory:read"],"nbf_offset":120},"request":{"method":"GET","path":"/v1/directory","headers":{"Authorization":"Bearer {{access_token}}"},"body":null},"response":{"status":200,"headers":` + shapeHeaders + `,"body":{"version":"{{version}}","users":[]}}}]}`
	shapeGoodCase = `{"name":"x","description":"d","response":{"status":200,"headers":{"Content-Type":"application/json"},"body":{"contract":1,"users":[{"name":"ada","admin":true,"keys":[{"key":"k","label":"l"}]},{"name":"bob"}]}},"expect":{"result":"good","users":[{"name":"ada","admin":true,"keys":[{"key":"k","label":"l"}]}],"skipped":[{"entry":1,"reason":"malformed"}]}}`
	shapeFailCase = `{"name":"y","description":"d","response":{"status":429,"headers":{"Retry-After":["30","30"]}},"expect":{"result":"failed","retry_after_seconds":null}}`
)

// TestFixtureShapeChecker is the checker's own control: a well-formed
// fixture passes, and each misspelt member, undocumented set-up step or
// placeholder, or impossible expectation fails it.
func TestFixtureShapeChecker(t *testing.T) {
	stems := memberSet("server-ed25519")
	if p := exchangeProblems([]byte(shapeExchange), stems); len(p) != 0 {
		t.Fatalf("the checker refuses a well-formed exchange: %v", p)
	}
	if p := clientProblems([]byte(shapeGoodCase)); len(p) != 0 {
		t.Fatalf("the checker refuses a well-formed good client fixture: %v", p)
	}
	if p := clientProblems([]byte(shapeFailCase)); len(p) != 0 {
		t.Fatalf("the checker refuses a well-formed failed client fixture: %v", p)
	}
	for _, tc := range []struct{ base, old, new string }{
		{shapeExchange, `"nbf_offset"`, `"nbf_ofset"`},
		{shapeExchange, `"lifetime"`, `"lifetim"`},
		{shapeExchange, `"clock.set"`, `"clock.sett"`},
		{shapeExchange, `"2026-11-16T06:01:00Z"`, `"tomorrow"`},
		{shapeExchange, `{{version}}`, `{{revision}}`},
		{shapeExchange, `"{{version}}"`, `"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`},
		{shapeExchange, `client_assertion={{assertion}}`, `client_assertion={{assertion}`},
		{shapeExchange, `"server-ed25519"`, `"server-ed25591"`},
		{shapeExchange, `"then"`, `"next"`},
		{shapeExchange, `"method":"GET"`, `"method":"GET","query":"x"`},
		{shapeExchange, `"ETag":null,`, ``},
		{shapeExchange, `"body":{"access_token"`, `"body":{"note":"{{message}}","access_token"`},
		{shapeExchange, `"Authorization":"Bearer {{access_token}}"`, `"Authorization":"Bearer x"`},
		{shapeGoodCase, `"malformed"`, `"malformd"`},
		{shapeGoodCase, `"entry":1`, `"entry":2`},
		{shapeGoodCase, `"reason"`, `"reasn"`},
		{shapeGoodCase, `[{"entry":1,"reason":"malformed"}]`, `{"bob":"malformed"}`},
		{shapeGoodCase, `"result":"good"`, `"result":"fine"`},
		{shapeGoodCase, `"status":200,`, `"status":200,"body_base64":"AA==",`},
		{shapeGoodCase, `"label":"l"}]}],"skipped"`, `"label":"l","fingerprint":"f"}]}],"skipped"`},
		{shapeFailCase, `"retry_after_seconds"`, `"retry_after_second"`},
		{shapeFailCase, `"retry_after_seconds":null`, `"retry_after_seconds":7200`},
	} {
		if !strings.Contains(tc.base, tc.old) {
			t.Fatalf("the control's base fixture does not hold %q", tc.old)
		}
		broken := []byte(strings.Replace(tc.base, tc.old, tc.new, 1))
		var problems []string
		if tc.base == shapeExchange {
			problems = exchangeProblems(broken, stems)
		} else {
			problems = clientProblems(broken)
		}
		if len(problems) == 0 {
			t.Errorf("the checker accepts the fixture with %q in place of %q", tc.new, tc.old)
		}
	}
}

// depthOf is a JSON value's nesting by rule 6: each object and each array a
// level, other values none.
func depthOf(v any) int {
	inner := 0
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			inner = max(inner, depthOf(e))
		}
	case map[string]any:
		for _, e := range x {
			inner = max(inner, depthOf(e))
		}
	default:
		return 0
	}
	return inner + 1
}

// TestBoundaryFixtures checks the client fixtures that pin a boundary sit
// exactly on it: nesting-16 nests 16 levels and each nesting-17 fixture 17,
// and invalid-utf8's body is invalid UTF-8 and, repaired, would be a good
// answer, so that only the UTF-8 check refuses it.
func TestBoundaryFixtures(t *testing.T) {
	for name, want := range map[string]int{"nesting-16": 16, "nesting-17-arrays": 17, "nesting-17-in-a-user": 17} {
		var c struct {
			Response struct {
				Body any `json:"body"`
			} `json:"response"`
		}
		decodeFixture(t, "fixtures/client/"+name+".json", &c)
		if got := depthOf(c.Response.Body); got != want {
			t.Errorf("%s nests %d levels, want %d", name, got, want)
		}
	}

	var c struct {
		Response struct {
			BodyBase64 string `json:"body_base64"`
		} `json:"response"`
	}
	decodeFixture(t, "fixtures/client/invalid-utf8.json", &c)
	b, err := base64.StdEncoding.DecodeString(c.Response.BodyBase64)
	if err != nil {
		t.Fatal(err)
	}
	if utf8.Valid(b) {
		t.Fatal("invalid-utf8's body is valid UTF-8")
	}
	var repaired struct {
		Contract int   `json:"contract"`
		Users    []any `json:"users"`
	}
	if err := json.Unmarshal(bytes.ToValidUTF8(b, []byte("?")), &repaired); err != nil || repaired.Contract != 1 || len(repaired.Users) == 0 {
		t.Errorf("invalid-utf8's body, repaired, is not an otherwise good answer: %v", err)
	}
}

// TestDuplicateKeyVariants checks client/duplicate-key.json holds what its
// description says: lines that differ only by a comment or by whitespace,
// each pair the same key blob, so a reader comparing text would admit them.
func TestDuplicateKeyVariants(t *testing.T) {
	var c struct {
		Response struct {
			Body struct {
				Users []struct {
					Keys []struct {
						Key string `json:"key"`
					} `json:"keys"`
				} `json:"users"`
			} `json:"body"`
		} `json:"response"`
	}
	decodeFixture(t, "fixtures/client/duplicate-key.json", &c)
	byBlob := map[string]map[string]bool{}
	for _, u := range c.Response.Body.Users {
		for _, k := range u.Keys {
			pk, err := config.ParseUserKey(k.Key)
			if err != nil {
				t.Fatalf("ParseUserKey refuses %q: %v", k.Key, err)
			}
			fp := gossh.FingerprintSHA256(pk)
			if byBlob[fp] == nil {
				byBlob[fp] = map[string]bool{}
			}
			byBlob[fp][k.Key] = true
		}
	}
	comment, whitespace := false, false
	for _, lines := range byBlob {
		if len(lines) < 2 {
			continue
		}
		for line := range lines {
			fields := strings.Fields(line)
			switch {
			case len(fields) > 2:
				comment = true
			case strings.Join(fields, " ") != line:
				whitespace = true
			}
		}
	}
	if !comment || !whitespace {
		t.Errorf("duplicate-key.json lacks a pair of lines of one blob differing by a comment (%v) or by whitespace (%v)", comment, whitespace)
	}
}
