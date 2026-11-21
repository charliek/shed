package main

import (
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// unitLines joins continued lines the way systemd does: a line ending in a
// backslash continues on the next line, and comment lines inside a
// continuation are skipped. Standalone comment and blank lines are dropped.
func unitLines(unit string) []string {
	var out []string
	cur := ""
	continuing := false
	for _, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if !continuing && line == "" {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			cur += strings.TrimSuffix(line, "\\") + " "
			continuing = true
			continue
		}
		out = append(out, cur+line)
		cur, continuing = "", false
	}
	if continuing {
		out = append(out, cur)
	}
	return out
}

// unitList returns every value of key in the unit's [Unit] section, each
// logical line split on whitespace as systemd reads a dependency list
// (repeated lines add to the list).
func unitList(t *testing.T, unit, key string) []string {
	t.Helper()
	var values []string
	section := ""
	for _, line := range unitLines(unit) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if section != "[Unit]" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		values = append(values, strings.Fields(v)...)
	}
	return values
}

// TestUnitsWaitForTheNetwork holds both shipped units to network-online
// ordering (plan 037 A13): the deb's packaging/shed-server.service and the
// unit `shed-server install` writes. A directory server pulls its user list
// at start-up and a remote-backend server calls its cloud provider; started
// before the network is online, both fail.
func TestUnitsWaitForTheNetwork(t *testing.T) {
	packaged, err := os.ReadFile(filepath.Join("..", "..", "packaging", "shed-server.service"))
	if err != nil {
		t.Fatalf("read the packaged unit: %v", err)
	}
	generated := generateUnitFile(&user.User{
		Username: "pat",
		Uid:      "1000",
		Gid:      "1000",
		HomeDir:  "/home/pat",
	})

	tests := []struct {
		name string
		unit string
		// also lists what each key must hold beside network-online.target.
		also map[string][]string
	}{
		{
			name: "packaged deb unit",
			unit: string(packaged),
			also: map[string][]string{
				"After": {"docker.service"},
				"Wants": {"docker.service"},
			},
		},
		{
			name: "shed-server install's unit",
			unit: generated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{"After", "Wants"} {
				got := unitList(t, tt.unit, key)
				for _, want := range append([]string{"network-online.target"}, tt.also[key]...) {
					if !slices.Contains(got, want) {
						t.Errorf("[Unit] %s= is %q; want it to include %s", key, got, want)
					}
				}
			}
		})
	}
}

// TestUnitsReadTheEnvironmentFile: both shipped units, the deb's
// packaging/shed-server.service and the unit `shed-server install` writes,
// give shed-server the variables in /etc/shed/server.env (a remote-backend
// server's cloud settings), and the leading "-" keeps a host without the file
// starting as before.
func TestUnitsReadTheEnvironmentFile(t *testing.T) {
	packaged, err := os.ReadFile(filepath.Join("..", "..", "packaging", "shed-server.service"))
	if err != nil {
		t.Fatalf("read the packaged unit: %v", err)
	}
	generated := generateUnitFile(&user.User{
		Username: "pat",
		Uid:      "1000",
		Gid:      "1000",
		HomeDir:  "/home/pat",
	})
	const want = "EnvironmentFile=-/etc/shed/server.env"
	for _, tt := range []struct{ name, unit string }{
		{"packaged deb unit", string(packaged)},
		{"shed-server install's unit", generated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			section := ""
			var found []string
			for _, line := range unitLines(tt.unit) {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
					section = line
					continue
				}
				if strings.HasPrefix(line, "EnvironmentFile=") {
					found = append(found, section+" "+line)
				}
			}
			if !slices.Equal(found, []string{"[Service] " + want}) {
				t.Errorf("EnvironmentFile lines are %q; want exactly %q in [Service]", found, want)
			}
		})
	}
}

// TestUnitListJoinsContinuedLines: a setting swallowed into a continued value
// is not a setting, and a dependency wrapped onto the next line is read.
func TestUnitListJoinsContinuedLines(t *testing.T) {
	swallowed := "[Unit]\nDescription=Shed \\\n# continued description\nAfter=network-online.target\nAfter=docker.service\n"
	if got := unitList(t, swallowed, "After"); slices.Contains(got, "network-online.target") {
		t.Errorf("After= is %q; the first After= is part of Description and must not be read", got)
	}
	if got := unitList(t, swallowed, "After"); !slices.Contains(got, "docker.service") {
		t.Errorf("After= is %q; want docker.service", got)
	}
	wrapped := "[Unit]\nAfter=docker.service \\\n# note\n  network-online.target\n"
	if got := unitList(t, wrapped, "After"); !slices.Contains(got, "network-online.target") || !slices.Contains(got, "docker.service") {
		t.Errorf("After= is %q; want the wrapped list read whole", got)
	}
}

// TestGeneratedUnitFillsItsPlaceholders: the template's other lines are
// unchanged by the ordering edit, and no placeholder is left in the output.
func TestGeneratedUnitFillsItsPlaceholders(t *testing.T) {
	got := generateUnitFile(&user.User{
		Username: "pat",
		Uid:      "1000",
		Gid:      "1000",
		HomeDir:  "/home/pat",
	})
	for _, want := range []string{
		"User=pat\n",
		"ExecStart=" + defaultBinaryPath + " serve\n",
		"Environment=HOME=/home/pat\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated unit lacks %q:\n%s", want, got)
		}
	}
	if strings.ContainsAny(got, "{}") {
		t.Errorf("generated unit still holds a placeholder:\n%s", got)
	}
}
