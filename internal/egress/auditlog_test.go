package egress

import (
	"bufio"
	"os"
	"path/filepath"
	"testing"
)

func recordN(a *AuditLog, shed string, n int) {
	for i := 0; i < n; i++ {
		a.Record(AuditRecord{Shed: shed, Host: "h", Port: 443, Verdict: "deny", Reason: "default-deny"})
	}
}

func TestAuditLog_RecentOrderAndJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := OpenAuditLog(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	a.Record(AuditRecord{Shed: "web", Host: "a.com", Verdict: "allow"})
	a.Record(AuditRecord{Shed: "web", Host: "b.com", Verdict: "deny"})
	a.Record(AuditRecord{Shed: "db", Host: "c.com", Verdict: "deny"})

	all := a.Recent("", 0)
	if len(all) != 3 || all[0].Host != "a.com" || all[2].Host != "c.com" {
		t.Fatalf("Recent(all) = %+v, want a,b,c in order", all)
	}
	web := a.Recent("web", 0)
	if len(web) != 2 || web[0].Host != "a.com" || web[1].Host != "b.com" {
		t.Errorf("Recent(web) = %+v, want a,b", web)
	}

	// JSONL file has one line per record.
	a.Close()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines++
	}
	if lines != 3 {
		t.Errorf("JSONL line count = %d, want 3", lines)
	}
}

func TestAuditLog_RingWrap(t *testing.T) {
	a, err := OpenAuditLog(filepath.Join(t.TempDir(), "a.jsonl"), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	recordN(a, "web", 25) // overflow the 10-slot ring

	got := a.Recent("web", 0)
	if len(got) != 10 {
		t.Errorf("ring retained %d records, want 10 (capacity)", len(got))
	}
}

func TestAuditLog_RecentLimit(t *testing.T) {
	a, err := OpenAuditLog(filepath.Join(t.TempDir(), "a.jsonl"), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	recordN(a, "web", 50)
	if got := a.Recent("web", 5); len(got) != 5 {
		t.Errorf("Recent(limit=5) = %d records, want 5", len(got))
	}
	if got := a.Recent("nope", 5); len(got) != 0 {
		t.Errorf("Recent(unknown shed) = %d, want 0", len(got))
	}
}
