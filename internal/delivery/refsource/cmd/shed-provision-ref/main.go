// Command shed-provision-ref runs the provision contract's reference source,
// for tests: a machine that holds its user's delivery stream on a
// shed-server and answers each request from a file of canned items, or
// misbehaves as told.
//
// It is built by `make provision-ref` into bin/test/ and is part of no
// release.
//
//	shed-provision-ref --server <url> [--cert <pem> --key <pem>] [--ca <pem>]
//	                   [--machine-id <hex>] [--machine-name <name>]
//	                   [--items <file>] [--refuse-fixture <file>]
//	                   [--silent] [--delay <duration>] [--twice]
//	                   [--second-connection] [--answer-machine-id <hex>]
//	                   [--answer-cert <pem> --answer-key <pem>] [--once]
//	shed-provision-ref --render-fixture <file>
//
// --render-fixture prints an answer fixture's body (the fixture language's
// answer, answer_text or answer_base64, with its fill), answering the
// fixtures' own request id, and exits: what a harness sends as the body of
// a reply it plays itself (the integration suite's hosted cases).
//
// --answer-cert answers with another certificate but the stream's own
// machine and stream ids; --answer-machine-id names another machine;
// --second-connection sends a stream id that is not the stream's. Each is
// one thing a server must refuse with not_stream_owner.
//
// It prints one JSON line per event on standard output (stream, refused,
// request, invalid_request, answer, dropped, end) and runs until it is
// interrupted, or, with --once, until its first stream ends.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/charliek/shed/internal/delivery/refsource"
)

const usage = `usage: shed-provision-ref --server <url> [--cert <pem> --key <pem>] [--ca <pem>]
                          [--machine-id <hex>] [--machine-name <name>]
                          [--items <file>] [--refuse-fixture <file>]
                          [--silent] [--delay <duration>] [--twice]
                          [--second-connection] [--answer-machine-id <hex>]
                          [--answer-cert <pem> --answer-key <pem>] [--once]
       shed-provision-ref --render-fixture <file>
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "shed-provision-ref:", err)
		os.Exit(2)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("shed-provision-ref", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	server := fs.String("server", "", "the shed-server's base URL")
	certFile := fs.String("cert", "", "the machine's client certificate (PEM), for an mtls server")
	keyFile := fs.String("key", "", "the client certificate's private key (PEM)")
	caFile := fs.String("ca", "", "the CA certificate (PEM) the server's certificate is checked against")
	machineID := fs.String("machine-id", "", "the machine id, 32 lowercase hex digits (default: a fresh one)")
	machineName := fs.String("machine-name", "provision-ref", "the machine's display name")
	itemsFile := fs.String("items", "", `canned items: a JSON file {"items": [...], "problems": [...]} (default: an empty answer)`)
	refuseFile := fs.String("refuse-fixture", "", "answer every request with this answer fixture of the contract instead")
	silent := fs.Bool("silent", false, "receive requests and answer none")
	delay := fs.Duration("delay", 0, "wait this long before answering")
	twice := fs.Bool("twice", false, "post every answer twice")
	second := fs.Bool("second-connection", false, "answer as a second connection that did not open the stream: own connections, a random stream id")
	answerMachine := fs.String("answer-machine-id", "", "answer as this machine id instead (another machine)")
	answerCert := fs.String("answer-cert", "", "answer with this client certificate instead, naming the stream's machine and stream ids (with --answer-key)")
	answerKey := fs.String("answer-key", "", "the answer certificate's private key (PEM)")
	once := fs.Bool("once", false, "exit when the first stream ends")
	render := fs.String("render-fixture", "", "print this answer fixture's body (for the fixtures' request id) and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *render != "" {
		f, err := refsource.LoadAnswerFixture(*render)
		if err != nil {
			return err
		}
		body, err := f.Body(refsource.FixtureRequestID)
		if err != nil {
			return err
		}
		_, err = stdout.Write(body)
		return err
	}

	client, err := httpClient(*certFile, *keyFile, *caFile)
	if err != nil {
		return err
	}
	cfg := refsource.Config{
		Server:           *server,
		Client:           client,
		MachineID:        *machineID,
		MachineName:      *machineName,
		AnswerMachineID:  *answerMachine,
		Silent:           *silent,
		Delay:            *delay,
		Twice:            *twice,
		SecondConnection: *second,
	}
	if cfg.MachineID == "" {
		cfg.MachineID = refsource.NewMachineID()
	}
	if (*answerCert == "") != (*answerKey == "") {
		return errors.New("--answer-cert and --answer-key go together")
	}
	if *answerCert != "" {
		other, err := httpClient(*answerCert, *answerKey, *caFile)
		if err != nil {
			return err
		}
		cfg.AnswerClient = other
	}
	if *itemsFile != "" {
		raw, err := os.ReadFile(*itemsFile)
		if err != nil {
			return err
		}
		if cfg.Canned, err = refsource.ParseCanned(raw); err != nil {
			return err
		}
	}
	if *refuseFile != "" {
		f, err := refsource.LoadAnswerFixture(*refuseFile)
		if err != nil {
			return err
		}
		cfg.Refuse = f
	}
	var mu sync.Mutex
	enc := json.NewEncoder(stdout)
	cfg.Report = func(e refsource.Event) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(e)
	}
	src, err := refsource.New(cfg)
	if err != nil {
		return err
	}
	if *once {
		return src.RunStream(ctx)
	}
	return src.Run(ctx)
}

// httpClient is a client with the machine's certificate (when given) and
// the CA (when given) as its only root.
func httpClient(certFile, keyFile, caFile string) (*http.Client, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("--cert and --key go together")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if certFile != "" {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificate", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr.TLSClientConfig = tlsCfg
	return &http.Client{Transport: tr}, nil
}
