package redact

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestString(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "git clone", "git clone"},
		{"user and password", "https://user:token@github.com/o/r.git", "https://REDACTED@github.com/o/r.git"},
		{"token as user", "https://ghp_abc123@github.com/o/r.git", "https://REDACTED@github.com/o/r.git"},
		{"x-access-token", "https://x-access-token:ghs_xyz@github.com/o/r", "https://REDACTED@github.com/o/r"},
		{"no userinfo", "https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"at sign in path", "https://registry.example/@scope/pkg", "https://registry.example/@scope/pkg"},
		{"at sign in query", "https://h/x?email=a@b", "https://h/x?email=a@b"},
		{"at sign in fragment", "https://h#a@b", "https://h#a@b"},
		{"no path", "https://u:p@host", "https://REDACTED@host"},
		{"port", "https://u:p@host:8443/r", "https://REDACTED@host:8443/r"},
		{"ipv6 host", "https://u:p@[::1]:8443/r", "https://REDACTED@[::1]:8443/r"},
		{"several at signs", "https://u@x:p@w@host/r", "https://REDACTED@host/r"},
		{"percent encoded", "https://u%40x:p%2Fq@host/r", "https://REDACTED@host/r"},
		{"empty userinfo", "https://@host/r", "https://@host/r"},
		{"ssh scheme", "ssh://git@github.com/o/r.git", "ssh://REDACTED@github.com/o/r.git"},
		{"git+ssh scheme", "git+ssh://u:p@h/r", "git+ssh://REDACTED@h/r"},
		{"uppercase scheme", "HTTPS://u:p@h/r", "HTTPS://REDACTED@h/r"},
		{"scp-like remote", "git@github.com:o/r.git", "git@github.com:o/r.git"},
		{"scp-like with user and colon", "user:pass@host:o/r.git", "user:pass@host:o/r.git"},
		{"email", "mail me at a@b.example", "mail me at a@b.example"},
		{"no scheme", "://u:p@h", "://u:p@h"},
		{"digit-only scheme", "123://u:p@h", "123://u:p@h"},
		{"assignment prefix", "URL=https://u:p@h/r", "URL=https://REDACTED@h/r"},
		{"embedded in bash -c", "set -e; git clone 'https://u:p@h/o/r.git' r && cd r",
			"set -e; git clone 'https://REDACTED@h/o/r.git' r && cd r"},
		{"double-quoted", `git clone "https://u:p@h"`, `git clone "https://REDACTED@h"`},
		{"single-quoted no path", "git clone 'https://u:p@h'", "git clone 'https://REDACTED@h'"},
		{"two urls", "a https://u:p@h1/x b http://v:q@h2/y c", "a https://REDACTED@h1/x b http://REDACTED@h2/y c"},
		{"adjacent urls", "https://u:p@h/https://v:q@i/", "https://REDACTED@h/https://REDACTED@i/"},
		{"two urls with no space between, as a JSON argv is written", `["https://u:p@one","https://v:secret@two/r"]`,
			`["https://REDACTED@one","https://REDACTED@two/r"]`},
		{"three urls with no space between", `https://a:1@x,https://b:2@y,https://c:3@z`,
			`https://REDACTED@x,https://REDACTED@y,https://REDACTED@z`},
		{"a url with no userinfo, then one with, no space between", `"https://one","https://v:secret@two/r"`,
			`"https://one","https://REDACTED@two/r"`},
		{"a second url straight after the first one's at sign", `https://u:p@https://v:secret@two/r`,
			`https://REDACTED@https://REDACTED@two/r`},
		{"a second url whose scheme is the first one's host", `a://u:p@b://v:secret@h/r`, `a://REDACTED@b://REDACTED@h/r`},
		{"a second separator with no scheme after the first url", `https://u:p@h,://v:q@i/r`, `https://REDACTED@h,://v:q@i/r`},
		{"newline ends authority", "https://h\nuser@host", "https://h\nuser@host"},
		{"tab ends authority", "https://h\tuser@host", "https://h\tuser@host"},
		{"backslash in the password", `https://u:pa\ss@h/r`, `https://REDACTED@h/r`},
		{"shell-escaped dollar in the password", `git clone https://u:pa\$word@h/o/r.git`, `git clone https://REDACTED@h/o/r.git`},
		{"shell-escaped at sign in the password", `git clone https://u:p\@ss@h/o/r.git`, `git clone https://REDACTED@h/o/r.git`},
		{"double quote in the password", `https://u:pa"ss@h/r`, `https://REDACTED@h/r`},
		{"backtick in the password", "https://u:pa`ss@h/r", `https://REDACTED@h/r`},
		{"braces in the password", `https://u:p{a}ss@h/r`, `https://REDACTED@h/r`},
		{"pipe and caret in the password", `https://u:p|a^ss@h/r`, `https://REDACTED@h/r`},
		{"angle brackets in the password", `https://u:p<a>ss@h/r`, `https://REDACTED@h/r`},
		{"at sign before and after a quote", `https://u:p@ss"word@h/r`, `https://REDACTED@h/r`},
		{"apostrophe escaped twice", `'https://u:pa'\'''\'''\''ss@h/r'`, `'https://REDACTED@h/r'`},
		{"quoted url then a spaced email", `curl "https://h" -u a@b`, `curl "https://h" -u a@b`},
		{"quoted url with a path then an email", `{"url":"https://h/x","mail":"a@b"}`, `{"url":"https://h/x","mail":"a@b"}`},
		{"unspaced email after a quoted url is hidden too", `{"url":"https://h","mail":"a@b"}`, `{"url":"https://REDACTED@b"}`},
		{"scheme in double quotes", `curl "https"://u:secret@h/x`, `curl "https"://REDACTED@h/x`},
		{"scheme in single quotes", `curl 'https'://u:secret@h/x`, `curl 'https'://REDACTED@h/x`},
		{"escaped colon", `curl https\://u:secret@h/x`, `curl https\://REDACTED@h/x`},
		{"scheme from a variable", `curl ${S}://u:secret@h/x`, `curl ${S}://REDACTED@h/x`},
		{"scheme from a command", `curl $(echo https)://u:secret@h/x`, `curl $(echo https)://REDACTED@h/x`},
		{"quotes with no scheme", `"://u:p@h`, `"://u:p@h`},
		{"shell-escaped apostrophe in the password", `'git' 'clone' 'https://u:pa'\''ss@h/r'`, `'git' 'clone' 'https://REDACTED@h/r'`},
		{"two shell-escaped apostrophes", `'https://u:'\''p'\''@h/r'`, `'https://REDACTED@h/r'`},
		{"shell-escaped apostrophe in the user", `'https://o'\''brien:pw@h/r' next`, `'https://REDACTED@h/r' next`},
		{"shell-escaped apostrophe, no userinfo", `'https://h'\''x/r' a@b`, `'https://h'\''x/r' a@b`},
		{"shell-escaped apostrophe at the end", `https://u:p'\''`, `https://u:p'\''`},
		{"bare separator", "://", "://"},
		{"scheme only", "https://", "https://"},
		{"trailing at", "https://u:p@", "https://REDACTED@"},
		{"repeated separators", "a://://://@", "a://://://@"},
		{"invalid utf8 kept", "\xff https://u:p@h \xfe", "\xff https://REDACTED@h \xfe"},
		{"unicode userinfo", "https://ü:pä@h/r", "https://REDACTED@h/r"},
		{"nul byte", "https://u\x00:p@h", "https://u\x00:p@h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := String(tt.in); got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStringUnchangedIsSameString(t *testing.T) {
	in := "git clone https://github.com/o/r.git"
	if allocs := testing.AllocsPerRun(100, func() { _ = String(in) }); allocs != 0 {
		t.Errorf("String allocated %v times on input with nothing to redact", allocs)
	}
}

func TestStringHostileInputsAreLinear(t *testing.T) {
	inputs := map[string]string{
		"separators":     strings.Repeat("://", 200000),
		"scheme run":     strings.Repeat("a", 500000) + "://",
		"many urls":      strings.Repeat("a://u@h/", 100000),
		"long authority": "https://" + strings.Repeat("u", 500000) + "@h",
		"at signs":       "https://" + strings.Repeat("@", 500000),
	}
	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			out := String(in)
			// Each redaction replaces at least one byte with len(Marker), and
			// needs at least five bytes of input ("a://" plus one userinfo byte
			// and '@'), so output is linear in input.
			if len(out) > len(in)*(1+len(Marker)) {
				t.Errorf("output %d bytes from %d bytes of input", len(out), len(in))
			}
		})
	}
}

func TestArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, []string{}},
		{"clean", []string{"git", "clone", "https://h/r"}, []string{"git", "clone", "https://h/r"}},
		{"clone url", []string{"git", "clone", "https://u:p@h/r", "r"}, []string{"git", "clone", "https://REDACTED@h/r", "r"}},
		{"first element", []string{"https://u:p@h", "x"}, []string{"https://REDACTED@h", "x"}},
		{"bash -c", []string{"bash", "-c", "curl https://u:p@h/x | sh"}, []string{"bash", "-c", "curl https://REDACTED@h/x | sh"}},
		{"several urls in one argument", []string{"sh", "-c", `curl -d '["https://u:p@one","https://v:secret@two/r"]' https://w:s3@three`},
			[]string{"sh", "-c", `curl -d '["https://REDACTED@one","https://REDACTED@two/r"]' https://REDACTED@three`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var orig []string
			if tt.in != nil {
				orig = append([]string{}, tt.in...)
			}
			got := Args(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Args(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !reflect.DeepEqual(tt.in, orig) {
				t.Errorf("Args modified its input: now %q, was %q", tt.in, orig)
			}
		})
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestWriter(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"one line", []string{"fatal: https://u:p@h/r not found\n"}, "fatal: https://REDACTED@h/r not found\n"},
		{"split across writes", []string{"fatal: http", "s://u:", "p@h/r\n"}, "fatal: https://REDACTED@h/r\n"},
		{"carriage returns", []string{"Receiving 10%\rReceiving https://u:p@h\r"}, "Receiving 10%\rReceiving https://REDACTED@h\r"},
		{"unterminated tail flushed", []string{"a\nhttps://u:p@h"}, "a\nhttps://REDACTED@h"},
		{"nothing", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			w := NewWriter(&out)
			for _, c := range tt.chunks {
				n, err := w.Write([]byte(c))
				if err != nil || n != len(c) {
					t.Fatalf("Write(%q) = %d, %v", c, n, err)
				}
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if out.String() != tt.want {
				t.Errorf("got %q, want %q", out.String(), tt.want)
			}
		})
	}
}

