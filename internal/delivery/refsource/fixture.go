package refsource

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// FixtureRequestID is the request every answer fixture answers: the id of
// its answer, which a live harness replaces with the live request's id
// wherever it appears in the body.
const FixtureRequestID = "0123456789abcdef0123456789abcdef"

// AnswerFixture is one file of fixtures/answers/: an answer, as a JSON value
// (Answer), as exact text (AnswerText) or as exact bytes (AnswerBase64), the
// contents the harness fills in (Fill), and what the server must do with it
// (Expect).
type AnswerFixture struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Request names the request fixture (fixtures/requests/<request>.json)
	// the answer is judged against: its id is FixtureRequestID, and its
	// shed's project directory places the base: project items.
	Request      string          `json:"request"`
	Answer       json.RawMessage `json:"answer,omitempty"`
	AnswerText   *string         `json:"answer_text,omitempty"`
	AnswerBase64 *string         `json:"answer_base64,omitempty"`
	Fill         []Fill          `json:"fill,omitempty"`
	Expect       Expect          `json:"expect"`
}

// Fill sets items[Item].content to the padded standard base64 of Bytes
// bytes, each 0x61 (a): a large content is generated, not stored.
type Fill struct {
	Item  int `json:"item"`
	Bytes int `json:"bytes"`
}

// Expect is what the server does with a fixture's answer: accepted, or
// refused with Status and Rule.
type Expect struct {
	Result string `json:"result"`
	Status int    `json:"status,omitempty"`
	Rule   string `json:"rule,omitempty"`
}

// LoadAnswerFixture reads and checks one answer fixture: exactly one of the
// three answer members, fill only with answer, and an expect that is
// accepted (nothing else) or refused (a status and a rule). No other member
// is part of the fixture language.
func LoadAnswerFixture(path string) (*AnswerFixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseAnswerFixture(raw)
}

// ParseAnswerFixture is LoadAnswerFixture on the file's bytes.
func ParseAnswerFixture(raw []byte) (*AnswerFixture, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f AnswerFixture
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("answer fixture: %w", err)
	}
	if f.Request == "" {
		return nil, errors.New("answer fixture: no request")
	}
	n := 0
	if f.Answer != nil {
		n++
	}
	if f.AnswerText != nil {
		n++
	}
	if f.AnswerBase64 != nil {
		n++
	}
	if n != 1 {
		return nil, errors.New("answer fixture: exactly one of answer, answer_text and answer_base64")
	}
	if len(f.Fill) > 0 && f.Answer == nil {
		return nil, errors.New("answer fixture: fill goes with answer only")
	}
	switch f.Expect.Result {
	case "accepted":
		if f.Expect.Status != 0 || f.Expect.Rule != "" {
			return nil, errors.New("answer fixture: an accepted fixture names no status or rule")
		}
	case "refused":
		if f.Expect.Status == 0 || f.Expect.Rule == "" {
			return nil, errors.New("answer fixture: a refused fixture names its status and rule")
		}
	default:
		return nil, fmt.Errorf("answer fixture: expect.result %q", f.Expect.Result)
	}
	return &f, nil
}

// Body is the fixture's answer as the bytes a source posts for requestID:
// the answer with its fill applied, FixtureRequestID replaced by requestID.
func (f *AnswerFixture) Body(requestID string) ([]byte, error) {
	var body []byte
	switch {
	case f.AnswerText != nil:
		body = []byte(*f.AnswerText)
	case f.AnswerBase64 != nil:
		b, err := base64.StdEncoding.DecodeString(*f.AnswerBase64)
		if err != nil {
			return nil, fmt.Errorf("answer_base64: %w", err)
		}
		body = b
	case len(f.Fill) == 0:
		body = append([]byte(nil), f.Answer...)
	default:
		b, err := fill(f.Answer, f.Fill)
		if err != nil {
			return nil, err
		}
		body = b
	}
	if requestID != FixtureRequestID {
		body = bytes.ReplaceAll(body, []byte(FixtureRequestID), []byte(requestID))
	}
	return body, nil
}

// fill decodes answer (numbers kept as written), sets each filled item's
// content and encodes it again.
func fill(answer json.RawMessage, fills []Fill) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(answer))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("answer: %w", err)
	}
	items, ok := doc["items"].([]any)
	if !ok {
		return nil, errors.New("fill: the answer has no items list")
	}
	for _, fl := range fills {
		if fl.Item < 0 || fl.Item >= len(items) || fl.Bytes < 0 {
			return nil, fmt.Errorf("fill: item %d of %d", fl.Item, len(items))
		}
		it, ok := items[fl.Item].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("fill: item %d is not an object", fl.Item)
		}
		it["content"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", fl.Bytes)))
	}
	return json.Marshal(doc)
}

// Transcript is one file of fixtures/stream/: a delivery stream as the
// server writes it, for a source to be tested against. The response's
// status and headers come first; then each chunk's text, written at at_ms
// milliseconds after the response; then the server ends the stream.
type Transcript struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Response    struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
	} `json:"response"`
	Chunks []Chunk `json:"chunks"`
	Expect struct {
		Requests []string `json:"requests"`
	} `json:"expect"`
}

// Chunk is a piece of a stream's body and when it is written.
type Chunk struct {
	AtMS int    `json:"at_ms"`
	Text string `json:"text"`
}

// LoadTranscript reads one stream transcript; no member outside the
// fixture language is allowed.
func LoadTranscript(path string) (*Transcript, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var t Transcript
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("transcript: %w", err)
	}
	return &t, nil
}
