package agentproto

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestWriteMessage_Empty(t *testing.T) {
	var buf bytes.Buffer

	err := WriteMessage(&buf, MsgTypePluginMessage, nil)
	if err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}

	// Verify header
	data := buf.Bytes()
	if len(data) != 5 {
		t.Errorf("wrote %d bytes, want 5", len(data))
	}

	if data[0] != MsgTypePluginMessage {
		t.Errorf("message type = %#x, want %#x", data[0], MsgTypePluginMessage)
	}

	length := binary.BigEndian.Uint32(data[1:5])
	if length != 0 {
		t.Errorf("payload length = %d, want 0", length)
	}
}

func TestWriteMessage_WithPayload(t *testing.T) {
	var buf bytes.Buffer

	payload := []byte("test payload data")
	err := WriteMessage(&buf, MsgTypeData, payload)
	if err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}

	// Verify header + payload
	data := buf.Bytes()
	expectedLen := 5 + len(payload)
	if len(data) != expectedLen {
		t.Errorf("wrote %d bytes, want %d", len(data), expectedLen)
	}

	if data[0] != MsgTypeData {
		t.Errorf("message type = %#x, want %#x", data[0], MsgTypeData)
	}

	length := binary.BigEndian.Uint32(data[1:5])
	if length != uint32(len(payload)) {
		t.Errorf("payload length = %d, want %d", length, len(payload))
	}

	if !bytes.Equal(data[5:], payload) {
		t.Error("payload content mismatch")
	}
}

func TestReadMessage_Valid(t *testing.T) {
	// Create a valid message
	payload := []byte("hello world")
	var buf bytes.Buffer
	buf.WriteByte(MsgTypeExecRequest)
	binary.Write(&buf, binary.BigEndian, uint32(len(payload)))
	buf.Write(payload)

	msgType, data, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}

	if msgType != MsgTypeExecRequest {
		t.Errorf("message type = %#x, want %#x", msgType, MsgTypeExecRequest)
	}

	if !bytes.Equal(data, payload) {
		t.Errorf("payload = %q, want %q", data, payload)
	}
}

func TestReadMessage_Empty(t *testing.T) {
	// Create an empty payload message
	var buf bytes.Buffer
	buf.WriteByte(MsgTypePluginMessage)
	binary.Write(&buf, binary.BigEndian, uint32(0))

	msgType, data, err := ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}

	if msgType != MsgTypePluginMessage {
		t.Errorf("message type = %#x, want %#x", msgType, MsgTypePluginMessage)
	}

	if data != nil {
		t.Errorf("payload = %v, want nil", data)
	}
}

func TestReadMessage_Truncated(t *testing.T) {
	// Header only, no complete read possible
	buf := bytes.NewBuffer([]byte{MsgTypeData})

	_, _, err := ReadMessage(buf)
	if err == nil {
		t.Error("ReadMessage() expected error for truncated header")
	}
}

func TestReadMessage_TruncatedPayload(t *testing.T) {
	// Header says 100 bytes but only 5 available
	var buf bytes.Buffer
	buf.WriteByte(MsgTypeData)
	binary.Write(&buf, binary.BigEndian, uint32(100))
	buf.Write([]byte("short"))

	_, _, err := ReadMessage(&buf)
	if err == nil {
		t.Error("ReadMessage() expected error for truncated payload")
	}
}

func TestReadMessage_Oversized(t *testing.T) {
	// Create a message claiming to have more than MaxMessageSize bytes
	var buf bytes.Buffer
	buf.WriteByte(MsgTypeData)
	binary.Write(&buf, binary.BigEndian, uint32(MaxMessageSize+1))

	_, _, err := ReadMessage(&buf)
	if err == nil {
		t.Error("ReadMessage() expected error for oversized message")
	}
}

func TestReadWriteRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		msgType byte
		payload []byte
	}{
		{"empty plugin message", MsgTypePluginMessage, nil},
		{"data message", MsgTypeData, []byte("some output data")},
		{"exec request", MsgTypeExecRequest, []byte(`{"cmd":["ls","-la"]}`)},
		{"exit code", MsgTypeExitCode, []byte(`{"code":0}`)},
		{"resize", MsgTypeResize, []byte(`{"rows":24,"cols":80}`)},
		{"signal", MsgTypeSignal, []byte(`{"signal":15}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			// Write
			if err := WriteMessage(&buf, tt.msgType, tt.payload); err != nil {
				t.Fatalf("WriteMessage() error = %v", err)
			}

			// Read
			gotType, gotPayload, err := ReadMessage(&buf)
			if err != nil {
				t.Fatalf("ReadMessage() error = %v", err)
			}

			if gotType != tt.msgType {
				t.Errorf("message type = %#x, want %#x", gotType, tt.msgType)
			}

			if !bytes.Equal(gotPayload, tt.payload) {
				t.Errorf("payload = %q, want %q", gotPayload, tt.payload)
			}
		})
	}
}

func TestMaxMessageSizeEnforced(t *testing.T) {
	// Create a reader that claims a huge message size
	var buf bytes.Buffer
	buf.WriteByte(MsgTypeData)
	binary.Write(&buf, binary.BigEndian, uint32(MaxMessageSize+1))

	_, _, err := ReadMessage(&buf)
	if err == nil {
		t.Error("expected error for message exceeding MaxMessageSize")
	}

	// Verify exact boundary
	buf.Reset()
	buf.WriteByte(MsgTypeData)
	binary.Write(&buf, binary.BigEndian, uint32(MaxMessageSize))
	// This should not error on the size check, but will error due to EOF
	_, _, err = ReadMessage(&buf)
	if err != io.EOF && err != io.ErrUnexpectedEOF {
		t.Errorf("expected EOF error, got: %v", err)
	}

	// Verify WriteMessage enforces size
	tooLarge := make([]byte, MaxMessageSize+1)
	if err := WriteMessage(&buf, MsgTypeData, tooLarge); err == nil {
		t.Error("expected error for WriteMessage exceeding MaxMessageSize")
	}
}

func TestMessageTypes(t *testing.T) {
	// Verify message type constants are distinct
	types := []byte{
		MsgTypeExecRequest,
		MsgTypeResize,
		MsgTypeSignal,
		MsgTypeExitCode,
		MsgTypeData,
		MsgTypePluginMessage,
	}

	seen := make(map[byte]bool)
	for _, msgType := range types {
		if seen[msgType] {
			t.Errorf("duplicate message type: %#x", msgType)
		}
		seen[msgType] = true
	}
}
