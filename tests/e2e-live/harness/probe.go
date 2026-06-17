//go:build e2e_live

package harness

import (
	"net/http"
	"time"
)

// probeReady бьёт GET по probeURL и возвращает true на 2xx. Используется
// startKeeperRun-ом для поллинга /readyz keeper-процесса.
//
// Один short-timeout client на вызов — не shared (тест-окружение, частота
// низкая, лишняя глобальная state не нужна).
func probeReady(probeURL string) bool {
	cl := &http.Client{Timeout: 1 * time.Second}
	resp, err := cl.Get(probeURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
