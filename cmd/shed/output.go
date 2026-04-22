package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// ActionResult is the standard envelope for mutation commands in JSON mode.
type ActionResult struct {
	Status  string      `json:"status"`
	Action  string      `json:"action"`
	Name    string      `json:"name,omitempty"`
	Server  string      `json:"server,omitempty"`
	Details interface{} `json:"details,omitempty"`
}

// outputJSON writes v as indented JSON to stdout.
func outputJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// outputError writes a JSON error object to stderr.
func outputError(err error) error {
	obj := struct {
		Error string `json:"error"`
	}{
		Error: err.Error(),
	}
	enc := json.NewEncoder(os.Stderr)
	enc.SetIndent("", "  ")
	return enc.Encode(obj)
}

// formatSize renders a byte count with sensible units. Handles B through GB
// because callers surface everything from bytes (lock files) to multi-GB
// rootfs images.
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
