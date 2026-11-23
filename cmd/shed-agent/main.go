//go:build linux
// +build linux

// Package main implements the shed-agent, which runs inside Firecracker VMs
// and handles command execution via vsock (or, in a cloud shed, unix
// sockets that the server reaches through SSH).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/remote/guest"
)

// agentFlags are the agent daemon's flags.
type agentFlags struct {
	consolePort, notifyPort, tcpProxyPort, httpPort uint
	shedName, listenSpec, socketGroup, layoutFile   string
}

// errExtraArgs is a daemon command line with an argument that is not a
// flag: an unknown subcommand, or a word after the flags.
var errExtraArgs = errors.New("unexpected argument")

// parseAgentFlags parses the daemon's command line (without the program
// name). It refuses any positional argument; the argument itself is not
// echoed.
func parseAgentFlags(args []string, stderr io.Writer) (*agentFlags, error) {
	o := &agentFlags{}
	fs := flag.NewFlagSet("shed-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.UintVar(&o.consolePort, "console-port", DefaultConsolePort, "vsock port for console connections")
	fs.UintVar(&o.notifyPort, "notify-port", DefaultNotifyPort, "vsock port for message channel")
	fs.UintVar(&o.tcpProxyPort, "tcp-proxy-port", DefaultTCPProxyPort, "vsock port for TCP proxy (DialService)")
	fs.StringVar(&o.shedName, "shed-name", "", "shed instance name (defaults to hostname)")
	fs.UintVar(&o.httpPort, "http-port", DefaultHTTPPort, "localhost HTTP port for plugin API")
	fs.StringVar(&o.listenSpec, "listen", listenVsock, `where the agent listens: "vsock" or "unix:<dir>" (one <port>.sock per port)`)
	fs.StringVar(&o.socketGroup, "socket-group", "", "unix only: group that may open the sockets (dir 0750, sockets 0660); default owner only")
	fs.StringVar(&o.layoutFile, "layout-file", guest.LayoutFile, "file holding the guest layout number (missing = 0)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "shed-agent: unknown subcommand or unexpected argument; subcommands are mount-9p, version and "+delivery.WriterCommand+"; the daemon takes flags only")
		return nil, errExtraArgs
	}
	return o, nil
}

func main() {
	// Subcommand dispatch (bounds-checked)
	if len(os.Args) > 1 && os.Args[1] == "mount-9p" {
		runMount9P()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		os.Exit(runVersion(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == delivery.WriterCommand {
		os.Exit(runApplyItems(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}

	// Parse flags. Anything left over (an unknown subcommand, or an argument
	// after the flags) is refused: it must never start a second agent.
	opts, err := parseAgentFlags(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("shed-agent starting...")

	name := opts.shedName
	if name == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			log.Fatalf("Failed to resolve shed name from hostname: %v", err)
		}
		name = h
	}

	listen, err := parseListen(opts.listenSpec, opts.socketGroup)
	if err != nil {
		log.Fatalf("Invalid listen flags: %v", err)
	}

	// A bad layout file is reported, not fatal: the agent still serves, and
	// a server that needs a layout refuses layout 0.
	layout, err := readLayoutFile(opts.layoutFile)
	if err != nil {
		log.Printf("Warning: %v; reporting layout 0", err)
		layout = 0
	}

	// Create and start server
	server := NewServer(uint32(opts.consolePort), uint32(opts.notifyPort), uint32(opts.tcpProxyPort), uint32(opts.httpPort), name)
	server.listenCfg = listen
	server.info = buildAgentInfo(layout)
	if err := server.Start(); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

	log.Printf("shed-agent ready")

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Printf("Shutting down...")

	server.Stop()
	log.Printf("Goodbye")
}
