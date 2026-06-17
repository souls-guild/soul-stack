package legion

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// MetricsSnapshot — срез интересных Keeper-метрик из /metrics для cliff-анализа
// (docs/testing/load-testing.md §4.1). Все поля — gauge/counter в моменте.
type MetricsSnapshot struct {
	StreamsActive  float64 // keeper_grpc_streams_active
	GoGoroutines   float64 // go_goroutines
	ResidentBytes  float64 // process_resident_memory_bytes
	HeapInUseBytes float64 // go_memstats_heap_inuse_bytes
	BootstrapTotal float64 // sum(keeper_grpc_bootstrap_total) — все label-ы
	Found          bool    // удалось ли скрейпнуть хоть что-то
}

// ScrapeMetrics тянет /metrics Keeper-а (отдельный listener, ADR-024) и
// парсит интересные семейства. Минимальный prom-парсер: построчно, без зависимости
// от prometheus client_golang (tests/load — тонкий модуль). Игнорирует метрики
// с лейблами кроме сумм по семейству, где это явно указано.
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

// parseSample разбирает строку `metric{labels} value [ts]` в (name, value).
// name отсекается до первого '{' или пробела; value — последний токен (минус
// опц. timestamp prometheus не пишет в exposition без ts — берём поле после имени).
func parseSample(line string) (name string, value float64, ok bool) {
	// Разрез имени: до '{' (с лейблами) или до первого пробела (без).
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
		// последний токен может быть timestamp; попробуем предпоследний
		if len(fields) >= 3 {
			if v2, err2 := strconv.ParseFloat(fields[len(fields)-2], 64); err2 == nil {
				return name, v2, true
			}
		}
		return "", 0, false
	}
	return name, v, true
}
