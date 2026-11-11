package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/netpolicy"
	"github.com/spf13/cobra"
)

var networkCmd = &cobra.Command{
	Use:   "network",
	Short: "Manage this server's network-policy rules",
}

var networkTeardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Delete this server's own nftables tables (bridge and inet shed_<name>)",
	Long: `Delete this server's own nftables tables, bridge shed_<name> and inet
shed_<name>, which enforce the isolated network profile. Nothing else is
touched: not the bridge, not the NAT or FORWARD rules, not any other table.
A missing table, or a missing nft, is success.

Isolated sheds lose their isolation until shed-server serve starts again and
reinstalls the tables; the guarded sheds found in this server's metadata are
listed.`,
	Args: cobra.NoArgs,
	RunE: runNetworkTeardown,
}

func init() {
	networkCmd.AddCommand(networkTeardownCmd)
	rootCmd.AddCommand(networkCmd)
}

func runNetworkTeardown(cmd *cobra.Command, _ []string) error {
	cfg, err := config.LoadServerConfigForCLI(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	return networkTeardown(cmd.Context(), cmd.OutOrStdout(), cfg, netpolicy.ExecRunner{Bin: "nft"})
}

// networkTeardown deletes the server's two tables through runner and reports
// the guarded sheds that lose their isolation.
func networkTeardown(ctx context.Context, w io.Writer, cfg *config.ServerConfig, runner netpolicy.Runner) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m := netpolicy.NewManager(netpolicy.Config{ServerName: cfg.Name}, runner, netpolicy.Open)
	if err := m.Teardown(ctx); err != nil {
		return err
	}
	fmt.Fprintf(w, "Deleted nftables tables bridge %s and inet %s (if present).\n", m.TableName(), m.TableName())
	if guarded := guardedShedNames(cfg); len(guarded) > 0 {
		fmt.Fprintf(w, "These isolated sheds are unguarded until shed-server serve starts again: %v\n", guarded)
	}
	return nil
}

// guardedShedNames reads the Firecracker instance metadata (when there is
// any and it is readable) and returns the sheds with a non-open profile. It
// decodes only the one field, so it works on every platform.
func guardedShedNames(cfg *config.ServerConfig) []string {
	if cfg.Firecracker == nil || cfg.Firecracker.InstanceDir == "" {
		return nil
	}
	dir := config.ExpandPath(cfg.Firecracker.InstanceDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), "metadata.json"))
		if err != nil {
			continue
		}
		var meta struct {
			Name           string `json:"name"`
			NetworkProfile string `json:"network_profile"`
		}
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		if config.EffectiveNetworkProfile(meta.NetworkProfile) != config.NetworkProfileOpen {
			name := meta.Name
			if name == "" {
				name = e.Name()
			}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
