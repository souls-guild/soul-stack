//go:build e2e_live

package harness

import (
	"net/http"
	"time"
)

// probeReady sends a GET to probeURL and returns true on 2xx. Used by
// startKeeperRun for polling the keeper process /readyz.
//
// One short-timeout client per call, not shared (test environment, low
// frequency, no need for extra global state).
func probeReady(probeURL string) bool {
	cl := &http.Client{Timeout: 1 * time.Second}
	resp, err := cl.Get(probeURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
