//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

// doctorCmd is a one-stop "is this install healthy?" report for the
// Firecracker backend. Designed to be the first thing a user runs
// when shed-server "isn't working" — the output is meant to make
// the failure mode obvious without having to grep journalctl or
// read internal docs.
//
// Each check has three outcomes:
//   - PASS: green, no action needed
//   - WARN: yellow, surfaces something worth knowing but won't
//     block normal operation (e.g. shed-server unit not
//     active when the user is iterating locally without
//     systemd)
//   - FAIL: red, would prevent `shed create` from working;
//     doctor returns a non-zero exit code if any FAIL fires
//
// Reuses the setup helpers (loadBridgeConfig, ensureFirecracker
// version constant, etc.) rather than duplicating the prerequisite
// matrix.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check whether this host is configured correctly to run shed-server",
	Long: `Run a sequence of self-checks against the local shed-server
install and report PASS / WARN / FAIL for each.

Designed as the first diagnostic step when "shed create" or
"shed-server pull-images" fail unexpectedly. Output groups checks
by category (host, package, config, network, images, extensions)
so a single run surfaces the most common misconfigurations.

Exits 0 if every check is PASS or WARN, 1 if any FAIL fired.`,
	RunE: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

type checkResult struct {
	name   string
	status string // "PASS", "WARN", "FAIL"
	detail string
}

type doctor struct {
	cfg     *config.ServerConfig
	results []checkResult
}

func (d *doctor) record(name, status, detail string) {
	d.results = append(d.results, checkResult{name: name, status: status, detail: detail})
}

func (d *doctor) pass(name, detail string) { d.record(name, "PASS", detail) }
func (d *doctor) warn(name, detail string) { d.record(name, "WARN", detail) }
func (d *doctor) fail(name, detail string) { d.record(name, "FAIL", detail) }

func runDoctor(cmd *cobra.Command, args []string) error {
	d := &doctor{}

	// --- Host prerequisites ---
	d.checkKVM()
	d.checkDocker()
	d.checkFirecracker()

	// --- Configuration ---
	// loadConfig is in serve.go and shared with `shed-server serve`
	// + `shed-server pull-images`; reuse so an unparseable config
	// becomes a doctor FAIL with the same error users see at
	// service start.
	cfg, err := loadConfig()
	if err != nil {
		d.fail("server.yaml", fmt.Sprintf("loadConfig: %v", err))
		d.print(cmd)
		// Without a config we can't run the rest of the checks
		// meaningfully — return the FAIL exit code now.
		return fmt.Errorf("doctor: configuration is broken; fix the FAIL above first")
	}
	d.cfg = cfg
	d.pass("server.yaml", "loaded from "+findConfigPath())

	// --- Kernel + bridge ---
	d.checkKernel()
	d.checkBridge()

	// --- Images + extensions ---
	d.checkImages()
	d.checkExtensions()

	// --- Systemd unit ---
	d.checkSystemdUnit(cmd.Context())

	d.print(cmd)
	for _, r := range d.results {
		if r.status == "FAIL" {
			return fmt.Errorf("doctor: %d check(s) failed; see report above", countStatus(d.results, "FAIL"))
		}
	}
	return nil
}

func (d *doctor) checkKVM() {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		d.fail("/dev/kvm", fmt.Sprintf("not readable: %v (enable virtualization in BIOS / nested-virt settings)", err))
		return
	}
	f.Close()
	d.pass("/dev/kvm", "readable")
}

func (d *doctor) checkDocker() {
	if _, err := exec.LookPath("docker"); err != nil {
		// Post-v0.5.2, the host no longer invokes mkfs.erofs via
		// docker — but image publish (shed image build) still does
		// for the local-dev path. Treat as WARN, not FAIL.
		d.warn("docker", "not on PATH; needed only for `shed image build` (not for `shed create`)")
		return
	}
	d.pass("docker", "on PATH")
}