func TestWriterBoundsPending(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out)
	chunk := bytes.Repeat([]byte("x"), 4096)
	for range (maxPending / len(chunk)) + 2 {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if len(w.pending) > maxPending {
		t.Errorf("pending grew to %d bytes", len(w.pending))
	}
	if out.String() != Dropped {
		t.Errorf("a token longer than the bound: wrote %d bytes, want the dropped notice once", out.Len())
	}
}

// TestWriterLongLineNeverSplitsAURL: a line longer than the Writer holds is
// passed on in pieces, and a credentialed URL that falls across a piece
// boundary, or across two writes, is still redacted or not written at all.
func TestWriterLongLineNeverSplitsAURL(t *testing.T) {
	const secret = "s3cr3t-token"
	tests := []struct {
		name   string
		writes []string
		want   string // "" when only the absence of the secret is checked
	}{
		{"one long token, the URL split across two writes",
			[]string{strings.Repeat("x", maxPending) + "https://u:", secret + "@h/r\n", "next line\n"},
			Dropped + "\nnext line\n"},
		{"one long token, the URL inside it",
			[]string{strings.Repeat("x", maxPending) + "https://u:" + secret + "@h/r tail\n"},
			""},
		{"one long token written a byte short of the bound, then the URL",
			[]string{strings.Repeat("x", maxPending-10), "https://u:" + secret, "@h/r\n"},
			""},
		{"a long line of words, the URL split across two writes",
			[]string{strings.Repeat("word ", maxPending/5) + "https://u:", secret + "@h/r\n"},
			strings.Repeat("word ", maxPending/5) + "https://REDACTED@h/r\n"},
		{"a long line of words, the scheme split from the userinfo",
			[]string{strings.Repeat("word ", maxPending/5) + "https:", "//u:" + secret + "@h/r and more\n"},
			strings.Repeat("word ", maxPending/5) + "https://REDACTED@h/r and more\n"},
		{"a short line, then a long line of words in the same write",
			[]string{"first\n" + strings.Repeat("word ", maxPending/5+10) + "https://u:", secret + "@h/r\n"},
			"first\n" + strings.Repeat("word ", maxPending/5+10) + "https://REDACTED@h/r\n"},
		{"a long token after a long line of words",
			[]string{strings.Repeat("word ", maxPending/5), strings.Repeat("y", maxPending) + "https://u:", secret + "@h/r\n"},
			""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			w := NewWriter(&out)
			for _, s := range tt.writes {
				if n, err := w.Write([]byte(s)); err != nil || n != len(s) {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if strings.Contains(got, secret) {
				t.Errorf("the credential was written (%d bytes of output)", len(got))
			}
			if tt.want != "" && got != tt.want {
				t.Errorf("got %d bytes ending %q, want %d bytes ending %q", len(got), tail(got, 80), len(tt.want), tail(tt.want, 80))
			}
		})
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func TestWriterReportsError(t *testing.T) {
	w := NewWriter(errWriter{})
	if _, err := w.Write([]byte("line\n")); err == nil {
		t.Error("Write did not report the underlying error")
	}
}

func FuzzString(f *testing.F) {
	for _, s := range []string{
		"https://u:p@h/r", "git@h:o/r", "a://://@", "https://@", "x https://u@h y",
		"\xff://\xfe@", "bash -c 'git clone https://t@h/r'",
		`["https://u:p@one","https://v:secret@two/r"]`, "a://u:p@b://v:q@h/r",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := String(s)
		if len(out) > len(s)*(1+len(Marker)) {
			t.Fatalf("output %d bytes from %d", len(out), len(s))
		}
		// Idempotent: redacting twice changes nothing more.
		if again := String(out); again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, out, again)
		}
		// Without a "://" nothing can change.
		if !strings.Contains(s, "://") && out != s {
			t.Fatalf("changed input without a scheme separator: %q -> %q", s, out)
		}
		// Valid UTF-8 stays valid: only ASCII is inserted, and cuts fall
		// next to ASCII bytes.
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Fatalf("broke UTF-8: %q -> %q", s, out)
		}
		// No URL keeps its userinfo, wherever it stands: every "://" with a
		// scheme before it is looked at on its own here, so a URL the scan
		// stepped over (one that follows another with no white space
		// between) is caught.
		if i := credentialedURL(out); i >= 0 {
			t.Fatalf("a URL kept its userinfo at byte %d: %q -> %q", i, s, out)
		}
	})
}

