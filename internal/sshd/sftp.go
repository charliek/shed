package sshd

import (
	"fmt"
	"log"

	"github.com/gliderlabs/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/vmutil"
)

// handleSFTP handles the "sftp" subsystem request by executing sftp-server
// inside the target shed VM. This enables scp and sftp file transfers.
func (s *Server) handleSFTP(sess ssh.Session) {
	// The username the connection authenticated as, as on every channel
	// (authenticatedUser); ensureRunning turns it into a shed (shedNameFor).
	user := authenticatedUser(sess.Context())
	remoteAddr := sess.RemoteAddr()

	log.Printf("SFTP session started: user=%s remote=%s", user, remoteAddr)
	defer log.Printf("SFTP session ended: user=%s remote=%s", user, remoteAddr)

	// The channel's principal, from the key the connection authenticated
	// with, resolved again for this channel.
	p, err := s.principalFor(sess.Context())
	if err != nil {
		log.Printf("SFTP session refused: user=%s: %v", user, err)
		fmt.Fprintf(sess.Stderr(), "Error: %v\n", err)
		_ = sess.Exit(1)
		return
	}
	ctx := backend.WithPrincipal(sess.Context(), p)

	shed, err := s.ensureRunning(ctx, user, sess.Stderr(), false)
	if err != nil {
		fmt.Fprintf(sess.Stderr(), "Error: %v\n", err)
		_ = sess.Exit(1)
		return
	}
	ctx = backend.PinCreatedAt(ctx, shed.CreatedAt)

	opts := backend.ExecOptions{
		Cmd:    []string{"/usr/lib/openssh/sftp-server"},
		Stdin:  &sessionReadCloser{sess},
		Stdout: &sessionWriteCloser{sess},
		Stderr: &sessionStderrWriteCloser{sess},
		TTY:    false,
	}

	log.Printf("SFTP: executing sftp-server in shed %s", shed.Name)

	if err := s.backend.Exec(ctx, shed.Name, opts); err != nil {
		log.Printf("SFTP: exec failed for shed %s: %v", shed.Name, err)
		if exitErr, ok := err.(*vmutil.ExitError); ok {
			_ = sess.Exit(exitErr.Code)
		} else {
			_ = sess.Exit(1)
		}
		return
	}

	_ = sess.Exit(0)
}
