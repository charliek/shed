package main

import "golang.org/x/sys/unix"

// The terminal requests echoOff makes: read the settings, set them now, and
// set them discarding input not yet read.
const (
	ioctlGetTermios      = unix.TIOCGETA
	ioctlSetTermios      = unix.TIOCSETA
	ioctlSetTermiosFlush = unix.TIOCSETAF
)
