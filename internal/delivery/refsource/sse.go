package refsource

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// sseEvent is one dispatched server-sent event.
type sseEvent struct {
	Type string
	Data string
}

// maxSSELine is the longest line the reader takes: a request is far smaller.
const maxSSELine = 1 << 20

// readSSE reads server-sent events from r as the HTML standard defines them
// (lines ended by LF, CRLF or CR; comment lines ignored; data lines joined
// with LF; an event dispatched at a blank line; an event with no data
// dropped) and calls fn with each, until r ends or fn returns false.
func readSSE(r io.Reader, fn func(sseEvent) bool) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	sc.Split(scanSSELines)
	var typ string
	var data []string
	hasData := false
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if hasData {
				t := typ
				if t == "" {
					t = "message"
				}
				if !fn(sseEvent{Type: t, Data: strings.Join(data, "\n")}) {
					return nil
				}
			}
			typ, data, hasData = "", nil, false
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			typ = value
		case "data":
			data = append(data, value)
			hasData = true
		}
	}
	return sc.Err()
}

// scanSSELines splits on LF, CRLF or CR.
func scanSSELines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\r' {
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if !atEOF {
				return 0, nil, nil // a CR at the end: wait to see whether LF follows
			}
		}
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
