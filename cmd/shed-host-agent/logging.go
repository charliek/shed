package main

import (
	"io"
	"os"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// newLogWriter returns the destination for the agent's operational log. With an
// empty logFile it writes to stderr (dev / foreground runs, where launchd or the
// terminal captures it). With a path it writes to a self-rotated file via
// lumberjack so the log can't grow unbounded — the brew service passes
// `-log-file <var>/log/shed-host-agent.log` instead of relying on launchd to
// capture (and never rotate) stderr.
func newLogWriter(logFile string) io.Writer {
	if logFile == "" {
		return os.Stderr
	}
	return &lumberjack.Logger{
		Filename:   logFile,
		MaxSize:    10, // MB before rotating (lumberjack measures in megabytes)
		MaxBackups: 5,  // keep the last 5 rotated files
		MaxAge:     30, // days
		Compress:   true,
	}
}
