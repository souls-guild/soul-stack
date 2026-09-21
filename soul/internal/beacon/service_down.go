package beacon

import (
	"context"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
	"google.golang.org/protobuf/types/known/structpb"
)

// ServiceDownName is the core-beacon address (`core.beacon.<name>`, VigilDef.check).
// Value comes from the shared [beaconaddr] source, symmetric with the keeper-side enum.
const ServiceDownName = beaconaddr.ServiceDown

const (
	stateServiceUp   State = "up"
	stateServiceDown State = "down"
)

// ServiceDown is a core-beacon observing service activity (ADR-030 S1).
// Read-only: status polling only (is-active or equivalent), no start/stop.
// Reuses core.service's activity detection via the shared
// [util.ServiceActive] (single source of truth, no backend-detection dup).
//
// Param `service` (string, required) — unit name. State: "up" if the
// service is active, "down" if stopped or the init system can't be
// determined (from the observer's view the service is unavailable — which
// is the event of interest).
type ServiceDown struct {
	Runner util.Runner
}

// NewServiceDown builds a beacon with the production Runner (os/exec). Tests
// substitute the Runner field with a fake.
func NewServiceDown() *ServiceDown { return &ServiceDown{Runner: util.OSRunner{}} }

func (b *ServiceDown) Check(ctx context.Context, params *structpb.Struct) (State, *structpb.Struct, error) {
	name, err := util.StringParam(params, "service")
	if err != nil {
		return "", nil, err
	}

	init := util.DetectInitSystem(ctx, b.Runner)
	if init == util.InitSystemUnknown {
		// Can't determine the init system → nothing to observe the service
		// with. A valid "down" state from the beacon's view (not a check
		// error): "service unavailable" is exactly the event Vigil catches.
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
