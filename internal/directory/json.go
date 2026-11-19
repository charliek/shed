package directory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"
)

// MaxNesting is the deepest a directory answer may nest (rule 6): each
// object and each array is a level, the top-level object level 1.
const MaxNesting = 16

// checkJSON checks body is valid UTF-8 holding exactly one JSON value, with
// nothing after it but whitespace, nested no deeper than maxDepth, and with
// no string escaping half of a UTF-16 surrogate pair (\ud800 alone), which
// is not text either. Invalid UTF-8 and unpaired surrogates are refused,
// never repaired: encoding/json would replace both silently. Numbers are
// checked for their syntax only, never converted, so a number no float can
// hold is the business of whoever reads that member, not the document's.
func checkJSON(body []byte, maxDepth int) error {
	if !utf8.Valid(body) {
		return errors.New("not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	depth, started := 0, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if !started {
				return errors.New("no JSON value")
			}
			if unpairedSurrogate(body) {
				return errors.New("a string escapes an unpaired UTF-16 surrogate")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("not JSON: %w", err)
		}
		if started && depth == 0 {
			return errors.New("data after the JSON value")
		}
		started = true
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
				if depth > maxDepth {
					return fmt.Errorf("nested deeper than %d levels", maxDepth)
				}
			default:
				depth--
			}
		}
	}
}

// unpairedSurrogate reports whether a string in body, which is valid JSON,
// holds a \u escape of a UTF-16 surrogate that is not one half of a pair
// (a high surrogate followed at once by an escaped low one).
func unpairedSurrogate(body []byte) bool {
	inString := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case !inString:
			inString = c == '"'
		case c == '"':
			inString = false
		case c != '\\':
		case body[i+1] != 'u':
			i++ // a one-character escape: skip the character escaped
		default:
			r := hex4(body[i+2 : i+6])
			switch {
			case r >= 0xdc00 && r <= 0xdfff:
				return true // a low surrogate first
			case r >= 0xd800 && r <= 0xdbff:
				if i+12 > len(body) || body[i+6] != '\\' || body[i+7] != 'u' {
					return true
				}
				if lo := hex4(body[i+8 : i+12]); lo < 0xdc00 || lo > 0xdfff {
					return true
				}
				i += 11
			default:
				i += 5
			}
		}
	}
	return false
}

// hex4 is four hexadecimal digits (which valid JSON guarantees after \u)
// as a number.
func hex4(b []byte) rune {
	var r rune
	for _, c := range b {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		}
	}
	return r
}

// jsonMember is one member of a JSON object, in document order.
type jsonMember struct {
	name  string
	value json.RawMessage
}

// jsonObject is an object's members in document order, a member named twice
// kept twice.
type jsonObject []jsonMember

// duplicate reports whether any member is named twice. Names compare after
// unescaping, as JSON defines them.
func (o jsonObject) duplicate() bool {
	seen := make(map[string]bool, len(o))
	for _, m := range o {
		if seen[m.name] {
			return true
		}
		seen[m.name] = true
	}
	return false
}

// count is how many times name is a member.
func (o jsonObject) count(name string) int {
	n := 0
	for _, m := range o {
		if m.name == name {
			n++
		}
	}
	return n
}

// one returns name's value when it is a member exactly once.
func (o jsonObject) one(name string) (json.RawMessage, bool) {
	var v json.RawMessage
	n := 0
	for _, m := range o {
		if m.name == name {
			v = m.value
			n++
		}
	}
	return v, n == 1
}

// parseDocument checks body as checkJSON does (nested at most MaxNesting
// deep) and returns its members when it is one JSON object. A member named
// twice is kept twice: refusing it is the caller's.
func parseDocument(body []byte) (jsonObject, error) {
	if err := checkJSON(body, MaxNesting); err != nil {
		return nil, err
	}
	obj, ok := parseObject(body)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}

// parseObject returns raw's members when raw is a JSON object, ok false
// otherwise. raw has passed checkJSON (or is a value inside one that has).
func parseObject(raw []byte) (jsonObject, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var obj jsonObject
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		name, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		obj = append(obj, jsonMember{name: name, value: bytes.TrimSpace(v)})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	return obj, true
}

// parseArray returns raw's elements when raw is a JSON array, ok false
// otherwise.
func parseArray(raw []byte) ([]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, false
	}
	elems := []json.RawMessage{}
	for dec.More() {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		elems = append(elems, bytes.TrimSpace(v))
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		return nil, false
	}
	return elems, true
}

// rawString returns v as a string when it is a JSON string (never null).
func rawString(v json.RawMessage) (string, bool) {
	if len(v) == 0 || v[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", false
	}
	return s, true
}

// rawBool returns v as a boolean when it is true or false (never null).
func rawBool(v json.RawMessage) (b, ok bool) {
	switch string(v) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// rawPositiveInt returns v when it is written as a positive integer: digits
// only, no sign, fraction or exponent, no leading zero.
func rawPositiveInt(v json.RawMessage) (int64, bool) {
	if len(v) == 0 || len(v) > 18 || v[0] < '1' || v[0] > '9' {
		return 0, false
	}
	var n int64
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}

// decodeStrictObject decodes data into v. data must be one JSON object with
// nothing after it, naming no member twice and no member outside allowed
// (compared exactly: encoding/json alone would match names without regard
// to case).
func decodeStrictObject(data []byte, allowed []string, v any) error {
	obj, err := parseDocument(data)
	if err != nil {
		return err
	}
	if obj.duplicate() {
		return errors.New("a member is named twice")
	}
	for _, m := range obj {
		if !slices.Contains(allowed, m.name) {
			return fmt.Errorf("unknown member %q", m.name)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
