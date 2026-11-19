package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/charliek/shed/internal/directory"
)

var directoryEnrollName string

var directoryCmd = &cobra.Command{
	Use:   "directory",
	Short: "Take the server's users from an account service (auth.directory)",
}

var directoryEnrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "Enrol this server with the account service auth.directory names",
	Long: `Enrols this server, once, with the account service at auth.directory.url:
it makes an Ed25519 key pair, sends the public key with a one-time enrolment
code, and on the service's answer writes the key and the enrolment to
<state dir>/directory/enrolment.json (mode 0600, in a directory of mode 0700).
The key never leaves this host.

The code is read from standard input, never from the command line: at a
prompt that does not echo when standard input is a terminal, otherwise one
line, piped from the command that issued it or from a file only you can
read. Run this on the shed host as the user the server runs as, with the same
--config.

A server already enrolled is refused. To enrol again: stop the server, remove
enrolment.json, disable the old enrolment on the account service, and enrol
with a new code. When the service's answer is lost, nothing is written:
disable the server the service enrolled, issue a new code and run this again.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// An interrupt cancels ctx: the command stops where it waits (for
		// the code, or for the service's answer) and returns through its
		// normal path, which puts the terminal back and releases the lock.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		s := enrolStreams{in: os.Stdin, out: cmd.OutOrStdout(), errOut: cmd.ErrOrStderr()}
		return directoryEnroll(ctx, s, directoryEnrollName, cmd.Flags().Changed("name"))
	},
}

func init() {
	directoryEnrollCmd.Flags().StringVar(&directoryEnrollName, "name", "", "the name the account service shows for this server (default: the host name)")
	directoryCmd.AddCommand(directoryEnrollCmd)
	rootCmd.AddCommand(directoryCmd)
}

// enrolStreams are the command's standard input (the code), output (what
// it reports) and error (the prompt).
type enrolStreams struct {
	in     io.Reader
	out    io.Writer
	errOut io.Writer
}

// enrolLockName is the lock file in <state dir>/directory that one
// enrolment holds (flock) from its checks to its write.
const enrolLockName = "enrol.lock"

// maxCodeRead is the most the command reads of standard input: a code of
// the longest length and its newline.
const maxCodeRead = directory.MaxCodeBytes + 1

// maxEmptyReads is how many reads in a row may return nothing (no byte, no
// error) before the read of the code gives up.
const maxEmptyReads = 100

var (
	errCodeTooLong = fmt.Errorf("the enrolment code is longer than %d bytes; nothing was sent", directory.MaxCodeBytes)
	errInterrupted = errors.New("interrupted; nothing was sent")
	errNoInput     = errors.New("standard input gives nothing, again and again; nothing was sent")
	errNoCode      = errors.New("no enrolment code on standard input; nothing was sent")
)

const (
	// codeMarker replaces the enrolment code wherever it appears in text
	// the command prints that came from the service.
	codeMarker = "[the enrolment code]"
	// codeNotShown stands for a piece of the service's text that would
	// still show the code once it is replaced.
	codeNotShown = "(not shown: it holds the enrolment code)"
)

// Seams for tests.
var (
	// enrolTimeout bounds the enrolment request.
	enrolTimeout = 30 * time.Second
	// enrolHostname is the default --name.
	enrolHostname = os.Hostname
	// createEnrolmentFile writes enrolment.json, once, through the open
	// state directory; linked says whether the file was created.
	createEnrolmentFile = (*directory.StateDir).CreateOnce
	// enrolHookLocked, when set, runs once the lock is held and the checks
	// for an existing enrolment have passed, before the code is read.
	enrolHookLocked func()
	// enrolHookCodeRead, when set, runs once the code is read, before the
	// command checks for an interrupt and sends it.
	enrolHookCodeRead func()
)

// directoryEnroll is `shed-server directory enroll`: name is --name, set
// whether it was given.
func directoryEnroll(ctx context.Context, s enrolStreams, name string, nameSet bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if !cfg.DirectoryConfigured() {
		return errors.New("auth.directory is not configured: this server does not take its users from an account service")
	}
	dc := cfg.Auth.Directory
	name, err = enrolName(name, nameSet)
	if err != nil {
		return err
	}

	stateDir := serverStateDir(cfg)
	fmt.Fprintf(s.out, "State directory: %s\n", stateDir)
	if err := ensureOwnStateDir(stateDir); err != nil {
		return err
	}
	// <state>/directory is opened and checked once; every step below (the
	// lock, the listing, the reads, the removals, the write) goes through
	// the directory opened.
	d, err := directory.MakeStateDir(filepath.Join(stateDir, directory.StateDirName))
	if err != nil {
		return fmt.Errorf("the directory's state: %w", err)
	}
	defer d.Close()
	unlock, err := d.Lock(enrolLockName)
	if errors.Is(err, directory.ErrLocked) {
		return fmt.Errorf("another enrolment is running (it holds %s); nothing was sent", d.Join(enrolLockName))
	}
	if err != nil {
		return fmt.Errorf("the enrolment lock: %w", err)
	}
	defer unlock()

	path := d.Join(directory.EnrolmentFileName)
	existing, err := readEnrolment(d)
	if err != nil {
		return fmt.Errorf("%s is there but cannot be used: %v.\n"+
			"Nothing was removed and no code was read. Inspect the file: if it holds this server's enrolment, put it right "+
			"(a regular file, owned by the user the server runs as, mode 0600). To enrol again instead: stop the server, "+
			"disable on the account service the server the file names (or, if it names none, the server this host enrolled), "+
			"remove the file by hand, and run this again", path, err)
	}
	if existing != nil {
		defer clear(existing.PrivateKey)
	}
	if err := removeLeftovers(s.out, d, existing); err != nil {
		return err
	}
	if existing != nil {
		id := existing.ClientID
		return fmt.Errorf("this server is already enrolled, as %s (%s); to enrol again, stop the server, remove %s, disable %s on the account service, and enrol with a new code", id, path, path, id)
	}
	if enrolHookLocked != nil {
		enrolHookLocked()
	}

	code, err := readEnrolmentCode(ctx, s.in, s.errOut)
	if err != nil {
		return err
	}
	if enrolHookCodeRead != nil {
		enrolHookCodeRead()
	}
	// The name is kept in enrolment.json: one holding the code is refused
	// before anything is sent, and the message shows neither.
	if keptHoldsCode(name, code) {
		if nameSet {
			return errors.New("--name holds the enrolment code; give another name. Nothing was sent")
		}
		return errors.New("this host's name holds the enrolment code; give a --name that does not. Nothing was sent")
	}
	// An interrupt that came before the request is sent: nothing was sent.
	// One that comes while it is under way is a lost answer (Enroll's
	// failure says so), since the service may have received it.
	if ctx.Err() != nil {
		return errInterrupted
	}
	reqCtx, cancel := context.WithTimeout(ctx, enrolTimeout)
	defer cancel()
	e, err := directory.Enroll(reqCtx, directory.EnrolRequest{
		Issuer:         dc.Issuer(),
		Organization:   dc.Organization,
		Code:           code,
		Name:           name,
		AllowPlainHTTP: bool(dc.AllowPlainHTTP),
	})
	if err != nil {
		return enrolFailure(err, code)
	}
	defer clear(e.PrivateKey)
	if f := codeKept(e, code); f != nil {
		return enrolFailure(f, code)
	}
	if err := installEnrolment(d, e, code); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "Enrolled with the account service at %s as %s (organization %s, name %q).\n",
		e.Issuer, redactCode(e.ClientID, code), redactCode(e.Organization, code), e.Name)
	fmt.Fprintf(s.out, "Wrote %s (mode 0600): this server's key for the account service, which never leaves this host.\n", path)
	return nil
}

// enrolName is the name to enrol under: --name when given, else the host
// name, either 1 to 255 printable ASCII characters.
func enrolName(flag string, set bool) (string, error) {
	if set {
		if !directory.ValidServerName(flag) {
			return "", fmt.Errorf("--name %q is not 1 to %d printable ASCII characters", flag, directory.MaxServerNameBytes)
		}
		return flag, nil
	}
	host, err := enrolHostname()
	if err != nil {
		return "", fmt.Errorf("--name is required: this host's name cannot be read (%v)", err)
	}
	if !directory.ValidServerName(host) {
		return "", fmt.Errorf("--name is required: this host's name %q is not 1 to %d printable ASCII characters", host, directory.MaxServerNameBytes)
	}
	return host, nil
}

// ensureOwnStateDir makes the state directory (mode 0700) when it does not
// exist, as serve does, and refuses one that is not a directory owned by the
// user running the command: the enrolment must belong to the user the
// server runs as.
func ensureOwnStateDir(dir string) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("state dir %s: %w", dir, err)
		}
		fi, err = os.Stat(dir)
	}
	if err != nil {
		return fmt.Errorf("state dir %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("state dir %s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("state dir %s: its owner cannot be read", dir)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("state dir %s is owned by uid %d, not by the user running this command (uid %d): run it as the user the server runs as", dir, st.Uid, os.Geteuid())
	}
	return nil
}

// readEnrolment reads enrolment.json: nil, nil when there is none; an error
// when there is something under its name that is not a usable enrolment.
func readEnrolment(d *directory.StateDir) (*directory.Enrolment, error) {
	return readEnrolmentNamed(d, directory.EnrolmentFileName)
}

// readEnrolmentNamed reads name in d as a complete enrolment: nil, nil when
// there is no such file.
func readEnrolmentNamed(d *directory.StateDir, name string) (*directory.Enrolment, error) {
	data, err := d.ReadFile(name, directory.MaxEnrolmentFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer clear(data)
	e, err := directory.ParseEnrolmentFile(data)
	if err != nil {
		return nil, fmt.Errorf("it is not a complete enrolment: %w", err)
	}
	return e, nil
}

// leftover is a temporary file of enrolment.json an earlier run left.
type leftover struct {
	name string
	e    *directory.Enrolment // nil when it cannot be read as one
	why  error
}

// removeLeftovers deals with the temporary files of enrolment.json an
// earlier run left when it stopped between writing one and removing it
// (each holds a key). existing is enrolment.json, read (nil: there is none).
//
// When any of them cannot be read as a complete enrolment (it cannot be
// read, its mode lets others use it, it is incomplete, it is not a regular
// file), or holds enrolment.json's client id with another key, nothing is
// removed: the error names each and says what to do. Otherwise each is
// removed, saying so: one that is enrolment.json's own (the same client id
// and key) names nothing; one holding another enrolment names its client
// id to disable, since this server keeps no key for it.
func removeLeftovers(w io.Writer, d *directory.StateDir, existing *directory.Enrolment) error {
	names, err := d.Names()
	if err != nil {
		return fmt.Errorf("the directory's state: %w", err)
	}
	var found []leftover
	defer func() {
		for _, l := range found {
			if l.e != nil {
				clear(l.e.PrivateKey)
			}
		}
	}()
	var bad []string
	for _, name := range names {
		if !directory.IsTempFileOf(name, directory.EnrolmentFileName) {
			continue
		}
		l := leftover{name: name}
		switch e, err := readEnrolmentNamed(d, name); {
		case err != nil:
			l.why = err
		case e == nil:
			continue // gone since it was listed
		case existing != nil && e.ClientID == existing.ClientID && !bytes.Equal(e.PrivateKey, existing.PrivateKey):
			l.e = e
			l.why = fmt.Errorf("it holds the client id %s of %s, with another key", e.ClientID, directory.EnrolmentFileName)
		default:
			l.e = e
		}
		found = append(found, l)
		if l.why != nil {
			bad = append(bad, fmt.Sprintf("%s (%v)", d.Join(name), l.why))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("a temporary file an earlier enrolment left cannot be read as a complete enrolment: %s.\n"+
			"Nothing was removed and no code was read. Inspect each: if a previous run may have enrolled this server, "+
			"find the server on the account service that this server does not know and disable it; "+
			"then remove the file by hand and run this again", strings.Join(bad, "; "))
	}
	for _, l := range found {
		path := d.Join(l.name)
		if err := d.Remove(l.name); err != nil {
			return fmt.Errorf("remove %s, a temporary file an earlier enrolment left: %w", path, err)
		}
		fmt.Fprintf(w, "Removed %s, a temporary file an earlier enrolment left when it stopped.\n", path)
		if existing == nil || existing.ClientID != l.e.ClientID {
			fmt.Fprintf(w, "It held an enrolment as %s, for which this server keeps no key: disable %s on the account service.\n", l.e.ClientID, l.e.ClientID)
		}
	}
	return nil
}

// readEnrolmentCode reads the code from in, at most maxCodeRead bytes: when
// in is a terminal, with echo turned off before the prompt (on prompt) is
// shown; otherwise one line. The code is the line trimmed: 1 to 64 bytes of
// UTF-8. When ctx ends first, the read is abandoned and the error is
// errInterrupted.
func readEnrolmentCode(ctx context.Context, in io.Reader, prompt io.Writer) (string, error) {
	var line []byte
	var err error
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		line, err = readCodeFromTerminal(ctx, f, prompt)
	} else {
		line, err = readCodeLineCtx(ctx, in)
	}
	if err != nil {
		return "", err
	}
	code := strings.TrimSpace(string(line))
	clear(line)
	switch {
	case code == "":
		return "", errNoCode
	case !directory.ValidCode(code):
		return "", fmt.Errorf("the enrolment code must be 1 to %d bytes of UTF-8; nothing was sent", directory.MaxCodeBytes)
	}
	return code, nil
}

// readCodeFromTerminal reads the code from the terminal f: echo is turned
// off, then the prompt is shown, then the line is read under the same bound
// as a pipe's. The terminal is put back as it was exactly once, on every
// path (an interrupt included), discarding any input not read (the rest of
// an over-long line), so none of it reaches the shell.
func readCodeFromTerminal(ctx context.Context, f *os.File, prompt io.Writer) ([]byte, error) {
	restore, err := echoOff(int(f.Fd()))
	if err != nil {
		return nil, fmt.Errorf("turn off echo on the terminal: %w; nothing was sent", err)
	}
	defer restore()
	fmt.Fprint(prompt, "Enrolment code (not echoed): ")
	line, err := readCodeLineCtx(ctx, f)
	fmt.Fprintln(prompt)
	return line, err
}

// readCodeLineCtx is readCodeLine, abandoned when ctx ends first: the read
// goes on in its goroutine (a read of standard input cannot be cancelled)
// and whatever it reads later is cleared and dropped.
func readCodeLineCtx(ctx context.Context, in io.Reader) ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := readCodeLine(in)
		got <- result{line, err}
	}()
	select {
	case r := <-got:
		return r.line, r.err
	case <-ctx.Done():
		go func() { clear((<-got).line) }()
		return nil, errInterrupted
	}
}

// readCodeLine reads one line from in, a byte at a time so nothing past it
// is consumed: the bytes before the newline or the end of input. More than
// maxCodeRead-1 bytes without a newline is refused, having read maxCodeRead.
// A reader that keeps giving nothing (no byte, no error) is given up on.
func readCodeLine(in io.Reader) ([]byte, error) {
	buf := make([]byte, 0, maxCodeRead)
	var one [1]byte
	empty := 0
	for len(buf) < maxCodeRead {
		n, err := in.Read(one[:])
		if n == 1 {
			empty = 0
			if one[0] == '\n' {
				return buf, nil
			}
			buf = append(buf, one[0])
			continue
		}
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if err != nil {
			clear(buf)
			return nil, fmt.Errorf("read the enrolment code from standard input: %w", err)
		}
		if empty++; empty >= maxEmptyReads {
			clear(buf)
			return nil, errNoInput
		}
	}
	clear(buf)
	return nil, errCodeTooLong
}

// redactCode is s with every occurrence of the enrolment code, as it is and
// as Go's quoting writes it, replaced by codeMarker.
func redactCode(s, code string) string {
	if code == "" {
		return s
	}
	s = strings.ReplaceAll(s, code, codeMarker)
	if q := strconv.Quote(code); q[1:len(q)-1] != code {
		s = strings.ReplaceAll(s, q[1:len(q)-1], codeMarker)
	}
	return s
}

// holdsCode reports whether s holds the code, as it is or as Go's quoting
// writes it.
func holdsCode(s, code string) bool {
	q := strconv.Quote(code)
	return strings.Contains(s, code) || strings.Contains(s, q[1:len(q)-1])
}

// keptHoldsCode reports whether s, a value enrolment.json would keep, holds
// the code: as it is, as Go's quoting writes it, or as the file's JSON
// encoding writes it.
func keptHoldsCode(s, code string) bool {
	if holdsCode(s, code) {
		return true
	}
	j, err := json.Marshal(s)
	return err == nil && strings.Contains(string(j[1:len(j)-1]), code)
}

// codeKept is the failure of a 201 that passed the library's checks but
// would put the enrolment code into enrolment.json: a value kept from it
// (client id, issuer, token endpoint, organization) holds the code. nil when
// none does. The failure is the service's 201 failing its checks: its client
// id is offered to disable only when it does not itself hold the code.
func codeKept(e *directory.Enrolment, code string) *directory.Failure {
	for _, v := range []struct{ member, value string }{
		{"client_id", e.ClientID}, {"issuer", e.Issuer}, {"token_endpoint", e.TokenEndpoint}, {"organization", e.Organization},
	} {
		if keptHoldsCode(v.value, code) {
			return &directory.Failure{
				Step: directory.StepEnroll, Kind: directory.KindBadAnswer, Status: http.StatusCreated,
				ClientID: e.ClientID, Err: fmt.Errorf("its %s holds the enrolment code", v.member),
			}
		}
	}
	return nil
}

// quoteService is the service's text s, the code replaced, quoted (one
// line, control characters escaped); when it still holds the code (the
// replacement made it anew), it is not shown.
func quoteService(s, code string) string {
	q := strconv.Quote(redactCode(s, code))
	if holdsCode(q, code) {
		return codeNotShown
	}
	return q
}

// serviceWords is the line that shows what the answer (or the transport)
// said, each piece quoted and the code removed: the service's words, apart
// from the command's own guidance. "" when there is nothing to show.
func serviceWords(f *directory.Failure, code string) string {
	var parts []string
	if f.Code != "" {
		parts = append(parts, "code "+quoteService(f.Code, code))
	}
	if f.Message != "" {
		parts = append(parts, "message "+quoteService(f.Message, code))
	}
	if f.Err != nil {
		parts = append(parts, "error "+quoteService(f.Err.Error(), code))
	}
	if len(parts) == 0 {
		return ""
	}
	return "\nThe service's words: " + strings.Join(parts, ", ")
}

// idToDisable is the client id the operator is told to disable: id when it
// passed the client id check and does not hold the code, else "" (the
// command then gives the recovery for an unknown server).
func idToDisable(id, code string) string {
	if !directory.ValidClientID(id) || holdsCode(id, code) {
		return ""
	}
	return id
}

// lostRecovery is what the operator does when the server the service
// enrolled is not known to this command.
const lostRecovery = "To recover: on the account service, find the server enrolled with this code that this server does not know and disable it; " +
	"issue a new code; then run 'shed-server directory enroll' again"

// enrolFailure is the command's error for a failed enrolment: its own
// guidance (a refusal; a lost answer and its recovery; a 201 that failed its
// checks and the client id to disable), built from values it checked, on
// lines apart from the service's words, which are quoted with the code
// removed.
func enrolFailure(err error, code string) error {
	var f *directory.Failure
	if !errors.As(err, &f) {
		return fmt.Errorf("enrolment: %s; nothing was sent", redactCode(err.Error(), code))
	}
	status := ""
	if f.Status != 0 {
		status = fmt.Sprintf(" (HTTP %d)", f.Status)
	}
	words := serviceWords(f, code)
	id := ""
	if !f.Lost() {
		id = idToDisable(f.ClientID, code)
	}
	switch {
	case f.Lost() || (f.ClientID != "" && id == ""):
		return fmt.Errorf("the account service's answer to the enrolment was lost%s. Nothing was written and the key was discarded, "+
			"but the service may have enrolled this server under a client id this server never learned.%s\n%s", status, words, lostRecovery)
	case id != "":
		return fmt.Errorf("the account service enrolled this server as %s, but its answer failed a check. Nothing was written and the key was discarded, "+
			"so this server keeps no key for %s.%s\nTo recover: disable %s on the account service, check auth.directory against the service, issue a new code and enrol again",
			id, id, words, id)
	default:
		retry := ""
		if f.HasRetryAfter {
			retry = fmt.Sprintf(" Try again after %s.", f.RetryAfter)
		}
		return fmt.Errorf("the account service refused the enrolment%s; nothing was written.%s%s", status, retry, words)
	}
}

// installEnrolment writes e to enrolment.json, once, through d. When the
// file was not created, the error names the client id to disable. When it
// was (the link happened) but a later step failed, the server is enrolled:
// the error says the write is not fully confirmed and names nothing to
// disable, whatever a read of the file would say.
func installEnrolment(d *directory.StateDir, e *directory.Enrolment, code string) error {
	path := d.Join(directory.EnrolmentFileName)
	id := idToDisable(e.ClientID, code)
	notKept := func(what string, err error) error {
		if id == "" {
			return fmt.Errorf("the account service enrolled this server, but %s (%s). Nothing is kept: the key was discarded.\n%s",
				what, redactCode(err.Error(), code), lostRecovery)
		}
		return fmt.Errorf("the account service enrolled this server as %s, but %s (%s), so this server keeps no key for %s.\n"+
			"To recover: disable %s on the account service, issue a new code and enrol again",
			id, what, redactCode(err.Error(), code), id, id)
	}
	data, err := e.MarshalFile()
	if err != nil {
		return notKept("the enrolment could not be encoded", err)
	}
	defer clear(data)
	linked, err := createEnrolmentFile(d, directory.EnrolmentFileName, data)
	switch {
	case err == nil:
		return nil
	case linked:
		return fmt.Errorf("enrolled with the account service as %s, and %s was created holding the enrolment, but the write is not fully confirmed (%s).\n"+
			"Nothing needs disabling. Before starting the server, make sure %s is there and intact (a regular file, mode 0600); "+
			"run this command again to remove a temporary copy it may have left beside it",
			redactCode(e.ClientID, code), path, redactCode(err.Error(), code), path)
	default:
		return notKept(path+" was not written", err)
	}
}
