package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The compute contract's folder, from this package's directory.
const (
	contractRoot = "../../../../tests/compute-contract"
	contractDir  = contractRoot + "/v1"
	manifestName = "MANIFEST.sha256"
)

func updating() bool { return os.Getenv("UPDATE_CONTRACT") == "1" }

func readContract(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func contractFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(contractDir, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			out = append(out, dir+"/"+e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatalf("no .json files in %s", dir)
	}
	sort.Strings(out)
	return out
}

// decodeClosed decodes a fixture into v, refusing any member v does not
// have: the fixture languages are closed.
func decodeClosed(t *testing.T, rel string, raw []byte, v any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
}

var manifestLine = regexp.MustCompile(`^([0-9a-f]{64})  (\S.*)$`)

// hashFiles hashes every file of v1/ but the manifest, by its path under
// v1/.
func hashFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(contractDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(contractDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == manifestName {
			return nil
		}
		if !d.Type().IsRegular() {
			t.Errorf("%s is not a regular file", rel)
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		files[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", contractDir, err)
	}
	if len(files) < 100 {
		t.Fatalf("only %d files under %s", len(files), contractDir)
	}
	return files
}

func renderManifest(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(files[p] + "  " + p + "\n")
	}
	return b.String()
}

// TestContractManifest holds v1/ to MANIFEST.sha256: every file but the
// manifest is listed with its SHA-256, sorted by path, and nothing else.
// UPDATE_CONTRACT=1 rewrites it.
func TestContractManifest(t *testing.T) {
	files := hashFiles(t)
	path := filepath.Join(contractDir, manifestName)
	if updating() {
		if err := os.WriteFile(path, []byte(renderManifest(files)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("UPDATE_CONTRACT: wrote %s (%d files)", manifestName, len(files))
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	text := string(raw)
	listed := map[string]string{}
	var order []string
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		m := manifestLine.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("manifest line %d is not `<sha256>  <path>`: %q", i+1, line)
			continue
		}
		if _, dup := listed[m[2]]; dup {
			t.Errorf("manifest lists %s twice", m[2])
		}
		listed[m[2]] = m[1]
		order = append(order, m[2])
	}
	if !sort.StringsAreSorted(order) {
		t.Errorf("manifest is not sorted by path")
	}
	for p, sum := range files {
		want, ok := listed[p]
		switch {
		case !ok:
			t.Errorf("%s is not in the manifest (a file the manifest does not list is not part of the contract)", p)
		case want != sum:
			t.Errorf("%s changed: manifest %s, file %s", p, want, sum)
		}
	}
	for p := range listed {
		if _, ok := files[p]; !ok {
			t.Errorf("the manifest lists %s, which is missing", p)
		}
	}
	if text != renderManifest(files) && !t.Failed() {
		t.Errorf("manifest is not in its canonical form; regenerate it with UPDATE_CONTRACT=1")
	}
}

var (
	urlRx     = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://(\[[^\]\s/]*\](?::[0-9]*)?|[^/\s"'<>()\[\]\\` + "`" + `]+)`)
	addressRx = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*)`)
	ipv4Rx    = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	hostRx    = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:com|net|org|io|dev|app|ai|cloud|co|me|local|internal|lan|home|test|example|invalid|localhost)\b`)
)

func allowedHost(h string) bool {
	h = strings.ToLower(h)
	return h == "127.0.0.1" || h == "example.test" || strings.HasSuffix(h, ".example.test")
}

// documentation are the IPv4 ranges set aside for documentation (RFC
// 5737), which name no host.
var documentation = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
}

// allowedIPv4 is an IPv4 literal the contract may name: loopback, a
// documentation address, or one of the refused classes the validation
// fixtures show (link-local, unspecified, multicast), none of which names
// anyone's host.
func allowedIPv4(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return false
	}
	for _, p := range documentation {
		if p.Contains(a) {
			return true
		}
	}
	return a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast()
}

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
		if !allowedHost(host) && !allowedIPv4(host) {
			out = append(out, "URL host "+m[1])
		}
	}
	for _, m := range addressRx.FindAllStringSubmatch(text, -1) {
		if !allowedHost(m[1]) {
			out = append(out, "address "+m[0])
		}
	}
	for _, m := range ipv4Rx.FindAllString(text, -1) {
		if !allowedIPv4(m) {
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

// TestContractNamesNoHost checks the folder (README and v1/) names no host
// or address but example.test, loopback, the documentation ranges and the
// refused classes' literals.
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
	if n < 100 {
		t.Fatalf("only %d files read", n)
	}
}

func TestPublicProblemsCatches(t *testing.T) {
	for text, want := range map[string]bool{
		"https://accounts.example.test/v1": false,
		"203.0.113.7":                      false,
		"169.254.169.254":                  false,
		"ada@example.test":                 false,
		"10.0.0.1":                         true,
		"8.8.8.8":                          true,
		"https://api.example.com/v1":       true,
		"host.internal":                    true,
	} {
		if got := len(publicProblems(text)) > 0; got != want {
			t.Errorf("%q: flagged %v, want %v", text, got, want)
		}
	}
}
