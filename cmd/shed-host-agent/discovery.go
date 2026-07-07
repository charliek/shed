package main

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// shedClientConfig is a minimal local mirror of shed's CLI client config
// (~/.shed/config.yaml). Only the server registry is read; the rest
// (default_server, sheds cache, timeouts) is ignored. Defined locally rather
// than imported because shed's config package is internal — matching this
// project's convention of redefining wire types locally (see internal/protocol).
type shedClientConfig struct {
	Servers map[string]shedServerEntry `yaml:"servers"`
}

type shedServerEntry struct {
	Host     string `yaml:"host"`
	HTTPPort int    `yaml:"http_port"`
	// SSHPort is the server's SSH port, used to mint a credentials token over the
	// _bootstrap channel. Matches the shed CLI's ssh_port field.
	SSHPort int `yaml:"ssh_port"`
	// CredentialsToken is the bearer token the host-agent sends to the
	// credential bus. Matches the shed CLI's credentials_token field.
	CredentialsToken string `yaml:"credentials_token,omitempty"`
	// APIURL, when set, is the https control-plane/bus URL and overrides
	// host+http_port. Matches the shed CLI's api_url field.
	APIURL string `yaml:"api_url,omitempty"`
	// TLSCertFingerprint pins the server's self-signed TLS cert
	// ("sha256:<hex>"). Matches the shed CLI's tls_cert_fingerprint field.
	TLSCertFingerprint string `yaml:"tls_cert_fingerprint,omitempty"`
}

// defaultShedHTTPPort matches shed's default server HTTP port.
const defaultShedHTTPPort = 8080

// resolveTargets computes the desired server set to broker for: in discovery
// mode it reads + filters the discovery source; otherwise the single legacy
// server. The error is non-nil only on a discovery read failure (callers decide
// whether to keep current servers or proceed best-effort). Shared by the daemon
// reconcile loop and the `status` subcommand so they never disagree.
func resolveTargets(cfg Config) ([]ServerTarget, error) {
	var discovered []ServerTarget
	if cfg.Discovery != nil {
		d, err := LoadDiscoveredServers(cfg.Discovery.Source)
		if err != nil {
			return nil, err
		}
		discovered = d
	}
	return cfg.ResolveTargets(discovered), nil
}

// LoadDiscoveredServers reads the shed CLI config at path and returns one
// ServerTarget per registered server, sorted by name for deterministic
// ordering. A missing file is not an error — it yields an empty slice so the
// agent can start and pick servers up live once the file appears. Entries with
// an empty host are skipped.
func LoadDiscoveredServers(path string) ([]ServerTarget, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading shed config %s: %w", path, err)
	}

	var cfg shedClientConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing shed config %s: %w", path, err)
	}

	targets := make([]ServerTarget, 0, len(cfg.Servers))
	for name, entry := range cfg.Servers {
		if entry.Host == "" {
			continue
		}
		// Prefer the explicit api_url (carries the https scheme + port) over
		// the legacy host+http_port plain-HTTP form.
		url := entry.APIURL
		if url == "" {
			port := entry.HTTPPort
			if port == 0 {
				port = defaultShedHTTPPort
			}
			url = fmt.Sprintf("http://%s:%d", entry.Host, port)
		}
		targets = append(targets, ServerTarget{
			Name:           name,
			URL:            url,
			Token:          entry.CredentialsToken,
			TLSFingerprint: entry.TLSCertFingerprint,
			SSHHost:        entry.Host,
			SSHPort:        entry.SSHPort,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets, nil
}
