//go:build !linux

package main

import (
	"fmt"
	"os"
)

// shed-firstboot is a Linux-only guest binary; the non-linux build exits
// non-zero so accidental host-side execution is loud rather than silent.
func main() {
	fmt.Fprintln(os.Stderr, "shed-firstboot is only supported on linux")
	os.Exit(1)
}
