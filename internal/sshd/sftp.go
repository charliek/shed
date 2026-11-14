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
	user := sess.User()
	remoteAddr := sess.RemoteAddr()

	log.Printf("SFTP session started: user=%s remote=%s", user, remoteAddr)
	defer log.Printf("SFTP session ended: user=%s remote=%s", user, remoteAddr)

	shedName := user
	if shedName == "" {
		fmt.Fprintf(sess.Stderr(), "Error: invalid username\n")
		_ = sess.Exit(1)
		return
	}

	ctx := sess.Context()

	if _, err := s.ensureRunning(ctx, shedName, sess.Stderr(), false); err != nil {
		fmt.Fprintf(sess.Stderr(), "Error: %v\n", err)
		_ = sess.Exit(1)
		return
	}

	opts := backend.ExecOptions{
		Cmd:    []string{"/usr/lib/openssh/sftp-server"},
		Stdin:  &sessionReadCloser{sess},
		Stdout: &sessionWriteCloser{sess},
		Stderr: &sessionStderrWriteCloser{sess},
		TTY:    false,
	}

	log.Printf("SFTP: executing sftp-server in shed %s", shedName)

	if err := s.backend.Exec(ctx, shedName, opts); err != nil {
		log.Printf("SFTP: exec failed for shed %s: %v", shedName, err)
		if exitErr, ok := err.(*vmutil.ExitError); ok {
			_ = sess.Exit(exitErr.Code)
		} else {
			_ = sess.Exit(1)
		}
		return
	}

	_ = sess.Exit(0)
}
