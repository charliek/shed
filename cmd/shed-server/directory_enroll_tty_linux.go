package main

import "golang.org/x/sys/unix"

// The terminal requests echoOff makes: read the settings, set them now, and
// set them discarding input not yet read.
const (
	ioctlGetTermios      = unix.TCGETS
	ioctlSetTermios      = unix.TCSETS
	ioctlSetTermiosFlush = unix.TCSETSF
)
