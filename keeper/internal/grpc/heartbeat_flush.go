package grpc

import (
	"sync"
	"time"
)

// defaultLastSeenFlushFactor — делитель reaper `stale_after`, дающий
// throttle-интервал PG-flush-а `last_seen_at`. При дефолтном
// `mark_disconnected.stale_after = 90s` → flush раз в 30s на каждый SID.
//
// 1/3 выбрана так, чтобы внутри окна disconnect-порога заведомо умещалось
// ≥2 flush-а: даже при пропуске одного (нет app-сообщений в окне) следующий
// успеет освежить snapshot до того, как Reaper сочтёт стрим мёртвым. Меньший
// делитель → лишние UPDATE-ы в PG; больший → риск ложного disconnected при
// редком трафике стрима.
const defaultLastSeenFlushFactor = 3

// lastSeenFlusher — per-SID throttle PG-flush-а `souls.last_seen_at`.
//
// Live-стрим обновляет heartbeat в Redis на каждое app-сообщение
// ([eventStreamHandler.touchSeen]); это быстрый слой. PG-snapshot нужен
// Reaper-у (`mark_disconnected` смотрит на `souls.last_seen_at`) и
// Operator API — но писать в PG на каждое сообщение слишком тяжело.
// Flusher пропускает в PG не чаще раза в [lastSeenFlusher.interval] на
// каждый SID (in-memory last-flush time).
//
// Стейт привязан к handler-инстансу (один на EventStream-listener). При
// multi-instance каждый Keeper флашит только SID-ы своих стримов
// (гарантия [SoulLease] — один Keeper на SID одновременно); при переезде
// стрима на другой Keeper новый handler throttle-времени не знает и
// флашит сразу — безопасно (один лишний UPDATE при failover).
type lastSeenFlusher struct {
	interval time.Duration

	mu       sync.Mutex
	lastByID map[string]time.Time
}

func newLastSeenFlusher(interval time.Duration) *lastSeenFlusher {
	return &lastSeenFlusher{
		interval: interval,
		lastByID: make(map[string]time.Time),
	}
}

// shouldFlush сообщает, пора ли сбросить `last_seen_at` для sid в PG, и при
// положительном ответе атомарно фиксирует now как момент последнего flush-а.
// Так гарантируется, что между двумя true-ответами для одного SID проходит
// не меньше [lastSeenFlusher.interval], даже при конкурентных вызовах.
func (f *lastSeenFlusher) shouldFlush(sid string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	last, ok := f.lastByID[sid]
	if ok && now.Sub(last) < f.interval {
		return false
	}
	f.lastByID[sid] = now
	return true
}

// forget убирает SID из throttle-стейта — вызывается при закрытии стрима,
// чтобы карта не росла на disconnected-Soul-ах. Следующее подключение того
// же SID начнёт с чистого листа (flush сразу), что и нужно.
func (f *lastSeenFlusher) forget(sid string) {
	f.mu.Lock()
	delete(f.lastByID, sid)
	f.mu.Unlock()
}
