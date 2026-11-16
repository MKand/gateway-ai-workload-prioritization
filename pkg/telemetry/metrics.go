// Package telemetry provides zero-dependency Prometheus-format metrics and
// structured logging setup for the governor binaries.
package telemetry

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metrics and renders them in the Prometheus text exposition
// format. It is safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	metrics []metric
}

type metric interface {
	write(w io.Writer)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) add(m metric) {
	r.mu.Lock()
	r.metrics = append(r.metrics, m)
	r.mu.Unlock()
}

// WriteText writes every metric in registration order.
func (r *Registry) WriteText(w io.Writer) {
	r.mu.Lock()
	ms := append([]metric(nil), r.metrics...)
	r.mu.Unlock()
	for _, m := range ms {
		m.write(w)
	}
}

// Handler serves the registry at /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.WriteText(w)
	})
}

// CounterVec is a monotonically increasing counter partitioned by labels.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.RWMutex
	values     map[string]*atomic.Int64 // key: label values joined by \xff
}

// NewCounterVec registers a counter with the given label names.
func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, values: map[string]*atomic.Int64{}}
	r.add(c)
	return c
}

// Add increments the counter for the given label values (one per label name).
// Negative deltas are ignored.
func (c *CounterVec) Add(delta int64, labelValues ...string) {
	if delta < 0 {
		return
	}
	key := strings.Join(labelValues, "\xff")
	c.mu.RLock()
	v, ok := c.values[key]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		if v, ok = c.values[key]; !ok {
			v = &atomic.Int64{}
			c.values[key] = v
		}
		c.mu.Unlock()
	}
	v.Add(delta)
}

// Inc adds 1.
func (c *CounterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Value returns the current value for the given label values (for tests).
func (c *CounterVec) Value(labelValues ...string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.values[strings.Join(labelValues, "\xff")]; ok {
		return v.Load()
	}
	return 0
}

func (c *CounterVec) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
	c.mu.RLock()
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %d\n", c.name, formatLabels(c.labels, strings.Split(k, "\xff")), c.values[k].Load())
	}
	c.mu.RUnlock()
}

// GaugeFunc is a gauge whose labelled values are computed at scrape time.
type GaugeFunc struct {
	name, help string
	labels     []string
	fn         func() []Sample
}

// Sample is one labelled gauge value.
type Sample struct {
	LabelValues []string
	Value       float64
}

// NewGaugeFunc registers a gauge computed by fn on every scrape.
func (r *Registry) NewGaugeFunc(name, help string, fn func() []Sample, labels ...string) *GaugeFunc {
	g := &GaugeFunc{name: name, help: help, labels: labels, fn: fn}
	r.add(g)
	return g
}

func (g *GaugeFunc) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
	samples := g.fn()
	sort.Slice(samples, func(i, j int) bool {
		return strings.Join(samples[i].LabelValues, "\xff") < strings.Join(samples[j].LabelValues, "\xff")
	})
	for _, s := range samples {
		fmt.Fprintf(w, "%s%s %s\n", g.name, formatLabels(g.labels, s.LabelValues), strconv.FormatFloat(s.Value, 'g', -1, 64))
	}
}

func formatLabels(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	parts := make([]string, 0, len(names))
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		parts = append(parts, n+`="`+escapeLabel(v)+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(v)
}
