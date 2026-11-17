package refserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServedKeyCorpus holds the served-list key check to the contract's key
// corpus, as a service: every refused line is refused, and the canonical
// form of every accepted line is served. The corpus was recorded from the
// service; this check shares no code with shed's client parser.
func TestServedKeyCorpus(t *testing.T) {
	read := func(dir string) map[string][]byte {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(contractDir, "fixtures", "keys", dir))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]byte{}
		for _, e := range entries {
			if name, ok := strings.CutSuffix(e.Name(), ".json"); ok {
				out[name] = readContract(t, "fixtures/keys/"+dir+"/"+e.Name())
			}
		}
		if len(out) == 0 {
			t.Fatalf("no %s entries", dir)
		}
		return out
	}
	for name, raw := range read("refuse") {
		var e struct {
			Line string `json:"line"`
			Code string `json:"code"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("refuse/%s: %v", name, err)
		}
		if _, err := checkServedKey(e.Line); err == nil {
			t.Errorf("refuse/%s (%s): served", name, e.Code)
		}
	}
	for name, raw := range read("accept") {
		var e struct {
			Canonical   string `json:"canonical"`
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("accept/%s: %v", name, err)
		}
		fp, err := checkServedKey(e.Canonical)
		if err != nil {
			t.Errorf("accept/%s: refused: %v", name, err)
			continue
		}
		if fp != e.Fingerprint {
			t.Errorf("accept/%s: fingerprint %s, want %s", name, fp, e.Fingerprint)
		}
	}
}
