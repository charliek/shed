package main

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var systemCmd = &cobra.Command{
	Use:   "system",
	Short: "Inspect and manage shed server disk usage",
	Long:  "Inspect and manage disk usage on shed servers (image cache, per-instance rootfs, orphans).",
}

var systemDFFlagAll bool

var systemDFCmd = &cobra.Command{
	Use:   "df",
	Short: "Show disk usage for the shed server",
	Long: `Show disk usage for the shed server's image cache, per-instance rootfs
copies, kernel/initrd, and orphan sidecar files.

By default queries the active server. Use -s/--server to target a specific
server, or --all to fan out to every configured server.

Use -v to show per-image and per-shed rows; otherwise a rollup is printed.`,
	Args: cobra.NoArgs,
	RunE: runSystemDF,
}

func init() {
	systemDFCmd.Flags().BoolVar(&systemDFFlagAll, "all", false, "Query every configured server")

	systemCmd.AddCommand(systemDFCmd)
	rootCmd.AddCommand(systemCmd)
}

func runSystemDF(cmd *cobra.Command, args []string) error {
	if systemDFFlagAll && serverFlag != "" {
		return fmt.Errorf("--all and --server are mutually exclusive")
	}

	if systemDFFlagAll {
		return runSystemDFAll()
	}
	return runSystemDFSingle()
}

func runSystemDFSingle() error {
	entry, name, err := getServerEntry()
	if err != nil {
		return err
	}

	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	du, err := client.GetSystemDF()
	if err != nil {
		return fmt.Errorf("failed to get disk usage from %s: %w", name, err)
	}

	// Server may not know its client-side name; overwrite for display.
	if du.ServerName == "" {
		du.ServerName = name
	}

	if jsonFlag {
		return outputJSON(du)
	}

	renderDF(os.Stdout, *du, verboseLevel >= 1)
	return nil
}

func runSystemDFAll() error {
	if len(clientConfig.Servers) == 0 {
		if jsonFlag {
			return outputJSON(config.SystemDFResponse{Servers: []config.DiskUsageOrError{}})
		}
		fmt.Println("No servers configured.")
		return nil
	}

	results := forEachServer(clientConfig.Servers, func(name string, entry config.ServerEntry) (*config.DiskUsage, error) {
		client := NewAPIClientFromEntry(&entry, DefaultTimeout)
		du, err := client.GetSystemDF()
		if err != nil {
			return nil, err
		}
		if du.ServerName == "" {
			du.ServerName = name
		}
		return du, nil
	})

	if jsonFlag {
		resp := config.SystemDFResponse{Servers: make([]config.DiskUsageOrError, 0, len(results))}
		for _, r := range results {
			entry := config.DiskUsageOrError{ServerName: r.ServerName}
			if r.Err != nil {
				entry.Error = r.Err.Error()
			} else {
				entry.Usage = r.Value
			}
			resp.Servers = append(resp.Servers, entry)
		}
		return outputJSON(resp)
	}

	// Text mode: one section per server, aggregate footer.
	var totals config.DiskUsageTotals
	var totalFiles int
	for i, r := range results {
		if i > 0 {
			fmt.Println()
		}
		if r.Err != nil {
			fmt.Printf("SERVER:  %s\n  error: %v\n", r.ServerName, r.Err)
			continue
		}
		renderDF(os.Stdout, *r.Value, verboseLevel >= 1)
		totals.Images.LogicalBytes += r.Value.Totals.Images.LogicalBytes
		totals.Images.PhysicalBytes += r.Value.Totals.Images.PhysicalBytes
		totals.Sheds.LogicalBytes += r.Value.Totals.Sheds.LogicalBytes
		totals.Sheds.PhysicalBytes += r.Value.Totals.Sheds.PhysicalBytes
		totals.Orphans.LogicalBytes += r.Value.Totals.Orphans.LogicalBytes
		totals.Orphans.PhysicalBytes += r.Value.Totals.Orphans.PhysicalBytes
		totals.All.LogicalBytes += r.Value.Totals.All.LogicalBytes
		totals.All.PhysicalBytes += r.Value.Totals.All.PhysicalBytes
		totalFiles += countFiles(r.Value)
	}

	fmt.Println()
	fmt.Printf("AGGREGATE across %d server(s): %s logical / %s physical (%d files)\n",
		len(results), formatSize(totals.All.LogicalBytes), formatSize(totals.All.PhysicalBytes), totalFiles)

	return nil
}

