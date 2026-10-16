// Package metrics is a small Prometheus text-format registry: counters and
// histograms with labels, plus gauges computed at scrape time. It exists so the
// engine stays one static binary with no metrics dependency.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
)

// DefaultBuckets are latency buckets in seconds, from 1 ms to 5 minutes.
var DefaultBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

type Registry struct {
	mu         sync.Mutex
	counters   map[string]*counterVec
	histograms map[string]*histogramVec
	gauges     []gaugeFunc
	order      []string
}

func New() *Registry {
	return &Registry{counters: map[string]*counterVec{}, histograms: map[string]*histogramVec{}}
}

type counterVec struct {
	help, labels string
	values       map[string]float64 // label values joined -> value
	labelNames   []string
}

type histogramVec struct {
	help       string
	labelNames []string
	buckets    []float64
	series     map[string]*histogram
}

type histogram struct {
	counts []uint64 // per bucket, non-cumulative
	sum    float64
	count  uint64
}

type gaugeFunc struct {
	name, help string
	fn         func() []Sample
}

// Sample is one gauge value with its labels.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Counter adds delta to the counter name with the given label values (in the
// order of labelNames, which must be the same on every call).
func (r *Registry) Counter(name, help string, labelNames []string, labelValues []string, delta float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[name]
	if !ok {
		c = &counterVec{help: help, values: map[string]float64{}, labelNames: labelNames}
		r.counters[name] = c
		r.order = append(r.order, name)
	}
	c.values[strings.Join(labelValues, "\x00")] += delta
}

// Observe records v in the histogram name.
func (r *Registry) Observe(name, help string, labelNames []string, labelValues []string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.histograms[name]
	if !ok {
		h = &histogramVec{help: help, labelNames: labelNames, buckets: DefaultBuckets, series: map[string]*histogram{}}
		r.histograms[name] = h
		r.order = append(r.order, name)
	}
	key := strings.Join(labelValues, "\x00")
	s, ok := h.series[key]
	if !ok {
		s = &histogram{counts: make([]uint64, len(h.buckets)+1)}
		h.series[key] = s
	}
	i := sort.SearchFloat64s(h.buckets, v)
	s.counts[i]++
	s.sum += v
	s.count++
}

// Gauge registers a gauge whose samples are computed on every scrape.
func (r *Registry) Gauge(name, help string, fn func() []Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges = append(r.gauges, gaugeFunc{name: name, help: help, fn: fn})
}

// Write renders every metric in the Prometheus text exposition format.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	order := append([]string(nil), r.order...)
	gauges := append([]gaugeFunc(nil), r.gauges...)
	var b strings.Builder
	for _, name := range order {
		if c, ok := r.counters[name]; ok {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, c.help, name)
			for _, key := range sortedKeys(c.values) {
				fmt.Fprintf(&b, "%s%s %s\n", name, labels(c.labelNames, strings.Split(key, "\x00"), "", ""), num(c.values[key]))
			}
		}
		if h, ok := r.histograms[name]; ok {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", name, h.help, name)
			keys := make([]string, 0, len(h.series))
			for k := range h.series {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				s, vals := h.series[key], strings.Split(key, "\x00")
				var cum uint64
				for i, ub := range h.buckets {
					cum += s.counts[i]
					fmt.Fprintf(&b, "%s_bucket%s %d\n", name, labels(h.labelNames, vals, "le", num(ub)), cum)
				}
				cum += s.counts[len(h.buckets)]
				fmt.Fprintf(&b, "%s_bucket%s %d\n", name, labels(h.labelNames, vals, "le", "+Inf"), cum)
				fmt.Fprintf(&b, "%s_sum%s %s\n", name, labels(h.labelNames, vals, "", ""), num(s.sum))
				fmt.Fprintf(&b, "%s_count%s %d\n", name, labels(h.labelNames, vals, "", ""), s.count)
			}
		}
	}
	r.mu.Unlock()

	for _, g := range gauges { // outside the lock: gauge functions may query the store
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		for _, s := range g.fn() {
			names := make([]string, 0, len(s.Labels))
			for k := range s.Labels {
				names = append(names, k)
			}
			sort.Strings(names)
			vals := make([]string, len(names))
			for i, n := range names {
				vals[i] = s.Labels[n]
			}
			fmt.Fprintf(&b, "%s%s %s\n", g.name, labels(names, vals, "", ""), num(s.Value))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func labels(names, values []string, extraName, extraValue string) string {
	var parts []string
	for i, n := range names {
		if i < len(values) {
			parts = append(parts, fmt.Sprintf("%s=%q", n, values[i]))
		}
	}
	if extraName != "" {
		parts = append(parts, fmt.Sprintf("%s=%q", extraName, extraValue))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return fmt.Sprintf("%g", v)
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
