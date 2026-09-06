package rc

import "testing"

// TestExpandTilde pins the lived failure: `--workdir ~/prox` started the agent
// in HOME, because the CLI quotes every argv element and tmux takes -c as a
// literal path — so nothing on the way expanded it. The session then reported a
// workdir the pane was not actually in, which is also why the hub could not
// correlate its message feed.
func TestExpandTilde(t *testing.T) {
	const home = "/home/shed"
	for _, tc := range []struct{ in, want string }{
		{"~/prox", "/home/shed/prox"},
		{"~", "/home/shed"},
		{"~/", "/home/shed/"},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		// Not a shell: no passwd lookup, so ~user is left alone rather than
		// guessed at.
		{"~root/x", "~root/x"},
		{"", ""},
	} {
		if got := expandTilde(tc.in, home); got != tc.want {
			t.Errorf("expandTilde(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// With no HOME there is nothing to expand against; leave the value alone
	// rather than inventing a path.
	if got := expandTilde("~/prox", ""); got != "~/prox" {
		t.Errorf("no HOME: got %q", got)
	}
}
