package sshd

import (
	"fmt"
	"log"

	"github.com/gliderlabs/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
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

	shed, err := s.backend.GetShed(ctx, shedName)
	if err != nil {
		log.Printf("SFTP: failed to get shed %s: %v", shedName, err)
		fmt.Fprintf(sess.Stderr(), "Error: shed '%s' not found\n", shedName)
		_ = sess.Exit(1)
		return
	}

	if shed.Status == config.StatusStopped {
		log.Printf("SFTP: auto-starting stopped shed: %s", shedName)
		if _, err := s.backend.StartShed(ctx, shedName); err != nil {
			fmt.Fprintf(sess.Stderr(), "Error: failed to start shed: %v\n", err)
			_ = sess.Exit(1)
			return
		}
		if err := s.waitForReady(ctx, shedName); err != nil {
			fmt.Fprintf(sess.Stderr(), "Error: shed not ready: %v\n", err)
			_ = sess.Exit(1)
			return
		}
		shed, err = s.backend.GetShed(ctx, shedName)
		if err != nil {
			fmt.Fprintf(sess.Stderr(), "Error: failed to get shed after start: %v\n", err)
			_ = sess.Exit(1)
			return
		}
	}

	if shed.Status != config.StatusRunning {
		fmt.Fprintf(sess.Stderr(), "Error: shed '%s' is not running (status: %s)\n", shedName, shed.Status)
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
