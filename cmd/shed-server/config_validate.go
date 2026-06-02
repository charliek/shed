package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var configValidateCmd = &cobra.Command{
	Use:   "config-validate",
	Short: "Validate the server configuration and exit",
	Long: `Load and validate the server configuration, exiting non-zero on any error.

Intended for packaging preflight (e.g. a deb upgrade checks the installed
config before restarting the service) and for operators verifying a config
edit. A config still carrying pre-v0.6.0 keys (base_rootfs / images) fails
here with a pointer to the upgrade guide.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		fmt.Println("config OK")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(configValidateCmd)
}
