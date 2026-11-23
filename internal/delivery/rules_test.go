package delivery

import (
	"strings"
	"testing"
	"time"
)

func TestCheckPath(t *testing.T) {
	cases := []struct {
		path string
		want Rule
	}{
		{".ssh/id_ed25519", ""},
		{"a", ""},
		{".a/.b/..c", ""},
		{"a/...", ""},
		{strings.Repeat("a", 255), ""},
		{"", RulePathEmpty},
		{strings.Repeat("a", 256), RulePathTooLong},
		{"a\x00b", RulePathNotPrintable},
		{"a\x7fb", RulePathNotPrintable},
		{"a\u0085b", RulePathNotPrintable},
		{"a\u202eb", RulePathNotPrintable},
		{"a\u200bb", RulePathNotPrintable},
		{"a b", ""},
		{`a\b`, RulePathBackslash},
		{"/a", RulePathAbsolute},
		{"/", RulePathAbsolute},
		{"a/", RulePathTrailingSlash},
		{"a//b", RulePathEmptySegment},
		{".", RulePathDotSegment},
		{"./a", RulePathDotSegment},
		{".ssh/./authorized_keys", RulePathDotSegment},
		{"a/.", RulePathDotSegment},
		{"..", RulePathDotDotSegment},
		{"a/../b", RulePathDotDotSegment},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := CheckPath(c.path); got != c.want {
				t.Errorf("CheckPath(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestCheckValue(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  Rule
	}{
		{"plain", "value", ""},
		{"inner spaces and shell characters", `a "b" $c  'd'`, ""},
		{"inner non-ASCII text", "caf\u00e9 \u65e5\u672c", ""},
		{"inner no-break space", "a\u00a0b", RuleValueNotPrintable},
		{"zero width space", "a\u200bb", RuleValueNotPrintable},
		{"right-to-left override", "a\u202eb", RuleValueNotPrintable},
		{"8192 bytes", strings.Repeat("v", 8192), ""},
		{"empty", "", RuleValueEmpty},
		{"8193 bytes", strings.Repeat("v", 8193), RuleValueTooLong},
		{"tab", "a\tb", RuleValueNotPrintable},
		{"trailing newline", "a\n", RuleValueNotPrintable},
		{"nul", "a\x00", RuleValueNotPrintable},
		{"c1", "a\u009fb", RuleValueNotPrintable},
		{"leading space", " a", RuleValueEdgeSpace},
		{"trailing space", "a ", RuleValueEdgeSpace},
		{"trailing no-break space", "a\u00a0", RuleValueNotPrintable},
		{"leading line separator", "\u2028a", RuleValueNotPrintable},
		{"trailing ideographic space", "a\u3000", RuleValueNotPrintable},
		{"only a space", " ", RuleValueEdgeSpace},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CheckValue(c.value); got != c.want {
				t.Errorf("CheckValue = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRefusedVariable(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"PATH", true},
		{"path", true},
		{"Path", true},
		{"LD_PRELOAD", true},
		{"ld_preload", true},
		{"Ld_Library_Path", true},
		{"LD_", true},
		{"SHED_TOKEN", true},
		{"shed_x", true},
		{"http_proxy", true},
		{"BASH_ENV", true},
		{"LDFLAGS", false},
		{"PATHS", false},
		{"SHED", false},
		{"XSHED_A", false},
		{"MY_LD_PATH", false},
		// Only ASCII letters are folded: a long s is not an s.
		{"ſHELL", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RefusedVariable(c.name); got != c.want {
				t.Errorf("RefusedVariable(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestRefusedHomeTarget(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{".ssh/authorized_keys", true},
		{".ssh/authorized_keys2", true},
		{".ssh/known_hosts", true},
		{".config/environment.d", true},
		{".config/environment.d/50-shed-delivery.conf", true},
		{".config/environment.d/a/b", true},
		{".ssh/authorized_keys.bak", false},
		{".ssh/known_hosts2", false},
		{".ssh/id_ed25519", false},
		{".config/environment.d.old/x", false},
		{".config/environmentd", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := RefusedHomeTarget(c.path); got != c.want {
				t.Errorf("RefusedHomeTarget(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

func TestTimes(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"2026-11-23T10:00:00Z", true},
		{"2028-02-29T23:59:59Z", true},
		{"2026-11-23T10:00:00z", false},
		{"2026-11-23t10:00:00Z", false},
		{"2026-11-23T10:00:00+00:00", false},
		{"2026-11-23T10:00:00.0Z", false},
		{"2026-02-30T10:00:00Z", false},
		{"2026-11-23T24:00:00Z", false},
		{"2026-11-23 10:00:00Z", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.s, func(t *testing.T) {
			if got := ValidTime(c.s); got != c.want {
				t.Errorf("ValidTime(%q) = %v, want %v", c.s, got, c.want)
			}
		})
	}
	at := time.Date(2026, 11, 23, 10, 0, 0, 999_000_000, time.FixedZone("x", 3600))
	if got := FormatTime(at); got != "2026-11-23T09:00:00Z" || !ValidTime(got) {
		t.Errorf("FormatTime = %q", got)
	}
}

func TestRuleStatus(t *testing.T) {
	for _, r := range Rules {
		want := 422
		switch r {
		case RuleBodyTooLarge:
			want = 413
		case RuleNotJSON:
			want = 400
		}
		if r.Status() != want {
			t.Errorf("%s: %d, want %d", r, r.Status(), want)
		}
	}
	if len(Rules) != 46 {
		t.Errorf("%d rules", len(Rules))
	}
}

// TestRefusalNamesNoSourceText: a refusal's location never repeats a
// member name the contract does not define, which is the source's own text.
func TestRefusalNamesNoSourceText(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		body string
		want Rule
	}{
		{"unknown at the top", `{"version":1,"id":"` + id + `","items":[],"problems":[],"sk-secret":1}`, RuleUnknownMember},
		{"duplicate unknown", `{"version":1,"id":"` + id + `","items":[],"problems":[],"sk-secret":1,"sk-secret":2}`, RuleDuplicateMember},
		{"null under an unknown", `{"version":1,"id":"` + id + `","items":[],"problems":[],"sk-secret":{"a":null}}`, RuleNullValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, r := ParseAnswer([]byte(c.body), &Request{ID: id})
			if r == nil || r.Rule != c.want {
				t.Fatalf("got %v, want %s", r, c.want)
			}
			if strings.Contains(r.Error(), "secret") {
				t.Fatalf("refusal %q repeats the source's text", r.Error())
			}
		})
	}
}

// TestItemMarshal: an item encodes to its kind's members only, and an
// answer with nil lists encodes them as [].
func TestItemMarshal(t *testing.T) {
	env := Item{Name: "k", Provider: "p", Kind: KindEnv, Stage: 9, Path: "ignored", Variable: "KEY", Value: "v", Fingerprint: "f"}
	b, err := env.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"name":"k","provider":"p","kind":"env","variable":"KEY","value":"v","fingerprint":"f"}`; got != want {
		t.Errorf("env item: %s, want %s", got, want)
	}
	file := Item{Name: "f", Provider: "p", Kind: KindFile, Stage: 1, Base: BaseHome, Path: "a", Mode: "0600", Content: []byte("hi"), Fingerprint: "f", ExpiresAt: "2026-11-23T10:00:00Z"}
	b, _ = file.MarshalJSON()
	if got, want := string(b), `{"name":"f","provider":"p","kind":"file","stage":1,"base":"home","path":"a","mode":"0600","content":"aGk=","fingerprint":"f","expires_at":"2026-11-23T10:00:00Z"}`; got != want {
		t.Errorf("file item: %s, want %s", got, want)
	}
	b, _ = Answer{Version: 1, ID: "x"}.MarshalJSON()
	if got, want := string(b), `{"version":1,"id":"x","items":[],"problems":[]}`; got != want {
		t.Errorf("answer: %s, want %s", got, want)
	}
}

// TestParseRecordStrict: a record names its members exactly; a member in
// another case, or unknown, refuses it, as a duplicate or a null does.
func TestParseRecordStrict(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"none", `{"state":"none"}`, true},
		{"upper-case member", `{"State":"none"}`, false},
		{"unknown member", `{"state":"none","extra":1}`, false},
		{"duplicate", `{"state":"none","state":"none"}`, false},
		{"null", `{"state":null}`, false},
		{"wrong state", `{"state":"delivered"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseRecord([]byte(c.body)); (err == nil) != c.ok {
				t.Errorf("err %v, want ok %v", err, c.ok)
			}
		})
	}
}
