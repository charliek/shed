package directory

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"testing"
	"time"
)

// clientFixture is one file of fixtures/client, in the language CONTRACT.md
// gives it ("Client fixtures").
type clientFixture struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Response    struct {
		Status     int                        `json:"status"`
		Headers    map[string]json.RawMessage `json:"headers"`
		Body       json.RawMessage            `json:"body"`
		Text       *string                    `json:"text"`
		BodyBase64 *string                    `json:"body_base64"`
	} `json:"response"`
	Expect struct {
		Result string `json:"result"`
		Users  []struct {
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
			Keys  []struct {
				Key     string `json:"key"`
				Label   string `json:"label"`
				Expires string `json:"expires"`
			} `json:"keys"`
		} `json:"users"`
		Skipped []struct {
			Entry  int    `json:"entry"`
			Reason string `json:"reason"`
		} `json:"skipped"`
		RetryAfterSeconds *int `json:"retry_after_seconds"`
	} `json:"expect"`
}

// skippedFixtures are client fixtures this client does not hold itself to,
// each with the reason. Empty: every fixture classifies as it says.
var skippedFixtures = map[string]string{}

func loadClientFixture(t *testing.T, rel string) clientFixture {
	t.Helper()
	var c clientFixture
	dec := json.NewDecoder(bytes.NewReader(readFixture(t, rel)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return c
}

// served is the fixture's answer: its headers (a string, or an array sent as
// one line each) and its body bytes.
func (c clientFixture) served(t *testing.T) served {
	t.Helper()
	s := served{status: c.Response.Status, header: http.Header{}}
	for name, raw := range c.Response.Headers {
		var one string
		var many []string
		switch {
		case json.Unmarshal(raw, &one) == nil:
			s.header[name] = []string{one}
		case json.Unmarshal(raw, &many) == nil:
			s.header[name] = many
		default:
			t.Fatalf("%s: header %s is neither a string nor an array of strings", c.Name, name)
		}
	}
	switch {
	case c.Response.Text != nil:
		s.body = []byte(*c.Response.Text)
	case c.Response.BodyBase64 != nil:
		b, err := base64.StdEncoding.DecodeString(*c.Response.BodyBase64)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		s.body = b
	case len(c.Response.Body) > 0 && string(c.Response.Body) != "null":
		s.body = c.Response.Body
	}
	return s
}

// check holds a classification to the fixture's expect.
func (c clientFixture) check(t *testing.T, how string, p *Pull, err error) {
	t.Helper()
	switch c.Expect.Result {
	case "good":
		if err != nil {
			t.Errorf("%s (%s): failed, want a good pull: %v", c.Name, how, err)
			return
		}
		got, want := describeList(p.List), c.expectedList()
		if got != want {
			t.Errorf("%s (%s):\n got %s\nwant %s", c.Name, how, got, want)
		}
	case "failed":
		if err == nil {
			t.Errorf("%s (%s): a good pull, want a failed refresh (%s)", c.Name, how, describeList(p.List))
			return
		}
		f := asFailure(t, err)
		want := c.Expect.RetryAfterSeconds
		switch {
		case want == nil && f.HasRetryAfter:
			t.Errorf("%s (%s): Retry-After %s honoured, want it treated as absent", c.Name, how, f.RetryAfter)
		case want != nil && !f.HasRetryAfter:
			t.Errorf("%s (%s): Retry-After treated as absent, want %ds", c.Name, how, *want)
		case want != nil && f.RetryAfter != time.Duration(*want)*time.Second:
			t.Errorf("%s (%s): Retry-After %s, want %ds", c.Name, how, f.RetryAfter, *want)
		}
	default:
		t.Fatalf("%s: expect.result %q", c.Name, c.Expect.Result)
	}
}

// describeList renders users (name, admin, each key's line, label and
// expiry) and the skipped entries, for comparison.
func describeList(l *List) string {
	var b strings.Builder
	b.WriteString("users:")
	for _, u := range l.Users {
		fmt.Fprintf(&b, " {%s admin=%v", u.Name, u.Admin)
		for _, k := range u.Keys {
			exp := "-"
			if k.Expires != nil {
				exp = k.Expires.UTC().Format(time.RFC3339Nano)
			}
			fmt.Fprintf(&b, " [%q %q %s]", k.Line, k.Label, exp)
		}
		b.WriteString("}")
	}
	b.WriteString(" skipped:")
	for _, s := range l.Skipped {
		fmt.Fprintf(&b, " %d=%s", s.Entry, s.Reason)
	}
	return b.String()
}

func (c clientFixture) expectedList() string {
	l := &List{}
	for _, u := range c.Expect.Users {
		user := User{Name: u.Name, Admin: u.Admin}
		for _, k := range u.Keys {
			key := Key{Line: k.Key, Label: k.Label}
			if k.Expires != "" {
				ts, err := time.Parse(time.RFC3339, k.Expires)
				if err != nil {
					return fmt.Sprintf("an expected expires %q that is not RFC 3339", k.Expires)
				}
				key.Expires = &ts
			}
			user.Keys = append(user.Keys, key)
		}
		l.Users = append(l.Users, user)
	}
	for _, s := range c.Expect.Skipped {
		l.Skipped = append(l.Skipped, Skipped{Entry: s.Entry, Reason: SkipReason(s.Reason)})
	}
	return describeList(l)
}

// classifyServed classifies a canned directory answer through the same code
// as a pull, without a network: the headers exactly as given (an HTTP server
// and client would trim a value's surrounding whitespace before rule 2 saw
// it).
func classifyServed(s served, org string) (*Pull, error) {
	resp := &http.Response{
		StatusCode:    s.status,
		Header:        s.header,
		Body:          io.NopCloser(bytes.NewReader(s.body)),
		ContentLength: -1,
	}
	a, f := readAnswer(resp, StepDirectory, http.StatusOK, MaxDirectoryBody, worldClock)
	if f != nil {
		return nil, f
	}
	p, f := classifyDirectory(a, org)
	if f != nil {
		return nil, f
	}
	return p, nil
}

// TestClientFixtures classifies every answer in fixtures/client as its
// expect says: the result, the users admitted with the keys used, every
// skipped entry by index with its reason, and the least wait before the
// next try. Each is classified twice: directly (the headers exactly as the
// fixture gives them) and as a real pull over HTTP from a local server.
func TestClientFixtures(t *testing.T) {
	files := fixtureFiles(t, "fixtures/client", ".json")
	for _, rel := range files {
		c := loadClientFixture(t, rel)
		t.Run(c.Name, func(t *testing.T) {
			if want := strings.TrimSuffix(path.Base(rel), ".json"); c.Name != want {
				t.Fatalf("name %q, want %q", c.Name, want)
			}
			if reason, ok := skippedFixtures[c.Name]; ok {
				t.Skip(reason)
			}
			s := c.served(t)

			p, err := classifyServed(s, worldOrg)
			c.check(t, "classified", p, err)

			fs := newFakeService(t)
			fs.directory = []served{s}
			p, err = fs.client(t, worldOrg).Pull(context.Background())
			c.check(t, "pulled", p, err)
			if got := fs.count("GET /v1/directory"); got != 1 {
				t.Errorf("the directory was asked %d times, want once", got)
			}
		})
	}
}

// TestClientFixturesSeeOrganization is TestClientFixtures' control on the
// organization: a client accepting another organization than example
// refuses every answer a fixture calls good.
func TestClientFixturesSeeOrganization(t *testing.T) {
	good := 0
	for _, rel := range fixtureFiles(t, "fixtures/client", ".json") {
		c := loadClientFixture(t, rel)
		if c.Expect.Result != "good" {
			continue
		}
		good++
		if p, err := classifyServed(c.served(t), "another"); err == nil {
			t.Errorf("%s: accepted under the organization another: %s", c.Name, describeList(p.List))
		}
	}
	if good == 0 {
		t.Fatal("no good fixtures")
	}
}
