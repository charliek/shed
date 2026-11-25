package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/source"
)

// minSourceIDPrefix is the shortest id prefix `shed source use` accepts.
const minSourceIDPrefix = 8

var sourceUserFlag string

var sourceCmd = &cobra.Command{
	Use:   "source",
	Short: "Show, move and clear the machine that answers your credential requests",
	Long: `A shed server sends your sheds' credential requests (SSH agent, Docker and
AWS helpers) to one of your machines only: your credential source on that
server. The first of your machines to connect becomes it; the others are
refused and keep retrying, naming the source.

  shed source status           Which machine is the source, and is it connected
  shed source ls               Every machine that has presented itself for you
  shed source use <machine>    Make another of your machines the source
  shed source unbind           Clear the source: the next machine to connect becomes it

Use -s/--server to pick the server. An admin can name another user with
--user.`,
}

var sourceLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List the machines that have presented themselves for you",
	Args:    cobra.NoArgs,
	RunE:    runSourceLs,
}

var sourceUseCmd = &cobra.Command{
	Use:   "use [--] <machine>",
	Short: "Make one of your machines the credential source",
	Long: `Make one of your machines the credential source on this server.

<machine> is a machine's full id, a prefix of its id of at least 8 hex
digits, or its exact name, looked up in 'shed source ls'. An argument that
matches more than one machine, in any of those ways (one machine's name can
be another's id prefix), changes nothing and lists them. Put -- before a
name that begins with '-'.

The old machine stops answering at once. The new one starts answering when
its desktop app next connects, within about 30 seconds
(at once from the desktop app's "Use this machine").`,
	Example: `  shed source use laptop
  shed source use 9a60b039
  shed -s myserver source use -- "-odd name"`,
	Args: cobra.ExactArgs(1),
	RunE: runSourceUse,
}

var sourceUnbindCmd = &cobra.Command{
	Use:   "unbind",
	Short: "Clear your credential source on this server",
	Long: `Clear your credential source on this server, so that no machine is it.

The server forgets the source and every machine it remembers for you, and
closes your machines' credential streams. Until a machine is the source
again, a create asks no machine for credentials and runs its install hook
as it would with no source (a source that is offline holds the hook back
until 'shed provision').

The first of your machines to connect afterwards becomes the source, as on
a first start. A desktop app that is running reconnects within about 30
seconds, so its machine becomes the source again: quit the app on that
machine first, or use 'shed source use' to pick another machine. Nothing
changes when you have no source.`,
	Example: `  shed source unbind
  shed -s myserver source unbind --user bob`,
	Args: cobra.NoArgs,
	RunE: runSourceUnbind,
}

var sourceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show which machine is the credential source, and whether it is connected",
	Args:  cobra.NoArgs,
	RunE:  runSourceStatus,
}

func init() {
	sourceCmd.PersistentFlags().StringVar(&sourceUserFlag, "user", "", "Another user's credential source (admin)")
	sourceCmd.SetFlagErrorFunc(sourceFlagError)
	sourceCmd.AddCommand(sourceLsCmd, sourceUseCmd, sourceUnbindCmd, sourceStatusCmd)
	rootCmd.AddCommand(sourceCmd)
}

// sourceFlagError is the flag parser's error on the source commands (cobra
// hands it to the subcommands too), escaped for the terminal: the parser
// quotes the argument it refused raw, and on `shed source use` that is
// usually a machine name that begins with '-', which came from a machine.
func sourceFlagError(c *cobra.Command, err error) error {
	msg := termSafe(err.Error())
	if c == sourceUseCmd {
		msg += "\nput -- before a machine name that begins with '-': shed source use -- NAME"
	}
	return errors.New(msg)
}

// isSourceCommand reports whether cmd is `shed source` or one of its
// subcommands, whose errors main prints escaped (reportError).
func isSourceCommand(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c == sourceCmd {
			return true
		}
	}
	return false
}

// termSafeLines is termSafe for a message of several lines: each line is
// escaped and the line breaks are kept.
func termSafeLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = termSafe(l)
	}
	return strings.Join(lines, "\n")
}

// sourceClient is the client for the server -s names (or the default), and
// the server's name.
func sourceClient() (*APIClient, string, error) {
	if sourceUserFlag != "" && !config.ValidUserName(sourceUserFlag) {
		return nil, "", fmt.Errorf("%q is not a valid user name", sourceUserFlag)
	}
	entry, name, err := getServerEntry()
	if err != nil {
		return nil, "", err
	}
	return NewAPIClientFromNamedEntry(name, entry, DefaultTimeout), name, nil
}

// sourceError is the error of a source command that the server refused:
// what was done, on which server, and the server's answer (escaped for the
// terminal).
func sourceError(what, server string, err error) error {
	if errors.Is(err, errSourcesUnsupported) {
		return fmt.Errorf("server %s does not support credential sources (it needs shed-server 0.10.0 or newer)", server)
	}
	return fmt.Errorf("%s on %s: %s", what, server, termSafe(err.Error()))
}

