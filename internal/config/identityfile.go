package config

// identityfile.go is a server entry's identity_file (plan 037 §3.3): the SSH
// key an entry uses. With it set, three things hold, and the docs state them
// in these words (docs/reference/cli.md, "One SSH key per entry"):
//
//   - P-a. The entry's API identity is that key and no other: a certificate
//     issued for the entry is accepted only when it was made for that key
//     (IssuedCertificateKey compares the certificate's subject, the SSH key
//     fingerprint the server enrolled, with the identity file's public key),
//     and a stored certificate is presented only when the same comparison
//     holds at its load (checkEntryCertificate); one made for another key is
//     never presented, and the entry enrols again with its identity file. An
//     enrolment is adopted only while the entry still names the same file
//     and the same key is in it (cmd/shed enrolledIdentityHolds), and a
//     control token, stored or issued, is never used (it names no key). These
//     are checks at those points, not a continuous binding: a certificate a
//     process already loaded stays in use until it next loads or renews.
//   - P-b. The CLI's own ssh for the entry never rides a connection another
//     key opened: each argv carries `-o ControlMaster=no -o ControlPath=none`
//     (the roost transport keeps its own shared connection, keyed by the
//     identity file and the key learned from it), and so does each generated
//     ssh config stanza.
//   - P-c. ssh offers that key first and no key that is only in the agent
//     (`-i <path> -o IdentitiesOnly=yes`). Other IdentityFile lines the
//     person's own ssh config names for the host can still be offered after
//     it: OpenSSH has no switch that ignores them, so the docs say to keep no
//     other IdentityFile for that host when the entry must use one key for
//     shell access too.
//
// The value has one rule wherever it comes from (the --identity-file flag, a
// hand-edited config.yaml): sdk/bootstrap.ValidateIdentityFile, the same rule
// the bootstrap SDK applies to Params.IdentityFile and the Rust readers mirror.
// The file itself is checked again at every use, before ssh is spawned: it
// must be a readable regular file (sdk/bootstrap.OpenIdentityFile, which never
// blocks and checks what it opened), and its public key must be learnable
// without a passphrase (IdentityPublicKey), so a key removed after it was set
// fails with its own message rather than as an ssh that refused every key.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// IdentityFileError is an entry's identity_file that cannot be used: its value
// breaks the rule, or the file is missing, not a regular file, unreadable, or
// holds no public key that can be learned without a passphrase. It is returned
// before any ssh is spawned for the entry.
type IdentityFileError struct {
	// Entry is the entry's config name ("" when the caller has none, as for
	// the value of `shed server add --identity-file` before the entry is
	// named).
	Entry string
	Path  string
	// Reason says why, in words that follow "cannot be used: ". It is shown
	// with any control character escaped (sdkbootstrap.SafeText).
	Reason string
}

// EntryRefusal marks the error as a clienttoken.Refusal: an entry whose
// identity file cannot be used may not be used at all, wherever that is found
// (at client build, at an enrolment, at a renewal), and every caller that
// tells a refusal from an unreachable server tells this one too.
func (e *IdentityFileError) EntryRefusal() {}

// InvalidatesHeld reports true: when the entry's key cannot be established,
// neither can that a certificate already held was made for it, so a source
// that meets this at a renewal ends at once and presents nothing more, however
// valid what it holds still is.
func (e *IdentityFileError) InvalidatesHeld() bool { return true }

func (e *IdentityFileError) Error() string {
	reason := sdkbootstrap.SafeText(e.Reason)
	if e.Entry == "" {
		return fmt.Sprintf("identity_file %q cannot be used: %s", e.Path, reason)
	}
	return fmt.Sprintf("entry %q: identity_file %q cannot be used: %s", e.Entry, e.Path, reason)
}

