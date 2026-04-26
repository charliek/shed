//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParseShedName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"missing", "console=ttyS0 root=/dev/vda", ""},
		{"present", "console=ttyS0 shed.name=mybox root=/dev/vda", "mybox"},
		{"first_token", "shed.name=foo console=ttyS0", "foo"},
		{"last_token", "console=ttyS0 shed.name=baz", "baz"},
		{"with_newline", "console=ttyS0 shed.name=hello\n", "hello"},
		{"empty_value", "shed.name= console=ttyS0", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseShedName([]byte(tt.in))
			if got != tt.want {
				t.Errorf("parseShedName(%q) = %q; want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestShouldRegen(t *testing.T) {
	tests := []struct {
		name        string
		currentName string
		recorded    *identity
		want        bool
	}{
		{"no_recorded", "newshed", nil, true},
		{"name_matches", "myshed", &identity{Name: "myshed"}, false},
		{"name_differs", "newshed", &identity{Name: "oldshed"}, true},
		{"empty_recorded_name", "newshed", &identity{Name: ""}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRegen(tt.currentName, tt.recorded)
			if got != tt.want {
				t.Errorf("shouldRegen(%q, %+v) = %v; want %v", tt.currentName, tt.recorded, got, tt.want)
			}
		})
	}
}

func TestLoadIdentity(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing_returns_nil", func(t *testing.T) {
		got, err := loadIdentity(filepath.Join(dir, "nonexistent.json"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("malformed_returns_nil", func(t *testing.T) {
		path := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := loadIdentity(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil for malformed, got %+v", got)
		}
	})

	t.Run("valid_round_trip", func(t *testing.T) {
		path := filepath.Join(dir, "good.json")
		if err := saveIdentity(path, &identity{Name: "foo"}); err != nil {
			t.Fatal(err)
		}
		got, err := loadIdentity(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || got.Name != "foo" {
			t.Errorf("got %+v; want {Name:foo}", got)
		}
	})
}

func TestRunFirstboot(t *testing.T) {
	type fixture struct {
		cmdline      string
		identityFile string // initial contents (empty = absent)
		// Recorded calls
		ranCommands []string
	}

	type want struct {
		err              bool
		expectRegenCalls bool
		identityName     string // expected name in identity.json after run
	}

	tests := []struct {
		name string
		fix  func() *fixture
		want want
	}{
		{
			name: "no_shed_name_in_cmdline_noop",
			fix: func() *fixture {
				return &fixture{cmdline: "console=ttyS0"}
			},
			want: want{},
		},
		{
			name: "fresh_boot_regenerates",
			fix: func() *fixture {
				return &fixture{cmdline: "shed.name=fresh console=ttyS0"}
			},
			want: want{expectRegenCalls: true, identityName: "fresh"},
		},
		{
			name: "name_matches_no_regen",
			fix: func() *fixture {
				return &fixture{
					cmdline:      "shed.name=stable",
					identityFile: `{"name":"stable"}`,
				}
			},
			want: want{identityName: "stable"},
		},
		{
			name: "name_differs_regen",
			fix: func() *fixture {
				return &fixture{
					cmdline:      "shed.name=newone",
					identityFile: `{"name":"oldone"}`,
				}
			},
			want: want{expectRegenCalls: true, identityName: "newone"},
		},
		{
			name: "malformed_identity_regen",
			fix: func() *fixture {
				return &fixture{
					cmdline:      "shed.name=spawn",
					identityFile: `{not json`,
				}
			},
			want: want{expectRegenCalls: true, identityName: "spawn"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			f := tt.fix()

			cmdlinePath := filepath.Join(tmp, "cmdline")
			if err := os.WriteFile(cmdlinePath, []byte(f.cmdline), 0o644); err != nil {
				t.Fatal(err)
			}

			machineIDPath := filepath.Join(tmp, "machine-id")
			if err := os.WriteFile(machineIDPath, []byte("oldid"), 0o644); err != nil {
				t.Fatal(err)
			}

			sshDir := filepath.Join(tmp, "ssh")
			if err := os.MkdirAll(sshDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sshDir, "ssh_host_rsa_key"), []byte("oldkey"), 0o600); err != nil {
				t.Fatal(err)
			}

			hostnamePath := filepath.Join(tmp, "hostname")
			if err := os.WriteFile(hostnamePath, []byte("oldhost\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			identityPath := filepath.Join(tmp, "identity.json")
			if f.identityFile != "" {
				if err := os.WriteFile(identityPath, []byte(f.identityFile), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			cfg := firstbootCfg{
				cmdlinePath:   cmdlinePath,
				machineIDPath: machineIDPath,
				sshKeyGlob:    filepath.Join(sshDir, "ssh_host_*"),
				hostnamePath:  hostnamePath,
				identityPath:  identityPath,
				runCommand: func(name string, args ...string) error {
					f.ranCommands = append(f.ranCommands, name)
					return nil
				},
			}

			err := runFirstboot(cfg)
			if (err != nil) != tt.want.err {
				t.Fatalf("err = %v; wantErr = %v", err, tt.want.err)
			}

			ranAny := len(f.ranCommands) > 0
			if ranAny != tt.want.expectRegenCalls {
				t.Errorf("ran commands = %v; expectRegenCalls = %v (commands: %v)",
					ranAny, tt.want.expectRegenCalls, f.ranCommands)
			}

			if tt.want.identityName != "" {
				got, err := loadIdentity(identityPath)
				if err != nil {
					t.Fatalf("post-load identity: %v", err)
				}
				if got == nil || got.Name != tt.want.identityName {
					t.Errorf("identity = %+v; want name=%q", got, tt.want.identityName)
				}
			}
		})
	}
}

func TestRunFirstboot_ErrorPaths(t *testing.T) {
	t.Run("missing_cmdline_errors", func(t *testing.T) {
		tmp := t.TempDir()
		cfg := firstbootCfg{
			cmdlinePath:   filepath.Join(tmp, "no-such-file"),
			machineIDPath: filepath.Join(tmp, "machine-id"),
			sshKeyGlob:    filepath.Join(tmp, "ssh_host_*"),
			hostnamePath:  filepath.Join(tmp, "hostname"),
			identityPath:  filepath.Join(tmp, "identity.json"),
			runCommand:    func(string, ...string) error { return nil },
		}
		err := runFirstboot(cfg)
		if err == nil {
			t.Fatal("expected error for missing cmdline file")
		}
	})

	t.Run("regen_command_failure_propagates", func(t *testing.T) {
		tmp := t.TempDir()
		os.WriteFile(filepath.Join(tmp, "cmdline"), []byte("shed.name=err"), 0o644)
		os.WriteFile(filepath.Join(tmp, "machine-id"), []byte(""), 0o644)
		os.WriteFile(filepath.Join(tmp, "hostname"), []byte(""), 0o644)
		cfg := firstbootCfg{
			cmdlinePath:   filepath.Join(tmp, "cmdline"),
			machineIDPath: filepath.Join(tmp, "machine-id"),
			sshKeyGlob:    filepath.Join(tmp, "ssh_host_*"),
			hostnamePath:  filepath.Join(tmp, "hostname"),
			identityPath:  filepath.Join(tmp, "identity.json"),
			runCommand: func(name string, args ...string) error {
				if name == "ssh-keygen" {
					return errors.New("simulated failure")
				}
				return nil
			},
		}
		if err := runFirstboot(cfg); err == nil {
			t.Fatal("expected ssh-keygen failure to propagate")
		}
	})
}
