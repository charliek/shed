package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const manifestName = "MANIFEST.sha256"

var manifestLine = regexp.MustCompile(`^([0-9a-f]{64})  (\S.*)$`)

// contractFiles hashes every file of v1/ but the manifest, by its path
// under v1/ (forward slashes).
func contractFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(contractDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
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
	if len(files) == 0 {
		t.Fatalf("no files under %s", contractDir)
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
// manifest is listed with its SHA-256, sorted by path, and nothing else is.
// A file added, removed or changed without its manifest line fails here, as
// `sha256sum -c MANIFEST.sha256` would in a copy.
func TestContractManifest(t *testing.T) {
	files := contractFiles(t)
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
	if !strings.HasSuffix(text, "\n") {
		t.Errorf("manifest does not end with a newline")
	}
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
