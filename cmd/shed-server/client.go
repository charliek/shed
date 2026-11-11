package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// clientBundle is what `shed-server client issue` prints: everything an
// orchestrator needs to reach this server as a service client. client_key is
// a secret.
type clientBundle struct {
	Client             string `json:"client"`
	AuthMode           string `json:"auth_mode"`
	HTTPSPort          int    `json:"https_port"`
	TLSCertFingerprint string `json:"tls_cert_fingerprint"`
	ClientCert         string `json:"client_cert"`
	ClientKey          string `json:"client_key"`
	CertSerial         string `json:"cert_serial"`
	ExpiresAt          string `json:"expires_at"`
}

// clientListEntry is one row of `shed-server client list --json`.
type clientListEntry struct {
	Name         string   `json:"name"`
	CertMaxTTL   string   `json:"cert_max_ttl"`
	NamePrefix   string   `json:"name_prefix"`
	MaxSheds     int      `json:"max_sheds"`
	MaxAge       string   `json:"max_age"`
	Network      string   `json:"network"`
	ServerMounts []string `json:"server_mounts"`
	Images       []string `json:"images"`
	CapsCPUs     int      `json:"caps_cpus,omitempty"`
	CapsMemoryMB int      `json:"caps_memory_mb,omitempty"`
}

var (
	clientIssueTTL string
	clientIssueOut string
	clientListJSON bool
)

var clientCmd = &cobra.Command{
	Use:   "client",
	Short: "Manage service clients (auth.clients, mtls mode)",
	Long: `Service clients are orchestrators that reach this server with a client
certificate and may create, run commands in and delete only their own sheds,
within the policy under auth.clients. Run these commands on the shed host, as
the user that runs serve, with the same --config.`,
}

var clientIssueCmd = &cobra.Command{
	Use:   "issue <name>",
	Short: "Sign a certificate for a service client and print its JSON bundle",
	Long: `Signs a client certificate for <name> (which must be under auth.clients)
with this server's client CA, from the same state directory serve uses, and
prints one JSON object: client, auth_mode, https_port, tls_cert_fingerprint,
client_cert, client_key, cert_serial, expires_at. The bundle holds a private
key: store it as a secret. With --out it is written to a new file (mode 0600,
never overwriting) and a summary goes to stderr.

Revoke a client by removing it from auth.clients and restarting serve.`,
	Args: cobra.ExactArgs(1),
	RunE: runClientIssue,
}

var clientListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the configured service clients",
	Args:  cobra.NoArgs,
	RunE:  runClientList,
}

func init() {
	clientIssueCmd.Flags().StringVar(&clientIssueTTL, "ttl", "", "certificate lifetime (default and maximum: the client's cert_max_ttl)")
	clientIssueCmd.Flags().StringVar(&clientIssueOut, "out", "", "write the bundle to this new file (0600) instead of stdout")
	clientListCmd.Flags().BoolVar(&clientListJSON, "json", false, "print JSON")
	clientCmd.AddCommand(clientIssueCmd, clientListCmd)
	rootCmd.AddCommand(clientCmd)
}

func runClientIssue(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	var ttl time.Duration
	if clientIssueTTL != "" {
		ttl, err = time.ParseDuration(clientIssueTTL)
		if err != nil {
			return fmt.Errorf("invalid --ttl %q: %w", clientIssueTTL, err)
		}
		if ttl <= 0 {
			return fmt.Errorf("--ttl must be positive")
		}
	}
	bundle, err := issueClientBundle(cfg, args[0], ttl)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if clientIssueOut == "" {
		_, err = cmd.OutOrStdout().Write(data)
		return err
	}
	if err := writeNewSecretFile(clientIssueOut, data); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "wrote service client bundle for %q to %s (serial %s, expires %s)\n",
		bundle.Client, clientIssueOut, bundle.CertSerial, bundle.ExpiresAt)
	return nil
}

