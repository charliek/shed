package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MachineEntry is shed's tolerant, READ-ONLY view of one `machines:` entry
// (plan 019 §3.1). The schema itself is Rust-owned — MachineEntry in
// crates/shed-core/src/config.rs — and this decoder reads only the subset the
// `shed roost-provider` menu needs: enough to name a host and dial it over
// ssh. `rc_bin` is deliberately NOT modeled here (plan 019 pin P7: it is
// Rust-owned and out of scope for Go); a shared fixture under
// crates/fixtures/machines/ documents that asymmetry explicitly.
type MachineEntry struct {
	// Name is the map key, e.g. "mini2" in `machines: { mini2: {...} }`.
	Name string
	// Host defaults to Name when absent or empty.
	Host string
	// User is the ssh login user; "" lets ssh decide (its own config, or the
	// current login), matching the Rust decoder's "absent → defer to ssh"
	// rule.
	User string
	// SSHPort defaults to 22.
	SSHPort int
	// KnownHosts is the UserKnownHostsFile to pin against; "" means
	// unpinned — the caller falls back to ssh's own default, exactly like an
	// absent value.
	KnownHosts string
}

// rawMachineEntry is the on-the-wire shape decoded straight off one entry's
// yaml.Node. It is deliberately narrower than crates/shed-core's
// MachineEntry (no rc_bin) and deliberately permissive: yaml.v3's default
// Decode ignores unknown keys rather than erroring on them (no
// KnownFields(true)), which is what makes an rc_bin-carrying entry, or any
// future Rust-added field, decode cleanly here instead of failing the whole
// config read.
type rawMachineEntry struct {
	Host       string `yaml:"host"`
	User       string `yaml:"user"`
	SSHPort    int    `yaml:"ssh_port"`
	KnownHosts string `yaml:"known_hosts"`
}

// DecodeMachines tolerantly decodes the `machines:` section carried in
// c.Machines — the opaque yaml.Node passthrough SaveToPath round-trips
// unread (see the doc comment on ClientConfig.Machines). Decoding from that
// already-parsed Node, rather than re-reading/re-parsing the config file, is
// the cleaner of the two options available here: it can never observe a
// config on disk that differs from the one this *ClientConfig was loaded
// from or is about to save, and it costs nothing extra since the Node is
// already in memory.
//
// A malformed entry (its value isn't a mapping, or a field's type doesn't
// match — e.g. `ssh_port: not-a-number`) is skipped with a one-line note on
// stderr; it never fails the whole decode. This is the tolerance contract
// `shed list` and the roost provider's `list` phase depend on: one bad hand
// edit in `machines:` must not take down every other command that touches
// config.yaml.
func (c *ClientConfig) DecodeMachines() []MachineEntry {
	return decodeMachines(&c.Machines)
}

func decodeMachines(node *yaml.Node) []MachineEntry {
	// An absent `machines:` key leaves the Node at its zero value (Kind 0,
	// never a MappingNode) — zero entries, not an error, mirroring the Rust
	// decoder's "machines_absent_is_empty" contract.
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}

	var out []MachineEntry
	// A mapping node's Content alternates key, value, key, value...
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]

		// A complex YAML key (`? {}` / `: {}`) decodes to a non-scalar
		// keyNode with an empty Value — left unchecked that produces an
		// entry with both Name and Host empty, which would reach the
		// provider menu as a blank row. Skip it with the same one-line
		// stderr note the malformed-value path below uses, rather than
		// letting a hand-edit like that through silently.
		if keyNode.Kind != yaml.ScalarNode || keyNode.Value == "" {
			fmt.Fprintf(os.Stderr, "shed: skipping machines entry with a non-scalar or empty key\n")
			continue
		}

		var raw rawMachineEntry
		if err := valNode.Decode(&raw); err != nil {
			fmt.Fprintf(os.Stderr, "shed: skipping malformed machines entry %q: %v\n", keyNode.Value, err)
			continue
		}

		host := raw.Host
		if host == "" {
			host = keyNode.Value
		}
		// An ssh destination that begins with a dash is not a host. OpenSSH
		// parses options BEFORE the destination word, so a `host:` of
		// `-oProxyCommand=…` is consumed as an option and that command runs
		// on THIS machine — verified against the local ssh:
		// `ssh -G -oPort=7777 -- echo hi` reports `port 7777` and
		// `hostname echo`. A trailing `--` cannot save it; option parsing has
		// already happened by then. roost refuses the same shape in its own
		// target classifier (roost-ipc/src/ssh.rs: "target starts with '-';
		// that looks like an option, not a host"), and so does the provider's
		// Target validation — this is the decoder half of the same rule, so a
		// config carrying one is dropped at the door rather than carried
		// around as an unusable entry.
		if strings.HasPrefix(host, "-") || strings.HasPrefix(raw.User, "-") {
			fmt.Fprintf(os.Stderr,
				"shed: skipping machines entry %q: its host or user begins with a dash, which ssh reads as an option rather than a destination\n",
				keyNode.Value)
			continue
		}
		// Match the Rust decoder's range check (shed-core's MachineEntry
		// holds ssh_port as a u16 and falls back to 22 for anything that
		// doesn't parse into 1..65535, e.g. 70000 or -1): a value outside
		// that range falls back to 22 rather than skipping the entry.
		//
		// ssh_port: 0 is the one remaining divergence between the two
		// decoders (Go falls back to 22 here same as any other
		// out-of-range value; Rust's u16 parse accepts 0 verbatim) — it is
		// deliberately left as-is and documented in
		// crates/fixtures/machines/README.md rather than "fixed", since
		// port 0 is meaningless for ssh and the Rust parser is not in scope
		// for this change.
		sshPort := raw.SSHPort
		if sshPort < 1 || sshPort > 65535 {
			sshPort = 22
		}

		out = append(out, MachineEntry{
			Name:       keyNode.Value,
			Host:       host,
			User:       raw.User,
			SSHPort:    sshPort,
			KnownHosts: raw.KnownHosts,
		})
	}
	// Sorted by name, matching the Rust parser (config.rs:289) rather than
	// yaml document order. Two reasons it has to be this and not the
	// document's: the shared fixture asserts ONE expected order for both
	// languages, and the provider's `list` menu should not reshuffle itself
	// because the user hand-edited an entry into a different spot.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
