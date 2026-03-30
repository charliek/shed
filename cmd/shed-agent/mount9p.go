//go:build linux
// +build linux

package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/user"
	"strconv"
	"syscall"
	"time"
)

// runMount9P handles the mount-9p subcommand. It connects to a 9P server
// over TCP, obtains a file descriptor, and uses syscall.Mount to mount
// the 9P filesystem at the target path. This requires root privileges.
func runMount9P() {
	fs := flag.NewFlagSet("mount-9p", flag.ExitOnError)

	addr := fs.String("addr", "", "9P server address (host:port)")
	target := fs.String("target", "", "mount target path")
	readOnly := fs.Bool("readonly", false, "mount as read-only")
	tag := fs.String("tag", "9p", "mount tag identifying the mount")

	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("mount-9p: failed to parse flags: %v", err)
	}

	if *addr == "" {
		log.Fatalf("mount-9p: --addr is required")
	}
	if *target == "" {
		log.Fatalf("mount-9p: --target is required")
	}

	// Connect to the 9P server
	conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
	if err != nil {
		log.Fatalf("mount-9p: failed to connect to %s: %v", *addr, err)
	}

	// Get the file descriptor from the TCP connection.
	// tcpConn.File() dups the fd; we close the original conn afterward.
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		conn.Close()
		log.Fatalf("mount-9p: connection is not TCP")
	}

	f, err := tcpConn.File()
	if err != nil {
		conn.Close()
		log.Fatalf("mount-9p: failed to get fd from connection: %v", err)
	}
	conn.Close() // Close original; kernel uses the dup'd fd

	fd := f.Fd()

	// Create the mount target directory
	if err := os.MkdirAll(*target, 0755); err != nil {
		f.Close()
		log.Fatalf("mount-9p: failed to create mount target %s: %v", *target, err)
	}

	// Build mount flags
	flags := uintptr(syscall.MS_NODEV)
	if *readOnly {
		flags |= syscall.MS_RDONLY
	}

	// Build mount data with 9P options
	mountData := fmt.Sprintf("trans=fd,rfdno=%d,wfdno=%d,version=9p2000.L,msize=1048576", fd, fd)

	// Perform the mount
	if err := syscall.Mount(*tag, *target, "9p", flags, mountData); err != nil {
		f.Close()
		log.Fatalf("mount-9p: mount failed: %v", err)
	}

	// Close the file descriptor; the kernel keeps it open after mount
	f.Close()

	// Note: do NOT chown the mount target after mount. After syscall.Mount,
	// the target path points at the remote 9P filesystem, so chown would
	// propagate through 9P to the host and change host directory ownership.
	// The UID remapping layer handles ownership transparently.

	log.Printf("mount-9p: mounted %s at %s (tag=%s, readonly=%v)", *addr, *target, *tag, *readOnly)
}

// chownToShedUser resolves the "shed" user dynamically and chowns the path.
func chownToShedUser(path string) error {
	u, err := user.Lookup("shed")
	if err != nil {
		return fmt.Errorf("lookup shed user: %w", err)
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}

	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}

	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", path, uid, gid, err)
	}

	return nil
}
