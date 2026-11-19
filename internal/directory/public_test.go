package directory

import (
	"encoding/base64"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// sshAlgorithmNames are the SSH algorithm names in the openssh.com
// namespace: key types and certificate types, never an address.
var sshAlgorithmNames = []string{
	gossh.KeyAlgoSKED25519,
	gossh.KeyAlgoSKECDSA256,
	gossh.CertAlgoRSAv01,
	gossh.CertAlgoECDSA256v01,
	gossh.CertAlgoECDSA384v01,
	gossh.CertAlgoECDSA521v01,
	gossh.CertAlgoED25519v01,
	gossh.CertAlgoSKECDSA256v01,
	gossh.CertAlgoSKED25519v01,
}

// An algorithm name stands in algorithm position: as a whole token, at the
// start of a line or after whitespace, a quote or a backtick, and before
// whitespace, a quote, a backtick or the end of a line.
var sshAlgorithmRx = func() *regexp.Regexp {
	quoted := make([]string, len(sshAlgorithmNames))
	for i, n := range sshAlgorithmNames {
		quoted[i] = regexp.QuoteMeta(n)
	}
	delim := "[\\s\"`]"
	return regexp.MustCompile(`(?m)(^|` + delim + `)(?:` + strings.Join(quoted, "|") + `)(` + delim + `|$)`)
}()

var (
	// A URL's authority: everything after the scheme up to the path; a
	// bracketed IPv6 literal (with an optional port) counts as one.
	urlRx = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://(\[[^\]\s/]*\](?::[0-9]*)?|[^/\s"'<>()\[\]\\` + "`" + `]+)`)
	// An address: local part, @, a domain, dotted or not.
	addressRx = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*)`)
	// A dotted IPv4 literal.
	ipv4Rx = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	// A bare host name under a common or reserved top-level domain, in any
	// case.
	hostRx = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:com|net|org|io|dev|app|ai|cloud|co|me|local|internal|lan|home|test|example|invalid|localhost)\b`)
	// A JWT-shaped string: three base64url parts, the first two decoding to
	// JSON objects (checked after the match).
	jwtRx = regexp.MustCompile(`[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]*`)
)

// allowedHost is a host the contract may name: example.test or a name under
// it, or 127.0.0.1.
func allowedHost(h string) bool {
	h = strings.ToLower(h)
	return h == "127.0.0.1" || h == "example.test" || strings.HasSuffix(h, ".example.test")
}

// decodeJWTPart decodes one base64url part of a JWT to the JSON object it
// holds, or reports that it is not one.
func decodeJWTPart(s string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", false
	}
	text := strings.TrimSpace(string(b))
	return text, strings.HasPrefix(text, "{")
}

// publicProblems lists every host and address in text that the contract may
// not name. A JWT is checked by its decoded header and claims (its
// signature is random bytes, and its base64 hides what it says).
func publicProblems(text string) []string {
	var out []string
	text = jwtRx.ReplaceAllStringFunc(text, func(tok string) string {
		parts := strings.SplitN(tok, ".", 3)
		header, ok1 := decodeJWTPart(parts[0])
		claims, ok2 := decodeJWTPart(parts[1])
		if !ok1 || !ok2 {
			return tok
		}
		for _, p := range publicProblems(header + "\n" + claims) {
			out = append(out, "in a JWT's header or claims, "+p)
		}
		return " "
	})
	for _, m := range urlRx.FindAllStringSubmatch(text, -1) {
		host := m[1]
		if i := strings.LastIndex(host, "@"); i >= 0 {
			out = append(out, "a URL with user information: "+m[0])
			host = host[i+1:]
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		if !allowedHost(host) {
			out = append(out, "a URL's host: "+m[0])
		}
	}
	text = sshAlgorithmRx.ReplaceAllString(text, "$1$2")
	for _, m := range addressRx.FindAllStringSubmatch(text, -1) {
		if strings.ToLower(m[1]) != "example.test" {
			out = append(out, "an address: "+m[0])
		}
	}
	for _, m := range ipv4Rx.FindAllString(text, -1) {
		if m != "127.0.0.1" {
			out = append(out, "an IP address: "+m)
		}
	}
	for _, m := range hostRx.FindAllString(text, -1) {
		if !allowedHost(m) {
			out = append(out, "a host: "+m)
		}
	}
	return out
}

// TestContractIsPublic checks that tests/directory-contract/ names no host
// but *.example.test and 127.0.0.1, and no address but @example.test.
func TestContractIsPublic(t *testing.T) {
	files := 0
	err := filepath.WalkDir(contractRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, problem := range publicProblems(string(b)) {
			t.Errorf("%s names %s", p, problem)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("no files under " + contractRoot)
	}
}

// TestPublicProblems shows the check finds what it is for and passes what
// the contract may name.
func TestPublicProblems(t *testing.T) {
	jwt := func(header, claims string) string {
		enc := base64.RawURLEncoding.EncodeToString
		return enc([]byte(header)) + "." + enc([]byte(claims)) + ".c2lnbmF0dXJl"
	}
	for _, tc := range []struct {
		text string
		bad  bool
	}{
		{"https://accounts.example.test/oauth2/token", false},
		{"http://127.0.0.1:8080/v1", false},
		{"ada@example.test", false},
		{"build-1.example.test", false},
		{"BUILD-1.EXAMPLE.TEST", false},
		{"sk-ssh-ed25519@openssh.com AAAA", false},
		{"ssh-ed25519-cert-v01@openssh.com AAAA", false},
		{`"algorithm": "sk-ecdsa-sha2-nistp256@openssh.com"`, false},
		{"| `sk-ssh-ed25519@openssh.com` |", false},
		{jwt(`{"alg":"RS256"}`, `{"iss":"https://accounts.example.test"}`), false},
		{"https://accounts.example.org/v1", true},
		{"http://10.0.0.7/", true},
		{"https://user@accounts.example.test/", true},
		{"ada@example.com", true},
		{"reach it at 192.168.1.20", true},
		{"a host called build.internal", true},
		{"the box db.example.test.corp.net", true},
		{"the box DB.CORP.NET", true},
		{"http://[fd00::1]/", true},
		{"http://[::1]:8080/v1", true},
		{"owner@privatebox", true},
		{"sk-owner@openssh.com AAAA", true},
		{"mail the owner, ssh-ed25519-cert-v01@openssh.com.", true},
		{"x" + "sk-ssh-ed25519@openssh.com", true},
		{jwt(`{"alg":"RS256"}`, `{"iss":"https://signin.corp.example.org"}`), true},
		{jwt(`{"alg":"RS256","jku":"https://keys.example.net/jwks"}`, `{}`), true},
		{jwt(`{"alg":"RS256"}`, `{"email":"owner@privatebox"}`), true},
	} {
		if got := len(publicProblems(tc.text)) > 0; got != tc.bad {
			t.Errorf("publicProblems(%q) found a problem = %v, want %v", tc.text, got, tc.bad)
		}
	}
}
