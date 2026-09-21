package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var (
	attachSessionFlag string
	attachNewFlag     bool
	attachTmuxFlag    bool
)

var attachCmd = &cobra.Command{
	Use:   "attach <name>",
	Short: "Attach to a shed: a roost tab, or a tmux session",
	Long: `Attach to a shed.

With a local roost app running, the shed is opened as a roost tab and this
terminal is left alone. Without one -- or with --tmux, or SHED_ATTACH=tmux --
this attaches to (or creates) a tmux session in the shed and drops you into it,
exactly as it always has.

--session/-S names the roost tab on the first path and the tmux session on the
second; --new forces a new one either way.

Examples:
  shed attach myproj                         # roost tab "default", or the tmux session
  shed attach myproj --session debug         # a named tab / tmux session
  shed attach myproj --new --session review  # force-create a new tab / session
  shed attach myproj --tmux                  # the tmux path, even with roost running`,
	Args: cobra.ExactArgs(1),
	RunE: runAttach,
}

func init() {
	attachCmd.Flags().StringVarP(&attachSessionFlag, "session", "S", config.DefaultSessionName, "Session name to attach to")
	attachCmd.Flags().BoolVar(&attachNewFlag, "new", false, "Force create a new session (error if exists)")
	attachCmd.Flags().BoolVar(&attachTmuxFlag, "tmux", false, "Use the tmux path even when a local roost app is running (same as SHED_ATTACH=tmux)")

	rootCmd.AddCommand(attachCmd)
}

func runAttach(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Validate the session name before touching the network, so a bad
	// invocation fails fast without auto-starting a stopped shed.
	if err := config.ValidateSessionName(attachSessionFlag); err != nil {
		return fmt.Errorf("invalid session name: %w", err)
	}

	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}
	client := NewAPIClientFromNamedEntry(serverName, entry, clientConfig.GetCreateTimeout())
	shed, err := ensureRunningShed(client, name)
	if err != nil {
		return err
	}

	return attachShed(name, serverName, entry, shed)
}

// attachPlain attaches to (or creates) a named tmux session — the FLOOR the
// roost path in attach_roost.go falls back to, and the only path a machine
// with no roost ever takes. (The session name was validated in runAttach.)
//
// Its argv is pinned byte-for-byte by testdata/attach_tmux_argv.golden.json:
// whatever the roost path does, this must keep producing exactly what it
// produced before roost existed.
func attachPlain(name, serverName string, entry *config.ServerEntry, shed *config.Shed) error {
	if attachNewFlag {
		sessions, err := listShedSessions(serverName, entry, name)
		if err != nil {
			return fmt.Errorf("failed to check existing sessions: %w", err)
		}
		for _, s := range sessions {
			if s.Name == attachSessionFlag {
				return fmt.Errorf("session %q already exists (use without --new to attach)", attachSessionFlag)
			}
		}
	}
	if verboseLevel > 0 {
		fmt.Printf("Attaching to session %q in %s on %s...\n", attachSessionFlag, name, serverName)
	}
	landingDir := shed.LandingDir
	if landingDir == "" {
		landingDir = config.HomePath
	}
	var tmuxCmd string
	if attachNewFlag {
		tmuxCmd = fmt.Sprintf("tmux new-session -s %s -c %s", attachSessionFlag, shellQuoteArg(landingDir))
	} else {
		tmuxCmd = fmt.Sprintf("tmux new-session -A -s %s -c %s", attachSessionFlag, shellQuoteArg(landingDir))
	}
	return execSSHTmux(name, entry, tmuxCmd)
}

// execSSH is the process-replacement seam execSSHTmux hands off to. Production
// wires it to the real syscall.Exec; a test overrides it to capture the argv
// syscall.Exec would have received instead of actually replacing the process
// (see TestAttachPlainTmuxArgvGolden). Never swapped outside tests.
var execSSH = syscall.Exec

// execSSHTmux replaces this process with `ssh -t … <tmuxCmd>` (the plain
// attach path). Interactive, so it intentionally omits BatchMode/ConnectTimeout
// (an interactive session may legitimately prompt); the rest is shared via
// baseSSHArgs.
func execSSHTmux(name string, entry *config.ServerEntry, tmuxCmd string) error {
	sshArgs := append([]string{"ssh", "-t"}, baseSSHArgs(name, entry)...)
	sshArgs = append(sshArgs, "--", tmuxCmd)
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}
	if err := execSSH(sshPath, sshArgs, os.Environ()); err != nil {
		return fmt.Errorf("failed to exec ssh: %w", err)
	}
	return nil
}

// listShedSessions is a thin wrapper so attachPlain can list sessions for the
// --new guard without reaching for a package-level client. Only the session rows
// are needed here (a name-existence check); warnings are for `shed sessions`.
func listShedSessions(serverName string, entry *config.ServerEntry, name string) ([]config.Session, error) {
	resp, err := NewAPIClientFromNamedEntry(serverName, entry, DefaultTimeout).ListSessions(name)
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}
