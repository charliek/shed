package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{4 * 1024, "4.0 KB"},
		{1024 * 1024, "1.0 MB"},
		{5 * 1024 * 1024, "5.0 MB"},
		{2 * 1024 * 1024 * 1024, "2.0 GB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := formatSize(tt.in); got != tt.want {
				t.Errorf("formatSize(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func sampleDiskUsage() config.DiskUsage {
	return config.DiskUsage{
		ServerName:  "prod-mac",
		Backend:     "vz",
		GeneratedAt: time.Date(2026, 4, 20, 11, 23, 45, 0, time.UTC),
		Images: []config.ImageDiskEntry{
			{Name: "default", DockerRef: "ghcr.io/x/default:v1", Size: config.DiskSize{LogicalBytes: 5 * 1024 * 1024 * 1024, PhysicalBytes: 4 * 1024 * 1024 * 1024}},
			{Name: "_base", IsBase: true, Size: config.DiskSize{LogicalBytes: 5 * 1024 * 1024 * 1024, PhysicalBytes: 0}},
		},
		Kernel: &config.FileEntry{Path: "/tmp/vmlinux", Size: config.DiskSize{LogicalBytes: 8 * 1024 * 1024, PhysicalBytes: 8 * 1024 * 1024}, Kind: "kernel"},
		Initrd: &config.FileEntry{Path: "/tmp/initrd", Size: config.DiskSize{LogicalBytes: 100 * 1024, PhysicalBytes: 100 * 1024}, Kind: "initrd"},
		Sheds: []config.ShedDiskEntry{
			{
				Name:       "api-dev",
				Status:     config.StatusRunning,
				Image:      "default",
				Rootfs:     config.FileEntry{Size: config.DiskSize{LogicalBytes: 2 * 1024 * 1024 * 1024, PhysicalBytes: 2 * 1024 * 1024 * 1024}},
				ConsoleLog: &config.FileEntry{Size: config.DiskSize{LogicalBytes: 800 * 1024}},
				Total:      config.DiskSize{LogicalBytes: 2 * 1024 * 1024 * 1024, PhysicalBytes: 2 * 1024 * 1024 * 1024},
			},
			{
				Name:   "api-test",
				Status: config.StatusStopped,
				Image:  "default",
				Rootfs: config.FileEntry{Size: config.DiskSize{LogicalBytes: 2 * 1024 * 1024 * 1024, PhysicalBytes: 2 * 1024 * 1024 * 1024}},
				Total:  config.DiskSize{LogicalBytes: 2 * 1024 * 1024 * 1024, PhysicalBytes: 2 * 1024 * 1024 * 1024},
			},
		},
		Orphans: []config.FileEntry{
			{Path: "/tmp/dead.lock", Size: config.DiskSize{LogicalBytes: 0, PhysicalBytes: 0}, Kind: "lock"},
		},
		Totals: config.DiskUsageTotals{
			Images:  config.DiskSize{LogicalBytes: 10 * 1024 * 1024 * 1024, PhysicalBytes: 4 * 1024 * 1024 * 1024},
			Sheds:   config.DiskSize{LogicalBytes: 4 * 1024 * 1024 * 1024, PhysicalBytes: 4 * 1024 * 1024 * 1024},
			Orphans: config.DiskSize{LogicalBytes: 0, PhysicalBytes: 0},
			All:     config.DiskSize{LogicalBytes: 14 * 1024 * 1024 * 1024, PhysicalBytes: 8 * 1024 * 1024 * 1024},
		},
		Notes: []string{"physical bytes may overcount shared extents"},
	}
}

func TestRenderDF_Rollup(t *testing.T) {
	var buf bytes.Buffer
	renderDF(&buf, sampleDiskUsage(), false)
	got := buf.String()

	for _, want := range []string{
		"SERVER:  prod-mac",
		"BACKEND: vz",
		"CATEGORY",
		"images",
		"sheds (1 stopped, 1 run)",
		"orphans",
		"TOTAL",
		"14.0 GB", // Totals.All.LogicalBytes
		"Note:",   // Notes section
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rollup missing %q.\nOUTPUT:\n%s", want, got)
		}
	}
	// Rollup must NOT contain verbose-only sections.
	for _, banned := range []string{"IMAGES (", "SHEDS (", "KERNEL / INITRD", "ORPHANS ("} {
		if strings.Contains(got, banned) {
			t.Errorf("rollup unexpectedly contains %q.\nOUTPUT:\n%s", banned, got)
		}
	}
}

func TestRenderDF_Verbose(t *testing.T) {
	var buf bytes.Buffer
	renderDF(&buf, sampleDiskUsage(), true)
	got := buf.String()

	for _, want := range []string{
		"IMAGES (2)",
		"default",
		"_base",
		"SHEDS (2)",
		"api-dev",
		"api-test",
		"KERNEL / INITRD",
		"/tmp/vmlinux",
		"/tmp/initrd",
		"ORPHANS (1)",
		"lock",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("verbose missing %q.\nOUTPUT:\n%s", want, got)
		}
	}
}

