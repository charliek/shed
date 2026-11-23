package refsource

import (
	"reflect"
	"strings"
	"testing"
)

func TestReadSSE(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []sseEvent
	}{
		{"one event", "event: request\ndata: {}\n\n", []sseEvent{{"request", "{}"}}},
		{"comments ignored", ": keepalive\n\n: x\nevent: request\ndata: a\n\n", []sseEvent{{"request", "a"}}},
		{"crlf", "event: request\r\ndata: a\r\n\r\n", []sseEvent{{"request", "a"}}},
		{"cr", "event: request\rdata: a\r\r", []sseEvent{{"request", "a"}}},
		{"data lines joined", "data: a\ndata: b\n\n", []sseEvent{{"message", "a\nb"}}},
		{"no space after the colon", "event:request\ndata:a\n\n", []sseEvent{{"request", "a"}}},
		{"an event with no data is dropped", "event: request\n\n", nil},
		{"unfinished at the end is dropped", "event: request\ndata: a\n", nil},
		{"unknown fields ignored", "id: 7\nretry: 10\nevent: request\ndata: a\n\n", []sseEvent{{"request", "a"}}},
		{"two events", "event: request\ndata: a\n\nevent: other\ndata: b\n\n", []sseEvent{{"request", "a"}, {"other", "b"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []sseEvent
			if err := readSSE(strings.NewReader(c.in), func(e sseEvent) bool { got = append(got, e); return true }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
