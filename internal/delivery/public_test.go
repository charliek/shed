package delivery

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// A URL's authority: everything after the scheme up to the path.
	urlRx = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://(\[[^\]\s/]*\](?::[0-9]*)?|[^/\s"'<>()\[\]\\` + "`" + `]+)`)
	// An address: local part, @, a domain, dotted or not.
	addressRx = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*)`)
	// A dotted IPv4 literal.
	ipv4Rx = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	// A bare host name under a common or reserved top-level domain.
	hostRx = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:com|net|org|io|dev|app|ai|cloud|co|me|local|internal|lan|home|test|example|invalid|localhost)\b`)
)

// allowedHost is a host the contract may name: example.test or a name under
// it, or 127.0.0.1.
func allowedHost(h string) bool {
	h = strings.ToLower(h)
	return h == "127.0.0.1" || h == "example.test" || strings.HasSuffix(h, ".example.test")
}

// publicProblems lists every host and address in text that the contract
// may not name.
func publicProblems(text string) []string {
	var out []string
	for _, m := range urlRx.FindAllStringSubmatch(text, -1) {
		host := m[1]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if i := strings.LastIndex(host, ":"); i >= 0 && !strings.HasSuffix(host, "]") {
			host = host[:i]
		}
		if !allowedHost(host) {
			out = append(out, "URL host "+m[1])
		}
	}
	for _, m := range addressRx.FindAllStringSubmatch(text, -1) {
		if !allowedHost(m[1]) {
			out = append(out, "address "+m[0])
		}
	}
	for _, m := range ipv4Rx.FindAllString(text, -1) {
		if m != "127.0.0.1" {
			out = append(out, "address "+m)
		}
	}
	for _, m := range hostRx.FindAllString(text, -1) {
		if !allowedHost(m) {
			out = append(out, "host "+m)
		}
	}
	return out
}

// TestContractNamesNoHost checks the folder (README and v2/) names no host
// or address outside example.test and 127.0.0.1.
func TestContractNamesNoHost(t *testing.T) {
	n := 0
	err := filepath.WalkDir(contractRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n++
		for _, prob := range publicProblems(string(b)) {
			t.Errorf("%s: %s", p, prob)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 10 {
		t.Fatalf("only %d files read", n)
	}
}

// TestPublicProblemsCatches is the check's control: it flags what it must.
func TestPublicProblemsCatches(t *testing.T) {
	cases := []struct {
		text string
		bad  bool
	}{
		{"git@git.example.test:example/widgets.git", false},
		{"https://127.0.0.1:18443/api", false},
		{"ada@example.test", false},
		{"https://git.example.com/x", true},
		{"git@github.com:a/b.git", true},
		{"10.0.0.1", true},
		{"host.internal", true},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := len(publicProblems(c.text)) > 0; got != c.bad {
				t.Errorf("flagged %v, want %v", got, c.bad)
			}
		})
	}
}
