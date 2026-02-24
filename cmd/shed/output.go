package main

import (
	"encoding/json"
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