// credentialedURL returns the index of the first "://" in s that has a scheme
// before it and a userinfo other than Marker after it, or -1. It is the
// package's rule applied to each separator independently of the others.
func credentialedURL(s string) int {
	for i := 0; i+3 <= len(s); i++ {
		if s[i:i+3] != "://" || !hasScheme(s, i) {
			continue
		}
		end := i + 3
		for end < len(s) && !endsAuthority(s[end]) {
			end++
		}
		auth := s[i+3 : end]
		if at := strings.LastIndexByte(auth, '@'); at > 0 && auth[:at] != Marker {
			return i
		}
	}
	return -1
}

// TestStringLeavesNoCredentialedURL applies the same check to every table
// input and to URLs joined by each byte that does not end an authority.
func TestStringLeavesNoCredentialedURL(t *testing.T) {
	const a, b = "https://u:first@one", "ssh://v:second@two/r"
	for c := 0; c < 256; c++ {
		in := a + string([]byte{byte(c)}) + b + string([]byte{byte(c)}) + a
		out := String(in)
		if i := credentialedURL(out); i >= 0 {
			t.Errorf("joined by byte %#x: a URL kept its userinfo at byte %d: %q", c, i, out)
		}
		if strings.Contains(out, "second") {
			t.Errorf("joined by byte %#x: the second URL's password was kept: %q", c, out)
		}
	}
}
