package errand

import (
	"fmt"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// buildProtoRequest converts DispatchRequest + errand_id into ErrandRequest.
// input → google.protobuf.Struct via structpb.NewStruct (JSON-compatible
// serialization of map[string]any). Errors only on non-representable
// types (chan/func), which should never appear in an API payload (HTTP
// decode yields plain JSON).
func buildProtoRequest(errandID string, req DispatchRequest) (*keeperv1.ErrandRequest, error) {
	var inputStruct *structpb.Struct
	if len(req.Input) > 0 {
		s, err := structpb.NewStruct(req.Input)
		if err != nil {
			return nil, fmt.Errorf("input → Struct: %w", err)
		}
		inputStruct = s
	}
	return &keeperv1.ErrandRequest{
		ErrandId:       errandID,
		Module:         req.Module,
		Input:          inputStruct,
		TimeoutSeconds: int32(req.TimeoutSec),
		DryRun:         req.DryRun,
	}, nil
}

// StatusFromProto maps proto ErrandStatus → the string Status used in
// the DB and DispatchResult. UNSPECIFIED → StatusFailed (defensive: a final
// ErrandResult with UNSPECIFIED violates the Soul contract, treated as fail).
func StatusFromProto(s keeperv1.ErrandStatus) Status {
	switch s {
	case keeperv1.ErrandStatus_ERRAND_STATUS_RUNNING:
		return StatusRunning
	case keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS:
		return StatusSuccess
	case keeperv1.ErrandStatus_ERRAND_STATUS_FAILED:
		return StatusFailed
	case keeperv1.ErrandStatus_ERRAND_STATUS_TIMED_OUT:
		return StatusTimedOut
	case keeperv1.ErrandStatus_ERRAND_STATUS_CANCELLED:
		return StatusCancelled
	case keeperv1.ErrandStatus_ERRAND_STATUS_MODULE_NOT_ALLOWED:
		return StatusModuleNotAllowed
	default:
		return StatusFailed
	}
}
