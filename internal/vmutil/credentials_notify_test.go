package vmutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/config"
)

func TestCredentialNotifyOnConnect(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"ssh": {
				Source:  "/host/.ssh",
				Target: "/home/shed/.ssh",
				Exclude: []string{"*.sock"},
			},
			"gh": {
				Source:  "/host/.config/gh",
				Target: "/home/shed/.config/gh",
			},
			"readonly-cred": {
				Source:   "/host/.readonly",
				Target:   "/home/shed/.readonly",
				ReadOnly: true,
			},
		},
	}

	handler := &credentialNotifyHandler{
		nl: &CredentialNotifyListener{
			serverCfg: serverCfg,
			name:      "test-vm",
		},
	}

	client, server := net.Pipe()
	defer client.Close()

	// Run OnConnect in a goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- handler.OnConnect(server)
		server.Close()
	}()

	// Read the setup message from the client side
	msgType, data, err := agentproto.ReadMessage(client)
	if err != nil {
		t.Fatalf("ReadMessage() error = %v", err)
	}
	if msgType != agentproto.MsgTypeNotifySetup {
		t.Fatalf("msgType = 0x%02x, want 0x%02x", msgType, agentproto.MsgTypeNotifySetup)
	}

	var setup agentproto.NotifySetupMessage
	if err := json.Unmarshal(data, &setup); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	// Should have 2 writable credentials (ssh, gh), not readonly-cred
	if len(setup.Credentials) != 2 {
		t.Fatalf("len(Credentials) = %d, want 2", len(setup.Credentials))
	}
	if setup.Credentials["ssh"] != "/home/shed/.ssh" {
		t.Errorf("Credentials[ssh] = %q, want %q", setup.Credentials["ssh"], "/home/shed/.ssh")
	}
	if setup.Credentials["gh"] != "/home/shed/.config/gh" {
		t.Errorf("Credentials[gh] = %q, want %q", setup.Credentials["gh"], "/home/shed/.config/gh")
	}
	if _, ok := setup.Credentials["readonly-cred"]; ok {
		t.Error("readonly credential should not be included")
	}

	// Excludes should only have ssh
	if len(setup.Excludes) != 1 {
		t.Fatalf("len(Excludes) = %d, want 1", len(setup.Excludes))
	}
	if len(setup.Excludes["ssh"]) != 1 || setup.Excludes["ssh"][0] != "*.sock" {
		t.Errorf("Excludes[ssh] = %v, want [*.sock]", setup.Excludes["ssh"])
	}

	if err := <-errCh; err != nil {
		t.Fatalf("OnConnect() error = %v", err)
	}
}

func TestCredentialNotifyOnConnectNoWritable(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"readonly": {
				Source:   "/host/.readonly",
				Target:   "/home/shed/.readonly",
				ReadOnly: true,
			},
		},
	}

	handler := &credentialNotifyHandler{
		nl: &CredentialNotifyListener{
			serverCfg: serverCfg,
			name:      "test-vm",
		},
	}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// OnConnect should return nil and send nothing
	if err := handler.OnConnect(server); err != nil {
		t.Fatalf("OnConnect() error = %v", err)
	}
}

func TestCredentialNotifyOnMessageFileChanged(t *testing.T) {
	// Provide an agent backed by an errDialer so Exec fails fast
	// instead of panicking on nil.
	dialer := &errDialer{err: fmt.Errorf("mock dial error")}
	agent := NewAgentClient(dialer, 1024, 1025, 1026)

	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"ssh": {
				Source: t.TempDir(),
				Target: "/home/shed/.ssh",
			},
		},
	}

	handler := &credentialNotifyHandler{
		nl: &CredentialNotifyListener{
			agent:     agent,
			serverCfg: serverCfg,
			name:      "test-vm",
		},
	}

	changed := agentproto.FileChangedMessage{
		Credential: "ssh",
		Files:      []string{"id_rsa", "id_rsa.pub"},
	}
	data, _ := json.Marshal(changed)

	// OnMessage dispatches to pullChangedFiles, which will fail (dial error)
	// but errors are logged, not returned — so OnMessage returns nil.
	err := handler.OnMessage(agentproto.MsgTypeFileChanged, data)
	if err != nil {
		t.Errorf("OnMessage() error = %v, want nil", err)
	}
}

func TestCredentialNotifyOnMessageUnexpectedType(t *testing.T) {
	handler := &credentialNotifyHandler{
		nl: &CredentialNotifyListener{
			name: "test-vm",
		},
	}

	// Unexpected type should log but return nil
	err := handler.OnMessage(0xFF, []byte("garbage"))
	if err != nil {
		t.Errorf("OnMessage() error = %v, want nil for unexpected type", err)
	}
}

