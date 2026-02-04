package agentproto

import (
	"encoding/json"
	"io"
)

// WriteExitCode sends an exit code message.
func WriteExitCode(w io.Writer, code int) error {
	msg := ExitCodeMessage{Code: code}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return WriteMessage(w, MsgTypeExitCode, data)
}

// WriteData sends a data frame (stdout/stderr output).
func WriteData(w io.Writer, data []byte) error {
	return WriteMessage(w, MsgTypeData, data)
}