// NameIdentityFileError turns an identity-file error from any layer into the
// one typed refusal, named for entry: the SDK's *sdkbootstrap.IdentityFileError
// (found by type, wherever it is wrapped, never by the words of the message,
// which can hold a path), or this package's own. It returns nil, false when
// err is neither.
func NameIdentityFileError(entry string, err error) (*IdentityFileError, bool) {
	var own *IdentityFileError
	if errors.As(err, &own) {
		named := *own
		if named.Entry == "" {
			named.Entry = entry
		}
		return &named, true
	}
	var sdkErr *sdkbootstrap.IdentityFileError
	if errors.As(err, &sdkErr) {
		return &IdentityFileError{Entry: entry, Path: sdkErr.Path, Reason: sdkErr.Reason}, true
	}
	return nil, false
}

// identityFileError names entry on an error about the identity file path.
func identityFileError(entry, path string, err error) error {
	if named, ok := NameIdentityFileError(entry, err); ok {
		return named
	}
	return &IdentityFileError{Entry: entry, Path: path, Reason: err.Error()}
}

// maxIdentityFileBytes bounds what is read of an identity file (or the .pub
// beside it) to learn its public key. A real key is a few kilobytes.
const maxIdentityFileBytes = 1 << 20

// readIdentityFile reads a key file through sdkbootstrap.OpenIdentityFile (no
// blocking open, a regular file only), at most maxIdentityFileBytes.
func readIdentityFile(path string) ([]byte, error) {
	f, err := sdkbootstrap.OpenIdentityFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(f, maxIdentityFileBytes+1))
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err // its own words, without the path
		}
		return nil, fmt.Errorf("the file cannot be read (%s)", sdkbootstrap.SafeText(err.Error()))
	}
	if len(data) > maxIdentityFileBytes {
		return nil, errors.New("the file is larger than any key file")
	}
	return data, nil
}

// IdentityPublicKey learns the public key of the identity file at path without
// a passphrase, the way ssh itself does for `-i`:
//
//   - a public key file (`ssh-ed25519 AAAA…`) is that key;
//   - a private key whose format carries the public key unencrypted (the
//     OpenSSH format, encrypted or not) or that is not encrypted at all gives
//     its own public key;
//   - otherwise (a legacy PEM key that is encrypted, or a key type this
//     reader does not parse) the `<path>.pub` beside it is read.
//
// When the private file gives a key and a readable `<path>.pub` names another,
// it is refused: which key ssh would offer is not certain. A file whose public
// key cannot be learned either way is refused, with what is needed. The error
// is an *IdentityFileError named for entry. The file is checked as
// sdkbootstrap.CheckIdentityFile checks it.
func IdentityPublicKey(entry, path string) (ssh.PublicKey, error) {
	refuse := func(reason string) error { return &IdentityFileError{Entry: entry, Path: path, Reason: reason} }
	data, err := readIdentityFile(path)
	if err != nil {
		return nil, identityFileError(entry, path, err)
	}
	if pub, _, _, _, err := ssh.ParseAuthorizedKey(data); err == nil {
		return pub, nil // a public key file
	}

	var own ssh.PublicKey
	var ownErr error
	if !bytes.Contains(data, []byte("PRIVATE KEY")) {
		ownErr = errors.New("it is neither a public key nor a private key")
	} else if signer, err := ssh.ParsePrivateKey(data); err == nil {
		own = signer.PublicKey()
	} else {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) && missing.PublicKey != nil {
			own = missing.PublicKey
		} else if errors.As(err, &missing) {
			ownErr = errors.New("it is encrypted and its format does not carry the public key")
		} else {
			ownErr = errors.New("its format is not one this reader parses")
		}
	}

	pubPath := path + ".pub"
	beside, besideErr := readIdentityFile(pubPath)
	var besideKey ssh.PublicKey
	switch {
	case besideErr == nil:
		if k, _, _, _, err := ssh.ParseAuthorizedKey(beside); err == nil {
			besideKey = k
		} else {
			besideErr = errors.New("it holds no public key")
		}
	case isNotExistIdentity(besideErr):
		besideErr = nil // no .pub: nothing to compare, nothing to fall back on
	}

	switch {
	case own != nil && besideKey != nil:
		if !bytes.Equal(own.Marshal(), besideKey.Marshal()) {
			return nil, refuse(fmt.Sprintf("its public key (%s) is not the one in %s (%s), so which key ssh offers is not certain; "+
				"make the .pub file match the key, or remove it", ssh.FingerprintSHA256(own), pubPath, ssh.FingerprintSHA256(besideKey)))
		}
		return own, nil
	case own != nil:
		return own, nil
	case besideKey != nil:
		return besideKey, nil
	}
	// The entry's API identity is checked against this public key (P-a), so a
	// key whose public half cannot be learned without its passphrase cannot be
	// used.
	reason := fmt.Sprintf("its public key cannot be learned without a passphrase (%s), and there is no %s beside it",
		ownErr, pubPath)
	if besideErr != nil {
		reason = fmt.Sprintf("its public key cannot be learned without a passphrase (%s), and %s cannot be used either (%s)",
			ownErr, pubPath, sdkbootstrap.SafeText(reasonOf(besideErr)))
	}
	return nil, refuse(reason + "; shed needs the public key to check that the server enrolled this key: " +
		"put the key's .pub file beside it, or use a key in the OpenSSH format (ssh-keygen -p -f <key> rewrites it)")
}

