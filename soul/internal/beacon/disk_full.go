package beacon

import (
	"context"
	"fmt"
	"syscall"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// DiskFullName — адрес core-beacon (`core.beacon.<name>`, VigilDef.check).
const DiskFullName = beaconaddr.DiskFull

const (
	stateDiskOK   State = "ok"
	stateDiskFull State = "full"
)

// diskFullDefaultThreshold — порог использования ФС по умолчанию (процент).
// "full" взводится при использовании ≥ порога.
const diskFullDefaultThreshold = 90.0

// diskUsage — снимок использования файловой системы: процент занятого места.
// Считается через statfs (read-only syscall), без парсинга вывода `df` — точнее
// и без зависимости от локали/формата утилиты.
type diskUsage struct {
	usedPercent float64
}

// DiskFull — core-beacon наблюдения за заполнением файловой системы (ADR-030).
// Read-only: один statfs-вызов, без записи. State: "full" если использование ФС
// ≥ threshold_percent, иначе "ok". Переход ok↔full edge-triggered → Portent.
//
// Params:
//   - `path` (string, required) — точка монтирования либо любой путь внутри ФС;
//   - `threshold_percent` (int, optional, default 90) — порог "full", 1..100.
type DiskFull struct {
	// Usage вынесен в поле для подмены в unit-тестах детерминированным снимком
	// (реальный statfs зависит от свободного места хоста — флейк). В проде —
	// statfsUsage поверх syscall.Statfs.
	Usage func(path string) (diskUsage, error)
}

// NewDiskFull собирает beacon с production-сэмплером (syscall.Statfs).
func NewDiskFull() *DiskFull { return &DiskFull{Usage: statfsUsage} }

func (b *DiskFull) Check(_ context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	path, err := util.StringParam(params, "path")
	if err != nil {
		return "", nil, err
	}
	threshold, err := optThresholdPercent(params, diskFullDefaultThreshold)
	if err != nil {
		return "", nil, err
	}

	u, err := b.Usage(path)
	if err != nil {
		return "", nil, fmt.Errorf("statfs %s: %v", path, err)
	}

	state := stateDiskOK
	if u.usedPercent >= threshold {
		state = stateDiskFull
	}
	return state, diskData(path, u.usedPercent, threshold), nil
}

// statfsUsage считает процент занятого места через statfs (read-only).
// used = total - Bavail; процент от total. Bavail (а не Bfree) — блоки,
// доступные непривилегированному процессу: root-reserved (~5% по умолчанию у
// ext-семейства) считается занятым, как и у обычного `df`. Иначе used_percent
// завышался против `df` и beacon ложно-рано взводил "full". Total/avail берутся
// в блоках Bsize — процент сокращает Bsize. Пустая ФС (Blocks == 0) → 0%, чтобы
// не делить на ноль.
func statfsUsage(path string) (diskUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return diskUsage{}, err
	}
	total := st.Blocks
	if total == 0 {
		return diskUsage{usedPercent: 0}, nil
	}
	used := total - st.Bavail
	return diskUsage{usedPercent: float64(used) / float64(total) * 100}, nil
}

func diskData(path string, usedPercent, threshold float64) *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{
		"path":         path,
		"used_percent": usedPercent,
		"threshold":    threshold,
	})
	return s
}

// optThresholdPercent разбирает опциональный threshold_percent (1..100). Пустой
// param → def. proto-json маршалит числа во float64 (OptIntParam требует целое).
func optThresholdPercent(params *structpb.Struct, def float64) (float64, error) {
	n, ok, err := util.OptIntParam(params, "threshold_percent")
	if err != nil {
		return 0, err
	}
	if !ok {
		return def, nil
	}
	if n < 1 || n > 100 {
		return 0, fmt.Errorf("param %q: must be 1..100, got %d", "threshold_percent", n)
	}
	return float64(n), nil
}
