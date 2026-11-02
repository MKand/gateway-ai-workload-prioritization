// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MKand/gateway-ai-workload-prioritization/pkg/logging"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

func envoyHeaders(m map[string]string) *extprocv3.HttpHeaders {
	hm := &corev3.HeaderMap{}
	for k, v := range m {
		hm.Headers = append(hm.Headers, &corev3.HeaderValue{Key: k, RawValue: []byte(v)})
	}
	return &extprocv3.HttpHeaders{Headers: hm}
}

func TestExtProcServer_StructuredDecisionLogging(t *testing.T) {
	t.Run("shed_and_reroute_emit_warning_logs_while_forward_stays_quiet_at_info", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := logging.New("json", "info", &buf)
		if err != nil {
			t.Fatalf("logging.New() error: %v", err)
		}

		// US primary at 97% (saturated), EU fallback at 10% (healthy).
		srv := NewExtProcServer(newTestEvaluator(t, snapshot(0.97, 0.10)))
		srv.SetLogger(logger)

		// 1. Critical forward -> no INFO/WARN log emitted at info level.
		srv.handleRequestHeaders(t.Context(), envoyHeaders(reqHeaders("critical", "")))
		if buf.Len() != 0 {
			t.Errorf("forward request emitted unexpected info-level log: %s", buf.String())
		}

		// 2. Best-effort shed -> WARNING log with decision=shed.
		srv.handleRequestHeaders(t.Context(), envoyHeaders(reqHeaders("best_effort", "")))

		// 3. Custom reroute -> WARNING log with decision=reroute and target_region.
		srv.handleRequestHeaders(t.Context(), envoyHeaders(reqHeaders("custom", testPolicy)))

		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("logged line count = %d, want 2:\n%s", len(lines), buf.String())
		}

		var shedLog, rerouteLog map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &shedLog); err != nil {
			t.Fatalf("shed log JSON error: %v", err)
		}
		if shedLog["severity"] != "WARNING" || shedLog["decision"] != DecisionShed {
			t.Errorf("shed log = %v, want severity=WARNING decision=shed", shedLog)
		}

		if err := json.Unmarshal([]byte(lines[1]), &rerouteLog); err != nil {
			t.Fatalf("reroute log JSON error: %v", err)
		}
		if rerouteLog["severity"] != "WARNING" || rerouteLog["decision"] != DecisionReroute || rerouteLog["target_region"] != euRegion {
			t.Errorf("reroute log = %v, want severity=WARNING decision=reroute target_region=%s", rerouteLog, euRegion)
		}
	})

	t.Run("debug_level_logs_forwarded_requests", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := logging.New("json", "debug", &buf)
		if err != nil {
			t.Fatalf("logging.New() error: %v", err)
		}

		srv := NewExtProcServer(newTestEvaluator(t, snapshot(0.20, 0.10)))
		srv.SetLogger(logger)
		srv.handleRequestHeaders(t.Context(), envoyHeaders(reqHeaders("critical", "")))

		var entry map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
			t.Fatalf("debug log JSON error: %v (raw: %q)", err, buf.String())
		}
		if entry["severity"] != "DEBUG" || entry["decision"] != DecisionForward {
			t.Errorf("debug forward log = %v, want severity=DEBUG decision=forward", entry)
		}
	})
}
