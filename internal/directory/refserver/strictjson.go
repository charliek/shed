package refserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// checkStrictJSON checks that raw is exactly one JSON value, in valid UTF-8,
// with no member named twice in any object, nothing after it but whitespace,
// and nesting no deeper than maxDepth (each object and each array a level,
// the outermost level 1). The standard decoder takes the last of two
// same-named members and replaces invalid UTF-8; the contract refuses both.
func checkStrictJSON(raw []byte, maxDepth int) error {
	if !utf8.Valid(raw) {
		return errors.New("not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		object  bool
		keys    map[string]bool
		wantKey bool
	}
	var stack []*frame
	started := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if !started || len(stack) != 0 {
				return errors.New("incomplete JSON value")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if started && len(stack) == 0 {
			return errors.New("data after the JSON value")
		}
		started = true

		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if top != nil && top.object && top.wantKey {
			if name, ok := tok.(string); ok {
				if top.keys[name] {
					return fmt.Errorf("the member %q is named twice", name)
				}
				top.keys[name] = true
				top.wantKey = false
				continue
			}
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				if len(stack)+1 > maxDepth {
					return fmt.Errorf("nested deeper than %d", maxDepth)
				}
				if top != nil && top.object {
					top.wantKey = true
				}
				stack = append(stack, &frame{object: t == '{', keys: map[string]bool{}, wantKey: t == '{'})
			default:
				stack = stack[:len(stack)-1]
			}
		default:
			if top != nil && top.object {
				top.wantKey = true
			}
		}
	}
}

// objectMembers decodes a JSON object (already checked by checkStrictJSON)
// into its members' raw values.
func objectMembers(raw []byte) (map[string]json.RawMessage, error) {
	if t := bytes.TrimLeft(raw, " \t\r\n"); len(t) == 0 || t[0] != '{' {
		return nil, errors.New("not a JSON object")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// jsonString decodes a raw member that must be a JSON string.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// maxNumericDate is the latest instant a numeric date may name:
// 9999-12-31T23:59:59Z. Every date the service compares is first held to
// 0..maxNumericDate, so no arithmetic on it or conversion of it to a time can
// overflow (time.Unix wraps silently near the int64 limits, which would turn
// a date absurdly far in the future into one far in the past).
const maxNumericDate = 253402300799

// numericDate decodes a raw member that must be a JSON integer naming an
// instant from the Unix epoch to the end of the year 9999, in seconds. The
// range is checked on the integer before anything converts it.
func numericDate(raw json.RawMessage) (int64, bool) {
	n, ok := jsonInt(raw)
	if !ok || n < 0 || n > maxNumericDate {
		return 0, false
	}
	return n, true
}

// decodeB64URL decodes one segment of unpadded base64url (a JWS part, a JWK
// coordinate). It accepts only the alphabet's 64 characters, and only text
// that encodes its bytes in the one canonical way: the standard decoder
// skips carriage returns and line feeds even in strict mode, which would
// let two texts stand for one value.
func decodeB64URL(s string) ([]byte, error) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return nil, fmt.Errorf("byte %d is not of the base64url alphabet", i)
		}
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, err
	}
	if base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, errors.New("not the canonical base64url of its bytes")
	}
	return b, nil
}

// jsonInt decodes a raw member that must be a JSON integer (no fraction, no
// exponent) that fits in an int64.
func jsonInt(raw json.RawMessage) (int64, bool) {
	// The standard decoder also stores a quoted numeral in a json.Number,
	// so a string is refused here, by its first byte.
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) || bytes.ContainsAny(raw, ".eE") {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	v, err := n.Int64()
	return v, err == nil
}

// unpairedSurrogate reports whether raw, a document checkStrictJSON took,
// holds a \u escape of a UTF-16 surrogate that is not one half of a
// high-then-low pair. Go's decoder would replace one with U+FFFD; the
// contracts refuse it, never repair it.
func unpairedSurrogate(raw []byte) bool {
	hex := func(i int) (rune, bool) {
		if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
			return 0, false
		}
		var r rune
		for _, c := range raw[i+2 : i+6] {
			switch {
			case c >= '0' && c <= '9':
				r = r<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				r = r<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				r = r<<4 | rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return r, true
	}
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			i++
			continue
		}
		r, ok := hex(i)
		if !ok {
			i += 2 // another escape, \\ among them
			continue
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			lo, ok := hex(i + 6)
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return true
			}
			i += 12
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		default:
			i += 6
		}
	}
	return false
}
