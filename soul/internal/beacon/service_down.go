package beacon

import (
	"context"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// ServiceDownName — адрес core-beacon (`core.beacon.<name>`, VigilDef.check).
// Значение — из общего источника [beaconaddr], симметрично keeper-side enum.
const ServiceDownName = beaconaddr.ServiceDown

const (
	stateServiceUp   State = "up"
	stateServiceDown State = "down"
)

// ServiceDown — core-beacon наблюдения за активностью сервиса (ADR-030 S1).
// Read-only: только опрос статуса (is-active / эквивалент), без start/stop.
// Логику определения активности переиспользует у core.service через общий
// [util.ServiceActive] (единый источник истины, без дубля backend-detection).
//
// Param `service` (string, required) — имя юнита. State: "up" если сервис
// активен, "down" если остановлен либо init-систему определить нельзя (с точки
// зрения наблюдателя сервис недоступен — это и есть событие интереса).
type ServiceDown struct {
	Runner util.Runner
}

// NewServiceDown собирает beacon с production-Runner-ом (os/exec). В тестах
// поле Runner подменяется на fake.
func NewServiceDown() *ServiceDown { return &ServiceDown{Runner: util.OSRunner{}} }

func (b *ServiceDown) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	name, err := util.StringParam(params, "service")
	if err != nil {
		return "", nil, err
	}

	init := util.DetectInitSystem(ctx, b.Runner)
	if init == util.InitSystemUnknown {
		// Init-систему не определить → сервис наблюдать нечем. Это валидное
		// состояние "down" с точки зрения beacon-а (а не ошибка проверки):
		// событие «сервис недоступен» как раз и есть то, что Vigil ловит.
		return stateServiceDown, serviceData(name, false, string(init)), nil
	}

	active, err := util.ServiceActive(ctx, b.Runner, init, name)
	if err != nil {
		return "", nil, err
	}
	if active {
		return stateServiceUp, serviceData(name, true, string(init)), nil
	}
	return stateServiceDown, serviceData(name, false, string(init)), nil
}

func serviceData(name string, active bool, initSystem string) *structpb.Struct {
	fields := map[string]any{
		"service": name,
		"active":  active,
	}
	if initSystem != "" {
		fields["init_system"] = initSystem
	}
	s, _ := structpb.NewStruct(fields)
	return s
}
