package delivery

import (
	"regexp"
	"strings"
	"time"
	"unicode"
)

var (
	// nameRe is an item's name and a provider's name.
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// variableRe is an env item's variable.
	variableRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	// fingerprintRe is an item's fingerprint.
	fingerprintRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// idRe is a request's id.
	idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// timeRe is the one form of a time: RFC 3339, UTC, Z, whole seconds.
	timeRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
)

// ValidName reports whether s is a valid item or provider name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidID reports whether s is a valid request id: 32 lowercase hex digits.
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidTime reports whether s is a time in the contract's one form:
// RFC 3339 in UTC, written with Z and whole seconds, naming a real instant.
func ValidTime(s string) bool {
	if !timeRe.MatchString(s) {
		return false
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// FormatTime writes t in the contract's one form.
func FormatTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
}

// ParseTime reads a time in the contract's one form.
func ParseTime(s string) (time.Time, bool) {
	if !ValidTime(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// isControl reports whether r is a control character: U+0000 to U+001F and
// U+007F to U+009F.
func isControl(r rune) bool { return unicode.IsControl(r) }

// hasControl reports whether s holds a control character.
func hasControl(s string) bool { return strings.IndexFunc(s, isControl) >= 0 }

// Printable reports whether every character of s is printable: a letter,
// mark, number, punctuation or symbol (Unicode general categories L, M, N,
// P and S), or U+0020 SPACE; Go's unicode.IsPrint. Control, format
// (U+200B, U+202E), private-use, surrogate and unassigned code points are
// not printable, nor is any white space but U+0020.
func Printable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// refusedVariables are the env variables no item may set.
var refusedVariables = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "IFS", "ENV", "BASH_ENV",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
}

// refusedVariablePrefixes are the prefixes no item's variable may start
// with.
var refusedVariablePrefixes = []string{"LD_", "SHED_"}

// RefusedVariable reports whether name is a variable the contract refuses,
// compared without regard to ASCII case (path is PATH, ld_preload starts
// LD_). An answer's variable is upper case by its alphabet already; the
// comparison without case is for a reader of a file a person wrote.
func RefusedVariable(name string) bool {
	up := asciiUpper(name)
	for _, v := range refusedVariables {
		if up == v {
			return true
		}
	}
	for _, p := range refusedVariablePrefixes {
		if strings.HasPrefix(up, p) {
			return true
		}
	}
	return false
}

// asciiUpper upper-cases ASCII letters only.
func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// refusedHomeFiles are the paths under home no file item may name.
var refusedHomeFiles = []string{".ssh/authorized_keys", ".ssh/authorized_keys2", ".ssh/known_hosts"}

// refusedHomeTree is the directory under home that no file item may name,
// nor anything under it: the guest's user environment directory.
const refusedHomeTree = ".config/environment.d"

// RefusedHomeTarget reports whether path (canonical) is a target under home
// the contract refuses.
func RefusedHomeTarget(path string) bool {
	for _, p := range refusedHomeFiles {
		if path == p {
			return true
		}
	}
	return path == refusedHomeTree || strings.HasPrefix(path, refusedHomeTree+"/")
}

// CheckPath applies the path rules, in order, and returns the rule path
// breaks, or "" for a relative, canonical path.
func CheckPath(path string) Rule {
	switch {
	case path == "":
		return RulePathEmpty
	case len(path) > MaxPathBytes:
		return RulePathTooLong
	case !Printable(path):
		return RulePathNotPrintable
	case strings.ContainsRune(path, '\\'):
		return RulePathBackslash
	case strings.HasPrefix(path, "/"):
		return RulePathAbsolute
	case strings.HasSuffix(path, "/"):
		return RulePathTrailingSlash
	}
	for _, seg := range strings.Split(path, "/") {
		switch seg {
		case "":
			return RulePathEmptySegment
		case ".":
			return RulePathDotSegment
		case "..":
			return RulePathDotDotSegment
		}
	}
	return ""
}

// CheckValue applies the env value rules, in order, and returns the rule
// value breaks, or "".
func CheckValue(value string) Rule {
	switch {
	case value == "":
		return RuleValueEmpty
	case len(value) > MaxValueBytes:
		return RuleValueTooLong
	case !Printable(value):
		return RuleValueNotPrintable
	case value[0] == ' ' || value[len(value)-1] == ' ':
		return RuleValueEdgeSpace
	}
	return ""
}
