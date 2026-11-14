// Package redact removes credentials from text before it is written to a log,
// an audit record or an error message. It is a leaf package (standard library
// only) so both shed-server and the in-guest shed-agent can import it.
//
// The one rule: in any URL-shaped token, scheme://userinfo@host..., the
// userinfo is replaced with Marker. Everything else is copied byte for byte.
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

// shellApostrophe is an apostrophe as it appears inside a single-quoted
// shell word.
const shellApostrophe = `'\''`

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
		for end < len(s) {
			// An apostrophe inside a single-quoted shell word is written
			// '\'' (close the quote, an escaped quote, open it again).
			// That is how the shed CLI sends an argv element holding one,
			// so the sequence is part of the token, not its end: a
			// password with an apostrophe in it must not stop the scan
			// short of the '@'.
			if strings.HasPrefix(s[end:], shellApostrophe) {
				end += len(shellApostrophe)
				continue
			}
			if endsAuthority(s[end]) {
				break
			}
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
func hasScheme(s string, i int) bool {
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

func isAlpha(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func isSchemeByte(c byte) bool {
	return isAlpha(c) || (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
}

// endsAuthority reports whether c ends a URL's authority: the RFC 3986
// delimiters '/', '?' and '#', plus whitespace, control bytes and the
// characters that cannot appear in a URL and so mark where a token embedded in
// a larger string stops. A single quote does not end it (a userinfo may hold
// one); a quoted 'https://u:p@host' still redacts because the userinfo is
// everything before the last '@'. A backslash ends it except in the shell's
// spelling of an apostrophe, which String steps over before asking.
func endsAuthority(c byte) bool {
	switch c {
	case '/', '?', '#', '\\', '"', '`', '<', '>', '{', '}', '|', '^', 0x7f:
		return true
	}
	return c <= ' '
}
