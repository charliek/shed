package main

import (
	"fmt"
	"io"
	"os"
	"strings"
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

var (
	systemPruneFlagAll       bool
	systemPruneFlagImages    bool
	systemPruneFlagInstances bool
	systemPruneFlagLogs      bool
	systemPruneFlagOrphans   bool
	systemPruneFlagDryRun    bool
	systemPruneFlagForce     bool
	systemPruneFlagUntil     time.Duration
	systemPruneFlagLogTail   int64
)

var systemPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Reclaim disk space on the shed server",
	Long: `Reclaim disk space by removing unused images, stopped instances past an
age threshold, and orphan sidecar files. Optionally truncates VZ console
logs to their last N bytes.

By default the command runs a dry-run first, prints the candidate table,
and waits for confirmation. Use --force to skip the prompt.

Scope flags are additive; if none are set, default scope is
--images --instances --orphans (not --logs, which is always opt-in).

--until filters stopped-instance pruning by mtime(metadata.json);
  --until 0s prunes every stopped instance regardless of age.

When --json is set, --force is required for the execute path (dry-run
without --force is still allowed).`,
	Args: cobra.NoArgs,
	RunE: runSystemPrune,
}

func init() {
	systemDFCmd.Flags().BoolVar(&systemDFFlagAll, "all", false, "Query every configured server")

	systemPruneCmd.Flags().BoolVar(&systemPruneFlagAll, "all", false, "Prune on every configured server")
	systemPruneCmd.Flags().BoolVar(&systemPruneFlagImages, "images", false, "Prune unreferenced cached images")
	systemPruneCmd.Flags().BoolVar(&systemPruneFlagInstances, "instances", false, "Prune stopped instances older than --until")
	systemPruneCmd.Flags().BoolVar(&systemPruneFlagLogs, "logs", false, "Truncate VZ console.log files to --log-tail-bytes")
	systemPruneCmd.Flags().BoolVar(&systemPruneFlagOrphans, "orphans", false, "Remove orphaned .tmp/.source sidecars")
	systemPruneCmd.Flags().BoolVar(&systemPruneFlagDryRun, "dry-run", false, "Show candidates without mutating")
	systemPruneCmd.Flags().BoolVar(&systemPruneFlagForce, "force", false, "Skip confirmation prompt; required with --json for execute")
	systemPruneCmd.Flags().DurationVar(&systemPruneFlagUntil, "until", 72*time.Hour, "Stopped-instance age threshold (0s = any age)")
	systemPruneCmd.Flags().Int64Var(&systemPruneFlagLogTail, "log-tail-bytes", 0, "Console log truncation target (0 = server default, 5 MiB)")

	systemCmd.AddCommand(systemDFCmd)
	systemCmd.AddCommand(systemPruneCmd)
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
	// Per-shed FILES count includes rootfs + optional console_log + any
	// OtherFiles (typically metadata.json). Matches countFiles() used for
	// the multi-server aggregate footer.
	shedFiles := 0
	runningCount, stoppedCount := 0, 0
	for _, s := range du.Sheds {
		shedFiles++ // rootfs
		if s.ConsoleLog != nil {
			shedFiles++
		}
		shedFiles += len(s.OtherFiles)
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
		shedLabel, shedFiles,
		formatSize(du.Totals.Sheds.LogicalBytes), formatSize(du.Totals.Sheds.PhysicalBytes))
	fmt.Fprintf(tw, "orphans\t%d\t%s\t%s\n",
		len(du.Orphans), formatSize(du.Totals.Orphans.LogicalBytes), formatSize(du.Totals.Orphans.PhysicalBytes))
	fmt.Fprintf(tw, "TOTAL\t%d\t%s\t%s\n",
		imageFiles+shedFiles+len(du.Orphans),
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

// pruneFlagsToOptions translates CLI flags into client options.
func pruneFlagsToOptions(dry bool) SystemPruneOptions {
	return SystemPruneOptions{
		Images:       systemPruneFlagImages,
		Instances:    systemPruneFlagInstances,
		Logs:         systemPruneFlagLogs,
		Orphans:      systemPruneFlagOrphans,
		DryRun:       dry,
		Until:        systemPruneFlagUntil,
		LogTailBytes: systemPruneFlagLogTail,
	}
}

// deletedShedNames returns the shed names that were actually deleted during
// an execute pass — used for client-side cache invalidation so subsequent
// `shed list` calls don't show stale entries.
func deletedShedNames(report *config.PruneReport) []string {
	var names []string
	for _, item := range report.Items {
		if item.Kind == "instance" && item.Action == "deleted" && item.Name != "" {
			names = append(names, item.Name)
		}
	}
	return names
}

func runSystemPrune(cmd *cobra.Command, args []string) error {
	if systemPruneFlagAll && serverFlag != "" {
		return fmt.Errorf("--all and --server are mutually exclusive")
	}
	// `--json` without `--force` blocks the execute path (dry-run is fine).
	// Mirrors the convention in `shed image prune` / `shed delete`.
	if jsonFlag && !systemPruneFlagForce && !systemPruneFlagDryRun {
		return fmt.Errorf("--force is required when combining --json with an execute pass (add --dry-run to preview)")
	}

	if systemPruneFlagAll {
		return runSystemPruneAll()
	}
	return runSystemPruneSingle()
}

func runSystemPruneSingle() error {
	entry, name, err := getServerEntry()
	if err != nil {
		return err
	}

	client := NewAPIClientFromEntry(entry, DefaultTimeout)

	// Always do a dry-run first. --dry-run and the real run return the same
	// shape; the difference is that the real run shows post-mutation Freed.
	dryReport, err := client.SystemPrune(pruneFlagsToOptions(true))
	if err != nil {
		return fmt.Errorf("failed to preview prune on %s: %w", name, err)
	}
	if dryReport.ServerName == "" {
		dryReport.ServerName = name
	}

	// Dry-run-only path: print and exit.
	if systemPruneFlagDryRun {
		if jsonFlag {
			return outputJSON(dryReport)
		}
		renderPrune(os.Stdout, *dryReport)
		return nil
	}

	// In non-JSON mode, show candidates + prompt unless --force.
	if !jsonFlag {
		renderPrune(os.Stdout, *dryReport)
		if dryReport.Totals.Items == 0 {
			// Nothing to do — don't prompt, don't re-hit the server.
			fmt.Println("Nothing to prune.")
			return nil
		}
		if !systemPruneFlagForce {
			fmt.Println()
			prompt := fmt.Sprintf("Proceed with prune on %s? [y/N] ", name)
			if !confirmAction(prompt) {
				fmt.Println("Cancelled.")
				return nil
			}
		}
	} else if dryReport.Totals.Items == 0 {
		// JSON + no candidates: emit the dry-run report and stop.
		return outputJSON(dryReport)
	}

	// Execute.
	report, err := client.SystemPrune(pruneFlagsToOptions(false))
	if err != nil {
		return fmt.Errorf("prune on %s: %w", name, err)
	}
	if report.ServerName == "" {
		report.ServerName = name
	}

	// Invalidate client-side shed cache for any instance we deleted.
	for _, n := range deletedShedNames(report) {
		clientConfig.RemoveShedCache(n)
	}
	if err := clientConfig.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save client config: %v\n", err)
	}

	if jsonFlag {
		return outputJSON(report)
	}
	renderPrune(os.Stdout, *report)
	return nil
}

func runSystemPruneAll() error {
	if len(clientConfig.Servers) == 0 {
		if jsonFlag {
			return outputJSON(config.SystemPruneResponse{Servers: []config.PruneReportOrError{}})
		}
		fmt.Println("No servers configured.")
		return nil
	}

	// Phase 1: dry-run fan-out across all servers.
	dryResults := forEachServer(clientConfig.Servers, func(name string, entry config.ServerEntry) (*config.PruneReport, error) {
		client := NewAPIClientFromEntry(&entry, DefaultTimeout)
		report, err := client.SystemPrune(pruneFlagsToOptions(true))
		if err != nil {
			return nil, err
		}
		if report.ServerName == "" {
			report.ServerName = name
		}
		return report, nil
	})

	if systemPruneFlagDryRun {
		return renderPruneAllReports(dryResults)
	}

	// Render dry-run table to stdout unless --json; always prompt once if
	// there are any candidates and --force isn't set.
	if !jsonFlag {
		for i, r := range dryResults {
			if i > 0 {
				fmt.Println()
			}
			if r.Err != nil {
				fmt.Printf("SERVER:  %s\n  error: %v\n", r.ServerName, r.Err)
				continue
			}
			renderPrune(os.Stdout, *r.Value)
		}
	}

	total := 0
	for _, r := range dryResults {
		if r.Value != nil {
			total += r.Value.Totals.Items
		}
	}
	if total == 0 {
		if jsonFlag {
			return renderPruneAllReports(dryResults)
		}
		fmt.Println("\nNothing to prune across all servers.")
		return nil
	}
	if !jsonFlag && !systemPruneFlagForce {
		fmt.Println()
		prompt := fmt.Sprintf("Proceed with prune across %d server(s)? [y/N] ", len(dryResults))
		if !confirmAction(prompt) {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	// Phase 2: execute fan-out.
	execResults := forEachServer(clientConfig.Servers, func(name string, entry config.ServerEntry) (*config.PruneReport, error) {
		client := NewAPIClientFromEntry(&entry, DefaultTimeout)
		report, err := client.SystemPrune(pruneFlagsToOptions(false))
		if err != nil {
			return nil, err
		}
		if report.ServerName == "" {
			report.ServerName = name
		}
		return report, nil
	})

	// Invalidate client shed cache for every deleted instance across servers.
	cacheDirty := false
	for _, r := range execResults {
		if r.Err != nil || r.Value == nil {
			continue
		}
		for _, n := range deletedShedNames(r.Value) {
			clientConfig.RemoveShedCache(n)
			cacheDirty = true
		}
	}
	if cacheDirty {
		if err := clientConfig.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save client config: %v\n", err)
		}
	}

	return renderPruneAllReports(execResults)
}

// renderPruneAllReports outputs a slice of per-server prune results in the
// current mode (text or JSON) and returns non-nil if the caller should fail.
// Per-server errors are reported inline and never cause a non-zero exit.
func renderPruneAllReports(results []ServerResult[*config.PruneReport]) error {
	if jsonFlag {
		resp := config.SystemPruneResponse{Servers: make([]config.PruneReportOrError, 0, len(results))}
		for _, r := range results {
			entry := config.PruneReportOrError{ServerName: r.ServerName}
			if r.Err != nil {
				entry.Error = r.Err.Error()
			} else {
				entry.Report = r.Value
			}
			resp.Servers = append(resp.Servers, entry)
		}
		return outputJSON(resp)
	}
	for i, r := range results {
		if i > 0 {
			fmt.Println()
		}
		if r.Err != nil {
			fmt.Printf("SERVER:  %s\n  error: %v\n", r.ServerName, r.Err)
			continue
		}
		renderPrune(os.Stdout, *r.Value)
	}
	return nil
}

// renderPrune writes the prune report to w as a sectioned table.
func renderPrune(w io.Writer, r config.PruneReport) {
	mode := "execute"
	if r.DryRun {
		mode = "dry-run"
	}
	fmt.Fprintf(w, "SERVER: %s (%s)", r.ServerName, mode)
	if r.Until != "" {
		fmt.Fprintf(w, " --until %s", r.Until)
	}
	if len(r.Scope) > 0 {
		fmt.Fprintf(w, " scope=%s", strings.Join(r.Scope, "+"))
	}
	fmt.Fprintln(w)

	// Bucket items by kind for clearer output.
	var images, instances, orphans, logs []config.PrunedItem
	for _, item := range r.Items {
		switch item.Kind {
		case "image":
			images = append(images, item)
		case "instance":
			instances = append(instances, item)
		case "console_log":
			logs = append(logs, item)
		default:
			orphans = append(orphans, item)
		}
	}

	if len(images) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "IMAGES (%d, %s)\n", len(images), formatSize(sumFreedLogical(images)))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tPATH\tLOGICAL\tPHYSICAL")
		for _, it := range images {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", it.Name, it.Path, formatSize(it.Freed.LogicalBytes), formatSize(it.Freed.PhysicalBytes))
		}
		tw.Flush()
	}

	if len(instances) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "INSTANCES (%d, %s)\n", len(instances), formatSize(sumFreedLogical(instances)))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tREASON\tLOGICAL\tPHYSICAL")
		for _, it := range instances {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", it.Name, it.Reason, formatSize(it.Freed.LogicalBytes), formatSize(it.Freed.PhysicalBytes))
		}
		tw.Flush()
	}

	if len(orphans) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "ORPHANS (%d, %s)\n", len(orphans), formatSize(sumFreedLogical(orphans)))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PATH\tKIND\tLOGICAL\tPHYSICAL")
		for _, it := range orphans {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", it.Path, it.Kind, formatSize(it.Freed.LogicalBytes), formatSize(it.Freed.PhysicalBytes))
		}
		tw.Flush()
	}

	if len(logs) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "LOGS TRUNCATED (%d, %s freed)\n", len(logs), formatSize(sumFreedLogical(logs)))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tPATH\tFREED")
		for _, it := range logs {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", it.Name, it.Path, formatSize(it.Freed.LogicalBytes))
		}
		tw.Flush()
	}

	if len(r.Skipped) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "SKIPPED (%d)\n", len(r.Skipped))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "KIND\tNAME/PATH\tREASON")
		for _, s := range r.Skipped {
			label := s.Name
			if label == "" {
				label = s.Path
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Kind, label, s.Reason)
		}
		tw.Flush()
	}

	fmt.Fprintln(w)
	if r.DryRun {
		fmt.Fprintf(w, "TOTAL TO FREE: %s logical / %s physical (%d items)\n",
			formatSize(r.Totals.Freed.LogicalBytes), formatSize(r.Totals.Freed.PhysicalBytes), r.Totals.Items)
	} else {
		fmt.Fprintf(w, "FREED: %s logical / %s physical (%d items)\n",
			formatSize(r.Totals.Freed.LogicalBytes), formatSize(r.Totals.Freed.PhysicalBytes), r.Totals.Items)
	}

	for _, note := range r.Notes {
		fmt.Fprintf(w, "Note: %s\n", note)
	}
}

// sumFreedLogical sums the logical bytes of a slice of items. Used for
// section headers.
func sumFreedLogical(items []config.PrunedItem) int64 {
	var sum int64
	for _, it := range items {
		sum += it.Freed.LogicalBytes
	}
	return sum
}
