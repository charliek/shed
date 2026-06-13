package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var tokenScope string

var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Manage HTTP API bearer tokens",
}

var tokenNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Generate a new scoped bearer token",
	Long: `Generate a new bearer token for the HTTP API.

Add the printed token to the server config under auth.http.tokens (with its
scope), and to the matching client in ~/.shed/config.yaml — control_token for
the CLI and desktop, credentials_token for the host-agent.`,
	Args: cobra.NoArgs,
	RunE: runTokenNew,
}

func init() {
	tokenNewCmd.Flags().StringVar(&tokenScope, "scope", config.TokenScopeControl,
		"token scope: control, credentials, or admin")
	tokenCmd.AddCommand(tokenNewCmd)
	rootCmd.AddCommand(tokenCmd)
}

func runTokenNew(cmd *cobra.Command, args []string) error {
	tok, err := config.GenerateToken(tokenScope)
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}