func TestPullChangedFilesPathValidation(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"ssh": {
				Source: "/host/.ssh",
				Target: "/home/shed/.ssh",
			},
		},
	}

	nl := &CredentialNotifyListener{
		serverCfg: serverCfg,
		name:      "test-vm",
		// agent is nil — we expect the function to filter out bad paths
		// and return nil when no valid files remain
	}

	// All paths should be rejected: ".." and absolute paths
	err := nl.pullChangedFiles("ssh", []string{
		"../../etc/passwd",
		"/etc/shadow",
		"../../../root/.ssh/id_rsa",
	})
	if err != nil {
		t.Errorf("pullChangedFiles() error = %v, want nil (all paths rejected)", err)
	}
}

func TestPullChangedFilesUnknownCredential(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{},
	}

	nl := &CredentialNotifyListener{
		serverCfg: serverCfg,
		name:      "test-vm",
	}

	err := nl.pullChangedFiles("nonexistent", []string{"file.txt"})
	if err == nil {
		t.Error("pullChangedFiles() expected error for unknown credential")
	}
}

func TestLimitedBufferExceedsMax(t *testing.T) {
	lb := &limitedBuffer{max: 10}

	// Write within limit
	n, err := lb.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != 5 {
		t.Fatalf("Write() = %d, want 5", n)
	}

	// Write that exceeds limit
	_, err = lb.Write([]byte("hello world"))
	if err == nil {
		t.Fatal("Write() expected error when exceeding max")
	}
}

func TestLimitedBufferExactLimit(t *testing.T) {
	lb := &limitedBuffer{max: 10}

	// Write exactly at limit
	n, err := lb.Write([]byte("0123456789"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != 10 {
		t.Fatalf("Write() = %d, want 10", n)
	}

	// One more byte should fail
	_, err = lb.Write([]byte("x"))
	if err == nil {
		t.Fatal("Write() expected error when exceeding max")
	}
}

func TestExtractTarToHostSkipsSymlinks(t *testing.T) {
	destDir := t.TempDir()

	// Create tar with a symlink entry
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	// Regular file
	tw.WriteHeader(&tar.Header{
		Name: "real.txt",
		Mode: 0644,
		Size: 4,
		Typeflag: tar.TypeReg,
	})
	tw.Write([]byte("real"))

	// Symlink (should be skipped)
	tw.WriteHeader(&tar.Header{
		Name:     "link.txt",
		Linkname: "/etc/passwd",
		Typeflag: tar.TypeSymlink,
	})

	tw.Close()
	gzw.Close()

	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	assertFileContent(t, filepath.Join(destDir, "real.txt"), "real")

	// Symlink should not exist
	if _, err := os.Lstat(filepath.Join(destDir, "link.txt")); !os.IsNotExist(err) {
		t.Error("symlink should not have been extracted")
	}
}

func TestExtractTarToHostSkipsOversizedFiles(t *testing.T) {
	destDir := t.TempDir()

	// Build a valid tar where the oversized file has real data so the tar
	// reader can advance past it. We use maxCredentialFileSize+1 bytes of
	// actual data to exceed the limit.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	oversized := maxCredentialFileSize + 1

	// Normal file first
	if err := tw.WriteHeader(&tar.Header{
		Name:     "small.txt",
		Mode:     0644,
		Size:     5,
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("small"))

	// Oversized file with real data
	if err := tw.WriteHeader(&tar.Header{
		Name:     "huge.bin",
		Mode:     0644,
		Size:     int64(oversized),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	// Write the full amount of data to keep the tar valid
	remaining := oversized
	chunk := make([]byte, 32*1024)
	for remaining > 0 {
		n := len(chunk)
		if n > remaining {
			n = remaining
		}
		tw.Write(chunk[:n])
		remaining -= n
	}

	tw.Close()
	gzw.Close()

	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	// Oversized file should not exist
	if _, err := os.Stat(filepath.Join(destDir, "huge.bin")); !os.IsNotExist(err) {
		t.Error("oversized file should not have been extracted")
	}

	assertFileContent(t, filepath.Join(destDir, "small.txt"), "small")
}


// assertFileContent reads a file and checks its content.
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if string(got) != want {
		t.Errorf("file %s content = %q, want %q", path, got, want)
	}
}

func TestSecurePathFromNotify(t *testing.T) {
	destDir := t.TempDir()

	// Valid path
	p, err := securePath(destDir, "subdir/file.txt")
	if err != nil {
		t.Fatalf("securePath() error = %v", err)
	}
	if p == "" {
		t.Fatal("securePath() returned empty path")
	}

	// Path traversal
	_, err = securePath(destDir, fmt.Sprintf("../../%s", "escape.txt"))
	if err == nil {
		t.Error("securePath() expected error for path traversal")
	}
}
