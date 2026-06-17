package main

import (
	"strings"

	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// classifyYC — per-provider [clouddriver.ClassifyFunc] для Yandex Cloud:
// маппит grpc-status-ошибки YC в общую таксономию SDK по grpc.Code +
// эвристикам по тексту message-а (YC не публикует closed-enum ErrorCode-ов,
// как AWS — используем grpc-канон + подстроки).
//
// Это единственная provider-specific часть error-обработки;
// backoff/retry/маппинг-в-event делает SDK (sdk/clouddriver), общий для всех
// драйверов тиража.
func classifyYC(err error) clouddriver.FailClass {
	st, ok := status.FromError(err)
	if !ok {
		// Не-grpc ошибка (сеть/DNS/EOF/TLS) — транзиентна: ретрай оправдан.
		return clouddriver.FailTransient
	}
	switch st.Code() {
	case codes.Unauthenticated, codes.PermissionDenied:
		return clouddriver.FailAuth
	case codes.NotFound:
		return clouddriver.FailNotFound
	case codes.ResourceExhausted:
		// YC: и rate-limit (429), и исчерпание квот возвращаются как
		// ResourceExhausted. Различаем по подстроке в message: throttling-
		// формулировки → transient, всё остальное → quota.
		if isThrottleMsg(st.Message()) {
			return clouddriver.FailTransient
		}
		return clouddriver.FailQuota
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return clouddriver.FailInvalidParams
	case codes.AlreadyExists:
		// Бывает при идемпотент-create по name. Не auth, не quota — относим к
		// invalid_params: оператор обязан выбрать другое имя либо удалить
		// конфликтующую VM.
		return clouddriver.FailInvalidParams
	case codes.Unavailable, codes.DeadlineExceeded, codes.Aborted, codes.Internal:
		return clouddriver.FailTransient
	case codes.Canceled:
		return clouddriver.FailTransient
	default:
		return clouddriver.FailUnknown
	}
}

// isThrottleMsg — YC возвращает ResourceExhausted как для rate-limit, так и
// для исчерпания квоты. По публичной документации API-rate-limit-сообщения
// содержат подстроки «throttl», «too many», «rate limit». Эвристика
// сознательно широкая: ложноположительный throttle ведёт к ретраю
// transient-операции, что безопаснее, чем не-ретраить исчерпание квоты.
func isThrottleMsg(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "throttl") ||
		strings.Contains(low, "too many requests") ||
		strings.Contains(low, "rate limit")
}