// issueClientBundle signs a service certificate for name and assembles the
// bundle. ttl 0 means the client's cert_max_ttl; a larger ttl is refused. The
// CA and the TLS certificate come from the same state directory serve uses
// (serverStateDir), so the bundle's pin and trust match the running server.
func issueClientBundle(cfg *config.ServerConfig, name string, ttl time.Duration) (clientBundle, error) {
	if !cfg.MTLSMode() {
		return clientBundle{}, fmt.Errorf("service clients require auth.mode: mtls (this server runs %s)", cfg.AuthModeValue())
	}
	cc, ok := cfg.ServiceClient(name)
	if !ok {
		return clientBundle{}, fmt.Errorf("no service client %q under auth.clients", name)
	}
	maxTTL := cc.CertMaxTTLValue()
	if ttl == 0 {
		ttl = maxTTL
	}
	if ttl < 0 {
		return clientBundle{}, fmt.Errorf("--ttl must be positive")
	}
	if ttl > maxTTL {
		return clientBundle{}, fmt.Errorf("--ttl %s exceeds client %q cert_max_ttl %s", ttl, name, maxTTL)
	}

	stateDir := serverStateDir(cfg)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return clientBundle{}, fmt.Errorf("state dir %s: %w", stateDir, err)
	}
	caCertPath, caKeyPath := caCertPaths(stateDir)
	ca, err := servertls.LoadOrGenerateCA(caCertPath, caKeyPath)
	if err != nil {
		return clientBundle{}, fmt.Errorf("client CA: %w", err)
	}
	certPath, keyPath := tlsCertPaths(cfg, stateDir)
	_, tlsDER, err := servertls.LoadOrGenerate(certPath, keyPath, cfg.TLSNames)
	if err != nil {
		return clientBundle{}, fmt.Errorf("tls cert: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return clientBundle{}, fmt.Errorf("generate client key: %w", err)
	}
	der, err := ca.SignServiceCert(&key.PublicKey, name, ttl)
	if err != nil {
		return clientBundle{}, fmt.Errorf("sign service certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return clientBundle{}, fmt.Errorf("parse issued certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return clientBundle{}, fmt.Errorf("encode client key: %w", err)
	}
	return clientBundle{
		Client:             name,
		AuthMode:           config.AuthModeMTLS,
		HTTPSPort:          cfg.HTTPSPort,
		TLSCertFingerprint: servertls.Fingerprint(tlsDER),
		ClientCert:         string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		ClientKey:          string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		CertSerial:         strings.ToLower(leaf.SerialNumber.Text(16)),
		ExpiresAt:          leaf.NotAfter.UTC().Format(time.RFC3339),
	}, nil
}

// writeNewSecretFile creates path (which must not exist) with mode 0600 and
// writes data. O_EXCL: an existing file, or a symlink planted at path, is
// refused rather than overwritten or followed.
func writeNewSecretFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; refusing to overwrite a credential", path)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	return f.Close()
}

func runClientList(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	return printClientList(cmd.OutOrStdout(), cfg, clientListJSON)
}

// printClientList prints the configured service clients, sorted by name.
func printClientList(w io.Writer, cfg *config.ServerConfig, asJSON bool) error {
	entries := []clientListEntry{}
	if cfg.Auth != nil {
		names := make([]string, 0, len(cfg.Auth.Clients))
		for name := range cfg.Auth.Clients {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			cc := cfg.Auth.Clients[name]
			p := cc.Policy
			entries = append(entries, clientListEntry{
				Name:         name,
				CertMaxTTL:   cc.CertMaxTTLValue().String(),
				NamePrefix:   p.NamePrefix,
				MaxSheds:     p.MaxSheds,
				MaxAge:       p.MaxAge.Duration().String(),
				Network:      p.NetworkProfile(),
				ServerMounts: append([]string{}, p.ServerMounts...),
				Images:       append([]string{}, p.Images...),
				CapsCPUs:     p.Caps.CPUs,
				CapsMemoryMB: p.Caps.MemoryMB,
			})
		}
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintln(w, "No service clients configured (auth.clients).")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPREFIX\tMAX SHEDS\tMAX AGE\tCERT MAX TTL\tNETWORK\tSERVER MOUNTS\tIMAGES\tCAPS")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Name, e.NamePrefix, e.MaxSheds, e.MaxAge, e.CertMaxTTL, e.Network,
			listOrDash(e.ServerMounts, "none"), listOrDash(e.Images, "default"), capsString(e.CapsCPUs, e.CapsMemoryMB))
	}
	return tw.Flush()
}

func listOrDash(xs []string, empty string) string {
	if len(xs) == 0 {
		return empty
	}
	return strings.Join(xs, ",")
}

func capsString(cpus, memMB int) string {
	var parts []string
	if cpus > 0 {
		parts = append(parts, fmt.Sprintf("cpus=%d", cpus))
	}
	if memMB > 0 {
		parts = append(parts, fmt.Sprintf("memory_mb=%d", memMB))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}
