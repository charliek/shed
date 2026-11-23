package vmutil

import (
	"errors"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/provision"
)

// A config that does not parse is reported by a fixed text and, when the
// parser gives one, its line (and column): numbers, never a value, a field
// name or the parser's words.
func TestConfigParseErrorKeepsOnlyTheLocation(t *testing.T) {
	for _, tc := range []struct {
		content, want string
	}{
		{"timeout: SeKr1t\n", "did not load (it does not parse), at line 1"},
		{"hooks:\n  install: x\n  SeKr1t\n", "did not load (it does not parse), at line 3"},
		{"\tSeKr1t", "did not load (it does not parse)"},
	} {
		_, perr := provision.ParseConfigContent([]byte(tc.content))
		if perr == nil {
			t.Fatalf("%q parsed", tc.content)
		}
		err := configParseError(perr)
		if !errors.Is(err, ErrProvisionConfigInvalid) || !strings.HasSuffix(err.Error(), tc.want) {
			t.Errorf("%q: %v, want ending %q", tc.content, err, tc.want)
		}
		for _, leak := range []string{"SeKr1t", "timeout", "install", "unmarshal", "expected"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%q: %v carries %q", tc.content, err, leak)
			}
		}
	}
	if err := configParseError(errors.New("yaml: line 99999999999: x")); err != ErrProvisionConfigInvalid {
		t.Errorf("an out-of-range line: %v", err)
	}
}
