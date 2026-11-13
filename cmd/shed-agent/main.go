//go:build linux
// +build linux

// Package main implements the shed-agent, which runs inside Firecracker VMs
// and handles command execution via vsock (or, in a cloud shed, unix
// sockets that the server reaches through SSH).
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/charliek/shed/internal/remote/guest"
)

func main() {
	// Subcommand dispatch (bounds-checked)
	if len(os.Args) > 1 && os.Args[1] == "mount-9p" {
		runMount9P()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		os.Exit(runVersion(os.Args[2:], os.Stdout, os.Stderr))
	}

	// Parse flags
	consolePort := flag.Uint("console-port", DefaultConsolePort, "vsock port for console connections")
	notifyPort := flag.Uint("notify-port", DefaultNotifyPort, "vsock port for message channel")
	tcpProxyPort := flag.Uint("tcp-proxy-port", DefaultTCPProxyPort, "vsock port for TCP proxy (DialService)")
	shedName := flag.String("shed-name", "", "shed instance name (defaults to hostname)")
	httpPort := flag.Uint("http-port", DefaultHTTPPort, "localhost HTTP port for plugin API")
	listenSpec := flag.String("listen", listenVsock, `where the agent listens: "vsock" or "unix:<dir>" (one <port>.sock per port)`)
	socketGroup := flag.String("socket-group", "", "unix only: group that may open the sockets (dir 0750, sockets 0660); default owner only")
	layoutFile := flag.String("layout-file", guest.LayoutFile, "file holding the guest layout number (missing = 0)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("shed-agent starting...")

	name := *shedName
	if name == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			log.Fatalf("Failed to resolve shed name from hostname: %v", err)
		}
		name = h
	}

	listen, err := parseListen(*listenSpec, *socketGroup)
	if err != nil {
		log.Fatalf("Invalid listen flags: %v", err)
	}

	// A bad layout file is reported, not fatal: the agent still serves, and
	// a server that needs a layout refuses layout 0.
	layout, err := readLayoutFile(*layoutFile)
	if err != nil {
		log.Printf("Warning: %v; reporting layout 0", err)
		layout = 0
	}

	// Create and start server
	server := NewServer(uint32(*consolePort), uint32(*notifyPort), uint32(*tcpProxyPort), uint32(*httpPort), name)
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
