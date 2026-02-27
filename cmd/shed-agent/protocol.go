//go:build linux
// +build linux

package main

import (
	"io"

	"github.com/charliek/shed/internal/agentproto"
)

// Re-export constants from agentproto for local use.
const (
	MaxMessageSize        = agentproto.MaxMessageSize
	MsgTypeExecRequest    = agentproto.MsgTypeExecRequest
	MsgTypeResize         = agentproto.MsgTypeResize
	MsgTypeSignal         = agentproto.MsgTypeSignal
	MsgTypeExitCode       = agentproto.MsgTypeExitCode
	MsgTypeData           = agentproto.MsgTypeData
	MsgTypeStdinEOF       = agentproto.MsgTypeStdinEOF
	MsgTypeHealthRequest  = agentproto.MsgTypeHealthRequest
	MsgTypeHealthResponse = agentproto.MsgTypeHealthResponse

	MsgTypeNotifySetup = agentproto.MsgTypeNotifySetup
	MsgTypeFileChanged = agentproto.MsgTypeFileChanged
)

// Type aliases for agentproto types.
type ExecRequest = agentproto.ExecRequest
type ResizeMessage = agentproto.ResizeMessage
type SignalMessage = agentproto.SignalMessage
type ExitCodeMessage = agentproto.ExitCodeMessage
type NotifySetupMessage = agentproto.NotifySetupMessage
type FileChangedMessage = agentproto.FileChangedMessage

// writeMessage writes a framed message.
func writeMessage(w io.Writer, msgType byte, payload []byte) error {
	return agentproto.WriteMessage(w, msgType, payload)
}

// readMessage reads a framed message.
func readMessage(r io.Reader) (byte, []byte, error) {
	return agentproto.ReadMessage(r)
}

// writeExitCode sends an exit code message.
func writeExitCode(w io.Writer, code int) error {
	return agentproto.WriteExitCode(w, code)
}

// writeData sends a data frame (stdout/stderr output).
func writeData(w io.Writer, data []byte) error {
	return agentproto.WriteData(w, data)
}