// sourceIsServiceError is the refusal of `shed source use` and `unbind` for
// a user the account service answers for on server: the server's own
// SOURCE_IS_SERVICE text, the service's URL, and what that means here.
func sourceIsServiceError(server string, svc *config.SourceRef, what string) error {
	return fmt.Errorf("on %s, %s (%s): %s; nothing changed", server, config.SourceIsServiceMessage, termSafe(svc.URL), what)
}

// sourceWho names whose sources a command shows, in a sentence.
func sourceWho() string {
	if sourceUserFlag == "" {
		return "you"
	}
	return "user " + sourceUserFlag
}

func runSourceLs(cmd *cobra.Command, args []string) error {
	client, server, err := sourceClient()
	if err != nil {
		return err
	}
	resp, err := client.ListSources(sourceUserFlag)
	if err != nil {
		return sourceError("list credential sources", server, err)
	}
	if jsonFlag {
		return writeTermJSON(cmd.OutOrStdout(), resp)
	}
	if svc := resp.Service(); svc != nil {
		renderServiceStatus(cmd.OutOrStdout(), server, svc)
		return nil
	}
	renderSources(cmd.OutOrStdout(), server, *resp)
	return nil
}

// renderSources prints one row per machine, the bound one marked.
func renderSources(w io.Writer, server string, resp config.SourcesResponse) {
	if len(resp.Machines) == 0 {
		fmt.Fprintf(w, "No machine has presented itself for %s on %s yet: the first one to connect becomes the credential source.\n", sourceWho(), server)
		return
	}
	bound := resp.Binding.MachineID()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tID\tKEY\tBOUND\tCONNECTED\tFIRST SEEN")
	for _, m := range resp.Machines {
		key := m.KeyFingerprint
		if key == "" {
			key = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			termSafe(m.Name), shortSourceID(m.ID), termSafe(key), yesNo(m.ID == bound), yesNo(m.Connected), formatTimeAgo(m.FirstSeen))
	}
	tw.Flush()
	if bound == "" {
		fmt.Fprintf(w, "\nNo machine is the credential source for %s yet: the next one to connect becomes it.\n", sourceWho())
	}
}

func runSourceUse(cmd *cobra.Command, args []string) error {
	client, server, err := sourceClient()
	if err != nil {
		return err
	}
	before, err := client.ListSources(sourceUserFlag)
	if err != nil {
		return sourceError("list credential sources", server, err)
	}
	if svc := before.Service(); svc != nil {
		return sourceIsServiceError(server, svc, "nothing to move")
	}
	m, err := resolveSourceMachine(before.Machines, args[0])
	if err != nil {
		return fmt.Errorf("%w\nnothing changed on %s", err, server)
	}
	after, err := client.UseSource(m.ID, sourceUserFlag)
	if err != nil {
		return sourceError("move the credential source", server, err)
	}
	if jsonFlag {
		return writeTermJSON(cmd.OutOrStdout(), after)
	}
	w := cmd.OutOrStdout()
	// Whether this request moved anything is read from the POST's answer,
	// not from the list read before it (another move may have come between):
	// every bind and move gives the binding a new generation, so an answer at
	// the generation the list showed is a move to the machine already bound,
	// which changes nothing.
	prev := before.Binding.MachineID()
	if before.Binding != nil && after.Binding != nil && after.Binding.Generation == before.Binding.Generation && after.Binding.MachineID() == m.ID {
		fmt.Fprintf(w, "Machine %s is already the credential source for %s on %s.\n", sourceMachineLabel(m), sourceWho(), server)
		return nil
	}
	fmt.Fprintf(w, "Machine %s is now the credential source for %s on %s.\n", sourceMachineLabel(m), sourceWho(), server)
	if old, ok := findSourceMachine(before.Machines, prev); ok && prev != m.ID {
		fmt.Fprintf(w, "Machine %s no longer answers. ", sourceMachineLabel(old))
	}
	fmt.Fprintln(w, "The new source answers once its desktop app next connects, within about 30 s.")
	return nil
}

func runSourceUnbind(cmd *cobra.Command, args []string) error {
	client, server, err := sourceClient()
	if err != nil {
		return err
	}
	// The document first: a user the account service answers for has no
	// binding to clear. A server too old to answer it is asked to unbind as
	// before, and refuses or answers for itself.
	if before, err := client.ListSources(sourceUserFlag); err == nil {
		if svc := before.Service(); svc != nil {
			return sourceIsServiceError(server, svc, "nothing to clear")
		}
	}
	resp, err := client.UnbindSource(sourceUserFlag)
	if err != nil {
		if errors.Is(err, errSourcesUnsupported) {
			return fmt.Errorf("server %s does not support clearing a credential source (it needs shed-server 0.11.0 or newer)", server)
		}
		return sourceError("clear the credential source", server, err)
	}
	if jsonFlag {
		return writeTermJSON(cmd.OutOrStdout(), resp)
	}
	renderSourceUnbind(cmd.OutOrStdout(), server, *resp)
	return nil
}

