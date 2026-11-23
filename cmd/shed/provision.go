package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

var (
	provisionStatus  bool
	provisionTimeout time.Duration
)

var provisionCmd = &cobra.Command{
	Use:   "provision <name>",
	Short: "Deliver a shed's credentials again and finish its owed steps",
	Long: `Ask your credential source (the machine you bound with 'shed source use')
for this shed's files and environment values, write them into the running
shed, and finish whatever its create left owed: the repository clone and the
install hook. Each step runs only when it is owed or failed, except that the
delivered files are always written again when your source answers.

On a shed whose create finished, this is a re-delivery and nothing else. A
file delivered before and no longer in your source's answer stays where it
is. An existing project directory is never removed.

Only the shed's own user can provision it. The shed must be running.

--no-provision (on create) skips the install hook, not delivery; the hook is
recorded as skipped, so this command does not run it later.

With --status, print the shed's delivery record instead: its state, each
step, the last delivery and the delivered items (never their values).`,
	Args: cobra.ExactArgs(1),
	RunE: runProvision,
}

func init() {
	provisionCmd.Flags().BoolVar(&provisionStatus, "status", false, "Print the shed's delivery record instead of provisioning")
	provisionCmd.Flags().DurationVar(&provisionTimeout, "timeout", 0, "Timeout for the provision (default: from config or 10m)")
	rootCmd.AddCommand(provisionCmd)
}

func runProvision(cmd *cobra.Command, args []string) error {
	name := args[0]
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}
	timeout := provisionTimeout
	if timeout == 0 {
		timeout = clientConfig.GetCreateTimeout()
	}
	client := NewAPIClientFromNamedEntry(serverName, entry, timeout)

	if provisionStatus {
		shed, err := client.GetShed(name)
		if err != nil {
			return fmt.Errorf("failed to read shed: %w", err)
		}
		if jsonFlag {
			return outputJSON(shed.Delivery)
		}
		renderDeliveryStatus(os.Stdout, shed)
		return nil
	}

	onProgress, finish, _ := progressSink(jsonFlag)
	shed, err := client.ProvisionShedWithProgress(name, onProgress)
	finish()
	if err != nil {
		return fmt.Errorf("failed to provision shed: %w", err)
	}
	if jsonFlag {
		return outputJSON(ActionResult{Status: "ok", Action: "provisioned", Name: name, Server: serverName, Details: shed})
	}
	d := shed.Delivery
	switch {
	case d == nil:
		printSuccess("Provisioned shed %s", name)
	case d.State == delivery.StateDelivered:
		printSuccess("Provisioned shed %s: delivered", name)
	case d.State == delivery.StateNotApplicable || d.State == delivery.StateNone:
		printSuccess("Provisioned shed %s: %s", name, deliveryStateText(d.State))
	default:
		fmt.Printf("Provision of shed %s ended: %s\n", name, deliveryStateText(d.State))
		if len(d.Owed) > 0 {
			fmt.Fprintf(os.Stderr, "Still owed: %s. See: shed provision %s --status\n", strings.Join(d.Owed, ", "), name)
		}
	}
	return nil
}

// deliveryStateText is a state as a person reads it.
func deliveryStateText(s delivery.State) string {
	switch s {
	case delivery.StateNone:
		return "no delivery record"
	case delivery.StateNotApplicable:
		return "no delivery (no source bound)"
	case delivery.StateNotDelivered:
		return "not delivered"
	}
	return string(s)
}

// renderDeliveryStatus prints a shed's delivery record: what `shed provision
// <name> --status` shows. Every value it prints is a name, a state, a time
// or a path: the record holds no delivered value. Server text is escaped.
func renderDeliveryStatus(out io.Writer, shed *config.Shed) {
	safe := sdkbootstrap.SafeText
	d := shed.Delivery
	if d == nil {
		fmt.Fprintf(out, "Shed %s: this server does not report delivery.\n", safe(shed.Name))
		return
	}
	fmt.Fprintf(out, "Delivery:  %s\n", safe(string(d.State)))
	if d.Last != nil {
		src := ""
		if d.Last.Source != nil {
			src = fmt.Sprintf(" from %s", safe(d.Last.Source.MachineName))
		}
		fmt.Fprintf(out, "Last:      %s (%s) %s%s\n", safe(d.Last.At), safe(string(d.Last.Reason)), safe(string(d.Last.Outcome)), src)
		if d.Last.Failure != nil {
			fmt.Fprintf(out, "Failure:   %s\n", safe(delivery.FailureText(d.Last.Failure)))
		}
	}
	if d.Steps != nil {
		fmt.Fprintln(out, "Steps:")
		for _, st := range []struct {
			name string
			step delivery.Step
		}{{"stage1", d.Steps.Stage1}, {"clone", d.Steps.Clone}, {"stage2", d.Steps.Stage2}, {"install", d.Steps.Install}} {
			line := fmt.Sprintf("  %-8s %s", st.name, safe(string(st.step.State)))
			if st.step.Reason != "" {
				line += " (" + safe(string(st.step.Reason)) + ")"
			}
			fmt.Fprintln(out, line)
		}
	}
	if len(d.Items) > 0 {
		fmt.Fprintln(out, "Items:")
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NAME\tPROVIDER\tSTAGE\tTARGET\tRESULT\tWRITTEN\tEXPIRES")
		for _, it := range d.Items {
			fmt.Fprintf(w, "  %s\t%s\t%d\t%s\t%s\t%s\t%s\n", safe(it.Name), safe(it.Provider), it.Stage,
				safe(itemTarget(it, shed.LandingDir)), safe(string(it.Result)), valueOrDash(safe(it.WrittenAt)), valueOrDash(safe(it.ExpiresAt)))
		}
		w.Flush()
	}
	if d.Last != nil && len(d.Last.Problems) > 0 {
		fmt.Fprintln(out, "Problems the source reported:")
		for _, p := range d.Last.Problems {
			fmt.Fprintf(out, "  %s: %s: %s\n", safe(p.Provider), safe(p.Code), safe(p.Message))
		}
	}
	if len(d.Expired) > 0 {
		fmt.Fprintf(out, "Expired:   %s\n", safe(strings.Join(d.Expired, ", ")))
	}
	if len(d.Owed) > 0 {
		fmt.Fprintf(out, "Owed:      %s. Run: shed provision %s\n", safe(strings.Join(d.Owed, ", ")), safe(shed.Name))
	}
}

// itemTarget is where an item went: ~/path for a home file, the project
// directory's path for a project file, $VARIABLE for an env value.
func itemTarget(it delivery.RecordItem, projectDir string) string {
	switch {
	case it.Kind == delivery.KindEnv:
		return "$" + it.Target
	case it.Base == delivery.BaseProject && projectDir != "":
		return strings.TrimSuffix(projectDir, "/") + "/" + it.Target
	case it.Base == delivery.BaseProject:
		return "<project>/" + it.Target
	}
	return "~/" + it.Target
}

// warnExpiredItems prints one warning per delivered item of shed that has
// expired: what `shed start` says (start asks no source).
func warnExpiredItems(w io.Writer, shed *config.Shed) {
	if shed == nil || shed.Delivery == nil {
		return
	}
	for _, name := range shed.Delivery.Expired {
		fmt.Fprintf(w, "Warning: delivered item %s has expired. Run: shed provision %s\n", sdkbootstrap.SafeText(name), sdkbootstrap.SafeText(shed.Name))
	}
}
