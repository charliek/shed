package main

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// loadEnvironmentD reads all *.conf files in the given directory and returns
// environment variables in KEY=VALUE format. This follows the systemd
// environment.d convention: one variable per line, # comments, blank lines
// ignored. Files are read in alphabetical order so later files can override
// earlier ones (matching systemd behavior).
func loadEnvironmentD(dir string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, "*.conf"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	sort.Strings(matches)

	var env []string
	for _, path := range matches {
		vars := parseEnvironmentFile(path)
		env = append(env, vars...)
	}
	return env
}

// parseEnvironmentFile reads a single environment file and returns KEY=VALUE pairs.
func parseEnvironmentFile(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var vars []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "=") {
			vars = append(vars, line)
		}
	}
	return vars
}
