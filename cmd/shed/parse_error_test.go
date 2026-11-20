package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestParseErrorEscapesServerText: what a server sends in an error answer
// (a plain-text body, or a structured error's code and message) is escaped
// before it is shown: an ESC sequence or a line break never reaches the
// terminal raw, and printable text is unchanged.
func TestParseErrorEscapesServerText(t *testing.T) {
	answer := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
	}
	cases := []struct {
		name, body, want string
	}{
		{"plain text", "bad\x1b[2Jthing\nHost *\n", `server returned status 500: bad\x1b[2Jthing\nHost *`},
		{"plain text, printable", "404 page not found\n", "server returned status 500: 404 page not found"},
		{"structured", `{"error":{"code":"BAD\u001b[31m","message":"no\nsuch \u001b]0;title\u0007shed"}}`,
			`BAD\x1b[31m: no\nsuch \x1b]0;title\ashed`},
		{"structured, printable", `{"error":{"code":"SHED_NOT_FOUND","message":"shed \"box\" not found"}}`,
			`SHED_NOT_FOUND: shed "box" not found`},
	}
	c := &APIClient{}
	for _, tc := range cases {
		err := c.parseError(answer(http.StatusInternalServerError, tc.body))
		var status *apiStatusError
		if !errors.As(err, &status) || status.Status != http.StatusInternalServerError {
			t.Errorf("%s: err = %#v, want an *apiStatusError with the status", tc.name, err)
			continue
		}
		if got := err.Error(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if strings.ContainsAny(err.Error(), "\x1b\n\a") {
			t.Errorf("%s: a raw control character reached the message: %q", tc.name, err)
		}
	}
}
