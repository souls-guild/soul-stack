package legion

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// MetricsSnapshot -- snapshot of interesting Keeper metrics from /metrics for
// cliff analysis (docs/testing/load-testing.md §4.1). All fields are
// gauge/counter at a point in time.
type MetricsSnapshot struct {
	StreamsActive  float64 // keeper_grpc_streams_active
	GoGoroutines   float64 // go_goroutines
	ResidentBytes  float64 // process_resident_memory_bytes
	HeapInUseBytes float64 // go_memstats_heap_inuse_bytes
	BootstrapTotal float64 // sum(keeper_grpc_bootstrap_total) -- all labels
	Found          bool    // whether anything was scraped at all
}

// ScrapeMetrics pulls Keeper's /metrics (separate listener, ADR-024) and
// parses the families of interest. Minimal prom parser: line by line,
// without a dependency on prometheus client_golang (tests/load is a thin
// module). Ignores labeled metrics except family sums where explicitly
// stated.
func ScrapeMetrics(ctx context.Context, metricsURL string) (MetricsSnapshot, error) {
	var snap MetricsSnapshot
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(metricsURL, "/")+"/metrics", nil)
	if err != nil {
		return snap, fmt.Errorf("legion: build metrics request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return snap, fmt.Errorf("legion: scrape metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snap, fmt.Errorf("legion: scrape metrics: status %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, val, ok := parseSample(line)
		if !ok {
			continue
		}
		switch {
		case name == "keeper_grpc_streams_active":
			snap.StreamsActive = val
			snap.Found = true
		case name == "go_goroutines":
			snap.GoGoroutines = val
			snap.Found = true
		case name == "process_resident_memory_bytes":
			snap.ResidentBytes = val
			snap.Found = true
		case name == "go_memstats_heap_inuse_bytes":
			snap.HeapInUseBytes = val
			snap.Found = true
		case strings.HasPrefix(line, "keeper_grpc_bootstrap_total"):
			snap.BootstrapTotal += val
			snap.Found = true
		}
	}
	if err := sc.Err(); err != nil {
		return snap, fmt.Errorf("legion: scan metrics: %w", err)
	}
	return snap, nil
}

// parseSample parses a line `metric{labels} value [ts]` into (name, value).
// name is cut at the first '{' or space; value is the last token (an
// optional timestamp -- prometheus does not write one in exposition without
// ts -- we take the field after the name).
func parseSample(line string) (name string, value float64, ok bool) {
	// Cut the name: up to '{' (with labels) or up to the first space (without).
	brace := strings.IndexByte(line, '{')
	space := strings.IndexByte(line, ' ')
	if brace >= 0 && (space < 0 || brace < space) {
		name = line[:brace]
	} else if space >= 0 {
		name = line[:space]
	} else {
		return "", 0, false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	if err != nil {
		// the last token may be a timestamp; try the second-to-last
		if len(fields) >= 3 {
			if v2, err2 := strconv.ParseFloat(fields[len(fields)-2], 64); err2 == nil {
				return name, v2, true
			}
		}
		return "", 0, false
	}
	return name, v, true
}
