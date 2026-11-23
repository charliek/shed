package delivery

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const testHome = "/home/tester"

func writerItems() []Item {
	return []Item{
		{Name: "key", Provider: "ssh", Kind: KindFile, Stage: 1, Base: BaseHome, Path: ".ssh/id_ed25519", Mode: "0600", Content: []byte("k")},
		{Name: "tok", Provider: "gh", Kind: KindEnv, Variable: "GH_TOKEN", Value: `a "b" $c <d>`},
		{Name: "dotenv", Provider: "app", Kind: KindFile, Stage: 2, Base: BaseProject, Path: ".env", Mode: "0644", Content: []byte("A=1\n")},
		{Name: "late", Provider: "app", Kind: KindFile, Stage: 2, Base: BaseHome, Path: ".config/late", Mode: "0400", Content: []byte{0, 0xff}},
	}
}

// TestWriterInputRoundTrip: what NewWriterInput builds for each stage, and
// that ParseWriterInput reads its Marshal back as it was.
func TestWriterInputRoundTrip(t *testing.T) {
	project := testHome + "/widgets"
	tests := []struct {
		stage      int
		wantFiles  []string
		wantEnv    []string
		wantProjet string
	}{
		{1, []string{"key"}, []string{"tok"}, ""},
		{2, []string{"dotenv", "late"}, nil, project},
	}
	for _, tt := range tests {
		in := NewWriterInput(tt.stage, project, writerItems())
		var files, envs []string
		for _, f := range in.Files {
			files = append(files, f.Name)
		}
		for _, e := range in.Env {
			envs = append(envs, e.Name)
		}
		if !reflect.DeepEqual(files, tt.wantFiles) || !reflect.DeepEqual(envs, tt.wantEnv) || in.ProjectDir != tt.wantProjet {
			t.Errorf("stage %d: files %v env %v project %q", tt.stage, files, envs, in.ProjectDir)
		}
		body, err := in.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte(`\u00`+"3c")) {
			t.Errorf("stage %d: HTML escaping in %s", tt.stage, body)
		}
		got, r := ParseWriterInput(body, testHome)
		if r != nil {
			t.Fatalf("stage %d: %v", tt.stage, r)
		}
		if !reflect.DeepEqual(*got, in) {
			t.Errorf("stage %d: round trip\n got %+v\nwant %+v", tt.stage, *got, in)
		}
	}
}

