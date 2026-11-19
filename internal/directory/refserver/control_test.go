package refserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// controlServer is a reference server on loopback HTTP whose inputs are a
// control directory.
type controlServer struct {
	t        *testing.T
	dir      string
	srv      *Server
	http     *httptest.Server
	mu       sync.Mutex
	enrolled []string
	// lastError is the error code of the last refused enrolment.
	lastError string
}

// controlDir is a new, empty control directory: mode 0700, whatever the
// umask.
func controlDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// openControl opens a control directory for the test's length.
func openControl(t *testing.T, dir string) *ControlDir {
	t.Helper()
	cd, err := OpenControlDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cd.Close() })
	return cd
}

func newControlServer(t *testing.T) *controlServer {
	t.Helper()
	return newControlServerWith(t, nil)
}

// newControlServerWith is newControlServer with the handler wrapped, when
// wrap is not nil, before the server starts.
func newControlServerWith(t *testing.T, wrap func(http.Handler) http.Handler) *controlServer {
	t.Helper()
	c := &controlServer{t: t, dir: controlDir(t)}
	c.http = httptest.NewUnstartedServer(nil)
	issuer := "http://" + c.http.Listener.Addr().String()
	srv, err := New(Config{
		Issuer:       issuer,
		Organization: "example",
		Inputs:       openControl(t, c.dir),
		OnEnrol: func(name, id string) {
			c.mu.Lock()
			c.enrolled = append(c.enrolled, name+" "+id)
			c.mu.Unlock()
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.srv = srv
	c.http.Config.Handler = srv
	if wrap != nil {
		c.http.Config.Handler = wrap(srv)
	}
	c.http.Start()
	t.Cleanup(c.http.Close)
	return c
}

func (c *controlServer) write(name, content string) {
	c.t.Helper()
	if err := os.WriteFile(filepath.Join(c.dir, name), []byte(content), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

func (c *controlServer) url(path string) string { return c.srv.Issuer() + path }

// enrol enrols a new key with a code over HTTP and returns the client id
// and the private key, or the status of a refusal.
func (c *controlServer) enrol(code string) (string, ed25519.PrivateKey, int) {
	c.t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		c.t.Fatal(err)
	}
	body := enrolBody(code, enrolName, edKey(pub))
	res, err := http.Post(c.url(pathEnroll), "application/json", strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		ClientID      string `json:"client_id"`
		TokenEndpoint string `json:"token_endpoint"`
	}
	if res.StatusCode != http.StatusCreated {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		c.lastError = e.Error.Code
		return "", nil, res.StatusCode
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		c.t.Fatal(err)
	}
	if out.TokenEndpoint != c.url(pathToken) {
		c.t.Errorf("token_endpoint %q", out.TokenEndpoint)
	}
	return out.ClientID, priv, res.StatusCode
}

// token logs a client in over HTTP.
func (c *controlServer) token(clientID string, key ed25519.PrivateKey) (string, int) {
	c.t.Helper()
	now := time.Now().Unix()
	claims, _ := json.Marshal(map[string]any{
		"iss": clientID, "sub": clientID, "aud": c.srv.Issuer(),
		"iat": now, "exp": now + 60, "jti": randomHex(16),
	})
	input := b64([]byte(`{"alg":"EdDSA","typ":"client-authentication+jwt"}`)) + "." + b64(claims)
	a := input + "." + b64(ed25519.Sign(key, []byte(input)))
	res, err := http.Post(c.url(pathToken), "application/x-www-form-urlencoded", strings.NewReader(formPrefix+url.QueryEscape(a)))
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil && res.StatusCode == http.StatusOK {
		c.t.Fatal(err)
	}
	return out.AccessToken, res.StatusCode
}

// pull fetches the directory over HTTP.
func (c *controlServer) pull(tok string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.url(pathDirectory), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return res.StatusCode, m
}

// TestControlDirFlow runs enrolment, login and pulls against a control
// directory over loopback HTTP: codes are spent once, users.json is read on
// each pull, the disabled file and the outage file take effect at once.
func TestControlDirFlow(t *testing.T) {
	c := newControlServer(t)
	k1 := newSSHKey(t)
	c.write("users.json", `[{"name":"ada","admin":true,"keys":[{"key":"`+k1+`","label":"laptop"}]}]`)
	c.write("codes", "build-1 code-one\n\nbuild-2 code-two\n")

	if _, _, status := c.enrol("no-such-code"); status != http.StatusUnauthorized {
		t.Fatalf("an unknown code: %d", status)
	}
	id, key, status := c.enrol("code-one")
	if status != http.StatusCreated {
		t.Fatalf("enrol: %d", status)
	}
	if _, _, status := c.enrol("code-one"); status != http.StatusConflict || c.lastError != "ENROLMENT_CODE_SPENT" {
		t.Fatalf("the code again: %d %s, want 409 ENROLMENT_CODE_SPENT", status, c.lastError)
	}
	if len(c.enrolled) != 1 || c.enrolled[0] != "build-1 "+id {
		t.Fatalf("OnEnrol: %v", c.enrolled)
	}

	tok, status := c.token(id, key)
	if status != http.StatusOK {
		t.Fatalf("token: %d", status)
	}
	status, body := c.pull(tok)
	if status != http.StatusOK || len(body["users"].([]any)) != 1 {
		t.Fatalf("pull: %d %v", status, body)
	}
	v1 := body["version"]

	c.write("users.json", `[]`)
	if status, body := c.pull(tok); status != http.StatusOK || len(body["users"].([]any)) != 0 || body["version"] == v1 {
		t.Fatalf("an emptied list: %d %v", status, body)
	}

	c.write("outage", "")
	if status, _ := c.pull(tok); status != http.StatusServiceUnavailable {
		t.Errorf("outage, directory: %d", status)
	}
	if _, status := c.token(id, key); status != http.StatusServiceUnavailable {
		t.Errorf("outage, token: %d", status)
	}
	if _, _, status := c.enrol("code-two"); status != http.StatusServiceUnavailable {
		t.Errorf("outage, enrolment: %d", status)
	}
	if err := os.Remove(filepath.Join(c.dir, "outage")); err != nil {
		t.Fatal(err)
	}

	c.write("disabled", id+"\n")
	if status, body := c.pull(tok); status != http.StatusForbidden {
		t.Errorf("disabled, directory: %d %v", status, body)
	}
	if _, status := c.token(id, key); status != http.StatusUnauthorized {
		t.Errorf("disabled, token: %d", status)
	}
	// A disabled server's name is free again, its key never.
	id2, _, status := c.enrol("code-two")
	if status != http.StatusCreated || id2 == id {
		t.Errorf("a second server: %d", status)
	}
}

// TestControlDirUsersMissing checks a missing users.json is an outage
// (503), never an empty list.
func TestControlDirUsersMissing(t *testing.T) {
	c := newControlServer(t)
	c.write("codes", "build-1 code-one\n")
	id, key, status := c.enrol("code-one")
	if status != http.StatusCreated {
		t.Fatalf("enrol: %d", status)
	}
	tok, _ := c.token(id, key)
	if status, body := c.pull(tok); status != http.StatusServiceUnavailable {
		t.Fatalf("no users.json: %d %v, want 503", status, body)
	}
}

// TestControlDirChecks checks the control directory's guards: each failure
// is a failed lookup.
func TestControlDirChecks(t *testing.T) {
	good := `[{"name":"ada","admin":true,"keys":[{"key":"` + newSSHKey(t) + `","label":"laptop"}]}]`
	for name, setup := range map[string]func(dir string) error{
		"directory writable by group": func(dir string) error { return os.Chmod(dir, 0o770) },
		"users.json writable by others": func(dir string) error {
			p := filepath.Join(dir, "users.json")
			if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
				return err
			}
			return os.Chmod(p, 0o602)
		},
		"users.json a symbolic link": func(dir string) error {
			p := filepath.Join(dir, "real.json")
			if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
				return err
			}
			return os.Symlink(p, filepath.Join(dir, "users.json"))
		},
		"users.json a directory": func(dir string) error { return os.Mkdir(filepath.Join(dir, "users.json"), 0o700) },
		"users.json too large": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "users.json"), []byte(`[`+strings.Repeat(" ", maxUsersFile)+`]`), 0o600)
		},
		"users.json an object":     writeUsers(`{"users":[]}`),
		"users.json null":          writeUsers(`null`),
		"users.json trailing data": writeUsers(good + `[]`),
		"an unknown member":        writeUsers(`[{"name":"ada","admin":true,"keys":[],"github":"ada"}]`),
		"a member twice":           writeUsers(`[{"name":"ada","name":"bob","admin":true,"keys":[]}]`),
		"admin missing":            writeUsers(`[{"name":"ada","keys":[]}]`),
		"keys null":                writeUsers(`[{"name":"ada","admin":true,"keys":null}]`),
		"a key without a label":    writeUsers(`[{"name":"ada","admin":true,"keys":[{"key":"x"}]}]`),
		"expires empty":            writeUsers(`[{"name":"ada","admin":true,"keys":[{"key":"x","label":"l","expires":""}]}]`),
		// An explicit null is not an absent expires: never "does not expire".
		"expires null":     writeUsers(strings.Replace(good, `"label":"laptop"`, `"label":"laptop","expires":null`, 1)),
		"expires a number": writeUsers(strings.Replace(good, `"label":"laptop"`, `"label":"laptop","expires":1`, 1)),
		"invalid UTF-8":    writeUsers("[{\"name\":\"ada\xff\",\"admin\":true,\"keys\":[]}]"),
		"users.json a FIFO": func(dir string) error {
			// Opened without blocking, and refused as not a regular file.
			return unix.Mkfifo(filepath.Join(dir, "users.json"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := controlDir(t)
			if err := setup(dir); err != nil {
				t.Fatal(err)
			}
			cd, err := OpenControlDir(dir)
			if err == nil {
				_, err = cd.Users()
				cd.Close()
			}
			if err == nil {
				t.Fatal("read")
			}
		})
	}
	// The good file, as a control, and the same file with an expires.
	for _, content := range []string{good, strings.Replace(good, `"label":"laptop"`, `"label":"laptop","expires":"2027-01-01T00:00:00Z"`, 1)} {
		dir := controlDir(t)
		if err := writeUsers(content)(dir); err != nil {
			t.Fatal(err)
		}
		if u, err := openControl(t, dir).Users(); err != nil || len(u) != 1 {
			t.Fatalf("the good file: %v %v", u, err)
		}
	}
}

