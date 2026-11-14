package guest

import (
	"encoding/json"
	"testing"
)

func TestSocketName(t *testing.T) {
	tests := []struct {
		port uint32
		want string
	}{
		{ConsolePort, "1024.sock"},
		{NotifyPort, "1026.sock"},
		{TCPProxyPort, "1028.sock"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := SocketName(tt.port); got != tt.want {
				t.Errorf("SocketName(%d) = %q, want %q", tt.port, got, tt.want)
			}
		})
	}
}

func TestCommands(t *testing.T) {
	tests := []struct {
		name, got, want string
	}{
		{"version", VersionCommand, "/usr/local/bin/shed-agent version"},
		{"hash", HashCommand, "sha256sum /usr/local/bin/shed-agent"},
		{"install", InstallCommand("abc123"), "sudo -n /usr/local/sbin/shed-agent-install abc123"},
		{"sudoers", SudoersRule, "shed-link ALL=(root) NOPASSWD: /usr/local/sbin/shed-agent-install"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestParseAgentInfo(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    AgentInfo
		wantErr bool
	}{
		{"version output", `{"version":"v0.9.2","protocol":1,"layout":1}` + "\n", AgentInfo{"v0.9.2", 1, 1}, false},
		{"unknown fields are ignored", `{"version":"dev","protocol":1,"layout":0,"commit":"abc"}`, AgentInfo{"dev", 1, 0}, false},
		{"empty is an old agent", "", AgentInfo{}, false},
		{"null payload is an old agent", "null", AgentInfo{}, false},
		{"not json", "shed-agent v1", AgentInfo{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAgentInfo([]byte(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAgentInfoJSONShape(t *testing.T) {
	data, err := json.Marshal(AgentInfo{Version: "dev", Protocol: AgentProtocol, Layout: Layout})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"version":"dev","protocol":1,"layout":1}`; string(data) != want {
		t.Errorf("JSON = %s, want %s", data, want)
	}
}

func TestCompatibility(t *testing.T) {
	tests := []struct {
		name               string
		protocol, layout   int
		wantProto, wantLay bool
	}{
		{"current", AgentProtocol, Layout, true, true},
		{"agent with no reply and an image with no layout file", 0, 0, false, false},
		{"newer than this server", AgentProtocol + 1, Layout + 1, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProtocolCompatible(tt.protocol); got != tt.wantProto {
				t.Errorf("ProtocolCompatible(%d) = %v, want %v", tt.protocol, got, tt.wantProto)
			}
			if got := LayoutSupported(tt.layout); got != tt.wantLay {
				t.Errorf("LayoutSupported(%d) = %v, want %v", tt.layout, got, tt.wantLay)
			}
		})
	}
}
