package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/charliek/shed/internal/config"
)

var egressProfileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage runtime user egress profiles (no server-config edit)",
	Long: `Manage user-defined egress profiles stored on the server at runtime.

A user profile is a named set of allow/deny/rule, authored as a whole document
(--file or 'edit'), and referenced by sheds exactly like a server-config profile
(shed create --egress <name>, shed egress set <shed> --profile <name>). Editing a
profile live-re-pushes running sheds that use it. Server-config profiles are a
read-only baseline and cannot be changed here.`,
}

var egressProfileFile string

var egressProfileLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List egress profiles (config baseline + user)",
	Args:  cobra.NoArgs,
	RunE:  runEgressProfileLs,
}

var egressProfileShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show one egress profile",
	Args:  cobra.ExactArgs(1),
	RunE:  runEgressProfileShow,
}

var egressProfileSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Create or replace a user profile from a file",
	Long: `Create or replace a user egress profile from a YAML file (whole document).

The file is a profile fragment:
  allow: [example.com, "*.internal.corp"]
  deny:  [tracker.io]
  rule:  'port == 443'   # optional CEL
  # mode: audit          # optional

Example:
  shed egress profile set my-stack --file ./my-stack.yaml`,
	Args: cobra.ExactArgs(1),
	RunE: runEgressProfileSet,
}

var egressProfileEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Create or edit a user profile in $EDITOR",
	Args:  cobra.ExactArgs(1),
	RunE:  runEgressProfileEdit,
}

var egressProfileRmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Remove a user profile",
	Args:  cobra.ExactArgs(1),
	RunE:  runEgressProfileRm,
}

func init() {
	egressProfileSetCmd.Flags().StringVar(&egressProfileFile, "file", "", "Path to a profile YAML document (required)")
	egressProfileCmd.AddCommand(egressProfileLsCmd, egressProfileShowCmd, egressProfileSetCmd, egressProfileEditCmd, egressProfileRmCmd)
	egressCmd.AddCommand(egressProfileCmd)
}

func egressProfileClient() (*APIClient, error) {
	entry, _, err := getServerEntry()
	if err != nil {
		return nil, err
	}
	return NewAPIClientFromEntry(entry, DefaultTimeout), nil
}

