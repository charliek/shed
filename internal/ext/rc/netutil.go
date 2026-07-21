package rc

import "net"

// freeLoopbackPort returns a currently-unused TCP port on the loopback interface,
// suitable for handing to a short-lived child process (opencode's `--port`) that will
// bind it moments later. Implementation: bind an ephemeral listener on 127.0.0.1:0,
// read back the OS-assigned port, and close the listener immediately so the caller's
// child process is free to bind it in turn.
//
// This has an inherent, ACCEPTED TOCTOU race: between this close and opencode's own
// bind, some other process on the host could grab the same port. The race is narrow
// (opencode is exec'd within the same tmux new-session call Create issues right after
// allocating the port) and the failure mode is benign, not silent: a lost race makes
// opencode's embedded HTTP server fail to bind, so opencode itself exits — which
// surfaces as a visible dead RC session (ClassifyPane's shed-guest "exited to shell"
// signal catches it), not a hard-to-diagnose hang or a watcher silently attached to
// the wrong port. See the design doc's R9 risk note.
func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // best-effort; a close failure here would not change the port we hand back
	return port, nil
}
