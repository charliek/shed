package roostprovider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestExactRequestJSON pins every request line this package can emit, byte for
// byte. This is the assertion that would have caught each of the four ways a
// Go-shaped guess at roost's wire goes wrong:
//
//   - `id` is a STRING. roost serializes every int64 id through `string_int64`,
//     so `{"id":1,…}` is a shape roost's own client never emits.
//   - empty params are `{}`, never `null`. `RawRequest.params` defaults to an
//     empty object when omitted, and `SessionIdentifyParams` is
//     `deny_unknown_fields` — it will not decode a null.
//   - `tab.open`'s `project_id` is a STRING too, `"0"` being roost's
//     "no project named" sentinel.
//   - no extra keys anywhere, in the envelope or in the params: roost denies
//     unknown fields on both.
func TestExactRequestJSON(t *testing.T) {
	tests := []struct {
		name   string
		op     string
		params any
		want   string
	}{
		{
			name:   "session.identify",
			op:     opSessionIdentify,
			params: emptyParams{},
			want:   `{"id":"1","op":"session.identify","params":{}}`,
		},
		{
			name:   "tab.list",
			op:     opTabList,
			params: emptyParams{},
			want:   `{"id":"1","op":"tab.list","params":{}}`,
		},
		{
			name: "tab.open with no project",
			op:   opTabOpen,
			params: TabOpenParams{
				ProjectID: "0",
				Cwd:       "/home/shed",
				Title:     "claude",
				Argv:      LaunchArgv("claude"),
			},
			want: `{"id":"1","op":"tab.open","params":` +
				`{"project_id":"0","cwd":"/home/shed","title":"claude","argv":["bash","-lc","exec \"$@\"","shed","claude"]}}`,
		},
		{
			name: "tab.open into a project",
			op:   opTabOpen,
			params: TabOpenParams{
				ProjectID: "12",
				Cwd:       "/home/shed/roost",
				Title:     "cursor",
				Argv:      LaunchArgv("cursor-agent"),
			},
			want: `{"id":"1","op":"tab.open","params":` +
				`{"project_id":"12","cwd":"/home/shed/roost","title":"cursor","argv":["bash","-lc","exec \"$@\"","shed","cursor-agent"]}}`,
		},
		{
			// HTML escaping is off, so an `&` in a cwd travels as itself.
			// Go's default would render it `&` — valid JSON that decodes
			// identically, but not what serde emits, so the two sides' wire
			// logs would stop being diffable.
			name: "a cwd with an ampersand is not HTML-escaped",
			op:   opTabOpen,
			params: TabOpenParams{
				ProjectID: "0",
				Cwd:       "/home/shed/a&b<c>d",
				Title:     "gx",
				Argv:      LaunchArgv("gx"),
			},
			want: `{"id":"1","op":"tab.open","params":` +
				`{"project_id":"0","cwd":"/home/shed/a&b<c>d","title":"gx","argv":["bash","-lc","exec \"$@\"","shed","gx"]}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeRequest(tc.op, tc.params)
			if err != nil {
				t.Fatalf("encodeRequest: %v", err)
			}
			// The newline is the frame terminator; assert it separately so a
			// missing one cannot hide inside a string comparison.
			if !bytes.HasSuffix(got, []byte("\n")) {
				t.Fatalf("request line is not newline-terminated: %q", got)
			}
			if line := string(bytes.TrimSuffix(got, []byte("\n"))); line != tc.want {
				t.Errorf("request:\n got %s\nwant %s", line, tc.want)
			}
			if bytes.Count(got, []byte("\n")) != 1 {
				t.Errorf("a request must be exactly one line: %q", got)
			}
		})
	}
}

const (
	okReply    = `{"id":"1","ok":true,"result":{"session_protocol":4}}`
	eventFrame = `{"event":"tab.opened","revision":43,"payload":{"tab":{"id":"5"}}}`
)

func TestReadReply(t *testing.T) {
	t.Run("a plain reply", func(t *testing.T) {
		resp, err := readReply(strings.NewReader(okReply+"\n"), opSessionIdentify)
		if err != nil {
			t.Fatalf("readReply: %v", err)
		}
		if !resp.OK {
			t.Fatalf("reply is not ok")
		}
	})

	// The property the plan calls out by name: a roost session pushes events at
	// a connection that never subscribed, and they are not this request's
	// answer. A reader that did not skip them would fail every call made while
	// anything else was happening on the far side.
	t.Run("event frames before the reply are skipped", func(t *testing.T) {
		stream := eventFrame + "\n" + eventFrame + "\n" + okReply + "\n"
		resp, err := readReply(strings.NewReader(stream), opSessionIdentify)
		if err != nil {
			t.Fatalf("readReply: %v", err)
		}
		if !resp.OK {
			t.Fatalf("reply is not ok")
		}
	})

	t.Run("a reply with no trailing newline", func(t *testing.T) {
		resp, err := readReply(strings.NewReader(okReply), opSessionIdentify)
		if err != nil {
			t.Fatalf("readReply: %v", err)
		}
		if !resp.OK {
			t.Fatalf("reply is not ok")
		}
	})

	t.Run("blank lines are not frames", func(t *testing.T) {
		resp, err := readReply(strings.NewReader("\n\n"+okReply+"\n"), opSessionIdentify)
		if err != nil {
			t.Fatalf("readReply: %v", err)
		}
		if !resp.OK {
			t.Fatalf("reply is not ok")
		}
	})

}

func TestReadReplyErrors(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   string
	}{
		{"nothing at all", "", "closed without answering"},
		{"only events", eventFrame + "\n", "closed without answering"},
		{"not JSON", "this is not json\n", "not JSON"},
		{
			"an answer to a different request",
			`{"id":"7","ok":true,"result":{}}` + "\n",
			`carried id "7"`,
		},
		// The id comparison is TYPE-SAFE. roost serializes every envelope id
		// through `string_int64`, so `"1"` is the only shape roost's own
		// session emits — a bare `1` is a different type from a different
		// speaker, and decoding it as this connection's reply would be
		// accepting a frame that is not ours.
		{
			"a numeric id is not our reply",
			`{"id":1,"ok":true,"result":{}}` + "\n",
			`carried id 1, not the string "1"`,
		},
		{
			"a float id is not our reply either",
			`{"id":1.0,"ok":true,"result":{}}` + "\n",
			"carried id 1.0",
		},
		{
			"a null id is not our reply",
			`{"id":null,"ok":true,"result":{}}` + "\n",
			"carried id null",
		},
		{
			"an object id is not our reply",
			`{"id":{"n":"1"},"ok":true,"result":{}}` + "\n",
			`carried id {"n":"1"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readReply(strings.NewReader(tc.stream), opTabList)
			if err == nil {
				t.Fatalf("readReply accepted %q", tc.stream)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestResponseErrorEnvelope pins how an `ok:false` reply surfaces: roost's
// kebab-case code and its human message, in the shape roost's own
// `render_response_error` uses.
func TestResponseErrorEnvelope(t *testing.T) {
	stream := `{"id":"1","ok":false,"error":{"code":"unknown-op","message":"no such op: tab.open"}}` + "\n"
	resp, err := readReply(strings.NewReader(stream), opTabOpen)
	if err != nil {
		t.Fatalf("readReply: %v", err)
	}
	_, err = resp.result()
	if err == nil {
		t.Fatalf("an ok=false envelope was not an error")
	}
	if err.Error() != "no such op: tab.open (unknown-op)" {
		t.Errorf("error = %q", err)
	}

	t.Run("ok=false with no body", func(t *testing.T) {
		resp, err := readReply(strings.NewReader(`{"id":"1","ok":false}`+"\n"), opTabOpen)
		if err != nil {
			t.Fatalf("readReply: %v", err)
		}
		if _, err := resp.result(); err == nil || !strings.Contains(err.Error(), "no error body") {
			t.Errorf("error = %v", err)
		}
	})
}

// TestVendoredVectorsDecode reads roost's own published replies through this
// package's result decoders — the shapes come from roost, not from a doc page.
//
// The envelope is unwrapped with a local helper rather than with readReply,
// because each vector carries the correlation id of whatever request roost
// recorded it against (4 for tab.list, 2 for tab.open) and readReply matches a
// single-request connection's id. Rewriting the vectors' ids to make them fit
// would be asserting against edited bytes; readReply's own id matching is
// covered above, on streams this file writes.
func TestVendoredVectorsDecode(t *testing.T) {
	t.Run("session.identify", func(t *testing.T) {
		var identify IdentifyResult
		decodeVectorResult(t, "session.identify.response.v4.json", &identify)
		if identify.SessionProtocol != SpokenProtocol {
			t.Errorf("session_protocol = %d", identify.SessionProtocol)
		}
	})

	t.Run("tab.list", func(t *testing.T) {
		var list TabListResult
		decodeVectorResult(t, "tab.list.session.response.json", &list)
		if len(list.Projects) != 1 {
			t.Fatalf("projects = %+v", list.Projects)
		}
		want := Project{ID: "1", Name: "Roost", Cwd: "/Users/me/projects/roost"}
		if list.Projects[0] != want {
			t.Errorf("project = %+v, want %+v", list.Projects[0], want)
		}
	})

	t.Run("tab.open", func(t *testing.T) {
		var opened TabOpenResult
		decodeVectorResult(t, "tab.open.response.json", &opened)
		if opened.Tab.ID != "5" {
			t.Errorf("tab id = %q", opened.Tab.ID)
		}
	})
}

// decodeVectorResult unwraps a vendored reply's `result` and decodes it.
func decodeVectorResult(t *testing.T, name string, into any) {
	t.Helper()
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	readVectorJSON(t, name, &envelope)
	if !envelope.OK {
		t.Fatalf("%s is not an ok envelope", name)
	}
	if err := json.Unmarshal(envelope.Result, into); err != nil {
		t.Fatalf("decoding %s's result: %v", name, err)
	}
}

// TestVendoredVectorIdsAreStrings guards the assumption idMatches is built on:
// roost really does put a quoted id on the wire, and the vectors are roost's
// own bytes.
func TestVendoredVectorIdsAreStrings(t *testing.T) {
	for _, name := range []string{
		"session.identify.response.v4.json",
		"tab.list.session.response.json",
		"tab.open.response.json",
	} {
		if !bytes.Contains(readVector(t, name), []byte(`"id": "`)) {
			t.Errorf("%s does not carry a string id", name)
		}
	}
}
