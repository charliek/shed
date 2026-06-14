// Package main implements shed-egress-proxy, the host-side Level-1 egress
// filtering proxy. It is a child process of shed-server: shed-server connects to
// its control UDS to configure per-shed forward-proxy listeners and to stream
// audit records back. The proxy is tag-free (runs on both VZ/macOS and FC/Linux)
// and CGO-free.
//
// SECURITY POSTURE: Level 1 is cooperative/audit, NOT a security boundary. See
// internal/egress for the policy engine and the full bypass inventory.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/charliek/shed/internal/egress"
)

func main() {
	socket := flag.String("control-socket", "", "path to the shed-server control UDS (required)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if *socket == "" {
		log.Fatalf("shed-egress-proxy: --control-socket is required")
	}

	ln, err := egress.ListenControl(*socket)
	if err != nil {
		log.Fatalf("shed-egress-proxy: listen control socket %s: %v", *socket, err)
	}
	log.Printf("shed-egress-proxy listening on %s", *socket)

	ps := egress.NewProxyServer()

	// Accept control connections from shed-server. Each connection drives
	// configure/remove and carries the audit stream back; per-shed data-plane
	// listeners persist across control-connection drops so a server restart can
	// re-push without disrupting guests.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed on shutdown
			}
			go func(c net.Conn) {
				if err := ps.Serve(c); err != nil {
					log.Printf("control connection ended: %v", err)
				}
			}(conn)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shed-egress-proxy shutting down")
	ln.Close()
	_ = os.Remove(*socket)
}
