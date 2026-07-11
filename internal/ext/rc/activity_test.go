package rc

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeLastMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain passes through", "hello world", "hello world"},
		{"collapses runs of whitespace", "hello   \t\n  world", "hello world"},
		{"trims leading and trailing whitespace", "  \tpadded\n ", "padded"},
		{"strips CSI color codes", "\x1b[31mred\x1b[0m and \x1b[1;32mgreen\x1b[0m", "red and green"},
		{"strips CSI with colon params (ISO 8613-6 truecolor)", "\x1b[38:2:255:0:0mred\x1b[0m", "red"},
		{"strips CSI with > private param", "\x1b[>4;2mtext\x1b[0m", "text"},
		{"strips CSI with = private param", "\x1b[=3htext", "text"},
		{"strips OSC hyperlink (BEL-terminated)", "\x1b]8;;https://example.com\x07link text\x1b]8;;\x07", "link text"},
		{"strips OSC (ST-terminated)", "\x1b]0;window title\x1b\\body", "body"},
		{"strips unterminated OSC hyperlink (chunk-cut capture)", "before \x1b]8;;https://example.com/truncat", "before"},
		{"strips lone two-byte escape", "before\x1bMafter", "beforeafter"},
		{"drops NUL and BEL control chars", "a\x00b\x07c", "abc"},
		{"drops DEL", "x\x7fy", "xy"},
		{"drops C1 controls (8-bit CSI, UTF-8 encoded)", "a\u009bc", "ac"},
		{"keeps multibyte content intact", "café résumé — 日本語", "café résumé — 日本語"},
		{"newline between words collapses to space", "line one\nline two", "line one line two"},
		{"empty stays empty", "", ""},
		{"whitespace-only becomes empty", "   \n\t  ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeLastMessage(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeLastMessage(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("output is not valid UTF-8: %q", got)
			}
		})
	}
}

func TestSanitizeLastMessageTruncatesOnRuneBoundary(t *testing.T) {
	t.Run("ascii truncates to 200 runes", func(t *testing.T) {
		in := strings.Repeat("a", 250)
		got := SanitizeLastMessage(in)
		if n := utf8.RuneCountInString(got); n != maxLastMessageRunes {
			t.Fatalf("rune count = %d, want %d", n, maxLastMessageRunes)
		}
	})

	t.Run("multibyte truncates on a codepoint boundary (never mid-rune)", func(t *testing.T) {
		// Every rune is 3 bytes; a naive byte slice at 200 would split a codepoint.
		in := strings.Repeat("世", 250)
		got := SanitizeLastMessage(in)
		if !utf8.ValidString(got) {
			t.Fatalf("truncation corrupted a multibyte rune: %q", got)
		}
		if n := utf8.RuneCountInString(got); n != maxLastMessageRunes {
			t.Fatalf("rune count = %d, want %d", n, maxLastMessageRunes)
		}
		if len(got) != maxLastMessageRunes*3 {
			t.Fatalf("byte len = %d, want %d (200 3-byte runes)", len(got), maxLastMessageRunes*3)
		}
	})

	t.Run("boundary run at exactly 200 runes is unchanged", func(t *testing.T) {
		in := strings.Repeat("x", maxLastMessageRunes)
		if got := SanitizeLastMessage(in); got != in {
			t.Fatalf("exactly-200 input was altered: len=%d", utf8.RuneCountInString(got))
		}
	})

	t.Run("escape sequences do not count toward the length budget", func(t *testing.T) {
		// 200 visible runes wrapped in color codes should survive intact.
		body := strings.Repeat("z", maxLastMessageRunes)
		in := "\x1b[31m" + body + "\x1b[0m"
		if got := SanitizeLastMessage(in); got != body {
			t.Fatalf("escapes affected truncation: got %d runes", utf8.RuneCountInString(got))
		}
	})
}

func TestDisplayActivity(t *testing.T) {
	// Lifecycle states that suppress activity entirely.
	blocking := []State{StateNeedsTrust, StateNeedsAuth, StateDead}
	// States that let activity render.
	passthrough := []State{StateStarting, StateReady, StateReconnecting}
	activities := []Activity{ActivityWorking, ActivityNeedsInput, ActivityIdle, ActivityUnknown}

	for _, st := range blocking {
		for _, act := range activities {
			if got := DisplayActivity(st, act); got != "" {
				t.Errorf("DisplayActivity(%q, %q) = %q, want \"\" (lifecycle trumps)", st, act, got)
			}
		}
	}
	for _, st := range passthrough {
		for _, act := range activities {
			if got := DisplayActivity(st, act); got != act {
				t.Errorf("DisplayActivity(%q, %q) = %q, want passthrough %q", st, act, got, act)
			}
		}
		// Empty activity stays empty even when the state permits.
		if got := DisplayActivity(st, ""); got != "" {
			t.Errorf("DisplayActivity(%q, \"\") = %q, want \"\"", st, got)
		}
	}
}
