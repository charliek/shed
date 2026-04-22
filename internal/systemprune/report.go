package systemprune

import (
	"fmt"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/diskstat"
	"github.com/charliek/shed/internal/vmimage"
)

// DefaultLogTailBytes is the tail size console.log is truncated to when
// PruneOptions.LogTailBytes is zero. Matches the previous per-backend
// default so behaviour is unchanged.
const DefaultLogTailBytes = 5 * 1024 * 1024

// NormalizePruneOptions applies defaults: empty scope → images +
// instances + orphans; zero LogTailBytes → DefaultLogTailBytes.
func NormalizePruneOptions(opts backend.PruneOptions) backend.PruneOptions {
	if !opts.Images && !opts.Instances && !opts.Logs && !opts.Orphans {
		opts.Images = true
		opts.Instances = true
		opts.Orphans = true
	}
	if opts.LogTailBytes == 0 {
		opts.LogTailBytes = DefaultLogTailBytes
	}
	return opts
}

// ScopeFlags returns the list of active scope names for the report.
func ScopeFlags(opts backend.PruneOptions) []string {
	var scope []string
	if opts.Images {
		scope = append(scope, "images")
	}
	if opts.Instances {
		scope = append(scope, "instances")
	}
	if opts.Logs {
		scope = append(scope, "logs")
	}
	if opts.Orphans {
		scope = append(scope, "orphans")
	}
	return scope
}

// FinalizeReport sums Freed across all items into Totals and appends the
// shared physical-bytes attribution note.
func FinalizeReport(r *config.PruneReport) {
	for _, item := range r.Items {
		r.Totals.Freed.LogicalBytes += item.Freed.LogicalBytes
		r.Totals.Freed.PhysicalBytes += item.Freed.PhysicalBytes
	}
	r.Totals.Items = len(r.Items)
	r.Notes = append(r.Notes,
		"physical bytes are attributed (stat.Blocks*512); clonefile/FICLONE clones and hardlinks may report bytes that won't actually be reclaimed",
	)
}

// ImageToPrunedItem wraps a vmimage.ImageInfo as a PrunedItem. Physical
// bytes are pulled via diskstat so the report reflects the actual on-disk
// footprint (ImageInfo only carries logical SizeBytes).
func ImageToPrunedItem(img vmimage.ImageInfo, dry bool) config.PrunedItem {
	size := config.DiskSize{LogicalBytes: img.SizeBytes}
	if img.Path != "" {
		if logical, physical, err := diskstat.Stat(img.Path); err == nil {
			size.LogicalBytes = logical
			size.PhysicalBytes = physical
		}
	}
	reason := ""
	if dry {
		reason = "unreferenced image"
	}
	return config.PrunedItem{
		Kind: "image", Name: img.Name, Path: img.Path, Action: "deleted",
		Freed: size, Reason: reason,
	}
}

// HumanDuration formats a duration for SkippedItem.Reason / PrunedItem.Reason.
// Rounds to the nearest sensible unit: seconds, minutes, hours, or days.
func HumanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int64(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int64(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int64(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int64(d.Hours()/24))
	}
}
