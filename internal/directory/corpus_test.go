package directory

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
)

// acceptEntry is a keys/accept/ file, its members in the file's order.
type acceptEntry struct {
	Line        string `json:"line"`
	Canonical   string `json:"canonical"`
	Fingerprint string `json:"fingerprint"`
	Algorithm   string `json:"algorithm"`
}

// refuseEntry is a keys/refuse/ file, its members in the file's order.
type refuseEntry struct {
	Line            string `json:"line"`
	Code            string `json:"code"`
	ShedAlsoRefuses bool   `json:"shed_also_refuses"`
}

// canonicalKey is the contract's canonical line for a parsed key:
// ssh.MarshalAuthorizedKey without its trailing newline.
func canonicalKey(k gossh.PublicKey) string {
	return strings.TrimSuffix(string(gossh.MarshalAuthorizedKey(k)), "\n")
}

// encodeCorpus renders a corpus file as it is committed: two-space
// indentation, no HTML escaping, a trailing newline.
func encodeCorpus(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeCorpus(t *testing.T, rel string, v any) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(contractDir, filepath.FromSlash(rel)), encodeCorpus(t, v), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCorpusAccept runs every keys/accept/ line through config.ParseUserKey:
// shed accepts it, re-marshals it to exactly canonical, fingerprints it as
// fingerprint, and names its type algorithm. Under UPDATE_CONTRACT=1 those
// three values are recorded from the parser instead.
func TestCorpusAccept(t *testing.T) {
	for _, rel := range fixtureFiles(t, "fixtures/keys/accept", ".json") {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			var e acceptEntry
			decodeFixture(t, rel, &e)
			key, err := config.ParseUserKey(e.Line)
			if err != nil {
				t.Fatalf("ParseUserKey refuses %q: %v", e.Line, err)
			}
			got := acceptEntry{
				Line:        e.Line,
				Canonical:   canonicalKey(key),
				Fingerprint: gossh.FingerprintSHA256(key),
				Algorithm:   key.Type(),
			}
			if updating() {
				writeCorpus(t, rel, got)
				return
			}
			if got != e {
				t.Errorf("shed's parser disagrees with the recorded values:\n got  %+v\n want %+v", got, e)
			}
		})
	}
}

// TestCorpusRefuse runs every keys/refuse/ line through config.ParseUserKey:
// shed refuses it exactly when shed_also_refuses is true. Under
// UPDATE_CONTRACT=1 shed_also_refuses is recorded from the parser instead.
func TestCorpusRefuse(t *testing.T) {
	refused := 0
	for _, rel := range fixtureFiles(t, "fixtures/keys/refuse", ".json") {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			var e refuseEntry
			decodeFixture(t, rel, &e)
			_, err := config.ParseUserKey(e.Line)
			got := e
			got.ShedAlsoRefuses = err != nil
			if got.ShedAlsoRefuses {
				refused++
			}
			if updating() {
				writeCorpus(t, rel, got)
				return
			}
			if got.ShedAlsoRefuses != e.ShedAlsoRefuses {
				t.Errorf("shed_also_refuses is %v, but ParseUserKey returns err=%v", e.ShedAlsoRefuses, err)
			}
		})
	}
	// Both outcomes occur in the corpus: a parser that refused everything, or
	// nothing, would not pass by accident.
	if refused == 0 {
		t.Errorf("shed refuses no line of the refuse corpus")
	}
}

// TestFixtureKeysAreCanonical checks every user key the fixtures serve (the
// world's users and every directory body of the exchanges): shed's parser
// accepts it and re-marshals it to the same line, as the contract's Keys
// section requires of every key a service sends.
func TestFixtureKeysAreCanonical(t *testing.T) {
	type key struct {
		Key string `json:"key"`
	}
	type user struct {
		Name string `json:"name"`
		Keys []key  `json:"keys"`
	}
	check := func(t *testing.T, where string, users []user) {
		for _, u := range users {
			for _, k := range u.Keys {
				pk, err := config.ParseUserKey(k.Key)
				if err != nil {
					t.Errorf("%s: %s: ParseUserKey refuses %q: %v", where, u.Name, k.Key, err)
					continue
				}
				if c := canonicalKey(pk); c != k.Key {
					t.Errorf("%s: %s: key is not canonical:\n got  %q\n want %q", where, u.Name, k.Key, c)
				}
			}
		}
	}

	var world struct {
		Users []user `json:"users"`
	}
	decodeFixture(t, "fixtures/world.json", &world)
	if len(world.Users) == 0 {
		t.Fatal("the world has no users")
	}
	check(t, "world.json", world.Users)

	type step struct {
		Request struct {
			Path string `json:"path"`
		} `json:"request"`
		Response struct {
			Status int `json:"status"`
			Body   struct {
				Users []user `json:"users"`
			} `json:"body"`
		} `json:"response"`
	}
	bodies := 0
	for _, rel := range fixtureFiles(t, "fixtures/exchanges", ".json") {
		var ex struct {
			step
			Then []step `json:"then"`
		}
		decodeFixture(t, rel, &ex)
		for _, s := range append([]step{ex.step}, ex.Then...) {
			if s.Request.Path == "/v1/directory" && s.Response.Status == 200 {
				bodies++
				check(t, rel, s.Response.Body.Users)
			}
		}
	}
	if bodies == 0 {
		t.Error("no exchange serves a directory body")
	}
}
