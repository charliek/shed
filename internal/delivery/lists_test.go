package delivery

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// contractTable returns the rows of the first table after the heading
// "### <heading>" in CONTRACT.md, each as its cells (trimmed), header and
// separator rows left out.
func contractTable(t *testing.T, heading string) [][]string {
	t.Helper()
	lines := strings.Split(string(readContractFile(t, "CONTRACT.md")), "\n")
	start := slices.Index(lines, "### "+heading)
	if start < 0 {
		t.Fatalf("CONTRACT.md has no heading %q", heading)
	}
	var rows [][]string
	inTable := false
	for _, l := range lines[start+1:] {
		if strings.HasPrefix(l, "#") {
			break
		}
		if !strings.HasPrefix(l, "|") {
			if inTable {
				break
			}
			continue
		}
		inTable = true
		cells := strings.Split(strings.Trim(l, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, cells)
	}
	if len(rows) < 3 {
		t.Fatalf("no table under %q", heading)
	}
	return rows[2:]
}

var codeRx = regexp.MustCompile("`([a-z0-9_]+)`")

// firstColumn is each row's first cell, a name in backticks.
func firstColumn(t *testing.T, heading string) []string {
	t.Helper()
	var out []string
	for _, row := range contractTable(t, heading) {
		m := codeRx.FindStringSubmatch(row[0])
		if m == nil || m[0] != row[0] {
			t.Fatalf("%s: first cell %q is not one name in backticks", heading, row[0])
		}
		out = append(out, m[1])
	}
	return out
}

func strs[T ~string](list []T) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = string(v)
	}
	return out
}

// TestContractLists holds the closed lists CONTRACT.md gives to the ones
// this package knows, in the same order: rule names (with their statuses),
// problem codes, error codes, delivery states, step states, reasons (with
// the steps each skips and fails), outcomes, item results and record rule
// names.
func TestContractLists(t *testing.T) {
	cases := []struct {
		heading string
		want    []string
	}{
		{"Rule names", strs(Rules)},
		{"Problem codes", ProblemCodes},
		{"Error codes", []string{CodeUnknownRequest, CodeAlreadyAnswered, CodeDeadlinePassed, CodeNotStreamOwner, CodeAnswerRefused}},
		{"Delivery states", strs(States)},
		{"Steps and step states", strs(StepStates)},
		{"Reasons", strs(StepReasons)},
		{"Outcomes", strs(Outcomes)},
		{"Item results", strs(ItemResults)},
		{"Record rule names", strs(RecordRules)},
	}
	for _, c := range cases {
		t.Run(c.heading, func(t *testing.T) {
			if got := firstColumn(t, c.heading); !slices.Equal(got, c.want) {
				t.Errorf("CONTRACT.md lists\n%v\nthe code knows\n%v", got, c.want)
			}
		})
	}
}

// TestContractRuleStatuses checks each rule's status in CONTRACT.md is the
// one the transport answers with.
func TestContractRuleStatuses(t *testing.T) {
	for _, row := range contractTable(t, "Rule names") {
		rule := Rule(strings.Trim(row[0], "`"))
		status, err := strconv.Atoi(row[1])
		if err != nil || status != rule.Status() {
			t.Errorf("%s: CONTRACT.md says %q, the code %d", rule, row[1], rule.Status())
		}
	}
}

// TestContractReasonSteps checks the steps CONTRACT.md says each reason
// skips and fails are the ones a record may carry it on.
func TestContractReasonSteps(t *testing.T) {
	stepsIn := func(cell string) []string {
		if strings.HasPrefix(cell, "every step") {
			return StepNames
		}
		var out []string
		for _, m := range codeRx.FindAllStringSubmatch(strings.SplitN(cell, ":", 2)[0], -1) {
			out = append(out, m[1])
		}
		return out
	}
	for _, row := range contractTable(t, "Reasons") {
		reason := StepReason(strings.Trim(row[0], "`"))
		for _, step := range StepNames {
			if got, want := slices.Contains(stepsIn(row[1]), step), slices.Contains(skipReasons[step], reason); got != want {
				t.Errorf("%s skips %s: CONTRACT.md %v, the code %v", reason, step, got, want)
			}
			if got, want := slices.Contains(stepsIn(row[2]), step), slices.Contains(failReasons[step], reason); got != want {
				t.Errorf("%s fails %s: CONTRACT.md %v, the code %v", reason, step, got, want)
			}
		}
	}
}
