// Command shed-directory-ref runs the reference implementation of the
// directory contract, for tests: plain HTTP on a loopback address only,
// with all of its state in memory and its users, codes and disabled
// servers read from a control directory (see refserver.ControlDir).
//
// It is also the compute contract's reference compute service, whose
// in-memory cloud is steered by compute.json in the control directory; the
// command applies that file, settles due operations and sweeps ended leases
// every second.
//
// It is built by `make directory-ref` into bin/test/ and is part of no
// release.
//
//	shed-directory-ref --control <dir> [--listen 127.0.0.1:0] [--issuer http://127.0.0.1:<port>]
//	                   [--organization example] [--signing-key <PKCS#8 PEM> --key-id <id>]
//	                   [--inventory <file>]
//
// --issuer serves under another loopback origin than the listening address:
// for a test relay in front of the server, which can refuse connections (the
// service down) while the server keeps its state. --inventory names a file
// the command rewrites every second with the compute service's machines,
// by server, so a test can confirm the machines it made are gone.
//
// It prints the issuer it serves as ("listening on <url>") and each
// enrolment ("enrolled <server name> <client id>") on standard output, one
// line each.
package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/directory/refserver"
)

// usage is the help text, before the flags' own lines.
const usage = `usage: shed-directory-ref --control <dir> [--listen 127.0.0.1:0] [--issuer http://127.0.0.1:<port>]
                          [--organization example] [--signing-key <PKCS#8 PEM> --key-id <id>]
                          [--inventory <file>]

Serves the directory contract over plain HTTP on a loopback address, for
tests. All state (enrolled servers, spent codes, the replay ledger, the
compute service's machines) is in memory; users, codes, disabled servers,
the servers that may not ask for deliveries (no-deliveries), each person's
delivery source (deliveries.json: the provision contract's hosted source,
POST /v1/deliveries), the compute service's steps, faults and settings
(compute.json: the compute contract, /v1/compute/*) and an outage are read
from the control directory on each request. After the clock moves back, an assertion
whose acceptance window ended before the latest time the server has seen is
refused until the clock catches up.

`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run runs the server until ctx ends, and returns the exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// The banner comes first, on every invocation, a flag error included.
	fmt.Fprintln(stderr, refserver.Banner)
	fs := flag.NewFlagSet("shed-directory-ref", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	listen := fs.String("listen", "127.0.0.1:0", "the loopback address and port to listen on")
	issuerFlag := fs.String("issuer", "", "the origin to serve as (default http://<the listening address>): http on a loopback address and port, for a test relay in front of the server")
	control := fs.String("control", "", "the control directory (users.json, codes, disabled, no-deliveries, deliveries.json, compute.json, outage)")
	org := fs.String("organization", "example", "the organization's slug")
	keyFile := fs.String("signing-key", "", "a PKCS#8 PEM RSA key to sign access tokens with (default: a new key)")
	keyID := fs.String("key-id", "", "the signing key's key id (with --signing-key)")
	inventory := fs.String("inventory", "", "a file to write, every second, the compute service's machines to: {\"servers\": {<server name>: [<shed id>...]}} (mode 0600), for tests")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *control == "" {
		fs.Usage()
		return 2
	}
	if err := checkLoopback(*listen); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *issuerFlag != "" {
		if err := checkLoopbackIssuer(*issuerFlag); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	inputs, err := refserver.OpenControlDir(*control)
	if err != nil {
		fmt.Fprintln(stderr, "control directory:", err)
		return 2
	}
	defer inputs.Close()
	var key *rsa.PrivateKey
	if *keyFile != "" {
		k, err := readRSAKey(*keyFile)
		if err != nil {
			fmt.Fprintln(stderr, "signing key:", err)
			return 2
		}
		key = k
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer ln.Close()
	if err := checkLoopback(ln.Addr().String()); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	issuer := "http://" + ln.Addr().String()
	if *issuerFlag != "" {
		issuer = *issuerFlag
	}

	var outMu sync.Mutex
	say := func(format string, args ...any) {
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Fprintf(stdout, format+"\n", args...)
	}
	logger := log.New(stderr, "", log.LstdFlags)
	srv, err := refserver.New(refserver.Config{
		Issuer:       issuer,
		Organization: *org,
		Inputs:       inputs,
		SigningKey:   key,
		KeyID:        *keyID,
		OnEnrol:      func(name, clientID string) { say("enrolled %s %s", name, clientID) },
		Logf:         logger.Printf,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second, // an enrolment body arrives whole within 10 seconds
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          logger,
	}
	say("listening on %s", srv.Issuer())
	done := make(chan error, 1)
	go func() { done <- hs.Serve(ln) }()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := srv.ComputeTick(); err != nil {
					logger.Printf("compute: %v", err)
				}
				if *inventory != "" {
					if err := writeInventory(*inventory, srv.ComputeInventory()); err != nil {
						logger.Printf("compute: inventory: %v", err)
					}
				}
			}
		}
	}()
	select {
	case err := <-done:
		fmt.Fprintln(stderr, err)
		return 1
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// checkLoopback refuses any listening address but a loopback IP literal
// (127.0.0.0/8 or ::1) with a port.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("--listen %q: the reference server listens on a loopback address only (127.0.0.1 or ::1)", addr)
	}
	return nil
}

// writeInventory replaces path with the machines by server, atomically,
// mode 0600.
func writeInventory(path string, inv map[string][]string) error {
	b, err := json.Marshal(map[string]any{"servers": inv})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".inventory-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// checkLoopbackIssuer refuses an --issuer that is not http://<loopback IP
// literal>:<port>, exactly (no path, no trailing slash).
func checkLoopbackIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "http" || u.Port() == "" || u.User != nil || u.Path != "" ||
		u.RawQuery != "" || u.Fragment != "" || u.String() != issuer {
		return fmt.Errorf("--issuer %q: want http://<loopback address>:<port>", issuer)
	}
	if err := checkLoopback(u.Host); err != nil {
		return fmt.Errorf("--issuer %q: a loopback address only (127.0.0.1 or ::1)", issuer)
	}
	return nil
}

// readRSAKey reads a PKCS#8 PEM RSA private key.
func readRSAKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PKCS#8 PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}
