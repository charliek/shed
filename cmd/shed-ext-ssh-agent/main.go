// shed-ext-ssh-agent is the guest-side SSH agent that runs inside shed microVMs.
// It implements the SSH agent protocol on a Unix domain socket and translates
// SSH operations into message bus requests to the host agent.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh/agent"

	"github.com/charliek/shed/internal/ext/sshagent"
	"github.com/charliek/shed/internal/version"
)

func main() {
	sockPath := flag.String("sock", "/run/shed-extensions/ssh-agent.sock", "Unix socket path")
	publishURL := flag.String("publish-url", "http://127.0.0.1:498/v1/publish", "shed-agent publish endpoint")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	logger.Info("starting shed-ext-ssh-agent", "version", version.Info(), "sock", *sockPath, "publish_url", *publishURL)

	// Remove stale socket file
	os.Remove(*sockPath)

	listener, err := net.Listen("unix", *sockPath)
	if err != nil {
		logger.Error("failed to listen", "path", *sockPath, "error", err)
		os.Exit(1)
	}

	// Make socket accessible
	if err := os.Chmod(*sockPath, 0600); err != nil {
		logger.Warn("failed to chmod socket", "error", err)
	}

	a := sshagent.New(sshagent.WithPublishURL(*publishURL))

	// Startup health check
	go func() {
		if err := a.Ping(2 * time.Second); err != nil {
			logger.Warn("shed-host-agent not connected for namespace 'ssh-agent'",
				"error", err,
				"hint", "SSH operations will fail until shed-host-agent is running on the host machine.",
			)
			writeStatus("ssh-agent: not connected")
		} else {
			logger.Info("shed-host-agent connected", "namespace", "ssh-agent")
			writeStatus("ssh-agent: connected")
		}
	}()

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-sigCh
		logger.Info("shutting down")
		cancel()
		listener.Close()
	}()

	logger.Info("listening", "sock", *sockPath)

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			logger.Error("accept failed", "error", err)
			continue
		}
		go func() {
			defer conn.Close()
			if err := agent.ServeAgent(a, conn); err != nil {
				logger.Debug("connection closed", "error", err)
			}
		}()
	}

	// Cleanup
	os.Remove(*sockPath)
	logger.Info("stopped")
}

// writeStatus writes the current status to a well-known file for programmatic
// consumption (e.g., by shed-ext status).
func writeStatus(status string) {
	_ = os.WriteFile("/run/shed-extensions/ssh-agent.status", []byte(status+"\n"), 0644)
}