// renderSourceUnbind says what an unbind did, and what happens next.
func renderSourceUnbind(w io.Writer, server string, resp config.SourceUnbindResponse) {
	if !resp.Unbound {
		fmt.Fprintf(w, "No credential source for %s on %s: nothing changed.\n", sourceWho(), server)
		return
	}
	prev := config.SourceMachine{}
	if resp.Previous != nil {
		prev = config.SourceMachine{ID: resp.Previous.ID, Name: resp.Previous.Name}
	}
	fmt.Fprintf(w, "Cleared the credential source for %s on %s (it was machine %s).\n", sourceWho(), server, sourceMachineLabel(prev))
	switch n := resp.ForgottenMachines; {
	case n == 1:
		fmt.Fprintln(w, "Forgot 1 machine; it is listed again when it next connects.")
	case n > 1:
		fmt.Fprintf(w, "Forgot %d machines; each is listed again when it next connects.\n", n)
	}
	whose := "your"
	if sourceUserFlag != "" {
		whose = "user " + sourceUserFlag + "'s"
	}
	fmt.Fprintf(w, "The first of %s machines to connect next becomes the source: a desktop app that is still running reconnects within about 30 s, and its machine becomes the source again.\n", whose)
}

// sourceMatch is a machine arg matched, and every way it matched.
type sourceMatch struct {
	m   config.SourceMachine
	how []string
}

// resolveSourceMachine picks the machine arg names, counting all three ways
// an argument can name one: its exact id (any case), a prefix of its id of
// at least minSourceIDPrefix hex digits (any case), and its exact name (case
// as given). It succeeds only when exactly one machine matches, however many
// ways that machine matched; when several do (two names, two prefixes, or
// one machine's name that is another's id or id prefix) it changes nothing
// and lists every candidate with how it matched.
func resolveSourceMachine(machines []config.SourceMachine, arg string) (config.SourceMachine, error) {
	lower := strings.ToLower(arg)
	prefixOK := len(lower) >= minSourceIDPrefix && isHex(lower)
	var hits []sourceMatch
	for _, m := range machines {
		var how []string
		switch {
		case m.ID == lower:
			how = append(how, "id")
		case prefixOK && strings.HasPrefix(m.ID, lower):
			how = append(how, "id prefix")
		}
		if m.Name == arg {
			how = append(how, "name")
		}
		if len(how) > 0 {
			hits = append(hits, sourceMatch{m: m, how: how})
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0].m, nil
	case len(hits) > 1:
		return config.SourceMachine{}, ambiguousSource(arg, hits)
	}
	return config.SourceMachine{}, fmt.Errorf("no machine named %q or with that id has presented itself for %s ('shed source ls' lists those that have)", arg, sourceWho())
}

// ambiguousSource is the error for an argument that matches several
// machines: it lists them, each by its full id, its name (escaped) and how
// the argument matched it.
func ambiguousSource(arg string, hits []sourceMatch) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d machines; name one by its full id:", arg, len(hits))
	for _, h := range hits {
		fmt.Fprintf(&b, "\n  %s  %s  (matched by %s)", h.m.ID, termSafe(h.m.Name), strings.Join(h.how, " and "))
	}
	return errors.New(b.String())
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return s != ""
}

func findSourceMachine(machines []config.SourceMachine, id string) (config.SourceMachine, bool) {
	for _, m := range machines {
		if id != "" && m.ID == id {
			return m, true
		}
	}
	return config.SourceMachine{}, false
}

// sourceMachineLabel is a machine as a sentence names it: its quoted name
// (escaped for the terminal) and the start of its id.
func sourceMachineLabel(m config.SourceMachine) string {
	return fmt.Sprintf("\"%s\" (%s)", termSafe(m.Name), shortSourceID(m.ID))
}

