package utilization

import "context"

// Source — слой доступа к живой утилизации хоста. Отделён от Collector ради
// тестируемости: production-реализация ([systemSource]) читает /proc + statfs,
// unit-тесты подменяют её fake-ом.
//
// Все методы best-effort: при недоступности факта возвращают zero-value
// (0 / пустой slice), не error и не panic (симметрия с soulprint.Source,
// ADR-018/ADR-072 — Keeper толерантен к sparse-полям).
type Source interface {
	// Load — load average за 1/5/15 минут.
	Load(ctx context.Context) LoadAvg
	// Memory — used/total RAM + used swap в МБ.
	Memory(ctx context.Context) MemInfo
	// Disks — занятость не-виртуальных точек монтирования.
	Disks(ctx context.Context) []Disk
	// Uptime — время работы хоста в секундах.
	Uptime(ctx context.Context) int64
	// CPUSample — сырые тики /proc/stat для дельта-расчёта cpu% (Collector-side).
	CPUSample(ctx context.Context) CPUSample
}

// LoadAvg — load average.
type LoadAvg struct {
	One, Five, Fifteen float64
}

// MemInfo — память в МБ (used = total - available).
type MemInfo struct {
	UsedMB, TotalMB, SwapUsedMB int64
}

// Disk — занятость одной точки монтирования, объёмы в МБ.
type Disk struct {
	Mount           string
	UsedMB, TotalMB int64
}

// CPUSample — снимок счётчиков /proc/stat строки `cpu `. Idle включает iowait,
// Total — сумма всех полей; cpu% = дельта busy/total между двумя сэмплами.
type CPUSample struct {
	Total, Idle uint64
}