// renderDF writes a tabular DF report for a single server to w.
// verbose=true adds per-image and per-shed rows.
func renderDF(w io.Writer, du config.DiskUsage, verbose bool) {
	fmt.Fprintf(w, "SERVER:  %s      BACKEND: %s\n", du.ServerName, du.Backend)
	fmt.Fprintf(w, "GENERATED: %s\n\n", du.GeneratedAt.UTC().Format(time.RFC3339))

	// Rollup table.
	imageFiles := len(du.Images)
	if du.Kernel != nil {
		imageFiles++
	}
	if du.Initrd != nil {
		imageFiles++
	}
	runningCount, stoppedCount := 0, 0
	for _, s := range du.Sheds {
		if s.Status == config.StatusRunning {
			runningCount++
		} else {
			stoppedCount++
		}
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tFILES\tLOGICAL\tPHYSICAL")
	fmt.Fprintf(tw, "images\t%d\t%s\t%s\n",
		imageFiles, formatSize(du.Totals.Images.LogicalBytes), formatSize(du.Totals.Images.PhysicalBytes))
	shedLabel := fmt.Sprintf("sheds (%d stopped, %d run)", stoppedCount, runningCount)
	fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n",
		shedLabel, len(du.Sheds),
		formatSize(du.Totals.Sheds.LogicalBytes), formatSize(du.Totals.Sheds.PhysicalBytes))
	fmt.Fprintf(tw, "orphans\t%d\t%s\t%s\n",
		len(du.Orphans), formatSize(du.Totals.Orphans.LogicalBytes), formatSize(du.Totals.Orphans.PhysicalBytes))
	fmt.Fprintf(tw, "TOTAL\t%d\t%s\t%s\n",
		imageFiles+len(du.Sheds)+len(du.Orphans),
		formatSize(du.Totals.All.LogicalBytes), formatSize(du.Totals.All.PhysicalBytes))
	tw.Flush()

	if verbose {
		renderDFVerbose(w, du)
	}

	if len(du.Notes) > 0 {
		fmt.Fprintln(w)
		for _, note := range du.Notes {
			fmt.Fprintf(w, "Note: %s\n", note)
		}
	}
}

// renderDFVerbose appends per-image and per-shed sections after the rollup.
func renderDFVerbose(w io.Writer, du config.DiskUsage) {
	if len(du.Images) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "IMAGES (%d)\n", len(du.Images))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tREF\tLOGICAL\tPHYSICAL")
		for _, img := range du.Images {
			ref := img.DockerRef
			if ref == "" {
				ref = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				img.Name, ref,
				formatSize(img.Size.LogicalBytes), formatSize(img.Size.PhysicalBytes))
		}
		tw.Flush()
	}

	if du.Kernel != nil || du.Initrd != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "KERNEL / INITRD")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PATH\tLOGICAL\tPHYSICAL")
		if du.Kernel != nil {
			fmt.Fprintf(tw, "%s\t%s\t%s\n",
				du.Kernel.Path,
				formatSize(du.Kernel.Size.LogicalBytes), formatSize(du.Kernel.Size.PhysicalBytes))
		}
		if du.Initrd != nil {
			fmt.Fprintf(tw, "%s\t%s\t%s\n",
				du.Initrd.Path,
				formatSize(du.Initrd.Size.LogicalBytes), formatSize(du.Initrd.Size.PhysicalBytes))
		}
		tw.Flush()
	}

	if len(du.Sheds) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "SHEDS (%d)\n", len(du.Sheds))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATUS\tIMAGE\tROOTFS\tCONSOLE\tTOTAL")
		for _, s := range du.Sheds {
			console := "n/a"
			if s.ConsoleLog != nil {
				console = formatSize(s.ConsoleLog.Size.LogicalBytes)
			}
			image := s.Image
			if image == "" {
				image = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				s.Name, s.Status, image,
				formatSize(s.Rootfs.Size.LogicalBytes),
				console,
				formatSize(s.Total.LogicalBytes))
		}
		tw.Flush()
	}

	if len(du.Orphans) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "ORPHANS (%d)\n", len(du.Orphans))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PATH\tLOGICAL\tPHYSICAL\tKIND")
		for _, f := range du.Orphans {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				f.Path,
				formatSize(f.Size.LogicalBytes), formatSize(f.Size.PhysicalBytes),
				f.Kind)
		}
		tw.Flush()
	}
}

// countFiles returns the number of files represented in a DiskUsage report.
// Used for the multi-server aggregate footer.
func countFiles(du *config.DiskUsage) int {
	n := len(du.Images) + len(du.Orphans)
	if du.Kernel != nil {
		n++
	}
	if du.Initrd != nil {
		n++
	}
	for _, s := range du.Sheds {
		n++ // rootfs
		if s.ConsoleLog != nil {
			n++
		}
		n += len(s.OtherFiles)
	}
	return n
}

// formatSize renders a byte count with sensible units. Unlike the coarse
// formatBytes in image.go (GB/MB only), this handles B through GB because df
// surfaces everything from bytes (lock files) to multi-GB rootfs images.
func formatSize(b int64) string {
	const (
		gb = 1024 * 1024 * 1024
		mb = 1024 * 1024
		kb = 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
