package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// Cloud sheds (plan 041 §3.4): a remote server's sheds are machines of its
// account service, each with a lease. The CLI shows their size and the time
// left, extends a lease, and says what each of the service's refusals means
// in fixed words.

// computeWords are the fixed words for the account service's refusals, by
// code and then by reason ("" for a code with no reason). The server's
// message for these codes is not shown: these words are.
var computeWords = map[string]map[string]string{
	config.ErrComputeLimitReached: {
		"person":       "you hold as many cloud machines as the account service allows a person; delete one of your cloud sheds and try again",
		"organization": "the organization holds as many cloud machines as the account service allows; try again once one is deleted",
		"":             "the account service's limit on cloud machines is reached",
	},
	config.ErrComputePolicyRefused: {
		"size":   "the account service does not allow this size",
		"lease":  "the account service does not allow this lease",
		"image":  "the account service does not offer this image",
		"access": "the account service offers no way to reach a machine that this server supports",
		"person": "the account service makes cloud machines only for its own people and for service clients; a local admin can list, stop, delete and extend cloud sheds but not create them",
		"client": "this service client is not registered with the account service for this server",
		"":       "the account service's policy refused this",
	},
	config.ErrComputeDenied:      {"": "the account service does not let this server make cloud machines"},
	config.ErrComputeUnavailable: {"": "the account service could not be reached, or this server has not finished reading its machines; try again later"},
	config.ErrComputeDeleting:    {"": "the account service is deleting this cloud shed's machine (its lease ended, or it was deleted at the service)"},
	config.ErrExtendNotAllowed:   {"": "this cloud shed's lease cannot be extended (a service client's shed, or the account service's policy)"},
	config.ErrLeaseAtCap:         {"": "the extend would take the lease past its cap; extend by fewer hours"},
	config.ErrLeaseEnded:         {"": "this cloud shed's lease has ended; its machine is being deleted"},
	config.ErrPersonInactive:     {"": "the shed's person is no longer active at the account service"},
}

// apiErrorText is how the CLI words an API error: "CODE: message", and for
// the account service's refusals "CODE (reason): fixed words". Everything
// from the server is escaped (SafeText).
func apiErrorText(d config.APIErrorDetail) string {
	code := sdkbootstrap.SafeText(d.Code)
	if byReason, ok := computeWords[d.Code]; ok {
		words, ok := byReason[d.Reason]
		if !ok {
			words = byReason[""]
		}
		if d.Reason != "" {
			return fmt.Sprintf("%s (%s): %s", code, sdkbootstrap.SafeText(d.Reason), words)
		}
		return code + ": " + words
	}
	text := code + ": " + sdkbootstrap.SafeText(d.Message)
	if d.Reason != "" {
		text += " (reason: " + sdkbootstrap.SafeText(d.Reason) + ")"
	}
	return text
}

// --- shed list ---

// isCloud reports whether a shed is a cloud shed: a remote one (a record
// from a server of this plan carries "cloud" too).
func isCloud(s config.Shed) bool {
	return s.Cloud != nil || s.Backend == config.BackendRemote
}

// cloudShown reports whether a listing gets the SIZE and LEASE columns: only
// when a listed shed is a cloud shed.
func cloudShown(sheds []listedShed) bool {
	for _, s := range sheds {
		if isCloud(s.shed) {
			return true
		}
	}
	return false
}

// sizeCell is a shed's SIZE cell: its cloud size's name, "-" for any other.
func sizeCell(s config.Shed) string {
	if s.Cloud == nil || s.Cloud.Size.Name == "" {
		return "-"
	}
	return sdkbootstrap.SafeText(s.Cloud.Size.Name)
}

// leaseCell is a shed's LEASE cell: the time left on its lease ("5h 10m"),
// marked "!" under its warning time, and "-" for a shed with none or whose
// lease is not known yet.
func leaseCell(s config.Shed, now time.Time) string {
	if s.Cloud == nil || s.Cloud.Lease == nil || s.Cloud.Lease.ExpiresAt.IsZero() {
		return "-"
	}
	l := *s.Cloud.Lease
	cell := config.FormatLeft(l.Left(now))
	if l.Soon(now) {
		cell += " !"
	}
	return cell
}

// cloudRunTime is how long a running cloud shed has run, from its machine's
// last start, and "" when not known.
func cloudRunTime(s config.Shed, now time.Time) string {
	if s.Cloud == nil || s.Cloud.StartedAt == nil || s.Status != config.StatusRunning {
		return ""
	}
	return config.FormatLeft(now.Sub(*s.Cloud.StartedAt))
}