func runEgressProfileLs(cmd *cobra.Command, args []string) error {
	client, err := egressProfileClient()
	if err != nil {
		return err
	}
	infos, err := client.EgressProfilesList()
	if err != nil {
		return fmt.Errorf("failed to list egress profiles: %w", err)
	}
	if jsonFlag {
		return outputJSON(infos)
	}
	if len(infos) == 0 {
		fmt.Println("No egress profiles defined.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSOURCE\tRULES")
	for _, info := range infos {
		fmt.Fprintf(w, "%s\t%s\t%s\n", info.Name, info.Source, egressProfileSummary(info.Profile))
	}
	return w.Flush()
}

func runEgressProfileShow(cmd *cobra.Command, args []string) error {
	client, err := egressProfileClient()
	if err != nil {
		return err
	}
	info, err := client.EgressProfileGet(args[0])
	if err != nil {
		return fmt.Errorf("failed to get egress profile %s: %w", args[0], err)
	}
	if jsonFlag {
		return outputJSON(info)
	}
	fmt.Printf("Name:   %s\n", info.Name)
	fmt.Printf("Source: %s\n", info.Source)
	out, _ := yaml.Marshal(info.Profile)
	fmt.Printf("\n%s", out)
	return nil
}

func runEgressProfileSet(cmd *cobra.Command, args []string) error {
	if egressProfileFile == "" {
		return fmt.Errorf("--file is required (path to a profile YAML document)")
	}
	data, err := os.ReadFile(egressProfileFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", egressProfileFile, err)
	}
	p, err := decodeEgressProfileYAML(data)
	if err != nil {
		return fmt.Errorf("parse %s: %w", egressProfileFile, err)
	}
	client, err := egressProfileClient()
	if err != nil {
		return err
	}
	info, err := client.EgressProfilePut(args[0], p)
	if err != nil {
		return fmt.Errorf("failed to set egress profile %s: %w", args[0], err)
	}
	if jsonFlag {
		return outputJSON(info)
	}
	fmt.Printf("Egress profile %q saved.\n", info.Name)
	return nil
}

func runEgressProfileRm(cmd *cobra.Command, args []string) error {
	client, err := egressProfileClient()
	if err != nil {
		return err
	}
	if err := client.EgressProfileDelete(args[0]); err != nil {
		return fmt.Errorf("failed to remove egress profile %s: %w", args[0], err)
	}
	if jsonFlag {
		return outputJSON(map[string]string{"status": "ok", "profile": args[0]})
	}
	fmt.Printf("Egress profile %q removed.\n", args[0])
	return nil
}

const egressProfileTemplate = `# Egress profile — edit and save to create/update it. An unchanged file aborts.
# allow/deny are domain globs: "*.github.com" matches subdomains (not the apex),
# "github.com" matches exactly. rule is an optional CEL expression. mode: audit
# allows + logs all non-guard traffic.
allow:
  - example.com
# deny:
#   - tracker.io
# rule: 'port == 443'
# mode: audit
`

func runEgressProfileEdit(cmd *cobra.Command, args []string) error {
	name := args[0]
	client, err := egressProfileClient()
	if err != nil {
		return err
	}
	// Seed from the existing profile, or a template for a new one. A GET error is
	// treated as "new profile" (most commonly a 404).
	seed := []byte(egressProfileTemplate)
	if info, err := client.EgressProfileGet(name); err == nil {
		if info.Source == "config" { // server-config baseline is read-only
			return fmt.Errorf("profile %q is a server-config profile (read-only); it can't be edited here", name)
		}
		if out, mErr := yaml.Marshal(info.Profile); mErr == nil {
			seed = out
		}
	} else if !strings.Contains(err.Error(), config.ErrProfileNotFound) {
		// Only a genuine not-found falls back to the template (create-on-save);
		// surface any other error rather than risk overwriting on save.
		return fmt.Errorf("failed to read egress profile %s: %w", name, err)
	}
	edited, err := editInEditor(name, seed)
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(edited), bytes.TrimSpace(seed)) {
		fmt.Println("No changes; aborted.")
		return nil
	}
	p, err := decodeEgressProfileYAML(edited)
	if err != nil {
		return fmt.Errorf("parse edited profile: %w", err)
	}
	info, err := client.EgressProfilePut(name, p)
	if err != nil {
		return fmt.Errorf("failed to save egress profile %s: %w", name, err)
	}
	fmt.Printf("Egress profile %q saved.\n", info.Name)
	return nil
}

// editInEditor writes seed to a temp file, opens $EDITOR (or vi) on it, and
// returns the edited bytes.
func editInEditor(name string, seed []byte) ([]byte, error) {
	f, err := os.CreateTemp("", "shed-egress-"+name+"-*.yaml")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(seed); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	ed := exec.Command(editor, tmp) //nolint:gosec // the user's own $EDITOR on a temp file
	ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := ed.Run(); err != nil {
		return nil, fmt.Errorf("editor %q: %w", editor, err)
	}
	return os.ReadFile(tmp)
}

// egressProfileSummary renders a one-line summary of a profile's rules for `ls`.
func egressProfileSummary(p config.EgressProfile) string {
	var parts []string
	if p.Mode != "" {
		parts = append(parts, "mode="+p.Mode)
	}
	if len(p.Allow) > 0 {
		parts = append(parts, "allow="+strings.Join(p.Allow, ","))
	}
	if len(p.Deny) > 0 {
		parts = append(parts, "deny="+strings.Join(p.Deny, ","))
	}
	if p.Rule != "" {
		parts = append(parts, "rule="+p.Rule)
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, " ")
}

// decodeEgressProfileYAML strict-decodes a profile document so a typo'd key
// (e.g. "allowed:") is rejected rather than silently dropped.
func decodeEgressProfileYAML(data []byte) (config.EgressProfile, error) {
	var p config.EgressProfile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return config.EgressProfile{}, err
	}
	return p, nil
}