// isNotExistIdentity reports whether err is OpenIdentityFile's "does not exist".
func isNotExistIdentity(err error) bool {
	var sdkErr *sdkbootstrap.IdentityFileError
	return errors.As(err, &sdkErr) && sdkErr.Reason == "the file does not exist"
}

// reasonOf is an identity-file error's reason alone, or the error's text.
func reasonOf(err error) string {
	var sdkErr *sdkbootstrap.IdentityFileError
	if errors.As(err, &sdkErr) {
		return sdkErr.Reason
	}
	return err.Error()
}

// IdentityFileKey checks the identity_file path of the entry named entry at
// the time of use (its value, the file, its public key: IdentityPublicKey)
// and returns the SHA-256 fingerprint of that public key, in the form the
// server writes as an issued certificate's subject. "" with no error when path
// is "".
func IdentityFileKey(entry, path string) (string, error) {
	if path == "" {
		return "", nil
	}
	pub, err := IdentityPublicKey(entry, path)
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(pub), nil
}

// IdentityFileArgs is the ssh argv for the identity_file path of the entry
// named entry: nil when path is "" (no identity file: the argv is what it was
// without the field), else sdkbootstrap.IdentityArgs (`-i <path> -o
// IdentitiesOnly=yes -o ControlMaster=no -o ControlPath=none`) once the file
// passes IdentityFileKey now. Every Go site that runs ssh for an entry calls
// it (or ServerEntry.SSHIdentityArgs) right before it builds the argv.
func IdentityFileArgs(entry, path string) ([]string, error) {
	if _, err := IdentityFileKey(entry, path); err != nil {
		return nil, err
	}
	return sdkbootstrap.IdentityArgs(path), nil
}

// SSHIdentityArgs is IdentityFileArgs for this entry, named name in messages.
func (e *ServerEntry) SSHIdentityArgs(name string) ([]string, error) {
	return IdentityFileArgs(name, e.IdentityFile)
}

// CheckIdentityFileValue checks only the value of an identity_file (the rule,
// not the file): for a reader that must not touch the file system, or a
// value whose file is checked later, at its use.
func CheckIdentityFileValue(entry, path string) error {
	if path == "" {
		return nil
	}
	if err := sdkbootstrap.ValidateIdentityFile(path); err != nil {
		return identityFileError(entry, path, err)
	}
	return nil
}

// IdentityFileFromFlag turns the value of an --identity-file flag into the
// value to store: "" (exactly empty) clears the field and is returned as it
// is; a leading `~` or `~/` is replaced by the home directory (no other
// rewriting: the path is stored as typed); the result must then pass the rule,
// name a readable regular file now, and give its public key without a
// passphrase (IdentityFileKey). A value that is only whitespace is refused,
// never read as "clear". entry names the entry in the error.
func IdentityFileFromFlag(entry, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	path := value
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", &IdentityFileError{Entry: entry, Path: value,
				Reason: "~ cannot be expanded: the home directory is not known"}
		}
		path = strings.TrimRight(home, "/") + path[1:]
	}
	if _, err := IdentityFileKey(entry, path); err != nil {
		return "", err
	}
	return path, nil
}