func shortSourceID(id string) string {
	if len(id) > minSourceIDPrefix {
		return id[:minSourceIDPrefix]
	}
	return id
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// sourceStatus is `shed source status --json`.
type sourceStatus struct {
	Server string `json:"server"`
	User   string `json:"user"`
	// Source is null when no machine is the source yet, and for a user the
	// account service answers for.
	Source *sourceStatusMachine `json:"source"`
	// Service is set when the account service answers for the user on this
	// server (no machine does).
	Service *sourceStatusService `json:"service,omitempty"`
}

// sourceStatusService is the account service as `shed source status
// --json` names it: its URL, and whether it is delivering to this server
// (false for five minutes after it refused the server deliveries).
type sourceStatusService struct {
	URL        string `json:"url"`
	Delivering bool   `json:"delivering"`
}

// renderServiceStatus says that the account service answers for the user on
// server, and whether it is delivering to it.
func renderServiceStatus(w io.Writer, server string, svc *config.SourceRef) {
	fmt.Fprintf(w, "Credential source for %s on %s: the account service (%s); no machine answers here.\n", sourceWho(), server, termSafe(svc.URL))
	if svc.State == source.StateNotDelivering {
		fmt.Fprintln(w, "The account service is not delivering credentials to this server: deliveries are not applicable (asked again after five minutes, or after 'shed users refresh').")
	}
}

type sourceStatusMachine struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	KeyFingerprint string    `json:"key_fingerprint"`
	Connected      bool      `json:"connected"`
	Namespaces     []string  `json:"namespaces"`
	BoundAt        time.Time `json:"bound_at"`
	BoundBy        string    `json:"bound_by"`
}

func runSourceStatus(cmd *cobra.Command, args []string) error {
	client, server, err := sourceClient()
	if err != nil {
		return err
	}
	resp, err := client.ListSources(sourceUserFlag)
	if err != nil {
		return sourceError("read the credential source", server, err)
	}
	st := sourceStatus{Server: server, User: resp.User}
	if svc := resp.Service(); svc != nil {
		st.Service = &sourceStatusService{URL: svc.URL, Delivering: svc.State != source.StateNotDelivering}
		if jsonFlag {
			return writeTermJSON(cmd.OutOrStdout(), st)
		}
		renderServiceStatus(cmd.OutOrStdout(), server, svc)
		return nil
	}
	if id := resp.Binding.MachineID(); id != "" {
		m, ok := findSourceMachine(resp.Machines, id)
		if !ok {
			m = config.SourceMachine{ID: id, Namespaces: []string{}}
		}
		st.Source = &sourceStatusMachine{
			ID:             id,
			Name:           m.Name,
			KeyFingerprint: m.KeyFingerprint,
			Connected:      m.Connected,
			Namespaces:     m.Namespaces,
			BoundAt:        resp.Binding.BoundAt,
			BoundBy:        resp.Binding.BoundBy,
		}
	}
	if jsonFlag {
		return writeTermJSON(cmd.OutOrStdout(), st)
	}
	renderSourceStatus(cmd.OutOrStdout(), server, *resp)
	return nil
}

// renderSourceStatus says in one or two lines which machine is the source and
// whether it is connected. No source is not an error.
func renderSourceStatus(w io.Writer, server string, resp config.SourcesResponse) {
	id := resp.Binding.MachineID()
	if id == "" {
		fmt.Fprintf(w, "No credential source for %s on %s yet: the first of your machines to connect becomes it.\n", sourceWho(), server)
		return
	}
	m, ok := findSourceMachine(resp.Machines, id)
	if !ok {
		m = config.SourceMachine{ID: id}
	}
	switch {
	case m.Connected && len(m.Namespaces) == 0:
		// Connected with its delivery stream only: no bus namespaces.
		fmt.Fprintf(w, "Credential source for %s on %s: %s, connected.\n",
			sourceWho(), server, sourceMachineLabel(m))
	case m.Connected:
		fmt.Fprintf(w, "Credential source for %s on %s: %s, connected (%s).\n",
			sourceWho(), server, sourceMachineLabel(m), termSafe(strings.Join(m.Namespaces, ", ")))
	default:
		fmt.Fprintf(w, "Credential source for %s on %s: %s, not connected: credential requests get no answer until it reconnects.\n",
			sourceWho(), server, sourceMachineLabel(m))
	}
	by := resp.Binding.BoundBy
	if by == "first-connect" {
		by = "the first machine to connect"
	}
	fmt.Fprintf(w, "Bound %s by %s.\n", formatTimeAgo(resp.Binding.BoundAt), termSafe(by))
}

// termSafe escapes what must not reach a terminal raw: control characters,
// Unicode format characters (bidirectional overrides, zero-width characters)
// and every other character that does not print, as \uXXXX; invalid UTF-8
// as \xXX. Machine names come from the machines themselves.
func termSafe(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == ' ' || unicode.IsPrint(r):
			b.WriteRune(r)
		case r > 0xFFFF:
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		i += size
	}
	return b.String()
}

// writeTermJSON writes v as indented JSON with every non-ASCII character that
// does not print (a bidirectional override, a zero-width character) written
// as a \u escape: the same JSON, safe to show on a terminal. (The encoder
// already escapes control characters; outside strings JSON is ASCII.)
func writeTermJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range string(data) {
		if r < utf8.RuneSelf || unicode.IsPrint(r) {
			b.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
			continue
		}
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	b.WriteString("\n")
	_, err = io.WriteString(w, b.String())
	return err
}
