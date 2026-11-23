package delivery

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

// nodeKind is the JSON type of a node.
type nodeKind int

const (
	kindObject nodeKind = iota
	kindArray
	kindString
	kindNumber
	kindBool
)

func (k nodeKind) String() string {
	switch k {
	case kindObject:
		return "object"
	case kindArray:
		return "array"
	case kindString:
		return "string"
	case kindNumber:
		return "number"
	}
	return "boolean"
}

// node is one JSON value of a strictly parsed document. A number keeps its
// literal text: the contract compares numbers as written (1, never 1.0).
type node struct {
	kind    nodeKind
	text    string // a string's value, or a number's literal
	b       bool
	members []member // an object's members, in document order
	elems   []*node  // an array's elements
}

type member struct {
	name string
	val  *node
}

// get returns the member named name, or nil.
func (n *node) get(name string) *node {
	for _, m := range n.members {
		if m.name == name {
			return m.val
		}
	}
	return nil
}

// parseDocument parses body as one JSON object under the contract's
// conventions. In order: body must be valid UTF-8 holding exactly one JSON
// value that is an object, with nothing after it but whitespace, nested no
// deeper than MaxNesting, and with no string escaping an unpaired UTF-16
// surrogate (not_json); then the first member named twice in one object
// (duplicate_member) or null (null_value), whichever comes first in the
// text. Member names compare after unescaping.
func parseDocument(body []byte) (*node, *Refusal) {
	if !utf8.Valid(body) {
		return nil, refuse(RuleNotJSON, "")
	}
	root, first, err := buildTree(body)
	if err != nil || unpairedSurrogate(body) {
		return nil, refuse(RuleNotJSON, "")
	}
	if root.kind != kindObject {
		return nil, refuse(RuleNotJSON, "")
	}
	if first != nil {
		return nil, first
	}
	return root, nil
}

// frame is an open object or array while the tree is built.
type frame struct {
	n       *node
	key     string // the member name waiting for its value
	haveKey bool
	seen    map[string]bool
	where   string
}

// buildTree builds the tree of body. It returns a syntax error (not_json),
// or the tree and the first duplicate_member or null_value refusal in
// document order (nil when there is none).
func buildTree(body []byte) (*node, *Refusal, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var stack []*frame
	var root *node
	var first *Refusal
	note := func(r *Refusal) {
		if first == nil {
			first = r
		}
	}
	// attach places v in the innermost open container, or makes it the root.
	attach := func(v *node) (string, error) {
		if len(stack) == 0 {
			if root != nil {
				return "", errors.New("data after the JSON value")
			}
			root = v
			return "", nil
		}
		top := stack[len(stack)-1]
		switch top.n.kind {
		case kindObject:
			where := join(top.where, safeName(top.key))
			top.n.members = append(top.n.members, member{name: top.key, val: v})
			top.haveKey = false
			return where, nil
		default:
			where := index(top.where, len(top.n.elems))
			top.n.elems = append(top.n.elems, v)
			return where, nil
		}
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if len(stack) == 0 && root != nil {
			return nil, nil, errors.New("data after the JSON value")
		}
		// An object waiting for a member name: this token is the name.
		if len(stack) > 0 {
			top := stack[len(stack)-1]
			if top.n.kind == kindObject && !top.haveKey {
				if d, ok := tok.(json.Delim); ok && d == '}' {
					stack = stack[:len(stack)-1]
					continue
				}
				name, ok := tok.(string)
				if !ok {
					return nil, nil, errors.New("object member name is not a string")
				}
				if top.seen[name] {
					note(refuse(RuleDuplicateMember, join(top.where, safeName(name))))
				}
				top.seen[name] = true
				top.key, top.haveKey = name, true
				continue
			}
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				if len(stack)+1 > MaxNesting {
					return nil, nil, errors.New("nested too deep")
				}
				n := &node{kind: kindObject}
				if t == '[' {
					n = &node{kind: kindArray}
				}
				where, err := attach(n)
				if err != nil {
					return nil, nil, err
				}
				stack = append(stack, &frame{n: n, seen: map[string]bool{}, where: where})
			case ']':
				stack = stack[:len(stack)-1]
			}
		case nil:
			where, err := attach(&node{kind: kindString})
			if err != nil {
				return nil, nil, err
			}
			note(refuse(RuleNullValue, where))
		case string:
			if _, err := attach(&node{kind: kindString, text: t}); err != nil {
				return nil, nil, err
			}
		case json.Number:
			if _, err := attach(&node{kind: kindNumber, text: string(t)}); err != nil {
				return nil, nil, err
			}
		case bool:
			if _, err := attach(&node{kind: kindBool, b: t}); err != nil {
				return nil, nil, err
			}
		}
	}
	if root == nil || len(stack) > 0 {
		return nil, nil, errors.New("no whole JSON value")
	}
	return root, first, nil
}

// knownNames are the member names the contract defines. A refusal's
// location names only these: any other name is a source's own text, which
// could be anything, so it is written *.
var knownNames = func() map[string]bool {
	m := map[string]bool{}
	for _, list := range [][]string{answerMembers, itemAll, problemMembers, requestMembers, shedMembers, repoMembers,
		recordMembers, StepNames, stepMembers, lastMembers, failureMembers, sourceMembers, rItemMembers} {
		for _, n := range list {
			m[n] = true
		}
	}
	return m
}()

// safeName is name when the contract defines it, else *.
func safeName(name string) string {
	if knownNames[name] {
		return name
	}
	return "*"
}

// join is where.name, or name at the top.
func join(where, name string) string {
	if where == "" {
		return name
	}
	return where + "." + name
}

// index is where[i].
func index(where string, i int) string {
	return where + "[" + strconv.Itoa(i) + "]"
}

// unpairedSurrogate reports whether a string in body, which is valid JSON,
// holds a \u escape of a UTF-16 surrogate that is not one half of a pair (a
// high surrogate followed at once by an escaped low one). encoding/json
// would replace one silently; the contract refuses it.
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
		case i+1 >= len(body):
			return false
		case body[i+1] != 'u':
			i++ // a one-character escape: skip the character escaped
		default:
			if i+6 > len(body) {
				return false
			}
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

// hex4 is four hexadecimal digits as a number.
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

// checkMembers checks an object's members against the names it may hold (unknown
// member), then the names it must hold (missing member), each in the order
// of the object's members and of required.
func checkMembers(n *node, where string, allowed, required []string) *Refusal {
	for _, m := range n.members {
		if !containsT(allowed, m.name) {
			return refuse(RuleUnknownMember, join(where, safeName(m.name)))
		}
	}
	for _, name := range required {
		if n.get(name) == nil {
			return refuse(RuleMissingMember, join(where, name))
		}
	}
	return nil
}

// checkTypes checks each member present of types (name to kind), in the
// order of names.
func checkTypes(n *node, where string, names []string, kinds map[string]nodeKind) *Refusal {
	for _, name := range names {
		v := n.get(name)
		if v != nil && v.kind != kinds[name] {
			return refuse(RuleWrongType, join(where, name))
		}
	}
	return nil
}
