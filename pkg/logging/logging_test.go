// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package logging_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/logging"
)

func TestNew_CloudLoggingJSONMapping(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New("json", "debug", &buf)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	logger.Debug("trace details", "key", "v1")
	logger.Info("server started", "port", 50051)
	logger.Warn("quota near ceiling", "utilization", 0.75)
	logger.Error("reconcile failed", "attempt", 2)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("logged line count = %d, want 4:\n%s", len(lines), buf.String())
	}

	wantSeverities := []string{"DEBUG", "INFO", "WARNING", "ERROR"}
	wantMessages := []string{"trace details", "server started", "quota near ceiling", "reconcile failed"}

	for i, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line[%d] invalid JSON %q: %v", i, line, err)
		}
		if got := entry["severity"]; got != wantSeverities[i] {
			t.Errorf("line[%d] severity = %v, want %q", i, got, wantSeverities[i])
		}
		if got := entry["message"]; got != wantMessages[i] {
			t.Errorf("line[%d] message = %v, want %q", i, got, wantMessages[i])
		}
		if _, hasLevel := entry["level"]; hasLevel {
			t.Errorf("line[%d] unexpectedly contains raw 'level' key: %v", i, entry)
		}
		if _, hasMsg := entry["msg"]; hasMsg {
			t.Errorf("line[%d] unexpectedly contains raw 'msg' key: %v", i, entry)
		}
	}
}

func TestNew_LevelFilteringAndTextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger, err := logging.New("text", "warn", &buf)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	logger.Info("suppressed info")
	logger.Warn("emitted warning", "component", "dataplane")

	out := buf.String()
	if strings.Contains(out, "suppressed info") {
		t.Errorf("output %q contains info message below warn threshold", out)
	}
	if !strings.Contains(out, "emitted warning") || !strings.Contains(out, "component=dataplane") {
		t.Errorf("output = %q, want warning message and attribute", out)
	}
}

func TestNew_DefaultsAndValidationErrors(t *testing.T) {
	var buf bytes.Buffer
	if _, err := logging.New("", "", &buf); err != nil {
		t.Fatalf("New(\"\", \"\") unexpected error: %v", err)
	}
	if _, err := logging.New("xml", "info", &buf); err == nil {
		t.Errorf("New(format=xml) error = nil, want validation error")
	}
	if _, err := logging.New("json", "verbose", &buf); err == nil {
		t.Errorf("New(level=verbose) error = nil, want validation error")
	}
}
