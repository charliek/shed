//go:build !linux

package sshlink

import "net"

// quickAck is a Linux matter; see quickack_linux.go. Elsewhere it does
// nothing.
func quickAck(net.Conn) func() { return func() {} }
