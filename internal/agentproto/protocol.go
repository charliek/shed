// Package agentproto provides the protocol types and functions for communication
// between the shed-agent running inside Firecracker VMs and the host.
package agentproto

import (
	"encoding/binary"
	"fmt"
	"io"
)

// MaxMessageSize is the maximum allowed message size (16 MB).
const MaxMessageSize = 16 * 1024 * 1024

// Message types for the vsock protocol.
const (
	MsgTypeExecRequest    byte = 0x01
	MsgTypeResize         byte = 0x02
	MsgTypeSignal         byte = 0x03
	MsgTypeExitCode       byte = 0x04
	MsgTypeData           byte = 0x05
	MsgTypeStdinEOF       byte = 0x06
	MsgTypeHealthRequest  byte = 0x10
	MsgTypeHealthResponse byte = 0x11

	MsgTypeNotifySetup byte = 0x12 // Deprecated: replaced by MsgTypePluginMessage with system:credentials namespace
	MsgTypeFileChanged byte = 0x13 // Deprecated: replaced by MsgTypePluginMessage with system:credentials namespace

	MsgTypePluginMessage byte = 0x20 // Bidirectional: plugin envelope (JSON)
)

// Deprecated: NotifySetupMessage is replaced by plugin.CredentialSetupPayload
// sent via MsgTypePluginMessage in the system:credentials namespace.
type NotifySetupMessage struct {
	Credentials map[string]string   `json:"credentials"`        // name → target path in VM
	Excludes    map[string][]string `json:"excludes,omitempty"` // name → exclude patterns
}

// Deprecated: FileChangedMessage is replaced by plugin.CredentialChangedPayload
// sent via MsgTypePluginMessage in the system:credentials namespace.
type FileChangedMessage struct {
	Credential string   `json:"credential"` // credential mount name (e.g., "gh")
	Files      []string `json:"files"`      // relative paths that changed
}

// ExecRequest is sent to execute a command.
type ExecRequest struct {
	Cmd        []string `json:"cmd"`
	Env        []string `json:"env,omitempty"`
	TTY        bool     `json:"tty"`
	Rows       uint16   `json:"rows,omitempty"`
	Cols       uint16   `json:"cols,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
}

// ResizeMessage is sent to resize the PTY.
type ResizeMessage struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// SignalMessage is sent to signal the running process.
type SignalMessage struct {
	Signal int `json:"signal"`
}

// ExitCodeMessage is sent when the command exits.
type ExitCodeMessage struct {
	Code int `json:"code"`
}

// WriteMessage writes a framed message.
// Format: [type:1 byte][length:4 bytes big-endian][payload:length bytes]
func WriteMessage(w io.Writer, msgType byte, payload []byte) error {
	if len(payload) > MaxMessageSize {
		return fmt.Errorf("WriteMessage: payload size %d exceeds maximum %d", len(payload), MaxMessageSize)
	}

	header := make([]byte, 5)
	header[0] = msgType
	binary.BigEndian.PutUint32(header[1:5], uint32(len(payload)))

	if err := writeAll(w, header); err != nil {
		return err
	}

	if len(payload) > 0 {
		if err := writeAll(w, payload); err != nil {
			return err
		}
	}

	return nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// ReadMessage reads a framed message.
func ReadMessage(r io.Reader) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}

	msgType := header[0]
	length := binary.BigEndian.Uint32(header[1:5])

	if length == 0 {
		return msgType, nil, nil
	}

	if length > MaxMessageSize {
		return 0, nil, fmt.Errorf("message size %d exceeds maximum %d", length, MaxMessageSize)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}

	return msgType, payload, nil
}
