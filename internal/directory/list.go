package directory

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
	gossh "golang.org/x/crypto/ssh"
)

// ContractVersion is the contract this client speaks.
const ContractVersion = 1

var (
	// organizationPattern is rule 5's form of an organization.
	organizationPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	// versionPattern is rule 6's form of a list's version.
	versionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidOrganization reports whether s has rule 5's form.
func ValidOrganization(s string) bool { return organizationPattern.MatchString(s) }

// Errors a refused answer wraps, so a caller can tell them apart in its log.
var (
	// ErrContract is an answer naming another contract number, or none.
	ErrContract = errors.New("the answer is not contract 1")
	// ErrOrganization is an answer naming another organization than the
	// one the client accepts, or none.
	ErrOrganization = errors.New("the answer names another organization")
)

// SkipReason is why one entry of a list was skipped: the first of rule 7's
// checks, in their fixed order, that the entry fails.
type SkipReason string

// Rule 7's reasons, in the order its checks run.
const (
	SkipDuplicateMember SkipReason = "duplicate-member"
	SkipMalformed       SkipReason = "malformed"
	SkipInvalidName     SkipReason = "invalid-name"
	SkipDuplicateName   SkipReason = "duplicate-name"
	SkipDuplicateKey    SkipReason = "duplicate-key"
	SkipNoUsableKey     SkipReason = "no-usable-key"
)

// List is a directory answer that rule 6 accepted, decoded entry by entry
// (rule 7). Users are the entries that passed, sorted by name; every other
// entry is in Skipped. Only the contract's own rules and shed's per-entry
// rules (a user name, a key line, a label, an expiry) have run: a client's
// further rules (a local admin's name or key, a service client's name) are
// its lookup's to apply. Nothing here depends on the clock: an expired key
// is in its user's Keys.
type List struct {
	// Organization is the organization the answer named (the accepted one).
	Organization string
	// Version is the list's version: 64 lowercase hex digits.
	Version string
	// Users are the users admitted, sorted by name.
	Users []User
	// Skipped are the entries not admitted, by their index in the answer.
	Skipped []Skipped
}

// User is one user of a list.
type User struct {
	// Name is the user's name: a valid shed user name, unique in the list.
	Name string
	// Admin is the list's admin flag.
	Admin bool
	// Keys are the keys this client uses, in the answer's order: never
	// empty, each listed nowhere else in the answer.
	Keys []Key
	// Fingerprints are the identities (gossh.FingerprintSHA256) of every
	// key the entry lists that rule 7 counts, in the answer's order: the
	// keys in Keys and those not used (an empty or shared label, an expiry
	// that is not an RFC 3339 date-time). A client's own rule on keys (a
	// list entry that lists a key a local admin holds is skipped whole) is
	// decided on these, never on Keys alone.
	Fingerprints []string
}

// Key is one key of a user, as this client uses it.
type Key struct {
	// PublicKey is the parsed key.
	PublicKey gossh.PublicKey
	// Fingerprint is the key's identity: gossh.FingerprintSHA256 of its
	// blob, never its text.
	Fingerprint string
	// Line is the key line as the service sent it.
	Line string
	// Canonical is the line re-marshalled: "<type> <base64 blob>".
	Canonical string
	// Label is the key's label: non-empty and unique within its user. It is
	// the service's text: quote it when printing.
	Label string
	// Expires is when the key stops opening connections; nil means never.
	Expires *time.Time
}

// Skipped is one entry the decoder did not admit.
type Skipped struct {
	// Entry is the entry's index in the answer's users, from 0.
	Entry int
	// Name is the entry's name when it had one as a string (perhaps not a
	// valid name); "" otherwise. The service's text: quote it when printing.
	Name string
	// Reason is the first of rule 7's checks the entry failed.
	Reason SkipReason
}

// DecodeList decodes a 200 answer of GET /v1/directory. organization is the
// organization the client accepts (rule 5: the configured one, or the one
// its enrolment answer named) and must be set. Any failure of rule 6 or rule
// 5 refuses the whole answer (a failed refresh); a bad entry only skips that
// entry (rule 7). Size (rule 1) is the reader's to enforce.
func DecodeList(body []byte, organization string) (*List, error) {
	if !ValidOrganization(organization) {
		return nil, fmt.Errorf("the accepted organization %q is not one", organization)
	}
	top, err := parseDocument(body)
	if err != nil {
		return nil, err
	}
	if top.duplicate() {
		return nil, errors.New("a member is named twice at the top level")
	}
	// contract: present and written as the integer 1. Another spelling of
	// one (1.0, 1e0) is refused: refusing keeps the copy the client has.
	if v, ok := top.one("contract"); !ok || string(v) != "1" {
		return nil, ErrContract
	}
	org, ok := top.one("organization")
	orgName, isString := rawString(org)
	if !ok || !isString || !ValidOrganization(orgName) {
		return nil, fmt.Errorf("%w: organization is not one", ErrOrganization)
	}
	if orgName != organization {
		return nil, fmt.Errorf("%w: %q, not %q", ErrOrganization, orgName, organization)
	}
	v, _ := top.one("version")
	version, isString := rawString(v)
	if !isString || !versionPattern.MatchString(version) {
		return nil, errors.New("version is not 64 lowercase hex digits")
	}
	u, _ := top.one("users")
	raws, ok := parseArray(u)
	if !ok {
		return nil, errors.New("users is not an array")
	}

	list := &List{Organization: orgName, Version: version, Users: []User{}, Skipped: []Skipped{}}
	entries := make([]*entry, len(raws))
	names := map[string]int{}
	fingerprints := map[string]int{}
	for i, raw := range raws {
		e := scanEntry(raw)
		entries[i] = e
		if e.nameCounted {
			names[e.name]++
		}
		for _, k := range e.keys {
			if k.counted {
				fingerprints[k.fingerprint]++
			}
		}
	}
	for i, e := range entries {
		reason := e.reason(names, fingerprints)
		if reason != "" {
			list.Skipped = append(list.Skipped, Skipped{Entry: i, Name: e.name, Reason: reason})
			continue
		}
		user := User{Name: e.name, Admin: e.admin}
		for _, k := range e.keys {
			if k.usable {
				user.Keys = append(user.Keys, k.key)
			}
			if k.counted {
				user.Fingerprints = append(user.Fingerprints, k.fingerprint)
			}
		}
		list.Users = append(list.Users, user)
	}
	sort.Slice(list.Users, func(i, j int) bool { return list.Users[i].Name < list.Users[j].Name })
	return list, nil
}

// entry is what one element of users holds, as rule 7 reads it.
type entry struct {
	duplicateMember bool
	malformed       bool
	// name is the entry's name when it is a string named once; nameCounted
	// says so (rule 7's counting).
	name        string
	nameCounted bool
	admin       bool
	keys        []entryKey
}

// entryKey is one element of an entry's keys.
type entryKey struct {
	// counted: key is a string named once, in a keys array named once, that
	// config.ParseUserKey accepts (used or not); fingerprint is then its
	// identity.
	counted     bool
	fingerprint string
	// usable: counted, with a label as rule 9 says and expires absent or an
	// RFC 3339 date-time; key is then the key as the client uses it.
	usable bool
	key    Key
	label  string
}

// scanEntry reads one entry: whether it has rule 7's structure, and the
// name and keys that count, whatever else is wrong with it.
func scanEntry(raw []byte) *entry {
	e := &entry{}
	obj, ok := parseObject(raw)
	if !ok {
		e.malformed = true
		return e
	}
	e.duplicateMember = obj.duplicate()

	nameRaw, once := obj.one("name")
	if name, isString := rawString(nameRaw); once && isString {
		e.name, e.nameCounted = name, true
	} else {
		e.malformed = true
	}
	adminRaw, ok := obj.one("admin")
	admin, isBool := rawBool(adminRaw)
	if !ok || !isBool {
		e.malformed = true
	}
	e.admin = admin

	keysRaw, ok := obj.one("keys")
	elems, isArray := parseArray(keysRaw)
	if !ok || !isArray {
		e.malformed = true
		return e
	}
	labels := map[string]int{}
	for _, el := range elems {
		k, ko := scanKey(el)
		if ko == nil {
			e.malformed = true
			continue
		}
		if ko.duplicate() {
			e.duplicateMember = true
		}
		if !k.wellFormed {
			e.malformed = true
		}
		labels[k.label]++
		e.keys = append(e.keys, k.entryKey)
	}
	// Rule 9: a label shared within the user leaves none of its keys used.
	for i := range e.keys {
		if e.keys[i].usable && labels[e.keys[i].label] > 1 {
			e.keys[i].usable = false
		}
	}
	return e
}

// scannedKey is an entryKey with its own structure check.
type scannedKey struct {
	entryKey
	wellFormed bool
}

// scanKey reads one element of keys. It returns the element's members (nil
// when it is not an object).
func scanKey(raw []byte) (scannedKey, jsonObject) {
	var k scannedKey
	obj, ok := parseObject(raw)
	if !ok {
		return k, nil
	}
	lineRaw, lineOnce := obj.one("key")
	line, lineIsString := rawString(lineRaw)
	labelRaw, labelOnce := obj.one("label")
	label, labelIsString := rawString(labelRaw)
	hasExpires := obj.count("expires") > 0
	expiresRaw, expiresOnce := obj.one("expires")
	expiresText, expiresIsString := rawString(expiresRaw)
	k.wellFormed = lineOnce && lineIsString && labelOnce && labelIsString &&
		(!hasExpires || expiresOnce && expiresIsString)
	k.label = label

	// Counted when key is a string named once that the parser accepts,
	// whatever else is wrong with the element.
	if !lineOnce || !lineIsString {
		return k, obj
	}
	pk, err := config.ParseUserKey(line)
	if err != nil {
		return k, obj
	}
	k.counted = true
	k.fingerprint = gossh.FingerprintSHA256(pk)

	// Used when its label is a non-empty string (uniqueness is checked over
	// the whole user) and expires is absent or an RFC 3339 date-time
	// (parseDateTime). Whether it is past is not asked here.
	usable := k.wellFormed && label != ""
	var expires *time.Time
	if usable && hasExpires {
		if t, ok := parseDateTime(expiresText); ok {
			expires = &t
		} else {
			usable = false
		}
	}
	k.usable = usable
	k.key = Key{
		PublicKey:   pk,
		Fingerprint: k.fingerprint,
		Line:        line,
		Canonical:   strings.TrimSuffix(string(gossh.MarshalAuthorizedKey(pk)), "\n"),
		Label:       label,
		Expires:     expires,
	}
	return k, obj
}

// parseDateTime reads s as an RFC 3339 date-time (section 5.6) and nothing
// else: YYYY-MM-DD, an upper-case T, hh:mm:ss, an optional fraction (a dot
// and one or more digits), then an upper-case Z or a numeric offset
// [+-]hh:mm. Every field has its exact number of digits and is within its
// range: the month 01-12, the day within its month (leap years counted), the
// hour 00-23, the minute and second 00-59, the offset's hour 00-23 and
// minute 00-59. Lower-case t and z are refused. A leap second (60) is
// refused: this client cannot represent it, and a service never sends one.
// An offset of -00:00 is accepted (the instant in UTC, its local offset
// unknown). Digits of a fraction past the ninth are dropped.
//
// time.Parse is not used: it refuses some of these and accepts other
// forms (a one-digit hour, an offset of +24:00, a comma before the
// fraction).
func parseDateTime(s string) (time.Time, bool) {
	if len(s) < len("2006-01-02T15:04:05Z") ||
		s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' {
		return time.Time{}, false
	}
	year, okY := digits(s[0:4])
	month, okMo := digits(s[5:7])
	day, okD := digits(s[8:10])
	hour, okH := digits(s[11:13])
	minute, okMi := digits(s[14:16])
	sec, okS := digits(s[17:19])
	if !okY || !okMo || !okD || !okH || !okMi || !okS ||
		month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || sec > 59 {
		return time.Time{}, false
	}
	if day > time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day() {
		return time.Time{}, false
	}
	rest := s[19:]
	nanos := 0
	if strings.HasPrefix(rest, ".") {
		n := 1
		for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
			n++
		}
		if n == 1 {
			return time.Time{}, false
		}
		frac := (rest[1:n] + "000000000")[:9]
		nanos, _ = digits(frac)
		rest = rest[n:]
	}
	loc := time.UTC
	switch {
	case rest == "Z":
	case len(rest) == 6 && (rest[0] == '+' || rest[0] == '-') && rest[3] == ':':
		oh, okOH := digits(rest[1:3])
		om, okOM := digits(rest[4:6])
		if !okOH || !okOM || oh > 23 || om > 59 {
			return time.Time{}, false
		}
		offset := oh*3600 + om*60
		if rest[0] == '-' {
			offset = -offset
		}
		loc = time.FixedZone("", offset)
	default:
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), day, hour, minute, sec, nanos, loc), true
}

// digits is s, ASCII digits only, as a number; ok false when s is empty or
// holds anything else.
func digits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// reason is the first of rule 7's checks e fails, or "" when it passes them
// all. names and fingerprints count each name and key over every entry.
func (e *entry) reason(names, fingerprints map[string]int) SkipReason {
	switch {
	case e.duplicateMember:
		return SkipDuplicateMember
	case e.malformed:
		return SkipMalformed
	case !config.ValidUserName(e.name):
		return SkipInvalidName
	case names[e.name] > 1:
		return SkipDuplicateName
	}
	usable := false
	for _, k := range e.keys {
		if k.counted && fingerprints[k.fingerprint] > 1 {
			return SkipDuplicateKey
		}
		usable = usable || k.usable
	}
	if !usable {
		return SkipNoUsableKey
	}
	return ""
}
