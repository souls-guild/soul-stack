//go:build e2e_live

// L3b E2E операция: examples/service/redis::update_config на ЖИВОМ Redis — hot-reload
// директив без рестарта процесса. Закрывает находки NIM-53: #1 persistence-enum
// aof_1sec — валидный ключ essence.persistence_presets (render не крашит на «no such
// key»); #2 io-threads — startup-only директива (CONFIG SET её пропускает, денилист
// плагина) → ложится НА ДИСК, rewrite:false не затирает.
package e2e_live_test

import "testing"

func TestL3bRedisLive_OpsUpdateConfig(t *testing.T) {
	stack, inc, adminPass := setupRedisStandalone(t, "rdb", "volatile-lru", 1024)
	c := plainConn(adminPass)

	// Смена бюджета памяти, политики вытеснения, режима persistence и io-threads одним
	// прогоном (CONFIG SET hot-settable + CONFIG REWRITE, io-threads пропускается).
	upd := stack.RunScenario(t, inc, "update_config", map[string]any{
		"memory_mb":        512,
		"maxmemory_policy": "allkeys-lru",
		"persistence":      "aof_1sec",
		"io_threads":       2,
	})
	stack.WaitApplySuccess(t, upd, 300)
	stack.WaitIncarnationReady(t, inc, 60)

	// (а) hot-settable директивы ожили на инстансе (CONFIG GET).
	stack.AssertRedisConfigGet(t, c, "maxmemory-policy", "allkeys-lru")
	// aof_1sec → appendonly yes: находка #1 — aof_1sec валидный enum presets, render не крашит.
	stack.AssertRedisConfigGet(t, c, "appendonly", "yes")

	// (б) startup-only io-threads видно ТОЛЬКО на диске (CONFIG SET его пропускает):
	// находка #2 — rewrite:false не затирает директиву в redis.conf.
	stack.AssertRedisConfFileDirective(t, 0, "/etc/redis/redis.conf", "io-threads", "2")
	// memory_mb=512 транслируется в maxmemory 384mb (reserve 75%, essence) на диске —
	// CONFIG GET maxmemory вернул бы байты, сверяем детерминированный рендер conf-файла.
	stack.AssertRedisConfFileDirective(t, 0, "/etc/redis/redis.conf", "maxmemory", "384mb")

	// (в) read-model: state несёт новые простые понятия (update_config state_changes).
	stack.AssertIncarnationState(t, inc, map[string]any{
		"persistence":      "aof_1sec",
		"maxmemory_policy": "allkeys-lru",
		"memory_mb":        512,
		"io_threads":       2,
	})
}
