package errand

import (
	"fmt"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// buildProtoRequest конвертирует DispatchRequest + errand_id в ErrandRequest.
// input → google.protobuf.Struct через structpb.NewStruct (json-совместимая
// сериализация map[string]any). Ошибка только на нерепрезентабельных
// типах (chan/func), которых в API-payload-е быть не должно (HTTP-decode
// даёт чистый JSON-набор).
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

// StatusFromProto маппит proto ErrandStatus → строковый Status, использующийся
// в БД и DispatchResult. UNSPECIFIED → StatusFailed (defensive: финальный
// ErrandResult с UNSPECIFIED — нарушение контракта Soul-а, трактуем как fail).
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
