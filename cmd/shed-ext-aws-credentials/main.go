// shed-ext-aws-credentials is the guest-side AWS credential proxy that runs inside shed
// microVMs. It serves the AWS container credential endpoint format and
// translates SDK requests into message bus requests to the host agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/ext/awsproxy"
	"github.com/charliek/shed/internal/version"
)

func main() {
	port := flag.Int("port", 499, "HTTP listen port")
	publishURL := flag.String("publish-url", "http://127.0.0.1:498/v1/publish", "shed-agent publish endpoint")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	logger.Info("starting shed-ext-aws-credentials", "version", version.Info(), "port", *port, "publish_url", *publishURL)

	proxy := awsproxy.New(
		awsproxy.WithPublishURL(*publishURL),
		awsproxy.WithLogger(logger),
	)

	// Startup health check
	go func() {
		if err := proxy.Ping(2 * time.Second); err != nil {
			logger.Warn("shed-host-agent not connected for namespace 'aws-credentials'",
				"error", err,
				"hint", "AWS operations will fail until shed-host-agent is running on the host machine.",
			)
			writeStatus("aws-credentials: not connected")
		} else {
			logger.Info("shed-host-agent connected", "namespace", "aws-credentials")
			writeStatus("aws-credentials: connected")
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/credentials", proxy.HandleCredentials)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-sigCh
		logger.Info("shutting down")
		cancel()
		if err := srv.Shutdown(context.Background()); err != nil {
			logger.Error("shutdown error", "error", err)
		}
	}()

	logger.Info("listening", "addr", addr)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		if ctx.Err() == nil {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}

	logger.Info("stopped")
}

func writeStatus(status string) {
	_ = os.WriteFile("/run/shed-extensions/aws-credentials.status", []byte(status+"\n"), 0644)
}
