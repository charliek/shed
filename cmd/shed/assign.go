package main

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var assignCmd = &cobra.Command{
	Use:   "assign <shed> <user>",
	Short: "Give a shed with no user to a user (admin)",
	Long: `Give a shed that has no user to one of the server's users (auth.users).

Sheds made before a server had a user list have no user. On a server with
exactly one user they become that user's when the server starts; on a server
with several, only an admin can see, stop and delete them, and nobody can
enter them, until an admin assigns each one here.

Only a shed with no user and no service client can be assigned: a shed is
never moved between users. A shed that mounts host directories (--local-dir,
--add-dir) or server mounts cannot be assigned either, because the user would
get those too; recreate it without them (--mounts none) instead. Once
assigned, the shed is that user's like any other: the admin who assigned it
can list, stop and delete it but not enter it.

Admin only. Use -s/--server to pick the server.`,
	Args: cobra.ExactArgs(2),
	RunE: runAssign,
}

func init() {
	rootCmd.AddCommand(assignCmd)
}

func runAssign(cmd *cobra.Command, args []string) error {
	name, user := args[0], args[1]
	if !config.ValidUserName(user) {
		return fmt.Errorf("%q is not a valid user name", user)
	}
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}
	client := NewAPIClientFromNamedEntry(serverName, entry, DefaultTimeout)
	shed, err := client.AssignShed(name, user)
	if err != nil {
		return fmt.Errorf("assign shed %s to %s on %s: %w", name, user, serverName, err)
	}
	if jsonFlag {
		return outputJSON(shed)
	}
	fmt.Printf("Shed %s on %s now belongs to user %s.\n", shed.Name, serverName, shed.User)
	return nil
}

// AssignShed gives a shed with no user to a user (admin only).
func (c *APIClient) AssignShed(name, user string) (*config.Shed, error) {
	var shed config.Shed
	req := config.AssignShedRequest{User: user}
	if err := c.doRequest(http.MethodPost, "/api/sheds/"+name+"/assign", req, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}
