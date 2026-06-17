package beacon

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// ProcessAbsentName — адрес core-beacon (`core.beacon.<name>`, VigilDef.check).
const ProcessAbsentName = beaconaddr.ProcessAbsent

const (
	stateProcessPresent State = "present"
	stateProcessAbsent  State = "absent"
)

// ProcessAbsent — core-beacon наблюдения за наличием процесса (ADR-030).
// Read-only: только опрос через `pgrep` (нет kill/signal). State: "present" если
// процесс по паттерну найден, "absent" если нет. Переход present↔absent
// edge-triggered → Portent (типичный кейс — упавший демон).
//
// Через util.Runner (как core.service / core.beacon.service_down), а не скан
// /proc: pgrep OS-агностичен (Linux/BSD), а Runner мок-абелен в unit-тестах.
//
// Param `pattern` (string, required) — имя/ERE-паттерн процесса (matches против
// имени процесса, как `pgrep <pattern>`).
type ProcessAbsent struct {
	Runner util.Runner
}

// NewProcessAbsent собирает beacon с production-Runner-ом (os/exec).
func NewProcessAbsent() *ProcessAbsent { return &ProcessAbsent{Runner: util.OSRunner{}} }

func (b *ProcessAbsent) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	pattern, err := util.StringParam(params, "pattern")
	if err != nil {
		return "", nil, err
	}

	// pgrep: exit 0 — найдено хотя бы одно совпадение; exit 1 — совпадений нет;
	// exit ≥2 — ошибка самого pgrep (битый паттерн / нет бинаря) → ошибка Check.
	r := b.Runner.Run(ctx, "pgrep", pattern)
	if r.Err != nil {
		return "", nil, fmt.Errorf("pgrep %s: %v", pattern, r.Err)
	}
	switch r.ExitCode {
	case 0:
		return stateProcessPresent, processData(pattern), nil
	case 1:
		return stateProcessAbsent, processData(pattern), nil
	default:
		return "", nil, fmt.Errorf("pgrep %s: exit %d: %s", pattern, r.ExitCode, r.Stderr)
	}
}

func processData(pattern string) *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{"pattern": pattern})
	return s
}
