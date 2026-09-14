// Package metrics is a minimal Prometheus text-format exporter: labeled
// counters and gauge callbacks, without external dependencies.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type Registry struct {
	mu       sync.Mutex
	counters []*CounterVec
	gauges   []gauge
}

type gauge struct {
	name, help string
	fn         func() float64
}

func New() *Registry { return &Registry{} }

// CounterVec is a counter with a fixed set of label names.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]float64 // joined label values -> count
}

// Counter registers a counter. Label values are passed to Inc in order.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	c := &CounterVec{name: name, help: help, labels: labels, values: map[string]float64{}}
	r.mu.Lock()
	r.counters = append(r.counters, c)
	r.mu.Unlock()
	return c
}

// GaugeFunc registers a gauge whose value is read at scrape time.
func (r *Registry) GaugeFunc(name, help string, fn func() float64) {
	r.mu.Lock()
	r.gauges = append(r.gauges, gauge{name, help, fn})
	r.mu.Unlock()
}

// Inc adds one for the given label values. A nil CounterVec is a no-op so
// callers need not check whether metrics are enabled.
func (c *CounterVec) Inc(labelValues ...string) { c.Add(1, labelValues...) }

func (c *CounterVec) Add(v float64, labelValues ...string) {
	if c == nil {
		return
	}
	key := strings.Join(labelValues, "\xff")
	c.mu.Lock()
	c.values[key] += v
	c.mu.Unlock()
}

// Value returns the current count for label values (for tests).
func (c *CounterVec) Value(labelValues ...string) float64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[strings.Join(labelValues, "\xff")]
}

func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// Write renders all metrics in Prometheus text exposition format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	counters := append([]*CounterVec(nil), r.counters...)
	gauges := append([]gauge(nil), r.gauges...)
	r.mu.Unlock()

	for _, c := range counters {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", c.name, c.help, c.name)
		c.mu.Lock()
		keys := make([]string, 0, len(c.values))
		for k := range c.values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s%s %g\n", c.name, labelString(c.labels, k), c.values[k])
		}
		c.mu.Unlock()
	}
	for _, g := range gauges {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", g.name, g.help, g.name, g.name, g.fn())
	}
}

func labelString(names []string, key string) string {
	if len(names) == 0 {
		return ""
	}
	values := strings.Split(key, "\xff")
	parts := make([]string, len(names))
	for i, n := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		parts[i] = fmt.Sprintf(`%s="%s"`, n, escape(v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		r.Write(w)
	})
}