func (d *doctor) checkFirecracker() {
	if _, err := os.Stat(firecrackerBinPath); err != nil {
		d.fail("firecracker", fmt.Sprintf("%s missing (run `sudo shed-server setup`)", firecrackerBinPath))
		return
	}
	// Capture --version so the operator can tell at a glance which
	// Firecracker is installed without shelling in further.
	out, err := exec.Command(firecrackerBinPath, "--version").Output()
	if err != nil {
		d.warn("firecracker", fmt.Sprintf("present at %s but --version failed: %v", firecrackerBinPath, err))
		return
	}
	d.pass("firecracker", strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]))
}

func (d *doctor) checkKernel() {
	if d.cfg.Firecracker == nil {
		return
	}
	kp := d.cfg.Firecracker.KernelPath
	if kp == "" {
		d.warn("kernel_path", "unset in firecracker.kernel_path; sheds will use the per-image kernel blob (the normal v0.5.2+ path)")
		return
	}
	info, err := os.Stat(kp)
	if err != nil {
		d.fail("kernel_path", fmt.Sprintf("%s: %v", kp, err))
		return
	}
	// 10 MB is a sanity floor: real vmlinux is ~30-40 MB. A few KB
	// is almost certainly a truncated download or empty placeholder.
	const minKernelBytes = 10 * 1024 * 1024
	if info.Size() < minKernelBytes {
		d.fail("kernel_path", fmt.Sprintf("%s is only %d bytes (< %d MB); likely truncated", kp, info.Size(), minKernelBytes/(1024*1024)))
		return
	}
	d.pass("kernel_path", fmt.Sprintf("%s (%d bytes)", kp, info.Size()))
}

func (d *doctor) checkBridge() {
	if d.cfg.Firecracker == nil {
		return
	}
	name := d.cfg.Firecracker.BridgeName
	if name == "" {
		d.warn("bridge", "firecracker.bridge_name unset; setup defaults to shed-br0")
		return
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		d.fail("bridge", fmt.Sprintf("interface %q not present: %v (run `sudo shed-server setup`)", name, err))
		return
	}
	if iface.Flags&net.FlagUp == 0 {
		d.warn("bridge", fmt.Sprintf("interface %q exists but is DOWN; bring up with `sudo ip link set %s up`", name, name))
		return
	}
	d.pass("bridge", fmt.Sprintf("%s up (idx %d, mtu %d)", name, iface.Index, iface.MTU))
}

func (d *doctor) checkImages() {
	if d.cfg.Firecracker == nil || d.cfg.Firecracker.ImagesDir == "" {
		return
	}
	imagesDir := d.cfg.Firecracker.ImagesDir
	tags, err := vmimage.ListTags(imagesDir)
	if err != nil {
		d.fail("images", fmt.Sprintf("ListTags(%s): %v", imagesDir, err))
		return
	}
	if len(tags) == 0 {
		d.warn("images", "no tags installed (run `sudo shed-server pull-images`)")
		return
	}
	var missing []string
	var legacy []string
	for _, tag := range tags {
		t, err := vmimage.GetTag(imagesDir, tag)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s (tag lookup: %v)", tag, err))
			continue
		}
		manifest, err := vmimage.LoadManifestByDigest(imagesDir, t.Digest)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s (manifest %s: %v)", tag, vmimage.ShortDigest(t.Digest), err))
			continue
		}
		// v0.5.2+: every shed manifest must reference an erofs blob.
		// Tags pointing at pre-v0.5.2 manifests would fail at boot
		// with the legacy-reject error in manager.resolveManifestLower
		// — surface that early as a FAIL here.
		erofsDigest := manifest.ShedRootfsErofsDigest()
		if erofsDigest == "" {
			legacy = append(legacy, fmt.Sprintf("%s (manifest %s lacks %s annotation)", tag, vmimage.ShortDigest(t.Digest), vmimage.AnnotationRootfsErofsDigest))
			continue
		}
		if !vmimage.BlobExists(imagesDir, erofsDigest) {
			missing = append(missing, fmt.Sprintf("%s (erofs blob %s not installed)", tag, vmimage.ShortDigest(erofsDigest)))
		}
	}
	if len(missing) > 0 || len(legacy) > 0 {
		var msg []string
		if len(missing) > 0 {
			msg = append(msg, fmt.Sprintf("missing blobs: %s", strings.Join(missing, "; ")))
		}
		if len(legacy) > 0 {
			msg = append(msg, fmt.Sprintf("pre-v0.5.2 manifests: %s (re-pull with `shed-server pull-images`)", strings.Join(legacy, "; ")))
		}
		d.fail("images", strings.Join(msg, " | "))
		return
	}
	d.pass("images", fmt.Sprintf("%d tag(s), all manifests + erofs blobs present", len(tags)))
}

