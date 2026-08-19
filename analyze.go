package main

// analyze.go — read the trace file back.
//
//	sheetstream -analyze trace.jsonl
//
// The exporter writes one JSON object per span per line. That is a fine wire
// format and an awful thing to read, so this turns it into three tables:
//
//  1. SPANS — count, p50, p95, max duration per span name. Where the time goes.
//  2. NUMERIC ATTRIBUTES — the same statistics for every number any span
//     recorded: bytes raw and compressed, rows, cells, bands added and dropped,
//     and every `client.*` measurement the browser reported.
//  3. FLAGS — how often each boolean attribute was true (digest suppression
//     rate lives here).
//
// Nothing is hardcoded about which attributes exist: whatever the spans carry
// is what gets summarised, so adding an attribute upstream needs no change
// here. The `client.*` rows get their own section because they measure what no
// server span can see — a debounce window, for instance.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

// spanLine is the subset of the exporter's output this needs. Extra fields in
// the file are ignored, so an exporter upgrade that adds them does not break
// the reader.
type spanLine struct {
	Name        string `json:"Name"`
	SpanContext struct {
		TraceID string `json:"TraceID"`
		SpanID  string `json:"SpanID"`
	} `json:"SpanContext"`
	Parent struct {
		SpanID string `json:"SpanID"`
	} `json:"Parent"`
	StartTime  time.Time `json:"StartTime"`
	EndTime    time.Time `json:"EndTime"`
	Attributes []struct {
		Key   string `json:"Key"`
		Value struct {
			Type  string `json:"Type"`
			Value any    `json:"Value"`
		} `json:"Value"`
	} `json:"Attributes"`
}

// series accumulates one measurement.
type series struct {
	vals []float64
}

func (s *series) add(v float64) { s.vals = append(s.vals, v) }

func (s *series) stats() (n int, p50, p95, max, sum float64) {
	n = len(s.vals)
	if n == 0 {
		return 0, 0, 0, 0, 0
	}
	sorted := append([]float64(nil), s.vals...)
	sort.Float64s(sorted)
	for _, v := range sorted {
		sum += v
	}
	return n, quantile(sorted, 0.50), quantile(sorted, 0.95), sorted[n-1], sum
}

// quantile is nearest-rank on an already-sorted slice. With the tens-to-
// hundreds of samples a hand-driven run produces, interpolating would imply a
// precision the sample size does not support.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

type boolCount struct{ tru, total int }

// AnalyzeTrace reads a JSON-lines trace file and writes the summary tables.
func AnalyzeTrace(path string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	durations := map[string]*series{}
	numeric := map[string]*series{}
	client := map[string]*series{}
	flags := map[string]*boolCount{}
	traces := map[string]struct{}{}
	var total, bad int

	sc := bufio.NewScanner(f)
	// Span lines are small, but a wide grid render can push an attribute value
	// up; 4 MiB is generous and cheap.
	sc.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var sp spanLine
		if err := json.Unmarshal([]byte(line), &sp); err != nil {
			bad++
			continue
		}
		if sp.Name == "" {
			bad++
			continue
		}
		total++
		traces[sp.SpanContext.TraceID] = struct{}{}
		if !sp.EndTime.IsZero() && !sp.StartTime.IsZero() {
			d := sp.EndTime.Sub(sp.StartTime)
			get(durations, sp.Name).add(float64(d.Nanoseconds()) / 1e6)
		}
		for _, a := range sp.Attributes {
			switch v := a.Value.Value.(type) {
			case float64:
				if strings.HasPrefix(a.Key, "client.") {
					get(client, a.Key).add(v)
				} else {
					get(numeric, sp.Name+" · "+a.Key).add(v)
				}
			case bool:
				b := flags[sp.Name+" · "+a.Key]
				if b == nil {
					b = &boolCount{}
					flags[sp.Name+" · "+a.Key] = b
				}
				b.total++
				if v {
					b.tru++
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	fmt.Fprintf(out, "%s — %d spans, %d traces", path, total, len(traces))
	if bad > 0 {
		fmt.Fprintf(out, ", %d unparseable lines", bad)
	}
	fmt.Fprintln(out)
	if total == 0 {
		fmt.Fprintln(out, "\n(nothing to report — was the server shut down cleanly?)")
		return nil
	}

	fmt.Fprintln(out, "\nSPAN DURATIONS (ms)")
	writeTable(out, "span", durations, 3)

	if len(client) > 0 {
		fmt.Fprintln(out, "\nCLIENT-REPORTED TIMINGS (ms, browser performance.now)")
		writeTable(out, "measurement", client, 2)
	}

	fmt.Fprintln(out, "\nNUMERIC ATTRIBUTES")
	writeTable(out, "span · attribute", numeric, 1)

	if len(flags) > 0 {
		fmt.Fprintln(out, "\nFLAGS")
		keys := make([]string, 0, len(flags))
		width := len("span · attribute")
		for k := range flags {
			keys = append(keys, k)
			if len(k) > width {
				width = len(k)
			}
		}
		sort.Strings(keys)
		fmt.Fprintf(out, "%-*s  %8s  %8s  %8s\n", width, "span · attribute", "true", "total", "rate")
		fmt.Fprintln(out, strings.Repeat("─", width+30))
		for _, k := range keys {
			b := flags[k]
			fmt.Fprintf(out, "%-*s  %8d  %8d  %7.1f%%\n", width, k, b.tru, b.total,
				100*float64(b.tru)/float64(b.total))
		}
	}
	return nil
}

func get(m map[string]*series, k string) *series {
	s := m[k]
	if s == nil {
		s = &series{}
		m[k] = s
	}
	return s
}

func writeTable(out io.Writer, header string, m map[string]*series, prec int) {
	if len(m) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}
	keys := make([]string, 0, len(m))
	width := len(header)
	for k := range m {
		keys = append(keys, k)
		if len(k) > width {
			width = len(k)
		}
	}
	sort.Strings(keys)
	fmt.Fprintf(out, "%-*s  %6s  %10s  %10s  %10s  %12s\n", width, header, "n", "p50", "p95", "max", "total")
	fmt.Fprintln(out, strings.Repeat("─", width+56))
	for _, k := range keys {
		n, p50, p95, max, sum := m[k].stats()
		fmt.Fprintf(out, "%-*s  %6d  %10.*f  %10.*f  %10.*f  %12.*f\n",
			width, k, n, prec, p50, prec, p95, prec, max, prec, sum)
	}
}