// writeCloudBlock is `shed list -vv`'s Cloud block for a cloud shed.
func writeCloudBlock(out io.Writer, s config.Shed, now time.Time) {
	c := s.Cloud
	if c == nil {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Cloud:")
	if c.Provider != "" {
		fmt.Fprintf(out, "  Provider:     %s\n", sdkbootstrap.SafeText(c.Provider))
	}
	fmt.Fprintf(out, "  Size:         %s (%d vCPU, %d MB)\n", sdkbootstrap.SafeText(c.Size.Name), c.Size.CPUs, c.Size.MemoryMB)
	if c.DiskGB > 0 {
		fmt.Fprintf(out, "  Disk:         %d GB\n", c.DiskGB)
	}
	if c.Image != "" {
		fmt.Fprintf(out, "  Image:        %s\n", sdkbootstrap.SafeText(c.Image))
	}
	if !c.CreatedAt.IsZero() {
		fmt.Fprintf(out, "  Created:      %s\n", c.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if run := cloudRunTime(s, now); run != "" {
		fmt.Fprintf(out, "  Running for:  %s\n", run)
	}
	l := c.Lease
	if l == nil {
		fmt.Fprintln(out, "  Lease:        not known yet")
		return
	}
	if !l.ExpiresAt.IsZero() {
		fmt.Fprintf(out, "  Deleted at:   %s (in %s)\n", l.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), config.FormatLeft(l.Left(now)))
	}
	if !l.MaxExpiresAt.IsZero() {
		fmt.Fprintf(out, "  Cap:          %s\n", l.MaxExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	extend := "no"
	if l.Extendable {
		extend = "yes"
		if len(l.PresetsH) > 0 {
			hs := make([]string, len(l.PresetsH))
			for i, h := range l.PresetsH {
				hs[i] = fmt.Sprintf("+%dh", h)
			}
			extend += " (" + strings.Join(hs, ", ") + ")"
		}
	}
	fmt.Fprintf(out, "  Extendable:   %s\n", extend)
}

// usageFooter is the footer line for one remote server's cloud usage:
//
//	Cloud: 3 of 4 machines (2 running, 1 stopped) · organization 7 of 10
//
// with the server's name after "Cloud" when the listing spans servers, and
// the organization's alone for a caller who is not a person of the service.
func usageFooter(server string, u config.CloudUsage, listAll bool) string {
	head := "Cloud"
	if listAll {
		head += " (" + server + ")"
	}
	org := fmt.Sprintf("organization %d of %d", u.Org.Machines, u.Org.Max)
	if p := u.Person; p != nil {
		return fmt.Sprintf("%s: %d of %d machines (%d running, %d stopped) · %s", head, p.Machines, p.Max, p.Running, p.Stopped, org)
	}
	return head + ": " + org
}

// cloudFooters returns the usage footers of the listing's servers that list
// a cloud shed, in server order. A server whose usage cannot be read (an
// older server, 501, the service down) gets none, quietly.
func cloudFooters(sheds []listedShed, listAll bool, usage func(server string) (*config.CloudUsage, error)) []string {
	seen := map[string]bool{}
	var servers []string
	for _, s := range sheds {
		if isCloud(s.shed) && !seen[s.server] {
			seen[s.server] = true
			servers = append(servers, s.server)
		}
	}
	sort.Strings(servers)
	var out []string
	for _, srv := range servers {
		u, err := usage(srv)
		if err != nil || u == nil {
			continue
		}
		out = append(out, usageFooter(srv, *u, listAll))
	}
	return out
}

// listUsage reads a server's cloud usage for the listing's footer.
func listUsage(server string) (*config.CloudUsage, error) {
	entry, ok := clientConfig.Servers[server]
	if !ok {
		return nil, errors.New("no such server")
	}
	return NewAPIClientFromNamedEntry(server, &entry, DefaultTimeout).CloudUsage()
}

// --- shed extend ---

var (
	extendHours int
	extendBy    time.Duration
)

var extendCmd = &cobra.Command{
	Use:   "extend <name> (--hours N | --by DURATION)",
	Short: "Move a cloud shed's lease later",
	Long: `Move a cloud shed's lease later, so its machine is deleted later.

A cloud shed's machine is deleted, running or stopped, when its lease
ends. Extend moves the end later by whole hours, never past the lease's
cap.

Examples:
  shed extend myproj --hours 6
  shed extend myproj --by 12h`,
	Args: cobra.ExactArgs(1),
	RunE: runExtend,
}

func init() {
	extendCmd.Flags().IntVar(&extendHours, "hours", 0, "Hours to move the lease's end by")
	extendCmd.Flags().DurationVar(&extendBy, "by", 0, "Duration to move the lease's end by, in whole hours (e.g. 6h)")
	rootCmd.AddCommand(extendCmd)
}

// extendHoursOf is the hours an extend asks for, from --hours or --by
// (exactly one given, whatever its value; whole hours, 1 to
// config.MaxExtendHours).
func extendHoursOf(hoursSet bool, hours int, bySet bool, by time.Duration) (int, error) {
	switch {
	case hoursSet && bySet:
		return 0, errors.New("give --hours or --by, not both")
	case bySet:
		if by%time.Hour != 0 {
			return 0, fmt.Errorf("--by %s is not whole hours", by)
		}
		hours = int(by / time.Hour)
	case !hoursSet:
		return 0, errors.New("give --hours N or --by DURATION")
	}
	if hours < 1 || hours > config.MaxExtendHours {
		return 0, fmt.Errorf("the extend must be 1 to %d hours", config.MaxExtendHours)
	}
	return hours, nil
}

func runExtend(cmd *cobra.Command, args []string) error {
	name := args[0]
	hours, err := extendHoursOf(cmd.Flags().Changed("hours"), extendHours, cmd.Flags().Changed("by"), extendBy)
	if err != nil {
		return err
	}
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}
	client := NewAPIClientFromNamedEntry(serverName, entry, clientConfig.GetCreateTimeout())
	// One request id for this invocation: a send whose reply is lost is
	// sent again with it, and the lease moves once.
	shed, err := extendWithRetry(client, name, hours, newRequestID(), extendRetryPause)
	if err != nil {
		var se *apiStatusError
		if errors.As(err, &se) && se.Status == http.StatusNotImplemented {
			return fmt.Errorf("shed %s on %s has no lease to extend (only a cloud shed has one)", name, serverName)
		}
		return fmt.Errorf("failed to extend shed: %w", err)
	}
	if jsonFlag {
		return outputJSON(ActionResult{Status: "ok", Action: "extended", Name: name, Server: serverName, Details: shed})
	}
	fmt.Fprintln(os.Stdout, extendedLine(shed, time.Now()))
	return nil
}

// extendedLine is what `shed extend` prints: the lease's new end and its
// cap.
func extendedLine(shed *config.Shed, now time.Time) string {
	if shed == nil {
		return "✓ Extended"
	}
	if shed.Cloud == nil || shed.Cloud.Lease == nil {
		return fmt.Sprintf("✓ Extended shed %s", shed.Name)
	}
	l := shed.Cloud.Lease
	line := fmt.Sprintf("✓ Extended shed %s: it is deleted at %s (in %s)", shed.Name,
		l.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), config.FormatLeft(l.Left(now)))
	if !l.MaxExpiresAt.IsZero() {
		line += "; the cap is " + l.MaxExpiresAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	return line
}

// --- the banner ---

// execBannerShown is the CLI's rule for the lease banner on `shed exec`:
// the server prints it for every session with a terminal, so the CLI prints
// it only for an exec that ran without one, and only when stderr is a
// terminal (a person reads it) and --json is off.
func execBannerShown(hasCommand bool, sshFlag string, stderrTTY, jsonOut bool) bool {
	return hasCommand && sshFlag == "-T" && stderrTTY && !jsonOut
}

// isCloudShed reports whether the named shed is a cloud shed, for the
// delete prompt's words. A shed that cannot be read is not called one.
func isCloudShed(serverName string, entry *config.ServerEntry, name string) bool {
	shed, err := NewAPIClientFromNamedEntry(serverName, entry, DefaultTimeout).GetShed(name)
	return err == nil && shed != nil && isCloud(*shed)
}

// writeCLIBanner writes the lease banner (config.LeaseBanner) the CLI
// prints itself (a `shed exec` without a terminal, a roost attach), when
// the shed's lease ends within its warning time.
func writeCLIBanner(w io.Writer, shed *config.Shed, now time.Time) {
	if line, ok := config.LeaseBanner(shed, now); ok {
		fmt.Fprintln(w, line)
	}
}

// bannerEnv is the SSH environment request that tells the server not to
// print its lease banner for this session.
const bannerEnv = "SHED_LEASE_BANNER=0"

// bannerOptOut is the ssh options for a session whose lease banner nobody
// reads: --json is on, or stderr is not a terminal. The session keeps the
// terminal it would have had; only the server's banner is left out.
func bannerOptOut(stderrTTY, jsonOut bool) []string {
	if stderrTTY && !jsonOut {
		return nil
	}
	return []string{"-o", "SetEnv=" + bannerEnv}
}

// roostBanner writes the banner after `shed attach` opened the shed in a
// roost tab: that session has no terminal on the server, which therefore
// prints nothing. Once, on stderr, when it is a terminal and --json is off.
func roostBanner(w io.Writer, shed *config.Shed, now time.Time, stderrTTY, jsonOut bool) {
	if stderrTTY && !jsonOut {
		writeCLIBanner(w, shed, now)
	}
}

// --- extend's request id and retries ---

// extendRetryPause is the pause before `shed extend` sends its request
// again; extendAttempts is how many sends it makes at most.
var extendRetryPause = 2 * time.Second

const extendAttempts = 3

// newRequestID returns a random lower-case UUID.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("shed: no randomness for a request id: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// extendWithRetry sends one extend, with request id rid, and sends it again
// with the same id when no answer came or the server could not decide (a
// 5xx): the server and the service answer a repeat with the first answer,
// so the lease moves once. A refusal (4xx) is final.
func extendWithRetry(client *APIClient, name string, hours int, rid string, pause time.Duration) (*config.Shed, error) {
	var shed *config.Shed
	var err error
	for attempt := 1; ; attempt++ {
		shed, err = client.ExtendShed(name, config.ExtendShedRequest{Hours: hours, RequestID: rid})
		var se *apiStatusError
		final := err == nil || (errors.As(err, &se) && se.Status < http.StatusInternalServerError)
		if final || attempt >= extendAttempts {
			return shed, err
		}
		time.Sleep(pause)
	}
}