func (d *doctor) checkExtensions() {
	if d.cfg.Extensions == nil || len(d.cfg.Extensions.Enabled) == 0 {
		d.pass("extensions", "none enabled")
		return
	}
	enabled := d.cfg.Extensions.Enabled
	// Extensions ship as a directory of *.yaml manifests installed
	// under /etc/shed-extensions.d (per shed-extensions's deb). If
	// the user enabled an extension by name but the corresponding
	// manifest isn't present, the credential broker won't start.
	const extDir = "/etc/shed-extensions.d"
	var missing []string
	for _, name := range enabled {
		manifestPath := filepath.Join(extDir, name+".yaml")
		if _, err := os.Stat(manifestPath); err != nil {
			missing = append(missing, fmt.Sprintf("%s (%s)", name, manifestPath))
		}
	}
	if len(missing) > 0 {
		d.warn("extensions", fmt.Sprintf("enabled in config but manifest missing: %s; install the shed-extensions deb", strings.Join(missing, ", ")))
		return
	}
	d.pass("extensions", fmt.Sprintf("%d enabled, all manifests present", len(enabled)))
}

func (d *doctor) checkSystemdUnit(ctx context.Context) {
	out, err := exec.CommandContext(ctx, "systemctl", "is-active", "shed-server").CombinedOutput()
	state := strings.TrimSpace(string(out))
	if err != nil {
		// is-active returns non-zero when the unit isn't active.
		// Treat "inactive" as WARN (some dev workflows run
		// shed-server in foreground via `make dev-server` without
		// systemd) rather than FAIL.
		d.warn("shed-server.service", fmt.Sprintf("not active (state=%s); start with `sudo systemctl start shed-server`", state))
		return
	}
	d.pass("shed-server.service", state)
}

// findConfigPath replicates loadConfig's search order for display
// purposes only. Best-effort; if the user passed -c we don't see
// it here because loadConfig doesn't expose its resolved path.
func findConfigPath() string {
	for _, p := range []string{"/etc/shed/server.yaml", "configs/server.yaml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "(default location)"
}

func (d *doctor) print(cmd *cobra.Command) {
	w := cmd.OutOrStdout()
	pass, warn, fail := 0, 0, 0
	for _, r := range d.results {
		switch r.status {
		case "PASS":
			pass++
		case "WARN":
			warn++
		case "FAIL":
			fail++
		}
	}
	fmt.Fprintf(w, "shed-server doctor: %d PASS, %d WARN, %d FAIL\n\n", pass, warn, fail)
	// Fixed-width tag column keeps the eye scanning the status
	// indicator (the part that matters) instead of jagged left
	// edges.
	const tagWidth = 24
	for _, r := range d.results {
		fmt.Fprintf(w, "  %-*s %-4s  %s\n", tagWidth, r.name, r.status, r.detail)
	}
}

func countStatus(results []checkResult, status string) int {
	n := 0
	for _, r := range results {
		if r.status == status {
			n++
		}
	}
	return n
}