// TestControlDirSymlinks checks a control directory reached through a
// symbolic link is refused, with or without a trailing slash, and that a
// real directory's path with a trailing slash is accepted.
func TestControlDirSymlinks(t *testing.T) {
	realDir := controlDir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, link + "/", link + "//", link + "/."} {
		if cd, err := OpenControlDir(p); err == nil {
			cd.Close()
			t.Errorf("%s: opened", p)
		}
	}
	for _, p := range []string{realDir, realDir + "/"} {
		cd, err := OpenControlDir(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		cd.Close()
	}
	if _, err := OpenControlDir(""); err == nil {
		t.Error("an empty path opened")
	}
}

// TestControlDirPinned checks the directory is the one opened: renamed away
// and replaced by another at its path, the server still reads the one it
// opened, never the replacement; and the directory's mode is checked on the
// open directory at every read.
func TestControlDirPinned(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "control")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "disabled"), []byte("srv-pinned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cd := openControl(t, path)
	if d, err := cd.Disabled("srv-pinned"); !d || err != nil {
		t.Fatalf("before: %v %v", d, err)
	}
	if err := os.Rename(path, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "disabled"), []byte("srv-other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := cd.Disabled("srv-pinned"); !d || err != nil {
		t.Fatalf("after the replacement: %v %v, want the opened directory's answer", d, err)
	}
	if d, _ := cd.Disabled("srv-other"); d {
		t.Fatal("the replacement directory was read")
	}
	if err := os.Chmod(filepath.Join(parent, "moved"), 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := cd.Disabled("srv-pinned"); err == nil {
		t.Fatal("the open directory made group-writable was still read")
	}
}

func writeUsers(content string) func(dir string) error {
	return func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "users.json"), []byte(content), 0o600)
	}
}

// TestControlDirCodesAndDisabled checks the line files: a malformed line
// is a failed lookup; missing files hold nothing.
func TestControlDirCodesAndDisabled(t *testing.T) {
	dir := controlDir(t)
	cd := openControl(t, dir)
	if _, found, err := cd.Code("x"); found || err != nil {
		t.Fatalf("no codes file: %v %v", found, err)
	}
	if d, err := cd.Disabled("x"); d || err != nil {
		t.Fatalf("no disabled file: %v %v", d, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codes"), []byte("only-one-field\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cd.Code("x"); err == nil {
		t.Error("a malformed codes line read")
	}
	if err := os.WriteFile(filepath.Join(dir, "disabled"), []byte("a b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cd.Disabled("a"); err == nil {
		t.Error("a malformed disabled line read")
	}
}
