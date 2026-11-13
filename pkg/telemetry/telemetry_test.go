package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegistry_TextFormat(t *testing.T) {
	reg := NewRegistry()
	c := reg.NewCounterVec("governor_test_total", "A test counter.", "target", "priority")
	c.Inc("us-central1/m", "critical")
	c.Add(2, "us-central1/m", "critical")
	c.Inc(`we"ird\n`, "best_effort")
	c.Add(-5, "us-central1/m", "critical") // ignored
	reg.NewGaugeFunc("governor_test_gauge", "A test gauge.", func() []Sample {
		return []Sample{{LabelValues: []string{"b"}, Value: 2.5}, {LabelValues: []string{"a"}, Value: -1}}
	}, "k")

	var buf bytes.Buffer
	reg.WriteText(&buf)
	got := buf.String()
	for _, want := range []string{
		"# HELP governor_test_total A test counter.\n# TYPE governor_test_total counter\n",
		`governor_test_total{target="us-central1/m",priority="critical"} 3` + "\n",
		`governor_test_total{target="we\"ird\\n",priority="best_effort"} 1` + "\n",
		"# TYPE governor_test_gauge gauge\n",
		"governor_test_gauge{k=\"a\"} -1\ngovernor_test_gauge{k=\"b\"} 2.5\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n---\n%s", want, got)
		}
	}
	if c.Value("us-central1/m", "critical") != 3 || c.Value("nope") != 0 {
		t.Errorf("Value() mismatch")
	}
}

func TestCounterVec_Concurrent(t *testing.T) {
	c := NewRegistry().NewCounterVec("x_total", "x", "l")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Inc("a")
			}
		}()
	}
	wg.Wait()
	if got := c.Value("a"); got != 5000 {
		t.Fatalf("got %d, want 5000", got)
	}
}

func TestSetupLogging(t *testing.T) {
	defer slog.SetDefault(slog.Default())
	var buf bytes.Buffer
	if err := SetupLogging(&buf, "json", "info"); err != nil {
		t.Fatal(err)
	}
	slog.Warn("hello", "target", "global/m")
	slog.Debug("hidden")
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("not one JSON line: %q (%v)", buf.String(), err)
	}
	if rec["severity"] != "WARNING" || rec["message"] != "hello" || rec["target"] != "global/m" {
		t.Errorf("unexpected record %v", rec)
	}

	if err := SetupLogging(io.Discard, "xml", "info"); err == nil {
		t.Error("expected error for unknown format")
	}
	if err := SetupLogging(io.Discard, "text", "loud"); err == nil {
		t.Error("expected error for unknown level")
	}
}

func TestServeHTTP(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close()

	reg := NewRegistry()
	reg.NewCounterVec("served_total", "s").Inc()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeHTTP(ctx, addr, reg) }()

	var body string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, "served_total 1") {
		t.Fatalf("metrics body = %q", body)
	}
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	resp.Body.Close()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ServeHTTP returned %v", err)
	}
	if err := ServeHTTP(context.Background(), "", reg); err != nil {
		t.Fatalf("empty addr should be a no-op, got %v", err)
	}
}
