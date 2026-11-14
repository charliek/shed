// Package redact removes credentials from text before it is written to a log,
// an audit record or an error message. It is a leaf package (standard library
// only) so both shed-server and the in-guest shed-agent can import it.
//
// The one rule: in any URL-shaped token, scheme://userinfo@host..., the
// userinfo is replaced with Marker. Everything else is copied byte for byte.
// The userinfo is everything from the "://" to the last '@' before the next
// '/', '?', '#', white space or control byte. A credential anywhere else (a
// query parameter, a header, an environment variable) is not recognised.
// A clone URL such as https://user:token@host/owner/repo.git becomes
// https://REDACTED@host/owner/repo.git wherever it appears: as a whole
// argument, or inside a larger one such as a bash -c script. An SCP-like git
// remote (git@host:owner/repo.git) has no scheme and is left alone.
//
// Redaction never changes what is executed: callers redact a copy for the log
// and run the original.
package redact

import (
	"strings"
)

// Marker replaces the userinfo of a URL.
const Marker = "REDACTED"

// String returns s with the userinfo of every URL-shaped token replaced by
// Marker. It runs in time linear in len(s), never panics, and returns s
// itself (no allocation) when there is nothing to redact.
func String(s string) string {
	var b strings.Builder
	last := 0 // s[:last] is already written to b (or needs no change)
	for from := 0; ; {
		k := strings.Index(s[from:], "://")
		if k < 0 {
			break
		}
		i := from + k
		from = i + 3
		if !hasScheme(s, i) {
			continue
		}
		start := i + 3
		end := start
		for end < len(s) && !endsAuthority(s[end]) {
			end++
		}
		at := strings.LastIndexByte(s[start:end], '@')
		// An empty userinfo ("https://@host") carries nothing to hide.
		if at <= 0 || start < last {
			continue
		}
		if b.Len() == 0 && last == 0 {
			b.Grow(len(s))
		}
		b.WriteString(s[last:start])
		b.WriteString(Marker)
		last = start + at
		// The authority holds no "://" (it stops at '/'), so the next match
		// starts after it.
		from = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// Args returns argv with String applied to each element. The result is a new
// slice when anything changed and argv itself otherwise, so a caller may log
// it but must never execute it in place of argv.
func Args(argv []string) []string {
	var out []string
	for i, a := range argv {
		r := String(a)
		if out == nil {
			if r == a {
				continue
			}
			out = make([]string, len(argv))
			copy(out, argv[:i])
		}
		out[i] = r
	}
	if out == nil {
		return argv
	}
	return out
}

// hasScheme reports whether the "://" at s[i:] is preceded by a URI scheme:
// a letter followed by letters, digits, '+', '-' or '.'.
//
// A shell can glue the scheme to the rest from pieces: "https"://u:p@h,
// https\://u:p@h, ${S}://u:p@h and $(cmd)://u:p@h all run with an ordinary
// URL. So the quotes, backslashes and closing brackets that may stand
// between such a piece and the "://" are stepped over first.
func hasScheme(s string, i int) bool {
	for i > 0 && isShellJoin(s[i-1]) {
		i--
	}
	j := i
	for j > 0 && isSchemeByte(s[j-1]) {
		j--
	}
	if j == i {
		return false
	}
	// The scheme begins at the first letter of that run; "1+https://" still
	// names https, so skip leading non-letters rather than reject.
	for j < i && !isAlpha(s[j]) {
		j++
	}
	return j < i
}

func isShellJoin(c byte) bool {
	switch c {
	case '"', '\'', '\\', '}', ')':
		return true
	}
	return false
}

func isAlpha(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func isSchemeByte(c byte) bool {
	return isAlpha(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
}

// endsAuthority reports whether c ends the text a URL's userinfo is looked
// for in: the RFC 3986 delimiters '/', '?' and '#', white space and control
// bytes. Nothing else ends it. Quotes, backslashes, braces and the other
// bytes a URL may not hold do appear in what people type as a password
// (pa\$word in a shell command, a raw '"' in an argv element), and stopping
// at one of them would leave the rest of the password, up to the '@', in the
// log. The cost is on the other side: in text with no white space, such as
// compact JSON, an '@' after the URL's closing quote is taken for the end of
// its userinfo and what stands between is redacted with it. Hiding too much
// of a log line is the cheaper mistake.
func endsAuthority(c byte) bool {
	switch c {
	case '/', '?', '#', 0x7f:
		return true
	}
	return c <= ' '
}