func TestRenderDF_FCNoInitrd(t *testing.T) {
	du := sampleDiskUsage()
	du.Backend = "firecracker"
	du.Initrd = nil
	// FC sheds don't have console.log; nil it out.
	for i := range du.Sheds {
		du.Sheds[i].ConsoleLog = nil
	}

	var buf bytes.Buffer
	renderDF(&buf, du, true)
	got := buf.String()

	if !strings.Contains(got, "BACKEND: firecracker") {
		t.Errorf("expected FC backend header, got:\n%s", got)
	}
	// The kernel/initrd section should still render with just kernel.
	if !strings.Contains(got, "/tmp/vmlinux") {
		t.Errorf("expected kernel row, got:\n%s", got)
	}
	if strings.Contains(got, "/tmp/initrd") {
		t.Errorf("unexpected initrd row on FC, got:\n%s", got)
	}
	// Per-shed CONSOLE column should show "n/a" for FC sheds.
	if !strings.Contains(got, "n/a") {
		t.Errorf("expected n/a for FC console column, got:\n%s", got)
	}
}

func TestRenderDF_ZeroState(t *testing.T) {
	du := config.DiskUsage{
		ServerName:  "empty",
		Backend:     "vz",
		GeneratedAt: time.Now().UTC(),
	}
	var buf bytes.Buffer
	renderDF(&buf, du, false)
	got := buf.String()

	if !strings.Contains(got, "TOTAL") {
		t.Errorf("expected TOTAL row even for empty state, got:\n%s", got)
	}
	if !strings.Contains(got, "0 B") {
		t.Errorf("expected zero-byte display, got:\n%s", got)
	}
}

func TestCountFiles(t *testing.T) {
	du := sampleDiskUsage()
	// 2 images + kernel + initrd + (2 sheds × rootfs) + 1 console + 1 orphan = 8
	got := countFiles(&du)
	if got != 8 {
		t.Errorf("countFiles = %d, want 8", got)
	}
}

func TestCountFiles_SnapshotOtherFiles(t *testing.T) {
	du := sampleDiskUsage()
	du.Snapshots = []config.SnapshotDiskEntry{
		{
			Name:   "snap-no-meta",
			Rootfs: config.FileEntry{Path: "/tmp/r1", Size: config.DiskSize{LogicalBytes: 1024}, Kind: "rootfs"},
			Total:  config.DiskSize{LogicalBytes: 1024},
		},
		{
			Name:   "snap-with-meta",
			Rootfs: config.FileEntry{Path: "/tmp/r2", Size: config.DiskSize{LogicalBytes: 2048}, Kind: "rootfs"},
			OtherFiles: []config.FileEntry{
				{Path: "/tmp/r2/snapshot.json", Size: config.DiskSize{LogicalBytes: 256, PhysicalBytes: 4096}, Kind: "metadata"},
			},
			Total: config.DiskSize{LogicalBytes: 2048 + 256, PhysicalBytes: 4096},
		},
	}
	// Base from the existing fixture is 8 (see TestCountFiles); plus 2 snapshot
	// rootfs + 1 metadata sidecar = 11.
	got := countFiles(&du)
	if got != 11 {
		t.Errorf("countFiles = %d, want 11", got)
	}
}

func samplePruneReport(dry bool) config.PruneReport {
	return config.PruneReport{
		DryRun:     dry,
		ServerName: "prod-mac",
		Scope:      []string{"images", "instances", "orphans"},
		Until:      "72h0m0s",
		Items: []config.PrunedItem{
			{Kind: "image", Name: "old-variant", Path: "/v/old.ext4", Action: "deleted", Freed: config.DiskSize{LogicalBytes: 5 << 30, PhysicalBytes: 4 << 30}},
			{Kind: "instance", Name: "api-old", Action: "deleted", Reason: "stopped 5d ago", Freed: config.DiskSize{LogicalBytes: 2 << 30, PhysicalBytes: 2 << 30}},
			{Kind: "tmp", Path: "/v/stale.ext4.tmp", Action: "deleted", Freed: config.DiskSize{LogicalBytes: 8192, PhysicalBytes: 8192}},
		},
		Skipped: []config.SkippedItem{
			{Kind: "instance", Name: "api-dev", Reason: "cannot prune running shed"},
			{Kind: "instance", Name: "api-test", Reason: "too recent (3h < 72h)"},
		},
		Totals: config.PruneReportTotals{
			Freed: config.DiskSize{LogicalBytes: (5 << 30) + (2 << 30) + 8192, PhysicalBytes: (4 << 30) + (2 << 30) + 8192},
			Items: 3,
		},
		Notes: []string{"physical bytes are attributed"},
	}
}

func TestRenderPrune_DryRun(t *testing.T) {
	var buf bytes.Buffer
	renderPrune(&buf, samplePruneReport(true))
	got := buf.String()
	for _, want := range []string{
		"SERVER: prod-mac (dry-run)",
		"--until 72h0m0s",
		"scope=images+instances+orphans",
		"IMAGES (1,",
		"old-variant",
		"INSTANCES (1,",
		"api-old",
		"stopped 5d ago",
		"ORPHANS (1,",
		"stale.ext4.tmp",
		"SKIPPED (2)",
		"cannot prune running shed",
		"too recent",
		"TOTAL TO FREE:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run render missing %q.\nOUTPUT:\n%s", want, got)
		}
	}
}

func TestRenderPrune_Execute(t *testing.T) {
	var buf bytes.Buffer
	renderPrune(&buf, samplePruneReport(false))
	got := buf.String()
	if strings.Contains(got, "dry-run") {
		t.Errorf("execute output leaked dry-run marker: %s", got)
	}
	if !strings.Contains(got, "FREED:") {
		t.Errorf("execute output missing FREED footer: %s", got)
	}
}

func TestDeletedShedNames(t *testing.T) {
	r := samplePruneReport(false)
	names := deletedShedNames(&r)
	if len(names) != 1 || names[0] != "api-old" {
		t.Errorf("deletedShedNames = %v, want [api-old]", names)
	}
}
