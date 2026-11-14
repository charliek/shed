package config

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// ShedIDLength is the length of a shed ID the server mints.
const ShedIDLength = 26

// shedIDEncoding is RFC 4648 base32 without padding. Its alphabet (A-Z, 2-7)
// lowercases to characters that are legal in a cloud label value and in an
// instance name.
var shedIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

var shedIDRegex = regexp.MustCompile(`^[a-z2-7]{26}$`)

// NewShedID returns a new permanent shed ID: 16 random bytes as 26 lowercase
// base32 characters. The server mints one per create; it is never taken from
// a request.
func NewShedID() (string, error) {
	return newShedID(rand.Reader)
}

func newShedID(r io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("generating shed ID: %w", err)
	}
	return strings.ToLower(shedIDEncoding.EncodeToString(b[:])), nil
}

// ValidShedID reports whether s has the form NewShedID mints.
func ValidShedID(s string) bool { return shedIDRegex.MatchString(s) }
