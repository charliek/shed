//go:build linux || darwin

package main

import (
	"sync"

	"golang.org/x/sys/unix"
)

// echoOff turns echo off on the terminal fd, keeping line input and the
// signal keys (so Ctrl-C still interrupts the command), as a password read
// does. restore puts the terminal back as it was, once however often it is
// called, discarding input not yet read.
func echoOff(fd int) (restore func(), err error) {
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	noEcho := *old
	noEcho.Lflag &^= unix.ECHO
	noEcho.Lflag |= unix.ICANON | unix.ISIG
	noEcho.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &noEcho); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermiosFlush, old) })
	}, nil
}
