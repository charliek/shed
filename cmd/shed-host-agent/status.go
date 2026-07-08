package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"text/tabwriter"
	"time"

	"github.com/charliek/shed/internal/ext/protocol"
	sdk "github.com/charliek/shed/sdk"
)

// statusNamespaces is the provider order shown in the status report.
var statusNamespaces = []string{
	protocol.NamespaceSSHAgent,
	protocol.NamespaceAWSCredentials,
	protocol.NamespaceDockerCredentials,
}

// runStatus queries the running daemon over the read-only status socket and
// prints its authoritative self-report. `status` is live-only: it never reads
// the config file (the daemon owns the truth about what it loaded), so it can
// never disagree with the running service. Returns a process exit code: 0 on a
// report, 1 when the agent isn't running.
func runStatus(jsonOut bool, out io.Writer) int {
	sock := statusSocketPath()
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shed-host-agent is not running — nothing is listening at %s\n", sock)
		fmt.Fprintln(os.Stderr, "Start it (Homebrew): brew services start shed-host-agent")
		fmt.Fprintln(os.Stderr, "  or run it directly: shed-host-agent -config <path>")
		return 1
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	var ls LiveStatus
	if err := json.NewDecoder(conn).Decode(&ls); err != nil {
		fmt.Fprintf(os.Stderr, "status: reading from the agent: %v\n", err)
		return 1
	}
	// Schema bumps only on a breaking change (additive fields don't — see
	// host-agent-ipc.md), so any mismatch means the payload can't be trusted:
	// refuse rather than render a possibly-misleading report.
	if ls.Schema != statusSchemaVersion {
		fmt.Fprintf(os.Stderr, "status: agent status schema is %d, this CLI expects %d "+
			"(version skew — match the shed-host-agent binary and CLI versions)\n", ls.Schema, statusSchemaVersion)
		return 1
	}
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(ls); err != nil {
			fmt.Fprintln(os.Stderr, "status: encode:", err)
			return 1
		}
		return 0
	}
	renderStatus(out, ls)
	return 0
}

// renderStatus writes the human-readable self-report.
func renderStatus(out io.Writer, ls LiveStatus) {
	fmt.Fprintf(out, "shed-host-agent status (pid %d, %s)\n", ls.Pid, ls.Version)
	if ls.ConfigPath != "" {
		fmt.Fprintf(out, "config:   %s\n", ls.ConfigPath)
	}
	fmt.Fprintf(out, "started:  %s\n", ls.StartedAt)
	fmt.Fprintln(out)

	gated := make(map[string]bool, len(ls.GateNamespaces))
	for _, ns := range ls.GateNamespaces {
		gated[ns] = true
	}
	fmt.Fprintln(out, "Approval policies:")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	for _, ns := range statusNamespaces {
		note := ""
		if gated[ns] {
			note = "(decided in shed-desktop)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", ns, ls.Policies[ns], note)
	}
	tw.Flush()
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Approval channel:")
	fmt.Fprintf(out, "  socket    %s\n", ls.ApprovalChannel.SocketPath)
	if ls.ApprovalChannel.ConsumerConnected {
		fmt.Fprintf(out, "  consumer  connected%s\n", clientSuffix(ls.ApprovalChannel))
	} else {
		fmt.Fprintln(out, "  consumer  none connected (shed-desktop-policy requests fail closed)")
	}
	fmt.Fprintln(out)

	fmt.Fprintf(out, "Servers (%d):\n", len(ls.Servers))
	if len(ls.Servers) == 0 {
		fmt.Fprintln(out, "  (none being watched)")
		return
	}
	for _, sv := range ls.Servers {
		fmt.Fprintf(out, "  %s  (%s)\n", serverLabel(sv.Name), sv.URL)
		if len(sv.Namespaces) == 0 {
			fmt.Fprintln(out, "    (no subscriptions yet)")
			continue
		}
		stw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		for _, ns := range sv.Namespaces {
			detail := ns.State
			if ns.LastError != "" {
				detail += ": " + ns.LastError
			}
			fmt.Fprintf(stw, "    %s\t%s\t%s\n", connMark(ns.State), ns.Namespace, detail)
		}
		stw.Flush()
	}
}

// clientSuffix renders the connected consumer's identity, e.g. " (ShedDesktop 1.2.0)".
func clientSuffix(ac ApprovalChannelStatus) string {
	if ac.ClientName == "" {
		return ""
	}
	if ac.ClientVersion == "" {
		return " (" + ac.ClientName + ")"
	}
	return " (" + ac.ClientName + " " + ac.ClientVersion + ")"
}

func serverLabel(name string) string {
	if name == "" {
		return "(default)"
	}
	return name
}

func connMark(state string) string {
	switch state {
	case sdk.ConnConnected:
		return "ok"
	case sdk.ConnStopped:
		return "-"
	default:
		return "x"
	}
}
