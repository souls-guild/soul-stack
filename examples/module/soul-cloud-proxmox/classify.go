package main

import (
	"errors"
	"strings"

	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// classifyProxmox — per-provider [clouddriver.ClassifyFunc] для Proxmox VE.
// Маппит HTTP-status REST-API в общую таксономию SDK. Proxmox не публикует
// closed-enum ErrorCode (как AWS), но HTTP-status стабилен:
//   - 401              → auth (нет/просрочен ticket, неверный token)
//   - 403              → auth (нет permission на ресурс/действие)
//   - 404              → not_found
//   - 400              → invalid_params
//   - 500              → разбираем по тексту body: «does not exist» → not_found,
//     прочее → transient (Proxmox любит 500 на временные
//     проблемы lock-контеншена и операционные конфликты)
//   - 5xx прочее       → transient
//   - 429              → transient (rate-limit, у Proxmox обычно нет, но на
//     всякий случай)
//
// Это единственная provider-specific часть error-обработки;
// backoff/retry/маппинг-в-event делает SDK (sdk/clouddriver), общий для всех
// драйверов тиража.
func classifyProxmox(err error) clouddriver.FailClass {
	var hErr *pveHTTPError
	if !errors.As(err, &hErr) {
		// Не-HTTP ошибка (сеть/DNS/EOF/TLS-handshake) — транзиентна.
		return clouddriver.FailTransient
	}
	body := strings.ToLower(hErr.Body)
	switch hErr.Status {
	case 401, 403:
		return clouddriver.FailAuth
	case 404:
		return clouddriver.FailNotFound
	case 400:
		return clouddriver.FailInvalidParams
	case 429:
		return clouddriver.FailTransient
	case 500:
		// Proxmox-конвенция: «<ресурс> does not exist» приходит как 500, а не
		// 404. Это самый частый источник false-positive «invalid_params» в
		// драйверах без эвристики — переводим в not_found, чтобы destroy
		// идемпотентности работал.
		if strings.Contains(body, "does not exist") ||
			strings.Contains(body, "no such") ||
			strings.Contains(body, "not found") {
			return clouddriver.FailNotFound
		}
		// Прочие 500 у Proxmox — transient (lock contention, storage I/O).
		return clouddriver.FailTransient
	}
	if hErr.Status >= 500 && hErr.Status < 600 {
		return clouddriver.FailTransient
	}
	return clouddriver.FailUnknown
}
