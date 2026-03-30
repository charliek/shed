//go:build linux
// +build linux

// Package main implements the shed-agent, which runs inside Firecracker VMs
// and handles command execution via vsock.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Parse flags
	consolePort := flag.Uint("console-port", DefaultConsolePort, "vsock port for console connections")
	healthPort := flag.Uint("health-port", DefaultHealthPort, "vsock port for health checks")
	notifyPort := flag.Uint("notify-port", DefaultNotifyPort, "vsock port for message channel")
	shedName := flag.String("shed-name", "", "shed instance name (defaults to hostname)")
	httpPort := flag.Uint("http-port", DefaultHTTPPort, "localhost HTTP port for plugin API")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("shed-agent starting...")

	name := *shedName
	if name == "" {
		h, _ := os.Hostname()
		name = h
	}

	// Create and start server
	server := NewServer(uint32(*consolePort), uint32(*healthPort), uint32(*notifyPort), uint32(*httpPort), name)
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
