package delivery

import "testing"

// A request built in code, not decoded, must not carry a project
// directory ParseAnswer cannot place lexically.
func TestParseAnswerRefusesANonCanonicalProjectDir(t *testing.T) {
	body := []byte(`{"version":1,"id":"0123456789abcdef0123456789abcdef","items":[],"problems":[]}`)
	for _, dir := range []string{"/home/shed/", "/home/shed//x", "home/shed/x", "/home/shed/./x", "/home/shed/../x"} {
		t.Run(dir, func(t *testing.T) {
			req := &Request{ID: "0123456789abcdef0123456789abcdef"}
			req.Shed.ProjectDir = dir
			if _, r := ParseAnswer(body, req); r == nil || r.Rule != ruleRequestValue {
				t.Fatalf("ParseAnswer with project_dir %q: refusal %v, want %s", dir, r, ruleRequestValue)
			}
		})
	}
	req := &Request{ID: "0123456789abcdef0123456789abcdef"}
	req.Shed.ProjectDir = "/home/shed/widgets"
	if _, r := ParseAnswer(body, req); r != nil {
		t.Fatalf("canonical project_dir refused: %v", r)
	}
}