// TestWriterInputEmptyLists: nil lists are written [] and read back.
func TestWriterInputEmptyLists(t *testing.T) {
	body, err := WriterInput{Version: 1, Stage: 1}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"version":1,"stage":1,"files":[],"env":[]}` {
		t.Errorf("got %s", body)
	}
	if _, r := ParseWriterInput(body, testHome); r != nil {
		t.Error(r)
	}
}

// TestParseWriterInputRefuses: each rule the writer applies to its input.
func TestParseWriterInputRefuses(t *testing.T) {
	file := func(path string) string {
		return `{"name":"f","base":"home","path":"` + path + `","mode":"0600","content":"eA=="}`
	}
	doc := func(stage, files, env string) string {
		return `{"version":1,"stage":` + stage + `,"files":[` + files + `],"env":[` + env + `]}`
	}
	envItem := func(variable, value string) string {
		b, _ := json.Marshal(WriterEnv{Name: "e", Variable: variable, Value: value})
		return string(b)
	}
	big := strings.Repeat("A", (MaxFileBytes/3+1)*4)
	tests := []struct {
		name string
		body string
		home string
		want Rule
	}{
		{"not json", `{`, "", RuleNotJSON},
		{"duplicate member", `{"version":1,"version":1,"stage":1,"files":[],"env":[]}`, "", RuleDuplicateMember},
		{"null", `{"version":1,"stage":1,"files":null,"env":[]}`, "", RuleNullValue},
		{"unknown member", `{"version":1,"stage":1,"files":[],"env":[],"x":1}`, "", RuleUnknownMember},
		{"missing member", `{"version":1,"stage":1,"files":[]}`, "", RuleMissingMember},
		{"wrong version", `{"version":2,"stage":1,"files":[],"env":[]}`, "", RuleUnsupportedVer},
		{"bad stage", doc("0", "", ""), "", RuleBadStage},
		{"env in stage 2", doc("2", "", envItem("A", "b")), "", RuleBadStage},
		{"file member missing", doc("1", `{"name":"f","base":"home","path":"x","mode":"0600"}`, ""), "", RuleMissingMember},
		{"file unknown member", doc("1", `{"name":"f","base":"home","path":"x","mode":"0600","content":"","stage":1}`, ""), "", RuleUnknownMember},
		{"bad name", doc("1", `{"name":"F","base":"home","path":"x","mode":"0600","content":""}`, ""), "", RuleBadName},
		{"bad base", doc("1", `{"name":"f","base":"root","path":"x","mode":"0600","content":""}`, ""), "", RuleBadBase},
		{"project base in stage 1", doc("1", `{"name":"f","base":"project","path":"x","mode":"0600","content":""}`, ""), "", RuleProjectBaseStage},
		{"project base with no project directory", doc("2", `{"name":"f","base":"project","path":"x","mode":"0600","content":""}`, ""), "", RuleBadBase},
		{"dot-dot", doc("1", file("a/../b"), ""), "", RulePathDotDotSegment},
		{"dot", doc("1", file("./b"), ""), "", RulePathDotSegment},
		{"absolute", doc("1", file("/etc/x"), ""), "", RulePathAbsolute},
		{"empty segment", doc("1", file("a//b"), ""), "", RulePathEmptySegment},
		{"backslash", doc("1", file(`a\\b`), ""), "", RulePathBackslash},
		{"refused target", doc("1", file(".ssh/known_hosts"), ""), "", RuleRefusedTarget},
		{"refused env tree", doc("1", file(".config/environment.d/50-shed-delivery.conf"), ""), "", RuleRefusedTarget},
		{"bad mode", doc("1", `{"name":"f","base":"home","path":"x","mode":"0700","content":""}`, ""), "", RuleBadMode},
		{"bad base64", doc("1", `{"name":"f","base":"home","path":"x","mode":"0600","content":"eA"}`, ""), "", RuleBadBase64},
		{"file too large", doc("1", `{"name":"f","base":"home","path":"x","mode":"0600","content":"`+big+`"}`, ""), "", RuleFileTooLarge},
		{"bad variable", doc("1", "", envItem("path", "x")), "", RuleBadVariable},
		{"refused variable", doc("1", "", envItem("SHED_X", "x")), "", RuleRefusedVariable},
		{"refused proxy variable", doc("1", "", envItem("HTTPS_PROXY", "x")), "", RuleRefusedVariable},
		{"empty value", doc("1", "", envItem("A", "")), "", RuleValueEmpty},
		{"edge space", doc("1", "", envItem("A", "x ")), "", RuleValueEdgeSpace},
		{"control in value", doc("1", "", envItem("A", "x\ty")), "", RuleValueNotPrintable},
		{"duplicate name", doc("1", file("a")+`,`+strings.Replace(file("b"), `"name":"f"`, `"name":"f"`, 1), ""), "", RuleDuplicateName},
		{"duplicate target", doc("1", file("a")+`,`+strings.Replace(file("a"), `"name":"f"`, `"name":"g"`, 1), ""), "", RuleDuplicateTarget},
		{"nested target", doc("1", file("a")+`,`+strings.Replace(file("a/b"), `"name":"f"`, `"name":"g"`, 1), ""), "", RuleNestedTarget},
		{"duplicate variable", doc("1", "", envItem("A", "1")+`,`+strings.Replace(envItem("A", "2"), `"name":"e"`, `"name":"g"`, 1)), "", RuleDuplicateTarget},
		{"oversize", strings.Repeat(" ", MaxWriterInputBytes+1), "", RuleBodyTooLarge},
		{"bad home", doc("1", "", ""), "relative/home", ruleRequestValue},
		{"root home", doc("1", "", ""), "/", ruleRequestValue},
	}
	projectDoc := func(dir, path string) string {
		return `{"version":1,"stage":2,"project_dir":"` + dir + `","files":[{"name":"p","base":"project","path":"` + path + `","mode":"0600","content":""}],"env":[]}`
	}
	tests = append(tests, []struct {
		name string
		body string
		home string
		want Rule
	}{
		{"project directory outside home", projectDoc("/srv/widgets", "x"), "", ruleRequestValue},
		{"project directory is home", projectDoc(testHome, "x"), "", ruleRequestValue},
		{"project directory beside home", projectDoc(testHome+"x/w", "x"), "", ruleRequestValue},
		{"project directory not canonical", projectDoc(testHome+"/w/../../etc", "x"), "", ruleRequestValue},
		{"project directory relative", projectDoc("w", "x"), "", ruleRequestValue},
		{"project directory with no project file", `{"version":1,"stage":2,"project_dir":"` + testHome + `/w","files":[],"env":[]}`, "", ruleRequestValue},
		{"project file landing on a refused target", projectDoc(testHome+"/.ssh", "authorized_keys"), "", RuleRefusedTarget},
		{"project file landing in the env tree", projectDoc(testHome+"/.config", "environment.d/x.conf"), "", RuleRefusedTarget},
	}...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := tt.home
			if home == "" {
				home = testHome
			}
			got, r := ParseWriterInput([]byte(tt.body), home)
			if r == nil {
				t.Fatalf("accepted: %+v", got)
			}
			if r.Rule != tt.want {
				t.Errorf("rule %s at %q, want %s", r.Rule, r.Where, tt.want)
			}
		})
	}
}

// TestParseWriterInputAccepts: a project file that does not land on a
// refused target is accepted, as the contract says.
func TestParseWriterInputAccepts(t *testing.T) {
	body := `{"version":1,"stage":2,"project_dir":"` + testHome + `/widgets","files":[{"name":"p","base":"project","path":".ssh/authorized_keys","mode":"0600","content":""}],"env":[]}`
	if _, r := ParseWriterInput([]byte(body), testHome); r != nil {
		t.Fatal(r)
	}
}

// TestWriterLineResults: the writer's line results are its own seven and
// not_written, and nothing else.
func TestWriterLineResults(t *testing.T) {
	want := []ItemResult{ResultOK, ResultIsDirectory, ResultSymlink, ResultNotDirectory, ResultPermission, ResultOtherFilesystem, ResultIO, ResultNotWritten}
	if !reflect.DeepEqual(WriterLineResults, want) {
		t.Errorf("got %v", WriterLineResults)
	}
	if EnvFile != ".config/environment.d/50-shed-delivery.conf" {
		t.Errorf("EnvFile = %q", EnvFile)
	}
}

// envItems is n env items whose env file is exactly size bytes: variables
// V00, V01, ... and values of 'v' filling the rest.
func envItems(n, size int) []Item {
	items := make([]Item, n)
	left := size
	for i := range items {
		v := "V" + string(rune('0'+i/10)) + string(rune('0'+i%10))
		line := left / (n - i)
		items[i] = Item{Name: "e" + strings.ToLower(v), Provider: "p", Kind: KindEnv, Variable: v, Value: strings.Repeat("v", line-len(v)-2), Fingerprint: "f"}
		left -= line
	}
	return items
}

// TestEnvTooLarge: an answer whose env file would be exactly
// MaxEnvFileBytes is accepted, one byte more is refused env_too_large.
func TestEnvTooLarge(t *testing.T) {
	req := &Request{ID: "0123456789abcdef0123456789abcdef"}
	for _, tt := range []struct {
		size int
		want Rule
	}{
		{MaxEnvFileBytes, ""},
		{MaxEnvFileBytes + 1, RuleEnvTooLarge},
	} {
		items := envItems(32, tt.size)
		if got := EnvFileSize(items); got != tt.size {
			t.Fatalf("built %d bytes, want %d", got, tt.size)
		}
		body, err := json.Marshal(Answer{Version: 1, ID: req.ID, Items: items})
		if err != nil {
			t.Fatal(err)
		}
		_, r := ParseAnswer(body, req)
		switch {
		case tt.want == "" && r != nil:
			t.Errorf("size %d refused: %v", tt.size, r)
		case tt.want != "" && (r == nil || r.Rule != tt.want):
			t.Errorf("size %d: %v, want %s", tt.size, r, tt.want)
		}
	}
}
