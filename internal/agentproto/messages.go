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

// WriteData sends a stdout data frame (agent→host: process stdout on the non-PTY
// channel, or merged output on the PTY channel; host→agent: stdin).
func WriteData(w io.Writer, data []byte) error {
	return WriteMessage(w, MsgTypeData, data)
}

// WriteStderr sends a process-stderr data frame (agent→host, non-PTY channel).
// Kept distinct from WriteData so the host can route it to a separate stderr
// stream instead of folding it into a binary stdout protocol (see MsgTypeStderr).
func WriteStderr(w io.Writer, data []byte) error {
	return WriteMessage(w, MsgTypeStderr, data)
}
