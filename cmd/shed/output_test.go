package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestOutputJSON(t *testing.T) {
	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	data := map[string]string{"key": "value"}
	if err := outputJSON(data); err != nil {
		t.Fatalf("outputJSON returned error: %v", err)
	}

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	// Verify it's valid JSON
	var parsed map[string]string
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, output)
	}

	if parsed["key"] != "value" {
		t.Errorf("expected key=value, got key=%s", parsed["key"])
	}

	// Verify indentation
	if output != "{\n  \"key\": \"value\"\n}\n" {
		t.Errorf("unexpected indentation:\n%s", output)
	}
}

func TestOutputJSONEmptySlice(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Ensure empty slice produces [] not null
	data := make([]string, 0)
	if err := outputJSON(data); err != nil {
		t.Fatalf("outputJSON returned error: %v", err)
	}

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if output != "[]\n" {
		t.Errorf("expected []\n, got %q", output)
	}
}

func TestOutputJSONNilSlice(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// nil slice produces null in JSON — callers should use make([]T, 0)
	var data []string
	if err := outputJSON(data); err != nil {
		t.Fatalf("outputJSON returned error: %v", err)
	}

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)
	output := buf.String()

	if output != "null\n" {
		t.Errorf("expected null\n, got %q", output)
	}
}

func TestActionResultSerialization(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	result := ActionResult{
		Status: "ok",
		Action: "created",
		Name:   "myproj",
		Server: "devbox",
	}
	if err := outputJSON(result); err != nil {
		t.Fatalf("outputJSON returned error: %v", err)
	}

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)

	var parsed ActionResult
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if parsed.Status != "ok" {
		t.Errorf("expected status=ok, got %s", parsed.Status)
	}
	if parsed.Action != "created" {
		t.Errorf("expected action=created, got %s", parsed.Action)
	}
	if parsed.Name != "myproj" {
		t.Errorf("expected name=myproj, got %s", parsed.Name)
	}
	if parsed.Server != "devbox" {
		t.Errorf("expected server=devbox, got %s", parsed.Server)
	}
	if parsed.Details != nil {
		t.Errorf("expected details=nil, got %v", parsed.Details)
	}
}

func TestActionResultWithDetails(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	result := ActionResult{
		Status: "ok",
		Action: "started",
		Name:   "myproj",
		Details: struct {
			Backend string `json:"backend"`
		}{Backend: "docker"},
	}
	if err := outputJSON(result); err != nil {
		t.Fatalf("outputJSON returned error: %v", err)
	}

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)

	var parsed map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	details, ok := parsed["details"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected details to be an object, got %T", parsed["details"])
	}
	if details["backend"] != "docker" {
		t.Errorf("expected backend=docker, got %v", details["backend"])
	}
}

func TestActionResultOmitsEmptyFields(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	result := ActionResult{
		Status: "ok",
		Action: "removed",
		Name:   "devbox",
	}
	if err := outputJSON(result); err != nil {
		t.Fatalf("outputJSON returned error: %v", err)
	}

	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	buf.ReadFrom(r)

	var parsed map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// server and details should be omitted
	if _, ok := parsed["server"]; ok {
		t.Error("expected server to be omitted")
	}
	if _, ok := parsed["details"]; ok {
		t.Error("expected details to be omitted")
	}
}

func TestOutputError(t *testing.T) {
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	if err := outputError(fmt.Errorf("something went wrong")); err != nil {
		t.Fatalf("outputError returned error: %v", err)
	}

	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	buf.ReadFrom(r)

	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}

	if parsed.Error != "something went wrong" {
		t.Errorf("expected error='something went wrong', got %q", parsed.Error)
	}
}
