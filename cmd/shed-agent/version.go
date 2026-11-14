//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/version"
)

// readLayoutFile reads the guest layout number: one decimal integer. A
// missing file is layout 0 (an image that predates layouts).
func readLayoutFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read layout file: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("layout file %s: want one non-negative integer, got %q", path, strings.TrimSpace(string(data)))
	}
	return n, nil
}

// buildAgentInfo is what this agent reports in "shed-agent version" and in a
// probe reply.
func buildAgentInfo(layout int) guest.AgentInfo {
	return guest.AgentInfo{
		Version:  version.Version,
		Protocol: guest.AgentProtocol,
		Layout:   layout,
	}
}

// runVersion implements "shed-agent version [--layout-file <path>]": one line
// of JSON on stdout. It needs no root and no running agent.
func runVersion(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("version", flag.ContinueOnError)
	fset.SetOutput(stderr)
	layoutFile := fset.String("layout-file", guest.LayoutFile, "file holding the guest layout number")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if fset.NArg() != 0 {
		fmt.Fprintf(stderr, "version: unexpected arguments %v\n", fset.Args())
		return 2
	}

	layout, err := readLayoutFile(*layoutFile)
	if err != nil {
		fmt.Fprintf(stderr, "version: %v\n", err)
		return 1
	}
	data, err := json.Marshal(buildAgentInfo(layout))
	if err != nil {
		fmt.Fprintf(stderr, "version: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", data); err != nil {
		return 1
	}
	return 0
}
