package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var egressCmd = &cobra.Command{
	Use:   "egress",
	Short: "Inspect and control per-shed network egress",
	Long: `Inspect and control per-shed network egress policy.

Egress control is an opt-in, audit-first host-side proxy (off by default). It is
cooperative, not a security boundary — see the docs for the full bypass model.`,
}

var egressShowCmd = &cobra.Command{
	Use:   "show <shed-name>",
	Short: "Show a shed's egress profiles and recent decisions",
	Long: `Show a shed's active egress profiles, assigned listener port, the
resolved profile rules, and recent allow/deny decisions.

Examples:
  shed egress show web
  shed egress show web --json`,
	Args: cobra.ExactArgs(1),
	RunE: runEgressShow,
}

var egressSetProfiles []string

var egressSetCmd = &cobra.Command{
	Use:   "set <shed-name>",
	Short: "Set a shed's egress profiles (live on a running shed)",
	Long: `Set a shed's egress profiles. On a running shed the change applies
live (listener re-pushed, guest env re-injected); on a stopped shed it persists
and applies on next start. An empty selection or 'off' disables egress.

Examples:
  shed egress set web --profile base,github
  shed egress set web --profile off`,
	Args: cobra.ExactArgs(1),
	RunE: runEgressSet,
}

var egressOffCmd = &cobra.Command{
	Use:   "off <shed-name>",
	Short: "Turn egress control off for a shed",
	Args:  cobra.ExactArgs(1),
	RunE:  runEgressOff,
}

func init() {
	egressSetCmd.Flags().StringSliceVar(&egressSetProfiles, "profile", nil, "Egress profiles to apply (comma-separated; empty or 'off' disables)")
	egressCmd.AddCommand(egressShowCmd, egressSetCmd, egressOffCmd)
	rootCmd.AddCommand(egressCmd)
}

func runEgressSet(cmd *cobra.Command, args []string) error {
	name := args[0]
	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	shed, err := client.EgressSet(name, egressSetProfiles)
	if err != nil {
		return fmt.Errorf("failed to set egress for %s: %w", name, err)
	}
	if jsonFlag {
		return outputJSON(shed)
	}
	if len(shed.EgressProfiles) == 0 && shed.EgressPort == 0 {
		fmt.Printf("Egress turned off for %s.\n", name)
	} else {
		fmt.Printf("Egress updated for %s (profiles: %s).\n", name, strings.Join(shed.EgressProfiles, ", "))
	}
	return nil
}

func runEgressOff(cmd *cobra.Command, args []string) error {
	name := args[0]
	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	if _, err := client.EgressOff(name); err != nil {
		return fmt.Errorf("failed to turn egress off for %s: %w", name, err)
	}
	if jsonFlag {
		return outputJSON(map[string]string{"status": "ok", "shed": name})
	}
	fmt.Printf("Egress turned off for %s.\n", name)
	return nil
}

func runEgressShow(cmd *cobra.Command, args []string) error {
	name := args[0]
	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	status, err := client.EgressShow(name)
	if err != nil {
		return fmt.Errorf("failed to get egress status for %s: %w", name, err)
	}
	if jsonFlag {
		return outputJSON(status)
	}
	printEgressStatus(status)
	return nil
}

func printEgressStatus(s *config.EgressStatus) {
	if !s.Enabled {
		fmt.Println("Egress control is disabled on this server.")
		return
	}
	if s.Port == 0 && len(s.Profiles) == 0 {
		fmt.Printf("Shed %q has no egress control (unrestricted).\n", s.Shed)
		return
	}
	fmt.Printf("Shed:     %s\n", s.Shed)
	fmt.Printf("Profiles: %s\n", strings.Join(s.Profiles, ", "))
	fmt.Printf("Port:     %d\n", s.Port)

	if len(s.Rules) > 0 {
		fmt.Println("\nRules:")
		for name, def := range s.Rules {
			fmt.Printf("  %s:", name)
			if def.Mode != "" {
				fmt.Printf(" mode=%s", def.Mode)
			}
			if len(def.Allow) > 0 {
				fmt.Printf(" allow=%s", strings.Join(def.Allow, ","))
			}
			if len(def.Deny) > 0 {
				fmt.Printf(" deny=%s", strings.Join(def.Deny, ","))
			}
			if def.Rule != "" {
				fmt.Printf(" rule=%q", def.Rule)
			}
			fmt.Println()
		}
	}

	if len(s.Recent) > 0 {
		fmt.Println("\nRecent decisions (most recent last):")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  TIME\tVERDICT\tHOST\tPORT\tREASON")
		for _, rec := range s.Recent {
			fmt.Fprintf(w, "  %s\t%s\t%s\t%d\t%s\n", rec.Time.Format("15:04:05"), rec.Verdict, rec.Host, rec.Port, rec.Reason)
		}
		w.Flush()
	}
}
