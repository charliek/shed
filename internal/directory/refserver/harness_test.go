package refserver

import (
	"strings"
	"testing"
)

// TestHarnessStrictBody is the negative control of the conformance
// harness's body check: a correct body passes, and each malformed one an
// implementation could send is refused before it is compared.
func TestHarnessStrictBody(t *testing.T) {
	good := `{"contract":1,"users":[{"name":"ada","keys":[]}],"version":"v"}`
	if err := strictBody([]byte(good)); err != nil {
		t.Fatalf("the good body: %v", err)
	}
	for name, body := range map[string]string{
		"a stray closing brace":           good + `}`,
		"a stray closing bracket":         good + `]`,
		"a second value":                  good + `{}`,
		"a second scalar":                 good + ` 1`,
		"a top-level member twice":        `{"contract":2,"contract":1}`,
		"a nested member twice":           `{"users":[{"name":"ada","name":"bob"}]}`,
		"a member twice deep in an array": `[[{"a":1}],[{"b":1,"b":2}]]`,
		"invalid UTF-8":                   "{\"a\":\"\xff\"}",
		"empty":                           ``,
		"unclosed":                        `{"a":1`,
		"text":                            `nope`,
	} {
		if err := strictBody([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// decodeNumbers, which the comparison uses, also needs a real end of
	// input.
	for _, body := range []string{good + `}`, good + `]`} {
		if _, err := decodeNumbers([]byte(body)); err == nil {
			t.Errorf("decodeNumbers accepted %q", body)
		}
	}
	if _, err := decodeNumbers([]byte(good + " \n")); err != nil {
		t.Errorf("decodeNumbers refused trailing whitespace: %v", err)
	}
}

// TestHarnessLimitCaller checks limits.exhaust's caller: the harness's
// address for the enrolment and token limits, the scenario's server for the
// directory limit, and an error, never the empty caller, when no step names
// a server.
func TestHarnessLimitCaller(t *testing.T) {
	s := &scenario{}
	for _, kind := range []string{"enroll", "token"} {
		if c, err := s.limitCaller(kind); err != nil || c != strings.Split(callerAddr, ":")[0] {
			t.Errorf("%s: %q %v", kind, c, err)
		}
	}
	if c, err := s.limitCaller("directory"); err == nil {
		t.Errorf("directory with no server named: %q, want an error", c)
	}
	if _, err := s.limitCaller("other"); err == nil {
		t.Error("an unknown limit accepted")
	}
	s.server = "{{client_id}}"
	if c, err := s.limitCaller("directory"); err == nil {
		t.Errorf("directory with {{client_id}} unbound: %q, want an error", c)
	}
	s.clientID = srvA
	if c, err := s.limitCaller("directory"); err != nil || c != srvA {
		t.Errorf("directory, bound: %q %v", c, err)
	}
	s.server = srvB
	if c, err := s.limitCaller("directory"); err != nil || c != srvB {
		t.Errorf("directory: %q %v", c, err)
	}
}
